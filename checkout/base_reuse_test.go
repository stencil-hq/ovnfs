package checkout

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

func TestBaseReuseFolderAndRestart(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"nested/file": "captured\n"})
	old := time.Unix(123456789, 0)
	if err := os.Chtimes(filepath.Join(dir, "nested/file"), old, old); err != nil {
		t.Fatal(err)
	}
	first := f.newCheckout(dir, "first")
	second := f.newCheckout(dir, "second")
	if first.Checkout.Base != second.Checkout.Base {
		t.Fatalf("unchanged folder bases differ: %s, %s", first.Checkout.Base, second.Checkout.Base)
	}
	if second.Capture == nil || second.Capture.Hashed != 0 {
		t.Fatalf("unchanged source content was rehashed: %+v", second.Capture)
	}
	captures, err := filepath.Glob(filepath.Join(f.state, "bases", "*", "capture.idx"))
	if err != nil || len(captures) != 1 {
		t.Fatalf("capture files = %v, %v", captures, err)
	}
	f.reg.mu.RLock()
	lineage := len(f.reg.lineage)
	f.reg.mu.RUnlock()
	if lineage != 0 || second.Checkout.Parent != "" {
		t.Fatalf("base reuse added lineage: %d, parent %q", lineage, second.Checkout.Parent)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	third := f.newCheckout(dir, "third")
	if third.Checkout.Base != second.Checkout.Base {
		t.Fatalf("restart lost base reuse: %s, %s", second.Checkout.Base, third.Checkout.Base)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "second"}); err != nil {
		t.Fatal(err)
	}
	if got := f.read("third", "nested/file"); got != "captured\n" {
		t.Fatalf("remaining checkout = %q", got)
	}
}

func TestBaseReuseInvalidatesCapturedMetadata(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, string)
	}{
		{"content", func(t *testing.T, dir string) { write(t, filepath.Join(dir, "nested/file"), "changed!\n") }},
		{"content with restored mtime", func(t *testing.T, dir string) {
			name := filepath.Join(dir, "nested/file")
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			write(t, name, "changed!\n")
			if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
		{"mtime", func(t *testing.T, dir string) {
			stamp := time.Unix(123456789, 0)
			if err := os.Chtimes(filepath.Join(dir, "nested/file"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory mtime", func(t *testing.T, dir string) {
			stamp := time.Unix(123456789, 0)
			if err := os.Chtimes(filepath.Join(dir, "nested"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}},
		{"mode", func(t *testing.T, dir string) {
			if runtime.GOOS == "windows" {
				t.Skip("Windows does not retain Unix permissions")
			}
			if err := os.Chmod(filepath.Join(dir, "nested/file"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"added", func(t *testing.T, dir string) { write(t, filepath.Join(dir, "nested/added"), "new\n") }},
		{"deleted", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "nested/file")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			dir := folder(t, map[string]string{"nested/file": "original\n"})
			first := f.newCheckout(dir, "first")
			root, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, dir)
			// A root-mtime shortcut must not hide changes below a source directory.
			if err := os.Chtimes(dir, root.ModTime(), root.ModTime()); err != nil {
				t.Fatal(err)
			}
			second := f.newCheckout(dir, "second")
			if first.Checkout.Base == second.Checkout.Base {
				t.Fatal("changed capture reused its old base")
			}
			third := f.newCheckout(dir, "third")
			if third.Checkout.Base != second.Checkout.Base {
				t.Fatal("new unchanged capture did not become reusable")
			}
		})
	}
}

func TestBaseReuseChecksManifestAndRules(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "content\n"})
	home := t.TempDir()
	f.reg.config.UserHome = home
	write(t, filepath.Join(home, ".o2/ovnfs.yml"), "env:\n  image: first\n")
	first := f.newCheckout(dir, "first")
	write(t, filepath.Join(home, ".o2/ovnfs.yml"), "env:\n  image: second\n")
	second := f.newCheckout(dir, "second")
	if first.Checkout.Base == second.Checkout.Base {
		t.Fatal("changed resolved manifest reused its old base")
	}
	third := f.newCheckout(dir, "third", "local /file")
	if third.Checkout.Base == second.Checkout.Base {
		t.Fatal("changed classification rules reused their old base")
	}
}

func TestBaseReuseGitPreservesIndependentState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "tracked\n", "staged": "initial\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "initial")
	write(t, filepath.Join(dir, "untracked"), "untracked\n")
	first := f.newCheckout(dir, "first")
	second := f.newCheckout(dir, "second")
	if first.Checkout.Base != second.Checkout.Base {
		t.Fatal("unchanged root git repository did not share its base")
	}
	// Private refs and indexes must remain independent despite the shared base.
	gitRun(t, dir, "--git-dir="+f.gitdir("first"), "symbolic-ref", "HEAD", "refs/heads/private")
	gitRun(t, dir, "--git-dir="+f.gitdir("first"), "update-index", "--force-remove", "staged")
	if got := gitRun(t, dir, "--git-dir="+f.gitdir("second"), "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Fatalf("second checkout HEAD = %q", got)
	}
	if got := gitRun(t, dir, "--git-dir="+f.gitdir("second"), "ls-files", "staged"); got != "staged" {
		t.Fatalf("second checkout index lost staged: %q", got)
	}
	third := f.newCheckout(dir, "third")
	if third.Checkout.Base != first.Checkout.Base {
		t.Fatal("private git changes invalidated the source's reusable base")
	}
	write(t, filepath.Join(dir, "untracked"), "modified\n")
	fourth := f.newCheckout(dir, "fourth")
	if fourth.Checkout.Base == third.Checkout.Base {
		t.Fatal("untracked change reused stale base")
	}
	gitRun(t, dir, "update-index", "--assume-unchanged", "file")
	fifth := f.newCheckout(dir, "fifth")
	if fifth.Checkout.Base == fourth.Checkout.Base {
		t.Fatal("changed git index reused stale base")
	}
	gitRun(t, dir, "config", "reuse.marker", "changed")
	sixth := f.newCheckout(dir, "sixth")
	if sixth.Checkout.Base == fifth.Checkout.Base {
		t.Fatal("changed git config reused stale base")
	}
	gitRun(t, dir, "branch", "another")
	seventh := f.newCheckout(dir, "seventh")
	if seventh.Checkout.Base == sixth.Checkout.Base {
		t.Fatal("changed git refs reused stale base")
	}
	gitRun(t, dir, "commit", "--allow-empty", "-qm", "new revision")
	eighth := f.newCheckout(dir, "eighth")
	if eighth.Checkout.Base == seventh.Checkout.Base {
		t.Fatal("changed git revision reused stale base")
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	ninth := f.newCheckout(dir, "ninth")
	if ninth.Checkout.Base != eighth.Checkout.Base {
		t.Fatal("restart lost git base reuse")
	}
	if got := f.read("second", "untracked"); got != "untracked\n" {
		t.Fatalf("old git capture content = %q", got)
	}
}

func TestBaseReuseBypassesRevisionAndTake(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "tracked\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-qm", "initial")
	first := f.newCheckout(dir, "first")
	for i, args := range []api.NewArgs{
		{Dir: dir, Name: "revision", Rev: "HEAD"},
		{Dir: dir, Name: "take", Take: []string{".:file"}},
	} {
		reply, err := f.reg.New(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if reply.Checkout.Base == first.Checkout.Base {
			t.Fatalf("special capture %d reused ordinary base", i)
		}
		f.reg.mu.RLock()
		key := f.reg.bases[reply.Checkout.Base].view.Meta().ReuseKey
		f.reg.mu.RUnlock()
		if key != "" {
			t.Fatalf("special capture %d was marked reusable", i)
		}
	}
	last := f.newCheckout(dir, "last")
	if last.Checkout.Base != first.Checkout.Base {
		t.Fatal("ordinary create did not return to ordinary base")
	}
}

func TestBaseReuseConcurrentCreateAndRemoval(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "content\n"})
	f.newCheckout(dir, "seed")
	start := make(chan struct{})
	errors := make(chan error, 9)
	for i := range 8 {
		go func(i int) {
			<-start
			_, err := f.reg.New(context.Background(), api.NewArgs{Dir: dir, Name: fmt.Sprintf("copy-%d", i)})
			errors <- err
		}(i)
	}
	go func() {
		<-start
		errors <- f.reg.Rm(context.Background(), api.RmArgs{Checkout: "seed"})
	}()
	close(start)
	for range 9 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	for i := range 8 {
		if got := f.read(fmt.Sprintf("copy-%d", i), "file"); got != "content\n" {
			t.Fatalf("copy-%d content after reopen = %q", i, got)
		}
	}
}

func TestBaseReuseRecoveryKeepsReferencedBase(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "content\n"})
	first := f.newCheckout(dir, "first")
	f.newCheckout(dir, "second")
	f.reg.mu.Lock()
	f.reg.intents["interrupted"] = diskIntent{Op: "creating", Checkout: "interrupted", Layer: first.Checkout.Base}
	err := f.reg.persistLocked()
	f.reg.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	for _, name := range []string{"first", "second"} {
		if got := f.read(name, "file"); got != "content\n" {
			t.Fatalf("%s after recovery = %q", name, got)
		}
	}
	third := f.newCheckout(dir, "third")
	if third.Checkout.Base != first.Checkout.Base {
		t.Fatal("recovery removed reusable capture")
	}
}
