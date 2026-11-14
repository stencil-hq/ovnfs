//go:build !windows

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

// smbClientPort is 0: mount_smbfs and mount.cifs dial any port.
func smbClientPort() int { return 0 }

// mountRoots is empty: Unix kernels resolve absolute links through the mount.
func mountRoots(string) []string { return nil }

// smbAccount is empty: mount_smbfs and mount.cifs connect as guest.
func smbAccount(string) (user, password string, err error) { return "", "", nil }

// mountServer mounts the localhost NFS or SMB server at addr on mount with the
// system client with the requested settings.
func mountServer(proto string, addr net.Addr, mount, user, password string, settings mountSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	cmd := mountCmd(proto, addr, mount, settings, "ovnfs", user, password)
	_, err := mountOutput(context.Background(), cmd)
	return err
}

// matchMount preserves compatible mounts. macOS cannot update NFS buffer
// sizes in place, so mismatched parameters require a bounded forced unmount.
func matchMount(proto string, addr net.Addr, dir string, settings mountSettings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" && proto == "nfs" {
		out, err := mountOutput(context.Background(), []string{"/usr/bin/nfsstat", "-f", "JSON", "-m", dir})
		if err != nil {
			return err
		}
		cmd := mountCmd(proto, addr, dir, settings, "ovnfs", "", "")
		reason, err := nfsMountMismatch(out, mountPath(dir), cmd)
		if err != nil {
			return err
		}
		if reason == "" {
			return nil
		}
		log.Printf("replacing NFS mount at %s: %s; forcing unmount (open handles and unflushed writes may be lost)", dir, reason)
		if err := unmount(dir, true); err != nil {
			return fmt.Errorf("replacing NFS mount: %w", err)
		}
		return mountServer(proto, addr, dir, "", "", settings)
	}
	if current, ok := mountAtime(dir); !ok || current == settings.Atime {
		return nil
	}
	cmd := remountCmd(dir, atimeOption(settings.Atime))
	_, err := mountOutput(context.Background(), cmd)
	return err
}

// makeMountPoint creates the directory the mount covers.
func makeMountPoint(dir string) error { return os.MkdirAll(dir, 0o755) }

// unmount detaches either protocol; an already removed mount counts as success.
func unmount(dir string, force bool) error {
	if !mounted(dir) {
		return nil
	}
	args := []string{"umount", dir}
	if force {
		args = []string{"umount", "-f", dir}
	}
	if _, err := mountOutput(context.Background(), args); err != nil {
		return err
	}
	if mounted(dir) {
		return fmt.Errorf("%s is still mounted after umount", dir)
	}
	return nil
}

// mountPath resolves aliases in the parent without entering a possibly hung mount.
func mountPath(dir string) string {
	dir = filepath.Clean(dir)
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return dir
	}
	return filepath.Join(parent, filepath.Base(dir))
}
