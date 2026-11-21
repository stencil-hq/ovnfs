package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMarkCheckout rewrites the marker of a copied private git config as git would
// read it, keeping every other line, and leaves a correct marker file untouched.
func TestMarkCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	get := func(config string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"config", "--file", config}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git config %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cases := map[string]string{
		"no section, no final newline": "[core]\n\tbare = false\n# trailing comment",
		"inherited marker":             "[core]\n\tbare = false\n[ovnfs]\n\tcheckout = parent\n[user]\n\tname = me\n",
		"inline and continued marker":  "[OVNFS] Checkout = parent\n[core]\n\tbare = false\n[ovnfs]\n\tcheckout = par\\\nent\n\tother = kept\n",
		"subsection kept":              "[ovnfs \"sub\"]\n\tcheckout = other\n[ovnfs]\n\t; checkout = commented\n\tcheckout = parent\n",
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			for _, marker := range []string{"child", `odd "name"; #x\`} {
				dir := t.TempDir()
				file := filepath.Join(dir, "config")
				if err := os.WriteFile(file, []byte(config), 0o640); err != nil {
					t.Fatal(err)
				}
				// Windows reports 0640 as 0666, so compare with what the host recorded.
				created, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				before := get(file, "--list")
				if err := MarkCheckout(dir, marker); err != nil {
					t.Fatal(err)
				}
				if got := get(file, "--get-all", "ovnfs.checkout"); got != marker {
					t.Fatalf("marker = %q, want %q", got, marker)
				}
				var kept []string
				for _, line := range strings.Split(before, "\n") {
					if !strings.HasPrefix(line, "ovnfs.checkout=") {
						kept = append(kept, line)
					}
				}
				after := get(file, "--list")
				for _, line := range kept {
					if !strings.Contains(after, line) {
						t.Fatalf("lost %q:\n%s", line, after)
					}
				}
				info, err := os.Stat(file)
				if err != nil || info.Mode().Perm() != created.Mode().Perm() {
					t.Fatalf("config mode %v, %v", info.Mode(), err)
				}
				old := time.Unix(1, 0)
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
				if err := MarkCheckout(dir, marker); err != nil {
					t.Fatal(err)
				}
				if info, err := os.Stat(file); err != nil || !info.ModTime().Equal(old) {
					t.Fatalf("marked config rewritten: %v", err)
				}
			}
		})
	}
}

// TestModuleDir places a submodule absorbed into its parent's modules directory, by
// a relative or an absolute gitfile, and no other nested repository.
func TestModuleDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	work := t.TempDir()
	sub, app := filepath.Join(work, "sub"), filepath.Join(work, "app")
	for _, dir := range []string{sub, app} {
		git(work, "init", "-q", dir)
		git(dir, "commit", "-q", "--allow-empty", "-m", "init")
	}
	git(app, "submodule", "add", "-q", sub, "vendor/sub")
	git(app, "submodule", "add", "-q", sub, "vendor/abs")
	parent, err := Open(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "vendor", "abs", ".git"), []byte("gitdir: "+filepath.Join(parent.CommonDir, "modules", "vendor", "abs")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(app, "init", "-q", "own")
	git(work, "init", "-q", "--separate-git-dir", filepath.Join(work, "elsewhere.git"), filepath.Join(app, "separate"))
	for rel, want := range map[string]string{"vendor/sub": "modules/vendor/sub", "vendor/abs": "modules/vendor/abs", "own": "", "separate": ""} {
		repo, err := Open(filepath.Join(app, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := parent.ModuleDir(repo); got != want {
			t.Errorf("%s: ModuleDir = %q, want %q", rel, got, want)
		}
	}
}

// TestUnsetModuleWorktrees clears the recorded work tree of every submodule git
// directory below a private git directory, nested ones and names with slashes
// included, and of nothing else.
func TestUnsetModuleWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	gitdir := t.TempDir()
	config := "[core]\n\tworktree = ../../elsewhere\n"
	for _, dir := range []string{"", "modules/vendor/sub", "modules/vendor/sub/modules/lib/inner", "modules/dep.lock"} {
		d := filepath.Join(gitdir, filepath.FromSlash(dir))
		if err := os.MkdirAll(filepath.Join(d, "refs"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string]string{"HEAD": "ref: refs/heads/main\n", "config": config} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := UnsetModuleWorktrees(gitdir); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{"": true, "modules/vendor/sub": false, "modules/vendor/sub/modules/lib/inner": false, "modules/dep.lock": false} {
		data, err := os.ReadFile(filepath.Join(gitdir, filepath.FromSlash(dir), "config"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(data), "worktree"); got != want {
			t.Errorf("%q: core.worktree kept = %v, want %v", dir, got, want)
		}
	}
}

// TestMakeGitdirConfig gives a private git directory the repository's config with
// the checkout's core settings, as git config would have written them: every
// earlier value replaced, core.worktree gone, everything else kept.
func TestMakeGitdirConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	r := testRepo(t)
	source := "[core] worktree = /elsewhere\n" +
		"\trepositoryformatversion = 0\n" +
		"\tBare = true\n" +
		"\tcheckstat = default\n" +
		"\tfsmonitor = tr\\\nue\n" +
		"[core \"sub\"]\n\tbare = true\n" +
		"[user]\n\tname = me\n" +
		"[core]\n\tuntrackedCache = true\n\ttrustctime = true\n\tautocrlf = input\n"
	if err := os.WriteFile(filepath.Join(r.CommonDir, "config"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(t.TempDir(), "git")
	objects := filepath.Join(t.TempDir(), "objects")
	if err := r.PrepareObjects(objects); err != nil {
		t.Fatal(err)
	}
	if err := r.MakeGitdir(gitdir, objects, r.IndexPath()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(gitdir, "config")
	out, err := exec.Command("git", "config", "--file", file, "--list").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --list: %v\n%s", err, out)
	}
	got := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		got[key] = append(got[key], value)
	}
	want := map[string][]string{
		"core.repositoryformatversion": {"0"},
		"core.bare":                    {"false"},
		"core.checkstat":               {"minimal"},
		"core.trustctime":              {"false"},
		"core.untrackedcache":          {"false"},
		"core.fsmonitor":               {"false"},
		"core.autocrlf":                {"input"},
		"core.sub.bare":                {"true"},
		"user.name":                    {"me"},
	}
	if len(got) != len(want) {
		t.Fatalf("config = %v, want %v", got, want)
	}
	for key, values := range want {
		if strings.Join(got[key], ",") != strings.Join(values, ",") {
			t.Errorf("%s = %v, want %v", key, got[key], values)
		}
	}
}

// TestLinkHooksKeepsLinkWhenStatFails keeps the link LinkHooks made when the
// repository's hooks cannot be told apart from gone, such as under an unreadable
// .git: only hooks that are really gone remove it.
func TestLinkHooksKeepsLinkWhenStatFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions do not deny access here")
	}
	repo := testRepo(t)
	hooks := filepath.Join(repo.CommonDir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := t.TempDir()
	if err := repo.LinkHooks(gitdir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repo.CommonDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(repo.CommonDir, 0o755) })
	if err := repo.LinkHooks(gitdir); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(gitdir, "hooks")); err != nil || target != hooks {
		t.Fatalf("link after a failed stat = %q, %v", target, err)
	}
}

// TestLinkHooksRemovesLinkToUnresolvableHooks treats a source hooks entry that can
// never resolve as gone, and removes the link made to it: a symlink loop, and a
// symlink to a path through a regular file.
func TestLinkHooksRemovesLinkToUnresolvableHooks(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		target func(hooks string) string
	}{
		{"loop", func(hooks string) string { return hooks }},
		{"through a file", func(string) string { return filepath.Join(file, "x") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := testRepo(t)
			hooks := filepath.Join(repo.CommonDir, "hooks")
			if err := os.RemoveAll(hooks); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			gitdir := t.TempDir()
			if err := repo.LinkHooks(gitdir); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(hooks); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.target(hooks), hooks); err != nil {
				t.Skipf("no symlinks: %v", err)
			}
			if err := repo.LinkHooks(gitdir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(gitdir, "hooks")); !os.IsNotExist(err) {
				t.Fatalf("link to unresolvable hooks stays: %v", err)
			}
		})
	}
}
