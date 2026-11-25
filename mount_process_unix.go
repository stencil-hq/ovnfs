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
	"sync/atomic"
	"syscall"
	"time"
)

// mountCommandWait bounds each native inspection, mount or unmount command.
const mountCommandWait = 5 * time.Second

// hungCommand is a command still running at its deadline. A kernel wait can
// keep it alive even after SIGKILL.
type hungCommand struct {
	name string
	pid  int
	err  error
}

func (h *hungCommand) Error() string { return h.name + ": " + h.err.Error() }
func (h *hungCommand) Unwrap() error { return h.err }

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
	var pid atomic.Int64
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
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Start()
		if err == nil {
			pid.Store(int64(cmd.Process.Pid))
			err = cmd.Wait()
		}
		done <- result{out.Bytes(), err}
	}()
	select {
	case <-ctx.Done():
		return nil, &hungCommand{filepath.Base(args[0]), int(pid.Load()), ctx.Err()}
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
