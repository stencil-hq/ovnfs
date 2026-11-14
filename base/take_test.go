package base

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

func TestTakeBlobsRemainLazyAcrossReopen(t *testing.T) {
	for _, src := range []string{"index", "revision"} {
		t.Run(src, func(t *testing.T) {
			repo, dir := repository(t)
			ctx := context.Background()
			writeTestFile(t, filepath.Join(dir, "source.txt"), "selected object content\n")
			runGit(t, dir, "add", "source.txt")
			oid := store.OID(runGit(t, dir, "rev-parse", ":source.txt"))
			take := Take{Path: "source.txt"}
			if src == "revision" {
				runGit(t, dir, "commit", "-qm", "selected")
				var err error
				take.Revision, err = repo.ResolveRevision(ctx, "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				take.Source = "HEAD"
			}
			writeTestFile(t, filepath.Join(dir, "source.txt"), "different working-tree content\n")
			st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{ID: "b_take", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
			b, _, err := CreateRevision(ctx, c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, nil, take)
			if err != nil {
				t.Fatal(err)
			}
			if st.Has(oid) {
				t.Fatal("taking a git blob materialized its content at creation")
			}
			if info, err := b.Bottom(time.Time{}).Lstat("source.txt"); err != nil || info.Size() != int64(len("selected object content\n")) {
				t.Fatalf("taken metadata before reopen = %v, %v", info, err)
			}
			meta := b.Meta()
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			b, err = Open(c, meta)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if st.Has(oid) {
				t.Fatal("reopening a take materialized its blob")
			}
			if info, err := b.Bottom(time.Time{}).Lstat("source.txt"); err != nil || info.Size() != int64(len("selected object content\n")) {
				t.Fatalf("taken metadata = %v, %v", info, err)
			}
			if got := readBase(t, b, "source.txt"); got != "selected object content\n" {
				t.Fatalf("taken content after reopen = %q", got)
			}
		})
	}
}

func TestTakeGitSourcesRequireCapturedRootRepository(t *testing.T) {
	repo, _ := repository(t)
	revision, err := repo.ResolveRevision(context.Background(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for _, take := range []Take{{}, {Source: "HEAD", Revision: revision}} {
		t.Run(take.Source, func(t *testing.T) {
			dir, err := source.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(dir, "source.txt"), "must not disappear\n")
			st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{ID: "b_take", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
			b, _, err := CreateRevision(context.Background(), c, source.Options{Root: dir, Rules: rules.Set{Rules: rules.Builtins()}}, nil, take)
			if b != nil {
				b.Close()
			}
			if !errors.Is(err, ErrTake) || !strings.Contains(err.Error(), "git repository at the folder root") {
				t.Fatalf("git take on a plain folder = %v, %v", b, err)
			}
		})
	}
}

func TestTakeNestedRepositoryBlobOriginsSurviveReopen(t *testing.T) {
	for _, direction := range []string{"root-into-nested", "nested-into-revision"} {
		t.Run(direction, func(t *testing.T) {
			ctx := context.Background()
			repo, dir := repository(t)
			nested := filepath.Join(dir, "nested")
			writeTestFile(t, filepath.Join(nested, "file"), "root object content\n")
			runGit(t, dir, "add", "nested/file")
			runGit(t, dir, "commit", "-qm", "root-owned file")
			revision, err := repo.ResolveRevision(ctx, "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(nested); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, nested, "init", "-q", "-b", "main")
			runGit(t, nested, "config", "user.email", "test@example.com")
			runGit(t, nested, "config", "user.name", "Test")
			file := filepath.Join(nested, "file")
			writeTestFile(t, file, "nested object content\n")
			// Avoid a racily clean index entry: the working blob must remain
			// solely in the nested object database until the first read.
			old := time.Unix(1_700_000_000, 123456789)
			if err := os.Chtimes(file, old, old); err != nil {
				t.Fatal(err)
			}
			runGit(t, nested, "add", "file")
			runGit(t, nested, "commit", "-qm", "nested-owned file")
			initial := revision
			take := Take{Source: ".", Path: "nested"}
			want := "nested object content\n"
			if direction == "root-into-nested" {
				initial = nil
				take = Take{Source: "HEAD", Path: "nested/file", Revision: revision}
				want = "root object content\n"
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{ID: "b_take", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
			b, _, err := CreateRevision(ctx, c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, initial, take)
			if err != nil {
				t.Fatal(err)
			}
			oid := b.Capture().Files["nested/file"].ID
			if st.Has(oid) {
				t.Fatal("nested take materialized its selected blob")
			}
			meta := b.Meta()
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			b, err = Open(c, meta)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if st.Has(oid) {
				t.Fatal("reopening a nested take materialized its selected blob")
			}
			if got := readBase(t, b, "nested/file"); got != want {
				t.Fatalf("nested take after reopen = %q, want %q", got, want)
			}
		})
	}
}

func TestTakeDirectoryFileTransitions(t *testing.T) {
	repo, dir := repository(t)
	ctx := context.Background()
	original, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "src")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "src"), "now a file\n")
	if err := os.Remove(filepath.Join(dir, "source.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "source.txt/nested"), "now a directory\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_take", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	b, _, err := CreateRevision(ctx, c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, original,
		Take{Source: ".", Path: "src"}, Take{Source: ".", Path: "source.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := readBase(t, b, "src"); got != "now a file\n" {
		t.Fatalf("directory to file = %q", got)
	}
	if got := readBase(t, b, "source.txt/nested"); got != "now a directory\n" {
		t.Fatalf("file to directory = %q", got)
	}
	if _, err := b.Bottom(time.Time{}).Lstat("src/nested.txt"); !os.IsNotExist(err) {
		t.Fatalf("old directory child survived: %v", err)
	}
}

func TestTakePreservesIgnoredDescendantOfDeletedDirectory(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "src/cache/cache.bin"), "live cache\n")
	writeTestFile(t, filepath.Join(dir, "src/deep/cache/cache.bin"), "nested cache\n")
	writeTestFile(t, filepath.Join(dir, ".gitignore"), "src/cache/\nsrc/deep/cache/\n")
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-qm", "ignored descendant")
	runGit(t, dir, "rm", "src/nested.txt")
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_take", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	b, _, err := CreateRevision(context.Background(), c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, nil, Take{Path: "src"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got := readBase(t, b, "src/cache/cache.bin"); got != "live cache\n" {
		t.Fatalf("ignored descendant = %q", got)
	}
	if got := readBase(t, b, "src/deep/cache/cache.bin"); got != "nested cache\n" {
		t.Fatalf("nested ignored descendant = %q", got)
	}
	if _, err := b.Bottom(time.Time{}).Lstat("src/nested.txt"); !os.IsNotExist(err) {
		t.Fatalf("deleted source child survived: %v", err)
	}
}
