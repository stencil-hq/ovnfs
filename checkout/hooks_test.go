package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

// installHook writes an executable post-checkout hook into hooksDir that appends
// its arguments and working directory to a record file, and returns that file
// (pwd -W, where MSYS sh has it, records a Windows path instead of /c/...).
func installHook(t *testing.T, hooksDir string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	script := "#!/bin/sh\n{ echo \"$@\"; pwd -W 2>/dev/null || pwd; } >> '" + filepath.ToSlash(record) + "'\n"
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return record
}

// assertHookRuns runs post-checkout in dir through git, as git does after a
// checkout, and checks that the hook recorded its arguments and ran in dir.
func assertHookRuns(t *testing.T, dir, record string) {
	t.Helper()
	if err := os.RemoveAll(record); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "hook", "run", "--ignore-missing", "post-checkout", "--", "old", "new", "1")
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the source's hook did not run in %s: %v", dir, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "old new 1" {
		t.Fatalf("hook recorded %q, want its arguments and working directory", lines)
	}
	if got, want := realDir(t, filepath.FromSlash(lines[1])), realDir(t, dir); got != want {
		t.Fatalf("hook ran in %s, want %s", got, want)
	}
}

// assertHookCheckout runs the source's hook in the exported checkout and checks
// that the link left it clean.
func (f *fixture) assertHookCheckout(name, sub, record string) {
	f.t.Helper()
	exported := f.export(name)
	assertHookRuns(f.t, filepath.Join(exported, filepath.FromSlash(sub)), record)
	if got := gitRun(f.t, exported, "status", "--porcelain"); got != "" {
		f.t.Fatalf("git status in %s = %q", name, got)
	}
	if f.dirty(name) {
		f.t.Fatalf("checkout %s reports changes", name)
	}
}

// removeSourceHooks removes the hooks directory of the repository at dir.
func removeSourceHooks(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(dir, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
}

// fillPool opens checkout "first" on dir and waits for the folder's pool of
// prepared checkouts to fill; it skips the test without folder events.
func (f *fixture) fillPool(dir string) {
	f.t.Helper()
	if f.reg.watcher == nil {
		f.t.Skip("no folder events on this platform")
	}
	f.newCheckout(dir, "first")
	eventually(f.t, "the pool did not fill", func() bool { return f.poolFull(dir) })
}

// assertNoStoredHooks checks that the stored git tree holds no hooks entry.
func (f *fixture) assertNoStoredHooks(tree string) {
	f.t.Helper()
	files, err := f.reg.store.Flatten(store.OID(tree))
	if err != nil {
		f.t.Fatal(err)
	}
	for p := range files {
		if p == "hooks" || strings.HasPrefix(p, "hooks/") {
			f.t.Fatalf("snapshot stores %s of a hooks link", p)
		}
	}
}

// TestHooksRunInEveryCreatePath checks that the hooks of the source repository run
// in checkouts opened by every create path.
func TestHooksRunInEveryCreatePath(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		open func(t *testing.T, f *fixture, dir string) string
	}{
		{"new", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "c")
			return "c"
		}},
		{"base", func(t *testing.T, f *fixture, dir string) string {
			first := f.newCheckout(dir, "first").Checkout
			if _, err := f.reg.New(ctx, api.NewArgs{Base: first.Base, Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
		{"warm", func(t *testing.T, f *fixture, dir string) string {
			f.fillPool(dir)
			f.newCheckout(dir, "c")
			return "c"
		}},
		{"fork", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "parent")
			if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "parent", Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
		{"from snapshot", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "parent")
			snap, _ := f.snapGit("parent")
			if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newHooksFixture(t)
			dir := reuseRepo(t)
			record := installHook(t, filepath.Join(dir, ".git", "hooks"))
			name := c.open(t, f, dir)
			f.assertHookCheckout(name, "", record)
		})
	}
}

// TestHooksSnapshotRelink gives a checkout made from a snapshot the source's hooks
// even when the snapshot's checkout had none, as one taken by an older build,
// keeps a real hooks directory, and stores no hooks link.
func TestHooksSnapshotRelink(t *testing.T) {
	ctx := context.Background()
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "old")
	if err := os.Remove(filepath.Join(f.gitdir("old"), "hooks")); err != nil {
		t.Fatal(err)
	}
	snap, _ := f.snapGit("old")
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromold"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("fromold", "", record)

	f.newCheckout(dir, "linked")
	snap, tree := f.snapGit("linked")
	f.assertNoStoredHooks(tree)
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromlinked"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("fromlinked", "", record)

	f.newCheckout(dir, "own")
	own := filepath.Join(f.gitdir("own"), "hooks")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, "custom"), "mine")
	snap, _ = f.snapGit("own")
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromown"}); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(f.gitdir("fromown"), "hooks")
	if info, err := os.Lstat(copied); err != nil || !info.IsDir() {
		t.Fatalf("real hooks directory in the snapshot's copy: %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(copied, "custom")); err != nil || string(data) != "mine" {
		t.Fatalf("hooks directory content = %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(copied, "post-checkout")); !os.IsNotExist(err) {
		t.Fatalf("the source's hook leaked into a real hooks directory: %v", err)
	}
}

// TestHooksReopenRelinks adds the link to checkouts that lack it when the
// registry opens, and leaves existing hooks entries alone.
func TestHooksReopenRelinks(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "old")
	f.newCheckout(dir, "own")
	if err := os.Remove(filepath.Join(f.gitdir("old"), "hooks")); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(f.gitdir("own"), "hooks")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, "custom"), "mine")
	f.reg.Close()
	f.open()
	f.assertHookCheckout("old", "", record)
	if info, err := os.Lstat(own); err != nil || !info.IsDir() {
		t.Fatalf("a real hooks directory was replaced: %v, %v", info, err)
	}
}

// TestHooksSurviveNoSourceHooks makes checkouts of a repository without a hooks
// directory: no link, and no error, also on reopen.
func TestHooksSurviveNoSourceHooks(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	removeSourceHooks(t, dir)
	f.newCheckout(dir, "app")
	f.reg.Close()
	f.open()
	if _, err := os.Lstat(filepath.Join(f.gitdir("app"), "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks entry without source hooks: %v", err)
	}
}

// TestHooksRmKeepsSource removes a checkout with its link and leaves the source's
// hooks as they were.
func TestHooksRmKeepsSource(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	hooks := filepath.Join(dir, ".git", "hooks")
	script := "#!/bin/sh\nexit 0\n"
	write(t, filepath.Join(hooks, "post-checkout"), script)
	f.newCheckout(dir, "app")
	if _, err := os.Lstat(filepath.Join(f.gitdir("app"), "hooks")); err != nil {
		t.Fatalf("checkout has no hooks link: %v", err)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	f.awaitTrash() // rm moves the git directory to the trash, which deletes it in the background
	if entries := checkoutDirs(t, f.state, "git"); len(entries) != 0 {
		t.Fatalf("git directories after rm = %v", entries)
	}
	if data, err := os.ReadFile(filepath.Join(hooks, "post-checkout")); err != nil || string(data) != script {
		t.Fatalf("source hook after rm = %q, %v", data, err)
	}
}

// TestHooksSubmodule runs a submodule repository's own hooks in the submodule of
// a checkout, also in a fork and after the link is lost.
func TestHooksSubmodule(t *testing.T) {
	f := newHooksFixture(t)
	app := submoduleRepo(t)
	record := installHook(t, filepath.Join(app, ".git", "modules", "vendor", "sub", "hooks"))
	f.newCheckout(app, "app")
	check := func(name string) { f.assertHookCheckout(name, "vendor/sub", record) }
	check("app")
	if _, err := f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	check("child")
	if err := os.Remove(filepath.Join(f.gitdir("app"), "modules", "vendor", "sub", "hooks")); err != nil {
		t.Fatal(err)
	}
	f.reg.Close()
	f.open()
	check("app")
}

// TestHooksLinkFailureIsNotice names a prepared checkout whose root git
// directory cannot take a link (read-only), with its submodule's git directory
// either taking it or not. The checkout is created without the root's link and
// with the submodule's if it could be made; exactly one fault names the checkout,
// and when both links fail it names both failures. The checkout opens again the
// same way. A link failure never fails a create or the registry's open.
func TestHooksLinkFailureIsNotice(t *testing.T) {
	t.Run("root", func(t *testing.T) { testHooksLinkFailure(t, false) })
	t.Run("root and submodule", func(t *testing.T) { testHooksLinkFailure(t, true) })
}

func testHooksLinkFailure(t *testing.T, submoduleFails bool) {
	enforcedPerms(t)
	f := newHooksFixture(t)
	// Linux refuses to move a 0555 directory to another parent, so the trash's
	// discard falls back to removeTree and the pooled read-only directories are
	// removed in place. What stays read-only is the named checkout's own git
	// directory below state/git; removeTree makes it writable as it removes the
	// state after Close, before TempDir's own removal.
	t.Cleanup(func() {
		f.reg.Close()
		if err := removeTree(f.state); err != nil {
			t.Error(err)
		}
	})
	app := submoduleRepo(t)
	f.fillPool(app)
	module := filepath.Join("modules", "vendor", "sub")
	prepared, err := filepath.Glob(filepath.Join(f.state, "git", preparedPrefix+"*"))
	if err != nil || len(prepared) == 0 {
		t.Fatalf("no prepared git directory: %v, %v", prepared, err)
	}
	for _, gitdir := range prepared {
		// Marked already, so naming the checkout does not rewrite the config.
		gitRun(t, app, "config", "--file", filepath.Join(gitdir, "config"), "ovnfs.checkout", "c")
		gitRun(t, app, "config", "--file", filepath.Join(gitdir, module, "config"), "ovnfs.checkout", "c")
		// A read-only directory cannot take its hooks link.
		if submoduleFails {
			if err := os.Chmod(filepath.Join(gitdir, module), 0o555); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(gitdir, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	f.newCheckout(app, "c")
	const notice = "link hooks of checkout c"
	// scan reports whether the sentinel mark was delivered and returns the faults
	// for the failed links.
	scan := func(mark string) (delivered bool, got []string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, m := range f.faults {
			switch {
			case m == mark:
				delivered = true
			case strings.Contains(m, notice):
				got = append(got, m)
			}
		}
		return delivered, got
	}
	// The notice worker emits notices in queue order, one durable write apart.
	// With the base loaded and the state writable, as here, a sentinel queued
	// after them is emitted only once every earlier notice has been, so a
	// duplicate posted by the same operation would show before it.
	assertFault := func() {
		t.Helper()
		const mark = "sentinel"
		f.reg.enqueueNotice(baseNotice{fault: mark})
		var got []string
		eventually(t, "the notices are not delivered", func() bool {
			var delivered bool
			delivered, got = scan(mark)
			return delivered
		})
		if len(got) != 1 {
			t.Fatalf("%d faults for the failed links, want 1: %q", len(got), got)
		}
		if submoduleFails {
			// namePrepared links in place, so the directory keeps its prepared name.
			want := filepath.Join(f.gitdir("c"), module, "hooks") + ":"
			if !strings.Contains(got[0], want) || strings.Count(got[0], "permission denied") != 2 {
				t.Fatalf("fault %q does not name both failures", got[0])
			}
		}
	}
	assertFault()
	gitdir := f.gitdir("c")
	if _, err := os.Lstat(filepath.Join(gitdir, "hooks")); !os.IsNotExist(err) {
		t.Fatalf("the root's hooks entry exists: %v", err)
	}
	_, err = os.Lstat(filepath.Join(gitdir, module, "hooks"))
	if submoduleFails && !os.IsNotExist(err) {
		t.Fatalf("the submodule's hooks entry exists: %v", err)
	}
	if !submoduleFails && err != nil {
		t.Fatalf("the submodule's link was not made after the root's failed: %v", err)
	}
	f.mu.Lock()
	f.faults = nil
	f.mu.Unlock()
	f.reg.Close()
	f.open()
	assertFault()
}

// TestHooksRetrofitKeepsTouched keeps a checkout's idle time when reopening adds
// its hooks link, which changes the git directory.
func TestHooksRetrofitKeepsTouched(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	gitdir := f.gitdir("app")
	if err := os.Remove(filepath.Join(gitdir, "hooks")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "--git-dir", gitdir, "config", "--local", "--unset", "ovnfs.checkout")
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{filepath.Join(gitdir, "logs", "HEAD"), gitdir} {
		if err := os.Chtimes(p, old, old); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	f.reg.Close()
	f.open()
	if _, err := os.Lstat(filepath.Join(gitdir, "hooks")); err != nil {
		t.Fatalf("reopen did not link hooks: %v", err)
	}
	if got := gitRun(t, dir, "--git-dir", gitdir, "config", "ovnfs.checkout"); got != "app" {
		t.Fatalf("marker after reopen = %q", got)
	}
	c := f.ls().Checkouts[0]
	if !c.Touched.Equal(c.Created) {
		t.Fatalf("touched %v after the retrofit, want the creation time %v", c.Touched, c.Created)
	}
}

// TestHooksOnlyOfCapturedRepo does not link a checkout to the hooks of a
// repository enclosing its folder, which a reopen finds once the folder's own
// repository is gone.
func TestHooksOnlyOfCapturedRepo(t *testing.T) {
	f := newHooksFixture(t)
	outer := folder(t, nil)
	gitRun(t, outer, "init", "-q", "-b", "main")
	installHook(t, filepath.Join(outer, ".git", "hooks"))
	dir := filepath.Join(outer, "inner")
	write(t, filepath.Join(dir, "a.txt"), "one")
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	f.newCheckout(dir, "app")
	f.reg.Close()
	if err := os.Rename(filepath.Join(dir, ".git"), filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.gitdir("app"), "hooks")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	f.open()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("checkout linked to the enclosing repository's hooks: %v", err)
	}
}

// TestHooksAddedAfterPoolFilled opens a checkout from a pool filled while the
// source had no hooks directory, after one was added.
func TestHooksAddedAfterPoolFilled(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	removeSourceHooks(t, dir)
	f.fillPool(dir)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "c")
	f.assertHookCheckout("c", "", record)
}

// TestHooksRemovedAfterPoolFilled opens a checkout from a pool filled while the
// source had hooks, after they were removed: no link is left dangling.
func TestHooksRemovedAfterPoolFilled(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.fillPool(dir)
	removeSourceHooks(t, dir)
	f.newCheckout(dir, "c")
	if _, err := os.Lstat(filepath.Join(f.gitdir("c"), "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks entry after the source's hooks went: %v", err)
	}
}

// TestHooksSourceHooksNotADirectory makes checkouts of a repository whose hooks
// entry is a file: no link, and no failure.
func TestHooksSourceHooksNotADirectory(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	removeSourceHooks(t, dir)
	write(t, filepath.Join(dir, ".git", "hooks"), "not a directory")
	f.newCheckout(dir, "app")
	if _, err := os.Lstat(filepath.Join(f.gitdir("app"), "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks entry from a source hooks file: %v", err)
	}
}

// TestHooksOwnLinkNotSnapshotted drops a hooks symlink of any target from a
// snapshot: a checkout from it gets the source's hooks, not the link the user made.
func TestHooksOwnLinkNotSnapshotted(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	mine := filepath.Join(t.TempDir(), "myhooks")
	installHook(t, mine)
	link := filepath.Join(f.gitdir("app"), "hooks")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mine, link); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	snap, tree := f.snapGit("app")
	f.assertNoStoredHooks(tree)
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: snap, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("child", "", record)
}

// TestHooksOlderSnapshotLink restores a snapshot an older build took of a git
// directory holding a hooks symlink the user made: the checkout from it still
// gets the source's hooks.
func TestHooksOlderSnapshotLink(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	snap, tree := f.snapGit("app")
	files, err := f.reg.store.Flatten(store.OID(tree))
	if err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(t.TempDir(), "myhooks")
	installHook(t, mine)
	id, err := f.reg.store.PutBytes([]byte(mine))
	if err != nil {
		t.Fatal(err)
	}
	files["hooks"] = store.File{Mode: store.ModeSymlink, ID: id}
	older, err := f.reg.store.Build(files)
	if err != nil {
		t.Fatal(err)
	}
	f.reg.mu.Lock()
	f.reg.snapshots[snap].git = older
	f.reg.mu.Unlock()
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: snap, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("child", "", record)
}

// TestHooksLegacySnapshotLinks restores a whole-directory snapshot from a
// released build holding hooks symlinks the user made, at the root and in a
// module git directory the capture does not list: neither is kept, and the root
// gets the source's hooks.
func TestHooksLegacySnapshotLinks(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	mine := filepath.Join(t.TempDir(), "myhooks")
	installHook(t, mine)
	id, _ := f.legacySnapshot("app", func(copied string) {
		extra := filepath.Join(copied, "modules", "extra")
		write(t, filepath.Join(extra, "HEAD"), "ref: refs/heads/main\n")
		write(t, filepath.Join(extra, "config"), "[core]\n\tbare = false\n")
		for _, link := range []string{filepath.Join(copied, "hooks"), filepath.Join(extra, "hooks")} {
			if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink(mine, link); err != nil {
				t.Skipf("no symlinks: %v", err)
			}
		}
	})
	f.open()
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: id, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("child", "", record)
	if _, err := os.Lstat(filepath.Join(f.gitdir("child"), "modules", "extra", "hooks")); !os.IsNotExist(err) {
		t.Fatalf("a module's hooks symlink survived the restore: %v", err)
	}
}

// TestHooksOffByDefault checks that with GitHooks off a new checkout has no hooks
// entry and the source's hook does not run in it, however it was created.
func TestHooksOffByDefault(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
		t.Fatal(err)
	}
	snap, _ := f.snapGit("app")
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "fromsnap"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app", "child", "fromsnap"} {
		if _, err := os.Lstat(filepath.Join(f.gitdir(name), "hooks")); !os.IsNotExist(err) {
			t.Fatalf("%s has a hooks entry with GitHooks off: %v", name, err)
		}
		gitRun(t, f.export(name), "hook", "run", "--ignore-missing", "post-checkout", "--", "old", "new", "1")
		if _, err := os.Stat(record); !os.IsNotExist(err) {
			t.Fatalf("the source's hook ran in %s with GitHooks off: %v", name, err)
		}
	}
}

// TestHooksOffKeepsHandMadeSymlink checks that with GitHooks off a hooks symlink
// the user made in a checkout's git directory is stored by a snapshot and
// restored by new --from as the same link, as before the setting existed.
func TestHooksOffKeepsHandMadeSymlink(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	f.newCheckout(dir, "app")
	mine := filepath.Join(t.TempDir(), "myhooks")
	installHook(t, mine)
	link := filepath.Join(f.gitdir("app"), "hooks")
	if err := os.Symlink(mine, link); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	snap, tree := f.snapGit("app")
	files, err := f.reg.store.Flatten(store.OID(tree))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := files["hooks"]; !ok || got.Mode != store.ModeSymlink {
		t.Fatalf("snapshot stores hooks as %+v, %v; want a symlink", got, ok)
	}
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: snap, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(f.gitdir("child"), "hooks")); err != nil || got != mine {
		t.Fatalf("restored hooks = %q, %v; want the link to %s", got, err, mine)
	}
}

// TestHooksKeptWhenSettingTurnedOff checks that a link made with GitHooks on stays
// when the registry reopens with it off: nothing removes it.
func TestHooksKeptWhenSettingTurnedOff(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	f.reg.Close()
	f.gitHooks = false
	f.open()
	f.assertHookCheckout("app", "", record)
}
