package smb

import (
	"encoding/binary"
	"io"
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// statusNoEAsOnFile reports an empty extended-attribute set to clients probing Unix metadata.
const statusNoEAsOnFile uint32 = 0xc0000052

// init registers the information query command.
func init() { handlers[cmdQueryInfo] = handleQueryInfo }

// appendBasicInfo writes FILE_BASIC_INFORMATION.
func appendBasicInfo(c *conn, dst []byte, info os.FileInfo) []byte {
	for _, t := range []uint64{ntinfo.Filetime(ntinfo.BirthTime(info)), ntinfo.Filetime(ntinfo.AccessTime(info)), ntinfo.Filetime(info.ModTime()), ntinfo.Filetime(ntinfo.ChangeTime(info))} {
		dst = binary.LittleEndian.AppendUint64(dst, t)
	}
	dst = binary.LittleEndian.AppendUint32(dst, c.fileAttributes(info))
	return binary.LittleEndian.AppendUint32(dst, 0)
}

// appendStandardInfo writes allocation, size, link count and deletion state.
func appendStandardInfo(dst []byte, info os.FileInfo, pending bool) []byte {
	_, _, _, _, links, _ := unixMetadata(info)
	dst = binary.LittleEndian.AppendUint64(dst, ntinfo.AllocationSize(info))
	dst = binary.LittleEndian.AppendUint64(dst, uint64(info.Size()))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(links))
	d := byte(0)
	if pending {
		d = 1
	}
	dir := byte(0)
	if info.IsDir() {
		dir = 1
	}
	return append(dst, d, dir, 0, 0)
}

// appendPosixInfo writes SMB3 POSIX information shared by stat and readdir.
func appendPosixInfo(dst []byte, p string, info os.FileInfo, id uint64) []byte {
	uid, gid, _, dev, links, _ := unixMetadata(info)
	for _, t := range []uint64{ntinfo.Filetime(ntinfo.BirthTime(info)), ntinfo.Filetime(ntinfo.AccessTime(info)), ntinfo.Filetime(info.ModTime()), ntinfo.Filetime(ntinfo.ChangeTime(info)), uint64(info.Size()), ntinfo.AllocationSize(info)} {
		dst = binary.LittleEndian.AppendUint64(dst, t)
	}
	dst = binary.LittleEndian.AppendUint32(dst, ntinfo.Attributes(info)&^1)
	dst = binary.LittleEndian.AppendUint64(dst, id)
	for _, n := range []uint32{uint32(dev), 0, uint32(links), infoReparseTag(info), posixMode(info)} {
		dst = binary.LittleEndian.AppendUint32(dst, n)
	}
	dst = appendSID(dst, 22, 1, uid)
	return appendSID(dst, 22, 2, gid)
}

// posixMode encodes the SMB3 POSIX file-type ordinal above the Unix permission bits.
func posixMode(info os.FileInfo) uint32 {
	mode := unixMode(info) & 0o7777
	var kind uint32
	switch {
	case info.IsDir():
		kind = 1
	case info.Mode()&os.ModeSymlink != 0:
		kind = 2
	case info.Mode()&os.ModeDevice != 0:
		kind = 4
		if info.Mode()&os.ModeCharDevice != 0 {
			kind = 3
		}
	case info.Mode()&os.ModeNamedPipe != 0:
		kind = 5
	case info.Mode()&os.ModeSocket != 0:
		kind = 6
	}
	return mode | kind<<12
}

// infoReparseTag identifies symbolic links in the Windows metadata view.
func infoReparseTag(info os.FileInfo) uint32 {
	if info.Mode()&os.ModeSymlink != 0 {
		return 0xa000000c
	}
	return 0
}

// queryFileInfo encodes an MS-FSCC file information class and reports variable length records.
func queryFileInfo(r *request, o *open, info os.FileInfo, class byte, pending bool) ([]byte, bool, uint32) {
	var b []byte
	variable := false
	fs := r.c.srv.FS
	position := func() uint64 {
		if o.file != nil {
			n, e := o.file.Seek(0, io.SeekCurrent)
			if e == nil {
				return uint64(n)
			}
		}
		return 0
	}
	// FileNameInformation names the file from the share root with a leading
	// backslash; FileNormalizedNameInformation, as Samba and Windows servers
	// return it, has none: Windows clients append it to \\server\share\ for
	// GetFinalPathNameByHandle (Rust's fs::canonicalize).
	name := func(lead string) []byte {
		n := lead + strings.ReplaceAll(o.path, "/", "\\")
		out := binary.LittleEndian.AppendUint32(nil, uint32(utf16Len(n)))
		return utf16Append(out, n)
	}
	switch class {
	case 4:
		b = appendBasicInfo(r.c, nil, info)
	case 5:
		b = appendStandardInfo(nil, info, pending)
	case 6:
		b = binary.LittleEndian.AppendUint64(nil, fs.FileID(o.path))
	case 7:
		b = make([]byte, 4)
	case 8:
		b = binary.LittleEndian.AppendUint32(nil, o.access)
	case 9:
		b = name("\\")
		variable = true
	case 48:
		b = name("")
		variable = true
	case 14:
		b = binary.LittleEndian.AppendUint64(nil, position())
	case 15:
		return nil, false, statusNoEAsOnFile
	case 16:
		b = binary.LittleEndian.AppendUint32(nil, o.options&0x3e)
	case 17:
		b = make([]byte, 4)
	case 18:
		b = appendBasicInfo(r.c, nil, info)
		b = appendStandardInfo(b, info, pending)
		b = binary.LittleEndian.AppendUint64(b, fs.FileID(o.path))
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = binary.LittleEndian.AppendUint32(b, o.access)
		b = binary.LittleEndian.AppendUint64(b, position())
		b = binary.LittleEndian.AppendUint32(b, o.options&0x3e)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, name("\\")...)
		variable = true
	case 21:
		return nil, false, statusObjectNameNotFound
	case 22:
		variable = true
		if !info.IsDir() {
			b = make([]byte, 8)
			n := "::$DATA"
			binary.LittleEndian.PutUint32(b[4:], uint32(utf16Len(n)))
			b = binary.LittleEndian.AppendUint64(b, uint64(info.Size()))
			b = binary.LittleEndian.AppendUint64(b, ntinfo.AllocationSize(info))
			b = utf16Append(b, n)
		}
	case 28:
		b = binary.LittleEndian.AppendUint64(nil, uint64(info.Size()))
		b = append(b, make([]byte, 8)...)
	case 34:
		b = appendBasicInfo(r.c, nil, info)
		b = b[:32]
		b = binary.LittleEndian.AppendUint64(b, ntinfo.AllocationSize(info))
		b = binary.LittleEndian.AppendUint64(b, uint64(info.Size()))
		b = binary.LittleEndian.AppendUint32(b, r.c.fileAttributes(info))
		b = append(b, 0, 0, 0, 0)
	case 35:
		b = binary.LittleEndian.AppendUint32(nil, r.c.fileAttributes(info))
		b = binary.LittleEndian.AppendUint32(b, infoReparseTag(info))
	case 59:
		b = binary.LittleEndian.AppendUint64(nil, 1)
		b = binary.LittleEndian.AppendUint64(b, fs.FileID(o.path))
		b = binary.LittleEndian.AppendUint64(b, 0)
	case 100:
		if !r.c.posix {
			return nil, false, statusInvalidInfoClass
		}
		b = appendPosixInfo(nil, o.path, info, fs.FileID(o.path))
		variable = true
	default:
		return nil, false, statusInvalidInfoClass
	}
	return b, variable, statusSuccess
}

// queryFSInfo encodes MS-FSCC volume and capacity information.
func queryFSInfo(r *request, class byte) ([]byte, bool, uint32) {
	s, err := r.c.srv.FS.Statfs()
	if err != nil {
		return nil, false, ntinfo.Status(err)
	}
	var b []byte
	variable := false
	block := s.BlockSize
	if block == 0 {
		block = 4096
	}
	sectors := block / 512
	if sectors == 0 {
		sectors = 1
	}
	switch class {
	case 1:
		b = binary.LittleEndian.AppendUint64(nil, 0)
		b = binary.LittleEndian.AppendUint32(b, 1)
		b = binary.LittleEndian.AppendUint32(b, uint32(utf16Len("ovnfs")))
		b = append(b, 0, 0)
		b = utf16Append(b, "ovnfs")
		variable = true
	case 3:
		b = binary.LittleEndian.AppendUint64(nil, s.Blocks)
		b = binary.LittleEndian.AppendUint64(b, s.Avail)
		b = binary.LittleEndian.AppendUint32(b, sectors)
		b = binary.LittleEndian.AppendUint32(b, 512)
	case 4:
		b = binary.LittleEndian.AppendUint32(nil, 7)
		b = binary.LittleEndian.AppendUint32(b, 0x20)
	case 5:
		b = binary.LittleEndian.AppendUint32(nil, 0x8f)
		b = binary.LittleEndian.AppendUint32(b, 255)
		b = binary.LittleEndian.AppendUint32(b, 8)
		b = utf16Append(b, "NTFS")
		variable = true
	case 7:
		for _, n := range []uint64{s.Blocks, s.Avail, s.Free} {
			b = binary.LittleEndian.AppendUint64(b, n)
		}
		b = binary.LittleEndian.AppendUint32(b, sectors)
		b = binary.LittleEndian.AppendUint32(b, 512)
	case 8:
		b = make([]byte, 64)
		copy(b, r.c.srv.guid[:])
	case 11:
		for _, n := range []uint32{512, block, block, block, 3, 0, 0} {
			b = binary.LittleEndian.AppendUint32(b, n)
		}
	case 100:
		if !r.c.posix {
			return nil, false, statusInvalidInfoClass
		}
		b = binary.LittleEndian.AppendUint32(nil, block)
		b = binary.LittleEndian.AppendUint32(b, block)
		for _, n := range []uint64{s.Blocks, s.Free, s.Avail, s.Files, s.FilesFree, 0} {
			b = binary.LittleEndian.AppendUint64(b, n)
		}
	default:
		return nil, false, statusInvalidInfoClass
	}
	return b, variable, statusSuccess
}

// handleQueryInfo validates QUERY_INFO and bounds the encoded response to the client's buffer.
func handleQueryInfo(r *request) ([]byte, uint32) {
	q := r.body
	if len(q) < 40 || binary.LittleEndian.Uint16(q) != 41 {
		return nil, statusInvalidParameter
	}
	o, st := r.openFile(fileID{binary.LittleEndian.Uint64(q[24:]), binary.LittleEndian.Uint64(q[32:])})
	if st != statusSuccess {
		return nil, st
	}
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	pending := r.c.srv.st.opens.deletePending(p)
	o.mu.Lock()
	defer o.mu.Unlock()
	info, err := o.stat(r.c.srv)
	if err != nil {
		return nil, ntinfo.Status(err)
	}
	var b []byte
	variable := false
	switch q[2] {
	case 1:
		b, variable, st = queryFileInfo(r, o, info, q[3], pending)
	case 2:
		b, variable, st = queryFSInfo(r, q[3])
	case 3:
		b = securityDescriptor(info, binary.LittleEndian.Uint32(q[16:]), r.c.owner, r.c.group)
		variable = true
	case 4:
		st = statusNotSupported
	default:
		st = statusInvalidParameter
	}
	if st != statusSuccess {
		return nil, st
	}
	max := uint64(binary.LittleEndian.Uint32(q[4:]))
	if uint64(len(b)) > max {
		if !variable {
			return nil, statusInfoLengthMismatch
		}
		b = b[:max]
		st = statusBufferOverflow
	}
	out := []byte{9, 0, 72, 0}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b)))
	out = append(out, b...)
	return out, st
}
