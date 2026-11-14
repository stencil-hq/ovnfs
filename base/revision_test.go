package base

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// TestRevisionIndexCarriesPresentedStat seeds the --rev index with exactly the
// size and mtime the view presents for REV's content, so git trusts those files
// unread, while taken paths, gitlinks and racily clean files are still read.
func TestRevisionIndexCarriesPresentedStat(t *testing.T) {
	ctx := context.Background()
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "rev-only.txt"), "revision\n")
	writeTestFile(t, filepath.Join(dir, "taken.txt"), "committed\n")
	writeTestFile(t, filepath.Join(dir, "future.txt"), "future\n")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("source.txt", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+runGit(t, dir, "rev-parse", "HEAD")+",sub")
	runGit(t, dir, "commit", "-qm", "revision")
	revision, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// The folder then drifts from REV: one file differs, one is staged for a
	// take, and one carries an mtime later than any index write.
	writeTestFile(t, filepath.Join(dir, "rev-only.txt"), "folder edit\n")
	writeTestFile(t, filepath.Join(dir, "taken.txt"), "staged taken\n")
	runGit(t, dir, "add", "taken.txt")
	past := time.Unix(1_700_000_000, 123456789)
	for _, p := range []string{"source.txt", "src/nested.txt"} {
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(p)), past, past); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "future.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_rev", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	before := time.Now()
	b, _, err := CreateRevision(ctx, c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, revision, Take{Path: "taken.txt"})
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	name := filepath.Join(c.StateDir, "revision-index")
	idx, err := repo.ReadIndex(name)
	if err != nil {
		t.Fatal(err)
	}
	var seeded []string
	for _, e := range idx.Entries {
		zero := e.Size == 0 && e.Mtime.Equal(time.Unix(0, 0))
		if e.Path == "taken.txt" || e.Path == "sub" {
			if !zero {
				t.Errorf("%s: stat %d %s, want zero: its content is not REV's", e.Path, e.Size, e.Mtime)
			}
			continue
		}
		info, err := b.Bottom(time.Time{}).Lstat(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		mtime := info.ModTime()
		switch {
		case !e.Mtime.Equal(mtime):
			t.Errorf("%s: seeded mtime %s, presented %s", e.Path, e.Mtime, mtime)
		case mtime.Unix() < before.Unix() && int64(e.Size) != info.Size():
			t.Errorf("%s: seeded size %d, presented %d", e.Path, e.Size, info.Size())
		case mtime.Unix() > after.Unix() && e.Size != 0:
			t.Errorf("%s: racily clean entry keeps size %d", e.Path, e.Size)
		case e.Size != 0 && int64(e.Size) != info.Size():
			t.Errorf("%s: seeded size %d, presented %d", e.Path, e.Size, info.Size())
		}
		seeded = append(seeded, e.Path)
	}
	want := []string{".gitignore", "future.txt", "rev-only.txt", "source.txt", "src/nested.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "link")
		slices.Sort(want)
	}
	if !slices.Equal(seeded, want) {
		t.Fatalf("seeded %v, want %v", seeded, want)
	}
	for _, e := range idx.Entries {
		if fork := b.Meta().ForkTime; e.Path == "rev-only.txt" && !e.Mtime.Equal(fork) {
			t.Fatalf("content the folder never held is seeded with %s, not the fork time %s", e.Mtime, fork)
		}
	}

	// Git trusts the seeded stat data without reading: a same-size edit that
	// keeps the presented mtime goes unnoticed, while racily clean and taken
	// entries are read and compared.
	work := t.TempDir()
	for _, e := range idx.Entries {
		host := filepath.Join(work, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			t.Fatal(err)
		}
		switch e.Path {
		case "sub":
			if err := os.Mkdir(host, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		case "link":
			if err := os.Symlink("source.txt", host); err != nil {
				t.Fatal(err)
			}
			continue
		}
		content := readBase(t, b, e.Path)
		if e.Path == "source.txt" || e.Path == "future.txt" {
			content = strings.ToUpper(content)
		}
		writeTestFile(t, host, content)
		info, err := b.Bottom(time.Time{}).Lstat(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(host, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "--git-dir", filepath.Join(dir, ".git"), "--work-tree", work,
		"-c", "core.checkStat=minimal", "-c", "core.trustctime=false", "status", "--porcelain")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_INDEX_FILE="+name, "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if got := strings.TrimRight(string(out), "\n"); got != " M future.txt\n M taken.txt" {
		t.Fatalf("git status = %q, want only the racily clean and taken files read and changed", got)
	}
}

// TestRevisionHiddenRootsStayHidden keeps a hidden root absent from a --rev
// checkout, where ignored content otherwise stays live: built-in hidden names
// and a `hidden tmp/` scratch tree alike. REV's tracked files below a hidden
// root still appear, without the ignored remainder.
func TestRevisionHiddenRootsStayHidden(t *testing.T) {
	ctx := context.Background()
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, ".gitignore"), "tmp/\n.DS_Store\n*.log\n")
	writeTestFile(t, filepath.Join(dir, "vendor", "lib.txt"), "vendored\n")
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "-qm", "ignores")
	revision, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{
		"tmp/copy/repo/file.txt": "scratch copy\n",
		".DS_Store":              "finder\n",
		"src/.DS_Store":          "finder\n",
		"notes/.DS_Store":        "finder\n",
		"vendor/build.log":       "ignored log\n",
		"vendor/new.txt":         "untracked\n",
	} {
		writeTestFile(t, filepath.Join(dir, filepath.FromSlash(p)), content)
	}
	always := []string{".DS_Store", "src/.DS_Store", "notes", "vendor/new.txt"}
	for _, tc := range []struct {
		name   string
		rules  []string
		takes  []Take
		absent []string
		live   map[string]string
	}{
		{name: "builtins", live: map[string]string{"tmp/copy/repo/file.txt": "scratch copy\n", "vendor/build.log": "ignored log\n"}},
		{name: "hidden", rules: []string{"hidden tmp/", "hidden vendor/"}, absent: []string{"tmp", "vendor/build.log"}},
		{name: "hidden with take", rules: []string{"hidden tmp/", "hidden vendor/"}, takes: []Take{{Source: ".", Path: "src"}}, absent: []string{"tmp", "vendor/build.log"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{ID: "b_rev", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
			o := source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}
			for _, line := range tc.rules {
				prepend(t, &o, line)
			}
			b, _, err := CreateRevision(ctx, c, o, revision, tc.takes...)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			for _, p := range append(slices.Clone(always), tc.absent...) {
				if info, err := b.Bottom(time.Time{}).Lstat(p); !os.IsNotExist(err) {
					t.Errorf("%s = %v, %v; want absent", p, info, err)
				}
			}
			for p, want := range tc.live {
				if got := readBase(t, b, p); got != want {
					t.Errorf("%s = %q, want %q", p, got, want)
				}
			}
			if got := readBase(t, b, "vendor/lib.txt"); got != "vendored\n" {
				t.Errorf("tracked file below a hidden root = %q", got)
			}
			list, err := b.Bottom(time.Time{}).ReadDir("")
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, info := range list {
				names = append(names, info.Name())
			}
			wantNames := []string{".gitignore", "source.txt", "src", "vendor"}
			if tc.live != nil {
				wantNames = []string{".gitignore", "source.txt", "src", "tmp", "vendor"}
			}
			if !slices.Equal(names, wantNames) {
				t.Errorf("root lists %v, want %v", names, wantNames)
			}
			if tc.live == nil {
				if class, _ := b.Class("tmp"); class != rules.Hidden {
					t.Errorf("tmp recorded as %q, want a hidden root", class)
				}
			}
		})
	}
}
