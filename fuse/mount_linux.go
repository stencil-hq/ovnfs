package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stencil-hq/ovnfs/export"
	"golang.org/x/sys/unix"
)

// Server is an active FUSE mount of the export root.
type Server struct {
	srv        *gofuse.Server
	fs         *exportFS
	mountpoint string
	abort      *os.File // this connection's fusectl abort file; nil without fusectl
}

// Mount serves e at mountpoint until Unmount, creating the directory. A dead
// mount left there by a server that was killed before it unmounted (kill -9,
// the OOM killer) is detached first, since it fails both mkdir and a new
// mount. It mounts through fusermount3 (or fusermount), so it needs no root
// where the distribution allows unprivileged FUSE mounts.
func Mount(e *export.Export, mountpoint string, opts Options) (*Server, error) {
	if err := detachDead(mountpoint); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, fmt.Errorf("fuse: mount directory: %w", err)
	}
	fsName := opts.FsName
	if fsName == "" {
		fsName = "ovnfs"
	}
	// The kernel checks modes and owners itself; nodev and nosuid are what
	// fusermount forces on unprivileged mounts anyway.
	options := []string{"default_permissions", "nodev", "nosuid"}
	if !opts.Atime {
		options = append(options, "noatime")
	}
	fs := newExportFS(e.Nodes())
	srv, err := gofuse.NewServer(fs, mountpoint, &gofuse.MountOptions{
		AllowOther: opts.AllowOther,
		FsName:     fsName,
		Name:       "ovnfs",
		Options:    options,
		MaxWrite:   gofuse.MAX_KERNEL_WRITE,
		Debug:      opts.Debug,
	})
	if err != nil {
		return nil, fmt.Errorf("fuse: mount %s: %w", mountpoint, err)
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		srv.Unmount()
		return nil, fmt.Errorf("fuse: mount %s: %w", mountpoint, err)
	}
	// fusectl names a connection by its mount's device number in the kernel's
	// encoding (major<<20 | minor). The file stays open: once the connection
	// is gone a write to it is a no-op, where the path could by then name a
	// later mount given the same number. Without fusectl it stays nil and a
	// forced Unmount can only unmount plainly.
	var stx unix.Statx_t
	var abort *os.File
	if err := unix.Statx(unix.AT_FDCWD, mountpoint, unix.AT_STATX_DONT_SYNC, 0, &stx); err == nil {
		abort, _ = os.OpenFile(fmt.Sprintf("/sys/fs/fuse/connections/%d/abort", stx.Dev_major<<20|stx.Dev_minor), os.O_WRONLY, 0)
	}
	return &Server{srv: srv, fs: fs, mountpoint: mountpoint, abort: abort}, nil
}

// detachDead lazily unmounts mountpoint when it is a FUSE mount whose server
// is gone, which the kernel answers with ENOTCONN; anything else, a live mount
// included, is left alone. Lazily, because processes may still hold the dead
// mount as their working directory.
func detachDead(mountpoint string) error {
	// Forced: a plain stat is answered from attributes the kernel still caches
	// for up to TTL after the server died.
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, mountpoint, unix.AT_STATX_FORCE_SYNC, unix.STATX_TYPE, &stx); !errors.Is(err, unix.ENOTCONN) {
		return nil
	}
	bin, err := exec.LookPath("fusermount3")
	if err != nil {
		if bin, err = exec.LookPath("fusermount"); err != nil {
			return fmt.Errorf("fuse: detach dead mount %s: %w", mountpoint, err)
		}
	}
	if out, err := exec.Command(bin, "-u", "-z", mountpoint).CombinedOutput(); err != nil {
		return fmt.Errorf("fuse: detach dead mount %s: %w: %s", mountpoint, err, bytes.TrimSpace(out))
	}
	return nil
}

// Unmount ends the mount and returns once the server has stopped. It fails
// while files on it are in use, unless force is set: then it aborts the
// connection, so whatever still uses the mount fails (ENOTCONN; EIO for
// reads of uncached data) and no request reaches the export any more, and
// detaches the dead mount. Without fusectl at mount time force cannot abort,
// and a busy mount fails as without it.
func (s *Server) Unmount(force bool) error {
	if err := s.srv.Unmount(); err != nil {
		err = fmt.Errorf("fuse: unmount %s: %w", s.mountpoint, err)
		if !force {
			return err
		}
		if s.abort == nil {
			return fmt.Errorf("%w (not forced: no fusectl at /sys/fs/fuse/connections when it was mounted)", err)
		}
		if _, err := s.abort.Write([]byte("1")); err != nil {
			return fmt.Errorf("fuse: abort %s: %w", s.mountpoint, err)
		}
		s.srv.Wait()
		// Only the dead mount: the path may since have been unmounted, or
		// hold a later live mount.
		if err := detachDead(s.mountpoint); err != nil {
			return err
		}
	}
	if s.abort != nil {
		s.abort.Close()
		s.abort = nil
	}
	return nil
}

// Invalidate drops the kernel's cached copies of checkout paths the daemon
// changed behind the mount, and of the roots of trees; see
// [checkout.Config.Invalidate]. The caller must hold no lock a FUSE request
// takes.
func (s *Server) Invalidate(checkout string, paths, trees []string) {
	s.fs.invalidate(checkout, paths, trees)
}

// Wait blocks until the mount is gone, by Unmount or an external umount.
func (s *Server) Wait() { s.srv.Wait() }
