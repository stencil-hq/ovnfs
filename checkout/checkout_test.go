package checkout

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/stats"
	"github.com/stencil-hq/ovnfs/store"
)

// fixture is a registry over a temporary state directory.
type fixture struct {
	t            *testing.T
	state, mount string
	reg          *Registry
	mu           sync.Mutex
	invalidated  []invalidation  // Config.Invalidate calls, in order
	faults       []string        // messages of Config.Emit's fault events, in order
	gitRemotes   bool            // Config.GitRemotes at the next open
	gitHooks     bool            // Config.GitHooks at the next open
	stats        *stats.Recorder // Config.Stats at the next open
}

// invalidation is one Config.Invalidate call; a "" tree covers the whole checkout.
type invalidation struct {
	checkout     string
	paths, trees []string
}

// whole reports whether i covers the whole checkout.
func (i invalidation) whole() bool { return slices.Contains(i.trees, "") }

// invalidations returns the Config.Invalidate calls recorded so far.
func (f *fixture) invalidations() []invalidation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.invalidated)
}

// invalidatedPath reports whether p of checkout was invalidated, alone or in a tree.
func (f *fixture) invalidatedPath(checkout, p string) bool {
	for _, i := range f.invalidations() {
		if i.checkout == checkout && (slices.Contains(i.paths, p) || slices.ContainsFunc(i.trees, func(t string) bool {
			_, ok := below(t, p)
			return ok
		})) {
			return true
		}
	}
	return false
}

// eventually polls cond for up to five seconds, for effects of asynchronous events.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// newFixture opens a registry that ignores the developer's ~/.o2/ovnfs.yml.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, state: filepath.Join(t.TempDir(), "state"), mount: filepath.Join(t.TempDir(), "mount")}
	f.open()
	t.Cleanup(func() { f.reg.Close() })
	return f
}

// newHooksFixture is newFixture with Config.GitHooks on, reopened as TestGitRemotes does.
func newHooksFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.reg.Close()
	f.gitHooks = true
	f.open()
	return f
}

// config is the registry configuration of the fixture's state.
func (f *fixture) config() Config {
	return Config{State: f.state, Mount: f.mount, Node: "n_" + filepath.Base(filepath.Dir(f.state)), Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20, GitRemotes: f.gitRemotes, GitHooks: f.gitHooks, Stats: f.stats,
		Invalidate: func(checkout string, paths, trees []string) {
			f.mu.Lock()
			f.invalidated = append(f.invalidated, invalidation{checkout: checkout, paths: slices.Clone(paths), trees: slices.Clone(trees)})
			f.mu.Unlock()
		},
		Emit: func(e Event) {
			payload, _ := e.Payload.(map[string]any)
			if msg, ok := payload["message"].(string); ok && e.Name == "fault" {
				f.mu.Lock()
				f.faults = append(f.faults, msg)
				f.mu.Unlock()
			}
		}}
}

// open (re)opens the registry on the fixture's state.
func (f *fixture) open() {
	f.t.Helper()
	reg, err := Open(f.config())
	if err != nil {
		f.t.Fatal(err)
	}
	f.reg = reg
}

// isolateGit keeps the developer's git configuration, such as merge.conflictStyle,
// out of the registry's git calls.
func isolateGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}

// write creates a fixture file with parents.
func write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// folder creates a source folder from path → content and waits past ctime granularity.
func folder(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, data := range files {
		write(t, filepath.Join(dir, p), data)
	}
	time.Sleep(10 * time.Millisecond)
	return dir
}

// newCheckout opens a checkout on dir.
func (f *fixture) newCheckout(dir, name string, rules ...string) api.NewReply {
	f.t.Helper()
	reply, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: name, Rules: rules})
	if err != nil {
		f.t.Fatal(err)
	}
	f.awaitCaches(reply.Checkout.Name)
	return reply
}

// awaitCaches waits until checkout's cache snapshot is complete, so the folder
// edits a test makes next come after it.
func (f *fixture) awaitCaches(checkout string) {
	f.reg.mu.RLock()
	h := f.reg.checkouts[checkout]
	f.reg.mu.RUnlock()
	if h != nil && h.caches != nil {
		h.caches.Wait()
	}
}

// ls lists every checkout and snapshot.
func (f *fixture) ls() api.LsReply {
	f.t.Helper()
	reply, err := f.reg.Ls(context.Background(), api.LsArgs{})
	if err != nil {
		f.t.Fatal(err)
	}
	return reply
}

// read returns a checkout file's content, or "<absent>".
func (f *fixture) read(checkout, p string) string {
	f.t.Helper()
	var s Session
	if err := f.reg.Enter(checkout, &s); err != nil {
		f.t.Fatal(err)
	}
	defer s.Done()
	file, err := s.FS.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// put writes a checkout file through the overlay, as an agent's editor would.
func (f *fixture) put(checkout, p, data string) {
	f.t.Helper()
	var s Session
	if err := f.reg.Enter(checkout, &s); err != nil {
		f.t.Fatal(err)
	}
	defer s.Done()
	if err := s.FS.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	file, err := s.FS.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := file.Write([]byte(data)); err != nil {
		f.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// remove deletes a checkout path through the overlay.
func (f *fixture) remove(checkout, p string) {
	f.t.Helper()
	var s Session
	if err := f.reg.Enter(checkout, &s); err != nil {
		f.t.Fatal(err)
	}
	defer s.Done()
	if err := s.FS.Remove(p); err != nil {
		f.t.Fatal(err)
	}
}

// snapFiles snapshots a checkout and lists the blob paths of its tree.
func (f *fixture) snapFiles(checkout string) []string {
	f.t.Helper()
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: checkout, Wait: true})
	if err != nil {
		f.t.Fatal(err)
	}
	files, err := f.reg.store.Flatten(store.OID(reply.Snapshot.Tree))
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for p, leaf := range files {
		if leaf.Mode != store.ModeDir {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// TestPlainFolderClasses opens a folder without git and checks each class in the
// view and in snapshots: only source paths are hashed, hidden paths do not exist.
func TestPlainFolderClasses(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{
		"src/a.txt":               "a",
		"node_modules/x/index.js": "module",
		".env":                    "SECRET=1",
		".DS_Store":               "junk",
		"data/raw.bin":            "raw",
		"logs/run.log":            "log",
		".o2/ovnfs.yml":           "rules: |\n  local data/**\n",
		"notes/.gitignore":        "*.log\n",
		"notes/keep.md":           "keep",
		"notes/drop.log":          "drop",
	})
	reply := f.newCheckout(dir, "plain")
	if reply.Checkout.Git || reply.Capture == nil {
		t.Fatalf("plain folder reply: %+v", reply)
	}
	if c := reply.Capture; c.Cache != 1 || c.Local != 3 || c.Hidden != 1 {
		t.Fatalf("roots: cache %d local %d hidden %d", c.Cache, c.Local, c.Hidden)
	}
	for p, want := range map[string]string{"src/a.txt": "a", "node_modules/x/index.js": "module", ".env": "SECRET=1", ".DS_Store": "<absent>", "data/raw.bin": "raw", "notes/drop.log": "drop"} {
		if got := f.read("plain", p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	f.put("plain", "src/b.txt", "b")
	f.put("plain", "node_modules/x/added.js", "installed")
	f.put("plain", ".env", "SECRET=2")
	f.put("plain", "data/new.bin", "new")
	got := f.snapFiles("plain")
	want := []string{".o2/ovnfs.yml", "logs/run.log", "notes/.gitignore", "notes/keep.md", "src/a.txt", "src/b.txt"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("snapshot files = %v, want %v", got, want)
	}
	if got := f.read("plain", "node_modules/x/added.js"); got != "installed" {
		t.Fatalf("cache write lost: %q", got)
	}
	explained, err := f.reg.Explain(context.Background(), api.ExplainArgs{Checkout: "plain", Path: "data/raw.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if explained.Class != "local" || explained.Rule.Origin != ".o2/ovnfs.yml:2" || !explained.Exists {
		t.Fatalf("explain = %+v", explained)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	explained, err = f.reg.Explain(context.Background(), api.ExplainArgs{Dir: link, Path: "data/raw.bin"})
	if err != nil || explained.Path != "data/raw.bin" || explained.Rule.Origin != ".o2/ovnfs.yml:2" {
		t.Fatalf("explain through a symlinked folder = %+v, %v", explained, err)
	}
}

// TestDiffAndApply merges a checkout's changes back three-way: unchanged folder paths
// take the checkout's version, clean text merges apply, overlapping edits get conflict
// markers that keep being reported until resolved, and a resolution is kept, across a
// restart, as the base later checkout edits merge against.
func TestDiffAndApply(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "1\n2\n3\n", "b.txt": "b\n", "c.txt": "c\nkeep\n", "node_modules/m.js": "m"})
	f.newCheckout(dir, "work")
	f.put("work", "a.txt", "ONE\n2\n3\n")
	f.remove("work", "b.txt")
	f.put("work", "d.txt", "d\n")
	f.put("work", "c.txt", "checkout\nkeep\n")
	f.put("work", "node_modules/m.js", "changed by install")
	diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "work", Patch: true})
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, c := range diff.Changes {
		statuses = append(statuses, c.Status+" "+c.Path)
	}
	if got := strings.Join(statuses, ", "); got != "M a.txt, D b.txt, M c.txt, A d.txt" {
		t.Fatalf("diff = %s", got)
	}
	if !strings.Contains(diff.Patch, "+ONE") || !strings.Contains(diff.Patch, "+++ b/d.txt") {
		t.Fatalf("patch:\n%s", diff.Patch)
	}
	// The user keeps working in the folder meanwhile.
	write(t, filepath.Join(dir, "a.txt"), "1\n2\nTHREE\n")
	write(t, filepath.Join(dir, "c.txt"), "folder\nkeep\n")
	apply, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(apply.Applied) != 2 || len(apply.Merged) != 1 || apply.Merged[0] != "a.txt" || len(apply.Conflicts) != 1 || apply.Conflicts[0].Path != "c.txt" || !apply.Conflicts[0].Markers {
		t.Fatalf("apply = %+v", apply)
	}
	markers := "<<<<<<< folder\nfolder\n=======\ncheckout\n>>>>>>> checkout work\nkeep\n"
	for p, want := range map[string]string{"a.txt": "ONE\n2\nTHREE\n", "c.txt": markers, "d.txt": "d\n", "node_modules/m.js": "m"} {
		data, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || string(data) != want {
			t.Errorf("folder %s = %q %v, want %q", p, data, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("b.txt survived apply: %v", err)
	}
	again, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Applied) != 0 || len(again.Merged) != 0 || len(again.Conflicts) != 1 || !again.Conflicts[0].Markers {
		t.Fatalf("second apply = %+v", again)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(data) != markers {
		t.Fatalf("second apply rewrote unresolved c.txt: %q", data)
	}

	write(t, filepath.Join(dir, "c.txt"), "resolved\nkeep\n")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	resolved, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "work"})
	if err != nil || len(resolved.Applied)+len(resolved.Merged)+len(resolved.Conflicts) != 0 {
		t.Fatalf("apply after resolving = %+v, %v", resolved, err)
	}
	// Against the base capture this edit overlaps the resolved line; against the marked version it does not.
	f.put("work", "c.txt", "checkout\nkeep\nmore\n")
	later, err := f.reg.Apply(context.Background(), api.ApplyArgs{Checkout: "work"})
	if err != nil || len(later.Merged) != 1 || len(later.Conflicts) != 0 {
		t.Fatalf("apply after a later edit = %+v, %v", later, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "c.txt")); string(data) != "resolved\nkeep\nmore\n" {
		t.Fatalf("later edit merged as %q", data)
	}
}

// TestForkAndReopen checks that a fork starts from the parent's state, diverges,
// and that checkouts, snapshots and hashes survive a restart.
func TestForkAndReopen(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "a"})
	f.newCheckout(dir, "parent")
	f.put("parent", "a.txt", "parent edit")
	fork, err := f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if fork.Checkout.Name != "parent-2" || fork.Checkout.Parent != fork.Snapshot.ID {
		t.Fatalf("fork = %+v", fork)
	}
	f.put("parent-2", "a.txt", "child edit")
	if got := f.read("parent", "a.txt"); got != "parent edit" {
		t.Fatalf("parent changed by child: %q", got)
	}
	f.reg.Close()
	f.open()
	if got := f.read("parent-2", "a.txt"); got != "child edit" {
		t.Fatalf("child after reopen = %q", got)
	}
	ls := f.ls()
	if len(ls.Checkouts) != 2 || len(ls.Snapshots) != 1 {
		t.Fatalf("ls after reopen = %+v", ls)
	}
	reply, err := f.reg.waitHashed(context.Background(), f.reg.snapshots[fork.Snapshot.ID])
	if err != nil || reply.Snapshot.Tree == "" {
		t.Fatalf("snapshot after reopen: %+v %v", reply, err)
	}
	if !ls.Checkouts[1].Dirty || ls.Checkouts[0].Dirty {
		t.Fatalf("dirty flags: %+v", ls.Checkouts)
	}
}

// TestNewInsideMount checks that new on a checkout's folder forks that checkout
// instead of capturing it through the daemon's own mount, also through a
// symlink, and refuses the mount itself and folders below a checkout.
func TestNewInsideMount(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "a", "sub/b.txt": "b"})
	parent := f.newCheckout(dir, "parent").Checkout
	f.put("parent", "a.txt", "parent edit")
	ctx := context.Background()
	child, err := f.reg.New(ctx, api.NewArgs{Dir: filepath.Join(f.mount, "parent"), Name: "child"})
	if err != nil {
		t.Fatal(err)
	}
	if c := child.Checkout; c.Name != "child" || c.Base != parent.Base || c.Source != parent.Source || c.Parent == "" || len(child.Warnings) != 1 {
		t.Fatalf("new on a checkout = %+v", child)
	}
	if got := f.read("child", "a.txt"); got != "parent edit" {
		t.Fatalf("child a.txt = %q", got)
	}

	// Unmounted here, so the checkout's folder can be faked for a symlink to resolve.
	if err := os.MkdirAll(filepath.Join(f.mount, "parent", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(f.mount, "parent"), link); err != nil {
		t.Fatal(err)
	}
	linked, err := f.reg.New(ctx, api.NewArgs{Dir: link})
	if err != nil || linked.Checkout.Name != "parent-2" || linked.Checkout.Base != parent.Base {
		t.Fatalf("new through a symlink = %+v, %v", linked, err)
	}

	// A repository holding the mount: new on a folder in it resolves to its root.
	holder := filepath.Dir(f.mount)
	write(t, filepath.Join(holder, "src", "a.txt"), "a")
	gitRun(t, holder, "init", "-q")

	for _, tt := range []struct {
		name string
		args api.NewArgs
		code string
	}{
		{"mount root", api.NewArgs{Dir: f.mount}, "invalid"},
		{"folder holding the mount", api.NewArgs{Dir: holder}, "invalid"},
		{"repository holding the mount", api.NewArgs{Dir: filepath.Join(holder, "src")}, "invalid"},
		{"below a checkout", api.NewArgs{Dir: filepath.Join(f.mount, "parent", "sub")}, "invalid"},
		{"folder-only flags", api.NewArgs{Dir: filepath.Join(f.mount, "parent"), Rules: []string{"local sub/**"}}, "invalid"},
		{"no such checkout", api.NewArgs{Dir: filepath.Join(f.mount, "gone")}, "not_found"},
	} {
		_, err := f.reg.New(ctx, tt.args)
		if coded := (*Error)(nil); !errors.As(err, &coded) || coded.Code != tt.code {
			t.Errorf("%s: err = %v, want %s", tt.name, err, tt.code)
		}
	}
	if n := len(f.ls().Checkouts); n != 3 {
		t.Fatalf("checkouts = %d, want parent, child and parent-2", n)
	}

	// A case-insensitive volume reaches the mount in another letter case, which
	// only file identity tells from an outside folder.
	upper := strings.ToUpper(f.mount)
	if !exists(upper) {
		return
	}
	variant, err := f.reg.New(ctx, api.NewArgs{Dir: filepath.Join(upper, "parent"), Name: "variant"})
	if err != nil || variant.Checkout.Parent == "" || variant.Checkout.Base != parent.Base {
		t.Fatalf("new on a checkout in another letter case = %+v, %v", variant, err)
	}
	_, err = f.reg.New(ctx, api.NewArgs{Dir: strings.ToUpper(holder)})
	if coded := (*Error)(nil); !errors.As(err, &coded) || coded.Code != "invalid" {
		t.Fatalf("folder holding the mount in another letter case: err = %v, want invalid", err)
	}
}

// gitRun runs git in dir for repository fixtures.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitdir is the private git directory of checkout name, or where one named name
// would be when no such checkout exists.
func (f *fixture) gitdir(name string) string {
	f.reg.mu.RLock()
	defer f.reg.mu.RUnlock()
	if h := f.reg.checkouts[name]; h != nil {
		return f.reg.gitdirPath(h.meta.dir())
	}
	return f.reg.gitdirPath(name)
}

// checkoutDirs lists the checkout directories below state/kind ("uppers" or "git"),
// leaving out those of checkouts prepared for later creates (see reuseFolder).
func checkoutDirs(t *testing.T, state, kind string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, kind))
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), preparedPrefix) {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// TestRepoCheckoutAndPlan checks git facts in a repository checkout and how plan splits
// files between the origin (pushed, unchanged) and this machine.
func TestRepoCheckoutAndPlan(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	f := newFixture(t)
	dir := folder(t, map[string]string{"tracked.txt": "t", "changed.txt": "before", ".gitignore": "build/\n*.log\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	remote := t.TempDir()
	gitRun(t, remote, "init", "-q", "--bare", "-b", "main")
	gitRun(t, dir, "remote", "add", "origin", remote)
	gitRun(t, dir, "push", "-q", "-u", "origin", "main")
	write(t, filepath.Join(dir, "changed.txt"), "after")
	write(t, filepath.Join(dir, "new.txt"), "untracked")
	write(t, filepath.Join(dir, "build/out.o"), "object")
	write(t, filepath.Join(dir, "debug.log"), "log")
	time.Sleep(10 * time.Millisecond)
	reply := f.newCheckout(dir, "repo")
	if !reply.Checkout.Git {
		t.Fatalf("repository checkout without git: %+v", reply)
	}
	if got := f.read("repo", ".git"); !strings.HasPrefix(got, "gitdir: ") {
		t.Fatalf(".git in checkout = %q", got)
	}
	if got := f.snapFiles("repo"); strings.Join(got, " ") != ".gitignore changed.txt new.txt tracked.txt" {
		t.Fatalf("snapshot files = %v", got)
	}
	plan, err := f.reg.Plan(context.Background(), api.PlanArgs{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Repos) != 1 || !plan.Repos[0].Pushed || plan.Repos[0].Upstream != "origin/main" {
		t.Fatalf("plan repos = %+v", plan.Repos)
	}
	if plan.Origin.Files != 2 || plan.Here.Files != 2 || plan.Local.Files != 1 || len(plan.Cache) != 1 || plan.Cache[0].Root != "build" {
		t.Fatalf("plan = origin %+v here %+v local %+v cache %+v", plan.Origin, plan.Here, plan.Local, plan.Cache)
	}
	checkoutPlan, err := f.reg.Plan(context.Background(), api.PlanArgs{Checkout: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if checkoutPlan.Origin.Files != 2 || checkoutPlan.Here.Files != 2 {
		t.Fatalf("checkout plan = origin %+v here %+v", checkoutPlan.Origin, checkoutPlan.Here)
	}
}

// TestSessionDoneConcurrentAndReuse checks that caller-owned storage releases
// exactly once, remains safe on failed Enter, and pins a new checkout on reuse.
func TestSessionDoneConcurrentAndReuse(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "base"})
	f.newCheckout(dir, "first")
	f.newCheckout(dir, "second")
	var s Session
	s.Done()
	(*Session)(nil).Done()
	defer s.Done()
	for _, name := range []string{"first", "second"} {
		if err := f.reg.Enter(name, &s); err != nil {
			t.Fatal(err)
		}
		h := s.record
		if h.freeze.TryLock() {
			h.freeze.Unlock()
			t.Fatal("session did not pin its checkout")
		}
		start := make(chan struct{})
		var calls sync.WaitGroup
		for range 16 {
			calls.Go(func() {
				<-start
				s.Done()
			})
		}
		close(start)
		calls.Wait()
		s.Done()
		if !h.freeze.TryLock() {
			t.Fatal("Done did not release the checkout")
		}
		h.freeze.Unlock()
		if err := f.reg.Enter("missing", &s); !isCode(err, "not_found") {
			t.Fatalf("missing checkout = %v", err)
		}
		s.Done()
	}
}

// TestSessionEnterDoesNotAllocate guards the per-file-call ownership contract.
func TestSessionEnterDoesNotAllocate(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"file": "base"}), "app")
	allocs := testing.AllocsPerRun(1000, func() {
		var s Session
		if err := f.reg.Enter("app", &s); err != nil {
			t.Fatal(err)
		}
		s.Done()
	})
	if allocs != 0 {
		t.Fatalf("Enter/Done allocated %g objects per call", allocs)
	}
}

// TestFreezeWaitsOutsideRegistryLock checks that snap, fork and rm wait out a
// checkout's file calls without the registry lock, so a slow call stalls only
// its own checkout and every other one keeps serving.
func TestFreezeWaitsOutsideRegistryLock(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "a"})
	f.newCheckout(dir, "other")
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		op   func(name string) error
	}{
		{"snap", func(name string) error { _, err := f.reg.Snap(ctx, api.SnapArgs{Checkout: name}); return err }},
		{"fork", func(name string) error {
			_, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: name, Name: name + "-child"})
			return err
		}},
		{"rm", func(name string) error { return f.reg.Rm(ctx, api.RmArgs{Checkout: name}) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f.newCheckout(dir, tt.name)
			var call Session // a slow file call in flight
			if err := f.reg.Enter(tt.name, &call); err != nil {
				t.Fatal(err)
			}
			f.reg.mu.RLock()
			h := f.reg.checkouts[tt.name]
			f.reg.mu.RUnlock()
			done := make(chan error, 1)
			go func() { done <- tt.op(tt.name) }()
			// A writer queued for the freeze lock makes TryRLock fail.
			for deadline := time.Now().Add(5 * time.Second); h.freeze.TryRLock(); {
				h.freeze.RUnlock()
				select {
				case err := <-done:
					call.Done()
					t.Fatalf("%s finished during a file call: %v", tt.name, err)
				default:
				}
				if time.Now().After(deadline) {
					call.Done()
					t.Fatalf("%s never waited for the file call", tt.name)
				}
				time.Sleep(time.Millisecond)
			}
			entered := make(chan error, 1)
			go func() {
				var s Session
				err := f.reg.Enter("other", &s)
				if err == nil {
					s.Done()
				}
				entered <- err
			}()
			select {
			case err := <-entered:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				call.Done()
				t.Fatalf("checkout other stalled while %s waited for a file call", tt.name)
			}
			call.Done()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
