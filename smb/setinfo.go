package smb

import (
	"encoding/binary"
	"io"
	"os"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// init registers the metadata mutation command.
func init() { handlers[cmdSetInfo] = handleSetInfo }

// handleSetInfo applies supported MS-FSCC metadata changes to the overlay.
func handleSetInfo(r *request) ([]byte, uint32) {
	q := r.body
	if len(q) < 32 || binary.LittleEndian.Uint16(q) != 33 {
		return nil, statusInvalidParameter
	}
	n := uint64(binary.LittleEndian.Uint32(q[4:]))
	offset := uint64(binary.LittleEndian.Uint16(q[8:]))
	if offset < 64 || offset+n > uint64(len(r.raw)) {
		return nil, statusInvalidParameter
	}
	data := r.raw[offset : offset+n]
	o, st := r.openFile(fileID{binary.LittleEndian.Uint64(q[16:]), binary.LittleEndian.Uint64(q[24:])})
	if st != statusSuccess {
		return nil, st
	}
	if q[2] == 1 && (q[3] == 10 || q[3] == 13 || q[3] == 64) {
		return namespaceSetInfo(r, o, q[3], data)
	}
	changesMetadata := q[2] == 3 || q[2] == 1 && (q[3] == 4 || q[3] == 19 || q[3] == 20 || q[3] == 39)
	if r.c.srv.WriteCache && changesMetadata && !r.cacheProtected {
		return r.withWriteBarrier(o, false, true, func() ([]byte, uint32) { return handleSetInfo(r) })
	}
	var cacheErr error
	if r.c.srv.WriteCache && changesMetadata {
		g := &r.c.srv.st.opens
		grant := g.beginCacheIO(o)
		defer func() { g.endCacheIO(grant, cacheErr) }()
	}
	o.mu.Lock()
	type change struct {
		p              string
		action, filter uint32
	}
	var changes []change
	defer func() {
		o.mu.Unlock()
		for _, c := range changes {
			r.c.srv.st.notify.changedFiltered(c.p, c.action, c.filter)
		}
	}()
	fs := r.c.srv.FS
	if q[2] == 2 {
		return nil, statusNotSupported
	}
	if q[2] == 3 {
		if o.access&0x40000 == 0 {
			return nil, statusAccessDenied
		}
		st = applySecurity(fs, o.path, data)
		if st != statusSuccess {
			return nil, st
		}
		changes = append(changes, change{o.path, notifyModified, 0x100})
		return []byte{2, 0}, statusSuccess
	}
	if q[2] != 1 {
		return nil, statusNotSupported
	}
	info, err := fs.Lstat(o.path)
	if err != nil {
		return nil, ntinfo.Status(err)
	}
	var filter uint32
	switch q[3] {
	case 4:
		if len(data) < 40 {
			return nil, statusInfoLengthMismatch
		}
		if o.access&0x100 == 0 {
			return nil, statusAccessDenied
		}
		at, mt := ntinfo.AccessTime(info), info.ModTime()
		changed := false
		av, mv := binary.LittleEndian.Uint64(data[8:]), binary.LittleEndian.Uint64(data[16:])
		if av != 0 && av != ^uint64(0) {
			at = ntinfo.FromFiletime(av)
			changed = true
			filter |= 0x20
		}
		if mv != 0 && mv != ^uint64(0) {
			mt = ntinfo.FromFiletime(mv)
			changed = true
			filter |= 0x10
		}
		if changed {
			if err = fs.Chtimes(o.path, at, mt); err != nil {
				return nil, ntinfo.Status(err)
			}
		}
		attrs := binary.LittleEndian.Uint32(data[32:])
		// A symlink's read-only attribute has no mode to map to, as in winfsp.
		if attrs != 0 && !r.c.posix && !r.c.appleReadDir.Load() && info.Mode()&os.ModeSymlink == 0 {
			mode := info.Mode()
			if attrs&1 != 0 {
				mode &^= 0200
			} else {
				mode |= 0200
			}
			if err = fs.Chmod(o.path, mode); err != nil {
				return nil, ntinfo.Status(err)
			}
			filter |= 4
		}
	case 11:
		return nil, statusNotSupported
	case 14:
		if len(data) < 8 {
			return nil, statusInfoLengthMismatch
		}
		pos := binary.LittleEndian.Uint64(data)
		if pos > uint64(^uint64(0)>>1) {
			return nil, statusInvalidParameter
		}
		if o.file == nil {
			return nil, statusInvalidDeviceRequest
		}
		if _, err = o.file.Seek(int64(pos), io.SeekStart); err != nil {
			return nil, ntinfo.Status(err)
		}
	case 16:
		if len(data) < 4 {
			return nil, statusInfoLengthMismatch
		}
		mode := binary.LittleEndian.Uint32(data)
		if mode & ^uint32(0x3e) != 0 || mode&0x30 == 0x30 {
			return nil, statusInvalidParameter
		}
		o.options = o.options&^0x3e | mode
	case 19, 20, 39:
		if len(data) < 8 {
			return nil, statusInfoLengthMismatch
		}
		size := binary.LittleEndian.Uint64(data)
		if size > uint64(^uint64(0)>>1) {
			return nil, statusInvalidParameter
		}
		if o.access&2 == 0 {
			return nil, statusAccessDenied
		}
		if info.IsDir() {
			return nil, statusFileIsADirectory
		}
		if q[3] == 39 {
			break
		}
		if q[3] == 19 && size >= uint64(info.Size()) {
			break
		}
		// Reopen through the overlay so a lower-only handle cannot mutate the lower tree.
		f, e := fs.OpenFile(o.path, os.O_WRONLY, 0)
		if e != nil {
			cacheErr = e
			return nil, ntinfo.Status(e)
		}
		e = f.Truncate(int64(size))
		ce := f.Close()
		if e != nil {
			cacheErr = e
			return nil, ntinfo.Status(e)
		}
		if ce != nil {
			cacheErr = ce
			return nil, ntinfo.Status(ce)
		}
		filter = 0x18
	default:
		return nil, statusInvalidInfoClass
	}
	if filter != 0 {
		changes = append(changes, change{o.path, notifyModified, filter})
	}
	return []byte{2, 0}, statusSuccess
}

// namespaceSetInfo releases both the open and CREATE transaction locks during
// cache breaks: clients may need to reopen, flush, or close the affected handles.
func namespaceSetInfo(r *request, o *open, class byte, data []byte) ([]byte, uint32) {
	// Retain the grant gate across asynchronous retries as well as the final
	// namespace transaction; same-key reopens must not reintroduce dirty data.
	g := &r.c.srv.st.opens
	var release func()
	if r.c.srv.WriteCache {
		release = g.holdWriteGrants()
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	var target string
	var deleting bool
	if class == 10 {
		if len(data) < 20 {
			return nil, statusInfoLengthMismatch
		}
		if binary.LittleEndian.Uint64(data[8:]) != 0 {
			return nil, statusInvalidParameter
		}
		size := uint64(binary.LittleEndian.Uint32(data[16:]))
		if size%2 != 0 || size > uint64(len(data)-20) {
			return nil, statusInvalidParameter
		}
		var st uint32
		target, st = smbPath(utf16Decode(data[20 : 20+size]))
		if st != statusSuccess {
			return nil, st
		}
	} else if class == 13 {
		if len(data) < 1 {
			return nil, statusInfoLengthMismatch
		}
		deleting = data[0] != 0
	} else {
		if len(data) < 4 {
			return nil, statusInfoLengthMismatch
		}
		flags := binary.LittleEndian.Uint32(data)
		if flags&^uint32(0x1f) != 0 {
			return nil, statusInvalidParameter
		}
		if flags&2 != 0 {
			return nil, statusNotSupported
		}
		deleting = flags&1 != 0
	}
	body, st, breaks := namespaceAttempt(r, o, class, data, target, deleting, nil)
	if len(breaks) == 0 {
		return body, st
	}
	if r.compound != nil {
		for len(breaks) != 0 {
			if !g.waitBreaks(breaks, nil) {
				return nil, statusCancelled
			}
			body, st, breaks = namespaceAttempt(r, o, class, data, target, deleting, nil)
		}
		return body, st
	}
	async := r.async()
	finish := release
	release = nil
	go func() {
		if finish != nil {
			defer finish()
		}
		for len(breaks) != 0 {
			if !g.waitBreaks(breaks, async.cancelled) {
				r.complete(async, nil, statusCancelled)
				return
			}
			body, st, breaks = namespaceAttempt(r, o, class, data, target, deleting, async.cancelled)
		}
		r.complete(async, body, st)
	}()
	return nil, statusPending
}

// namespaceAttempt revalidates handle lifetime, paths and directory contents
// after every break and commits while competing namespace operations are excluded.
func namespaceAttempt(r *request, o *open, class byte, data []byte, target string, deleting bool, cancelled <-chan struct{}) ([]byte, uint32, []*oplockGrant) {
	g := &r.c.srv.st.opens
	if g.cacheError() != nil {
		return nil, statusUnsuccessful, nil
	}
	// A file's rename or delete reaches only its own name and the target's; a
	// directory's reaches every name below it.
	var unlock func()
	for {
		o.mu.Lock()
		p, dir := o.path, o.isDir
		o.mu.Unlock()
		switch {
		case dir:
			unlock = g.lockAll()
		case class == 10:
			unlock = g.lockNames(p, target)
		default:
			unlock = g.lockName(p)
		}
		o.mu.Lock()
		same := o.path == p
		o.mu.Unlock()
		if same || dir {
			break
		}
		unlock()
	}
	defer unlock()
	select {
	case <-cancelled:
		return nil, statusCancelled, nil
	default:
	}
	g.mu.Lock()
	live := g.entries[o]
	var key [16]byte
	lease := false
	if grant := g.grants[o]; grant != nil && grant.lease {
		key, lease = grant.key, true
	}
	g.mu.Unlock()
	if !live {
		return nil, statusFileClosed, nil
	}
	o.mu.Lock()
	p, access := o.path, o.access
	o.mu.Unlock()
	if access&accessDelete == 0 {
		return nil, statusAccessDenied, nil
	}
	fs := r.c.srv.FS
	info, err := fs.Lstat(p)
	if err != nil {
		return nil, ntinfo.Status(err), nil
	}
	if class == 10 {
		if target == p {
			return []byte{2, 0}, statusSuccess, nil
		}
		if p == "" || target == "" {
			return nil, statusAccessDenied, nil
		}
		if g.deletePending(p) || g.deletePending(target) {
			return nil, statusDeletePending, nil
		}
		_, e := fs.Lstat(target)
		if e == nil && data[0] == 0 {
			return nil, statusObjectNameCollision, nil
		}
		if e != nil && !os.IsNotExist(e) {
			return nil, ntinfo.Status(e), nil
		}
		breaks := g.breakNamespaceSource(r.c, p, key, lease, info.IsDir())
		if e == nil {
			breaks = append(breaks, g.breakDescendants(target)...)
		}
		if len(breaks) != 0 {
			return nil, statusPending, breaks
		}
		if e == nil && len(g.at(target)) != 0 {
			return nil, statusAccessDenied, nil
		}
		if g.cacheError() != nil {
			return nil, statusUnsuccessful, nil
		}
		if err := fs.Rename(p, target); err != nil {
			return nil, ntinfo.Status(err), nil
		}
		g.rename(p, target)
		filter := uint32(1)
		if info.IsDir() {
			filter = 2
		}
		r.c.srv.st.notify.changedFiltered(p, notifyRenamedOld, filter)
		r.c.srv.st.notify.changedFiltered(target, notifyRenamedNew, filter)
		return []byte{2, 0}, statusSuccess, nil
	}
	if deleting {
		if p == "" || r.c.fileAttributes(info)&1 != 0 {
			return nil, statusCannotDelete, nil
		}
		if breaks := g.breakNamespaceSource(r.c, p, key, lease, info.IsDir()); len(breaks) != 0 {
			return nil, statusPending, breaks
		}
		if info.IsDir() {
			entries, e := fs.ReadDir(p)
			if e != nil {
				return nil, ntinfo.Status(e), nil
			}
			if len(entries) != 0 {
				return nil, statusDirectoryNotEmpty, nil
			}
		}
	}
	if g.cacheError() != nil {
		return nil, statusUnsuccessful, nil
	}
	o.mu.Lock()
	o.deleteOnClose = deleting
	o.mu.Unlock()
	g.setDeletePending(p, deleting)
	return []byte{2, 0}, statusSuccess, nil
}
