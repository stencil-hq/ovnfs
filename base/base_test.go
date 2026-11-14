package base

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// repository creates a hermetic checkout with tracked sources and ignored roots.
func repository(t *testing.T) (*git.Repo, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(dir, "source.txt"), "fork source\n")
	writeTestFile(t, filepath.Join(dir, "src", "nested.txt"), "nested source\n")
	writeTestFile(t, filepath.Join(dir, ".gitignore"), "target/\nnode_modules/\ncache/\n")
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "-qm", "initial")
	repo, err := git.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, dir
}

// runGit runs fixture Git commands with actionable diagnostics.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

// writeTestFile creates a fixture entry and all of its parent directories.
func writeTestFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// createTestBase separates ctime boundaries before capturing a fixture with the built-in rules.
func createTestBase(t *testing.T, repo *git.Repo, configure func(*Config, *source.Options)) (*Base, Config) {
	t.Helper()
	time.Sleep(10 * time.Millisecond)
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_test", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	o := source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}
	if configure != nil {
		configure(&c, &o)
	}
	b, _, err := Create(context.Background(), c, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	time.Sleep(10 * time.Millisecond)
	return b, c
}

// prepend adds a rule line above the built-ins.
func prepend(t *testing.T, o *source.Options, line string) {
	t.Helper()
	rule, err := rules.ParseLine(line, "test")
	if err != nil {
		t.Fatal(err)
	}
	o.Rules.Rules = append([]rules.Rule{rule}, o.Rules.Rules...)
}

// readBase reads through the production bottom interface rather than the donor.
func readBase(t *testing.T, b *Base, p string) string {
	t.Helper()
	f, err := b.Bottom(time.Time{}).Open(p)
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

// TestSourceFallbackPreservesCapturedMtime covers modified and deleted source fallback.
func TestSourceFallbackPreservesCapturedMtime(t *testing.T) {
	repo, dir := repository(t)
	captured := time.Unix(1_700_000_000, 123456789)
	if err := os.Chtimes(filepath.Join(dir, "source.txt"), captured, captured); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filepath.Join(dir, "source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := createTestBase(t, repo, nil)
	writeTestFile(t, filepath.Join(dir, "source.txt"), "donor changed\n")
	if got := readBase(t, b, "source.txt"); got != "fork source\n" {
		t.Fatalf("source = %q", got)
	}
	info, err := b.Bottom(time.Time{}).Lstat("source.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(original.ModTime()) {
		t.Fatalf("mtime = %s, want %s", info.ModTime(), original.ModTime())
	}
	if err := os.Remove(filepath.Join(dir, "src", "nested.txt")); err != nil {
		t.Fatal(err)
	}
	if got := readBase(t, b, "src/nested.txt"); got != "nested source\n" {
		t.Fatalf("deleted source = %q", got)
	}
	if _, err := b.Bottom(time.Time{}).Lstat(".git"); !os.IsNotExist(err) {
		t.Fatalf(".git visible: %v", err)
	}
}

// TestLiveRootsFollowTheFolder serves cache and local roots from the folder as
// it is now, while new source paths stay out and symlinked ancestors inside a
// live root cannot lead outside the folder.
func TestLiveRootsFollowTheFolder(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=1")
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "leak"), "outside")
	b, _ := createTestBase(t, repo, nil)
	if got := readBase(t, b, "node_modules/pkg/index.js"); got != "v1" {
		t.Fatalf("cache file = %q", got)
	}
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v2")
	writeTestFile(t, filepath.Join(dir, "node_modules", "added", "x.js"), "added")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=2")
	writeTestFile(t, filepath.Join(dir, "new-source"), "new source")
	if err := os.Symlink(outside, filepath.Join(dir, "node_modules", "escape")); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"node_modules/pkg/index.js": "v2", "node_modules/added/x.js": "added", ".env": "SECRET=2"} {
		if got := readBase(t, b, p); got != want {
			t.Errorf("%s = %q, want the folder's current %q", p, got, want)
		}
	}
	if _, err := b.Bottom(time.Time{}).Lstat("new-source"); !os.IsNotExist(err) {
		t.Errorf("new source path visible: %v", err)
	}
	if _, err := b.Bottom(time.Time{}).Lstat("node_modules/escape/leak"); err == nil {
		t.Error("a symlink inside a live root led outside the folder")
	}
	entries, err := b.Bottom(time.Time{}).ReadDir("node_modules")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "added escape pkg" {
		t.Fatalf("live listing = %v", names)
	}
	if err := os.RemoveAll(filepath.Join(dir, "node_modules", "pkg")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Bottom(time.Time{}).Lstat("node_modules/pkg/index.js"); !os.IsNotExist(err) {
		t.Fatalf("deleted cache file still visible: %v", err)
	}
}

// TestFencedCacheHidesLaterFolderChanges serves a checkout's cache paths as the
// folder held them when it opened: files the folder rewrites or adds later are
// absent (a build rebuilds them rather than trusting the folder's newer
// output, #58), untouched ones and directories stay, and local paths follow
// the folder.
func TestFencedCacheHidesLaterFolderChanges(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "node_modules", "kept", "a.js"), "kept")
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v1")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=1")
	b, _ := createTestBase(t, repo, nil)
	view := b.Bottom(time.Now())
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "index.js"), "v2")
	writeTestFile(t, filepath.Join(dir, "node_modules", "pkg", "added.js"), "added")
	writeTestFile(t, filepath.Join(dir, ".env"), "SECRET=2")
	for _, p := range []string{"node_modules/pkg/index.js", "node_modules/pkg/added.js"} {
		if _, err := view.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s changed after the open is visible: %v", p, err)
		}
	}
	entries, err := view.ReadDir("node_modules/pkg")
	if err != nil || len(entries) != 0 {
		t.Errorf("listing after the folder rewrote pkg = %d entries, %v; want none", len(entries), err)
	}
	for p, want := range map[string]string{"node_modules/kept/a.js": "kept", ".env": "SECRET=2"} {
		f, err := view.Open(p)
		if err != nil {
			t.Fatalf("open %s: %v", p, err)
		}
		got, err := io.ReadAll(f)
		f.Close()
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", p, got, err, want)
		}
	}
	if got := readBase(t, b, "node_modules/pkg/index.js"); got != "v2" {
		t.Errorf("unfenced view = %q, want the folder's current v2", got)
	}
}

// TestLiveLinksIntoTheFolderAreRelative serves a live link's absolute target
// inside the folder relative to the link, so a checkout's link names the
// checkout's copy; targets outside the folder and relative ones stay as stored.
func TestLiveLinksIntoTheFolderAreRelative(t *testing.T) {
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
	for link, want := range map[string]string{
		"node_modules/@x/src":  "../../src",
		"node_modules/root":    "..",
		"node_modules/escape":  filepath.ToSlash(outside),
		"node_modules/sibling": "pkg",
	} {
		if got, err := b.Bottom(time.Time{}).Readlink(link); err != nil || got != want {
			t.Errorf("readlink %s = %q, %v; want %q", link, got, err, want)
		}
	}
}

// TestForkTimeUsesUTC verifies creation and restart normalize timestamps without changing instants.
func TestForkTimeUsesUTC(t *testing.T) {
	repo, _ := repository(t)
	b, c := createTestBase(t, repo, nil)
	created := b.Meta().ForkTime
	if created.Location() != time.UTC {
		t.Fatalf("created fork location = %v, want UTC", created.Location())
	}
	m := b.Meta()
	m.ForkTime = m.ForkTime.In(time.FixedZone("fixture", 2*60*60))
	if err := b.Close(); err != nil {
		t.Fatalf("close before restart = %v, want nil", err)
	}
	reopened, err := Open(c, m)
	if err != nil {
		t.Fatalf("reopen error = %v, want nil", err)
	}
	defer reopened.Close()
	got := reopened.Meta().ForkTime
	if got.Location() != time.UTC || !got.Equal(created) {
		t.Fatalf("reopened fork = %v (%v), want %v (UTC)", got, got.Location(), created)
	}
	encoded, err := got.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal fork error = %v, want nil", err)
	}
	if !strings.HasSuffix(string(encoded), "Z\"") {
		t.Fatalf("serialized fork = %s, want UTC Z suffix", encoded)
	}
}

// TestCapturedNamesOutliveTheFolder keeps listing and reading captured source
// files after the folder deletes them, from the captured blobs.
func TestCapturedNamesOutliveTheFolder(t *testing.T) {
	repo, dir := repository(t)
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		writeTestFile(t, filepath.Join(dir, "built", name), name)
	}
	b, _ := createTestBase(t, repo, nil)
	entries, err := b.Bottom(time.Time{}).ReadDir("built")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 8 {
		t.Fatalf("entries = %d", len(entries))
	}
	if err := os.RemoveAll(filepath.Join(dir, "built")); err != nil {
		t.Fatal(err)
	}
	if entries, err = b.Bottom(time.Time{}).ReadDir("built"); err != nil || len(entries) != 8 {
		t.Fatalf("listing after the folder deleted it = %d entries, %v", len(entries), err)
	}
	for _, entry := range entries {
		if got := readBase(t, b, "built/"+entry.Name()); got != entry.Name() {
			t.Fatalf("%s = %q", entry.Name(), got)
		}
	}
}

// TestReadDirPreservesCapturedMetadata exercises bulk metadata after host mutations.
func TestReadDirPreservesCapturedMetadata(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "cache", "old"), "old")
	b, _ := createTestBase(t, repo, nil)
	check := func(p string, names []string) {
		t.Helper()
		entries, err := b.Bottom(time.Time{}).ReadDir(p)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, entry := range entries {
			got = append(got, entry.Name())
			info, err := b.Bottom(time.Time{}).Lstat(filepath.ToSlash(filepath.Join(p, entry.Name())))
			if err != nil {
				t.Fatal(err)
			}
			// NTFS refreshes the copy of a directory's timestamps kept in its
			// parent's index lazily, so a native listing (os.ReadDir too) may
			// report an older directory mtime than a stat of the directory.
			staleDirTime := runtime.GOOS == "windows" && entry.IsDir()
			if entry.Mode() != info.Mode() || entry.Size() != info.Size() || !staleDirTime && !entry.ModTime().Equal(info.ModTime()) {
				t.Errorf("%s/%s: listing metadata differs from stat: %v, %v", p, entry.Name(), entry, info)
			}
		}
		if !reflect.DeepEqual(got, names) {
			t.Fatalf("%s: got %v, want %v", p, got, names)
		}
	}
	check("", []string{".gitignore", "cache", "source.txt", "src"})
	writeTestFile(t, filepath.Join(dir, "source.txt"), "different source bytes")
	writeTestFile(t, filepath.Join(dir, "new-source"), "not captured")
	writeTestFile(t, filepath.Join(dir, "cache", "new"), "live")
	if err := os.RemoveAll(filepath.Join(dir, "src")); err != nil {
		t.Fatal(err)
	}
	check("", []string{".gitignore", "cache", "source.txt", "src"})
	check("src", []string{"nested.txt"})
	check("cache", []string{"new", "old"})
}

// TestCaptureKeepsOriginalIndex verifies staged state, dirty content and carried files.
func TestCaptureKeepsOriginalIndex(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "source.txt"), "staged\n")
	runGit(t, dir, "add", "source.txt")
	writeTestFile(t, filepath.Join(dir, "source.txt"), "dirty\n")
	writeTestFile(t, filepath.Join(dir, "cache", "secret"), "carried")
	writeTestFile(t, filepath.Join(dir, "untracked"), "untracked")
	original, err := os.ReadFile(filepath.Join(repo.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := createTestBase(t, repo, func(c *Config, o *source.Options) { prepend(t, o, "source cache/secret") })
	saved, err := os.ReadFile(b.Repos()[0].Index)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved, original) {
		t.Fatal("saved index differs from fork-time user index")
	}
	current, err := os.ReadFile(filepath.Join(repo.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, original) {
		t.Fatal("capture modified the user's index")
	}
	writeTestFile(t, filepath.Join(dir, "source.txt"), "after fork")
	if got := readBase(t, b, "source.txt"); got != "dirty\n" {
		t.Fatalf("dirty source = %q", got)
	}
	if got := readBase(t, b, "cache/secret"); got != "carried" {
		t.Fatalf("carry = %q", got)
	}
	if got := readBase(t, b, "untracked"); got != "untracked" {
		t.Fatalf("untracked = %q", got)
	}
	if m, head := b.Meta(), b.Repos()[0].Head; m.Tree == "" || head == "" {
		t.Fatalf("capture metadata missing: %+v head %q", m, head)
	}
}

// TestNestedRepositoryIsFrozenAsSource reads a submodule's unchanged tracked files from its own objects after the donor changes.
func TestNestedRepositoryIsFrozenAsSource(t *testing.T) {
	repo, dir := repository(t)
	_, submodule := repository(t)
	runGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", submodule, "vendor")
	runGit(t, dir, "commit", "-qm", "submodule")
	b, _ := createTestBase(t, repo, nil)
	writeTestFile(t, filepath.Join(dir, "vendor", "source.txt"), "changed submodule")
	if got := readBase(t, b, "vendor/source.txt"); got != "fork source\n" {
		t.Fatalf("gitlink content = %q", got)
	}
}

// TestAncestorReplacementCannotSubstituteOldInodes rejects directory-rename bypasses.
func TestAncestorReplacementCannotSubstituteOldInodes(t *testing.T) {
	repo, dir := repository(t)
	replacement := t.TempDir()
	writeTestFile(t, filepath.Join(replacement, "nested.txt"), "foreign old source")
	b, _ := createTestBase(t, repo, nil)
	if _, err := b.Bottom(time.Time{}).Lstat("src"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "src"), filepath.Join(dir, "old-src")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(dir, "src")); err != nil {
		t.Fatal(err)
	}
	if got := readBase(t, b, "src/nested.txt"); got != "nested source\n" {
		t.Fatalf("substituted source = %q", got)
	}
}

// TestCapturedSymlinkAndMode verifies blob fallback retains the Git file type.
func TestCapturedSymlinkAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows privileges")
	}
	repo, dir := repository(t)
	if err := os.Symlink("source.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "link")
	runGit(t, dir, "commit", "-qm", "symlink")
	b, _ := createTestBase(t, repo, nil)
	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	target, err := b.Bottom(time.Time{}).Readlink("link")
	if err != nil {
		t.Fatal(err)
	}
	if target != "source.txt" {
		t.Fatalf("link = %q", target)
	}
	info, err := b.Bottom(time.Time{}).Lstat("link")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink mode lost")
	}
}

// TestOpenMigratesCaptureFiles reopens a base from its capture index, and one
// created before capture indexes from its JSON capture file, which it replaces
// by an index; each serves the capture it was created with, and a damaged
// index is refused.
func TestOpenMigratesCaptureFiles(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "dir", "ü 日本.txt"), "unicode\n")
	writeTestFile(t, filepath.Join(dir, "target", "out.bin"), "built\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_test", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	res, _, err := Capture(context.Background(), c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateCaptured(c, res, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	meta := b.Meta()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if len(res.Roots) == 0 || len(res.Info) == 0 {
		t.Fatalf("fixture captured no roots or stat data: %+v", res)
	}
	reopen := func(name string) {
		t.Helper()
		b, err := Open(c, meta)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer b.Close()
		if !reflect.DeepEqual(b.Repos(), res.Repos) || !reflect.DeepEqual(b.Roots(), res.Roots) {
			t.Fatalf("%s: repos %+v roots %v, want %+v %v", name, b.Repos(), b.Roots(), res.Repos, res.Roots)
		}
		for p, f := range res.Files {
			if got, ok := b.Leaf(p); !ok || got != f {
				t.Fatalf("%s: leaf %q = %+v, want %+v", name, p, got, f)
			}
		}
		for p, i := range res.Info {
			if g, ok := b.Info(p); !ok || !g.Mtime.Equal(i.Mtime) || g.Size != i.Size || g.Ino != i.Ino {
				t.Fatalf("%s: info %q = %+v, want %+v", name, p, g, i)
			}
		}
		for p, want := range map[string]bool{"src": true, "src/nested.txt": true, "dir": false, "sr": false, "src/n": false} {
			if got := b.Domains().Facts(source.Dir(dir), p, nil).Tracked(); got != want {
				t.Fatalf("%s: %q tracked = %v", name, p, got)
			}
		}
		if got := readBase(t, b, "dir/ü 日本.txt"); got != "unicode\n" {
			t.Fatalf("%s: read %q", name, got)
		}
	}
	reopen("index")

	legacy, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.StateDir, jsonCaptureFile), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.StateDir, captureIndexFile)); err != nil {
		t.Fatal(err)
	}
	reopen("migrated")
	if _, err := os.Stat(filepath.Join(c.StateDir, jsonCaptureFile)); !os.IsNotExist(err) {
		t.Fatalf("migration left the capture file: %v", err)
	}
	reopen("after migration")

	if err := os.WriteFile(filepath.Join(c.StateDir, captureIndexFile), []byte(captureMagic+"truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := Open(c, meta); err == nil {
		b.Close()
		t.Fatal("opened a base whose capture index is damaged")
	}
}
