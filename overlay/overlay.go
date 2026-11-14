// Package overlay merges a writable upper over indexed immutable layers and a
// read-only bottom. AUFS whiteouts and opaque directories remain a 1:1 on-disk
// diff. The filesystem is upper's only writer; it is not a symlink sandbox.
package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/helper/chroot"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// Marker names are reserved in every component of an overlay path.
const (
	whPrefix   = ".wh."
	metaPrefix = ".wh..wh."
	opaqueName = metaPrefix + ".opq"
	workName   = metaPrefix + "work"
	writeFlags = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_CREATE | os.O_TRUNC
)

// FS is a concurrent-safe writable upper over immutable layers and a bottom.
// The index and cached inode metadata are kept current under mu.
type FS struct {
	mu          sync.RWMutex
	upper, work string
	layers      []*Layer
	bottom      Bottom
	bottomEpoch uint64
	index       *Layer
	tmpSeq      uint64
	pathIDs     map[string]inodeKey
	aliases     map[inodeKey]inodeNames
	mem         *Memory             // nil: every write goes to upper on disk
	files       map[string]*memFile // upper names whose data is in memory
	hosts       map[*file]struct{}  // open descriptors of upper files, see [FS.CloseFiles]
	writers     map[inodeKey]int    // open writable descriptors per upper inode, see [FS.upperInfo]
	staging     sync.RWMutex        // shared by an unlocked writeback, see [FS.writeBack]
}

// Filesystem interface assertions keep the overlay compatible with protocols.
var (
	_ billy.Filesystem = (*FS)(nil)
	_ billy.Change     = (*FS)(nil)
)

// New opens upper over layers (top first) and bottom. An empty upper is
// read-only. With mem, small written files stay in memory under its budget
// until they spill into upper (see [Memory]); nil keeps every write on disk.
func New(upper string, layers []*Layer, bottom Bottom, mem *Memory) (*FS, error) {
	f := &FS{layers: slices.Clone(layers), bottom: bottom, hosts: make(map[*file]struct{}), writers: make(map[inodeKey]int)}
	for _, l := range layers {
		if l == nil {
			return nil, errors.New("overlay: nil layer")
		}
	}
	if upper == "" {
		return f, nil
	}
	f.pathIDs = make(map[string]inodeKey)
	f.aliases = make(map[inodeKey]inodeNames)
	if mem != nil {
		f.mem, f.files = mem, make(map[string]*memFile)
	}
	var err error
	f.upper, err = filepath.Abs(upper)
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		if within(f.upper, l.dir) || within(l.dir, f.upper) {
			return nil, errors.New("overlay: upper and layers must not contain each other")
		}
	}
	if err := os.MkdirAll(f.upper, 0o755); err != nil {
		return nil, err
	}
	f.work = filepath.Join(f.upper, workName)
	if err := os.RemoveAll(f.work); err != nil {
		return nil, err
	}
	if err := os.Mkdir(f.work, 0o700); err != nil {
		return nil, err
	}
	f.index, err = indexLayer(f.upper, true)
	if err != nil {
		return nil, err
	}
	for p, e := range f.index.entries {
		if err := f.track(p, e.Info); err != nil {
			return nil, err
		}
	}
	// A fresh upper must not change the visible root's timestamp.
	if len(f.index.entries) == 1 {
		idx := f.index
		f.index = nil
		_, info, e := f.lookup("")
		f.index = idx
		if e == nil {
			if err := os.Chtimes(f.upper, info.ModTime(), info.ModTime()); err != nil {
				return nil, err
			}
			if err := f.refresh(""); err != nil {
				return nil, err
			}
		}
	}
	return f, nil
}

// Rebase changes the immutable bottom without disturbing open files or inode identities.
func (f *FS) Rebase(bottom Bottom) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bottom = bottom
	f.bottomEpoch++
}

// Upper returns the host directory receiving writes, or empty for read-only views.
func (f *FS) Upper() string { return f.upper }

// UpperFile reports a regular file owned by this writable upper, including
// memory files. Lease policies use it to exclude immutable and live lower data;
// disk metadata is refreshed so hard-link aliases cannot gain separate caches.
func (f *FS) UpperFile(name string) (os.FileInfo, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := Clean(name)
	if f.index == nil || reserved(p) {
		return nil, false
	}
	entry, ok := f.index.entries[p]
	if !ok || entry.Kind != KindFile {
		return nil, false
	}
	if m := f.files[p]; m != nil {
		return m.info(path.Base(p)), true
	}
	info, err := lstatIndexed(f.up(p), entry.Info)
	return info, err == nil && info.Mode().IsRegular()
}

// Stack returns an independent slice of immutable layers, top first.
func (f *FS) Stack() []*Layer { return slices.Clone(f.layers) }

// Freeze transfers this upper's index to a frozen layer after the caller has
// flushed it ([FS.Flush]), renamed the upper to dir and removed its work
// directory. The caller must exclude concurrent operations and never use this
// FS after a successful call.
func (f *FS) Freeze(dir string) (*Layer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upper == "" || f.index == nil {
		return nil, &fs.PathError{Op: "freeze", Path: dir, Err: syscall.EROFS}
	}
	if len(f.files) != 0 {
		return nil, errors.New("overlay: freeze before flushing memory files")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := hostLstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "freeze", Path: root, Err: syscall.ENOTDIR}
	}
	l := f.index
	if wrote, err := l.saveOrigins(root); err != nil {
		return nil, err
	} else if wrote {
		if info, err = hostLstat(root); err != nil {
			return nil, err
		}
	}
	e := entry(info)
	e.Opaque = l.entries[""].Opaque
	l.set("", e)
	l.dir = root
	f.index = nil
	f.pathIDs = nil
	f.aliases = nil
	f.upper, f.work = "", ""
	return l, nil
}

// Empty reports whether the upper has no data or markers outside its work dir.
func (f *FS) Empty() (bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.upper == "" {
		return true, nil
	}
	if len(f.files) != 0 {
		return false, nil
	}
	items, err := os.ReadDir(f.upper)
	if err != nil {
		return false, err
	}
	for _, item := range items {
		if item.Name() != workName {
			return false, nil
		}
	}
	return true, nil
}

// source identifies the upper, a frozen layer, or the bottom for one lookup.
type source struct {
	layer *Layer
	upper bool
}

// up maps an overlay path into its writable upper.
func (f *FS) up(p string) string { return filepath.Join(f.upper, p) }

// lookup walks only indexed layers before consulting the bottom.
func (f *FS) lookup(p string) (source, os.FileInfo, error) {
	if reserved(p) {
		return source{}, nil, notExist("lstat", p)
	}
	if f.index != nil {
		if e, ok := f.index.entries[p]; ok {
			if e.Kind == KindWhiteout {
				return source{}, nil, notExist("lstat", p)
			}
			if m := f.files[p]; m != nil {
				return source{layer: f.index, upper: true}, m.info(path.Base(p)), nil
			}
			info, err := f.upperInfo(p, e)
			return source{layer: f.index, upper: true}, info, err
		}
		if f.index.barrier(p) {
			return source{}, nil, notExist("lstat", p)
		}
	}
	for _, l := range f.layers {
		if e, ok := l.entries[p]; ok {
			if e.Kind == KindWhiteout {
				return source{}, nil, notExist("lstat", p)
			}
			return source{layer: l}, e.Info, nil
		}
		if l.barrier(p) {
			return source{}, nil, notExist("lstat", p)
		}
	}
	if f.bottom == nil {
		return source{}, nil, notExist("lstat", p)
	}
	info, err := f.bottom.Lstat(p)
	if missing(err) {
		return source{}, nil, notExist("lstat", p)
	}
	return source{}, info, err
}

// lowerHas detects a name in any immutable source, even if currently hidden.
func (f *FS) lowerHas(p string) (bool, error) {
	for _, l := range f.layers {
		if e, ok := l.entries[p]; ok && e.Kind != KindWhiteout {
			return true, nil
		}
	}
	if f.bottom == nil {
		return false, nil
	}
	_, err := f.bottom.Lstat(p)
	if missing(err) {
		return false, nil
	}
	return err == nil, err
}

// readlink reads the selected backing source without doing another lookup.
func (f *FS) readlink(s source, p string) (string, error) {
	if s.layer != nil {
		return s.layer.Readlink(p)
	}
	return f.bottom.Readlink(p)
}

// InBottom reports whether name, without following a final symlink, is
// served by the bottom: neither the upper nor a frozen layer provides or
// hides it.
func (f *FS) InBottom(name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, _, err := f.lookup(Clean(name))
	return err == nil && s.layer == nil
}

// Lstat describes name without following its final symlink.
func (f *FS) Lstat(name string) (os.FileInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, info, err := f.lookup(Clean(name))
	return info, err
}

// Stat describes name, resolving relative final symlinks within the overlay.
func (f *FS) Stat(name string) (os.FileInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := Clean(name)
	for range 40 {
		s, info, err := f.lookup(p)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return info, err
		}
		target, err := f.readlink(s, p)
		if err != nil {
			return nil, err
		}
		if filepath.IsAbs(target) {
			return os.Stat(target)
		}
		p = Clean(path.Join(path.Dir(p), target))
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: syscall.ELOOP}
}

// Readlink returns a symlink's stored target.
func (f *FS) Readlink(name string) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p := Clean(name)
	s, _, err := f.lookup(p)
	if err != nil {
		return "", err
	}
	return f.readlink(s, p)
}

// ReadDir merges direct entries from all visible levels in name order.
func (f *FS) ReadDir(name string) ([]os.FileInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.readDir(Clean(name))
}

// readDir applies ancestor barriers, whiteouts, opaque directories and shadows.
func (f *FS) readDir(p string) ([]os.FileInfo, error) {
	if !reserved(p) && f.bottomOnly(p) {
		items, err := f.bottom.ReadDir(p)
		if err != nil {
			return nil, err
		}
		return visibleEntries(items), nil
	}
	_, info, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	seen := make(map[string]bool)
	out := make([]os.FileInfo, 0)
	merge := func(l *Layer, upper bool) (bool, error) {
		e, present := l.entries[p]
		if present && e.Kind != KindDir {
			return true, nil
		}
		for n, c := range l.children[p] {
			if seen[n] {
				continue
			}
			seen[n] = true
			if c.Kind == KindWhiteout {
				continue
			}
			ci := c.Info
			if m := f.files[path.Join(p, n)]; upper && m != nil {
				ci = m.info(n)
			} else if upper {
				var err error
				ci, err = f.upperInfo(path.Join(p, n), c)
				if err != nil {
					return false, err
				}
			}
			out = append(out, ci)
		}
		return (present && e.Opaque) || l.barrier(p), nil
	}
	stop := false
	if f.index != nil {
		stop, err = merge(f.index, true)
		if err != nil {
			return nil, err
		}
	}
	for _, l := range f.layers {
		if stop {
			break
		}
		stop, err = merge(l, false)
		if err != nil {
			return nil, err
		}
	}
	if !stop && f.bottom != nil {
		items, err := f.bottom.ReadDir(p)
		if err != nil && !missing(err) {
			return nil, err
		}
		for _, item := range items {
			if !seen[item.Name()] && !strings.HasPrefix(item.Name(), whPrefix) {
				out = append(out, item)
			}
		}
	}
	slices.SortFunc(out, func(a, b os.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// bottomOnly reports whether no upper or layer level has an entry, child or
// barrier at directory p, so the bottom listing alone is the result.
func (f *FS) bottomOnly(p string) bool {
	if f.bottom == nil {
		return false
	}
	for _, l := range f.layers {
		if !l.untouched(p) {
			return false
		}
	}
	return f.index == nil || f.index.untouched(p)
}

// untouched reports whether l has no entry, child or barrier for directory p.
func (l *Layer) untouched(p string) bool {
	_, present := l.entries[p]
	return !present && len(l.children[p]) == 0 && !l.barrier(p)
}

// visibleEntries filters reserved names in the bottom's caller-owned listing.
func visibleEntries(items []os.FileInfo) []os.FileInfo {
	if items == nil {
		return []os.FileInfo{}
	}
	out := items[:0]
	for _, item := range items {
		if !strings.HasPrefix(item.Name(), whPrefix) {
			out = append(out, item)
		}
	}
	clear(items[len(out):])
	return out
}

// Open opens name read-only from whichever level provides it.
func (f *FS) Open(name string) (billy.File, error) { return f.OpenFile(name, os.O_RDONLY, 0) }

// openLower opens a regular file served by a frozen layer or the bottom for
// reading, holding the shared lock only for the lookup: that content never
// changes under f, so readers of different files do not queue behind each
// other's host I/O. A write that copies the file up meanwhile orders after
// this open. ok is false for anything else (a symlink, a directory, an upper
// or missing name), which [FS.OpenFile] decides under the exclusive lock.
func (f *FS) openLower(name string, flag int) (h billy.File, ok bool, err error) {
	p := Clean(name)
	f.mu.RLock()
	s, info, err := f.lookup(p)
	bottom := f.bottom
	f.mu.RUnlock()
	if err != nil || s.upper || !info.Mode().IsRegular() {
		return nil, false, nil
	}
	if s.layer != nil {
		h, err := s.layer.open(p, p, flag)
		return h, true, err
	}
	c, err := bottom.Open(p)
	if err != nil {
		return nil, true, err
	}
	return wrapContent(c, p), true, nil
}

// Create creates or truncates a file in the writable upper.
func (f *FS) Create(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// OpenFile copies immutable data up before opening with any write intent.
func (f *FS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	writing := flag&writeFlags != 0
	if !writing {
		if h, ok, err := f.openLower(name, flag); ok {
			return h, err
		}
	} else {
		f.staging.RLock()
		defer f.staging.RUnlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
retry:
	p := Clean(name)
	if writing {
		if err := f.mutable("open", p); err != nil {
			return nil, err
		}
		if p == "" || reserved(p) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EPERM}
		}
	}
	original := p
	s, info, err := f.lookup(p)
	if err == nil && writing && flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EEXIST}
	}
	for links := 0; err == nil && info.Mode()&os.ModeSymlink != 0; links++ {
		if links == 40 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.ELOOP}
		}
		var target string
		var linkErr error
		if s.layer != nil {
			target, linkErr = f.readlink(s, p)
		} else {
			bottom, epoch := f.bottom, f.bottomEpoch
			f.mu.Unlock()
			target, linkErr = bottom.Readlink(p)
			f.mu.Lock()
			current, currentInfo, currentErr := f.lookup(p)
			if f.bottomEpoch != epoch || currentErr != nil || current.layer != nil || currentInfo.Mode()&os.ModeSymlink == 0 {
				goto retry
			}
		}
		if linkErr != nil {
			return nil, linkErr
		}
		if filepath.IsAbs(target) {
			return open(target, original, flag, perm, nil)
		}
		p = Clean(path.Join(parent(p), target))
		s, info, err = f.lookup(p)
	}
	if !writing {
		if err != nil {
			return nil, err
		}
		if m := f.files[p]; m != nil {
			return f.openMem(m, original, flag)
		}
		if s.upper {
			h, err := open(f.up(p), original, flag, 0, nil)
			if err != nil {
				return nil, err
			}
			f.adopt(h)
			f.bind(h, p)
			return h, nil
		}
		if s.layer != nil {
			return s.layer.open(p, original, flag)
		}
		if !info.Mode().IsRegular() {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
		}
		bottom := f.bottom
		f.mu.Unlock()
		fh, err := bottom.Open(p)
		f.mu.Lock()
		if err != nil {
			return nil, err
		}
		return wrapContent(fh, original), nil
	}
	if p == "" || reserved(p) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EPERM}
	}
	// startMem makes p an empty memory file; nil without error means disk.
	startMem := func(mode os.FileMode) (*memFile, error) {
		if f.mem == nil {
			return nil, nil
		}
		return f.addMem(p, mode, time.Now(), nil)
	}
	creating := err != nil
	if err == nil {
		if m := f.files[p]; m != nil {
			return f.openMem(m, original, flag)
		}
		if !s.upper {
			if info.Mode().IsRegular() && flag&os.O_TRUNC != 0 {
				if err := f.ensureDir(parent(p)); err != nil {
					return nil, err
				}
				if m, err := startMem(info.Mode().Perm()); m != nil || err != nil {
					if err != nil {
						return nil, err
					}
					return f.openMem(m, original, flag)
				}
				err := f.preserveDirTime(parent(p), func() error {
					fh, err := os.OpenFile(f.up(p), os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
					if err != nil {
						return err
					}
					if err := fh.Chmod(info.Mode().Perm()); err != nil {
						fh.Close()
						return err
					}
					if err := fh.Close(); err != nil {
						return err
					}
					return keepMode(f.up(p), info.Mode())
				})
				if err != nil {
					return nil, err
				}
				if err := f.refresh(p); err != nil {
					return nil, err
				}
			} else if s.layer == nil && info.Mode().IsRegular() {
				changed, err := f.copyUpBottomForOpen(p, info)
				if err != nil {
					return nil, err
				}
				if changed {
					goto retry
				}
				if m := f.files[p]; m != nil {
					return f.openMem(m, original, flag)
				}
			} else if err := f.copyUp(p); err != nil {
				return nil, err
			} else if m := f.files[p]; m != nil {
				return f.openMem(m, original, flag)
			}
		}
	} else {
		if !errors.Is(err, fs.ErrNotExist) || flag&os.O_CREATE == 0 {
			return nil, err
		}
		if err := f.ensureDir(parent(p)); err != nil {
			return nil, err
		}
		if m, err := startMem(perm & memModeBits &^ umask); m != nil || err != nil {
			if err != nil {
				return nil, err
			}
			// A new name changes the parent like a host create; the creating open skips the permission check.
			if err := f.touchDir(parent(p)); err != nil {
				return nil, err
			}
			return f.newHandle(m, original, flag), nil
		}
	}
	fh, err := open(f.up(p), p, flag, perm, f)
	if err != nil {
		return nil, err
	}
	if creating {
		err = keepMode(f.up(p), perm&^umask)
	}
	if err == nil {
		err = f.clearWhiteout(p)
	}
	if err == nil {
		err = f.refresh(p)
	}
	if err == nil {
		err = f.refresh(parent(p))
	}
	if err != nil {
		delete(f.hosts, fh)
		fh.File.Close()
		return nil, err
	}
	fh.name = original
	f.bind(fh, p)
	return fh, nil
}

// TempFile creates a uniquely named upper file in an existing merged directory.
func (f *FS) TempFile(dir, prefix string) (billy.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := Clean(dir)
	if err := f.mutable("createtemp", d); err != nil {
		return nil, err
	}
	if reserved(d) || strings.HasPrefix(prefix, whPrefix) {
		return nil, &fs.PathError{Op: "createtemp", Path: dir, Err: syscall.EPERM}
	}
	if err := f.ensureDir(d); err != nil {
		return nil, err
	}
	fh, err := hostfile.CreateTemp(f.up(d), prefix)
	if err != nil {
		return nil, err
	}
	p := path.Join(d, filepath.Base(fh.Name()))
	if err := keepMode(fh.Name(), 0o600); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.clearWhiteout(p); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.refresh(p); err != nil {
		fh.Close()
		return nil, err
	}
	if err := f.refresh(d); err != nil {
		fh.Close()
		return nil, err
	}
	h := f.adopt(&file{File: fh, name: p, writable: true})
	f.bind(h, p)
	return h, nil
}

// MkdirAll creates missing directories, making recreated lower names opaque.
func (f *FS) MkdirAll(name string, perm os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Clean(name)
	if err := f.mutable("mkdir", p); err != nil {
		return err
	}
	if reserved(p) {
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.EPERM}
	}
	for i := range len(p) + 1 {
		if i < len(p) && p[i] != '/' {
			continue
		}
		q := p[:i]
		_, info, err := f.lookup(q)
		if err == nil {
			if !info.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: q, Err: syscall.ENOTDIR}
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := f.ensureDir(parent(q)); err != nil {
			return err
		}
		if err := os.Mkdir(f.up(q), perm); err != nil {
			return err
		}
		if err := keepMode(f.up(q), perm&^umask); err != nil {
			return err
		}
		if err := f.refresh(q); err != nil {
			return err
		}
		if err := f.placeDir(q, false); err != nil {
			return err
		}
		if err := f.refresh(parent(q)); err != nil {
			return err
		}
	}
	return nil
}

// Mknod creates a socket or FIFO in the writable upper.
func (f *FS) Mknod(name string, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Clean(name)
	if err := f.mutable("mknod", p); err != nil {
		return err
	}
	if p == "" || reserved(p) {
		return &fs.PathError{Op: "mknod", Path: name, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(p); err == nil {
		return &fs.PathError{Op: "mknod", Path: name, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	if err := makeNode(f.up(p), f.stagingName(), mode); err != nil {
		return err
	}
	if err := f.clearWhiteout(p); err != nil {
		return err
	}
	if err := f.refresh(p); err != nil {
		return err
	}
	return f.refresh(parent(p))
}

// Symlink creates a symlink without modifying any immutable source.
func (f *FS) Symlink(target, link string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Clean(link)
	if err := f.mutable("symlink", p); err != nil {
		return err
	}
	if p == "" || reserved(p) {
		return &fs.PathError{Op: "symlink", Path: link, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(p); err == nil {
		return &fs.PathError{Op: "symlink", Path: link, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	if err := os.Symlink(target, f.up(p)); err != nil {
		return err
	}
	if err := f.clearWhiteout(p); err != nil {
		return err
	}
	if err := f.refresh(p); err != nil {
		return err
	}
	return f.refresh(parent(p))
}

// Remove removes a file or merged-empty directory and masks all lower copies.
func (f *FS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Clean(name)
	if err := f.mutable("remove", p); err != nil {
		return err
	}
	if p == "" {
		return &fs.PathError{Op: "remove", Path: name, Err: syscall.EBUSY}
	}
	s, info, err := f.lookup(p)
	if err != nil {
		return err
	}
	if info.IsDir() {
		items, err := f.readDir(p)
		if err != nil {
			return err
		}
		if len(items) != 0 {
			return &fs.PathError{Op: "remove", Path: name, Err: syscall.ENOTEMPTY}
		}
	}
	lower, err := f.lowerHas(p)
	if err != nil {
		return err
	}
	if lower {
		if err := f.whiteout(p); err != nil {
			return err
		}
	}
	alias := f.otherAlias(p)
	inMem := f.files[p] != nil
	if s.upper {
		switch {
		case inMem:
		case info.IsDir():
			// Merged-empty: only markers can remain beneath it.
			if err := os.RemoveAll(f.up(p)); err != nil {
				return err
			}
		default:
			// Unlike os.Remove on Windows, keeps a read-only hard link's other names read-only.
			if err := hostfile.Remove(f.up(p)); err != nil {
				return err
			}
		}
		f.dropIndex(p)
	}
	if alias != "" {
		if err := f.refresh(alias); err != nil {
			return err
		}
	}
	if lower {
		f.index.set(p, Entry{Kind: KindWhiteout})
	}
	if inMem {
		return f.touchDir(parent(p))
	}
	return f.refresh(parent(p))
}

// Link creates a hard link after copying an immutable source into upper.
func (f *FS) Link(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	src, dst := Clean(oldname), Clean(newname)
	if err := f.mutable("link", dst); err != nil {
		return err
	}
	if src == "" || dst == "" || reserved(src) || reserved(dst) {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	_, info, err := f.lookup(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	if _, _, err := f.lookup(dst); err == nil {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := f.copyUp(src); err != nil {
		return err
	}
	if m := f.files[src]; m != nil {
		if err := f.spill(m); err != nil {
			return err
		}
	}
	if err := f.ensureDir(parent(dst)); err != nil {
		return err
	}
	if err := os.Link(f.up(src), f.up(dst)); err != nil {
		return err
	}
	if err := f.clearWhiteout(dst); err != nil {
		return err
	}
	if err := f.refresh(dst); err != nil {
		return err
	}
	return f.refresh(parent(dst))
}

// Rename materializes a merged source tree before moving it entirely in upper.
func (f *FS) Rename(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	src, dst := Clean(from), Clean(to)
	if err := f.mutable("rename", src); err != nil {
		return err
	}
	if src == "" || dst == "" || reserved(src) || reserved(dst) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EPERM}
	}
	_, sinfo, err := f.lookup(src)
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if strings.HasPrefix(dst, src+"/") {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EINVAL}
	}
	ds, dinfo, derr := f.lookup(dst)
	if derr != nil && !errors.Is(derr, fs.ErrNotExist) {
		return derr
	}
	if derr == nil && (sameFile(sinfo, dinfo) || sameStored(sinfo, dinfo)) {
		return nil
	}
	if derr == nil {
		if sinfo.IsDir() && !dinfo.IsDir() {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.ENOTDIR}
		}
		if !sinfo.IsDir() && dinfo.IsDir() {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EISDIR}
		}
		if dinfo.IsDir() {
			items, err := f.readDir(dst)
			if err != nil {
				return err
			}
			if len(items) != 0 {
				return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.ENOTEMPTY}
			}
		}
	}
	lower, err := f.lowerHas(src)
	if err != nil {
		return err
	}
	if sinfo.IsDir() {
		err = f.copyUpTree(src)
	} else {
		err = f.copyUp(src)
	}
	if err != nil {
		return err
	}
	if err := f.ensureDir(parent(dst)); err != nil {
		return err
	}
	dstAlias := f.otherAlias(dst)
	if derr == nil && dinfo.IsDir() && ds.upper {
		if err := os.RemoveAll(f.up(dst)); err != nil {
			return err
		}
		f.dropIndex(dst)
	}
	// A memory file moves only in the index; it replaces a host file by unlinking it.
	srcMem := f.files[src] != nil
	if !srcMem {
		if err := hostfile.Rename(f.up(src), f.up(dst)); err != nil {
			return err
		}
	} else if derr == nil && ds.upper && f.files[dst] == nil {
		if err := hostfile.Remove(f.up(dst)); err != nil {
			return err
		}
	}
	// Move indexed descendants without traversing the renamed directory on disk.
	var move func(string, string)
	move = func(a, b string) {
		e := f.index.entries[a]
		if (e.Kind == KindFile || e.Kind == KindSymlink) && e.Origin == "" {
			e.Origin = a
		}
		f.index.set(b, e)
		f.moveIdentity(a, b)
		if m := f.files[a]; m != nil {
			delete(f.files, a)
			f.files[b], m.path = m, b
		}
		for n := range f.index.children[a] {
			move(path.Join(a, n), path.Join(b, n))
		}
	}
	f.dropIndex(dst)
	move(src, dst)
	f.dropIndex(src)
	if dstAlias != "" {
		if err := f.refresh(dstAlias); err != nil {
			return err
		}
	}
	if sinfo.IsDir() {
		err = f.placeDir(dst, true)
	} else {
		err = f.clearWhiteout(dst)
		if err == nil {
			err = f.refresh(dst)
		}
	}
	if err != nil {
		return err
	}
	if lower {
		if err := f.whiteout(src); err != nil {
			return err
		}
	}
	update := f.refresh
	if srcMem {
		update = f.touchDir
	}
	if err := update(parent(src)); err != nil {
		return err
	}
	return update(parent(dst))
}

// Join joins overlay path elements with slashes.
func (f *FS) Join(elem ...string) string { return path.Join(elem...) }

// Chroot exposes a merged subtree as a billy filesystem.
func (f *FS) Chroot(p string) (billy.Filesystem, error) { return chroot.New(f, Clean(p)), nil }

// Root returns the virtual root of the merged filesystem.
func (f *FS) Root() string { return "/" }

// Capabilities reports the operations supported by this filesystem.
func (f *FS) Capabilities() billy.Capability { return billy.AllCapabilities }

// Chmod changes mode after copying the entry up. A symlink's own mode changes,
// never its target's; hosts without symlink modes refuse it as unsupported.
func (f *FS) Chmod(name string, mode os.FileMode) error {
	entry := Clean(name)
	return f.change(name, "chmod", func(p string) error {
		// change holds f.mu, so the index has the copied-up entry's kind.
		if f.index.entries[entry].Kind == KindSymlink {
			return lchmod(p, mode)
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
		return keepMode(p, mode)
	}, func(m *memFile) { m.mode = mode & memModeBits })
}

// Lchown changes ownership without following the final symlink.
func (f *FS) Lchown(name string, uid, gid int) error {
	return f.change(name, "lchown", func(p string) error { return os.Lchown(p, uid, gid) }, nil)
}

// Chown changes ownership after copying the entry up. Like Lchown, it never
// follows a symlink: its target may lie outside the checkout.
func (f *FS) Chown(name string, uid, gid int) error {
	return f.change(name, "chown", func(p string) error { return os.Lchown(p, uid, gid) }, nil)
}

// Chtimes changes access and modification times in upper only, of a symlink
// itself rather than its target; a zero time leaves that time unchanged, as
// for os.Chtimes.
func (f *FS) Chtimes(name string, atime, mtime time.Time) error {
	return f.change(name, "chtimes", func(p string) error { return hostfile.Lchtimes(p, atime, mtime) },
		func(m *memFile) {
			if !atime.IsZero() {
				m.atime = atime
			}
			if !mtime.IsZero() {
				m.mtime = mtime
			}
		})
}

// change applies one metadata mutation under the structural lock: host to an
// upper path, or mem to a memory file. Without mem, a memory file spills first
// (ownership needs the kernel's permission checks). host must not follow a
// symlink: a target inside the mount would re-enter the daemon, which waits
// for f.mu.
func (f *FS) change(name, op string, host func(string) error, mem func(*memFile)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Clean(name)
	if err := f.mutable(op, p); err != nil {
		return err
	}
	if err := f.copyUp(p); err != nil {
		return err
	}
	if m := f.files[p]; m != nil {
		if mem != nil {
			mem(m)
			m.ctime = time.Now()
			m.version++
			return f.refresh(p)
		}
		if err := f.spill(m); err != nil {
			return err
		}
	}
	if err := host(f.up(p)); err != nil {
		return err
	}
	return f.refresh(p)
}

// mutable rejects all mutations of a read-only frozen stack.
func (f *FS) mutable(op, p string) error {
	if f.upper == "" {
		return &fs.PathError{Op: op, Path: p, Err: syscall.EROFS}
	}
	return nil
}

// refresh synchronizes one upper entry's metadata without forgetting opacity.
func (f *FS) refresh(p string) error {
	if m := f.files[p]; m != nil {
		f.index.set(p, Entry{Kind: KindFile, Info: m.info(path.Base(p))})
		return nil
	}
	info, err := hostLstat(f.up(p))
	if err != nil {
		return err
	}
	return f.cacheInfo(p, info)
}

// placeDir isolates a new or renamed directory from old content beneath it.
func (f *FS) placeDir(p string, complete bool) error {
	lower, err := f.lowerHas(p)
	if err != nil {
		return err
	}
	if complete || lower {
		err := f.preserveDirTime(p, func() error {
			fh, err := os.OpenFile(filepath.Join(f.up(p), opaqueName), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if err := fh.Close(); err != nil {
				return err
			}
			e := f.index.entries[p]
			e.Opaque = true
			f.index.set(p, e)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := f.clearWhiteout(p); err != nil {
		return err
	}
	return f.refresh(p)
}

// whiteout places a marker before removing an upper entry.
func (f *FS) whiteout(p string) error {
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.whiteoutPath(p), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	if _, err := os.Lstat(f.up(p)); missing(err) {
		f.index.set(p, Entry{Kind: KindWhiteout})
	}
	return nil
}

// clearWhiteout drops the deletion marker after a data entry has replaced it.
func (f *FS) clearWhiteout(p string) error {
	if p == "" {
		return nil
	}
	if err := os.Remove(f.whiteoutPath(p)); err != nil && !missing(err) {
		return err
	}
	if e, ok := f.index.entries[p]; ok && e.Kind == KindWhiteout {
		f.dropIndex(p)
	}
	return nil
}

// whiteoutPath gives the host path of a name's AUFS marker.
func (f *FS) whiteoutPath(p string) string {
	return filepath.Join(f.up(parent(p)), whPrefix+path.Base(p))
}

// ensureDir copies existing merged parents up with their modes and mtimes.
func (f *FS) ensureDir(p string) error {
	if e, ok := f.index.entries[p]; ok && e.Kind != KindWhiteout {
		if e.Kind != KindDir {
			return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.ENOTDIR}
		}
		return nil
	}
	_, info, err := f.lookup(p)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &fs.PathError{Op: "mkdir", Path: p, Err: syscall.ENOTDIR}
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	err = f.preserveDirTime(parent(p), func() error {
		if err := os.Mkdir(f.up(p), info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chmod(f.up(p), info.Mode().Perm()); err != nil {
			return err
		}
		if err := keepMode(f.up(p), info.Mode()); err != nil {
			return err
		}
		return os.Chtimes(f.up(p), info.ModTime(), info.ModTime())
	})
	if err != nil {
		return err
	}
	return f.refresh(p)
}

// preserveDirTime prevents copy-up from invalidating unchanged directory caches.
func (f *FS) preserveDirTime(d string, fn func() error) error {
	info, err := os.Lstat(f.up(d))
	if err != nil {
		return err
	}
	opErr := fn()
	timeErr := os.Chtimes(f.up(d), info.ModTime(), info.ModTime())
	refreshErr := f.refresh(d)
	if opErr != nil {
		return opErr
	}
	if timeErr != nil {
		return timeErr
	}
	return refreshErr
}

// copyUpBottomForOpen stages an immutable bottom file without holding mu.
// On reacquiring mu it either publishes that same file or tells OpenFile to
// resolve the name again if another operation changed its source.
func (f *FS) copyUpBottomForOpen(p string, info os.FileInfo) (bool, error) {
	bottom, epoch := f.bottom, f.bottomEpoch
	inMemory := f.mem != nil && info.Size() <= f.mem.fileMax
	for {
		var tmp string
		if !inMemory {
			tmp = f.stagingName()
		}
		f.mu.Unlock()
		var data []byte
		var err error
		if inMemory {
			var c Content
			c, err = bottom.Open(p)
			if err == nil {
				data, err = readBounded(c, info.Size(), f.mem.fileMax)
				err = errors.Join(err, c.Close())
			}
		} else {
			err = bottom.CopyTo(p, tmp)
			if err == nil {
				err = keepMode(tmp, info.Mode())
			}
		}
		f.mu.Lock()
		current, currentInfo, lookupErr := f.lookup(p)
		if f.bottomEpoch != epoch || lookupErr != nil || current.layer != nil || !currentInfo.Mode().IsRegular() {
			if tmp != "" {
				_ = os.RemoveAll(tmp)
			}
			return true, nil
		}
		if err != nil {
			if tmp != "" {
				_ = os.RemoveAll(tmp)
			}
			return false, err
		}
		if err := f.ensureDir(parent(p)); err != nil {
			if tmp != "" {
				_ = os.RemoveAll(tmp)
			}
			return false, err
		}
		if inMemory {
			if int64(len(data)) <= f.mem.fileMax {
				m, err := f.addMem(p, info.Mode(), info.ModTime(), data)
				if m != nil || err != nil {
					return false, err
				}
			}
			inMemory = false
			continue
		}
		defer os.RemoveAll(tmp)
		if err := f.preserveDirTime(parent(p), func() error { return hostfile.Rename(tmp, f.up(p)) }); err != nil {
			return false, err
		}
		return false, f.refresh(p)
	}
}

// copyUp stages the selected immutable entry before publishing it atomically.
func (f *FS) copyUp(p string) error {
	s, info, err := f.lookup(p)
	if err != nil {
		return err
	}
	if s.upper {
		return nil
	}
	if info.IsDir() {
		return f.ensureDir(p)
	}
	if err := f.ensureDir(parent(p)); err != nil {
		return err
	}
	if ok, err := f.copyUpMem(s, p, info); ok || err != nil {
		return err
	}
	f.tmpSeq++
	tmp := filepath.Join(f.work, strconv.FormatUint(f.tmpSeq, 36))
	defer os.RemoveAll(tmp)
	if s.layer != nil {
		err = s.layer.copyTo(p, tmp, info)
	} else {
		err = f.bottom.CopyTo(p, tmp)
	}
	if err == nil && info.Mode()&os.ModeSymlink == 0 {
		err = keepMode(tmp, info.Mode())
	}
	if err != nil {
		return err
	}
	if err := f.preserveDirTime(parent(p), func() error { return hostfile.Rename(tmp, f.up(p)) }); err != nil {
		return err
	}
	return f.refresh(p)
}

// copyUpTree recursively copies the merged view rather than any one layer.
func (f *FS) copyUpTree(p string) error {
	if err := f.ensureDir(p); err != nil {
		return err
	}
	items, err := f.readDir(p)
	if err != nil {
		return err
	}
	for _, info := range items {
		q := path.Join(p, info.Name())
		if info.IsDir() {
			err = f.copyUpTree(q)
		} else {
			err = f.copyUp(q)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// upperInfo returns an indexed upper entry's metadata. Every change made
// through the overlay refreshes the index, so it is the host's, except while
// a writable descriptor of the entry's inode is open (writes go straight to
// the host file) or the entry has no known inode; only then is the host asked.
// The caller holds f.mu.
func (f *FS) upperInfo(p string, e Entry) (os.FileInfo, error) {
	if e.Kind == KindDir {
		return e.Info, nil
	}
	if key, ok := f.pathIDs[p]; ok && f.writers[key] == 0 {
		return e.Info, nil
	}
	return lstatIndexed(f.up(p), e.Info)
}

// file exposes an OS file with an overlay name. A writable upper descriptor
// writes without the overlay lock; its inode's indexed metadata is refreshed
// when it closes (see [FS.upperInfo]).
type file struct {
	*os.File
	name        string
	inode       inodeKey
	tracked     bool
	writable    bool
	fs          *FS
	ownerClosed bool
	closeErr    error
}

// open wraps a host file without losing its overlay-relative name; an owner
// tracks it as one of its upper descriptors.
func open(real, name string, flag int, perm os.FileMode, owner *FS) (*file, error) {
	fh, err := hostfile.OpenFile(real, flag, perm)
	if err != nil {
		return nil, err
	}
	h := &file{File: fh, name: name, writable: writable(flag)}
	if owner != nil {
		owner.adopt(h)
	}
	return h, nil
}

// adopt makes h one of f's upper descriptors, closed by [FS.CloseFiles]. The caller holds f.mu.
func (f *FS) adopt(h *file) *file {
	h.fs = f
	f.hosts[h] = struct{}{}
	return h
}

// CloseFiles closes every open descriptor of an upper file, because Windows
// refuses to rename a directory while any file below it is open. The caller
// must exclude I/O on those handles: the export reopens them in the checkout's
// next generation. Their eventual Close consumes the original close result,
// so retiring a cached protocol writer does not report a second-close error.
func (f *FS) CloseFiles() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for h := range f.hosts {
		h.closeErr = h.File.Close()
		h.ownerClosed = true
		if h.writable {
			h.syncInfo()
		}
	}
	clear(f.hosts)
	clear(f.writers)
}

// Close releases the descriptor. Closing a writable one refreshes its inode's
// indexed metadata: its writes bypassed the index, and on Windows the close
// itself can still move the mtime.
func (f *file) Close() error {
	if f.fs == nil {
		return f.File.Close()
	}
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if _, open := f.fs.hosts[f]; !open {
		if f.ownerClosed {
			f.ownerClosed = false
			return f.closeErr
		}
		return f.File.Close()
	}
	delete(f.fs.hosts, f)
	err := f.File.Close()
	if f.writable && f.tracked {
		if f.fs.writers[f.inode]--; f.fs.writers[f.inode] <= 0 {
			delete(f.fs.writers, f.inode)
		}
		f.syncInfo()
	}
	return err
}

// Name returns the overlay-relative name instead of the private host path.
func (f *file) Name() string { return f.name }

// Chmod applies descriptor metadata changes without leaving the upper index stale.
func (f *file) Chmod(mode os.FileMode) error {
	if f.fs == nil {
		return f.File.Chmod(mode)
	}
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	err := f.File.Chmod(mode)
	if err == nil {
		name := f.File.Name()
		if p := f.fs.aliases[f.inode].first; f.tracked && p != "" {
			name = f.fs.up(p) // the host name may have been renamed since the open
		}
		err = keepMode(name, mode)
	}
	f.syncInfo()
	return err
}

// Chown applies descriptor ownership changes under the upper's structural lock.
func (f *file) Chown(uid, gid int) error {
	if f.fs == nil {
		return f.File.Chown(uid, gid)
	}
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	err := f.File.Chown(uid, gid)
	f.syncInfo()
	return err
}

// syncInfo re-reads the metadata of every surviving name of the descriptor's
// inode; renames since it was opened move the names, not the inode.
func (f *file) syncInfo() {
	if !f.tracked {
		return
	}
	if p := f.fs.aliases[f.inode].first; p != "" {
		_ = f.fs.restat(p)
	}
}

// AppleDouble reports macOS xattr sidecar names ("._name"). Clients without
// native xattrs store xattrs in such files, and the macOS kernel tags every
// new file with com.apple.provenance, so exports keep them reachable by name
// but never list them, and searches skip them.
func AppleDouble(name string) bool { return strings.HasPrefix(name, "._") }

// Clean canonicalizes a path to slash-separated root-relative form.
func Clean(p string) string {
	p = filepath.ToSlash(p) // no-op (and allocation-free) where the separator is '/'
	if slashRelativeClean(p) {
		return p
	}
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}

// slashRelativeClean reports whether p is already its own Clean form: empty,
// or non-rooted segments with no empty, "." or ".." segment and no trailing slash.
func slashRelativeClean(p string) bool {
	for start := 0; start < len(p); {
		end := strings.IndexByte(p[start:], '/')
		if end < 0 {
			end = len(p)
		} else {
			end += start
		}
		seg := p[start:end]
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		start = end + 1
		if end == len(p)-1 {
			return false // trailing slash
		}
	}
	return true
}

// parent returns the canonical parent path, with the root represented by empty.
func parent(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// reserved detects marker names in every component of a path.
func reserved(p string) bool {
	if !strings.Contains(p, whPrefix) {
		return false
	}
	for c := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(c, whPrefix) {
			return true
		}
	}
	return false
}

// within reports whether p is dir or a descendant of dir.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// missing includes ENOTDIR because a non-directory ancestor hides a subtree.
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// notExist attaches the virtual path to a missing-entry error.
func notExist(op, p string) error { return &fs.PathError{Op: op, Path: p, Err: syscall.ENOENT} }
