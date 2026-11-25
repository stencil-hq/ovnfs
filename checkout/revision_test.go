package checkout

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

func revisionRepo(t *testing.T) string {
	t.Helper()
	dir := folder(t, map[string]string{
		"tracked.txt": "original\n",
		"other.txt":   "other\n",
		".gitignore":  "build/\n*.log\nnode_modules/kept/\n",
	})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "initial")
	return dir
}

// TestNewRevisionSubmoduleIgnores leaves a submodule empty even when its captured
// files include a directory ignored only by the superproject.
func TestNewRevisionSubmoduleIgnores(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		name := "untracked"
		if tracked {
			name = "tracked"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			app := submoduleRepo(t)
			sub := filepath.Join(app, "vendor", "sub")
			write(t, filepath.Join(app, ".gitignore"), "__pycache__/\n")
			gitRun(t, app, "add", ".gitignore")
			gitRun(t, app, "commit", "-qm", "ignore caches")
			write(t, filepath.Join(sub, "pkg/__pycache__/c.pyc"), "submodule cache\n")
			if tracked {
				gitRun(t, sub, "add", "pkg")
				gitRun(t, sub, "commit", "-qm", "add submodule cache")
				gitRun(t, app, "add", "vendor/sub")
				gitRun(t, app, "commit", "-qm", "update submodule")
			}
			head := gitRun(t, sub, "rev-parse", "HEAD")
			if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: app, Name: "revision", Rev: "HEAD"}); err != nil {
				t.Fatal(err)
			}
			exported := f.export("revision")
			if entries, err := os.ReadDir(filepath.Join(exported, "vendor", "sub")); err != nil || len(entries) != 0 {
				t.Fatalf("revision submodule = %v, %v; want an empty directory", entries, err)
			}
			if got := gitRun(t, exported, "submodule", "status"); got != "-"+head+" vendor/sub" {
				t.Fatalf("submodule status = %q, want it uninitialized", got)
			}
			if diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "revision"}); err != nil || len(diff.Changes) != 0 {
				t.Fatalf("initial revision diff = %+v, %v", diff, err)
			}
		})
	}
}

func TestNewRevisionDirtyFolder(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "tracked.txt"), "user's dirty edit\n")
	write(t, filepath.Join(dir, "new.txt"), "untracked\n")
	write(t, filepath.Join(dir, "build/out.o"), "kept build\n")
	write(t, filepath.Join(dir, "debug.log"), "kept ignored file\n")
	// Neither a dirty ignore file nor a nonignored built-in cache directory
	// should leak untracked files into the revision checkout.
	write(t, filepath.Join(dir, ".gitignore"), "build/\nnew.txt\n")
	write(t, filepath.Join(dir, "node_modules/not-ignored"), "remove\n")
	write(t, filepath.Join(dir, "node_modules/kept/cache"), "nested ignored\n")
	reply, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "revision", Rev: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"tracked.txt": "original\n", "new.txt": "<absent>",
		"build/out.o": "kept build\n", "debug.log": "kept ignored file\n",
		"node_modules/not-ignored": "<absent>",
		"node_modules/kept/cache":  "nested ignored\n",
	} {
		if got := f.read(reply.Checkout.Name, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	// A small materialized view exercises the private index with real git,
	// without requiring a kernel mount in package tests.
	work := t.TempDir()
	for _, p := range []string{".gitignore", "tracked.txt", "other.txt"} {
		write(t, filepath.Join(work, p), f.read("revision", p))
	}
	if got := gitRun(t, work, "--git-dir", f.gitdir("revision"), "--work-tree", work, "status", "--porcelain"); got != "" {
		t.Fatalf("revision status = %q", got)
	}
	assertDiff := func(want string) {
		t.Helper()
		diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "revision"})
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, change := range diff.Changes {
			paths = append(paths, change.Path)
		}
		if got := strings.Join(paths, " "); got != want {
			t.Fatalf("diff = %q, want %q", got, want)
		}
	}
	assertDiff("")
	f.put("revision", "other.txt", "agent edit\n")
	assertDiff("other.txt")
	for _, dry := range []bool{true, false} {
		applied, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "revision", DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		if len(applied.Applied) != 1 || applied.Applied[0].Path != "other.txt" || len(applied.Conflicts) != 0 {
			t.Fatalf("apply dry=%t = %+v", dry, applied)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "tracked.txt")); err != nil || string(got) != "user's dirty edit\n" {
			t.Fatalf("source dirty edit = %q, %v", got, err)
		}
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	assertDiff("other.txt")
}

func TestNewRevisionHiddenRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := folder(t, map[string]string{"public/a.txt": "before\n", "private/s.txt": "private\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "initial")
	const name = "revision"
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: name, Rev: "main", Rules: []string{"hidden private/"}}); err != nil {
		t.Fatal(err)
	}
	work := f.export(name)
	if _, err := os.Stat(filepath.Join(work, "private")); !os.IsNotExist(err) {
		t.Fatalf("exported hidden directory: %v", err)
	}
	if got := gitRun(t, work, "status", "--porcelain"); got != "" {
		t.Fatalf("initial git status = %q", got)
	}
	if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: name}); err != nil || len(diff.Changes) != 0 {
		t.Fatalf("initial revision diff = %+v, %v", diff, err)
	}
	if got := strings.Join(f.snapFiles(name), " "); got != "public/a.txt" {
		t.Fatalf("snapshot contains %q", got)
	}
	fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	for _, checkout := range []string{name, fork.Checkout.Name} {
		if got := f.read(checkout, "private/s.txt"); got != "<absent>" {
			t.Errorf("%s reopened hidden file: %q", checkout, got)
		}
		reply, err := f.reg.Explain(ctx, api.ExplainArgs{Checkout: checkout, Path: "private/s.txt"})
		if err != nil || reply.Exists || reply.Class != "hidden" || reply.At != "private" || reply.Rule.Text != "private/" {
			t.Errorf("%s explain = %+v, %v", checkout, reply, err)
		}
	}
	dst := newFixture(t)
	_, remote := offerFixture(t, f, dst, name)
	if got := dst.read(remote.Name, "private/s.txt"); got != "<absent>" {
		t.Errorf("remote hidden file = %q", got)
	}

	f.put(name, "public/a.txt", "edited\n")
	applied, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: name, DryRun: true})
	if err != nil || len(applied.Conflicts) != 0 || len(applied.Applied) != 1 || applied.Applied[0].Path != "public/a.txt" {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	work = f.export(name)
	gitRun(t, work, "add", "--all")
	if got := gitRun(t, work, "diff", "--cached", "--name-only"); got != "public/a.txt" {
		t.Fatalf("staged paths = %q", got)
	}
	gitRun(t, work, "commit", "-qm", "edit public file")
	landed, err := f.reg.Land(ctx, api.LandArgs{Checkout: name})
	if err != nil || landed.Refused || len(landed.Applied) != 1 || landed.Applied[0].Path != "public/a.txt" ||
		landed.Commit != gitRun(t, work, "rev-parse", "HEAD") {
		t.Fatalf("land = %+v, %v", landed, err)
	}
	if got := gitRun(t, dir, "show", "HEAD:private/s.txt"); got != "private" {
		t.Errorf("landed commit's hidden file = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "private", "s.txt")); err != nil || string(got) != "private\n" {
		t.Errorf("source's hidden file = %q, %v", got, err)
	}
}

func TestHistoricalRevisionHiddenRules(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := folder(t, map[string]string{
		"public/a.txt": "public\n", "public/big.txt": "large in REV\n", "private/s.txt": "private\n",
	})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "historical")
	gitRun(t, dir, "rm", "-qr", "private")
	write(t, filepath.Join(dir, "private"), "now a regular file\n")
	write(t, filepath.Join(dir, "public", "big.txt"), "x\n")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "directory replaced by a file")
	for i, rule := range []string{"hidden private/ & tracked", "hidden !(public | public/**)"} {
		name := fmt.Sprintf("historical-%d", i)
		rules := []string{rule, "hidden big.txt & size > 8B"}
		if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: name, Rev: "HEAD~1", Rules: rules}); err != nil {
			t.Fatal(err)
		}
		if got := f.read(name, "public/a.txt"); got != "public\n" {
			t.Errorf("%s removed an allowed file: %q", rule, got)
		}
		for _, p := range []string{"private/s.txt", "public/big.txt"} {
			if got := f.read(name, p); got != "<absent>" {
				t.Errorf("%s kept %s: %q", rule, p, got)
			}
		}
	}
}

// TestRevisionLFSObjectsCompareAsPointers compares content with a revision's
// LFS files as git's clean filter does: an object rewritten unchanged is no
// change, and a folder holding the object still counts as unchanged, so apply
// can delete it. Apply works with objects, never pointers: a mode change keeps
// the object, and a text merge starts from it.
func TestRevisionLFSObjectsCompareAsPointers(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "")
	f := newFixture(t)
	const image, text = "golden image\n", "one\ntwo\nthree\nfour\n"
	pointers := map[string]string{}
	for _, object := range []string{image, text} {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(object)))
		pointers[object] = fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", sum, len(object))
	}
	objects := map[string]string{"a.png": image, "b.png": image, "c.png": image, "notes.txt": text}
	files := map[string]string{".gitattributes": "*.png filter=lfs -text\n*.txt filter=lfs -text\n"}
	for p, object := range objects {
		files[p] = pointers[object]
	}
	dir := folder(t, files)
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "lfs")
	gitRun(t, dir, "config", "filter.lfs.process", "git-lfs filter-process")
	for _, object := range []string{image, text} {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(object)))
		write(t, filepath.Join(dir, ".git", "lfs", "objects", sum[:2], sum[2:4], sum), object)
	}
	for p, object := range objects {
		write(t, filepath.Join(dir, p), object) // as git's checkout leaves the folder
	}
	// The capture stores this untracked copy, so the store holds the pointer blob too.
	write(t, filepath.Join(dir, "pointer.txt"), pointers[image])
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "lfs", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	if got := f.read("lfs", "a.png"); got != image {
		t.Fatalf("a.png = %q, want its object", got)
	}
	f.put("lfs", "a.png", image)
	f.remove("lfs", "b.png")
	diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "lfs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 || diff.Changes[0].Path != "b.png" {
		t.Fatalf("diff = %+v, want only the deleted b.png", diff.Changes)
	}
	var s Session
	if err := f.reg.Enter("lfs", &s); err != nil {
		t.Fatal(err)
	}
	err = s.FS.Chmod("c.png", 0o755)
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	f.put("lfs", "notes.txt", "one\ntwo\nthree\nfour, edited in the checkout\n")
	write(t, filepath.Join(dir, "notes.txt"), "one, edited in the folder\ntwo\nthree\nfour\n")
	applied, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "lfs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Conflicts) != 0 || len(applied.Merged) != 1 {
		t.Fatalf("apply = %+v, want notes.txt merged and no conflict", applied)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.png")); !os.IsNotExist(err) {
		t.Fatalf("b.png in the folder: %v, want deleted", err)
	}
	for p, want := range map[string]string{"c.png": image, "notes.txt": "one, edited in the folder\ntwo\nthree\nfour, edited in the checkout\n"} {
		if got, err := os.ReadFile(filepath.Join(dir, p)); err != nil || string(got) != want {
			t.Errorf("folder %s = %q, %v; want %q", p, got, err, want)
		}
	}
	// An edit applied, then undone in the checkout, brings the object back.
	for _, content := range []string{"edited image\n", image} {
		f.put("lfs", "a.png", content)
		if _, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "lfs"}); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "a.png")); err != nil || string(got) != content {
			t.Fatalf("folder a.png = %q, %v; want %q", got, err, content)
		}
	}
}

// TestEditKeepsExecutableMode snapshots an edited executable from a revision as
// still executable, on hosts whose upper files cannot hold mode bits too.
func TestEditKeepsExecutableMode(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "run.sh"), "echo old\n")
	gitRun(t, dir, "add", "run.sh")
	gitRun(t, dir, "update-index", "--chmod=+x", "run.sh")
	gitRun(t, dir, "commit", "-qm", "executable")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "exec", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	f.put("exec", "run.sh", "echo new\n")
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "exec", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.reg.store.Flatten(store.OID(reply.Snapshot.Tree))
	if err != nil {
		t.Fatal(err)
	}
	if got := files["run.sh"].Mode; got != store.ModeExec {
		t.Fatalf("edited executable snapshots as mode %o", got)
	}
}

func TestRevisionHEADAttachment(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	commit := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "tag", "release")
	for _, tt := range []struct{ rev, head string }{
		{"main", "ref: refs/heads/main"},
		{"refs/heads/main", "ref: refs/heads/main"},
		{commit, commit}, {"HEAD", commit}, {"release", commit},
	} {
		t.Run(tt.rev, func(t *testing.T) {
			reply, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Rev: tt.rev})
			if err != nil {
				t.Fatal(err)
			}
			head, err := os.ReadFile(filepath.Join(f.gitdir(reply.Checkout.Name), "HEAD"))
			if err != nil || strings.TrimSpace(string(head)) != tt.head {
				t.Fatalf("HEAD = %q, %v; want %q", head, err, tt.head)
			}
		})
	}
}

func TestNewHistoricalRevision(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	commit := gitRun(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "mv", "tracked.txt", "renamed.txt")
	write(t, filepath.Join(dir, "other.txt"), "new commit\n")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "different tree")
	write(t, filepath.Join(dir, "staged.txt"), "staged addition\n")
	gitRun(t, dir, "add", "staged.txt")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "historical", Rev: commit}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		"tracked.txt": "original\n", "other.txt": "other\n",
		"renamed.txt": "<absent>", "staged.txt": "<absent>",
	} {
		if got := f.read("historical", p); got != want {
			t.Fatalf("%s = %q, want %q", p, got, want)
		}
	}
	if diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "historical"}); err != nil || len(diff.Changes) != 0 {
		t.Fatalf("historical diff = %+v, %v", diff, err)
	}
	if got := strings.Join(f.snapFiles("historical"), " "); got != ".gitignore other.txt tracked.txt" {
		t.Fatalf("historical snapshot = %s", got)
	}
}

func TestNewCheckoutRevisionOwnsParentCommit(t *testing.T) {
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "")
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	gitRun(t, dir, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
	host := gitRun(t, dir, "rev-parse", "HEAD")
	// A revision capture is not retained by the warm folder cache.
	opened, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "parent", Rev: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	parent := opened.Checkout
	work := f.export(parent.Name)
	gitRun(t, work, "checkout", "-qb", "private")
	write(t, filepath.Join(work, "tracked.txt"), "parent commit\n")
	write(t, filepath.Join(work, "sub", "private.txt"), "committed private file\n")
	lfs := "object kept only by the parent\n"
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte(lfs)))
	write(t, filepath.Join(work, ".gitattributes"), "*.bin filter=lfs\n")
	write(t, filepath.Join(work, "sub", "large.bin"), fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(lfs)))
	write(t, filepath.Join(f.gitdir(parent.Name), "lfs", "objects", oid[:2], oid[2:4], oid), lfs)
	gitRun(t, work, "add", "--all")
	gitRun(t, work, "commit", "-qm", "private revision")
	commit := gitRun(t, work, "rev-parse", "HEAD")
	gitRun(t, work, "repack", "-a", "-d", "-l")
	write(t, filepath.Join(work, "tracked.txt"), "dirty parent file\n")
	write(t, filepath.Join(work, "staged.txt"), "staged parent file\n")
	gitRun(t, work, "add", "staged.txt")
	f.put(parent.Name, "tracked.txt", "dirty mounted file\n")
	f.put(parent.Name, "parent.log", "ignored mounted file\n")
	write(t, filepath.Join(dir, "host.log"), "ignored host file\n")

	// No actual kernel mount is needed: even a nested spelling resolves Git
	// from the parent state and the capture's filesystem from the host.
	child, err := f.reg.New(ctx, api.NewArgs{Dir: filepath.Join(parent.Path, "sub"), Name: "child", Rev: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if c := child.Checkout; c.Source != parent.Source || c.Parent != "" || c.Base == parent.Base || child.Capture == nil || child.Capture.Root != parent.Source {
		t.Fatalf("nested revision = %+v, want an independent capture of %s", child, parent.Source)
	}
	if got := gitRun(t, dir, "--git-dir", f.gitdir("child"), "config", "ovnfs.source"); got != parent.Source {
		t.Fatalf("nested revision source = %q, want %q", got, parent.Source)
	}
	if got := gitRun(t, dir, "rev-parse", "HEAD"); got != host {
		t.Fatalf("host HEAD = %q, want %s", got, host)
	}
	if got := gitRun(t, dir, "for-each-ref", "--format=%(refname)", "refs/heads"); got != "refs/heads/main" {
		t.Fatalf("host refs = %q, want only main", got)
	}
	if err := f.reg.Rm(ctx, api.RmArgs{Checkout: parent.Name}); err != nil {
		t.Fatal(err)
	}
	parentObjects := f.reg.objectsPath(parent.Base, "")
	eventually(t, "parent base's Git objects were not collected", func() bool { return !exists(parentObjects) })
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	for p, want := range map[string]string{
		"tracked.txt": "parent commit\n", "sub/private.txt": "committed private file\n", "sub/large.bin": lfs,
		"staged.txt": "<absent>", "parent.log": "<absent>", "host.log": "ignored host file\n",
	} {
		if got := f.read("child", p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	gitdir := f.gitdir("child")
	if got := gitRun(t, dir, "--git-dir", gitdir, "symbolic-ref", "HEAD"); got != "refs/heads/private" {
		t.Fatalf("child HEAD points to %q, want private", got)
	}
	if got := gitRun(t, dir, "--git-dir", gitdir, "rev-parse", "HEAD", "HEAD^"); got != commit+"\n"+host {
		t.Fatalf("child history = %q, want %s followed by %s", got, commit, host)
	}
	if got := gitRun(t, dir, "--git-dir", gitdir, "show", "HEAD:sub/private.txt"); got != "committed private file" {
		t.Fatalf("child Git blob = %q", got)
	}
	if twin, err := f.reg.New(ctx, api.NewArgs{Base: child.Checkout.Base, Name: "twin"}); err != nil || twin.Checkout.Base != child.Checkout.Base {
		t.Fatalf("new on persisted revision capture = %+v, %v", twin, err)
	}
	if got := f.read("twin", "sub/private.txt"); got != "committed private file\n" {
		t.Fatalf("twin Git blob = %q", got)
	}
	if got := gitRun(t, dir, "--git-dir", f.gitdir("twin"), "rev-parse", "HEAD"); got != commit {
		t.Fatalf("twin HEAD = %q, want %s", got, commit)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: child.Checkout.Path, Name: "grandchild", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	if got := f.read("grandchild", "sub/large.bin"); got != lfs {
		t.Fatalf("grandchild LFS object = %q, want %q", got, lfs)
	}
}

func TestNewCheckoutRevisionRejectsMixedFlags(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	parent := f.newCheckout(revisionRepo(t), "parent").Checkout
	for _, args := range []api.NewArgs{
		{Dir: parent.Path, Rev: "HEAD", Take: []string{".:tracked.txt"}},
		{Dir: filepath.Join(parent.Path, "sub"), Rev: "HEAD", Rules: []string{"hidden tracked.txt"}},
	} {
		_, err := f.reg.New(ctx, args)
		if e, ok := err.(*Error); !ok || e.Code != "invalid" || !strings.Contains(e.Message, parent.Source) {
			t.Errorf("nested new %+v: %v, want source-folder guidance", args, err)
		}
	}
	if got := f.ls(); len(got.Checkouts) != 1 || len(got.Snapshots) != 0 {
		t.Fatalf("rejected options created state: %+v", got)
	}
}

func TestRevisionErrorsLeaveNoCheckout(t *testing.T) {
	f := newFixture(t)
	repo := revisionRepo(t)
	for _, args := range []api.NewArgs{
		{Dir: repo, Name: "bad", Rev: "does-not-exist"},
		{Dir: t.TempDir(), Name: "bad", Rev: "HEAD"},
		{Snapshot: "missing", Name: "bad", Rev: "HEAD"},
	} {
		if _, err := f.reg.New(context.Background(), args); err == nil {
			t.Fatalf("new %+v unexpectedly succeeded", args)
		}
		if len(f.reg.checkouts) != 0 || len(f.reg.bases) != 0 || len(f.reg.reserved) != 0 || len(f.reg.intents) != 0 {
			t.Fatal("failed revision creation left registry state")
		}
		for _, sub := range []string{"git", "uppers", "bases"} {
			entries, err := os.ReadDir(filepath.Join(f.state, sub))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed creation left %s: %v, %v", sub, entries, err)
			}
		}
	}
}

func TestCheckoutMarkersAndMigration(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	source := f.newCheckout(dir, "parent").Checkout.Source
	fork, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "parent", Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.New(ctx, api.NewArgs{Snapshot: fork.Snapshot.ID, Name: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"parent", "child", "snapshot"} {
		if got := gitRun(t, dir, "--git-dir", f.gitdir(name), "config", "--local", "--get", "ovnfs.checkout"); got != name {
			t.Fatalf("%s marker = %q", name, got)
		}
		if got := gitRun(t, dir, "--git-dir", f.gitdir(name), "config", "--local", "--get", "ovnfs.source"); got != source {
			t.Fatalf("%s source = %q, want %q", name, got, source)
		}
	}
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil || strings.Contains(string(config), "[ovnfs]") {
		t.Fatalf("source config changed: %s, %v", config, err)
	}
	gitdir := f.gitdir("parent")
	gitRun(t, dir, "--git-dir", gitdir, "config", "--local", "--unset", "ovnfs.checkout")
	gitRun(t, dir, "--git-dir", gitdir, "config", "--local", "--unset", "ovnfs.source")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if got := gitRun(t, dir, "--git-dir", gitdir, "config", "--local", "--get", "ovnfs.checkout"); got != "parent" {
		t.Fatalf("reopened checkout marker = %q", got)
	}
	if got := gitRun(t, dir, "--git-dir", gitdir, "config", "--local", "--get", "ovnfs.source"); got != source {
		t.Fatalf("reopened checkout source = %q, want %q", got, source)
	}
}

// A revision can ignore paths that the dirty folder does not. Both captured
// source files and files beneath a captured local root can then survive from
// the folder, but neither may bypass the revision's hidden rules.
func TestRevisionHiddenInheritedPaths(t *testing.T) {
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, ".gitignore"), "*.out\n")
	write(t, filepath.Join(dir, ".o2/ovnfs.yml"), `rules: |
  source mixed.out/ & ignored
  hidden ignored & (*.out | private.txt)
  local local.out/ | local-root/ | mixed.out/
`)
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "ignore output in revision")
	write(t, filepath.Join(dir, ".gitignore"), "")
	for _, p := range []string{"captured.out", "elsewhere.out/output", "local.out/output", "local-root/nested.out", "mixed.out/private.txt", "mixed.out/kept.txt"} {
		write(t, filepath.Join(dir, p), "folder output\n")
	}
	write(t, filepath.Join(dir, "retain.log"), "folder log\n")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: "revision", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"captured.out", "elsewhere.out/output", "local.out/output", "mixed.out/private.txt"} {
		if got := f.read("revision", p); got != "<absent>" {
			t.Errorf("%s = %q, want hidden by revision", p, got)
		}
	}
	// The local-root/ directory decision precedes any decision for nested.out.
	if got := f.read("revision", "local-root/nested.out"); got != "folder output\n" {
		t.Errorf("ignored file below a local root = %q", got)
	}
	if got := f.read("revision", "mixed.out/kept.txt"); got != "folder output\n" {
		t.Errorf("visible sibling of hidden ignored file = %q", got)
	}
	if got := f.read("revision", "retain.log"); got != "<absent>" {
		t.Errorf("file not ignored by REV = %q", got)
	}
}
