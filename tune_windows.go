//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

func tuneDetach(cmd *exec.Cmd) { cmd.SysProcAttr = detachedProcess() }

func tuneKill(process *os.Process) error {
	if process == nil {
		return nil
	}
	// taskkill includes compiler children; a direct Process.Kill does not.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	killer := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe"), "/T", "/F", "/PID", strconv.Itoa(process.Pid))
	killer.WaitDelay = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- killer.Run() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
	case <-ctx.Done():
	}
	return process.Kill()
}

func tuneMount(string) (string, error) {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return "", err
	}
	for letter := 'Z'; letter >= 'D'; letter-- {
		if drives&(1<<(letter-'A')) != 0 {
			continue
		}
		drive := string(letter) + ":"
		remote, err := driveRemote(drive)
		if err != nil {
			return "", err
		}
		if remote == "" {
			return drive + `\`, nil
		}
	}
	return "", errors.New("tune needs a free drive letter")
}

// A detached Windows drive has no directory to remove.
func tuneRemoveMount(string) error { return nil }

func tuneUnmount(_ context.Context, _ string, mount string) error { return unmount(mount, true) }
