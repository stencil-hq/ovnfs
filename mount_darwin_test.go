//go:build darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMountNFS puts a mount_nfs on PATH that exits with status first on its
// first call and with status rest on every later call.
func fakeMountNFS(t *testing.T, first, rest string) {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\nif [ -e '" + calls + "' ]; then exit " + rest + "; fi\n: > '" + calls + "'\necho 'mount_nfs: Resource busy' >&2\nexit " + first + "\n"
	if err := os.WriteFile(filepath.Join(bin, "mount_nfs"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// busySiblings lists the directories attach moved aside next to mount.
func busySiblings(t *testing.T, mount string) []string {
	t.Helper()
	moved, err := filepath.Glob(mount + ".busy-*")
	if err != nil {
		t.Fatal(err)
	}
	return moved
}

var testNFSAddr = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2049}

// TestAttachMovesBusyEmptyMountPoint recovers from a hung forced unmount that
// still holds an empty, unmounted mount point.
func TestAttachMovesBusyEmptyMountPoint(t *testing.T) {
	fakeMountNFS(t, "16", "0")
	mount := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := attach("nfs", testNFSAddr, mount, "", "", mountSettings{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if moved := busySiblings(t, mount); len(moved) != 1 {
		t.Fatalf("moved mount points: %v, want one", moved)
	}
	entries, err := os.ReadDir(mount)
	if err != nil || len(entries) != 0 {
		t.Fatalf("new mount point: %v entries, err %v", len(entries), err)
	}
}

// TestAttachKeepsBusyMountPoint moves no directory with entries and no
// directory after a failure other than a busy mount point.
func TestAttachKeepsBusyMountPoint(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		file         bool
	}{
		{"busy with entries", "16", true},
		{"other failure", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeMountNFS(t, tc.status, tc.status)
			mount := filepath.Join(t.TempDir(), "mnt")
			if err := os.Mkdir(mount, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.file {
				if err := os.WriteFile(filepath.Join(mount, "a.txt"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := attach("nfs", testNFSAddr, mount, "", "", mountSettings{})
			if err == nil || !strings.Contains(err.Error(), "exit status "+tc.status) {
				t.Fatalf("attach: %v, want the mount error", err)
			}
			if moved := busySiblings(t, mount); len(moved) != 0 {
				t.Fatalf("moved mount point: %v", moved)
			}
			if _, err := os.Stat(mount); err != nil {
				t.Fatal(err)
			}
		})
	}
}
