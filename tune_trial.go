package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/manifest"
)

// tuneOutput bounds worker execution even when a killed process cannot yet be
// reaped. Workers own a process group so Cargo's children are killed with them.
func tuneOutput(ctx context.Context, args []string, dir string, env []string) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir, cmd.Env = dir, env
		tuneDetach(cmd)
		cmd.Cancel = func() error { return tuneKill(cmd.Process) }
		cmd.WaitDelay = 100 * time.Millisecond
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		err := cmd.Run()
		if err != nil {
			text := diagnostic.Bytes()
			if len(text) > 4096 {
				text = text[len(text)-4096:]
			}
			err = fmt.Errorf("%s: %w: %s", filepath.Base(args[0]), err, bytes.TrimSpace(text))
		}
		done <- result{out.Bytes(), err}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return r.data, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tuneTrial keeps all mounted filesystem calls in killable subprocesses. Its
// second error is a cleanup failure: callers must stop and retain the scratch
// root rather than traversing a possibly still-mounted directory to remove it.
func tuneTrial(ctx context.Context, root, source, home, exe string, base serveCmd, config tuneConfig, cargo *tuneProject, runs int, timeout time.Duration) (result tuneResult, cleanupErr error) {
	result.Config = config
	trial, err := os.MkdirTemp(root, "trial-")
	if err != nil {
		result.Error = err.Error()
		return
	}
	mount, err := tuneMount(trial)
	if err != nil {
		result.Error = err.Error()
		cleanupErr = os.RemoveAll(trial)
		return
	}
	started := false
	defer func() {
		if !started {
			cleanupErr = errors.Join(tuneRemoveMount(mount), os.RemoveAll(trial))
		}
	}()
	state, privateHome := filepath.Join(trial, "state"), filepath.Join(trial, "home")
	command := func(args ...string) []string {
		out := make([]string, 0, 5+len(args))
		out = append(out, exe, "--state", state, "--mount", mount)
		return append(out, args...)
	}
	if err = os.MkdirAll(privateHome, 0700); err != nil {
		result.Error = err.Error()
		return
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		result.Error = err.Error()
		return
	}
	if data, readErr := os.ReadFile(manifest.UserDoc(home).Path); readErr == nil {
		path := manifest.UserDoc(privateHome).Path
		if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
			err = os.WriteFile(path, data, 0600)
		}
		if err != nil {
			result.Error = err.Error()
			return
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		result.Error = readErr.Error()
		return
	}
	log, err := os.Create(logPath(state))
	if err != nil {
		result.Error = err.Error()
		return
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"serve", "--state", state, "--mount", mount, "--proto", config.Proto,
		"--memory", config.Memory.String(), "--nfs-io-size", config.NFSIOSize.String(),
		"--blob-cache", base.BlobCache.String(), "--node-cache", base.NodeCache.String(),
		"--fsync=" + strconv.FormatBool(base.Fsync), "--atime=" + strconv.FormatBool(base.Atime), "--no-git-remotes"}
	srv := exec.CommandContext(wait, exe, args...)
	srv.Env = append(os.Environ(), "HOME="+privateHome, "USERPROFILE="+privateHome)
	srv.Stdout, srv.Stderr = log, log
	tuneDetach(srv)
	srv.Cancel = func() error { return tuneKill(srv.Process) }
	err = srv.Start()
	log.Close()
	if err != nil {
		result.Error = err.Error()
		return
	}
	started = true
	exited, stopped := make(chan error, 1), make(chan struct{})
	go func() { err := srv.Wait(); close(stopped); exited <- err }()
	kill := func() {
		select {
		case <-stopped:
			return
		default:
			_ = tuneKill(srv.Process)
		}
	}
	defer func() {
		// Stop gracefully first; a failed or wedged trial contains only scratch
		// data, so cleanup may kill it and force-detach its private mount.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
		defer stop()
		stopCtx, stopCall := context.WithTimeout(cleanup, 5*time.Second)
		_, stopErr := tuneOutput(stopCtx, command("stop"), "", nil)
		stopCall()
		if stopErr != nil {
			kill()
		}
		_, detachErr := tuneOutput(cleanup, command("tune-unmount", config.Proto, mount), "", nil)
		kill()
		select {
		case <-stopped:
		case <-cleanup.Done():
			cleanupErr = fmt.Errorf("trial process did not exit; retained %s: %w", trial, cleanup.Err())
			return
		}
		if detachErr != nil {
			cleanupErr = fmt.Errorf("trial mount cleanup failed; retained %s at %s: %w", trial, mount, detachErr)
			return
		}
		if err := tuneRemoveMount(mount); err != nil {
			cleanupErr = err
			return
		}
		cleanupErr = os.RemoveAll(trial)
	}()
	client, err := awaitDaemon(wait, state, srv.Process.Pid, exited)
	if err != nil {
		result.Error = err.Error()
		return
	}
	defer client.Close()
	private := &cli{ctx: wait, state: state, mount: mount, conn: client}
	create := func(name, dir string, rules []string) (string, error) {
		raw, err := private.call("new", api.NewArgs{Dir: dir, Name: name, Rules: rules})
		reply, err := decodeReply[api.NewReply]("new", raw, err)
		return reply.Checkout.Path, err
	}
	var samples [][]tuneMetric
	for i := range runs {
		name := fmt.Sprintf("io-%d", i)
		path, err := create(name, source, nil)
		if err != nil {
			result.Error = err.Error()
			return
		}
		disk := filepath.Join(trial, fmt.Sprintf("disk-%d", i))
		if err := os.Mkdir(disk, 0700); err != nil {
			result.Error = err.Error()
			return
		}
		out, err := tuneOutput(wait, command("tune-work", disk, path), "", nil)
		if err != nil {
			result.Error = err.Error()
			return
		}
		var sample []tuneMetric
		if err := json.Unmarshal(out, &sample); err != nil {
			result.Error = fmt.Sprintf("workload report: %v", err)
			return
		}
		samples = append(samples, sample)
		if _, err := private.call("rm", api.RmArgs{Checkout: name}); err != nil {
			result.Error = err.Error()
			return
		}
		if err := os.RemoveAll(disk); err != nil {
			result.Error = err.Error()
			return
		}
	}
	result.Metrics, err = medianTune(samples)
	if err != nil {
		result.Error = err.Error()
		return
	}
	if cargo != nil {
		path, err := create("cargo", cargo.Root, []string{"hidden dir & target"})
		if err != nil {
			result.Error = err.Error()
			return
		}
		raw, snapErr := private.call("snap", api.SnapArgs{Checkout: "cargo", Wait: true})
		snapshot, snapErr := decodeReply[api.SnapReply]("snap", raw, snapErr)
		if snapErr != nil {
			result.Error = snapErr.Error()
			return
		}
		if snapshot.Snapshot.Tree == "" {
			result.Error = "Cargo source snapshot has no content identity"
			return
		}
		if cargo.Tree == "" {
			cargo.Tree = snapshot.Snapshot.Tree
		} else if cargo.Tree != snapshot.Snapshot.Tree {
			cargo.Changed = true
			result.Error = "Cargo source changed between trials"
			return
		}
		path = filepath.Join(path, cargo.Subdir)
		args := command("tune-cargo", path, cargo.Touch, "--runs", strconv.Itoa(runs))
		if cargo.Toolchain != "" {
			args = append(args, "--toolchain", cargo.Toolchain)
		}
		out, err := tuneOutput(wait, args, "", nil)
		if err != nil {
			result.Error = err.Error()
			return
		}
		var metrics []tuneMetric
		if err := json.Unmarshal(out, &metrics); err != nil {
			result.Error = fmt.Sprintf("Cargo report: %v", err)
			return
		}
		result.Metrics = append(result.Metrics, metrics...)
	}
	return
}

// tuneUnmountCmd bounds private-mount cleanup outside the orchestration process.
type tuneUnmountCmd struct {
	Proto string `arg:""`
	Mount string `arg:""`
}

// Run detaches only the private mount named by the tuning supervisor.
func (cmd *tuneUnmountCmd) Run(c *cli) error { return tuneUnmount(c.ctx, cmd.Proto, cmd.Mount) }
