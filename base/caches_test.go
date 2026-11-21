package base

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/overlay"
)

// read returns the content of p in view.
func read(t *testing.T, view overlay.Bottom, p string) string {
	t.Helper()
	f, err := view.Open(p)
	if err != nil {
		t.Fatalf("open %s: %v", p, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// names lists directory p of view.
func names(t *testing.T, view overlay.Bottom, p string) []string {
	t.Helper()
	entries, err := view.ReadDir(p)
	if err != nil {
		t.Fatalf("readdir %s: %v", p, err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// gone fails the test unless view shows nothing at each of paths.
func gone(t *testing.T, view overlay.Bottom, why string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := view.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s: %s is in the view (%v)", why, p, err)
		}
	}
}

// TestCacheRootsAreFenced serves a cache root from the folder without copying
// it and fences it at the snapshot: an entry the folder rewrites or removes
// afterwards leaves the view instead of changing in it, nothing the folder
// adds appears, what it left alone stays, and a symlink planted in the folder
// cannot lead the view outside it. A local root is the snapshot's copy
// throughout, and a later snapshot shows the folder as it is then.
func TestCacheRootsAreFenced(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	writeTestFile(t, filepath.Join(dir, "node_modules", "kept", "k.js"), "kept")
	writeTestFile(t, filepath.Join(dir, "node_modules", "gone", "a.js"), "gone")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=1")
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "leak"), "outside")
	b, _ := createTestBase(t, repo, nil)
	first := b.TakeCaches("c1")
	first.Wait()
	old := b.Bottom(first)
	if _, err := os.Lstat(filepath.Join(first.tree, "node_modules")); !os.IsNotExist(err) {
		t.Errorf("the snapshot holds a copy of the cache root: %v", err)
	}
	for p, want := range map[string]string{"node_modules/pkg/index.js": "v1", "node_modules/kept/k.js": "kept", ".env": "SECRET=1"} {
		if got := read(t, old, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}

	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v2")
	// As an installer extracting an archive does: the mtime says nothing.
	long := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "node_modules", "pkg", "index.js"), long, long); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "node_modules", "added", "x.js"), "added")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=2")
	writeTestFile(t, filepath.Join(dir, "new-source"), "new source")
	if err := os.RemoveAll(filepath.Join(dir, "node_modules", "gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "node_modules", "escape")); err != nil {
		t.Fatal(err)
	}

	for p, want := range map[string]string{"node_modules/kept/k.js": "kept", ".env": "SECRET=1"} {
		if got := read(t, old, p); got != want {
			t.Errorf("%s = %q after the folder changed other entries, want %q", p, got, want)
		}
	}
	gone(t, old, "rewritten, removed or added after the fence",
		"node_modules/pkg/index.js", "node_modules/gone", "node_modules/gone/a.js", "node_modules/added/x.js",
		"node_modules/escape", "node_modules/escape/leak", "new-source")
	if got := names(t, old, "node_modules"); !slices.Contains(got, "kept") || !slices.Contains(got, "pkg") || slices.Contains(got, "gone") || slices.Contains(got, "escape") {
		t.Errorf("fenced listing = %v, want kept and pkg without gone and escape", got)
	}
	if got := names(t, old, "node_modules/pkg"); len(got) != 0 {
		t.Errorf("the rewritten file is listed: %v", got)
	}
	added := filepath.Join(dir, "node_modules", "added")
	if info, err := os.Lstat(added); err != nil {
		t.Fatal(err)
	} else if _, known := birthTime(added, info); known {
		gone(t, old, "a directory made after the fence", "node_modules/added")
	}

	second := b.TakeCaches("c2")
	view := b.Bottom(second)
	for p, want := range map[string]string{"node_modules/pkg/index.js": "v2", "node_modules/added/x.js": "added", ".env": "SECRET=2"} {
		if got := read(t, view, p); got != want {
			t.Errorf("newer snapshot: %s = %q, want %q", p, got, want)
		}
	}
	gone(t, view, "newer snapshot", "node_modules/gone/a.js")
	if _, err := view.Lstat("node_modules/escape/leak"); err == nil {
		t.Error("a symlink inside a cache root led outside the folder")
	}
	gone(t, old, "after a newer snapshot", "node_modules/pkg/index.js", "node_modules/added/x.js")
}

// TestFencedFileNeverChangesUnderAReader fails the reads of an open cache
// file after a same-size rewrite with a restored mtime. A replacement can
// invalidate the old descriptor but can never feed it new bytes.
func TestFencedFileNeverChangesUnderAReader(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "target", "rewritten"), "old content")
	writeTestFile(t, filepath.Join(dir, "target", "replaced"), "old content")
	b, _ := createTestBase(t, repo, nil)
	view := b.Bottom(b.TakeCaches("c1"))
	rewritten, err := view.Open("target/rewritten")
	if err != nil {
		t.Fatal(err)
	}
	defer rewritten.Close()
	replaced, err := view.Open("target/replaced")
	if err != nil {
		t.Fatal(err)
	}
	defer replaced.Close()

	p := filepath.Join(dir, "target", "rewritten")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, p, "new content")
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "target", "replaced.tmp"), "new")
	if err := hostfile.Rename(filepath.Join(dir, "target", "replaced.tmp"), filepath.Join(dir, "target", "replaced")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 64)
	if n, err := rewritten.ReadAt(buf, 0); !errors.Is(err, syscall.ESTALE) {
		t.Errorf("read of a file rewritten in place = %q, %v; want ESTALE", buf[:n], err)
	}
	if n, err := replaced.ReadAt(buf, 0); !errors.Is(err, syscall.ESTALE) && (string(buf[:n]) != "old content" || (err != nil && err != io.EOF)) {
		t.Errorf("read of a replaced file = %q, %v; want the opened bytes or ESTALE", buf[:n], err)
	}
	gone(t, view, "changed after the fence", "target/rewritten", "target/replaced", "target/replaced.tmp")
}

// TestCacheRootsOnAnotherVolume starts cache roots empty and copies local
// roots where the folder is not on the state's volume, whose inodes are the
// only ones a view can name.
func TestCacheRootsOnAnotherVolume(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=1")
	b, _ := createTestBase(t, repo, nil)
	c := b.newCaches("c1")
	c.fence = fence{}
	b.serve(c)
	go c.take(b)
	c.Wait()
	view := b.Bottom(c)
	if info, err := view.Lstat("node_modules"); err != nil || !info.IsDir() {
		t.Errorf("cache root = %v, %v; want an empty directory", info, err)
	}
	if got := names(t, view, "node_modules"); len(got) != 0 {
		t.Errorf("cache root = %v, want empty", got)
	}
	gone(t, view, "another volume", "node_modules/pkg", "node_modules/pkg/index.js")
	if got := read(t, view, ".env"); got != "SECRET=1" {
		t.Errorf("local root = %q, want a copy", got)
	}
}

// An incomplete snapshot lists even non-directory roots while every directory
// slot is busy. Either kind of shutdown releases its waiter without recording
// a completed tree; a later open must copy the directory, not serve it empty.
func TestLocalDirectoryTakeQueue(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, ".aws", "item"), "local")
	writeTestFile(t, filepath.Join(dir, ".env"), "local file")
	writeTestFile(t, filepath.Join(dir, "node_modules", "item"), "cache")

	b, config := createTestBase(t, repo, nil)
	other, _ := createTestBase(t, repo, nil)
	stop := make(chan struct{})
	other.config.StopTakes = stop
	for range cap(takeSlots) {
		takeSlots <- struct{}{}
	}
	release := sync.OnceFunc(func() {
		for range cap(takeSlots) {
			<-takeSlots
		}
	})
	defer release()

	promptly := func(what string, f func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- f() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s waited for a directory-copy slot", what)
		}
	}
	c, second := b.OpenCaches("c1"), other.OpenCaches("c2")
	promptly("parent listing", func() error {
		infos, err := b.Bottom(c).ReadDir("")
		if err != nil {
			return err
		}
		for _, name := range []string{".aws", ".env", "node_modules"} {
			if !slices.ContainsFunc(infos, func(i os.FileInfo) bool { return i.Name() == name }) {
				return fmt.Errorf("missing %s", name)
			}
		}
		return nil
	})
	promptly("second root listing", func() error { <-second.listed; return nil })
	promptly("base close", b.Close)
	close(stop)
	promptly("registry stop", func() error { second.Wait(); return nil })
	for _, take := range []*Caches{c, second} {
		if _, err := os.Stat(filepath.Join(take.dir, rootsFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("queued snapshot marked complete: %v", err)
		}
	}

	release()
	reopened, err := Open(config, b.Meta())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retaken := reopened.OpenCaches("c1")
	if got := read(t, reopened.Bottom(retaken), ".aws/item"); got != "local" {
		t.Errorf("retaken local directory: %q", got)
	}
	if _, err := os.Stat(filepath.Join(retaken.tree, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a fenced cache root was copied: %v", err)
	}
}

// TestCacheSnapshotsReopenAndRemove keeps a complete snapshot with its fence
// across a restart, takes an incomplete one again with a new fence, serves
// nothing of a local root once its base is closed and releases snapshots for
// removal on request.
func TestCacheSnapshotsReopenAndRemove(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=1")
	writeTestFile(t, filepath.Join(dir, "target", "out"), "one")
	first, config := createTestBase(t, repo, nil)
	first.TakeCaches("c1").Wait()
	first.TakeCaches("c2").Wait()
	if got := read(t, first.Bottom(first.OpenCaches("c1")), ".env"); got != "SECRET=1" {
		t.Errorf("snapshot served again = %q", got)
	}
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=2")
	writeTestFile(t, filepath.Join(dir, "target", "out"), "two")
	if err := os.Remove(filepath.Join(config.StateDir, cachesDir, "c2", rootsFile)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Bottom(first.OpenCaches("c1")).Lstat(".env"); !os.IsNotExist(err) {
		t.Errorf("a closed base's snapshot still serves: %v", err)
	}
	b, err := Open(config, first.Meta())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	reopened := b.Bottom(b.OpenCaches("c1"))
	if got := read(t, reopened, ".env"); got != "SECRET=1" {
		t.Errorf("reopened snapshot = %q, want its own SECRET=1", got)
	}
	gone(t, reopened, "reopened snapshot, rewritten after its fence", "target/out")
	retaken := b.Bottom(b.OpenCaches("c2"))
	if got := read(t, retaken, ".env"); got != "SECRET=2" {
		t.Errorf("reopened incomplete snapshot = %q, want it taken again", got)
	}
	if got := read(t, retaken, "target/out"); got != "two" {
		t.Errorf("reopened incomplete snapshot: target/out = %q, want the folder's at the new fence", got)
	}
	// Released, nothing of the snapshot is held open, so its directory can go.
	if err := os.RemoveAll(b.ReleaseCaches("c1")); err != nil {
		t.Fatal(err)
	}
	if ids, err := b.CachesIDs(); err != nil || !slices.Equal(ids, []string{"c2"}) {
		t.Fatalf("snapshots after removal = %v, %v", ids, err)
	}
}

// TestSnapshotWithoutFenceFileIsFencedWhenComplete reopens a snapshot of a
// build that cloned cache roots into it, which wrote no fence: the checkouts
// reading it see the folder's cache roots as of the snapshot's completion,
// not as of the restart.
func TestSnapshotWithoutFenceFileIsFencedWhenComplete(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "target", "kept"), "kept")
	writeTestFile(t, filepath.Join(dir, "target", "out"), "one")
	first, config := createTestBase(t, repo, nil)
	first.TakeCaches("c1").Wait()
	if err := os.Remove(filepath.Join(config.StateDir, cachesDir, "c1", fenceFile)); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "target", "out"), "two")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(config, first.Meta())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	view := b.Bottom(b.OpenCaches("c1"))
	if got := read(t, view, "target/kept"); got != "kept" {
		t.Errorf("target/kept = %q, want the folder's", got)
	}
	gone(t, view, "rewritten after the snapshot was complete", "target/out")
}

// TestBaseIdentities gives captured entries and roots their Capture identity,
// served from the folder or the capture alike, and the folder's entries below
// a cache root the State identity of their inode; Locate maps each back to
// its path.
func TestBaseIdentities(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, ".env"), "local")
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	b, _ := createTestBase(t, repo, nil)
	c := b.TakeCaches("c1")
	c.Wait()
	view := b.Bottom(c).(bottom)
	writeTestFile(t, filepath.Join(dir, "source.txt"), "changed, served from the capture")
	for _, p := range []string{"", "src", "src/nested.txt", "source.txt", "node_modules"} {
		info, err := view.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		id, ok := ident.Of(info)
		n, _ := b.index.lookup(p)
		if !ok || id != captureID(n) {
			t.Errorf("%q: identity %v, want Capture %d", p, id, n)
		}
		if got, ok := view.Locate(id); !ok || got != p {
			t.Errorf("locate %v = %q, %v; want %q", id, got, ok, p)
		}
	}
	for _, p := range []string{".env", "node_modules/pkg", "node_modules/pkg/index.js"} {
		// Query the ID from the listing first, as WinFsp does. It must
		// resolve before a client makes a separate lookup of the name.
		entries, err := view.ReadDir(path.Dir(p))
		if err != nil {
			t.Fatal(err)
		}
		name := path.Base(p)
		at := slices.IndexFunc(entries, func(info os.FileInfo) bool { return info.Name() == name })
		if at < 0 {
			t.Fatalf("missing %s in listing", p)
		}
		root := dir
		if p == ".env" {
			root = c.tree
		}
		host, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		_, ino, err := fileIdentity(host, root, p)
		if err != nil {
			t.Fatal(err)
		}
		if isNTFS(root) {
			ino = ident.NTFSRecord(ino)
		}
		want, fits := ident.New(ident.State, ino)
		listed, ok := ident.Of(entries[at])
		if ok != fits || listed != want {
			t.Fatalf("listed %s has identity %v, %v; want %v, %v", p, listed, ok, want, fits)
		}
		if fits {
			if got, ok := view.Locate(listed); !ok || got != p {
				t.Fatalf("listed %s resolves to %q, %v", p, got, ok)
			}
		}
		info, err := view.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if id, ok := ident.Of(info); ok != fits || id != want {
			t.Errorf("%s: lookup identity %v, %v; want %v, %v", p, id, ok, want, fits)
		}
	}
	hidden, _ := ident.New(ident.Capture, uint64(b.index.nodes))
	if _, ok := view.Locate(hidden); ok {
		t.Error("an identity beyond the capture was located")
	}
}

// TestLinksIntoTheFolderAreRelative serves the absolute target of a link
// below a cache root, when it lies inside the folder, relative to the link, so
// a checkout's link names the checkout's copy; targets outside the folder and
// relative ones stay as stored.
func TestLinksIntoTheFolderAreRelative(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	b, _ := createTestBase(t, repo, nil)
	outside := t.TempDir()
	for link, target := range map[string]string{
		"node_modules/@x/src":  filepath.Join(b.Meta().Dir, "src"),
		"node_modules/root":    b.Meta().Dir,
		"node_modules/escape":  outside,
		"node_modules/sibling": "pkg",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	view := b.Bottom(b.TakeCaches("c1"))
	for link, want := range map[string]string{
		"node_modules/@x/src":  "../../src",
		"node_modules/root":    "..",
		"node_modules/escape":  filepath.ToSlash(outside),
		"node_modules/sibling": "pkg",
	} {
		if got, err := view.Readlink(link); err != nil || got != want {
			t.Errorf("readlink %s = %q, %v; want %q", link, got, err, want)
		}
	}
}

// TestCachesAffectedBy marks a snapshot stale only for changes at, below or
// above a cache or local root.
func TestCachesAffectedBy(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "a", "node_modules", "x.js"), "x")
	b, _ := createTestBase(t, repo, nil)
	for p, want := range map[string]bool{"": true, "a": true, "a/node_modules": true, "a/node_modules/x.js": true, "source.txt": false, "src/nested.txt": false, "a/b": false} {
		if got := b.CachesAffectedBy(p); got != want {
			t.Errorf("CachesAffectedBy(%q) = %v, want %v", p, got, want)
		}
	}
}

// BenchmarkCacheRootReadDir compares the cost of listing the same dependency
// directory with native metadata alone and through the checkout's cache fence.
func BenchmarkCacheRootReadDir(b *testing.B) {
	repo, dir := repository(b)
	const path = "target/debug/deps"
	const count = 1024
	dir = filepath.Join(dir, filepath.FromSlash(path))
	for i := range count {
		writeTestFile(b, filepath.Join(dir, fmt.Sprintf("libdependency-%05d.rmeta", i)), "x")
	}
	base, _ := createTestBase(b, repo, nil)
	cache := base.TakeCaches("c1")
	cache.Wait()
	view := base.Bottom(cache)
	b.Run("native", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			items, err := fsys.ReadDir(dir)
			if err != nil || len(items) != count {
				b.Fatalf("native listing: %d entries, %v", len(items), err)
			}
		}
	})
	b.Run("fenced", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			items, err := view.ReadDir(path)
			if err != nil || len(items) != count {
				b.Fatalf("fenced listing: %d entries, %v", len(items), err)
			}
		}
	})
}

// BenchmarkCacheRootLstat measures Lstat of one entry below a cache root, in
// a 30,000-entry folder directory, as NFS LOOKUP and GETATTR issue it.
func BenchmarkCacheRootLstat(b *testing.B) {
	repo, dir := repository(b)
	const entries = 30000
	names := make([]string, entries)
	for i := range names {
		names[i] = fmt.Sprintf("target/debug/deps/f%05d", i)
		writeTestFile(b, filepath.Join(dir, filepath.FromSlash(names[i])), "x")
	}
	base, _ := createTestBase(b, repo, nil)
	view := base.Bottom(base.TakeCaches("c1"))
	for _, p := range names {
		if _, err := view.Lstat(p); err != nil {
			b.Fatal(err)
		}
	}
	i := 0
	for b.Loop() {
		if _, err := view.Lstat(names[i%entries]); err != nil {
			b.Fatal(err)
		}
		i++
	}
}
