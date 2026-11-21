//go:build !windows

package hostfile

import (
	"errors"
	"os"
	"syscall"
)

// OpenFile is [os.OpenFile]: Unix descriptors never block renames or unlinks.
func OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

// Rename is [os.Rename], which already replaces open files.
func Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Remove is [os.Remove], which never changes other hard links' modes.
func Remove(name string) error { return os.Remove(name) }

// CreateTemp is [os.CreateTemp].
func CreateTemp(dir, pattern string) (*os.File, error) { return os.CreateTemp(dir, pattern) }

// Symlink is [os.Symlink]; every Unix account may create symlinks.
func Symlink(target, link string) error { return os.Symlink(target, link) }

// Linked returns info without joining its path segments: Unix has no junctions.
func Linked(info os.FileInfo, _ ...string) os.FileInfo { return info }

// Unresolvable reports whether err from a stat means the path can never resolve:
// a symlink loop or a path through a file, as against one that is merely absent
// or unreadable for now.
func Unresolvable(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR)
}
