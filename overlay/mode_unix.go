//go:build !windows

package overlay

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// Unix files hold their mode natively; see mode_windows.go for hosts that cannot.

// sameFile accepts native directory metadata as well as os.Lstat records.
// Stored layers use their own link groups: historical inode numbers can be reused.
func sameFile(a, b os.FileInfo) bool {
	a, b = unalias(a), unalias(b)
	if _, stored := a.(storedInfo); stored {
		return false
	}
	if _, stored := b.(storedInfo); stored {
		return false
	}
	x, ok := a.Sys().(*syscall.Stat_t)
	y, ok2 := b.Sys().(*syscall.Stat_t)
	return ok && ok2 && x != nil && y != nil && x.Dev == y.Dev && x.Ino == y.Ino
}

// hostLstat is [os.Lstat].
func hostLstat(name string) (os.FileInfo, error) { return os.Lstat(name) }

// lstatIndexed is [os.Lstat]: the host's mode is current.
func lstatIndexed(name string, _ os.FileInfo) (os.FileInfo, error) { return os.Lstat(name) }

// withMode returns info: the host's mode is the file's.
func withMode(_ string, info os.FileInfo) (os.FileInfo, error) { return info, nil }

// keepMode has nothing to add after the host chmod or create that set the mode.
func keepMode(string, os.FileMode) error { return nil }

// lchmod changes the symlink name's own mode; Linux refuses it with EOPNOTSUPP.
func lchmod(name string, mode os.FileMode) error {
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= unix.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		bits |= unix.S_ISGID
	}
	if mode&os.ModeSticky != 0 {
		bits |= unix.S_ISVTX
	}
	if err := unix.Fchmodat(unix.AT_FDCWD, name, bits, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "chmod", Path: name, Err: err}
	}
	return nil
}

// makeNode makes only data-free nodes. A socket is bound at tmp, a free
// staging name on name's filesystem, through a short symlink to its directory,
// so neither the upper's path nor name has to fit sockaddr_un's limit, then is
// linked into place without replacing an existing name.
func makeNode(name, tmp string, mode os.FileMode) error {
	switch mode & os.ModeType {
	case os.ModeNamedPipe:
		if err := unix.Mkfifo(name, uint32(mode.Perm())); err != nil {
			return &fs.PathError{Op: "mknod", Path: name, Err: err}
		}
	case os.ModeSocket:
		alias, err := os.MkdirTemp("", "ovnfs-node-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(alias)
		link := filepath.Join(alias, "d")
		if err := os.Symlink(filepath.Dir(tmp), link); err != nil {
			return err
		}
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(link, filepath.Base(tmp)), Net: "unix"})
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			return err
		}
		if err := os.Chmod(tmp, mode.Perm()); err != nil {
			return err
		}
		if err := os.Link(tmp, name); err != nil {
			return err
		}
	default:
		return &fs.PathError{Op: "mknod", Path: name, Err: unix.EPERM}
	}
	return nil
}
