//go:build windows

package winfsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/overlay"
	"golang.org/x/sys/windows"
)

// fixture is a real export over temporary registry state, mounted on a free drive.
type fixture struct {
	reg    *checkout.Registry
	export *export.Export
	state  string
	drive  string
	srv    *Server
}

// freeDrive returns the last drive letter no volume holds.
func freeDrive(t *testing.T) string {
	t.Helper()
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	for c := 'Z'; c >= 'D'; c-- {
		if mask&(1<<(c-'A')) == 0 {
			return string(c) + `:\`
		}
	}
	t.Skip("no free drive letter")
	return ""
}

// newFixture opens checkouts alpha and beta over one lower folder and mounts the export.
func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	if !Installed() {
		t.Skip("WinFsp is not installed")
	}
	lower := t.TempDir()
	for name, data := range files {
		p := filepath.Join(lower, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{state: t.TempDir(), drive: freeDrive(t)}
	var err error
	f.reg, err = checkout.Open(checkout.Config{State: f.state, Mount: t.TempDir(), Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20, Retire: func(name string) {
		if f.export != nil {
			f.export.Retire(name)
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
	f.open(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: lower, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	f.mount(t)
	return f
}

// open (re)opens the export over the fixture's persistent node log.
func (f *fixture) open(t *testing.T) {
	t.Helper()
	e, err := export.New(export.NewRouter(f.reg), filepath.Join(f.state, "nodes.log"), f.state, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.export = e
	t.Cleanup(func() { e.Close() })
}

// mount serves the export on the fixture's drive until the test ends.
func (f *fixture) mount(t *testing.T) {
	t.Helper()
	srv, err := Mount(f.export, f.drive)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	t.Cleanup(func() { f.unmount(t) })
}

// unmount removes the drive once.
func (f *fixture) unmount(t *testing.T) {
	t.Helper()
	if f.srv == nil {
		return
	}
	if err := f.srv.Unmount(false); err != nil {
		t.Error(err)
	}
	f.srv.Wait()
	f.srv = nil
}

// path returns a path on the drive.
func (f *fixture) path(p string) string { return filepath.Join(f.drive, filepath.FromSlash(p)) }

// fileID returns a name's file index through the drive, without following a final symlink.
func (f *fixture) fileID(t *testing.T, p string) uint64 {
	t.Helper()
	name, err := windows.UTF16PtrFromString(f.path(p))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer windows.CloseHandle(h)
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &d); err != nil {
		t.Fatal(err)
	}
	return uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow)
}

// read asserts a file's contents through the drive.
func (f *fixture) read(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(f.path(p))
	if err != nil || string(got) != want {
		t.Fatalf("read %s = %q, %v; want %q", p, got, err, want)
	}
}

// names lists a directory through the drive.
func (f *fixture) names(t *testing.T, p string) []string {
	t.Helper()
	entries, err := os.ReadDir(f.path(p))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// wantErr asserts that err matches target.
func wantErr(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: got %v, want %v", what, err, target)
	}
}

// TestRootListsCheckoutsReadOnly checks the root's listing and refusals.
func TestRootListsCheckoutsReadOnly(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower"})
	if got := f.names(t, ""); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("root lists %v, want [alpha beta]", got)
	}
	if id := f.fileID(t, ""); id != 1 {
		t.Fatalf("root file id %d, want 1", id)
	}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"mkdir", func() error { return os.Mkdir(f.path("gamma"), 0o755) }},
		{"create", func() error { return os.WriteFile(f.path("file"), nil, 0o644) }},
		{"rmdir checkout", func() error { return os.Remove(f.path("alpha")) }},
		{"rename checkout", func() error { return os.Rename(f.path("alpha"), f.path("gamma")) }},
		{"move into root", func() error { return os.Rename(f.path("alpha/file"), f.path("file")) }},
	} {
		wantErr(t, c.name, c.call(), fs.ErrPermission)
	}
	f.read(t, "alpha/file", "lower")
}

// TestCheckoutReadWrite exercises file and directory operations inside one checkout.
func TestCheckoutReadWrite(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower", "dir/full": "x", "Case": "upper-case name"})
	f.read(t, "alpha/file", "lower")

	// Copy-up isolates the write to alpha.
	if err := os.WriteFile(f.path("alpha/file"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/file", "upper")
	f.read(t, "beta/file", "lower")

	// Names are case-sensitive.
	f.read(t, "alpha/Case", "upper-case name")
	if _, err := os.Stat(f.path("alpha/case")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat other case: %v, want not found", err)
	}

	// Create, positioned write, flush, append-only handles, and truncate.
	fh, err := os.OpenFile(f.path("alpha/new"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte("hello world"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte("W"), 6); err != nil {
		t.Fatal(err)
	}
	if err := fh.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/new", "hello World")
	_, err = os.OpenFile(f.path("alpha/new"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	wantErr(t, "exclusive create", err, fs.ErrExist)
	ap, err := os.OpenFile(f.path("alpha/new"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	ap.Close()
	f.read(t, "alpha/new", "hello World!")
	if err := os.Truncate(f.path("alpha/new"), 5); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/new", "hello")

	// Times, and FILE_ATTRIBUTE_READONLY as the owner write bit.
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(f.path("alpha/new"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(f.path("alpha/new")); err != nil || !info.ModTime().Equal(mtime) || info.Size() != 5 {
		t.Fatalf("after chtimes: %v, %v", info, err)
	}
	if err := os.Chmod(f.path("alpha/new"), 0o444); err != nil {
		t.Fatal(err)
	}
	if info, err := f.export.Nodes().Lstat("alpha/new"); err != nil || info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("read-only attribute left mode %v, %v", info.Mode(), err)
	}
	wantErr(t, "write read-only", os.WriteFile(f.path("alpha/new"), nil, 0o644), fs.ErrPermission)
	if err := os.Chmod(f.path("alpha/new"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path("alpha/new"), []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Directories, renames, and their errors.
	if err := os.Mkdir(f.path("alpha/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "mkdir existing", os.Mkdir(f.path("alpha/sub"), 0o755), fs.ErrExist)
	if err := os.WriteFile(f.path("alpha/sub/inner"), []byte("in"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path("alpha/sub"), f.path("alpha/moved")); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/moved/inner", "in")
	if err := os.Rename(f.path("alpha/new"), f.path("alpha/moved/inner")); err != nil {
		t.Fatalf("rename over a file: %v", err)
	}
	f.read(t, "alpha/moved/inner", "again")
	wantErr(t, "rename across checkouts", os.Rename(f.path("alpha/file"), f.path("beta/moved")), windows.ERROR_NOT_SAME_DEVICE)
	f.read(t, "alpha/file", "upper")
	if _, err := os.Stat(f.path("beta/moved")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-checkout rename leaked into beta: %v", err)
	}
	wantErr(t, "rmdir non-empty", windows.RemoveDirectory(windows.StringToUTF16Ptr(f.path("alpha/moved"))), windows.ERROR_DIR_NOT_EMPTY)
	if err := os.RemoveAll(f.path("alpha/moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.path("alpha/dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("alpha/dir/full")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat below removed directory: %v, want not found", err)
	}
	f.read(t, "beta/dir/full", "x")
	if got := f.names(t, "alpha"); !slices.Equal(got, []string{"Case", "file"}) {
		t.Fatalf("alpha lists %v", got)
	}

	var free, total, avail uint64
	if err := windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(f.drive), &avail, &total, &free); err != nil || total == 0 {
		t.Fatalf("disk space = %d of %d, %v", avail, total, err)
	}
}

// TestSymlinks creates, reads, and traverses file and directory symlinks.
func TestSymlinks(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower", "dir/in": "deep"})
	if err := os.Symlink("file", f.path("alpha/link")); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("creating symlinks needs Developer Mode or SeCreateSymbolicLinkPrivilege")
		}
		t.Fatal(err)
	}
	if target, err := os.Readlink(f.path("alpha/link")); err != nil || target != "file" {
		t.Fatalf("readlink = %q, %v", target, err)
	}
	if target, err := f.export.Nodes().Readlink("alpha/link"); err != nil || target != "file" {
		t.Fatalf("export readlink = %q, %v", target, err)
	}
	f.read(t, "alpha/link", "lower")
	if err := os.Symlink("dir", f.path("alpha/dirlink")); err != nil {
		t.Fatal(err)
	}
	if target, err := f.export.Nodes().Readlink("alpha/dirlink"); err != nil || target != "dir" {
		t.Fatalf("export readlink = %q, %v", target, err)
	}
	f.read(t, "alpha/dirlink/in", "deep")
	// Targets naming the checkout through the drive or the volume are stored
	// relative, so they resolve inside the checkout.
	for name, target := range map[string]string{"alpha/drivelink": f.path("alpha/dir"), "alpha/sharelink": `\\ovnfs\` + f.drive[:1] + `\alpha\dir`} {
		if err := os.Symlink(target, f.path(name)); err != nil {
			t.Fatal(err)
		}
		if got, err := f.export.Nodes().Readlink(name); err != nil || got != "dir" {
			t.Fatalf("%s stored as %q, %v; want dir", name, got, err)
		}
		f.read(t, name+"/in", "deep")
	}
	if err := os.Remove(f.path("alpha/link")); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/file", "lower")
}

// TestFinalPathsReopen checks that a handle's final path, which rustc and
// other canonicalizing tools reopen, names the file on the \\ovnfs\ volume.
func TestFinalPathsReopen(t *testing.T) {
	f := newFixture(t, map[string]string{"dir/file": "lower"})
	h, err := os.Open(f.path("alpha/dir/file"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(h.Fd()), &buf[0], uint32(len(buf)), 0)
	if err != nil {
		t.Fatal(err)
	}
	final := windows.UTF16ToString(buf[:n])
	if want := `\\?\UNC\ovnfs\` + f.drive[:1] + `\alpha\dir\file`; final != want {
		t.Fatalf("final path %q, want %q", final, want)
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != "lower" {
		t.Fatalf("reopen %s = %q, %v", final, got, err)
	}
}

// TestFileIDsAreStableNodeIDs checks that file indexes are the export's node
// ids and survive copy-up, renames, and a remount over the same node log.
func TestFileIDsAreStableNodeIDs(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower"})
	nodes := f.export.Nodes()
	id := f.fileID(t, "alpha/file")
	if want := nodes.ID("alpha/file"); id != want {
		t.Fatalf("file id %d, node id %d", id, want)
	}
	if id == f.fileID(t, "beta/file") {
		t.Fatal("checkouts share a file id")
	}
	if err := os.WriteFile(f.path("alpha/file"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.fileID(t, "alpha/file"); got != id {
		t.Fatalf("copy-up changed file id %d -> %d", id, got)
	}
	if err := os.Rename(f.path("alpha/file"), f.path("alpha/renamed")); err != nil {
		t.Fatal(err)
	}
	if got := f.fileID(t, "alpha/renamed"); got != id {
		t.Fatalf("rename changed file id %d -> %d", id, got)
	}
	f.unmount(t)
	if err := f.export.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	f.mount(t)
	if got := f.fileID(t, "alpha/renamed"); got != id {
		t.Fatalf("remount changed file id %d -> %d", id, got)
	}
	f.read(t, "alpha/renamed", "upper")
}

// TestChangesUnderneathShowWithinTTL checks that edits and checkout removals
// made outside the drive reach it once the driver's short caches expire.
func TestChangesUnderneathShowWithinTTL(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower"})
	f.read(t, "alpha/file", "lower")
	held, err := os.Open(f.path("beta/file"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	w, err := f.export.Nodes().OpenFile("alpha/file", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("changed underneath")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "beta"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(TTL + 100*time.Millisecond)
	f.read(t, "alpha/file", "changed underneath")
	if got := f.names(t, ""); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("root lists %v after removing beta", got)
	}
	if _, err := os.Stat(f.path("beta")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed checkout: %v, want not found", err)
	}
	if _, err := held.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatal("read in a removed checkout succeeded")
	}
}

// TestOpenFilesFollowTheirNode checks Unix-style deletion of open files, that
// readers see a later writer's copy-up, and listings longer than one reply.
func TestOpenFilesFollowTheirNode(t *testing.T) {
	f := newFixture(t, map[string]string{"shared": "lower"})
	// Deleting an open file needs FILE_SHARE_DELETE, which os.Create omits.
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(f.path("alpha/tmp")), windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.CREATE_NEW, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh := os.NewFile(uintptr(h), "tmp")
	defer fh.Close()
	if _, err := fh.Write([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path("alpha/tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path("alpha/tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after delete: %v, want not found", err)
	}
	if err := os.WriteFile(f.path("alpha/tmp"), []byte("new"), 0o644); err != nil {
		t.Fatalf("recreate a deleted name still open: %v", err)
	}
	buf := make([]byte, 10)
	if _, err := fh.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, []byte("still here")) {
		t.Fatalf("read after delete = %q, %v", buf, err)
	}
	if info, err := fh.Stat(); err != nil || info.Size() != 10 {
		t.Fatalf("stat handle after delete: %v, %v", info, err)
	}
	f.read(t, "alpha/tmp", "new")

	r, err := os.Open(f.path("alpha/shared"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.WriteFile(f.path("alpha/shared"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadAt(buf[:5], 0); err != nil || string(buf[:n]) != "upper" {
		t.Fatalf("reader opened before copy-up reads %q, %v", buf[:n], err)
	}

	if err := os.Mkdir(f.path("alpha/many"), 0o755); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i := range 600 {
		name := fmt.Sprintf("entry-with-a-longish-name-%04d", i)
		want = append(want, name)
		if err := os.WriteFile(f.path("alpha/many/"+name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.names(t, "alpha/many"); !slices.Equal(got, want) {
		t.Fatalf("listed %d entries, want %d", len(got), len(want))
	}
}
