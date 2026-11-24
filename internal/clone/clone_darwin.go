package clone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Reflink clones the open regular file in to the new path dst with mode, sharing its blocks.
// fclonefileat clones the already-open inode, so a path swapped after open cannot redirect the clone.
func Reflink(in *os.File, dst string, mode os.FileMode) error {
	err := unix.Fclonefileat(int(in.Fd()), unix.AT_FDCWD, dst, unix.CLONE_NOFOLLOW)
	if unsupported(err) {
		return fmt.Errorf("clone to %s: %w: %w", dst, errors.ErrUnsupported, err)
	}
	if err != nil {
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		os.Remove(dst)
		return fmt.Errorf("clone to %s: %w", dst, err)
	}
	return nil
}

// cloneHierarchy clones the regular file, directory or symlink src to the new
// path dst in one clonefile(2) call, which takes a directory's whole hierarchy
// along with every entry's mode, extended attributes and access time.
// Populating directories may change their mtimes, so restore those after it.
// info describes src.
// It reports false, leaving dst absent, where the volume cannot clone.
func cloneHierarchy(src, dst string, info fs.FileInfo) (bool, error) {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if unsupported(err) {
		return false, nil
	}
	if err != nil {
		return false, &os.LinkError{Op: "clonefile", Old: src, New: dst, Err: err}
	}
	if !info.IsDir() {
		return true, nil
	}
	return true, dirTimes(src, dst)
}

// dirTimes restores mtimes below dst without overwriting the clone's atimes.
// It lists the private clone rather than src, so a directory that appeared
// or vanished in src since the clone is skipped.
func dirTimes(src, dst string) error {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := dirTimes(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
	}
	info, err := os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !info.IsDir() {
		return nil
	}
	if err != nil {
		return err
	}
	mtime := info.ModTime()
	if len(entries) == 0 {
		// A clone can keep an empty directory's mtime because it adds no
		// children. Setting even the same mtime would dirty its metadata.
		cloned, err := os.Lstat(dst)
		if err != nil {
			return err
		}
		if mtime.Equal(cloned.ModTime()) {
			return nil
		}
	}
	return os.Chtimes(dst, time.Time{}, mtime)
}
