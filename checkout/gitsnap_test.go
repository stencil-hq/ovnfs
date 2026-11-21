package checkout

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
)

// snapGit snapshots a checkout and returns the snapshot's id and stored git tree.
func (f *fixture) snapGit(checkout string) (string, string) {
	f.t.Helper()
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: checkout, Wait: true})
	if err != nil {
		f.t.Fatal(err)
	}
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	return reply.Snapshot.ID, string(f.reg.snapshots[reply.Snapshot.ID].git)
}

// TestGitSnapshotsStoreGitdir checks that snapshots keep a checkout's git directory as a
// stored tree (shared while unchanged, never copied per snapshot), and that checkouts
// from them, including forks, start from exactly that git state.
func TestGitSnapshotsStoreGitdir(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	gitdir := f.gitdir("app")
	initial := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD")
	first, firstTree := f.snapGit("app")
	_, secondTree := f.snapGit("app")
	if firstTree == "" || firstTree != secondTree {
		t.Fatalf("unchanged git directory stored as %q then %q", firstTree, secondTree)
	}
	gitRun(t, dir, "--git-dir", gitdir, "commit", "--allow-empty", "-qm", "agent")
	agent := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD")
	write(t, filepath.Join(gitdir, "index.lock"), "")
	_, thirdTree := f.snapGit("app")
	if err := os.Remove(filepath.Join(gitdir, "index.lock")); err != nil {
		t.Fatal(err)
	}
	if thirdTree == secondTree {
		t.Fatal("commit in the checkout did not change its stored git directory")
	}
	if entries, err := os.ReadDir(filepath.Join(f.state, "snapshots")); err != nil || len(entries) != 0 {
		t.Fatalf("snapshot directories = %v, %v", entries, err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: first, Name: "old"}); err != nil {
		t.Fatal(err)
	}
	old := f.gitdir("old")
	if got := gitRun(t, dir, "--git-dir", old, "rev-parse", "HEAD"); got != initial {
		t.Fatalf("checkout from first snapshot at %s, want %s", got, initial)
	}
	if got := gitRun(t, dir, "--git-dir", old, "ls-files"); !strings.Contains(got, "tracked.txt") {
		t.Fatalf("index from first snapshot lists %q", got)
	}
	if got := gitRun(t, dir, "--git-dir", old, "config", "--get", "ovnfs.checkout"); got != "old" {
		t.Fatalf("marker = %q", got)
	}
	fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	child := f.gitdir("child")
	if got := gitRun(t, dir, "--git-dir", child, "cat-file", "-t", "HEAD"); got != "commit" {
		t.Fatalf("fork cannot read its commit: %q", got)
	}
	if got := gitRun(t, dir, "--git-dir", child, "rev-parse", "HEAD"); got != agent {
		t.Fatalf("fork at %s, want %s", got, agent)
	}
	if _, err := os.Lstat(filepath.Join(child, "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("fork inherited a lock file: %v", err)
	}
	if fork.Checkout.Name != "child" {
		t.Fatalf("fork = %+v", fork)
	}
	if entries := checkoutDirs(t, f.state, "git"); len(entries) != 3 {
		t.Fatalf("git directories = %v (want one per checkout, no leftovers)", entries)
	}
}

// TestGitSnapshotKeepsNestedEmptyDirs snapshots a git directory holding a tree of
// empty directories, as ref deletions leave behind, and restores it in a checkout
// from the snapshot.
func TestGitSnapshotKeepsNestedEmptyDirs(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	nested := filepath.Join("refs", "tool", "turns", "a", "b")
	if err := os.MkdirAll(filepath.Join(f.gitdir("app"), nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.gitdir("app"), "refs", "tool", "turns", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	id, _ := f.snapGit("app")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: id, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{nested, filepath.Join("refs", "tool", "turns", "c")} {
		if info, err := os.Stat(filepath.Join(f.gitdir("view"), d)); err != nil || !info.IsDir() {
			t.Fatalf("%s in the restored git directory: %v", d, err)
		}
	}
}

// TestGitSnapshotTreesAreGCRoots sweeps every object no root reaches and checks a
// snapshot's git directory still materializes afterwards.
func TestGitSnapshotTreesAreGCRoots(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	head := gitRun(t, dir, "--git-dir", f.gitdir("app"), "rev-parse", "HEAD")
	id, _ := f.snapGit("app")
	f.reg.mu.RLock()
	roots := f.reg.treeRootsLocked()
	f.reg.mu.RUnlock()
	// A cutoff in the future leaves nothing protected by age or by a recent Build.
	if _, err := f.reg.store.Sweep(roots, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: id, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "--git-dir", f.gitdir("view"), "rev-parse", "HEAD"); got != head {
		t.Fatalf("view at %s, want %s", got, head)
	}
}

// legacySnapshot snapshots checkout as a build that copied whole git directories
// did: it closes the registry, stores the checkout's git directory as a copy at
// snapshots/<id>/git (edit changes the copy first), and clears the snapshot's
// git tree. The caller reopens the registry; the snapshot's id and its directory
// are returned.
func (f *fixture) legacySnapshot(checkout string, edit func(copied string)) (id, legacy string) {
	f.t.Helper()
	id, _ = f.snapGit(checkout)
	gitdir := f.gitdir(checkout)
	if err := f.reg.Close(); err != nil {
		f.t.Fatal(err)
	}
	legacy = filepath.Join(f.state, "snapshots", id)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		f.t.Fatal(err)
	}
	copied := filepath.Join(legacy, "git")
	if err := git.CopyGitdir(gitdir, copied, "unused"); err != nil {
		f.t.Fatal(err)
	}
	if edit != nil {
		edit(copied)
	}
	d := readDisk(f.t, f.state)
	for i := range d.Snapshots {
		d.Snapshots[i].Git = ""
	}
	if err := writeState(f.state, d); err != nil {
		f.t.Fatal(err)
	}
	return id, legacy
}

// TestLegacySnapshotGitCopy opens a snapshot recorded by a build that copied the whole
// git directory into snapshots/<id>/git, and drops it.
func TestLegacySnapshotGitCopy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	f.newCheckout(dir, "app")
	gitdir := f.gitdir("app")
	head := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD")
	id, legacy := f.legacySnapshot("app", nil)
	gitRun(t, dir, "--git-dir", gitdir, "commit", "--allow-empty", "-qm", "later")
	f.open()
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: id, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, dir, "--git-dir", f.gitdir("view"), "rev-parse", "HEAD"); got != head {
		t.Fatalf("view of legacy snapshot at %s, want %s", got, head)
	}
	if err := f.reg.Drop(ctx, api.DropArgs{Snapshot: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(legacy, "git")); !os.IsNotExist(err) {
		t.Fatalf("dropped legacy copy remains: %v", err)
	}
}
