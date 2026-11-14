package base

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// mklinkJ creates a junction at link pointing at target, as Bun and npm do on Windows.
func mklinkJ(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}

// TestJunctionsAreLinks serves junctions as the directory symlinks they act as,
// in live roots and in captured source alike, instead of empty irregular files.
func TestJunctionsAreLinks(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	mklinkJ(t, filepath.Join(dir, "linked"), filepath.Join(dir, "src"))
	b, _ := createTestBase(t, repo, nil)
	mklinkJ(t, filepath.Join(dir, "node_modules", "agent"), filepath.Join(b.Meta().Dir, "src"))
	for p, want := range map[string]string{"linked": filepath.ToSlash(filepath.Join(dir, "src")), "node_modules/agent": "../src"} {
		info, err := b.Bottom(time.Time{}).Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("lstat %s = %v, %v; want a symlink", p, info, err)
		}
		if got, err := b.Bottom(time.Time{}).Readlink(p); err != nil || got != want {
			t.Errorf("readlink %s = %q, %v; want %q", p, got, err, want)
		}
	}
	entries, err := b.Bottom(time.Time{}).ReadDir("node_modules")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "agent" && e.Mode()&os.ModeSymlink == 0 {
			t.Errorf("listed junction mode = %v, want a symlink", e.Mode())
		}
	}
}
