//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// mountCommandWait bounds each native inspection, mount or unmount command.
const mountCommandWait = 5 * time.Second

// mountOutput bounds even an unkillable kernel wait: the child is killed and
// reaped asynchronously, never holding up the caller after its deadline.
func mountOutput(ctx context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, mountCommandWait)
	defer cancel()
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		// Resolve PATH here too: it can contain a stalled mount.
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		cmd.WaitDelay = 100 * time.Millisecond
		out, err := cmd.CombinedOutput()
		done <- result{out, err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", filepath.Base(args[0]), ctx.Err())
	case r := <-done:
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(args[0]), ctx.Err())
		}
		if r.err != nil {
			return r.out, fmt.Errorf("%s: %w: %s", filepath.Base(args[0]), r.err, bytes.TrimSpace(r.out))
		}
		return r.out, nil
	}
}
