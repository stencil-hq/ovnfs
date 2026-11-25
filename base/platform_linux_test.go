//go:build linux

package base

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestVolumeIDSameFilesystem(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Mkdir(a, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b, 0o700); err != nil {
		t.Fatal(err)
	}
	ia, err := volumeID(a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := volumeID(b)
	if err != nil {
		t.Fatal(err)
	}
	if ia != ib {
		t.Fatalf("volumeID(%q)=%d, volumeID(%q)=%d", a, ia, b, ib)
	}
}

func TestVolumeIDUsesStatfsFsid(t *testing.T) {
	dir := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	want := packFsid(st.Fsid)
	if want == 0 {
		t.Skip("statfs f_fsid is zero on this filesystem")
	}
	got, err := volumeID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("volumeID = %d, want f_fsid %d", got, want)
	}
}

func TestPackFsid(t *testing.T) {
	id := unix.Fsid{Val: [2]int32{-1, 2}}
	got := packFsid(id)
	want := uint64(0xffffffff) | uint64(2)<<32
	if got != want {
		t.Fatalf("fsid(%v) = %#x, want %#x", id.Val, got, want)
	}
	if packFsid(unix.Fsid{}) != 0 {
		t.Fatal("zero fsid should pack to 0")
	}
}

func TestOnVolumeMatchesVolumeID(t *testing.T) {
	dir := t.TempDir()
	id, err := volumeID(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !onVolume(dir, info, id) {
		t.Fatalf("onVolume(%q) = false for the directory's own volumeID", dir)
	}
	if onVolume(dir, info, id^1) {
		t.Fatalf("onVolume(%q) = true for a different volume id", dir)
	}
}
