package fsys

import (
	"errors"
	"io"
	"os"
	"path"

	"github.com/go-git/go-billy/v5"
)

// FromOverlay lets daemon searches read a pinned checkout without its network mount.
// The caller must keep the checkout session alive until every search worker exits.
func FromOverlay(f billy.Filesystem) FS { return &overlayFS{f: f} }

type overlayFS struct{ f billy.Filesystem }

func (*overlayFS) Abs(root string) (string, error) { return path.Clean("/" + root), nil }
func (f *overlayFS) NewSession() Session           { return &overlaySession{f: f.f} }
func (*overlayFS) Separator() byte                 { return '/' }

// Workers is 8: each overlay file costs several host syscalls, which run in
// parallel; grepping a 10k-file checkout took 120 ms with 4 workers, 85 ms
// with 8, and no less with more (Apple M-series, warm cache).
func (*overlayFS) Workers() int { return 8 }

type overlaySession struct {
	f     billy.Filesystem
	ls    Listing
	files []billy.File
	free  []uintptr
}

func (s *overlaySession) OpenDir(p string) (Handle, error) {
	info, err := s.f.Lstat(p)
	if err != nil {
		return Handle{}, err
	}
	if !info.IsDir() {
		return Handle{}, &os.PathError{Op: "open", Path: p, Err: errNotDir}
	}
	return Handle{path: p, ok: true}, nil
}

func (s *overlaySession) OpenChild(parent Handle, name string) (Handle, error) {
	return s.OpenDir(path.Join(parent.path, name))
}

func (s *overlaySession) Close(h Handle) {
	if h.ok && h.h != 0 {
		_ = s.files[h.h-1].Close()
		s.files[h.h-1] = nil
		s.free = append(s.free, h.h)
	}
}

// List lists what the checkout mount lists: without AppleDouble sidecars.
func (s *overlaySession) List(h Handle, detail Detail, _ uint32) (*Listing, error) {
	entries, err := s.f.ReadDir(h.path)
	if err != nil {
		return nil, err
	}
	s.ls.reset()
	for _, info := range entries {
		typ := overlayType(info.Mode())
		if typ == 0 || AppleDouble(info.Name()) {
			continue
		}
		size, mtime := int64(-1), int64(0)
		if detail == Full {
			mtime = info.ModTime().UnixNano()
			if typ == File {
				size = info.Size()
			}
		}
		s.ls.add([]byte(info.Name()), typ, size, mtime, 0)
	}
	return &s.ls, nil
}

func (s *overlaySession) OpenFile(dir Handle, name string) (Handle, FileInfo, error) {
	p := name
	if dir.ok {
		p = path.Join(dir.path, name)
	}
	info, err := s.f.Stat(p)
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return Handle{}, FileInfo{}, ErrNotRegular
	}
	f, err := s.f.Open(p)
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	var slot uintptr
	if n := len(s.free); n != 0 {
		slot, s.free = s.free[n-1], s.free[:n-1]
		s.files[slot-1] = f
	} else {
		s.files = append(s.files, f)
		slot = uintptr(len(s.files))
	}
	return Handle{h: slot, path: p, size: info.Size(), ok: true}, overlayInfo(info), nil
}

func (s *overlaySession) Read(h Handle, buf []byte, limit int64) ([]byte, error) {
	return readInto(buf, h.size, limit, func(p []byte, off int64) (int, error) {
		n, err := s.files[h.h-1].ReadAt(p, off)
		if err == io.EOF {
			err = nil
		}
		return n, err
	})
}

func (*overlaySession) Map(Handle, int64) ([]byte, error) { return nil, errors.ErrUnsupported }
func (*overlaySession) Unmap([]byte)                      {}

func (s *overlaySession) Stat(p string, follow bool) (FileInfo, error) {
	stat := s.f.Lstat
	if follow {
		stat = s.f.Stat
	}
	info, err := stat(p)
	if err != nil {
		return FileInfo{}, err
	}
	return overlayInfo(info), nil
}

func (s *overlaySession) StatDir(h Handle) (Stamp, error) {
	info, err := s.Stat(h.path, false)
	return info.Stamp, err
}

func (*overlaySession) IdentifyDir(Handle) (ID, error) { return ID{}, errors.ErrUnsupported }

func (*overlaySession) Identify(string) (ID, error) { return ID{}, errors.ErrUnsupported }

// Live cache-class paths can change without changing the overlay's directory stamp.
func (*overlaySession) StableStamps(Handle, Stamp) bool { return false }

func overlayInfo(info os.FileInfo) FileInfo {
	return FileInfo{Type: overlayType(info.Mode()), Size: info.Size(), Stamp: Stamp{Mtime: info.ModTime().UnixNano()}}
}

func overlayType(mode os.FileMode) Type {
	switch {
	case mode.IsRegular():
		return File
	case mode.IsDir():
		return Dir
	case mode&os.ModeSymlink != 0:
		return Symlink
	default:
		return 0
	}
}
