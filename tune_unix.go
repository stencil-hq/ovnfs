//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

func tuneDetach(cmd *exec.Cmd) { cmd.SysProcAttr = detachedProcess() }

func tuneKill(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func tuneMount(root string) (string, error) {
	if runtime.GOOS == "linux" {
		// FUSE confinement permits /tmp even when state lives on another volume.
		return os.MkdirTemp("", "ovnfs-tune-mount-")
	}
	return filepath.Join(root, "mount"), nil
}

func tuneRemoveMount(mount string) error {
	err := os.Remove(mount)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func tuneUnmount(ctx context.Context, proto, mount string) error {
	if !mounted(mount) {
		return nil
	}
	if proto == "fuse" && runtime.GOOS == "linux" {
		helper, err := exec.LookPath("fusermount3")
		if err != nil {
			helper, err = exec.LookPath("fusermount")
		}
		if err != nil {
			return err
		}
		if _, err = mountOutput(ctx, []string{helper, "-u", "-z", mount}); err != nil {
			return err
		}
		if mounted(mount) {
			return errors.New("private FUSE mount is still attached")
		}
		return nil
	}
	return unmount(mount, true)
}
