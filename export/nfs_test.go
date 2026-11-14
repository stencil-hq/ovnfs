//go:build !windows

package export

import (
	"testing"
)

// TestResetNFSHandles keeps the root identity while rotating every descendant.
func TestResetNFSHandles(t *testing.T) {
	e := &Export{fs: newMetadataTestFS(t)}
	b := e.NFS()
	if err := b.MkdirAll("co/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := b.Create("co/dir/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	root := b.FileID("")
	old := []uint64{b.FileID("co"), b.FileID("co/dir"), b.FileID("co/dir/file")}
	if err := e.ResetNFSHandles(); err != nil {
		t.Fatal(err)
	}
	if id := b.FileID(""); id != root {
		t.Fatalf("root id changed from %d to %d", root, id)
	}
	for _, id := range old {
		if _, ok := b.Path(id); ok {
			t.Errorf("old id %d still resolves", id)
		}
	}
	fresh := []uint64{b.FileID("co"), b.FileID("co/dir"), b.FileID("co/dir/file")}
	for i, id := range fresh {
		if id == old[i] {
			t.Errorf("path %d reused id %d", i, id)
		}
	}
}

// TestNFSIDsTrackRenameAndRetirement checks that the ids NFS handles carry
// follow renames and go stale on removal and checkout retirement.
func TestNFSIDsTrackRenameAndRetirement(t *testing.T) {
	e := &Export{fs: newMetadataTestFS(t)}
	b := e.NFS()
	if err := b.MkdirAll("co/a", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := b.Create("co/a/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	id := b.FileID("co/a/file")
	if err := b.Rename("co/a", "co/b"); err != nil {
		t.Fatal(err)
	}
	if p, ok := b.Path(id); !ok || p != "co/b/file" {
		t.Fatalf("renamed id resolves to %q, live %v", p, ok)
	}
	if err := b.Chmod("co/b/file", 0o600); err != nil {
		t.Fatal(err)
	}
	if !b.RetireHandle(id) {
		t.Fatal("live handle was not retired")
	}
	if _, ok := b.Path(id); ok {
		t.Fatal("retired handle still resolves")
	}
	next := b.FileID("co/b/file")
	if next == id {
		t.Fatal("retired path reused its old id")
	}
	if p, ok := b.Path(next); !ok || p != "co/b/file" {
		t.Fatalf("fresh id resolves to %q, live %v", p, ok)
	}
	id = next
	if err := b.Remove("co/b/file"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Path(id); ok {
		t.Fatal("removed id still resolves")
	}
	dir := b.FileID("co/b")
	e.Retire("co")
	if _, ok := b.Path(dir); ok {
		t.Fatal("retired directory id still resolves")
	}
	if root, ok := b.Path(b.FileID("")); !ok || root != "" {
		t.Fatalf("root resolves to %q, live %v", root, ok)
	}
}
