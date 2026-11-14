//go:build !windows

package export_test

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// TestSMBRetainedHandleFollowsRenameAcrossFork keeps a cached descriptor on
// its renamed identity, never on a replacement that later reuses its name.
func TestSMBRetainedHandleFollowsRenameAcrossFork(t *testing.T) {
	f := newStack(t, map[string]string{"dir/file": "before"})
	f.create(t, "parent")
	backend := f.export.SMB()
	h, err := backend.OpenFile("parent/dir/file", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := backend.Rename("parent/dir", "parent/moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Fork(t.Context(), api.ForkArgs{Checkout: "parent", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write([]byte("!")); err != nil {
		t.Fatalf("renamed handle after fork: %v", err)
	}
	read := func(path string) string {
		t.Helper()
		file, err := backend.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := read("parent/moved/file"); got != "before!" {
		t.Fatalf("renamed parent: %q", got)
	}
	if got := read("child/moved/file"); got != "before" {
		t.Fatalf("frozen child changed: %q", got)
	}
	if err := backend.Remove("parent/moved/file"); err != nil {
		t.Fatal(err)
	}
	replacement, err := backend.Create("parent/moved/file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Snap(t.Context(), api.SnapArgs{Checkout: "parent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write([]byte("wrong")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retired handle reopened replacement: %v", err)
	}
	if got := read("parent/moved/file"); got != "replacement" {
		t.Fatalf("retired handle corrupted replacement: %q", got)
	}
}

// TestSMBWriteCacheRejectsAliases prevents separate path leases from hiding
// writes through a second hard link of the same underlying file.
func TestSMBWriteCacheRejectsAliases(t *testing.T) {
	f := newStack(t, map[string]string{"file": "before"})
	f.create(t, "co")
	backend := f.export.SMB()
	policy := backend.(interface{ WriteCacheable(string) bool })
	h, err := backend.OpenFile("co/file", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !policy.WriteCacheable("co/file") {
		t.Fatal("isolated owned source file cannot use write caching")
	}
	if err := f.export.Nodes().Link("co/file", "co/alias"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"co/file", "co/alias"} {
		if policy.WriteCacheable(path) {
			t.Fatalf("write caching admitted hard-link alias %s", path)
		}
	}
}
