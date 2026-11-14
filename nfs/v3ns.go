package nfs

import (
	"errors"
	"os"
	"path"
	"sync"
)

// Namespace procedures. Backend errors keep their errno (statusOf), so
// clients see EXDEV, ENOTEMPTY, EPERM and EACCES as such: a cross-checkout
// move fails XDEV and the client falls back to copying.

// dir3 resolves the directory of a diropargs3 naming an entry to create or
// remove, and vets the name; dot is the status for "." and "..".
func (s *Server) dir3(id uint64, ok bool, name string, dot status) (string, os.FileInfo, status) {
	p, st := s.path3(id, ok)
	if st != stOK {
		return "", nil, st
	}
	if st := checkName3(name); st != stOK {
		return "", nil, st
	}
	if isDot(name) {
		return "", nil, dot
	}
	info, err := s.FS.Lstat(p)
	if err != nil {
		return "", nil, statusOf(err)
	}
	if !info.IsDir() {
		return "", nil, stNotDir
	}
	return p, info, stOK
}

// created3 appends the result of a CREATE, MKDIR, SYMLINK or MKNOD of p in dir: the
// new handle, its attributes and the directory's wcc_data (without pre-op
// attributes, which could not be read atomically with the change).
func (s *Server) created3(res *encoder, dir, p string) status {
	fid := s.FS.FileID(p)
	if fid == 0 {
		return stIO
	}
	res.bool(true)
	res.fh(fid)
	s.postOp3(res, fid, p)
	s.wcc3(res, nil, s.FS.FileID(dir), dir)
	return stOK
}

// createhow3 modes.
const (
	createUnchecked = 0
	createGuarded   = 1
	createExclusive = 2
)

// create3 answers CREATE (RFC 1813 section 3.3.8). GUARDED refuses an
// existing name. UNCHECKED of an existing regular file only applies a
// requested size, as Linux's nfsd does: a client racing its own stale
// negative cache must not chmod, chown or retime the file. EXCLUSIVE, which
// O_EXCL opens send, refuses an existing name unless an EXCLUSIVE create
// with the same verifier made it (a retransmission); the client sets the
// attributes with SETATTR afterwards. Without it, macOS would fall back to
// UNCHECKED and O_EXCL would not be exclusive.
//
// A file created without its owner's write permission stays open for the
// creator's writes (keptFiles).
func (s *Server) create3(d *decoder, res *encoder) status {
	id, ok := d.fh(maxFH3)
	name := d.str(maxPath3)
	how := d.u32()
	var sa sattr3
	var verf [8]byte
	switch how {
	case createUnchecked, createGuarded:
		sa = decodeSattr3(d)
	case createExclusive:
		copy(verf[:], d.fixed(8))
	default:
		d.fail()
	}
	if d.err != nil {
		return stInval
	}
	dir, _, st := s.dir3(id, ok, name, stExist)
	if st != stOK {
		return st
	}
	p := path.Join(dir, name)
	if info, err := s.FS.Lstat(p); err == nil {
		return s.exists3(res, dir, p, info, how, &sa, verf)
	}
	perm := os.FileMode(0o666)
	if sa.setMode {
		perm = goMode(sa.mode) & os.ModePerm
	}
	f, err := s.FS.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, os.ErrExist) {
		// Created meanwhile by someone else.
		if info, err := s.FS.Lstat(p); err == nil {
			return s.exists3(res, dir, p, info, how, &sa, verf)
		}
	}
	if err != nil {
		return statusOf(err)
	}
	fid := s.FS.FileID(p)
	if fid == 0 {
		f.Close()
		return stIO
	}
	if how == createExclusive {
		s.v3.verfs.put(fid, verf)
	}
	if perm&0o200 == 0 {
		if err := s.v3.kept.keep(keptKey{id: fid}, f); err != nil {
			return statusOf(err)
		}
	} else if err := f.Close(); err != nil {
		return statusOf(err)
	}
	if st := s.apply3(p, &sa); st != stOK {
		return st
	}
	return s.created3(res, dir, p)
}

// exists3 answers a CREATE of p, which exists as info: EXIST for GUARDED,
// for anything but a regular file, and for EXCLUSIVE but a retransmission
// with verifier verf; else the file as it is, truncated first to a
// requested size.
func (s *Server) exists3(res *encoder, dir, p string, info os.FileInfo, how uint32, sa *sattr3, verf [8]byte) status {
	switch {
	case how == createGuarded, !info.Mode().IsRegular():
		return stExist
	case how == createExclusive:
		if !s.v3.verfs.match(s.FS.FileID(p), verf) {
			return stExist
		}
	case sa.setSize:
		id := s.FS.FileID(p)
		res.beginSize(id, sa.size)
		s.Recall(id)
		st := s.apply3(p, &sattr3{size: sa.size, setSize: true})
		s.Recall(id)
		if st != stOK {
			return st
		}
		res.acceptSize()
	}
	return s.created3(res, dir, p)
}

// createVerfs remembers the verifiers of recent EXCLUSIVE creates by file
// id, the most recent 1024, so that a retransmitted CREATE succeeds where
// another fails EXIST. A server restart forgets them; a retransmission
// across it fails EXIST.
type createVerfs struct {
	mu    sync.Mutex
	byID  map[uint64][8]byte
	order [1024]uint64
	pos   int
}

// put records that an EXCLUSIVE create with verf made file id.
func (c *createVerfs) put(id uint64, verf [8]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = make(map[uint64][8]byte)
	}
	if _, ok := c.byID[id]; !ok {
		delete(c.byID, c.order[c.pos])
		c.order[c.pos] = id
		c.pos = (c.pos + 1) % len(c.order)
	}
	c.byID[id] = verf
}

// match reports whether an EXCLUSIVE create with verf made file id.
func (c *createVerfs) match(id uint64, verf [8]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.byID[id]
	return ok && v == verf
}

// mkdir3 answers MKDIR (section 3.3.9).
func (s *Server) mkdir3(d *decoder, res *encoder) status {
	id, ok := d.fh(maxFH3)
	name := d.str(maxPath3)
	sa := decodeSattr3(d)
	if d.err != nil {
		return stInval
	}
	dir, _, st := s.dir3(id, ok, name, stExist)
	if st != stOK {
		return st
	}
	p := path.Join(dir, name)
	if _, err := s.FS.Lstat(p); err == nil {
		return stExist
	}
	perm := os.FileMode(0o755)
	if sa.setMode {
		perm = goMode(sa.mode) & os.ModePerm
	}
	if err := s.FS.MkdirAll(p, perm); err != nil {
		return statusOf(err)
	}
	if st := s.apply3(p, &sa); st != stOK {
		return st
	}
	return s.created3(res, dir, p)
}

// symlink3 answers SYMLINK (section 3.3.10). A dangling link is an existing
// name too.
func (s *Server) symlink3(d *decoder, res *encoder) status {
	id, ok := d.fh(maxFH3)
	name := d.str(maxPath3)
	sa := decodeSattr3(d)
	target := d.str(maxPath3)
	if d.err != nil {
		return stInval
	}
	dir, _, st := s.dir3(id, ok, name, stExist)
	if st != stOK {
		return st
	}
	p := path.Join(dir, name)
	if _, err := s.FS.Lstat(p); err == nil {
		return stExist
	}
	if err := s.FS.Symlink(target, p); err != nil {
		return statusOf(err)
	}
	if st := s.apply3(p, &sa); st != stOK {
		return st
	}
	return s.created3(res, dir, p)
}

// mknod3 creates sockets and FIFOs; device nodes remain unsupported.
func (s *Server) mknod3(d *decoder, res *encoder) status {
	id, ok := d.fh(maxFH3)
	name := d.str(maxPath3)
	typ := fileType(d.u32())
	var sa sattr3
	switch typ {
	case typeSock, typeFifo:
		sa = decodeSattr3(d)
	case typeBlk, typeChr:
		sa = decodeSattr3(d)
		d.u32()
		d.u32()
	default:
		if d.err != nil {
			return stInval
		}
		return stBadType
	}
	if d.err != nil {
		return stInval
	}
	dir, _, st := s.dir3(id, ok, name, stExist)
	if st != stOK {
		return st
	}
	if typ != typeSock && typ != typeFifo {
		return stAccess
	}
	maker, ok := s.FS.(interface {
		Mknod(string, os.FileMode) error
	})
	if !ok {
		return stNotSupp
	}
	mode := os.FileMode(0o666)
	if sa.setMode {
		mode = goMode(sa.mode) & os.ModePerm
	}
	if typ == typeSock {
		mode |= os.ModeSocket
	} else {
		mode |= os.ModeNamedPipe
	}
	p := path.Join(dir, name)
	if err := maker.Mknod(p, mode); err != nil {
		return statusOf(err)
	}
	if st := s.apply3(p, &sa); st != stOK {
		return st
	}
	return s.created3(res, dir, p)
}

// remove3 answers REMOVE (section 3.3.12).
func (s *Server) remove3(d *decoder, res *encoder) status { return s.unlink3(d, res, false) }

// rmdir3 answers RMDIR (section 3.3.13).
func (s *Server) rmdir3(d *decoder, res *encoder) status { return s.unlink3(d, res, true) }

// unlink3 removes a file (REMOVE) or an empty directory (RMDIR), after
// recalling NFSv4 delegations of it. Entries are examined
// without following symlinks, so REMOVE takes a link to a directory.
func (s *Server) unlink3(d *decoder, res *encoder, isDir bool) status {
	id, ok := d.fh(maxFH3)
	name := d.str(maxPath3)
	if d.err != nil {
		return stInval
	}
	dir, dinfo, st := s.dir3(id, ok, name, stInval)
	if st != stOK {
		return st
	}
	pre := attrsOf(id, dinfo)
	p := path.Join(dir, name)
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	switch {
	case isDir && !info.IsDir():
		return stNotDir
	case !isDir && info.IsDir():
		return stIsDir
	case isDir:
		entries, err := s.FS.ReadDir(p)
		if err != nil {
			return statusOf(err)
		}
		if len(entries) > 0 {
			return stNotEmpty
		}
	}
	fid := s.FS.FileID(p)
	if fid == 0 {
		return stIO
	}
	s.Recall(fid)
	err = s.FS.Remove(p)
	s.Recall(fid)
	if err != nil {
		return statusOf(err)
	}
	s.wcc3(res, &pre, id, dir)
	return stOK
}

// rename3 answers RENAME (section 3.3.14), after recalling NFSv4
// delegations of the moved file and of any file it replaces.
func (s *Server) rename3(d *decoder, res *encoder) status {
	fromID, fromOK := d.fh(maxFH3)
	fromName := d.str(maxPath3)
	toID, toOK := d.fh(maxFH3)
	toName := d.str(maxPath3)
	if d.err != nil {
		return stInval
	}
	fromDir, fromInfo, st := s.dir3(fromID, fromOK, fromName, stInval)
	if st != stOK {
		return st
	}
	toDir, toInfo, st := s.dir3(toID, toOK, toName, stInval)
	if st != stOK {
		return st
	}
	fromPre, toPre := attrsOf(fromID, fromInfo), attrsOf(toID, toInfo)
	from, to := path.Join(fromDir, fromName), path.Join(toDir, toName)
	// Look before taking an id, so a missing source allocates none.
	if _, err := s.FS.Lstat(from); err != nil {
		return statusOf(err)
	}
	moved, replaced := s.FS.FileID(from), uint64(0)
	if _, err := s.FS.Lstat(to); err == nil {
		replaced = s.FS.FileID(to)
	}
	recall := func() {
		s.Recall(moved)
		if replaced != 0 {
			s.Recall(replaced)
		}
	}
	recall()
	err := s.FS.Rename(from, to)
	recall()
	if err != nil {
		return statusOf(err)
	}
	s.wcc3(res, &fromPre, fromID, fromDir)
	s.wcc3(res, &toPre, toID, toDir)
	return stOK
}

// link3 answers LINK (section 3.3.15): the arguments are the
// file's handle and the new name's diropargs3; the result carries the
// file's attributes and the directory's wcc_data, no handle. Directories
// cannot be linked (PERM). The file's delegations are recalled, since its
// link count changes.
func (s *Server) link3(d *decoder, res *encoder) status {
	id, ok := d.fh(maxFH3)
	dirID, dirOK := d.fh(maxFH3)
	name := d.str(maxPath3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, ok)
	if st != stOK {
		return st
	}
	dir, dinfo, st := s.dir3(dirID, dirOK, name, stExist)
	if st != stOK {
		return st
	}
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	if info.IsDir() {
		return stPerm
	}
	pre := attrsOf(dirID, dinfo)
	s.Recall(id)
	err = s.FS.Link(p, path.Join(dir, name))
	s.Recall(id)
	if err != nil {
		return statusOf(err)
	}
	s.postOp3(res, id, p)
	s.wcc3(res, &pre, dirID, dir)
	return stOK
}
