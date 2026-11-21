//go:build windows

package hostfile

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/stencil-hq/ovnfs/ntinfo"
	"golang.org/x/sys/windows"
)

// shareAll lets other handles read, write, rename and delete the file, as Unix does.
const shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// OpenFile opens name like [os.OpenFile] but shares delete access, so the file
// stays renamable and deletable while open. Directories open read-only.
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if name == "" {
		return nil, &os.PathError{Op: "open", Path: name, Err: syscall.ENOENT}
	}
	p, err := windows.UTF16PtrFromString(longPath(name))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	if flag&os.O_APPEND != 0 {
		// Append-only handles write at the end whatever the offset; O_TRUNC still needs FILE_WRITE_DATA.
		if flag&os.O_TRUNC == 0 {
			access &^= windows.GENERIC_WRITE
		}
		access |= windows.FILE_APPEND_DATA | windows.FILE_WRITE_ATTRIBUTES | windows.FILE_WRITE_EA | windows.STANDARD_RIGHTS_WRITE | windows.SYNCHRONIZE
	}
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	if flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		attrs |= windows.FILE_FLAG_BACKUP_SEMANTICS // directories open only for reading
	}
	if flag&os.O_SYNC != 0 {
		attrs |= windows.FILE_FLAG_WRITE_THROUGH
	}
	// O_TRUNC truncates after opening: CREATE_ALWAYS would replace a read-only file (go.dev/issue/38225).
	create := uint32(windows.OPEN_EXISTING)
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		create = windows.CREATE_NEW
		attrs |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	case flag&os.O_CREATE != 0:
		create = windows.OPEN_ALWAYS
	}
	h, err := windows.CreateFile(p, access, shareAll, nil, create, attrs, 0)
	if err != nil {
		if err == windows.ERROR_ACCESS_DENIED && attrs&windows.FILE_FLAG_BACKUP_SEMANTICS == 0 {
			if fa, e := windows.GetFileAttributes(p); e == nil && fa&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
				err = syscall.EISDIR
			}
		}
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(h), name)
	if flag&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, &os.PathError{Op: "open", Path: name, Err: err}
		}
	}
	return f, nil
}

// fileRenameInfo is FILE_RENAME_INFO with the Flags member of its union.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// Rename moves oldpath to newpath with POSIX semantics: a replaced file may be
// open elsewhere (its handles keep the old data) or read-only, and oldpath
// itself may be open. Filesystems without POSIX renames, and Windows before
// 10 1809, fall back to [os.Rename].
func Rename(oldpath, newpath string) error {
	err := posixRename(oldpath, newpath)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER), errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_INVALID_FUNCTION):
		return os.Rename(oldpath, newpath)
	}
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
}

// posixRename renames through a handle with FILE_RENAME_FLAG_POSIX_SEMANTICS.
func posixRename(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(longPath(oldpath))
	if err != nil {
		return err
	}
	to, err := filepath.Abs(newpath)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(longPath(to))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(from, windows.DELETE|windows.SYNCHRONIZE, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	n := len(name) - 1 // without the terminating NUL
	size := int(unsafe.Offsetof(fileRenameInfo{}.FileName)) + (n+1)*2
	buf := make([]uint64, (size+7)/8) // FILE_RENAME_INFO needs pointer alignment
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	// IGNORE_READONLY_ATTRIBUTE replaces a read-only target as Unix rename does.
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS | windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE
	info.FileNameLength = uint32(n * 2)
	copy(unsafe.Slice(&info.FileName[0], n+1), name)
	return windows.SetFileInformationByHandle(h, windows.FileRenameInfoEx, (*byte)(unsafe.Pointer(&buf[0])), uint32(size))
}

// Remove deletes the file or empty directory name with POSIX semantics: the
// name goes away even while the file is open, and a read-only file is deleted
// without clearing the read-only attribute its other hard links share (which
// [os.Remove] does). Filesystems without POSIX deletes, and Windows before
// 10 1809, fall back to [os.Remove].
func Remove(name string) error {
	err := posixRemove(name)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER), errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_INVALID_FUNCTION):
		return os.Remove(name)
	}
	return &os.PathError{Op: "remove", Path: name, Err: err}
}

// posixRemove deletes through a handle with FILE_DISPOSITION_POSIX_SEMANTICS.
func posixRemove(name string) error {
	p, err := windows.UTF16PtrFromString(longPath(name))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE, shareAll, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
}

// CreateTemp creates a new file in dir like [os.CreateTemp], opened by [OpenFile].
func CreateTemp(dir, pattern string) (*os.File, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for range 10000 {
		name := filepath.Join(dir, prefix+strconv.FormatUint(uint64(rand.Uint32()), 10)+suffix)
		f, err := OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if !os.IsExist(err) {
			return f, err
		}
	}
	return nil, &os.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: os.ErrExist}
}

// Symlink is [os.Symlink], except that an account that may not create symbolic
// links (no SeCreateSymbolicLinkPrivilege and no Developer Mode, as for most
// standard users) gets a junction when target is an absolute path to a
// directory. Junctions need no privilege, and [Linked] and [os.Readlink] read
// one as the symlink it stands for. Every other failure is os.Symlink's.
func Symlink(target, link string) error {
	err := os.Symlink(target, link)
	if !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || !filepath.IsAbs(target) {
		return err
	}
	if info, statErr := os.Stat(target); statErr != nil || !info.IsDir() {
		return err
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		return err
	}
	if err := setJunction(link, target); err != nil {
		os.Remove(link)
		return &os.LinkError{Op: "junction", Old: target, New: link, Err: err}
	}
	return nil
}

// setJunction turns the empty directory dir into a junction to target.
func setJunction(dir, target string) error {
	buf, err := ntinfo.JunctionReparse(filepath.ToSlash(target))
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(longPath(dir))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var n uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
}

// Linked describes a junction, which Go reports as an irregular file since Go
// 1.23, as the directory symlink it acts as; [os.Readlink] reads its target.
// info is the path's [os.Lstat] or [os.DirEntry.Info]; path segments are joined only for reparse points.
func Linked(info os.FileInfo, parts ...string) os.FileInfo {
	if info.Mode()&os.ModeIrregular == 0 {
		return info
	}
	// Only symlinks and junctions have a target; other reparse points stay irregular.
	if _, err := os.Readlink(filepath.Join(parts...)); err != nil {
		return info
	}
	return junction{info}
}

// junction is a junction's metadata typed as a symlink.
type junction struct{ os.FileInfo }

// Mode replaces the irregular type with the symlink type.
func (j junction) Mode() os.FileMode { return j.FileInfo.Mode()&^os.ModeIrregular | os.ModeSymlink }

// longPath adds the \\?\ prefix to long absolute drive paths, which the os
// package does internally and plain CreateFile needs (MAX_PATH is 260).
func longPath(p string) string {
	if len(p) < 248 || strings.HasPrefix(p, `\\`) || !filepath.IsAbs(p) {
		return p
	}
	return `\\?\` + filepath.Clean(p)
}

// Unresolvable reports whether err from a stat means the path can never resolve:
// a symlink loop, as against one that is merely absent or unreadable for now. A
// path through a file is already ERROR_PATH_NOT_FOUND here, which is
// [os.ErrNotExist].
func Unresolvable(err error) bool {
	return errors.Is(err, windows.ERROR_CANT_RESOLVE_FILENAME)
}
