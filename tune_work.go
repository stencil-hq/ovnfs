package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// tuneWorkCmd confines every potentially blocking mount operation to a killable worker.
type tuneWorkCmd struct {
	Disk  string `arg:""`
	Ovnfs string `arg:""`
}

// Run measures I/O and metadata through the real native and mounted paths.
func (cmd *tuneWorkCmd) Run(c *cli) (err error) {
	var roots [2]string
	for i, dir := range []string{cmd.Disk, cmd.Ovnfs} {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		root, mkdirErr := os.MkdirTemp(dir, ".ovnfs-tune-work-")
		if mkdirErr != nil {
			return mkdirErr
		}
		defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
		roots[i] = root
	}
	rows, err := measureBenchFiles(c.ctx, roots[0], roots[1])
	if err != nil {
		return err
	}
	metrics := make([]tuneMetric, 0, len(rows)+2)
	for _, row := range rows {
		metrics = append(metrics, tuneMetric{Name: row.Name, Group: "io", Unit: "MiB/s", Rate: row.Ovnfs.MiBPS, Native: row.Disk.MiBPS, Ratio: row.Ratio})
	}
	metadata, err := measureTuneMetadata(c.ctx, roots, benchSample)
	if err != nil {
		return err
	}
	metrics = append(metrics, metadata...)
	return json.NewEncoder(stdout).Encode(metrics)
}

// The lifecycle has one live artifact; the fingerprint tree has 128 256-byte
// files. Repetition never increases either working set. Fixture setup is untimed.
func measureTuneMetadata(ctx context.Context, roots [2]string, duration time.Duration) (metrics []tuneMetric, err error) {
	var dirs [2]string
	for i, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir, mkdirErr := os.MkdirTemp(root, "metadata-")
		if mkdirErr != nil {
			return nil, mkdirErr
		}
		defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
		dirs[i] = dir
	}
	var rates [2]float64
	var modes [2]string
	for _, side := range []int{0, 1} {
		rates[side], modes[side], err = measureTuneArtifacts(ctx, dirs[side], duration)
		if err != nil {
			return nil, fmt.Errorf("artifact lifecycle on %s: %w", roots[side], err)
		}
	}
	metrics = append(metrics, tuneMetric{Name: "Artifact lifecycle (link/copy)", Details: fmt.Sprintf("native: %s; mounted: %s", modes[0], modes[1]), Group: "metadata", Unit: "artifacts/s", Rate: rates[1], Native: rates[0], Ratio: rates[1] / rates[0]})
	for _, side := range []int{1, 0} {
		rates[side], err = measureTuneFingerprint(ctx, dirs[side], duration)
		if err != nil {
			return nil, fmt.Errorf("tree fingerprint on %s: %w", roots[side], err)
		}
	}
	metrics = append(metrics, tuneMetric{Name: "Tree stat/readdir/fingerprint", Group: "metadata", Unit: "entries/s", Rate: rates[1], Native: rates[0], Ratio: rates[1] / rates[0]})
	return metrics, nil
}

func tuneWrite(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func tuneWriteFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := tuneWrite(f, data); err != nil {
		return err
	}
	return f.Sync()
}

// Copy fallback reads the actual source rather than rewriting a remembered
// payload. Both publication paths are subsequently read back and byte-checked.
func tuneCopyFile(src, dst string, buffer []byte, size int64) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	n, err := io.CopyBuffer(out, in, buffer)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("artifact copy: got %d bytes, want %d", n, size)
	}
	return out.Sync()
}

func tuneCheckFile(path string, data, buffer []byte) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	n, err := io.ReadFull(f, buffer[:len(data)])
	if err != nil {
		return err
	}
	if n != len(data) || !bytes.Equal(buffer[:n], data) {
		return fmt.Errorf("%s: artifact bytes differ", path)
	}
	n, err = f.Read(buffer[:1])
	if n != 0 || err != io.EOF {
		return fmt.Errorf("%s: expected EOF after %d bytes: n=%d, err=%v", path, len(data), n, err)
	}
	return nil
}

func tuneLinkUnsupported(err error) bool {
	if runtime.GOOS == "windows" {
		// CreateHardLink returns native Win32 codes, not Go's POSIX errno
		// aliases. Keep these here so the worker needs no platform-only API.
		const (
			errorInvalidFunction syscall.Errno = 1
			errorNotSameDevice   syscall.Errno = 17
			errorNotSupported    syscall.Errno = 50
		)
		if errors.Is(err, errorInvalidFunction) || errors.Is(err, errorNotSameDevice) || errors.Is(err, errorNotSupported) {
			return true
		}
	}
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}

func measureTuneArtifacts(ctx context.Context, dir string, duration time.Duration) (float64, string, error) {
	if duration <= 0 {
		return 0, "", fmt.Errorf("artifact duration must be positive")
	}
	data, buffer := make([]byte, 4096), make([]byte, 4096)
	for i := range data {
		data[i] = byte(i*37 + i/256)
	}
	tmp, artifact, published := filepath.Join(dir, "artifact.tmp"), filepath.Join(dir, "artifact"), filepath.Join(dir, "published")
	started := time.Now()
	var count, links, copies int64
	for time.Since(started) < duration || count == 0 {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		if err := tuneWriteFile(tmp, data); err != nil {
			return 0, "", err
		}
		if err := os.Rename(tmp, artifact); err != nil {
			return 0, "", err
		}
		if err := os.Link(artifact, published); err != nil {
			if !tuneLinkUnsupported(err) {
				return 0, "", err
			}
			if err := tuneCopyFile(artifact, published, buffer, int64(len(data))); err != nil {
				return 0, "", err
			}
			copies++
		} else {
			links++
		}
		if err := tuneCheckFile(published, data, buffer); err != nil {
			return 0, "", err
		}
		for _, path := range []string{published, artifact} {
			if err := os.Remove(path); err != nil {
				return 0, "", err
			}
		}
		count++
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	mode := "link"
	if copies > 0 {
		mode = "copy"
		if links > 0 {
			mode = "link+copy"
		}
	}
	return float64(count) / time.Since(started).Seconds(), mode, nil
}

func measureTuneFingerprint(ctx context.Context, dir string, duration time.Duration) (float64, error) {
	if duration <= 0 {
		return 0, fmt.Errorf("fingerprint duration must be positive")
	}
	root := filepath.Join(dir, "tree")
	if err := os.Mkdir(root, 0o700); err != nil {
		return 0, err
	}
	data, buffer := make([]byte, 256), make([]byte, 256)
	for i := range data {
		data[i] = byte(i*53 + 7)
	}
	for d := range 8 {
		child := filepath.Join(root, fmt.Sprintf("crate-%02d", d))
		if err := os.Mkdir(child, 0o700); err != nil {
			return 0, err
		}
		for f := range 16 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if err := tuneWriteFile(filepath.Join(child, fmt.Sprintf("source-%02d.rs", f)), data); err != nil {
				return 0, err
			}
		}
	}
	wantHash := sha256.Sum256(data)
	started := time.Now()
	var count int64
	for time.Since(started) < duration || count == 0 {
		children, err := os.ReadDir(root)
		if err != nil {
			return 0, err
		}
		if len(children) != 8 {
			return 0, fmt.Errorf("fingerprint tree: got %d directories, want 8", len(children))
		}
		for _, child := range children {
			entries, err := os.ReadDir(filepath.Join(root, child.Name()))
			if err != nil {
				return 0, err
			}
			if len(entries) != 16 {
				return 0, fmt.Errorf("fingerprint tree %s: got %d files, want 16", child.Name(), len(entries))
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				path := filepath.Join(root, child.Name(), entry.Name())
				info, err := os.Stat(path)
				if err != nil {
					return 0, err
				}
				if !info.Mode().IsRegular() || info.Size() != int64(len(data)) {
					return 0, fmt.Errorf("%s: invalid fingerprint source", path)
				}
				if err := tuneCheckFile(path, data, buffer); err != nil {
					return 0, err
				}
				if sha256.Sum256(buffer) != wantHash {
					return 0, fmt.Errorf("%s: fingerprint mismatch", path)
				}
				count++
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return float64(count) / time.Since(started).Seconds(), nil
}

// tuneCargoCmd only changes a disposable checkout; compiler children inherit
// this worker's process group so the supervisor can kill the entire build.
type tuneCargoCmd struct {
	Dir       string `arg:""`
	Touch     string `arg:""`
	Runs      int    `default:"3"`
	Toolchain string
}

// Run measures real cold, no-op and incremental Cargo builds in a disposable checkout.
func (cmd *tuneCargoCmd) Run(c *cli) error {
	if cmd.Runs < 1 {
		return fmt.Errorf("Cargo runs must be positive")
	}
	dir, err := filepath.Abs(cmd.Dir)
	if err != nil {
		return err
	}
	touch, err := tuneCargoTouchPath(dir, cmd.Touch)
	if err != nil {
		return err
	}
	env := tuneCargoEnv(dir, cmd.Toolchain)
	cold, err := tuneCargoBuild(c.ctx, dir, env)
	if err != nil {
		return fmt.Errorf("Cargo cold: %w", err)
	}
	noop, incremental := make([]float64, 0, cmd.Runs), make([]float64, 0, cmd.Runs)
	for range cmd.Runs {
		seconds, err := tuneCargoBuild(c.ctx, dir, env)
		if err != nil {
			return fmt.Errorf("Cargo no-op: %w", err)
		}
		noop = append(noop, seconds)
		info, err := os.Stat(touch)
		if err != nil {
			return err
		}
		// Wait past coarse timestamp resolution rather than assigning a future
		// mtime, which would make subsequent supposedly no-op builds dirty.
		delay := time.Until(info.ModTime().Add(time.Second))
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return c.ctx.Err()
			case <-timer.C:
			}
		}
		now := time.Now()
		if err := hostfile.Lchtimes(touch, time.Time{}, now); err != nil {
			return err
		}
		updated, err := os.Stat(touch)
		if err != nil {
			return err
		}
		if !updated.ModTime().After(info.ModTime()) {
			return fmt.Errorf("Cargo touch did not advance %s mtime", cmd.Touch)
		}
		seconds, err = tuneCargoBuild(c.ctx, dir, env)
		if err != nil {
			return fmt.Errorf("Cargo incremental: %w", err)
		}
		incremental = append(incremental, seconds)
	}
	return json.NewEncoder(stdout).Encode([]tuneMetric{
		{Name: "Cargo cold", Group: "cargo", Unit: "builds/s", Rate: 1 / cold},
		{Name: "Cargo no-op", Group: "cargo", Unit: "builds/s", Rate: 1 / tuneMedianDuration(noop)},
		{Name: "Cargo incremental", Group: "cargo", Unit: "builds/s", Rate: 1 / tuneMedianDuration(incremental)},
	})
}

func tuneCargoTouchPath(dir, source string) (string, error) {
	if !filepath.IsLocal(source) || filepath.Ext(source) != ".rs" {
		return "", fmt.Errorf("Cargo touch must be a relative Rust source file within the checkout")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, source))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("Cargo touch escapes the disposable checkout")
	}
	if rel == "target" || strings.HasPrefix(rel, "target"+string(filepath.Separator)) {
		return "", fmt.Errorf("Cargo touch must not be inside target")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Cargo touch must be a regular source file")
	}
	return path, nil
}

// The supervisor resolves the original workspace's effective toolchain before
// relocating it. Without one, retain normal Cargo/rustup discovery and overrides.
func tuneCargoEnv(dir, toolchain string) []string {
	env := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "CARGO_TARGET_DIR" || key == "CARGO_INCREMENTAL" || key == "RUSTUP_AUTO_INSTALL" || key == "RUSTUP_TOOLCHAIN" && toolchain != "" || key == "RUSTC" || key == "CARGO_BUILD_RUSTC" || strings.Contains(key, "RUSTC") && strings.Contains(key, "WRAPPER") {
			continue
		}
		env = append(env, item)
	}
	if toolchain != "" {
		env = append(env, "RUSTUP_TOOLCHAIN="+toolchain)
	}
	return append(env, "CARGO_TARGET_DIR="+filepath.Join(dir, "target"), "CARGO_INCREMENTAL=1", "RUSTUP_AUTO_INSTALL=0", "RUSTC_WRAPPER=", "RUSTC_WORKSPACE_WRAPPER=", "CARGO_BUILD_RUSTC_WRAPPER=", "CARGO_BUILD_RUSTC_WORKSPACE_WRAPPER=")
}

// Keep the diagnostic tail: Cargo's final compiler errors are more useful than
// dependency progress at the start. os/exec serializes writes to a shared sink.
type tuneCargoOutput struct{ data []byte }

func (b *tuneCargoOutput) Write(p []byte) (int, error) {
	const capacity = 32 << 10
	n := len(p)
	if len(p) >= capacity {
		b.data = append(b.data[:0], p[len(p)-capacity:]...)
	} else {
		if excess := len(b.data) + len(p) - capacity; excess > 0 {
			copy(b.data, b.data[excess:])
			b.data = b.data[:len(b.data)-excess]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func tuneCargoBuild(ctx context.Context, dir string, env []string) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, "cargo", "build", "--workspace", "--locked", "--offline")
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = 100 * time.Millisecond
	output := &tuneCargoOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	started := time.Now()
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("cargo build --workspace --locked --offline: %w\n%s", errors.Join(err, ctx.Err()), strings.TrimSpace(string(output.data)))
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return time.Since(started).Seconds(), nil
}

func tuneMedianDuration(values []float64) float64 {
	slices.Sort(values)
	middle := len(values) / 2
	if len(values)%2 == 0 {
		return (values[middle-1] + values[middle]) / 2
	}
	return values[middle]
}
