//go:build !windows

package remotefs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/remotefs"
)

// exportFixture opens a real registry and export on a temp state dir, creates
// one checkout from a seeded folder, and returns the export, the checkout name and the folder.
func exportFixture(t *testing.T) (*export.Export, string, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "pkg", "lib.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	var exported *export.Export
	reg, err := checkout.Open(checkout.Config{
		State: state,
		Mount: filepath.Join(dir, "mount"),
		Retire: func(name string) {
			if exported != nil {
				exported.Retire(name)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reg.Close(); err != nil {
			t.Errorf("close registry: %v", err)
		}
		if exported != nil {
			if err := exported.Close(); err != nil {
				t.Errorf("close export: %v", err)
			}
		}
	})
	exported, err = export.New(export.NewRouter(reg), filepath.Join(state, "nodes.log"), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := reg.New(context.Background(), api.NewArgs{Dir: src, Name: "vm"})
	if err != nil {
		t.Fatalf("new checkout: %v", err)
	}
	return exported, reply.Checkout.Name, src
}

// serveExport serves one checkout of e on a fresh socket and returns a connected client.
func serveExport(t *testing.T, share remotefs.Share, opts remotefs.Options) *remotefs.Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("remotefs share sockets are unsupported on Windows")
	}
	dir, err := os.MkdirTemp("/tmp", "rfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := remotefs.Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- remotefs.Serve(l, share, opts) }()
	c, err := remotefs.Dial(sock, "home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		l.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return c
}

// TestExportCheckoutOverRemoteFS drives a real checkout through the VMM's op set and
// checks copy-on-write isolation and node ids that follow renames.
func TestExportCheckoutOverRemoteFS(t *testing.T) {
	e, name, src := exportFixture(t)
	nodes := e.Nodes()
	c := serveExport(t, remotefs.Share{FS: nodes, Checkout: name, Writable: true}, remotefs.Options{})

	if st, err := c.Stat("pkg/lib.go"); err != nil || st.Kind != remotefs.KindFile || st.Size != 12 {
		t.Fatalf("stat seeded file = %+v, %v", st, err)
	}
	if _, err := c.Mkdir("out", 0o755, 0, 0); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := c.Create("out/a.txt", 0o644, 0, 0, true, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	if n, err := c.Write("out/a.txt", 0, []byte("from the vm")); err != nil || n != 11 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if err := c.Sync("out/a.txt", true); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n, err := c.Write("pkg/lib.go", 12, []byte("// edited\n")); err != nil || n != 10 {
		t.Fatalf("write seeded file = %d, %v", n, err)
	}
	if got, err := c.Read("pkg/lib.go", 0, 64); err != nil || string(got) != "package pkg\n// edited\n" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(src, "pkg", "lib.go")); err != nil || string(got) != "package pkg\n" {
		t.Fatalf("source folder changed under the checkout: %q, %v", got, err)
	}

	size := uint64(4)
	if st, err := c.Setattr("out/a.txt", remotefs.SetAttr{Size: &size}); err != nil || st.Size != 4 {
		t.Fatalf("setattr = %+v, %v", st, err)
	}
	entries, err := c.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "out" || entries[1].Name != "pkg" {
		t.Fatalf("list root = %+v", entries)
	}

	if err := nodes.Symlink("../pkg", name+"/out/link"); err != nil {
		t.Fatal(err)
	}
	if st, err := c.Stat("out/link/lib.go"); err != nil || st.Kind != remotefs.KindFile {
		t.Fatalf("stat through symlink = %+v, %v", st, err)
	}
	if err := c.Unlink("out/link"); err != nil {
		t.Fatalf("unlink symlink: %v", err)
	}
	if _, err := c.Stat("pkg/lib.go"); err != nil {
		t.Fatalf("unlinking a symlink removed its target: %v", err)
	}

	id := nodes.ID(name + "/out/a.txt")
	if err := c.Rename("out/a.txt", "out/b.txt", true); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := nodes.ID(name + "/out/b.txt"); got != id {
		t.Fatalf("node id after rename = %d, want %d", got, id)
	}
	if err := c.Unlink("out/b.txt"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if err := c.Rmdir("out"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if _, err := c.Stat("out"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat removed dir = %v, want not_found", err)
	}
}

// TestExportCacheBarrier exposes pending external writes and rejects mutation
// when their owner cannot flush, rather than reading stale or overwriting data.
func TestExportCacheBarrier(t *testing.T) {
	e, name, _ := exportFixture(t)
	nodes := e.Nodes()
	var failed atomic.Bool
	var flush sync.Once
	var flushErr error
	quiesce := func(context.Context) (func(), error) {
		if failed.Load() {
			return nil, errors.New("write cache owner disconnected")
		}
		flush.Do(func() {
			f, err := nodes.OpenFile(name+"/pkg/lib.go", os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				flushErr = err
				return
			}
			_, flushErr = f.Write([]byte("pending client write"))
			flushErr = errors.Join(flushErr, f.Close())
		})
		return func() {}, flushErr
	}
	c := serveExport(t, remotefs.Share{FS: nodes, Checkout: name, Writable: true}, remotefs.Options{Quiesce: quiesce})
	if got, err := c.Read("pkg/lib.go", 0, 64); err != nil || string(got) != "pending client write" {
		t.Fatalf("read before external flush: %q, %v", got, err)
	}
	failed.Store(true)
	if _, err := c.Write("pkg/lib.go", 0, []byte("overwritten")); err == nil {
		t.Fatal("write succeeded despite failed cache barrier")
	}
	failed.Store(false)
	if got, err := c.Read("pkg/lib.go", 0, 64); err != nil || string(got) != "pending client write" {
		t.Fatalf("failed barrier changed data: %q, %v", got, err)
	}
}

// TestExportCheckoutRootIsProtected checks the checkout root itself cannot be removed or renamed.
func TestExportCheckoutRootIsProtected(t *testing.T) {
	e, name, _ := exportFixture(t)
	c := serveExport(t, remotefs.Share{FS: e.Nodes(), Checkout: name, Writable: true}, remotefs.Options{})
	if err := c.Rmdir(""); err == nil {
		t.Fatal("rmdir of the checkout root succeeded")
	}
	if err := c.Rename("", "x", false); err == nil {
		t.Fatal("rename of the checkout root succeeded")
	}
	if st, err := c.Stat(""); err != nil || st.Kind != remotefs.KindDir {
		t.Fatalf("stat checkout root = %+v, %v", st, err)
	}
}
