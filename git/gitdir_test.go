package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	get := func(t *testing.T, config string, args ...string) string {
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
		"comment ending in backslash":  "[ovnfs]\n\tcheckout = /x ; old \\\n\tfoo = keep\n",
		"kept continuation":            "[ovnfs]\n\tnote = a\\\n\tcheckout = b\n[core]\n\tbare = false\n",
		"continued quoted value":       "[ovnfs]\n\tnote = \"a \\\n; b # c\"\n\tcheckout = parent\n[core]\n\tbare = false\n",
		"escaped backslash at end":     "[ovnfs]\n\tnote = a\\\\\n\tcheckout = parent\n[core]\n\tbare = false\n",
		"quoted bracket in header":     "[ovnfs \"a]b\"] checkout = x\n[ovnfs]\n\tcheckout = parent\n[core]\n\tbare = false\n",
		"inline variable after header": "[ovnfs] checkout = x\n\tfoo = keep\n[core]\n\tbare = false\n",
		"header value continued":       "[ovnfs] note = a \\\n\tcheckout = b\n[core]\n\tbare = false\n",
		"continued to end of file":     "[alias]\n\tst = status --short\\\n",
		"backslash at end of file":     "[alias]\n\tst = status --short\\",
		"header value to end of file":  "[ovnfs] note = a\\\n",
		"two headers on a line":        "[user]\n\tname = me\n[core][ovnfs] checkout = old\n",
		"target header before another": "[ovnfs][core] bare = true\n",
		"target header between others": "[ovnfs][core][ovnfs] checkout = x\n",
		"byte order mark":              "\xef\xbb\xbf[ovnfs]\n\tcheckout = old\n[core]\n\tbare = false\n",
		"CRLF continuation":            "[ovnfs]\r\n\tcheckout = par\\\r\nent\r\n\tfoo = keep\r\n[core]\r\n\tbare = false\r\n",
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
				before := get(t, file, "--list")
				if err := MarkCheckout(dir, marker); err != nil {
					t.Fatal(err)
				}
				if got := get(t, file, "--get-all", "ovnfs.checkout"); got != marker {
					t.Fatalf("marker = %q, want %q", got, marker)
				}
				withoutMarker := func(list string) []string {
					var rest []string
					for line := range strings.SplitSeq(list, "\n") {
						if !strings.HasPrefix(line, "ovnfs.checkout=") {
							rest = append(rest, line)
						}
					}
					return rest
				}
				kept := withoutMarker(before)
				after := get(t, file, "--list")
				if got := withoutMarker(after); !slices.Equal(got, kept) {
					t.Fatalf("variables changed:\nbefore %q\nafter  %q", kept, got)
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
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
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

// TestLinkHooks checks that LinkHooks links a git directory's hooks to the
// repository's when that is a directory, and changes nothing otherwise or when the
// git directory already has a hooks entry.
func TestLinkHooks(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, hooks, gitdir string)
		check func(t *testing.T, hooks, link string)
	}{
		{"hooks directory", func(t *testing.T, hooks, gitdir string) {}, func(t *testing.T, hooks, link string) {
			if got, err := os.Readlink(link); err != nil || got != hooks {
				t.Fatalf("link = %q, %v; want %s", got, err, hooks)
			}
		}},
		{"missing", func(t *testing.T, hooks, gitdir string) {
			if err := os.RemoveAll(hooks); err != nil {
				t.Fatal(err)
			}
		}, absent},
		{"a file", func(t *testing.T, hooks, gitdir string) {
			if err := os.RemoveAll(hooks); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(hooks, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, absent},
		{"existing directory", func(t *testing.T, hooks, gitdir string) {
			if err := os.Mkdir(filepath.Join(gitdir, "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, hooks, link string) {
			if info, err := os.Lstat(link); err != nil || !info.IsDir() {
				t.Fatalf("existing hooks directory replaced: %v, %v", info, err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// A linked worktree's own git directory holds no hooks: they are
			// the common directory's.
			wt := filepath.Join(t.TempDir(), "wt")
			testGit(t, testRepo(t).Top, "worktree", "add", "-q", wt)
			repo, err := Open(wt)
			if err != nil {
				t.Fatal(err)
			}
			hooks := filepath.Join(repo.CommonDir, "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			gitdir := t.TempDir()
			c.setup(t, hooks, gitdir)
			if err := repo.LinkHooks(gitdir); err != nil {
				t.Fatal(err)
			}
			c.check(t, hooks, filepath.Join(gitdir, "hooks"))
		})
	}
}

// absent checks that no hooks entry exists at link.
func absent(t *testing.T, _, link string) {
	t.Helper()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("hooks entry exists: %v", err)
	}
}
