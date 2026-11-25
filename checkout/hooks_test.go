package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// installHook writes an executable post-checkout hook into hooksDir that creates a
// record file, and returns that file.
func installHook(t *testing.T, hooksDir string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	script := "#!/bin/sh\n: > '" + filepath.ToSlash(record) + "'\n"
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return record
}

// assertHookCheckout runs post-checkout in the exported checkout, as git does
// after a checkout, and checks that the hook that created record ran.
func (f *fixture) assertHookCheckout(name, sub, record string) {
	f.t.Helper()
	dir := filepath.Join(f.export(name), filepath.FromSlash(sub))
	if err := os.RemoveAll(record); err != nil {
		f.t.Fatal(err)
	}
	gitRun(f.t, dir, "hook", "run", "--ignore-missing", "post-checkout", "--", "old", "new", "1")
	if _, err := os.Stat(record); err != nil {
		f.t.Fatalf("the source's hook did not run in %s: %v", dir, err)
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
		{"fork", func(t *testing.T, f *fixture, dir string) string {
			f.newCheckout(dir, "parent")
			if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "parent", Name: "c"}); err != nil {
				t.Fatal(err)
			}
			return "c"
		}},
		{"rev on a checkout", func(t *testing.T, f *fixture, dir string) string {
			parent := f.newCheckout(dir, "parent").Checkout
			if _, err := f.reg.New(ctx, api.NewArgs{Dir: parent.Path, Rev: "HEAD", Name: "c"}); err != nil {
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

// TestHooksOwnHooksDirSurvivesSnapshot checks that a real hooks directory in a
// checkout's git directory goes through a snapshot untouched.
func TestHooksOwnHooksDirSurvivesSnapshot(t *testing.T) {
	f := newHooksFixture(t)
	dir := reuseRepo(t)
	installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	own := filepath.Join(f.gitdir("app"), "hooks")
	if err := os.Remove(own); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, "custom"), "mine")
	snap, _ := f.snapGit("app")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: snap, Name: "child"}); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(f.gitdir("child"), "hooks")
	if info, err := os.Lstat(copied); err != nil || !info.IsDir() {
		t.Fatalf("real hooks directory in the snapshot's copy: %v, %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(copied, "custom")); err != nil || string(data) != "mine" {
		t.Fatalf("hooks directory content = %q, %v", data, err)
	}
}

// TestHooksSubmodule runs a submodule repository's own hooks in the submodule of
// a checkout, and posts no fault on reopen once the checkout has removed the
// submodule's git directory.
func TestHooksSubmodule(t *testing.T) {
	f := newHooksFixture(t)
	app := submoduleRepo(t)
	record := installHook(t, filepath.Join(app, ".git", "modules", "vendor", "sub", "hooks"))
	f.newCheckout(app, "app")
	f.assertHookCheckout("app", "vendor/sub", record)
	f.reg.Close()
	if err := os.RemoveAll(filepath.Join(f.gitdir("app"), "modules", "vendor", "sub")); err != nil {
		t.Fatal(err)
	}
	if faults := f.reopenFaults(); len(faults) != 0 {
		t.Fatalf("faults after the submodule's git directory went = %q", faults)
	}
}

// reopenFaults opens the registry again and returns the faults it posted. Notices
// are emitted in queue order, so once a sentinel queued after the open arrives,
// every notice the open posted has.
func (f *fixture) reopenFaults() []string {
	f.t.Helper()
	ch := make(chan string, 64)
	cfg := f.config()
	cfg.Emit = func(e Event) {
		if payload, _ := e.Payload.(map[string]any); e.Name == "fault" {
			ch <- payload["message"].(string)
		}
	}
	reg, err := Open(cfg)
	if err != nil {
		f.t.Fatalf("open = %v", err)
	}
	f.reg = reg
	const mark = "sentinel"
	reg.enqueueNotice(baseNotice{fault: mark})
	var faults []string
	for {
		select {
		case m := <-ch:
			if m == mark {
				return faults
			}
			faults = append(faults, m)
		case <-time.After(30 * time.Second):
			f.t.Fatal("the notices were not delivered")
		}
	}
}

// TestHooksLinkFailureIsNotice reopens a registry whose checkout has a read-only
// root git directory, which cannot take the hooks link: the registry still opens,
// exactly one fault names the checkout, and the submodule's link is made.
func TestHooksLinkFailureIsNotice(t *testing.T) {
	enforcedPerms(t)
	f := newHooksFixture(t)
	app := submoduleRepo(t)
	installHook(t, filepath.Join(app, ".git", "hooks"))
	installHook(t, filepath.Join(app, ".git", "modules", "vendor", "sub", "hooks"))
	f.newCheckout(app, "c")
	f.reg.Close()
	gitdir := f.gitdir("c")
	module := filepath.Join(gitdir, "modules", "vendor", "sub")
	for _, link := range []string{filepath.Join(gitdir, "hooks"), filepath.Join(module, "hooks")} {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(gitdir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(gitdir, 0o755) })
	faults := f.reopenFaults()
	if len(faults) != 1 || !strings.Contains(faults[0], "checkout c") {
		t.Fatalf("faults = %q, want one naming checkout c", faults)
	}
	if _, err := os.Lstat(filepath.Join(module, "hooks")); err != nil {
		t.Fatalf("the submodule's link was not made after the root's failed: %v", err)
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

// TestHooksOffByDefault checks that with GitHooks off the source's hook does not
// run in a new checkout.
func TestHooksOffByDefault(t *testing.T) {
	f := newFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	gitRun(t, f.export("app"), "hook", "run", "--ignore-missing", "post-checkout", "--", "old", "new", "1")
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the source's hook ran with GitHooks off: %v", err)
	}
}

// TestHooksSnapshotsAcrossSetting checks that a hooks symlink the user made
// round-trips through a snapshot with GitHooks off, that a snapshot taken so
// gives a checkout opened with it on the folder's hooks, not the stale link, and
// that a snapshot taken with it on restores without a hooks entry with it off.
func TestHooksSnapshotsAcrossSetting(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := reuseRepo(t)
	record := installHook(t, filepath.Join(dir, ".git", "hooks"))
	f.newCheckout(dir, "app")
	mine := filepath.Join(t.TempDir(), "myhooks")
	installHook(t, mine)
	if err := os.Symlink(mine, filepath.Join(f.gitdir("app"), "hooks")); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	old, _ := f.snapGit("app")
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: old, Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(filepath.Join(f.gitdir("kept"), "hooks")); err != nil || got != mine {
		t.Fatalf("restored hooks = %q, %v; want the link to %s", got, err, mine)
	}

	f.reg.Close()
	f.gitHooks = true
	f.open()
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: old, Name: "relinked"}); err != nil {
		t.Fatal(err)
	}
	f.assertHookCheckout("relinked", "", record)
	snap, _ := f.snapGit("relinked")

	f.reg.Close()
	f.gitHooks = false
	f.open()
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: snap, Name: "bare"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.gitdir("bare"), "hooks")); !os.IsNotExist(err) {
		t.Fatalf("hooks entry restored from a snapshot taken with GitHooks on: %v", err)
	}
}
