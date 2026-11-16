package fsys

import (
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// Native is the host filesystem. Directory handles are real descriptors
// (Windows: HANDLEs) and every operation is a direct syscall; see the
// native_*.go files for the per-platform fast paths.
var Native FS = native

var native = &nativeFS{}

type nativeFS struct {
	stable sync.Map // device → bool, for StableStamps
}

func (*nativeFS) Abs(root string) (string, error) { return filepath.Abs(root) }

func (n *nativeFS) NewSession() Session { return &session{fs: n} }

func (*nativeFS) Separator() byte { return pathSep }

func (*nativeFS) Workers() int { return defaultWorkers }

// session is per-goroutine scratch: buffers are reused across calls so
// steady-state listing and reading allocate nothing.
type session struct {
	fs    *nativeFS
	ls    Listing
	buf   []byte   // bulk listing syscall buffer
	cname []byte   // NUL-terminated name for *at syscalls
	wname []uint16 // Windows: UTF-16 name for NtCreateFile

	// Metadata listing ([ReadDir]): the directory being read, for the paths
	// in per-entry errors, and the entries collected so far. ReadDir hands
	// the entries to its caller, so the session never reuses them.
	dir     string
	entries []os.DirEntry
}

// readDirSessions pools the sessions [ReadDir] borrows, keeping their listing
// buffers across calls.
var readDirSessions = sync.Pool{New: func() any { return &session{fs: native} }}

// ReadDir lists host directory path sorted by name with the native backend's
// own listing, without following symlinks. Every entry is returned, special
// files included, and its Info is complete [os.Lstat] data (the same Mode,
// Size, ModTime and Sys). An entry that cannot be described, for instance
// because it was removed after being listed, returns that error from Info
// while the rest list normally; only a failure of the directory itself fails
// the call, with the error [os.ReadDir] gives. Names are as the host reports
// them, so on Windows they are UTF-8 conversions of the UTF-16 names.
//
// This is native host I/O: it takes a path, bypasses [FS], and returns
// entries that own all their memory.
func ReadDir(path string) ([]os.DirEntry, error) {
	s := readDirSessions.Get().(*session)
	entries, err := s.readDir(path)
	s.dir, s.entries = "", nil
	readDirSessions.Put(s)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

// AppleDouble reports macOS xattr sidecar names ("._name"). They remain
// reachable by name for xattr writes, but exports and searches never list them.
func AppleDouble(name string) bool { return strings.HasPrefix(name, "._") }

// failedEntry is a listed entry whose metadata could not be read: its
// listing type is known but Info reports why the stat failed.
type failedEntry struct {
	name string
	typ  os.FileMode
	err  error
}

func (e failedEntry) Name() string               { return e.name }
func (e failedEntry) IsDir() bool                { return e.typ.IsDir() }
func (e failedEntry) Type() fs.FileMode          { return e.typ }
func (e failedEntry) Info() (fs.FileInfo, error) { return nil, e.err }

// cstr returns name NUL-terminated in s.cname. A name with an embedded NUL
// would silently name a shorter path to the kernel, so it is rejected with
// EINVAL before any syscall.
func (s *session) cstr(name string) ([]byte, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return nil, syscall.EINVAL
	}
	s.cname = append(append(s.cname[:0], name...), 0)
	return s.cname, nil
}

// nanos converts a seconds+nanoseconds timestamp to Unix nanoseconds with
// saturating arithmetic: sec*1e9 saturates first, then nsec is added
// saturating, so an overflowed negative sec still takes a positive nsec.
func nanos(sec, nsec int64) int64 {
	const billion = 1_000_000_000
	var ns int64
	switch {
	case sec > math.MaxInt64/billion:
		ns = math.MaxInt64
	case sec < math.MinInt64/billion:
		ns = math.MinInt64
	default:
		ns = sec * billion
	}
	sum := ns + nsec
	if nsec > 0 && sum < ns {
		return math.MaxInt64
	}
	if nsec < 0 && sum > ns {
		return math.MinInt64
	}
	return sum
}

func wrap(h handle) Handle { return Handle{h: uintptr(h), ok: true} }

func (h Handle) native() handle { return handle(h.h) }

func (s *session) OpenDir(path string) (Handle, error) {
	h, err := s.openDir(path)
	if err != nil {
		return Handle{}, err
	}
	return wrap(h), nil
}

func (s *session) OpenChild(parent Handle, name string) (Handle, error) {
	h, err := s.openChild(parent.native(), name)
	if err != nil {
		return Handle{}, err
	}
	return wrap(h), nil
}

func (s *session) Close(h Handle) {
	if h.ok {
		closeHandle(h.native())
	}
}

func (s *session) List(h Handle, detail Detail, children uint32) (*Listing, error) {
	s.ls.reset()
	if err := s.list(h.native(), detail, children); err != nil {
		return nil, err
	}
	return &s.ls, nil
}

func (s *session) StatDir(h Handle) (Stamp, error) {
	st, _, _, err := statHandle(h.native())
	return st, err
}

func (s *session) StableStamps(h Handle, st Stamp) bool {
	if !memoStableByDev {
		return fineTimestamps(h.native())
	}
	if ok, found := s.fs.stable.Load(st.Dev); found {
		return ok.(bool)
	}
	ok := fineTimestamps(h.native())
	s.fs.stable.Store(st.Dev, ok)
	return ok
}

// grow returns buf with capacity for at least n bytes, keeping its contents.
func grow(buf []byte, n int) []byte {
	if cap(buf) >= n {
		return buf
	}
	return append(buf[:cap(buf)], make([]byte, n-cap(buf))...)[:len(buf)]
}

// readInto fills buf from read (of p at file offset off) until EOF or
// limit bytes, given size from fstat. A read that returns exactly the
// remaining size is taken as end of file, saving the confirming zero-length
// read per file; files reporting size 0 (pseudo files) and short reads keep
// reading until EOF.
func readInto(buf []byte, size, limit int64, read func(p []byte, off int64) (int, error)) ([]byte, error) {
	buf = buf[:0]
	want := int64(4096) // size 0: empty, or a pseudo file of unknown size
	if size > 0 {
		want = min(size, limit)
	}
	buf = grow(buf, int(min(want, limit)))
	for int64(len(buf)) < limit {
		if len(buf) == cap(buf) {
			buf = grow(buf, int(min(limit, int64(2*cap(buf)+4096))))
		}
		n, err := read(buf[len(buf):min(cap(buf), int(limit))], int64(len(buf)))
		if err != nil {
			return buf, err
		}
		if n == 0 {
			break
		}
		buf = buf[:len(buf)+n]
		if size > 0 && int64(len(buf)) == size {
			break
		}
	}
	return buf, nil
}
