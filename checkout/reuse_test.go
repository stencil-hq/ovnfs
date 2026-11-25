package checkout

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// reuseRepo is a committed repository with an ignored build directory.
func reuseRepo(t *testing.T) string {
	t.Helper()
	dir := folder(t, map[string]string{"a.txt": "one", ".gitignore": "build/\n", "build/out": "o"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	return dir
}

// checkoutHead returns the commit HEAD names in a checkout's private git directory.
func (f *fixture) checkoutHead(name string) string {
	f.t.Helper()
	return gitRun(f.t, f.t.TempDir(), "--git-dir", f.gitdir(name), "rev-parse", "HEAD")
}

// reuseFolder returns the registry's record of the folder at dir, found by its
// source root as creates resolve it (on macOS, temporary directories lie below
// the /var symlink); nil for none. f.reg.mu is held.
func (f *fixture) reuseFolder(dir string) *reuseFolder {
	f.t.Helper()
	root, err := f.reg.resolveRoot(dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.reg.folders[root]
}

// prepared counts the checkouts the folder at dir holds prepared.
func (f *fixture) prepared(dir string) int {
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	if folder := f.reuseFolder(dir); folder != nil {
		return len(folder.pool)
	}
	return 0
}

// poolFull reports whether the folder at dir holds as many prepared checkouts as its pool should.
func (f *fixture) poolFull(dir string) bool {
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	folder := f.reuseFolder(dir)
	return folder != nil && len(folder.pool) >= folder.poolTarget()
}

// TestSequentialCreatesKeepPoolSmall makes creates of a folder one after another,
// each as soon as the last returns: none runs beside another, so the pool stays at
// poolMin however many follow.
func TestSequentialCreatesKeepPoolSmall(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	for i := range 2 * poolMin {
		f.newCheckout(dir, fmt.Sprintf("c%d", i))
	}
	f.reg.mu.RLock()
	size := f.reuseFolder(dir).poolSize
	f.reg.mu.RUnlock()
	if size != poolMin {
		t.Fatalf("pool size %d after sequential creates, want %d", size, poolMin)
	}
}

// TestPoolTargetFitsBudget keeps the git directories a folder holds prepared
// within poolBudget, always keeping one, and a pool without git directories at
// its size.
func TestPoolTargetFitsBudget(t *testing.T) {
	cases := []struct {
		size  int
		bytes int64
		want  int
	}{
		{poolMax, 0, poolMax},
		{poolMin, 4 << 20, poolMin},
		{poolMax, 4 << 20, 64},
		{poolMax, poolBudget, 1},
		{poolMin, poolBudget + 1, 1},
	}
	for _, c := range cases {
		f := reuseFolder{poolSize: c.size, template: gitTemplate{bytes: c.bytes}}
		if got := f.poolTarget(); got != c.want {
			t.Errorf("pool of %d with %d-byte templates keeps %d, want %d", c.size, c.bytes, got, c.want)
		}
	}
}

// TestFolderCreatesReuseUnchangedCapture opens later checkouts of an unchanged
// folder on the earlier capture, and captures again as soon as the folder
// changes in any way a checkout shows: source content, or the repository's
// commits. Edits below a cache path change nothing a capture holds. Checkouts
// prepared before a source edit serve the new capture, with working git.
func TestFolderCreatesReuseUnchangedCapture(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	first := f.newCheckout(dir, "first").Checkout
	second := f.newCheckout(dir, "second").Checkout
	if second.Base != first.Base {
		t.Fatalf("unchanged folder captured again: base %s, then %s", first.Base, second.Base)
	}
	if got := f.read("second", "a.txt"); got != "one" {
		t.Fatalf("reused capture serves %q", got)
	}

	write(t, filepath.Join(dir, "build/out"), "rebuilt")
	if got := f.newCheckout(dir, "cache").Checkout.Base; got != first.Base {
		t.Fatalf("an edit below a cache path forced a capture: base %s, want %s", got, first.Base)
	}
	// It took a new cache snapshot instead, whose fence shows the rebuilt
	// output; the earlier checkouts keep their fence, which hides it.
	if got := f.read("cache", "build/out"); got != "rebuilt" {
		t.Fatalf("checkout after a cache edit reads build/out = %q", got)
	}
	if got := f.read("first", "build/out"); got != "<absent>" {
		t.Fatalf("earlier checkout reads build/out = %q after the folder rebuilt it; want it gone, not rebuilt", got)
	}
	eventually(t, "the pool did not fill", func() bool { return f.poolFull(dir) })
	write(t, filepath.Join(dir, "a.txt"), "two")
	edited := f.newCheckout(dir, "edited").Checkout
	if edited.Base == first.Base {
		t.Fatal("a source edit made just before the create reused the old capture")
	}
	if got := f.read("edited", "a.txt"); got != "two" {
		t.Fatalf("checkout after a source edit serves %q", got)
	}
	if got := f.read("second", "a.txt"); got != "one" {
		t.Fatalf("earlier checkout changed to %q", got)
	}
	// The recapture wrote its git template in the background; the pool fills again from it.
	eventually(t, "the pool did not refill after the recapture", func() bool { return f.poolFull(dir) })
	// A checkout prepared for the recapture, from its template, serves it.
	if got := f.newCheckout(dir, "later").Checkout.Base; got != edited.Base {
		t.Fatalf("create after the recapture on base %s, want %s", got, edited.Base)
	}
	if got := f.read("later", "a.txt"); got != "two" {
		t.Fatalf("prepared checkout serves %q after the edit", got)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("later"), "cat-file", "-t", "HEAD^{tree}"); got != "tree" {
		t.Fatalf("prepared checkout cannot read its objects: %q", got)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("later"), "config", "ovnfs.checkout"); got != "later" {
		t.Fatalf("checkout marker = %q", got)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("later"), "config", "ovnfs.source"); got != edited.Source {
		t.Fatalf("checkout source = %q, want %q", got, edited.Source)
	}

	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "later")
	committed := f.newCheckout(dir, "committed").Checkout
	if committed.Base == edited.Base {
		t.Fatal("a commit made just before the create reused the old capture")
	}
	if got, want := f.checkoutHead("committed"), gitRun(t, dir, "rev-parse", "HEAD"); got != want {
		t.Fatalf("checkout HEAD %s, want the folder's %s", got, want)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("committed"), "config", "ovnfs.checkout"); got != "committed" {
		t.Fatalf("checkout marker = %q", got)
	}

	if got := f.newCheckout(dir, "rules", "local a.txt").Checkout.Base; got == committed.Base {
		t.Fatal("a create with other rules reused a capture classified by different ones")
	}
}

// TestCoolingKeepsMovablePools keeps a repository's warm capture and its
// prepared checkouts past a work-tree edit, so the next create takes one of
// them instead of discarding the pool and preparing another, and releases them
// once the repository's git state moves on. A folder outside any repository,
// whose pool cannot move, goes at its first edit.
func TestCoolingKeepsMovablePools(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	repo, plain := reuseRepo(t), folder(t, map[string]string{"a.txt": "one"})
	// settled reports whether the folder at dir holds a full pool and fills it no more.
	settled := func(dir string) bool {
		f.reg.mu.RLock()
		defer f.reg.mu.RUnlock()
		folder := f.reuseFolder(dir)
		return folder != nil && len(folder.pool) >= folder.poolTarget() && folder.filling == 0
	}
	// outgrown reports whether the folder at dir changed since its warm capture
	// began, and whether its repository's git state did.
	outgrown := func(dir string) (work, git bool) {
		f.reg.mu.RLock()
		defer f.reg.mu.RUnlock()
		folder := f.reuseFolder(dir)
		return folder.warmGen != folder.gen.Load(), folder.warmGitGen != folder.gitGen.Load()
	}
	f.newCheckout(repo, "first")
	f.newCheckout(plain, "plain")
	eventually(t, "the pools did not fill", func() bool { return settled(repo) && settled(plain) })
	f.reg.mu.RLock()
	var pool []string
	for _, h := range f.reuseFolder(repo).pool {
		pool = append(pool, h.meta.dir())
	}
	f.reg.mu.RUnlock()

	write(t, filepath.Join(repo, "a.txt"), "two")
	write(t, filepath.Join(plain, "a.txt"), "two")
	eventually(t, "the edits were not seen", func() bool {
		repoWork, _ := outgrown(repo)
		plainWork, _ := outgrown(plain)
		return repoWork && plainWork
	})
	if _, git := outgrown(repo); git {
		t.Fatal("a work-tree edit counted as a change of git state")
	}
	f.reg.cool(time.Now())
	if got := f.prepared(repo); got != len(pool) {
		t.Fatalf("cooling after a work-tree edit left %d of %d prepared checkouts", got, len(pool))
	}
	if got := f.prepared(plain); got != 0 {
		t.Fatalf("cooling after an edit kept %d prepared checkouts of a folder outside any repository", got)
	}
	f.newCheckout(repo, "edited")
	f.reg.mu.RLock()
	dir := f.reg.checkouts["edited"].meta.dir()
	f.reg.mu.RUnlock()
	if !slices.Contains(pool, dir) {
		t.Fatalf("create after a work-tree edit opened %s, want one of the kept pool %v", dir, pool)
	}
	if got := f.read("edited", "a.txt"); got != "two" {
		t.Fatalf("checkout after the edit serves %q", got)
	}

	eventually(t, "the pool did not refill", func() bool { return settled(repo) })
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "later")
	eventually(t, "the commit was not seen", func() bool {
		_, git := outgrown(repo)
		return git
	})
	f.reg.cool(time.Now())
	if got := f.prepared(repo); got != 0 {
		t.Fatalf("cooling after a commit kept %d prepared checkouts", got)
	}
}

// TestNewOnCaptureStartsAsCaptured opens a checkout on an earlier capture after
// the folder moved on: it holds the captured files and the git state the
// capture's first checkout started from, under its own name.
func TestNewOnCaptureStartsAsCaptured(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	first := f.newCheckout(dir, "first").Checkout
	head := f.checkoutHead("first")
	f.put("first", "a.txt", "agent")
	write(t, filepath.Join(dir, "a.txt"), "two")
	gitRun(t, dir, "commit", "-qam", "later")

	reply, err := f.reg.New(ctx, api.NewArgs{Base: first.Base, Name: "twin"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Checkout.Base != first.Base || !reply.Checkout.Git {
		t.Fatalf("twin = %+v, want a git checkout on %s", reply.Checkout, first.Base)
	}
	if got := f.read("twin", "a.txt"); got != "one" {
		t.Fatalf("twin serves %q, want the captured file", got)
	}
	if got := f.checkoutHead("twin"); got != head {
		t.Fatalf("twin HEAD %s, want %s", got, head)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("twin"), "config", "ovnfs.checkout"); got != "twin" {
		t.Fatalf("twin marker = %q", got)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("twin"), "config", "ovnfs.source"); got != first.Source {
		t.Fatalf("twin source = %q, want %q", got, first.Source)
	}
	if got := gitRun(t, f.t.TempDir(), "--git-dir", f.gitdir("twin"), "cat-file", "-t", "HEAD^{tree}"); got != "tree" {
		t.Fatalf("twin cannot read its objects: %q", got)
	}
	// The first checkout's name stands for its capture.
	named, err := f.reg.New(ctx, api.NewArgs{Base: "first", Name: "baseline"})
	if err != nil || named.Checkout.Base != first.Base || f.read("baseline", "a.txt") != "one" {
		t.Fatalf("a checkout on first's capture, by name = %+v, %v", named.Checkout, err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Base: "b_missing"}); err == nil {
		t.Fatal("a checkout on an unknown capture opened")
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Base: first.Base, Dir: dir}); err == nil {
		t.Fatal("a create naming both a folder and a capture opened")
	}
}

// TestPreparedCheckoutsOutsideRepositories serves checkouts of a folder outside
// any repository from prepared ones as well, with no git directory.
func TestPreparedCheckoutsOutsideRepositories(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := folder(t, map[string]string{"a.txt": "one"})
	first := f.newCheckout(dir, "first").Checkout
	eventually(t, "no checkout was prepared", func() bool { return f.prepared(dir) > 0 })
	second := f.newCheckout(dir, "second").Checkout
	if second.Base != first.Base || second.Git {
		t.Fatalf("second = %+v, want a checkout without git on base %s", second, first.Base)
	}
	f.put("second", "a.txt", "two")
	if got, other := f.read("second", "a.txt"), f.read("first", "a.txt"); got != "two" || other != "one" {
		t.Fatalf("second holds %q, first %q", got, other)
	}
}

// TestUnnamedFolderCreates opens concurrent checkouts with registry-chosen names,
// each its own, and leaves no files behind for prepared checkouts nobody took
// once the registry closes.
func TestUnnamedFolderCreates(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := reuseRepo(t)
	const n = 12
	replies := make([]api.NewReply, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			replies[i], errs[i] = f.reg.New(context.Background(), api.NewArgs{Dir: dir})
		})
	}
	wg.Wait()
	names := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		name := replies[i].Checkout.Name
		if names[name] {
			t.Fatalf("two creates got checkout %s", name)
		}
		names[name] = true
		f.put(name, "a.txt", name)
	}
	for name := range names {
		if got := f.read(name, "a.txt"); got != name {
			t.Fatalf("%s holds %q: checkouts share files", name, got)
		}
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: replies[0].Checkout.Name}); err != nil {
		t.Fatal(err)
	}
	delete(names, replies[0].Checkout.Name)

	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	var live []string
	for _, c := range f.ls().Checkouts {
		live = append(live, c.Name)
	}
	var want []string
	for name := range names {
		want = append(want, name)
	}
	slices.Sort(want)
	if !slices.Equal(live, want) {
		t.Fatalf("after reopen checkouts %v, want %v", live, want)
	}
	// No create ran since the reopen, so nothing is prepared: every directory left
	// belongs to a live checkout.
	for _, kind := range []string{"uppers", "git"} {
		entries, err := os.ReadDir(filepath.Join(f.state, kind))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(want) {
			t.Fatalf("%s holds %d directories for %d live checkouts", kind, len(entries), len(want))
		}
	}
	for name := range names {
		if got := f.read(name, "a.txt"); got != name {
			t.Fatalf("%s after reopen holds %q", name, got)
		}
	}
}
