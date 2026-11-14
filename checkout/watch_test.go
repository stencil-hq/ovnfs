package checkout

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stencil-hq/ovnfs/watch"
)

// TestFolderRescanInvalidatesOnlyItsTree keeps a rescan deep in a cache
// directory to that subtree: widening it to the whole checkout rechecks every
// handle the checkout ever issued.
func TestFolderRescanInvalidatesOnlyItsTree(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"src/main": "one", "target/debug/out": "one"})
	f.newCheckout(dir, "app", "cache dir & target")
	f.reg.mu.RLock()
	root := f.reg.checkouts["app"].base.view.Meta().Dir
	f.reg.mu.RUnlock()

	n := len(f.invalidations())
	f.reg.folderChanged([]watch.Change{{Path: filepath.Join(root, "target", "debug"), Tree: true}})
	got := f.invalidations()[n:]
	if len(got) != 1 || got[0].whole() || !slices.Equal(got[0].trees, []string{"target/debug"}) {
		t.Fatalf("invalidations = %+v, want only the tree target/debug", got)
	}

	f.reg.folderChanged([]watch.Change{{Path: root, Tree: true}})
	if got := f.invalidations()[n+1:]; len(got) != 1 || !got[0].whole() {
		t.Fatalf("invalidations = %+v, want the folder root to invalidate the whole checkout", got)
	}
}

// TestFolderEditsInvalidateLivePaths recalls clients' caches of cache paths
// edited in the source folder, which the checkout then no longer serves (they
// changed after it opened), while captured source stays pinned and is neither
// invalidated nor ever untracked.
func TestFolderEditsInvalidateLivePaths(t *testing.T) {
	f := newFixture(t)
	if f.reg.watcher == nil {
		t.Skip("no folder events on this platform")
	}
	dir := folder(t, map[string]string{"src/main": "one", "target/out": "one"})
	f.newCheckout(dir, "app", "cache dir & target")
	if !f.reg.Tracked("app", "src/main") {
		t.Fatal("captured source must be tracked: the folder cannot change it")
	}
	eventually(t, "the watcher never covered the folder", func() bool { return f.reg.Tracked("app", "target/out") })
	// Source first: had it been invalidated, that event would not arrive after target's.
	write(t, filepath.Join(dir, "src/main"), "two")
	write(t, filepath.Join(dir, "target/out"), "two")
	eventually(t, "a folder edit to a cache path was not invalidated", func() bool { return f.invalidatedPath("app", "target/out") })
	if f.invalidatedPath("app", "src/main") {
		t.Fatal("a folder edit to captured source was invalidated")
	}
	if got := f.read("app", "target/out"); got != "<absent>" {
		t.Fatalf("cache path the folder changed after the open = %q, want absent", got)
	}
	if got := f.read("app", "src/main"); got != "one" {
		t.Fatalf("captured source = %q", got)
	}
}
