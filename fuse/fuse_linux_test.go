package fuse

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/overlay"
	"golang.org/x/sys/unix"
)

// fixture is a real export over temporary registry state, mounted over FUSE.
type fixture struct {
	reg    *checkout.Registry
	export *export.Export
	state  string
	mnt    string
	srv    *Server
}

// requireFUSE skips unless this host can make FUSE mounts.
func requireFUSE(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("/dev/fuse unavailable: %v", err)
	}
	f.Close()
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, err := exec.LookPath("fusermount"); err != nil {
			t.Skip("fusermount3/fusermount not installed")
		}
	}
}

// newFixture opens checkouts alpha and beta over one lower folder and mounts the export.
func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	requireFUSE(t)
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
	f := &fixture{state: t.TempDir(), mnt: t.TempDir()}
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

// mount serves the export at the fixture's mountpoint until the test ends.
func (f *fixture) mount(t *testing.T) {
	t.Helper()
	srv, err := Mount(f.export, f.mnt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	t.Cleanup(func() { f.unmount(t) })
}

// unmount detaches the mount once and waits for the server to stop.
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

// path returns a path under the mountpoint.
func (f *fixture) path(p string) string { return filepath.Join(f.mnt, p) }

// ino returns a path's inode number through the mount.
func (f *fixture) ino(t *testing.T, p string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(f.path(p), &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

// read asserts a file's contents through the mount.
func (f *fixture) read(t *testing.T, p, want string) {
	t.Helper()
	got, err := os.ReadFile(f.path(p))
	if err != nil || string(got) != want {
		t.Fatalf("read %s = %q, %v; want %q", p, got, err, want)
	}
}

// names lists a directory through the mount.
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

// wantErrno asserts that err carries errno.
func wantErrno(t *testing.T, what string, err error, errno syscall.Errno) {
	t.Helper()
	if !errors.Is(err, errno) {
		t.Fatalf("%s: got %v, want %v", what, err, errno)
	}
}

// TestRootListsCheckoutsReadOnly checks the virtual root's listing, attributes, and refusals.
func TestRootListsCheckoutsReadOnly(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower"})
	if got := f.names(t, ""); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("root lists %v, want [alpha beta]", got)
	}
	info, err := os.Stat(f.mnt)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o555 {
		t.Fatalf("root = %v, %v; want a 0555 directory", info, err)
	}
	if ino := f.ino(t, ""); ino != rootID {
		t.Fatalf("root inode %d, want %d", ino, rootID)
	}
	// The kernel refuses writes to the 0555 root before asking the server,
	// except for root, whom the router then refuses.
	denied := syscall.EACCES
	if os.Geteuid() == 0 {
		denied = syscall.EROFS
	}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"mkdir", func() error { return os.Mkdir(f.path("gamma"), 0o755) }},
		{"create", func() error { return os.WriteFile(f.path("file"), nil, 0o644) }},
		{"symlink", func() error { return os.Symlink("alpha", f.path("link")) }},
		{"rmdir checkout", func() error { return os.Remove(f.path("alpha")) }},
		{"rename checkout", func() error { return os.Rename(f.path("alpha"), f.path("gamma")) }},
		{"move into root", func() error { return os.Rename(f.path("alpha/file"), f.path("file")) }},
	} {
		wantErrno(t, c.name, c.call(), denied)
	}
	if os.Geteuid() == 0 {
		wantErrno(t, "chmod checkout", os.Chmod(f.path("alpha"), 0o700), syscall.EROFS)
	}
	if _, err := os.Stat(f.path("gamma")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing checkout: %v, want ENOENT", err)
	}
}

// TestCheckoutReadWrite exercises the file and directory operations inside one checkout.
func TestCheckoutReadWrite(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower", "dir/full": "x"})
	f.read(t, "alpha/file", "lower")

	// Copy-up isolates the write to alpha.
	if err := os.WriteFile(f.path("alpha/file"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/file", "upper")
	f.read(t, "beta/file", "lower")

	// Create, positioned write, append, fsync, and truncate.
	fh, err := os.OpenFile(f.path("alpha/new"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
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
	_, err = os.OpenFile(f.path("alpha/new"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	wantErrno(t, "O_EXCL create", err, syscall.EEXIST)
	if info, err := os.Stat(f.path("alpha/new")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("created mode = %v, %v; want 0640", info, err)
	}
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

	// setattr: chmod, chown, utimens.
	if err := os.Chmod(f.path("alpha/new"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(f.path("alpha/new"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.path("alpha/new"))
	if err != nil || info.Mode().Perm() != 0o600 || !info.ModTime().Equal(mtime) || info.Size() != 5 {
		t.Fatalf("after setattr: %v, %v", info, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Lchown(f.path("alpha/new"), 1234, 5678); err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		if err := unix.Lstat(f.path("alpha/new"), &st); err != nil || st.Uid != 1234 || st.Gid != 5678 {
			t.Fatalf("after chown: uid %d gid %d, %v", st.Uid, st.Gid, err)
		}
	}

	// Directories, symlinks, and hard links.
	if err := os.Mkdir(f.path("alpha/sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, "mkdir existing", os.Mkdir(f.path("alpha/sub"), 0o750), syscall.EEXIST)
	if info, err := os.Stat(f.path("alpha/sub")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o750 {
		t.Fatalf("mkdir: %v, %v", info, err)
	}
	if err := os.Symlink("../file", f.path("alpha/sub/link")); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(f.path("alpha/sub/link")); err != nil || target != "../file" {
		t.Fatalf("readlink = %q, %v", target, err)
	}
	f.read(t, "alpha/sub/link", "upper")
	if err := os.Link(f.path("alpha/new"), f.path("alpha/sub/hard")); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/sub/hard", "hello")
	if err := os.WriteFile(f.path("alpha/sub/hard"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/new", "shared")
	wantErrno(t, "link across checkouts", os.Link(f.path("alpha/new"), f.path("beta/new")), syscall.EXDEV)
	if got := f.names(t, "alpha/sub"); !slices.Equal(got, []string{"hard", "link"}) {
		t.Fatalf("sub lists %v", got)
	}

	// Renames, removals, and their errors.
	if err := os.Rename(f.path("alpha/sub"), f.path("alpha/moved")); err != nil {
		t.Fatal(err)
	}
	f.read(t, "alpha/moved/hard", "shared")
	wantErrno(t, "rename across checkouts", os.Rename(f.path("alpha/file"), f.path("beta/moved")), syscall.EXDEV)
	f.read(t, "alpha/file", "upper")
	if _, err := os.Stat(f.path("beta/moved")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-checkout rename leaked into beta: %v", err)
	}
	wantErrno(t, "rmdir non-empty", unix.Rmdir(f.path("alpha/moved")), syscall.ENOTEMPTY)
	wantErrno(t, "rmdir file", unix.Rmdir(f.path("alpha/file")), syscall.ENOTDIR)
	wantErrno(t, "unlink dir", unix.Unlink(f.path("alpha/moved")), syscall.EISDIR)
	if err := os.RemoveAll(f.path("alpha/moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path("alpha/dir/full")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path("alpha/dir")); err != nil {
		t.Fatal(err)
	}
	wantErrno(t, "removed lookup", unix.Access(f.path("alpha/dir"), unix.F_OK), syscall.ENOENT)
	f.read(t, "beta/dir/full", "x")
	if got := f.names(t, "alpha"); !slices.Equal(got, []string{"file", "new"}) {
		t.Fatalf("alpha lists %v", got)
	}

	var sfs unix.Statfs_t
	if err := unix.Statfs(f.path("alpha"), &sfs); err != nil || sfs.Blocks == 0 || sfs.Bsize == 0 {
		t.Fatalf("statfs = %+v, %v", sfs, err)
	}
}

// TestInodesAreStableNodeIDs checks that inode numbers are the export's node
// ids and survive copy-up, renames, and a remount over the same node log.
func TestInodesAreStableNodeIDs(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower"})
	nodes := f.export.Nodes()
	ino := f.ino(t, "alpha/file")
	if want := nodes.ID("alpha/file"); ino != want {
		t.Fatalf("inode %d, node id %d", ino, want)
	}
	if ino == f.ino(t, "beta/file") {
		t.Fatal("checkouts share an inode")
	}
	if err := os.WriteFile(f.path("alpha/file"), []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.ino(t, "alpha/file"); got != ino {
		t.Fatalf("copy-up changed inode %d -> %d", ino, got)
	}
	if err := os.Rename(f.path("alpha/file"), f.path("alpha/renamed")); err != nil {
		t.Fatal(err)
	}
	if got := f.ino(t, "alpha/renamed"); got != ino {
		t.Fatalf("rename changed inode %d -> %d", ino, got)
	}
	f.unmount(t)
	if err := f.export.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	f.mount(t)
	if got := f.ino(t, "alpha/renamed"); got != ino {
		t.Fatalf("remount changed inode %d -> %d", ino, got)
	}
	f.read(t, "alpha/renamed", "upper")
}

// deadMountEnv names the mountpoint TestMountReplacesDeadMount's helper process
// mounts before it is killed.
const deadMountEnv = "OVNFS_TEST_DEAD_MOUNT"

// TestMountReplacesDeadMount checks that the mount a server killed before it
// unmounted leaves behind (the daemon after kill -9) does not block the next
// Mount there, where it fails both mkdir and fusermount3, also while the
// kernel still caches the dead mount's attributes.
func TestMountReplacesDeadMount(t *testing.T) {
	if dir := os.Getenv(deadMountEnv); dir != "" {
		f := newFixture(t, map[string]string{"file": "lower"})
		f.unmount(t)
		f.mnt = dir
		f.mount(t)
		fmt.Println("mounted")
		time.Sleep(time.Minute) // until the parent kills this process
		t.Fatal("helper was not killed")
	}
	f := newFixture(t, map[string]string{"file": "lower"})
	f.unmount(t)
	helper := exec.Command(os.Args[0], "-test.run=^TestMountReplacesDeadMount$")
	helper.Env = append(os.Environ(), deadMountEnv+"="+f.mnt, "TMPDIR="+t.TempDir())
	helper.Stderr = os.Stderr
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if line == "mounted\n" {
		// Any use before the kill leaves the root's attributes cached for TTL.
		_, err = os.Stat(f.path("alpha/file"))
	}
	helper.Process.Kill()
	helper.Wait()
	t.Cleanup(func() { detachDead(f.mnt) })
	if line != "mounted\n" || err != nil {
		t.Fatalf("helper mount: %q, %v", line, err)
	}

	f.mount(t)
	f.read(t, "alpha/file", "lower")
}

// TestForcedUnmountEndsBusyMount checks that a forced Unmount ends a mount the
// plain one refuses while a file on it is open (the daemon's "stop again to
// force"): the mountpoint is a plain directory again, and the holder's
// descriptor fails instead of reaching the export.
func TestForcedUnmountEndsBusyMount(t *testing.T) {
	var sfs unix.Statfs_t
	if err := unix.Statfs("/sys/fs/fuse/connections", &sfs); err != nil || sfs.Type != fusectlMagic {
		t.Skip("fusectl is not mounted at /sys/fs/fuse/connections")
	}
	f := newFixture(t, map[string]string{"held": "never read", "file": "lower"})
	if f.srv.abort == nil {
		t.Fatal("fusectl is mounted, but Mount found no abort file for its connection")
	}
	held, err := os.Open(f.path("alpha/held"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := f.srv.Unmount(false); err == nil {
		t.Fatal("plain unmount succeeded while a file on the mount is open")
	}
	f.read(t, "alpha/file", "lower")

	if err := f.srv.Unmount(true); err != nil {
		t.Fatalf("forced unmount: %v", err)
	}
	f.srv = nil
	if names, err := os.ReadDir(f.mnt); err != nil || len(names) != 0 {
		t.Fatalf("mountpoint after forced unmount lists %v, %v; want an empty directory", names, err)
	}
	if n, err := held.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatalf("read through the ended mount returned %d bytes", n)
	}
}

// fusectlMagic is the control filesystem's FUSE_CTL_SUPER_MAGIC.
const fusectlMagic = 0x65735543

// TestChangesUnderneathShowWithinTTL checks that edits and checkout removals made
// outside the mount reach it once the kernel's short caches expire.
func TestChangesUnderneathShowWithinTTL(t *testing.T) {
	f := newFixture(t, map[string]string{"file": "lower"})
	f.read(t, "alpha/file", "lower")
	held, err := os.Open(f.path("beta/file"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	nodes := f.export.Nodes()
	w, err := nodes.OpenFile("alpha/file", os.O_WRONLY|os.O_TRUNC, 0)
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
		t.Fatalf("removed checkout: %v, want ENOENT", err)
	}
	_, err = held.ReadAt(make([]byte, 4), 0)
	wantErrno(t, "read in removed checkout", err, syscall.ESTALE)
}

// TestOpenFileOutlivesItsName checks POSIX unlinked-but-open behavior and
// listings longer than one READDIR reply.
func TestOpenFileOutlivesItsName(t *testing.T) {
	f := newFixture(t, nil)
	fh, err := os.Create(f.path("alpha/tmp"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.Write([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path("alpha/tmp")); err != nil {
		t.Fatal(err)
	}
	info, err := fh.Stat()
	if err != nil || info.Size() != 10 {
		t.Fatalf("fstat after unlink: %v, %v", info, err)
	}
	buf := make([]byte, 10)
	if _, err := fh.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, []byte("still here")) {
		t.Fatalf("read after unlink = %q, %v", buf, err)
	}
	if err := fh.Truncate(5); err != nil {
		t.Fatalf("ftruncate after unlink: %v", err)
	}
	if info, err := fh.Stat(); err != nil || info.Size() != 5 {
		t.Fatalf("fstat after ftruncate: %v, %v", info, err)
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
	slices.Sort(want)
	if got := f.names(t, "alpha/many"); !slices.Equal(got, want) {
		t.Fatalf("listed %d entries, want %d", len(got), len(want))
	}
}

// TestListingReportsCurrentSizes checks that READDIRPLUS answers with the sizes
// entries have when it runs, not when their directory was opened: the kernel
// would keep a stale size for the attribute TTL and clamp reads to it.
func TestListingReportsCurrentSizes(t *testing.T) {
	f := newFixture(t, nil)
	if err := os.Mkdir(f.path("alpha/out"), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := os.Create(f.path("alpha/out/obj.o"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(f.path("alpha/out"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	data := bytes.Repeat([]byte("x"), 100000)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.ReadDir(-1); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(f.path("alpha/out/obj.o")); err != nil || info.Size() != int64(len(data)) {
		t.Fatalf("stat after listing: %v, %v; want size %d", info, err, len(data))
	}
	if got, err := os.ReadFile(f.path("alpha/out/obj.o")); err != nil || len(got) != len(data) {
		t.Fatalf("read after listing: %d bytes, %v; want %d", len(got), err, len(data))
	}
}

// TestErrnoMapping checks the export error to errno table.
func TestErrnoMapping(t *testing.T) {
	for _, c := range []struct {
		err  error
		want gofuse.Status
	}{
		{nil, gofuse.OK},
		{&fs.PathError{Op: "open", Path: "a/x", Err: syscall.ENOTEMPTY}, gofuse.Status(syscall.ENOTEMPTY)},
		{&os.LinkError{Op: "rename", Old: "a/x", New: "b/x", Err: syscall.EXDEV}, gofuse.Status(syscall.EXDEV)},
		{&fs.PathError{Op: "stat", Path: "c", Err: fs.ErrNotExist}, gofuse.ENOENT},
		{&fs.PathError{Op: "mkdir", Path: "c", Err: fs.ErrPermission}, gofuse.EROFS},
		{&fs.PathError{Op: "read", Path: "a/x", Err: fs.ErrClosed}, gofuse.Status(syscall.ESTALE)},
		{errors.New("boom"), gofuse.EIO},
	} {
		if got := errno(c.err); got != c.want {
			t.Errorf("errno(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestSpecialNodes checks bind and mkfifo through the actual FUSE mount.
func TestSpecialNodes(t *testing.T) {
	f := newFixture(t, nil)
	sock := f.path("alpha/sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fifo := f.path("alpha/fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p    string
		kind os.FileMode
	}{{sock, os.ModeSocket}, {fifo, os.ModeNamedPipe}} {
		info, err := os.Lstat(tc.p)
		if err != nil || info.Mode()&os.ModeType != tc.kind {
			t.Fatalf("%s: %v, %v", tc.p, info, err)
		}
	}
}

// TestIndexFaultBlocksTruncate exercises a writable kernel handle: neither
// truncate's descriptor path nor fstat's fallback may hide an identity error.
func TestIndexFaultBlocksTruncate(t *testing.T) {
	f := newFixture(t, map[string]string{"d/file": "unchanged", "pressure/file": "pressure"})
	held, err := os.OpenFile(f.path("alpha/d/file"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	nodes := f.export.Nodes()
	f.export.SetNodeCache(0)
	nodes.ID("alpha/pressure/file")
	nodes.ID("beta/d/file")
	nodes.ID("beta/pressure/file")
	files, err := filepath.Glob(filepath.Join(f.state, "nodes.log.cache", "instance-*", "*", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("cold indexes: %v %v", files, err)
	}
	for _, name := range files {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	if nodes.ID("beta/d/file") != 0 || nodes.IdentityError() == nil {
		t.Fatal("missing cold index did not latch a fault")
	}
	wantErrno(t, "ftruncate after index fault", held.Truncate(1), syscall.EIO)
	var session checkout.Session
	if err := f.reg.Enter("alpha", &session); err != nil {
		t.Fatal(err)
	}
	info, err := session.FS.Lstat("d/file")
	session.Done()
	if err != nil || info.Size() != int64(len("unchanged")) {
		t.Fatalf("failed truncate changed file size: %v %v", info, err)
	}
	time.Sleep(TTL + 100*time.Millisecond) // let the kernel's earlier attrs expire
	if _, err := held.Stat(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("fstat hid index fault: %v", err)
	}
}

func TestSlowReleaseDoesNotHoldHandleMutex(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	h := &fileHandle{release: func() { close(entered); <-resume }}
	other := &fileHandle{}
	x := &exportFS{handles: map[uint64]any{1: h, 2: other}}
	done := make(chan struct{})
	go func() { x.drop(1); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("release did not start")
	}
	found := make(chan *fileHandle, 1)
	go func() { found <- x.file(2) }()
	select {
	case got := <-found:
		if got != other {
			t.Error("lost unrelated handle")
		}
	case <-time.After(time.Second):
		close(resume)
		t.Fatal("release held handle mutex")
	}
	close(resume)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drop did not finish")
	}
}
