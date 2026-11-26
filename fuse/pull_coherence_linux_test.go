package fuse

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/overlay"
)

// newFolderFixture mounts checkout alpha of a folder holding files, wired as
// the daemon wires it (serve.go): daemon-side changes reach the export's
// Invalidate, and the mount's, and it returns the folder.
func newFolderFixture(t *testing.T, files map[string]string) (*fixture, string) {
	t.Helper()
	requireFUSE(t)
	lower := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(lower, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{state: t.TempDir(), mnt: t.TempDir()}
	var err error
	f.reg, err = checkout.Open(checkout.Config{State: f.state, Mount: t.TempDir(), Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20,
		Invalidate: func(name string, paths, trees []string) {
			f.export.Invalidate(name, paths, trees)
			if f.srv != nil {
				f.srv.Invalidate(name, paths, trees)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.reg.Close(); err != nil {
			t.Error(err)
		}
	})
	f.export = export.New(export.NewRouter(f.reg), f.state)
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: lower, Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	f.mount(t)
	return f, lower
}

// TestPullShowsThroughTheMountAtOnce checks that what a pull writes into a
// checkout reads back through the mount as soon as the pull returns, not
// once the kernel's caches expire (TTL): a caller runs git right after.
func TestPullShowsThroughTheMountAtOnce(t *testing.T) {
	const marked = "<<<<<<< checkout alpha\ncheckout side\n=======\nfolder side\n>>>>>>> folder\n"
	for _, c := range []struct {
		name  string
		probe func(p string) // through the mount, between the write and the pull
	}{
		{"cold", func(string) {}},
		{"stat before pull", func(p string) { os.Stat(p) }},
		{"read before pull", func(p string) { os.ReadFile(p) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, folder := newFolderFixture(t, map[string]string{"x.txt": "base\n"})
			x := f.path("alpha/x.txt")
			if err := os.WriteFile(x, []byte("checkout side\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			c.probe(x)
			if err := os.WriteFile(filepath.Join(folder, "x.txt"), []byte("folder side\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			reply, err := f.reg.PullFolder(context.Background(), api.PullArgs{Checkout: "alpha"})
			if err != nil {
				t.Fatal(err)
			}
			if len(reply.Conflicts) != 1 || !reply.Conflicts[0].Markers {
				t.Fatalf("pull = %+v, want x.txt marked", reply)
			}
			info, err := os.Stat(x)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(x)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != marked || info.Size() != int64(len(marked)) {
				t.Errorf("right after the pull: stat size %d, read %d bytes %q; want %d bytes", info.Size(), len(got), got, len(marked))
			}
		})
	}
}

// TestPullNamesShowThroughTheMountAtOnce checks that a file a pull removes is
// gone through the mount at once, though the kernel had its name cached, and
// that one it adds is there.
func TestPullNamesShowThroughTheMountAtOnce(t *testing.T) {
	f, folder := newFolderFixture(t, map[string]string{"gone.txt": "gone\n"})
	for _, p := range []string{"alpha/gone.txt", "alpha/added.txt"} {
		os.Stat(f.path(p))
	}
	if err := os.Remove(filepath.Join(folder, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "added.txt"), []byte("added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.PullFolder(context.Background(), api.PullArgs{Checkout: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("alpha/gone.txt")); !os.IsNotExist(err) {
		t.Errorf("stat of the removed file = %v, want ENOENT", err)
	}
	f.read(t, "alpha/added.txt", "added\n")
}
