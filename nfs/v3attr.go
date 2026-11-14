package nfs

import (
	"errors"
	"math"
	"os"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// maxReportedSizes keeps a reconnect from forgetting a size another
// connection has already reported. It never decreases.
type maxReportedSizes struct {
	mu   sync.Mutex
	byID map[uint64]uint64
}

func (s *maxReportedSizes) observe(id, size uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.byID[id]; ok && previous > size {
		return previous
	}
	if s.byID == nil {
		s.byID = make(map[uint64]uint64)
	}
	s.byID[id] = size
	return size
}

func (s *maxReportedSizes) previous(id uint64) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size, ok := s.byID[id]
	return size, ok
}

// clientReportedSizes orders size regressions with the operation that caused
// them; an unexpected regression retires the handle before it is reported.
type clientReportedSizes struct {
	mu      sync.Mutex
	changed *sync.Cond
	byID    map[uint64]uint64
	pending map[uint64]bool
}

func (s *clientReportedSizes) report(maximum *maxReportedSizes, id, size uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wait(id)
	if previous, ok := s.byID[id]; ok {
		if previous > size {
			return previous
		}
		if maximum.observe(id, size) <= size {
			delete(s.byID, id)
		} else {
			s.byID[id] = size
		}
		return size
	}
	return maximum.observe(id, size)
}

func (s *clientReportedSizes) regressed(maximum *maxReportedSizes, id, size uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wait(id)
	if previous, ok := s.byID[id]; ok {
		return size < previous
	}
	previous, ok := maximum.previous(id)
	return ok && size < previous
}

func (s *clientReportedSizes) wait(id uint64) {
	for s.pending[id] {
		if s.changed == nil {
			s.changed = sync.NewCond(&s.mu)
		}
		s.changed.Wait()
	}
}

func (s *clientReportedSizes) begin(id uint64) {
	s.mu.Lock()
	for s.pending[id] {
		if s.changed == nil {
			s.changed = sync.NewCond(&s.mu)
		}
		s.changed.Wait()
	}
	if s.pending == nil {
		s.pending = make(map[uint64]bool)
	}
	s.pending[id] = true
	s.mu.Unlock()
}

func (s *clientReportedSizes) finish(id, size uint64, accepted bool) {
	s.mu.Lock()
	if accepted {
		if s.byID == nil {
			s.byID = make(map[uint64]uint64)
		}
		s.byID[id] = size
	}
	delete(s.pending, id)
	if s.changed != nil {
		s.changed.Broadcast()
	}
	s.mu.Unlock()
}

// beginSize holds back concurrent attributes until a client-requested resize
// reply has been sent. That client already updates its VM size safely as part
// of the resize operation.
func (e *encoder) beginSize(id, size uint64) {
	if e.sizes == nil {
		return
	}
	e.sizes.begin(id)
	e.sizeID, e.size, e.sizePending = id, size, true
}

func (e *encoder) acceptSize() { e.sizeAccepted = e.sizePending }

func (e *encoder) finishSize(sent bool) {
	if !e.sizePending {
		return
	}
	accepted := sent && e.sizeAccepted
	if accepted {
		e.maxSizes.observe(e.sizeID, e.size)
	}
	e.sizes.finish(e.sizeID, e.size, accepted)
	e.sizePending = false
}

func (e *encoder) reportSize(a *attrs) {
	if a.typ != typeReg || e.sizes == nil || e.sizePending && e.sizeID == a.fileid {
		return
	}
	a.size = e.sizes.report(e.maxSizes, a.fileid, a.size)
}

func (e *encoder) sizeRegressed(a *attrs) bool {
	return a.typ == typeReg && e.sizes != nil && e.sizes.regressed(e.maxSizes, a.fileid, a.size)
}

// fattr3 appends a as an fattr3.
func fattr3(e *encoder, a *attrs) {
	e.reportSize(a)
	e.u32(uint32(a.typ))
	e.u32(a.mode)
	e.u32(a.nlink)
	e.u32(a.uid)
	e.u32(a.gid)
	e.u64(a.size)
	e.u64(a.size) // used
	e.u32(a.major)
	e.u32(a.minor)
	e.u64(0) // fsid: the one export
	e.u64(a.fileid)
	time3(e, a.mtime) // atime
	time3(e, a.mtime)
	time3(e, a.mtime) // ctime
}

// time3 appends t as an nfstime3.
func time3(e *encoder, t time.Time) {
	e.u32(uint32(t.Unix()))
	e.u32(uint32(t.Nanosecond()))
}

// postAttrs3 appends a present post_op_attr.
func postAttrs3(e *encoder, a *attrs) {
	e.bool(true)
	fattr3(e, a)
}

// postOp3 appends the post_op_attr of file id at p, absent when p cannot
// be stat'ed.
func (s *Server) postOp3(e *encoder, id uint64, p string) {
	a, err := s.stat(id, p)
	if err != nil {
		e.bool(false)
		return
	}
	postAttrs3(e, &a)
}

// wcc3 appends wcc_data: the wcc_attr of pre (nil: absent), then the
// current attributes of file id at p.
func (s *Server) wcc3(e *encoder, pre *attrs, id uint64, p string) {
	if pre == nil {
		e.bool(false)
	} else {
		e.bool(true)
		e.u64(pre.size)
		time3(e, pre.mtime)
		time3(e, pre.mtime) // ctime
	}
	s.postOp3(e, id, p)
}

// sattr3 is a decoded sattr3: the attributes a client sets.
type sattr3 struct {
	mode, uid, gid                                       uint32
	size                                                 uint64
	atime, mtime                                         time.Time
	setMode, setUID, setGID, setSize, setAtime, setMtime bool
}

// decodeSattr3 reads an sattr3.
func decodeSattr3(d *decoder) (a sattr3) {
	if a.setMode = d.bool(); a.setMode {
		a.mode = d.u32()
	}
	if a.setUID = d.bool(); a.setUID {
		a.uid = d.u32()
	}
	if a.setGID = d.bool(); a.setGID {
		a.gid = d.u32()
	}
	if a.setSize = d.bool(); a.setSize {
		a.size = d.u64()
	}
	a.atime, a.setAtime = decodeSetTime3(d)
	a.mtime, a.setMtime = decodeSetTime3(d)
	return a
}

// decodeSetTime3 reads a set_atime or set_mtime: false for DONT_CHANGE.
func decodeSetTime3(d *decoder) (time.Time, bool) {
	switch d.u32() {
	case 0: // DONT_CHANGE
		return time.Time{}, false
	case 1: // SET_TO_SERVER_TIME
		return time.Now(), true
	case 2: // SET_TO_CLIENT_TIME
		sec := d.u32()
		nsec := d.u32()
		return time.Unix(int64(sec), int64(nsec)), true
	}
	d.fail()
	return time.Time{}, false
}

// apply3 sets a on the file at p, as SETATTR and the creating procedures
// do. The mode of a symlink is left alone: its permission bits
// mean nothing, and billy's Chmod follows the link, changing the target or
// failing ENOENT for a dangling one. Ownership changes the link itself
// (Lchown); a size is refused on a symlink.
//
// The size goes first, so a truncate still works when the same call removes
// write permission, and the mode after the owner, which a chown may strip
// of setuid and setgid.
func (s *Server) apply3(p string, a *sattr3) status {
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	link := info.Mode()&os.ModeSymlink != 0
	if a.setSize {
		if link {
			return stNotSupp
		}
		if st := s.truncate3(p, a.size); st != stOK {
			return st
		}
	}
	if a.setUID || a.setGID {
		cur, _ := unixstat.Of(info)
		uid, gid := cur.UID, cur.GID
		if a.setUID {
			uid = a.uid
		}
		if a.setGID {
			gid = a.gid
		}
		if uid != cur.UID || gid != cur.GID {
			if err := s.FS.Lchown(p, int(uid), int(gid)); err != nil {
				return statusOf(err)
			}
		}
	}
	if a.setMode && !link && a.mode&0o7777 != posixMode(info.Mode()) {
		if info.Mode().IsRegular() && info.Mode()&0o200 != 0 && a.mode&0o200 == 0 {
			s.keepWritable3(p)
		}
		if err := s.FS.Chmod(p, goMode(a.mode)); err != nil {
			return statusOf(err)
		}
	}
	if a.setAtime || a.setMtime {
		atime, mtime := info.ModTime(), info.ModTime()
		if a.setAtime {
			atime = a.atime
		}
		if a.setMtime {
			mtime = a.mtime
		}
		if err := s.FS.Chtimes(p, atime, mtime); err != nil {
			return statusOf(err)
		}
	}
	return stOK
}

// keepWritable3 keeps the file at p open for writing before it loses its
// owner's write permission, for writes the client still sends (keptFiles).
func (s *Server) keepWritable3(p string) {
	id := s.FS.FileID(p)
	if s.v3.kept.has(keptKey{id: id}) {
		return
	}
	if f, err := s.FS.OpenFile(p, os.O_WRONLY, 0); err == nil {
		s.v3.kept.keep(keptKey{id: id}, f)
	}
}

// truncate3 sets the size of the file at p, through its kept file when the
// backend refuses to open it for writing (keptFiles). Size 0 opens with
// O_TRUNC, so a file from a lower layer is not copied up only to be emptied.
func (s *Server) truncate3(p string, size uint64) status {
	if size > math.MaxInt64 {
		return stInval
	}
	flag := os.O_WRONLY
	if size == 0 {
		flag |= os.O_TRUNC
	}
	f, err := s.FS.OpenFile(p, flag, 0)
	if errors.Is(err, os.ErrPermission) {
		if ok, kerr := s.v3.kept.with(keptKey{id: s.FS.FileID(p)}, func(f billy.File) error { return f.Truncate(int64(size)) }); ok {
			return statusOf(kerr)
		}
	}
	if err != nil {
		return statusOf(err)
	}
	if size != 0 {
		err = f.Truncate(int64(size))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return statusOf(err)
}
