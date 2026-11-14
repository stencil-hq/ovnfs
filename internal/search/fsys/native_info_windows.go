//go:build windows

package fsys

import (
	"encoding/binary"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// winInfo is the complete metadata of an ordinary (non-reparse) directory
// entry, built from the NtQueryDirectoryFile record without further syscalls.
// It implements both os.FileInfo and os.DirEntry with the values os.Lstat
// would return for the same entry.
type winInfo struct {
	name   string
	data   syscall.Win32FileAttributeData
	change int64 // ChangeTime in Unix nanoseconds; the attribute data has none
}

// ChangeTime returns the NTFS change time, in Unix nanoseconds, of the entry
// at path that info describes (its [os.Lstat] data or a [ReadDir] entry's
// Info). Content, metadata and renames move it, and unlike the mtime no tool
// can set it back. A listed entry carries it; anything else costs a
// nonfollowing stat of path, which fails as that stat does.
func ChangeTime(path string, info os.FileInfo) (int64, error) {
	if i, ok := info.(*winInfo); ok {
		return i.change, nil
	}
	s := readDirSessions.Get().(*session)
	fi, err := s.Stat(path, false)
	readDirSessions.Put(s)
	if err != nil {
		return 0, err
	}
	return fi.Stamp.Ctime, nil
}

var (
	_ os.FileInfo = (*winInfo)(nil)
	_ os.DirEntry = (*winInfo)(nil)
)

func (i *winInfo) Name() string { return i.name }

func (i *winInfo) Size() int64 {
	return int64(i.data.FileSizeHigh)<<32 + int64(i.data.FileSizeLow)
}

// Mode follows os.Stat for a file that is not a reparse point: read-only files
// are 0444 and the rest 0666; directories add ModeDir and the execute bits.
func (i *winInfo) Mode() fs.FileMode {
	m := fs.FileMode(0o666)
	if i.data.FileAttributes&syscall.FILE_ATTRIBUTE_READONLY != 0 {
		m = 0o444
	}
	if i.data.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		m |= fs.ModeDir | 0o111
	}
	return m
}

func (i *winInfo) ModTime() time.Time {
	return time.Unix(0, i.data.LastWriteTime.Nanoseconds())
}

func (i *winInfo) IsDir() bool { return i.Mode().IsDir() }

// Sys returns the entry's own *syscall.Win32FileAttributeData, without
// allocating; callers must not modify it.
func (i *winInfo) Sys() any { return &i.data }

func (i *winInfo) Type() fs.FileMode { return i.Mode().Type() }

func (i *winInfo) Info() (fs.FileInfo, error) { return i, nil }

func (i *winInfo) String() string { return fs.FormatDirEntry(i) }

// filetimeOf splits 100ns ticks into a Win32 FILETIME.
func filetimeOf(ticks uint64) syscall.Filetime {
	return syscall.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(ticks >> 32)}
}

// readDir lists the directory at path with complete os.Lstat-compatible
// metadata through the same open and NtQueryDirectoryFile loop as compact
// listings. Entries are unsorted; the caller sorts. Reparse points (symlinks,
// junctions, mount points, cloud placeholders, dedup files) carry a tag the
// listing lacks, so each is described by os.Lstat; its failure becomes that
// entry's error.
func (s *session) readDir(path string) ([]os.DirEntry, error) {
	s.dir = path
	h, err := s.openDir(path)
	if err != nil {
		return nil, err
	}
	defer closeHandle(h)
	s.entries = nil
	if err := s.list(h, metadataDetail, 0); err != nil {
		return nil, err
	}
	return s.entries, nil
}

// addMetadata appends the entry of one FILE_DIRECTORY_INFORMATION record to
// s.entries. name is its WTF-8 name, so unpaired surrogates survive; os.Lstat
// of such a name (reparse entries only) fails and is reported per entry.
func (s *session) addMetadata(rec []byte, name string, attrs uint32, slab *[]winInfo) {
	if attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		path := s.dir
		if n := len(path); n > 0 && path[n-1] != '\\' && path[n-1] != '/' {
			path += `\`
		}
		info, err := os.Lstat(path + name)
		if err != nil {
			// The listing carries no reparse tag; Symlink matches how the
			// compact listing classifies every reparse point.
			s.entries = append(s.entries, &failedEntry{name: name, typ: fs.ModeSymlink, err: err})
			return
		}
		s.entries = append(s.entries, fs.FileInfoToDirEntry(info))
		return
	}
	size := binary.LittleEndian.Uint64(rec[fdiEndOfFile:])
	*slab = (*slab)[:len(*slab)+1]
	info := &(*slab)[len(*slab)-1]
	*info = winInfo{name: name, data: syscall.Win32FileAttributeData{
		FileAttributes: attrs,
		CreationTime:   filetimeOf(binary.LittleEndian.Uint64(rec[fdiCreationTime:])),
		LastAccessTime: filetimeOf(binary.LittleEndian.Uint64(rec[fdiLastAccessTime:])),
		LastWriteTime:  filetimeOf(binary.LittleEndian.Uint64(rec[fdiLastWriteTime:])),
		FileSizeHigh:   uint32(size >> 32),
		FileSizeLow:    uint32(size),
	}, change: filetimeNanos(int64(binary.LittleEndian.Uint64(rec[fdiChangeTime:])))}
	s.entries = append(s.entries, info)
}
