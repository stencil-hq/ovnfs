package export_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/overlay"
)

// routerFixture constructs the actual registry and captured base used by router tests.
func routerFixture(t *testing.T, names ...string) (*checkout.Registry, *export.Router) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	var exported *export.Export
	reg, err := checkout.Open(checkout.Config{
		State:     filepath.Join(dir, "state"),
		Mount:     filepath.Join(dir, "mount"),
		Memory:    overlay.NewMemory(64<<20, 1<<20, 0),
		BlobCache: 64 << 20,
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
	rt := export.NewRouter(reg)
	exported, err = export.New(rt, filepath.Join(dir, "state", "nodes.log"), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := reg.New(context.Background(), api.NewArgs{Dir: repo, Name: name}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	return reg, rt
}

// routerContents reads a router path without retaining a handle across registry operations.
func routerContents(t *testing.T, rt *export.Router, name string) string {
	t.Helper()
	f, err := rt.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRouterRootListsCheckoutAttributes checks the virtual root and real checkout attributes.
func TestRouterRootListsCheckoutAttributes(t *testing.T) {
	reg, rt := routerFixture(t, "zed", "alpha")
	fi, err := rt.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != os.ModeDir|0o555 || !fi.IsDir() || !fi.ModTime().Equal(reg.RootTime()) {
		t.Fatalf("unexpected virtual root: mode=%v mtime=%v", fi.Mode(), fi.ModTime())
	}
	entries, err := rt.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		var s checkout.Session
		if err := reg.Enter(entry.Name(), &s); err != nil {
			t.Fatal(err)
		}
		real, err := s.FS.Lstat("")
		s.Done()
		if err != nil {
			t.Fatal(err)
		}
		if entry.Mode() != real.Mode() || entry.Size() != real.Size() || !entry.ModTime().Equal(real.ModTime()) || !reflect.DeepEqual(entry.Sys(), real.Sys()) {
			t.Fatalf("checkout %s did not preserve root attributes", entry.Name())
		}
	}
	if !reflect.DeepEqual(names, []string{"alpha", "zed"}) {
		t.Fatalf("root names = %v", names)
	}
	before := fi.ModTime()
	if err := reg.Rm(context.Background(), api.RmArgs{Checkout: "zed"}); err != nil {
		t.Fatal(err)
	}
	after, err := rt.Lstat("")
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before) || !after.ModTime().Equal(reg.RootTime()) {
		t.Fatalf("root mtime did not advance on remove: %v -> %v", before, after.ModTime())
	}
	entries, err = rt.ReadDir("/")
	if err != nil || len(entries) != 1 || entries[0].Name() != "alpha" {
		t.Fatalf("root after remove = %v, %v", entries, err)
	}
}

// TestRouterRootMutationsDenied covers root paths and the checkout directories themselves.
func TestRouterRootMutationsDenied(t *testing.T) {
	_, rt := routerFixture(t, "alpha")
	for _, name := range []string{"", "/", ".", "alpha", "/alpha/", "new-checkout"} {
		t.Run(name, func(t *testing.T) {
			checks := []struct {
				name string
				call func() error
			}{
				{"create", func() error {
					f, err := rt.Create(name)
					if f != nil {
						f.Close()
					}
					return err
				}},
				{"open-write", func() error {
					f, err := rt.OpenFile(name, os.O_RDWR, 0)
					if f != nil {
						f.Close()
					}
					return err
				}},
				{"mkdir", func() error { return rt.MkdirAll(name, 0o755) }},
				{"remove", func() error { return rt.Remove(name) }},
				{"rename-from", func() error { return rt.Rename(name, "alpha/other") }},
				{"rename-to", func() error { return rt.Rename("alpha/source", name) }},
				{"symlink", func() error { return rt.Symlink("target", name) }},
				{"link-from", func() error { return rt.Link(name, "alpha/hardlink") }},
				{"link-to", func() error { return rt.Link("alpha/source", name) }},
				{"chmod", func() error { return rt.Chmod(name, 0o700) }},
				{"chown", func() error { return rt.Chown(name, -1, -1) }},
				{"lchown", func() error { return rt.Lchown(name, -1, -1) }},
				{"chtimes", func() error { return rt.Chtimes(name, time.Now(), time.Now()) }},
			}
			for _, check := range checks {
				if err := check.call(); !errors.Is(err, os.ErrPermission) {
					t.Errorf("%s(%q) = %v, want permission denied", check.name, name, err)
				}
			}
		})
	}
	if f, err := rt.TempFile("", "tmp"); !errors.Is(err, os.ErrPermission) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("root TempFile = %v", err)
	}
}

// TestRouterDispatchAndCrossCheckoutRename checks missing checkouts, isolation, and EXDEV shape.
func TestRouterDispatchAndCrossCheckoutRename(t *testing.T) {
	_, rt := routerFixture(t, "alpha", "beta")
	if _, err := rt.Stat("missing/file"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown checkout Stat = %v", err)
	}
	if _, err := rt.Open("missing/file"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown checkout Open = %v", err)
	}
	if err := rt.MkdirAll("missing/dir", 0o755); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown checkout MkdirAll = %v", err)
	}
	f, err := rt.Create("alpha/source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("alpha")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := routerContents(t, rt, "beta/source"); got != "base" {
		t.Fatalf("write leaked to beta: %q", got)
	}
	err = rt.Link("alpha/source", "beta/hardlink")
	var hardlinkError *os.LinkError
	if !errors.As(err, &hardlinkError) || !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("cross-checkout link = %#v", err)
	}
	if err := rt.Link("alpha/source", "alpha/hardlink"); err != nil {
		t.Fatal(err)
	}
	if got := routerContents(t, rt, "alpha/hardlink"); got != "alpha" {
		t.Fatalf("hardlink content = %q", got)
	}
	err = rt.Rename("alpha/source", "beta/moved")
	var link *os.LinkError
	if !errors.As(err, &link) || !errors.Is(err, syscall.EXDEV) || link.Old != "alpha/source" || link.New != "beta/moved" {
		t.Fatalf("cross-checkout rename = %#v", err)
	}
	if err := rt.Rename("alpha/source", "alpha/moved"); err != nil {
		t.Fatal(err)
	}
	if got := routerContents(t, rt, "alpha/moved"); got != "alpha" {
		t.Fatalf("renamed content = %q", got)
	}
	if err := rt.Symlink("moved", "alpha/link"); err != nil {
		t.Fatal(err)
	}
	if got, err := rt.Readlink("alpha/link"); err != nil || got != "moved" {
		t.Fatalf("readlink = %q, %v", got, err)
	}
	view, err := rt.Chroot("alpha")
	if err != nil {
		t.Fatal(err)
	}
	chrootFile, err := view.Open("moved")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(chrootFile)
	chrootFile.Close()
	if err != nil || string(data) != "alpha" {
		t.Fatalf("chroot content = %q, %v", data, err)
	}
	if rt.Root() != "/" || rt.Join("alpha", "dir", "..", "moved") != "alpha/moved" || rt.Capabilities() != billy.AllCapabilities {
		t.Fatal("router path or capability contract mismatch")
	}
}

// TestRouterWritableHandlesReopenAfterCheckpoint checks each file factory against a frozen child.
func TestRouterWritableHandlesReopenAfterCheckpoint(t *testing.T) {
	factories := []struct {
		name string
		open func(*export.Router) (billy.File, error)
	}{
		{"create", func(rt *export.Router) (billy.File, error) { return rt.Create("parent/held") }},
		{"exclusive", func(rt *export.Router) (billy.File, error) {
			return rt.OpenFile("parent/held", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
		}},
		{"append", func(rt *export.Router) (billy.File, error) {
			return rt.OpenFile("parent/held", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
		}},
		{"tempfile", func(rt *export.Router) (billy.File, error) { return rt.TempFile("parent", "held-") }},
	}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			reg, rt := routerFixture(t, "parent")
			f, err := factory.open(rt)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.Write([]byte("before")); err != nil {
				t.Fatal(err)
			}
			cp, err := reg.Snap(context.Background(), api.SnapArgs{Checkout: "parent", Label: "freeze"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reg.New(context.Background(), api.NewArgs{Snapshot: cp.Snapshot.ID, Name: "child"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("-after")); err != nil {
				t.Fatalf("write through pre-freeze handle: %v", err)
			}
			if got := routerContents(t, rt, f.Name()); got != "before-after" {
				t.Fatalf("parent after freeze = %q", got)
			}
			childPath := "child/" + strings.TrimPrefix(f.Name(), "parent/")
			if got := routerContents(t, rt, childPath); got != "before" {
				t.Fatalf("pre-freeze handle modified child's layer: %q", got)
			}
			if _, err := reg.Snap(context.Background(), api.SnapArgs{Checkout: "parent", Label: "again"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("!")); err != nil {
				t.Fatal(err)
			}
			if got := routerContents(t, rt, f.Name()); got != "before-after!" {
				t.Fatalf("second reopen lost offset or truncated content: %q", got)
			}
			if err := f.Truncate(4); err != nil {
				t.Fatal(err)
			}
			if got := routerContents(t, rt, f.Name()); got != "befo" {
				t.Fatalf("truncate = %q", got)
			}
			if got := routerContents(t, rt, childPath); got != "before" {
				t.Fatalf("truncate reached child: %q", got)
			}
		})
	}
}

// TestRouterPositionedWritesReopen checks SMB positioned I/O and flushes after a freeze.
func TestRouterPositionedWritesReopen(t *testing.T) {
	reg, rt := routerFixture(t, "alpha")
	f, err := rt.Create("alpha/positioned")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	cp, err := reg.Snap(context.Background(), api.SnapArgs{Checkout: "alpha", Label: "positioned"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.New(context.Background(), api.NewArgs{Snapshot: cp.Snapshot.ID, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	writer, ok := f.(io.WriterAt)
	if !ok {
		t.Fatal("router file lacks positioned writes")
	}
	if n, err := writer.WriteAt([]byte("X"), 4); err != nil || n != 1 {
		t.Fatalf("positioned write = %d, %v", n, err)
	}
	if _, err := f.Write([]byte("Y")); err != nil {
		t.Fatal(err)
	}
	syncer, ok := f.(interface{ Sync() error })
	if !ok {
		t.Fatal("router file lacks flush")
	}
	if err := syncer.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := routerContents(t, rt, "alpha/positioned"); got != "abYdXf" {
		t.Fatalf("positioned write changed sequential offset: %q", got)
	}
	if got := routerContents(t, rt, "child/positioned"); got != "abcdef" {
		t.Fatalf("positioned write reached frozen child: %q", got)
	}
}

// TestRouterRemovedHandlesStayClosed checks name reuse cannot revive a retained SMB file.
func TestRouterRemovedHandlesStayClosed(t *testing.T) {
	reg, rt := routerFixture(t, "alpha")
	f, err := rt.OpenFile("alpha/source", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cp, err := reg.Snap(context.Background(), api.SnapArgs{Checkout: "alpha", Label: "keep base"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Rm(context.Background(), api.RmArgs{Checkout: "alpha", StickySnapshots: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.New(context.Background(), api.NewArgs{Snapshot: cp.Snapshot.ID, Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("not allowed")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("removed handle revived after name reuse: %v", err)
	}
	if got := routerContents(t, rt, "alpha/source"); got != "base" {
		t.Fatalf("retained handle mutated replacement checkout: %q", got)
	}
}

// TestRouterReadHandlePreservesOffset checks ReadAt does not perturb a later generation's seek.
func TestRouterReadHandlePreservesOffset(t *testing.T) {
	reg, rt := routerFixture(t, "alpha")
	f, err := rt.Open("/alpha/./source")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := f.Name(); got != "alpha/source" {
		t.Fatalf("canonical handle name = %q", got)
	}
	prefix := make([]byte, 2)
	if _, err := io.ReadFull(f, prefix); err != nil || string(prefix) != "ba" {
		t.Fatalf("initial read = %q, %v", prefix, err)
	}
	if _, err := reg.Snap(context.Background(), api.SnapArgs{Checkout: "alpha", Label: "freeze"}); err != nil {
		t.Fatal(err)
	}
	replacement, err := rt.Create("alpha/source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Write([]byte("more")); err != nil {
		replacement.Close()
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := f.ReadAt(prefix, 0); err != nil || n != 2 || string(prefix) != "mo" {
		t.Fatalf("positioned read = %q, %v", prefix, err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "re" {
		t.Fatalf("read after freeze lost offset = %q, %v", data, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(f)
	if err != nil || string(data) != "more" {
		t.Fatalf("seek after freeze = %q, %v", data, err)
	}
	if err := reg.Rm(context.Background(), api.RmArgs{Checkout: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(prefix); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("removed checkout handle read = %v", err)
	}
}
