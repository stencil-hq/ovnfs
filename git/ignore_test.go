package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIgnoreMatchesGit compares the frozen-view matcher with canonical check-ignore.
func TestIgnoreMatchesGit(t *testing.T) {
	r := testRepo(t)
	global := filepath.Join(t.TempDir(), "global-ignore")
	testWrite(t, global, "*.global\nglobal-dir/\n")
	testGit(t, r.Top, "config", "core.excludesFile", global)
	testWrite(t, filepath.Join(r.CommonDir, "info", "exclude"), "*.info\nshared-only\n")
	testWrite(t, filepath.Join(r.Top, ".gitignore"), "\ufeff# comment\n*.log\n!root.keep.log\n/root-only\nbuild/\n!build/keep\nonly-dir/\nnegated/*\n!negated/keep\nanchored/path\n**/generated/**\n\\#literal\n\\!literal\nspace\\ \npadded\\  \nreinclude.global\n!reinclude.global\n")
	testWrite(t, filepath.Join(r.Top, "sub", ".gitignore"), "!keep.log\n/local-only\nlocal-dir/\n*.tmp\n!keep.tmp\n")
	testWrite(t, filepath.Join(r.Top, "build", ".gitignore"), "!keep\n")
	testWrite(t, filepath.Join(r.Top, "trailing", ".gitignore"), "!keep\n")
	extra, err := os.OpenFile(filepath.Join(r.Top, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.WriteString("*.tmp\n!parent.log\na/**/b/c\nemb**edded\ntrailing/**\nclass[!0-9]\nnumber[[:digit:]]\nword[[:alpha:]]\n"); err != nil {
		extra.Close()
		t.Fatal(err)
	}
	if err := extra.Close(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		dir  bool
	}{
		{"plain", false}, {"error.log", false}, {"root.keep.log", false},
		{"sub/error.log", false}, {"sub/keep.log", false}, {"sub/deep/keep.log", false},
		{"root-only", false}, {"sub/root-only", false}, {"sub/local-only", false}, {"sub/deep/local-only", false},
		{"build", true}, {"build/output", false}, {"build/keep", false}, {"sub/build/output", false},
		{"only-dir", false}, {"sub/only-dir", true}, {"sub/only-dir/file", false},
		{"negated/drop", false}, {"negated/keep", false}, {"negated/keep-dir/file", false},
		{"anchored/path", false}, {"sub/anchored/path", false},
		{"generated/root", false}, {"sub/generated/deep/file", false},
		{"#literal", false}, {"!literal", false}, {"space ", false},
		{"padded ", false}, {"padded  ", false},
		{"x.global", false}, {"sub/x.global", false}, {"reinclude.global", false},
		{"x.info", false}, {"sub/x.info", false}, {"shared-only", false},
		{"global-dir/file", false}, {"sub/global-dir/file", false},
		{"sub/local-dir/file", false}, {"sub/x.tmp", false}, {"sub/keep.tmp", false},
		{"parent.log/drop.tmp", false}, {"a/b/x/b/c", false},
		{"embedded", false}, {"embZZedded", false},
		{"trailing", true}, {"trailing/drop", false}, {"trailing/keep", false},
		{"classx", false}, {"class1", false},
		{"number1", false}, {"numberx", false},
		{"wordA", false}, {"word1", false},
	}
	for _, candidate := range cases {
		name := filepath.Join(r.Top, candidate.name)
		if candidate.dir {
			if err := os.MkdirAll(name, 0755); err != nil {
				t.Fatal(err)
			}
		} else {
			testWrite(t, name, "fixture\n")
		}
	}
	matcher, err := testIgnore(r, r.Top)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range cases {
		cmd := exec.Command("git", "-C", r.Top, "check-ignore", "--no-index", "-q", "--", candidate.name)
		err := cmd.Run()
		want := err == nil
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("check-ignore %s: %v", candidate.name, err)
			}
		}
		if got := matcher.Ignored(candidate.name, candidate.dir); got != want {
			t.Errorf("Ignored(%q, %v) = %v, git = %v", candidate.name, candidate.dir, got, want)
		}
	}
}

// TestIgnoreUsesFrozenView ensures rules come from the supplied reader, not the live checkout.
func TestIgnoreUsesFrozenView(t *testing.T) {
	r := testRepo(t)
	testWrite(t, filepath.Join(r.Top, ".gitignore"), "live-only\n")
	view := t.TempDir()
	testWrite(t, filepath.Join(view, ".gitignore"), "frozen-only\n")
	i, err := testIgnore(r, view)
	if err != nil {
		t.Fatal(err)
	}
	if !i.Ignored("frozen-only", false) || i.Ignored("live-only", false) {
		t.Fatal("matcher read rules from live checkout")
	}
}
