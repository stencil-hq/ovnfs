package clone

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAPFSDirectoryTimes(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	child := filepath.Join(src, "empty")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	// Use a later atime so listing the directories need not advance it.
	atime := time.Date(2100, 1, 1, 0, 0, 0, 123456789, time.UTC)
	mtime := time.Date(2001, 2, 3, 4, 5, 6, 987654321, time.UTC)
	for _, name := range []string{src, child} {
		if err := os.Chtimes(name, atime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err != nil {
		if unsupported(err) {
			t.Skipf("directory cloning is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	stat := func(name string) *syscall.Stat_t {
		t.Helper()
		info, err := os.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		return info.Sys().(*syscall.Stat_t)
	}
	empty := filepath.Join(dst, "empty")
	before := stat(empty)
	if err := dirTimes(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dst, empty} {
		got := stat(name)
		if got.Mtimespec.Nano() != mtime.UnixNano() || got.Atimespec.Nano() != atime.UnixNano() {
			t.Errorf("%s: got mtime %v, atime %v; want %v, %v", name,
				time.Unix(0, got.Mtimespec.Nano()), time.Unix(0, got.Atimespec.Nano()), mtime, atime)
		}
		if name == empty && got.Mtimespec == before.Mtimespec && got.Ctimespec != before.Ctimespec {
			t.Error("rewrote the empty directory's metadata even though its mtime already matched")
		}
	}
}
