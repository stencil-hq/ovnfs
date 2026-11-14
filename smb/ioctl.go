package smb

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"io"
	"math"
	"net"
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// FSCTL codes identify the filesystem and named-pipe operations exposed over SMB.
const (
	ioctlValidateNegotiate = 0x00140204
	ioctlNetworkInterfaces = 0x001401fc
	ioctlGetReparse        = 0x000900a8
	ioctlSetReparse        = 0x000900a4
	ioctlDeleteReparse     = 0x000900ac
	ioctlResumeKey         = 0x00140078
	ioctlCopyChunk         = 0x001440f2
	ioctlCopyChunkWrite    = 0x001480f2
	ioctlPipeTransceive    = 0x0011c017
	ioctlDFSReferrals      = 0x00060194
	ioctlDFSReferralsEx    = 0x000601b0
	ioctlCreateObjectID    = 0x000900c0
	ioctlGetObjectID       = 0x0009009c
	ioctlSetSparse         = 0x000900c4
	ioctlAllocatedRanges   = 0x000940cf
	ioctlZeroData          = 0x000980c8
	ioctlGetIntegrity      = 0x0009027c
	reparseTagNFS          = 0x80000014
	copyMaxChunks          = 256
	copyMaxChunk           = 1 << 20
	copyMaxTotal           = 16 << 20
)

// init registers filesystem-control dispatch with the SMB command table.
func init() { handlers[cmdIoctl] = handleIoctl }

// handleIoctl validates SMB2 IOCTL framing before dispatching a filesystem control.
func handleIoctl(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 56 || binary.LittleEndian.Uint16(b) != 57 || binary.LittleEndian.Uint32(b[48:]) != 1 {
		return nil, statusInvalidParameter
	}
	code := binary.LittleEndian.Uint32(b[4:])
	id := fileID{binary.LittleEndian.Uint64(b[8:]), binary.LittleEndian.Uint64(b[16:])}
	if (code == ioctlValidateNegotiate || code == ioctlNetworkInterfaces || code == ioctlDFSReferrals || code == ioctlDFSReferralsEx) && id != relatedFileID {
		return nil, statusInvalidParameter
	}
	off, count := uint64(binary.LittleEndian.Uint32(b[24:])), uint64(binary.LittleEndian.Uint32(b[28:]))
	maxOut := binary.LittleEndian.Uint32(b[44:])
	if maxOut > r.c.maxTrans && r.c.maxTrans != 0 {
		maxOut = r.c.maxTrans
	}
	var in []byte
	if count != 0 {
		if off < 120 || off > uint64(len(r.raw)) || count > uint64(len(r.raw))-off {
			return nil, statusInvalidParameter
		}
		in = r.raw[off : off+count]
	}
	var out []byte
	st := uint32(statusSuccess)
	switch code {
	case ioctlValidateNegotiate:
		if r.hdr.Flags&flagSigned == 0 {
			return ioctlValidationFailure(r)
		}
		if len(in) < 24 {
			return nil, statusInvalidParameter
		}
		n := int(binary.LittleEndian.Uint16(in[22:]))
		if n == 0 || len(in) != 24+2*n {
			return nil, statusInvalidParameter
		}
		valid := binary.LittleEndian.Uint32(in) == r.c.clientCaps && binary.LittleEndian.Uint16(in[20:]) == r.c.clientSecMode && subtle.ConstantTimeCompare(in[4:20], r.c.clientGUID[:]) == 1 && n == len(r.c.clientDialects)
		for i := range n {
			if !valid {
				break
			}
			valid = binary.LittleEndian.Uint16(in[24+2*i:]) == r.c.clientDialects[i]
		}
		if !valid {
			return ioctlValidationFailure(r)
		}
		if maxOut < 24 {
			return nil, statusBufferTooSmall
		}
		out = binary.LittleEndian.AppendUint32(out, r.c.serverCaps)
		out = append(out, r.c.srv.guid[:]...)
		out = binary.LittleEndian.AppendUint16(out, r.c.serverSecMode)
		out = binary.LittleEndian.AppendUint16(out, r.c.dialect)
	case ioctlNetworkInterfaces:
		if maxOut < 152 {
			return nil, statusBufferTooSmall
		}
		out = ioctlInterface(r.c.nc.LocalAddr())
	case ioctlDFSReferrals, ioctlDFSReferralsEx:
		return nil, statusFsDriverRequired
	default:
		o, s := r.openFile(id)
		if s != statusSuccess {
			return nil, s
		}
		id = o.id
		if r.c.srv.WriteCache && !r.cacheProtected && o.pipe == nil {
			switch code {
			case ioctlCopyChunk, ioctlCopyChunkWrite:
				// COPYCHUNK reads source bytes on the server, not in the
				// requesting client's cache, so even its own lease must drain.
				return r.withWriteBarrier(nil, true, false, func() ([]byte, uint32) { return handleIoctl(r) })
			case ioctlSetReparse:
				return r.withWriteBarrier(o, false, false, func() ([]byte, uint32) { return handleIoctl(r) })
			case ioctlZeroData, ioctlAllocatedRanges:
				return r.withWriteBarrier(o, false, true, func() ([]byte, uint32) { return handleIoctl(r) })
			}
			// Object/resume IDs, reparse queries, and unsupported capability
			// probes neither consume cached data nor mutate its backing file.
		}
		switch code {
		case ioctlGetReparse:
			if maxOut < 8 {
				return nil, statusBufferTooSmall
			}
			out, st = ioctlReadReparse(r, o)
		case ioctlSetReparse:
			st = ioctlWriteReparse(r, o, in)
		case ioctlDeleteReparse:
			st = statusNotSupported
		case ioctlResumeKey:
			if o.file == nil {
				return nil, statusInvalidDeviceRequest
			}
			if maxOut < 32 {
				return nil, statusBufferTooSmall
			}
			out = ioctlKey(r.c.srv, o.id)
			out = append(out, make([]byte, 8)...)
		case ioctlCopyChunk, ioctlCopyChunkWrite:
			if maxOut < 12 {
				return nil, statusInvalidParameter
			}
			out, st = ioctlCopy(r, o, in, code == ioctlCopyChunk)
		case ioctlPipeTransceive:
			if o.pipe == nil {
				return nil, statusInvalidDeviceRequest
			}
			out, st = o.pipe.transact(in, int(maxOut))
		case ioctlCreateObjectID, ioctlGetObjectID:
			if maxOut < 64 {
				return nil, statusBufferTooSmall
			}
			o.mu.Lock()
			p := o.path
			o.mu.Unlock()
			out = make([]byte, 64)
			id := r.c.srv.FS.FileID(p)
			if id == 0 {
				return nil, statusInternalError
			}
			binary.LittleEndian.PutUint64(out, id)
			copy(out[8:16], r.c.srv.guid[:8])
		case ioctlSetSparse:
			st = statusInvalidDeviceRequest
		case ioctlAllocatedRanges:
			out, st = ioctlRanges(r, o, in, maxOut)
		case ioctlZeroData:
			st = ioctlZero(r, o, in)
		case ioctlGetIntegrity:
			st = statusInvalidDeviceRequest
		default:
			st = statusNotSupported
		}
	}
	if st != statusSuccess && st != statusBufferOverflow && !((code == ioctlCopyChunk || code == ioctlCopyChunkWrite) && len(out) != 0) {
		return nil, st
	}
	if uint64(len(out)) > uint64(maxOut) {
		out = out[:maxOut]
		st = statusBufferOverflow
	}
	return ioctlResponse(code, id, out), st
}

// ioctlResponse encodes the fixed SMB2 IOCTL response and its output buffer.
func ioctlResponse(code uint32, id fileID, out []byte) []byte {
	b := make([]byte, 48, 48+len(out))
	binary.LittleEndian.PutUint16(b, 49)
	binary.LittleEndian.PutUint32(b[4:], code)
	binary.LittleEndian.PutUint64(b[8:], id.Persistent)
	binary.LittleEndian.PutUint64(b[16:], id.Volatile)
	if len(out) != 0 {
		binary.LittleEndian.PutUint32(b[32:], 112)
	}
	binary.LittleEndian.PutUint32(b[36:], uint32(len(out)))
	return append(b, out...)
}

// ioctlValidationFailure drops a connection whose signed negotiation transcript changed.
func ioctlValidationFailure(r *request) ([]byte, uint32) {
	previous := r.onSent
	r.onSent = func(msg []byte) {
		if previous != nil {
			previous(msg)
		}
		_ = r.c.nc.Close()
	}
	return nil, statusAccessDenied
}

// ioctlInterface describes the connection's local address without RSS or RDMA capabilities.
func ioctlInterface(addr net.Addr) []byte {
	b := make([]byte, 152)
	binary.LittleEndian.PutUint32(b[4:], 1)
	binary.LittleEndian.PutUint64(b[16:], 10000000000)
	var ip net.IP
	var zone string
	if a, ok := addr.(*net.TCPAddr); ok {
		ip, zone = a.IP, a.Zone
	} else if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		ip = net.ParseIP(host)
	}
	if v4 := ip.To4(); v4 != nil {
		binary.LittleEndian.PutUint16(b[24:], 2)
		copy(b[28:32], v4)
	} else {
		binary.LittleEndian.PutUint16(b[24:], 23)
		copy(b[32:48], ip.To16())
		if iface, err := net.InterfaceByName(zone); err == nil {
			binary.LittleEndian.PutUint32(b[48:], uint32(iface.Index))
		}
	}
	return b
}

// ioctlReadReparse represents a backend symlink using the Windows symlink reparse layout.
func ioctlReadReparse(r *request, o *open) ([]byte, uint32) {
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	info, err := r.c.srv.FS.Lstat(p)
	if err != nil {
		return nil, ntinfo.Status(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil, statusNotAReparsePoint
	}
	target, err := r.c.srv.FS.Readlink(p)
	if err != nil {
		return nil, ntinfo.Status(err)
	}
	b, err := ntinfo.SymlinkReparse(target)
	if err != nil {
		return nil, statusInvalidParameter
	}
	return b, statusSuccess
}

// ioctlWriteReparse converts an empty placeholder into a Windows or NFS-style
// symlink: CreateSymbolicLink and junction tools link an empty file or
// directory they just created. A junction becomes a symlink to its target.
func ioctlWriteReparse(r *request, o *open, in []byte) uint32 {
	if len(in) < 8 || int(binary.LittleEndian.Uint16(in[4:])) != len(in)-8 {
		return statusInvalidParameter
	}
	if o.access&(accessWriteData|0x100) == 0 {
		return statusAccessDenied
	}
	tag := binary.LittleEndian.Uint32(in)
	var target string
	switch tag {
	case ntinfo.ReparseTagSymlink, ntinfo.ReparseTagMountPoint:
		var err error
		if target, err = ntinfo.ParseSymlinkReparse(in); err != nil {
			return statusInvalidParameter
		}
	case reparseTagNFS:
		if len(in) < 16 || binary.LittleEndian.Uint64(in[8:]) != 0x00000000014b4e4c {
			return statusIoReparseTagNotHandled
		}
		if (len(in)-16)&1 != 0 {
			return statusInvalidParameter
		}
		target = strings.ReplaceAll(utf16Decode(in[16:]), "\\", "/")
	default:
		return statusIoReparseTagNotHandled
	}
	if target == "" || strings.ContainsRune(target, 0) {
		return statusInvalidParameter
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pipe != nil || o.file == nil && !o.isDir {
		return statusInvalidDeviceRequest
	}
	if tag == ntinfo.ReparseTagMountPoint && !o.isDir {
		return statusNotADirectory
	}
	target = ntinfo.MountRelative(target, o.path, r.c.srv.MountRoots)
	fs := r.c.srv.FS
	info, err := fs.Lstat(o.path)
	if err != nil {
		return ntinfo.Status(err)
	}
	if o.isDir {
		entries, err := fs.ReadDir(o.path)
		if err != nil {
			return ntinfo.Status(err)
		}
		if len(entries) != 0 {
			return statusDirectoryNotEmpty
		}
	} else if !info.Mode().IsRegular() || info.Size() != 0 {
		return statusInvalidParameter
	}
	// restore puts the placeholder back when the link cannot replace it.
	restore := func() {
		if o.isDir {
			_ = fs.MkdirAll(o.path, info.Mode().Perm())
		} else {
			o.file, _ = fs.OpenFile(o.path, os.O_CREATE|os.O_RDWR, info.Mode().Perm())
		}
	}
	if o.file != nil {
		if err := o.file.Close(); err != nil {
			return ntinfo.Status(err)
		}
		o.file = nil
	}
	if err := fs.Remove(o.path); err != nil {
		restore()
		return ntinfo.Status(err)
	}
	if err := fs.Symlink(target, o.path); err != nil {
		restore()
		return ntinfo.Status(err)
	}
	o.isDir = false
	r.c.srv.st.notify.changed(o.path, notifyModified)
	return statusSuccess
}

// ioctlKey authenticates an open identifier so resume keys cannot name unrelated handles.
func ioctlKey(s *Server, id fileID) []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint64(b, id.Persistent)
	binary.LittleEndian.PutUint64(b[8:], id.Volatile)
	var material [32]byte
	copy(material[:16], s.guid[:])
	copy(material[16:], b[:16])
	hash := sha256.Sum256(material[:])
	copy(b[16:], hash[:8])
	return b
}

// ioctlCopy validates COPYCHUNK limits and copies bounded extents between live opens.
func ioctlCopy(r *request, dst *open, in []byte, requireRead bool) ([]byte, uint32) {
	limits := func() ([]byte, uint32) {
		b := binary.LittleEndian.AppendUint32(nil, copyMaxChunks)
		b = binary.LittleEndian.AppendUint32(b, copyMaxChunk)
		b = binary.LittleEndian.AppendUint32(b, copyMaxTotal)
		return b, statusInvalidParameter
	}
	if len(in) < 32 {
		return limits()
	}
	n := binary.LittleEndian.Uint32(in[24:])
	if n > copyMaxChunks || uint64(len(in)) < 32+uint64(n)*24 {
		return limits()
	}
	var total uint64
	var largest uint32
	for i := range n {
		b := in[32+i*24:]
		length := binary.LittleEndian.Uint32(b[16:])
		if length == 0 || length > copyMaxChunk {
			return limits()
		}
		total += uint64(length)
		if total > copyMaxTotal {
			return limits()
		}
		if length > largest {
			largest = length
		}
		source, target := binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint64(b[8:])
		if source > math.MaxInt64 || uint64(length) > math.MaxInt64-source {
			return nil, statusInvalidViewSize
		}
		if target != math.MaxUint64 && (target > math.MaxInt64 || uint64(length) > math.MaxInt64-target) {
			return limits()
		}
	}
	id := fileID{binary.LittleEndian.Uint64(in), binary.LittleEndian.Uint64(in[8:])}
	if subtle.ConstantTimeCompare(in[:24], ioctlKey(r.c.srv, id)) != 1 {
		return nil, statusObjectNameNotFound
	}
	src := r.c.srv.st.opens.byID(id)
	if src == nil || src.tree.sess != r.sess {
		return nil, statusObjectNameNotFound
	}
	if src.tree.ipc || dst.tree.ipc {
		return limits()
	}
	if src.access&1 == 0 || dst.access&6 == 0 || requireRead && dst.access&1 == 0 {
		return nil, statusAccessDenied
	}
	// A stable identifier order avoids deadlocking simultaneous opposite-direction copies.
	first, second := src, dst
	if first.id.Persistent > second.id.Persistent || first.id.Persistent == second.id.Persistent && first.id.Volatile > second.id.Volatile {
		first, second = second, first
	}
	first.mu.Lock()
	if second != first {
		second.mu.Lock()
		defer second.mu.Unlock()
	}
	defer first.mu.Unlock()
	if src.file == nil || dst.file == nil {
		return nil, statusInvalidDeviceRequest
	}
	buf := make([]byte, largest)
	var chunks, chunkBytes, copied uint32
	st := uint32(statusSuccess)
	for i := range n {
		b := in[32+i*24:]
		source, target := int64(binary.LittleEndian.Uint64(b)), int64(binary.LittleEndian.Uint64(b[8:]))
		length := binary.LittleEndian.Uint32(b[16:])
		chunkBytes = 0
		if target == -1 || dst.access&2 == 0 {
			info, err := r.c.srv.FS.Stat(dst.path)
			if err != nil {
				st = ntinfo.Status(err)
				break
			}
			target = info.Size()
			if uint64(target) > math.MaxInt64-uint64(length) {
				st = statusInvalidParameter
				break
			}
		}
		nr, er := src.file.ReadAt(buf[:length], source)
		if er != nil && er != io.EOF {
			st = ntinfo.Status(er)
			break
		}
		if nr != int(length) {
			st = statusInvalidViewSize
			break
		}
		nw, ew := ioctlWriteAt(dst.file, buf[:length], target)
		chunkBytes = uint32(nw)
		copied += uint32(nw)
		if ew != nil {
			st = ntinfo.Status(ew)
			break
		}
		if nw != int(length) {
			st = statusDiskFull
			break
		}
		chunks++
		chunkBytes = 0
	}
	if copied != 0 {
		r.c.srv.st.notify.changed(dst.path, notifyModified)
	}
	out := binary.LittleEndian.AppendUint32(nil, chunks)
	out = binary.LittleEndian.AppendUint32(out, chunkBytes)
	out = binary.LittleEndian.AppendUint32(out, copied)
	return out, st
}

// statusInvalidViewSize reports a COPYCHUNK source range extending beyond EOF.
const statusInvalidViewSize uint32 = 0xc000001f

// ioctlWriteAt uses native positional writes when available, otherwise the caller's open lock protects Seek.
func ioctlWriteAt(f interface {
	io.Writer
	io.Seeker
}, b []byte, off int64) (int, error) {
	if w, ok := f.(io.WriterAt); ok {
		return w.WriteAt(b, off)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return f.Write(b)
}

// ioctlRanges conservatively reports the whole non-sparse file when the query intersects its data.
func ioctlRanges(r *request, o *open, in []byte, maxOut uint32) ([]byte, uint32) {
	if len(in) != 16 {
		return nil, statusInvalidParameter
	}
	start, length := binary.LittleEndian.Uint64(in), binary.LittleEndian.Uint64(in[8:])
	if start > math.MaxInt64 || length > math.MaxInt64 || length > math.MaxInt64-start {
		return nil, statusInvalidParameter
	}
	if o.access&1 == 0 {
		return nil, statusAccessDenied
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return nil, statusInvalidDeviceRequest
	}
	info, err := r.c.srv.FS.Stat(o.path)
	if err != nil {
		return nil, ntinfo.Status(err)
	}
	if length == 0 || start >= uint64(info.Size()) {
		return nil, statusSuccess
	}
	if maxOut < 16 {
		return nil, statusBufferTooSmall
	}
	out := binary.LittleEndian.AppendUint64(nil, 0)
	out = binary.LittleEndian.AppendUint64(out, uint64(info.Size()))
	return out, statusSuccess
}

// ioctlZero overwrites an in-file range without extending EOF or allocating an unbounded buffer.
func ioctlZero(r *request, o *open, in []byte) uint32 {
	if len(in) != 16 {
		return statusInvalidParameter
	}
	start, end := binary.LittleEndian.Uint64(in), binary.LittleEndian.Uint64(in[8:])
	if start > end || end > math.MaxInt64 {
		return statusInvalidParameter
	}
	if o.access&2 == 0 {
		return statusAccessDenied
	}
	var cacheErr error
	if r.c.srv.WriteCache {
		g := &r.c.srv.st.opens
		grant := g.beginCacheIO(o)
		defer func() { g.endCacheIO(grant, cacheErr) }()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.file == nil {
		return statusInvalidDeviceRequest
	}
	info, err := r.c.srv.FS.Stat(o.path)
	if err != nil {
		return ntinfo.Status(err)
	}
	if end > uint64(info.Size()) {
		end = uint64(info.Size())
	}
	if start >= end {
		return statusSuccess
	}
	zero := make([]byte, 64<<10)
	for start < end {
		n := min(end-start, uint64(len(zero)))
		nw, err := ioctlWriteAt(o.file, zero[:n], int64(start))
		start += uint64(nw)
		if err != nil {
			cacheErr = err
			return ntinfo.Status(err)
		}
		if nw != int(n) {
			cacheErr = io.ErrShortWrite
			return statusDiskFull
		}
	}
	r.c.srv.st.notify.changed(o.path, notifyModified)
	return statusSuccess
}
