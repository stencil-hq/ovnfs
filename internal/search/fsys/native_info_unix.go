//go:build darwin || linux

package fsys

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// nativeInfo is one entry of a [ReadDir] listing: its complete lstat data as
// both [os.FileInfo] and [os.DirEntry]. Sys is the *syscall.Stat_t that
// [os.Lstat] reports.
type nativeInfo struct {
	name string
	mode os.FileMode
	mod  time.Time
	st   syscall.Stat_t
}

// ChangeTime returns the status change time, in Unix nanoseconds, of the
// entry at path that info describes (its [os.Lstat] data or a [ReadDir]
// entry's Info). Content, metadata and renames move it, and unlike the mtime
// no tool can set it back. It fails for an info with no host stat data.
func ChangeTime(_ string, info os.FileInfo) (int64, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.ErrUnsupported
	}
	return statChange(st), nil
}

func (e *nativeInfo) Name() string               { return e.name }
func (e *nativeInfo) Size() int64                { return e.st.Size }
func (e *nativeInfo) Mode() fs.FileMode          { return e.mode }
func (e *nativeInfo) ModTime() time.Time         { return e.mod }
func (e *nativeInfo) IsDir() bool                { return e.mode.IsDir() }
func (e *nativeInfo) Sys() any                   { return &e.st }
func (e *nativeInfo) Type() fs.FileMode          { return e.mode.Type() }
func (e *nativeInfo) Info() (fs.FileInfo, error) { return e, nil }

// newInfo takes the next nativeInfo from a batch-sized slab. Its backing
// array is never grown or reused: returned interfaces point into it.
func newInfo(slab *[]nativeInfo) *nativeInfo {
	*slab = (*slab)[:len(*slab)+1]
	info := &(*slab)[len(*slab)-1]
	*info = nativeInfo{}
	return info
}

// dropInfo releases the nativeInfo newInfo returned last.
func dropInfo(slab *[]nativeInfo) { *slab = (*slab)[:len(*slab)-1] }

// modeFromStat converts st_mode to [os.FileMode] as the os package does.
func modeFromStat(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFBLK:
		m |= os.ModeDevice
	case unix.S_IFCHR:
		m |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFDIR:
		m |= os.ModeDir
	case unix.S_IFIFO:
		m |= os.ModeNamedPipe
	case unix.S_IFLNK:
		m |= os.ModeSymlink
	case unix.S_IFSOCK:
		m |= os.ModeSocket
	}
	if mode&unix.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if mode&unix.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if mode&unix.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	return m
}
