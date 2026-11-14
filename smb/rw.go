package smb

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// init registers positional data I/O and flush handlers.
func init() {
	handlers[cmdRead] = handleRead
	handlers[cmdWrite] = handleWrite
	handlers[cmdFlush] = handleFlush
}

// handleRead implements bounded file and named-pipe reads without changing the file cursor.
func handleRead(r *request) ([]byte, uint32) {
	if r.c.srv.st.opens.cacheError() != nil {
		return nil, statusUnsuccessful
	}
	b := r.body
	if len(b) < 48 || binary.LittleEndian.Uint16(b) != 49 {
		return nil, statusInvalidParameter
	}
	n := binary.LittleEndian.Uint32(b[4:])
	off := binary.LittleEndian.Uint64(b[8:])
	minimum := binary.LittleEndian.Uint32(b[32:])
	if n > r.c.maxRead || minimum > n || off > math.MaxInt64 || uint64(n) > math.MaxInt64-off || binary.LittleEndian.Uint32(b[36:]) != 0 {
		return nil, statusInvalidParameter
	}
	o, st := r.openFile(getOpenID(b[16:]))
	if st != statusSuccess {
		return nil, st
	}
	if o.access&accessReadData == 0 {
		return nil, statusAccessDenied
	}
	if o.isDir {
		return nil, statusFileIsADirectory
	}
	var data []byte
	var out []byte
	if o.pipe != nil {
		data, st = o.pipe.read(int(n))
		if st != statusSuccess && st != statusBufferOverflow {
			return nil, st
		}
	} else {
		if r.c.srv.st.locks.conflicts(o, off, uint64(n), false) {
			return nil, statusFileLockConflict
		}
		out = make([]byte, 16+int(n))
		data = out[16:]
		o.mu.Lock()
		if o.file == nil {
			o.mu.Unlock()
			return nil, statusInvalidDeviceRequest
		}
		count, err := o.file.ReadAt(data, int64(off))
		o.mu.Unlock()
		if err != nil && err != io.EOF {
			return nil, ntinfo.Status(err)
		}
		if n != 0 && (count == 0 || uint32(count) < minimum) {
			return nil, statusEndOfFile
		}
		data = data[:count]
	}
	if out == nil {
		out = make([]byte, 16, 16+len(data))
		out = append(out, data...)
	} else {
		out = out[:16+len(data)]
	}
	binary.LittleEndian.PutUint16(out, 17)
	out[2] = 80
	binary.LittleEndian.PutUint32(out[4:], uint32(len(data)))
	return out, st
}

// handleWrite writes at the explicit offset, copying lower files up at CREATE time.
func handleWrite(r *request) (response []byte, status uint32) {
	b := r.body
	if len(b) < 48 || binary.LittleEndian.Uint16(b) != 49 {
		return nil, statusInvalidParameter
	}
	do := int(binary.LittleEndian.Uint16(b[2:]))
	n := binary.LittleEndian.Uint32(b[4:])
	off := binary.LittleEndian.Uint64(b[8:])
	if n > r.c.maxWrite || (n != 0 && do < 112) || uint64(do)+uint64(n) > uint64(len(r.raw)) || (off != math.MaxUint64 && (off > math.MaxInt64 || uint64(n) > math.MaxInt64-off)) || binary.LittleEndian.Uint32(b[32:]) != 0 {
		return nil, statusInvalidParameter
	}
	o, st := r.openFile(getOpenID(b[16:]))
	if st != statusSuccess {
		return nil, st
	}
	if o.access&(accessWriteData|accessAppendData) == 0 {
		return nil, statusAccessDenied
	}
	if o.isDir {
		return nil, statusFileIsADirectory
	}
	if o.pipe == nil {
		g := &r.c.srv.st.opens
		grant := g.beginCacheIO(o)
		defer func() {
			var err error
			if status != statusSuccess {
				err = fmt.Errorf("backend WRITE failed with status %#x", status)
			}
			g.endCacheIO(grant, err)
		}()
	}
	data := r.raw[do:int(uint64(do)+uint64(n))]
	count := len(data)
	if o.pipe != nil {
		st = o.pipe.write(data)
		if st != statusSuccess {
			return nil, st
		}
	} else {
		o.mu.Lock()
		if o.file == nil {
			o.mu.Unlock()
			return nil, statusInvalidDeviceRequest
		}
		p := o.path
		if off == math.MaxUint64 || o.access&accessWriteData == 0 {
			info, err := r.c.srv.FS.Stat(p)
			if err != nil {
				o.mu.Unlock()
				return nil, ntinfo.Status(err)
			}
			off = uint64(info.Size())
		}
		o.mu.Unlock()
		if r.c.srv.st.locks.conflicts(o, off, uint64(n), true) {
			return nil, statusFileLockConflict
		}
		o.mu.Lock()
		if o.file == nil {
			o.mu.Unlock()
			return nil, statusFileClosed
		}
		var err error
		if writer, ok := o.file.(io.WriterAt); ok {
			count, err = writer.WriteAt(data, int64(off))
		} else {
			_, err = o.file.Seek(int64(off), io.SeekStart)
			if err == nil {
				count, err = o.file.Write(data)
			}
		}
		if err == nil && (binary.LittleEndian.Uint32(b[44:])&1 != 0 || o.options&createWriteThrough != 0) {
			if syncer, ok := o.file.(interface{ Sync() error }); ok {
				err = syncer.Sync()
			}
		}
		o.mu.Unlock()
		if count != 0 {
			r.c.srv.st.notify.changedFiltered(p, notifyModified, 0x18)
		}
		if err != nil {
			return nil, ntinfo.Status(err)
		}
		if count != len(data) {
			return nil, statusDiskFull
		}
	}
	out := make([]byte, 16)
	binary.LittleEndian.PutUint16(out, 17)
	binary.LittleEndian.PutUint32(out[4:], uint32(count))
	return out, statusSuccess
}

// handleFlush commits writes where the backing file supports durable synchronization.
func handleFlush(r *request) (response []byte, status uint32) {
	b := r.body
	if len(b) < 24 || binary.LittleEndian.Uint16(b) != 24 {
		return nil, statusInvalidParameter
	}
	o, st := r.openFile(getOpenID(b[8:]))
	if st != statusSuccess {
		return nil, st
	}
	g := &r.c.srv.st.opens
	grant := g.beginCacheIO(o)
	defer func() {
		var err error
		if status != statusSuccess {
			err = fmt.Errorf("backend FLUSH failed with status %#x", status)
		}
		g.endCacheIO(grant, err)
	}()
	o.mu.Lock()
	if o.file == nil && o.pipe == nil {
		o.mu.Unlock()
		return nil, statusFileClosed
	}
	if syncer, ok := o.file.(interface{ Sync() error }); ok {
		err := syncer.Sync()
		o.mu.Unlock()
		if err != nil {
			return nil, ntinfo.Status(err)
		}
	} else {
		o.mu.Unlock()
	}
	return []byte{4, 0, 0, 0}, statusSuccess
}
