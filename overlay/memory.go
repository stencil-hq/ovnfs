package overlay

import (
	"container/list"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// fileOverhead is a memory file's charge beyond its data, so that many empty
// files still count against the budget.
const fileOverhead = 256

// victimScan bounds how many cold files one admission inspects for spilling.
const victimScan = 64

// memModeBits are the mode bits a memory file keeps: permissions plus setuid,
// setgid and sticky, the bits chmod(2) sets on a regular file.
const memModeBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// errAppendWriteAt matches the os package's refusal of positioned appends.
var errAppendWriteAt = errors.New("os: invalid use of WriteAt on file opened with O_APPEND")

// memInodes numbers memory files for stat data; host inodes never reach this range.
var memInodes atomic.Uint64

// root skips the owner permission check, as the kernel does for root.
var root = os.Geteuid() == 0

func init() { memInodes.Store(1 << 62) }

// Memory is the byte budget overlays share for keeping written upper files in
// memory instead of on disk. New, truncated and small copied-up regular files
// start in memory; a file moves into its upper (spills) once it has been left
// untouched for the idle age, when it outgrows the per-file limit, when newer
// files need the budget (least recently used first), before an operation that
// needs a host file (hard link, chown, lock), and at [FS.Flush]. Short-lived
// files never reach the disk. Directories, symlinks and whiteouts always live
// on disk. Memory contents do not survive a crash unless synced with
// SyncOnFsync enabled; pass no Memory to [New] to write all files to disk.
type Memory struct {
	// SyncOnFsync spills and syncs a named memory file when its handle is
	// synced. Set before sharing the budget with any overlay.
	SyncOnFsync bool

	limit, fileMax int64
	idle           time.Duration

	mu      sync.Mutex
	used    int64
	lru     list.List // *memFile, most recently used first
	writing bool      // the writeback worker runs
}

// NewMemory returns a budget of limit bytes that keeps files of up to fileMax
// bytes, writing back files untouched for idle (0 keeps them until memory
// pressure or [FS.Flush]).
func NewMemory(limit, fileMax int64, idle time.Duration) *Memory {
	return &Memory{limit: limit, fileMax: min(fileMax, limit), idle: idle}
}

// writeback spills files left untouched for the idle age, oldest first, so
// snapshots and crashes find only recent files in memory. It exits once no
// file is waiting; admit restarts it.
func (mem *Memory) writeback() {
	for {
		mem.mu.Lock()
		e := mem.lru.Back()
		if e == nil {
			mem.writing = false
			mem.mu.Unlock()
			return
		}
		m := e.Value.(*memFile)
		if wait := mem.idle - time.Since(m.touched); wait > 0 {
			mem.mu.Unlock()
			time.Sleep(wait)
			continue
		}
		owner := m.owner
		mem.mu.Unlock()
		owner.writeBack(m)
	}
}

// park takes m out of the LRU after a failed spill, keeping its charge, so
// eviction and writeback stop retrying it; its next write or [FS.Flush] tries again.
func (mem *Memory) park(m *memFile) {
	mem.mu.Lock()
	if m.elem != nil {
		mem.lru.Remove(m.elem)
		m.elem = nil
	}
	mem.mu.Unlock()
}

// Usage reports the bytes charged to memory files and the budget's limit.
func (mem *Memory) Usage() (used, limit int64) {
	mem.mu.Lock()
	defer mem.mu.Unlock()
	return mem.used, mem.limit
}

// touch marks m as the most recently used file.
func (mem *Memory) touch(m *memFile) {
	mem.mu.Lock()
	if m.elem != nil {
		mem.lru.MoveToFront(m.elem)
		m.touched = time.Now()
	}
	mem.mu.Unlock()
}

// uncharge returns n of m's bytes after its data shrank.
func (mem *Memory) uncharge(m *memFile, n int64) {
	mem.mu.Lock()
	mem.used -= n
	m.charged -= n
	mem.mu.Unlock()
}

// release returns m's whole charge and forgets its LRU position.
func (mem *Memory) release(m *memFile) {
	mem.mu.Lock()
	if m.elem != nil {
		mem.lru.Remove(m.elem)
		m.elem = nil
	}
	mem.used -= m.charged
	m.charged = 0
	mem.mu.Unlock()
}

// victim picks the least recently used file other than keep that can spill
// now: one of f's own (the caller holds f.mu) or another overlay's whose lock
// it takes without waiting, so admissions never deadlock across overlays. The
// caller holds mem.mu and must unlock a foreign owner after spilling.
func (mem *Memory) victim(f *FS, keep *memFile) *memFile {
	e := mem.lru.Back()
	for range victimScan {
		if e == nil {
			return nil
		}
		m := e.Value.(*memFile)
		e = e.Prev()
		if m == keep {
			continue
		}
		if m.owner == f || m.owner.mu.TryLock() {
			return m
		}
	}
	return nil
}

// memFile is an upper regular file whose data lives in memory. It has exactly
// one name (hard links spill it first); path is empty once it is unlinked,
// while open handles keep reading and writing its data.
type memFile struct {
	owner               *FS
	path                string
	data                []byte
	mode                os.FileMode // memModeBits only
	atime, mtime, ctime time.Time
	created             time.Time
	ino                 uint64
	uid, gid            uint32 // what a host create in its directory gets
	handles             map[*memHandle]struct{}
	version             uint64        // counts changes of data and metadata, see [FS.writeBack]
	elem                *list.Element // LRU position while named; guarded by Memory.mu
	touched             time.Time     // last open or write; guarded by Memory.mu
	charged             int64
}

// info snapshots m's metadata under the given base name.
func (m *memFile) info(name string) os.FileInfo {
	return memInfo{name: name, size: int64(len(m.data)), mode: m.mode, atime: m.atime, mtime: m.mtime, ctime: m.ctime, created: m.created, ino: m.ino, uid: m.uid, gid: m.gid}
}

// access makes the owner permission check the kernel makes when the daemon
// reopens its own upper file; a file's creating open skips it.
func (m *memFile) access(name string, flag int) error {
	if root || (!readable(flag) || m.mode&0o400 != 0) && (!writable(flag) || m.mode&0o200 != 0) {
		return nil
	}
	return &fs.PathError{Op: "open", Path: name, Err: syscall.EACCES}
}

// resize sets m's length within its capacity, zeroing bytes that growth exposes.
func (m *memFile) resize(n int64) {
	old := int64(len(m.data))
	m.data = m.data[:n]
	if n > old {
		clear(m.data[old:])
	}
}

// memInfo is a point-in-time FileInfo of a memory file with host-style stat data.
type memInfo struct {
	name                string
	size                int64
	mode                os.FileMode
	atime, mtime, ctime time.Time
	created             time.Time
	ino                 uint64
	uid, gid            uint32
}

// Name returns the base name.
func (i memInfo) Name() string { return i.name }

// Size returns the data length.
func (i memInfo) Size() int64 { return i.size }

// Mode returns the permission bits of a regular file.
func (i memInfo) Mode() os.FileMode { return i.mode }

// ModTime returns the last data or chtimes change.
func (i memInfo) ModTime() time.Time { return i.mtime }

// IsDir is always false: directories live on disk.
func (memInfo) IsDir() bool { return false }

// Sys returns synthetic stat data with the owner the file has on disk.
func (i memInfo) Sys() any { return memStat(i) }

// admit charges n more bytes to m, linking it into the budget's LRU, after
// spilling least recently used files while the budget is exhausted. It reports
// false when nothing more can spill; the caller then keeps the data on disk.
// The caller holds f.mu.
func (f *FS) admit(m *memFile, n int64) bool {
	mem := f.mem
	for {
		mem.mu.Lock()
		if mem.used+n <= mem.limit {
			mem.used += n
			m.charged += n
			if m.elem == nil {
				m.elem = mem.lru.PushFront(m)
			} else {
				mem.lru.MoveToFront(m.elem)
			}
			m.touched = time.Now()
			if mem.idle > 0 && !mem.writing {
				mem.writing = true
				go mem.writeback()
			}
			mem.mu.Unlock()
			return true
		}
		v := mem.victim(f, m)
		mem.mu.Unlock()
		if v == nil {
			return false
		}
		owner := v.owner
		err := owner.spill(v)
		if err != nil {
			mem.park(v)
		}
		if owner != f {
			owner.mu.Unlock()
		}
		if err != nil {
			return false
		}
	}
}

// addMem publishes p as a memory file holding data, leaving the parent's
// mtime to the caller: a new name stamps it ([FS.touchDir]), a copy-up keeps
// it. It returns nil without error when the budget refuses the file. The
// caller ensured p's parent exists in upper and p has no upper entry.
func (f *FS) addMem(p string, mode os.FileMode, mtime time.Time, data []byte) (*memFile, error) {
	now := time.Now()
	m := &memFile{owner: f, path: p, data: data, mode: mode & memModeBits, atime: mtime, mtime: mtime, ctime: now, created: now, ino: memInodes.Add(1), handles: make(map[*memHandle]struct{})}
	m.uid, m.gid = newOwner(f.index.entries[parent(p)].Info)
	if !f.admit(m, fileOverhead+int64(cap(data))) {
		return nil, nil
	}
	if err := f.clearWhiteout(p); err != nil {
		f.mem.release(m)
		return nil, err
	}
	f.files[p] = m
	f.index.set(p, Entry{Kind: KindFile, Info: m.info(path.Base(p))})
	return m, nil
}

// copyUpMem copies a small immutable regular file into memory, reporting false
// when it must be copied to disk instead (too large, or no budget).
func (f *FS) copyUpMem(s source, p string, info os.FileInfo) (bool, error) {
	if f.mem == nil || !info.Mode().IsRegular() || info.Size() > f.mem.fileMax {
		return false, nil
	}
	var data []byte
	var err error
	if s.layer != nil {
		data, err = s.layer.ReadFile(p)
	} else {
		var c Content
		if c, err = f.bottom.Open(p); err == nil {
			data, err = readBounded(c, info.Size(), f.mem.fileMax)
			err = errors.Join(err, c.Close())
		}
	}
	if err != nil || int64(len(data)) > f.mem.fileMax {
		return false, err
	}
	m, err := f.addMem(p, info.Mode(), info.ModTime(), data)
	return m != nil, err
}

// readBounded reads r, sized for about size bytes, stopping once it holds more
// than limit so callers can reject content that grew.
func readBounded(r io.Reader, size, limit int64) ([]byte, error) {
	buf := make([]byte, 0, min(size, limit)+1)
	for {
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF || int64(len(buf)) > limit {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, len(buf))
		}
	}
}

// touchDir stamps directory d's mtime as a host create or unlink would, for a
// name change that happened only in memory.
func (f *FS) touchDir(d string) error {
	if err := os.Chtimes(f.up(d), time.Time{}, time.Now()); err != nil {
		return err
	}
	return f.refresh(d)
}

// grow makes room for size bytes of m's data, spilling m instead when it would
// outgrow the per-file limit or the budget. It reports whether m is still in
// memory. Unlinked files cannot spill and grow unbudgeted.
func (f *FS) grow(m *memFile, size int64) (bool, error) {
	c := int64(cap(m.data))
	if size <= c {
		return true, nil
	}
	next := max(size, 2*c, 4096)
	if m.path != "" {
		if size > f.mem.fileMax {
			return false, f.spill(m)
		}
		next = min(next, f.mem.fileMax)
		if !f.admit(m, next-c) {
			return false, f.spill(m)
		}
	}
	data := make([]byte, len(m.data), next)
	copy(data, m.data)
	m.data = data
	return true, nil
}

// truncateMem sets m's size, reporting false when it spilled instead. Emptying
// a named file returns its capacity to the budget.
func (f *FS) truncateMem(m *memFile, size int64) (bool, error) {
	in, err := f.grow(m, size)
	if !in || err != nil {
		return false, err
	}
	if size == 0 && m.path != "" {
		f.mem.uncharge(m, int64(cap(m.data)))
		m.data = nil
	} else {
		m.resize(size)
	}
	now := time.Now()
	m.mtime, m.ctime = now, now
	m.version++
	return true, nil
}

// writeBack spills m for the writeback worker if it is still waiting in
// memory and still idle. A file without open handles is staged from a copy
// without f.mu, so the checkout's lookups and opens go on during the host
// create, write and close (milliseconds where Windows Defender scans new
// files); it replaces m only if m did not change meanwhile, else a later
// round retries. [FS.Flush] and [FS.Discard] wait for a staging in progress.
func (f *FS) writeBack(m *memFile) {
	f.staging.RLock()
	defer f.staging.RUnlock()
	f.mu.Lock()
	f.mem.mu.Lock()
	due := m.elem != nil && time.Since(m.touched) >= f.mem.idle
	f.mem.mu.Unlock()
	if !due {
		f.mu.Unlock()
		return
	}
	if len(m.handles) != 0 {
		if f.spill(m) != nil {
			f.mem.park(m)
		}
		f.mu.Unlock()
		return
	}
	copied := &memFile{data: slices.Clone(m.data), mode: m.mode, atime: m.atime, mtime: m.mtime, created: m.created, uid: m.uid, gid: m.gid}
	version := m.version
	tmp := f.stagingName()
	f.mu.Unlock()
	_, err := stage(tmp, copied)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && m.version == version && m.path != "" && f.files[m.path] == m && len(m.handles) == 0 {
		p := m.path
		err = f.preserveDirTime(parent(p), func() error { return hostfile.Rename(tmp, f.up(p)) })
		if err == nil {
			if f.installed(m, nil) != nil {
				f.mem.park(m)
			}
			return
		}
	}
	os.Remove(tmp)
	if err != nil {
		f.mem.park(m)
	}
}

// stagingName returns a new private name in the work directory. The caller holds f.mu.
func (f *FS) stagingName() string {
	f.tmpSeq++
	return filepath.Join(f.work, strconv.FormatUint(f.tmpSeq, 36))
}

// detach removes m from the namespace and the budget; open handles keep its data.
func (f *FS) detach(m *memFile) {
	delete(f.files, m.path)
	m.path = ""
	f.mem.release(m)
}

// spill moves m into the upper as an ordinary file and hands each open handle
// a host descriptor at its offset. The caller holds f.mu; m must be named.
func (f *FS) spill(m *memFile) error {
	p := m.path
	tmp := f.stagingName()
	fds, err := stage(tmp, m)
	if err == nil {
		err = f.preserveDirTime(parent(p), func() error { return hostfile.Rename(tmp, f.up(p)) })
	}
	if err != nil {
		for _, fd := range fds {
			fd.Close()
		}
		os.Remove(tmp)
		return err
	}
	return f.installed(m, fds)
}

// installed retires memory file m once its staged copy holds its name in the
// upper, handing each open handle its staged descriptor. The caller holds f.mu.
func (f *FS) installed(m *memFile, fds map[*memHandle]*os.File) error {
	p := m.path
	delete(f.files, p)
	f.mem.release(m)
	err := f.refresh(p)
	for h, fd := range fds {
		h.disk = f.adopt(&file{File: fd, name: h.name, writable: writable(h.flag)})
		f.bind(h.disk, p)
		h.m = nil
		f.memoryHandles--
	}
	clear(m.handles)
	m.data = nil
	return err
}

// stage writes m's data and metadata to the new file tmp and opens a host
// descriptor for each handle. Descriptors open before the final chmod, which
// may forbid their access mode.
func stage(tmp string, m *memFile) (map[*memHandle]*os.File, error) {
	out, err := hostfile.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// A file shows the same owner in memory and on disk (see newOwner); the
	// staging directory need not hand out the group the final one does.
	err = matchOwner(out, m.uid, m.gid)
	if err == nil {
		_, err = out.Write(m.data)
	}
	fds := make(map[*memHandle]*os.File, len(m.handles))
	for h := range m.handles {
		if err != nil {
			break
		}
		var fd *os.File
		if fd, err = hostfile.OpenFile(tmp, h.flag&^(os.O_CREATE|os.O_EXCL|os.O_TRUNC), 0); err != nil {
			break
		}
		fds[h] = fd
		if h.flag&os.O_APPEND == 0 {
			_, err = fd.Seek(h.off, io.SeekStart)
		}
	}
	if err == nil {
		err = out.Chmod(m.mode)
	}
	err = errors.Join(err, out.Close())
	if err == nil {
		err = keepMode(tmp, m.mode)
	}
	if err == nil {
		err = os.Chtimes(tmp, m.atime, m.mtime)
	}
	if err == nil {
		err = setCreationTime(tmp, m.created)
	}
	return fds, err
}

// newHandle registers a handle on m so a spill can hand it a host descriptor.
func (f *FS) newHandle(m *memFile, name string, flag int) *memHandle {
	h := &memHandle{fs: f, name: name, flag: flag, m: m}
	f.memoryHandles++
	m.handles[h] = struct{}{}
	f.mem.touch(m)
	return h
}

// openMem opens an existing memory file like a host open: permission check,
// then truncation for O_TRUNC writers.
func (f *FS) openMem(m *memFile, name string, flag int) (billy.File, error) {
	if err := m.access(name, flag); err != nil {
		return nil, err
	}
	h := f.newHandle(m, name, flag)
	if flag&os.O_TRUNC == 0 || !writable(flag) {
		return h, nil
	}
	in, err := f.truncateMem(m, 0)
	if err == nil && !in {
		err = h.disk.File.Truncate(0)
	}
	if err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Flush spills every memory file into the upper, after which the upper
// directory alone holds this overlay's changes. Snapshots, diffs and shutdown
// call it before reading the upper from disk.
func (f *FS) Flush() error {
	f.staging.Lock()
	defer f.staging.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.files {
		if err := f.spill(m); err != nil {
			return err
		}
	}
	return nil
}

// Discard drops every memory file without writing it and returns its budget,
// for an overlay whose upper is being deleted.
func (f *FS) Discard() {
	f.staging.Lock()
	defer f.staging.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.files {
		f.detach(m)
	}
}

// Unflushed reports whether some file data lives only in memory, leaving out
// files at or below any of the paths except.
func (f *FS) Unflushed(except ...string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for p := range f.files {
		if !slices.ContainsFunc(except, func(dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }) {
			return true
		}
	}
	return false
}

// writable reports an access mode that permits writes.
func writable(flag int) bool { return flag&(os.O_WRONLY|os.O_RDWR) != 0 }

// readable reports an access mode that permits reads.
func readable(flag int) bool { return flag&(os.O_WRONLY|os.O_RDWR) != os.O_WRONLY }

// memHandle is an open descriptor of a memory file with its own offset. When
// the file spills, a host descriptor takes over for good and the handle
// forwards to it. Fields other than disk's target are guarded by fs.mu.
type memHandle struct {
	fs     *FS
	name   string
	flag   int
	m      *memFile // nil once spilled
	off    int64
	disk   *file // set once, by spill
	closed bool
}

// lock takes fs.mu for a memory-backed call, or returns the host file that
// took over (with fs.mu released).
func (h *memHandle) lock() *file {
	h.fs.mu.Lock()
	if d := h.disk; d != nil {
		h.fs.mu.Unlock()
		return d
	}
	return nil
}

// rlock is lock for calls that only read the file.
func (h *memHandle) rlock() *file {
	h.fs.mu.RLock()
	if d := h.disk; d != nil {
		h.fs.mu.RUnlock()
		return d
	}
	return nil
}

// check rejects calls on a closed handle or one whose access mode forbids op.
func (h *memHandle) check(op string, allowed func(int) bool) error {
	if h.closed {
		return &fs.PathError{Op: op, Path: h.name, Err: os.ErrClosed}
	}
	if allowed != nil && !allowed(h.flag) {
		return &fs.PathError{Op: op, Path: h.name, Err: syscall.EBADF}
	}
	return nil
}

// Name returns the overlay-relative name the handle was opened with.
func (h *memHandle) Name() string { return h.name }

// Read reads at the handle's offset and advances it.
func (h *memHandle) Read(p []byte) (int, error) {
	if d := h.lock(); d != nil {
		return d.Read(p)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("read", readable); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if h.off >= int64(len(h.m.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.m.data[h.off:])
	h.off += int64(n)
	return n, nil
}

// ReadAt reads at off without moving the offset, failing with io.EOF when short.
func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	if d := h.rlock(); d != nil {
		return d.ReadAt(p, off)
	}
	defer h.fs.mu.RUnlock()
	if err := h.check("read", readable); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, &fs.PathError{Op: "readat", Path: h.name, Err: syscall.EINVAL}
	}
	if off >= int64(len(h.m.data)) {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	n := copy(p, h.m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write writes at the offset (or the end for O_APPEND) and advances it.
func (h *memHandle) Write(p []byte) (int, error) {
	if d := h.lock(); d != nil {
		return d.Write(p)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("write", writable); err != nil {
		return 0, err
	}
	off := h.off
	if h.flag&os.O_APPEND != 0 {
		off = int64(len(h.m.data))
	}
	return h.write(p, off, true)
}

// WriteAt writes at off without moving the offset.
func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	if d := h.lock(); d != nil {
		return d.WriteAt(p, off)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("writeat", writable); err != nil {
		return 0, err
	}
	if h.flag&os.O_APPEND != 0 {
		return 0, errAppendWriteAt
	}
	if off < 0 {
		return 0, &fs.PathError{Op: "writeat", Path: h.name, Err: syscall.EINVAL}
	}
	return h.write(p, off, false)
}

// write stores p at off in memory, or through the host descriptor when the
// write spills the file; seq moves the offset. The caller holds fs.mu.
func (h *memHandle) write(p []byte, off int64, seq bool) (int, error) {
	m := h.m
	end := off + int64(len(p))
	in, err := h.fs.grow(m, end)
	if err != nil {
		return 0, err
	}
	if !in {
		var n int
		if seq {
			n, err = h.disk.File.Write(p)
		} else {
			n, err = h.disk.File.WriteAt(p, off)
		}
		return n, err
	}
	if end > int64(len(m.data)) {
		m.resize(end)
	}
	copy(m.data[off:], p)
	now := time.Now()
	m.mtime, m.ctime = now, now
	m.version++
	if m.path != "" {
		h.fs.mem.touch(m)
	}
	if seq {
		h.off = end
	}
	return len(p), nil
}

// Seek moves the offset; positions past the end are allowed, as for host files.
func (h *memHandle) Seek(offset int64, whence int) (int64, error) {
	if d := h.lock(); d != nil {
		return d.Seek(offset, whence)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("seek", nil); err != nil {
		return 0, err
	}
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = h.off + offset
	case io.SeekEnd:
		pos = int64(len(h.m.data)) + offset
	default:
		return 0, &fs.PathError{Op: "seek", Path: h.name, Err: syscall.EINVAL}
	}
	if pos < 0 {
		return 0, &fs.PathError{Op: "seek", Path: h.name, Err: syscall.EINVAL}
	}
	h.off = pos
	return pos, nil
}

// Truncate resizes the file, spilling it when it outgrows memory.
func (h *memHandle) Truncate(size int64) error {
	if d := h.lock(); d != nil {
		return d.Truncate(size)
	}
	defer h.fs.mu.Unlock()
	if err := h.check("truncate", writable); err != nil {
		return err
	}
	if size < 0 {
		return &fs.PathError{Op: "truncate", Path: h.name, Err: syscall.EINVAL}
	}
	in, err := h.fs.truncateMem(h.m, size)
	if err != nil || in {
		return err
	}
	return h.disk.File.Truncate(size)
}

// Stat describes the file as it is now.
func (h *memHandle) Stat() (os.FileInfo, error) {
	if d := h.rlock(); d != nil {
		return d.Stat()
	}
	defer h.fs.mu.RUnlock()
	if err := h.check("stat", nil); err != nil {
		return nil, err
	}
	return h.m.info(path.Base(h.name)), nil
}

// Sync spills and syncs a named file when configured, otherwise it succeeds
// without I/O. An unlinked memory file has no host file to sync.
func (h *memHandle) Sync() error {
	if !h.fs.mem.SyncOnFsync {
		if d := h.rlock(); d != nil {
			return d.Sync()
		}
		defer h.fs.mu.RUnlock()
		return h.check("sync", nil)
	}
	if d := h.lock(); d != nil {
		return d.Sync()
	}
	if err := h.check("sync", nil); err != nil || h.m.path == "" {
		h.fs.mu.Unlock()
		return err
	}
	err := h.fs.spill(h.m)
	d := h.disk
	h.fs.mu.Unlock()
	if err != nil {
		return err
	}
	return d.Sync()
}

// Lock spills the file so an advisory lock reaches a host descriptor.
// An unlinked file has no host file; locking it succeeds trivially.
func (h *memHandle) Lock() error {
	if d := h.lock(); d != nil {
		return d.Lock()
	}
	if err := h.check("lock", nil); err != nil || h.m.path == "" {
		h.fs.mu.Unlock()
		return err
	}
	err := h.fs.spill(h.m)
	d := h.disk
	h.fs.mu.Unlock()
	if err != nil {
		return err
	}
	return d.Lock()
}

// Unlock releases a lock taken after spilling; a memory handle holds none.
func (h *memHandle) Unlock() error {
	if d := h.lock(); d != nil {
		return d.Unlock()
	}
	defer h.fs.mu.Unlock()
	return h.check("unlock", nil)
}

// Close releases the handle; the data stays with the file.
func (h *memHandle) Close() error {
	if d := h.lock(); d != nil {
		return d.Close()
	}
	defer h.fs.mu.Unlock()
	if err := h.check("close", nil); err != nil {
		return err
	}
	h.closed = true
	delete(h.m.handles, h)
	h.fs.memoryHandles--
	return nil
}
