package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTuneMetadataNativePair(t *testing.T) {
	roots := [2]string{t.TempDir(), t.TempDir()}
	metrics, err := measureTuneMetadata(t.Context(), roots, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 2 {
		t.Fatalf("got %d metadata workloads, want 2", len(metrics))
	}
	for _, metric := range metrics {
		if metric.Group != "metadata" || metric.Rate <= 0 || metric.Native <= 0 || metric.Ratio != metric.Rate/metric.Native {
			t.Fatalf("invalid paired metadata measurement: %+v", metric)
		}
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatalf("fixture cleanup: entries=%v err=%v", entries, err)
		}
	}
}

func TestTuneArtifactCopyAndValidation(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "copy")
	data, buffer := []byte("real Cargo-like artifact bytes\x00\xff"), make([]byte, 128)
	if err := tuneWriteFile(src, data); err != nil {
		t.Fatal(err)
	}
	if err := tuneCopyFile(src, dst, buffer, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if err := tuneCheckFile(dst, data, buffer); err != nil {
		t.Fatal(err)
	}
	if err := tuneCopyFile(src, filepath.Join(dir, "wrong-size"), buffer, int64(len(data)+1)); err == nil {
		t.Fatal("copy accepted a short source")
	}
	for _, bad := range [][]byte{data[:len(data)-1], append(bytes.Clone(data), 0), bytes.Repeat([]byte{'x'}, len(data))} {
		if err := os.WriteFile(dst, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := tuneCheckFile(dst, data, buffer); err == nil {
			t.Fatalf("validation accepted corrupted bytes %q", bad)
		}
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tuneWrite(f, data); err == nil {
		t.Fatal("read-only artifact write succeeded")
	}
	if err := tuneWrite(tuneShortWriter{}, data); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
}

type tuneShortWriter struct{}

func (tuneShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestTuneMetadataCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := measureTuneArtifacts(ctx, t.TempDir(), time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("artifact cancellation: %v", err)
	}
	if _, err := measureTuneFingerprint(ctx, t.TempDir(), time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprint cancellation: %v", err)
	}
	if _, err := measureTuneMetadata(ctx, [2]string{t.TempDir(), t.TempDir()}, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("paired cancellation: %v", err)
	}
}

func TestTuneCargoSourceBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.rs"), []byte("fn source() {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tuneCargoTouchPath(dir, "lib.rs"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../lib.rs", filepath.Join(dir, "lib.rs"), ".", "Cargo.toml"} {
		if _, err := tuneCargoTouchPath(dir, path); err == nil {
			t.Fatalf("accepted invalid source %q", path)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.rs")
	if err := os.WriteFile(outside, []byte("fn outside() {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.rs")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := tuneCargoTouchPath(dir, "escape.rs"); err == nil {
		t.Fatal("accepted source symlink outside disposable checkout")
	}
}

func TestTuneCargoPinnedEnvironment(t *testing.T) {
	dir := t.TempDir()
	pin := "1.91.0"
	t.Setenv("RUSTUP_TOOLCHAIN", "unrelated-toolchain")
	t.Setenv("RUSTC_WRAPPER", "cached-compiler")
	t.Setenv("CARGO_TARGET_DIR", "/outside/target")
	env := map[string]string{}
	for _, item := range tuneCargoEnv(dir, pin) {
		key, value, _ := strings.Cut(item, "=")
		env[key] = value
	}
	for key, want := range map[string]string{
		"RUSTUP_TOOLCHAIN": pin, "RUSTUP_AUTO_INSTALL": "0", "RUSTC_WRAPPER": "",
		"RUSTC_WORKSPACE_WRAPPER": "", "CARGO_INCREMENTAL": "1", "CARGO_TARGET_DIR": filepath.Join(dir, "target"),
	} {
		if got, ok := env[key]; !ok || got != want {
			t.Fatalf("%s=%q present=%v, want %q", key, got, ok, want)
		}
	}
	inherited := false
	for _, item := range tuneCargoEnv(dir, "") {
		if item == "RUSTUP_TOOLCHAIN=unrelated-toolchain" {
			inherited = true
		}
	}
	if !inherited {
		t.Fatal("unresolved toolchain did not preserve normal rustup override")
	}
}
