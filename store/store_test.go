package store

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// openStore opens a fresh store with a 64 MiB blob cache in a temp directory.
func openStore(t *testing.T) *Store {
	t.Helper()
	return openStoreCache(t, 64<<20)
}

// openStoreCache opens a fresh store with the given cache budget in a temp directory.
func openStoreCache(t *testing.T, cache int64) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), cache)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// age marks stored object id as last stored at: a packed object's freshness, a loose one's mtime.
func age(t *testing.T, s *Store, id OID, at time.Time) {
	t.Helper()
	r := rawOf(id)
	s.packs.touchMu.Lock()
	s.packs.mu.Lock()
	x, i, e, ok := s.packs.locate(r)
	if x != nil {
		x.setAt(i, at.UnixNano())
	} else if ok {
		e.at = at.UnixNano()
		s.packs.entries[r] = e
	}
	s.packs.mu.Unlock()
	s.packs.touchMu.Unlock()
	if !ok {
		if err := os.Chtimes(s.objectPath(id), at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// forget drops stored object id's copy in the active pack or its loose copy,
// leaving any cached body.
func forget(t *testing.T, s *Store, id OID) {
	t.Helper()
	s.packs.mu.Lock()
	x, _, _, ok := s.packs.locate(rawOf(id))
	delete(s.packs.entries, rawOf(id))
	s.packs.mu.Unlock()
	if x != nil {
		t.Fatalf("%s is in a sealed pack", id)
	}
	if !ok {
		if err := removeFile(s.objectPath(id)); err != nil {
			t.Fatal(err)
		}
	}
	if s.Has(id) {
		t.Fatalf("%s still stored", id)
	}
}

// packedObjects returns every packed object as lookups find it.
func packedObjects(s *Store) map[rawID]packed {
	s.packs.mu.RLock()
	defer s.packs.mu.RUnlock()
	objects := map[rawID]packed{}
	for _, x := range s.packs.sources() {
		for i := range x.n {
			if _, ok := objects[x.id(i)]; !ok {
				e := x.entry(i)
				e.at = x.at(i)
				objects[x.id(i)] = e
			}
		}
	}
	for id, e := range s.packs.entries {
		if _, ok := objects[id]; !ok {
			objects[id] = e
		}
	}
	return objects
}

// gitRepo creates a SHA-1 repository in a temp directory, skipping when git is unavailable.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "", "init", "-q", "--object-format=sha1")
	return dir
}

// runGit runs git in dir with stdin and returns trimmed stdout.
func runGit(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.symlinks=true", "-c", "core.filemode=true"}, args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBlobIDsMatchGit checks HashFile, PutFile, PutBytes and HashBytes agree with git hash-object.
func TestBlobIDsMatchGit(t *testing.T) {
	repo := gitRepo(t)
	s := openStore(t)
	for _, content := range []string{"", "hello\n", "no newline", strings.Repeat("x\x00y", 50000)} {
		p := filepath.Join(repo, "f")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		want := OID(runGit(t, repo, "", "hash-object", "f"))
		got, size, err := HashFile(p)
		if err != nil || got != want || size != int64(len(content)) {
			t.Fatalf("HashFile = %s %d %v, want %s %d", got, size, err, want, len(content))
		}
		put, size, err := s.PutFile(p)
		if err != nil || put != want || size != int64(len(content)) {
			t.Fatalf("PutFile = %s %d %v, want %s", put, size, err, want)
		}
		if b, err := s.PutBytes([]byte(content)); err != nil || b != want {
			t.Fatalf("PutBytes = %s %v, want %s", b, err, want)
		}
		if h := HashBytes("blob", []byte(content)); h != want {
			t.Fatalf("HashBytes = %s, want %s", h, want)
		}
		if data, err := s.ReadBlob(want); err != nil || string(data) != content {
			t.Fatalf("ReadBlob mismatch: %v", err)
		}
	}
	if HashBytes("tree", nil) != EmptyTree {
		t.Fatal("empty tree id mismatch")
	}
}

// TestTreeIDsMatchGitWriteTree builds a working tree with names that sort
// differently as directories, an exec bit, a symlink and nesting, and checks
// Build reproduces git write-tree's root id.
func TestTreeIDsMatchGitWriteTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks and exec bits")
	}
	repo := gitRepo(t)
	write := func(p, content string, mode os.FileMode) {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a.b", "dot\n", 0o644)
	write("a/x", "in a\n", 0o644)
	write("a0", "zero\n", 0o644)
	write("a-", "dash\n", 0o644)
	write("run.sh", "#!/bin/sh\n", 0o755)
	write("deep/er/still/leaf.txt", "leaf\n", 0o644)
	write("deep/er/still.txt", "sibling\n", 0o644)
	if err := os.Symlink("a/x", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "", "add", "-A")
	want := OID(runGit(t, repo, "", "write-tree"))

	s := openStore(t)
	files := map[string]File{}
	err := filepath.Walk(repo, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repo, p)
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		mode, ok := ModeOf(info.Mode())
		if !ok {
			t.Fatalf("unexpected type %s", rel)
		}
		var id OID
		if mode == ModeSymlink {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			id, err = s.PutBytes([]byte(target))
			if err != nil {
				return err
			}
		} else if id, _, err = s.PutFile(p); err != nil {
			return err
		}
		files[rel] = File{Mode: mode, ID: id}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Build(files)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Build = %s, git write-tree = %s", got, want)
	}
	if !s.Has(got) {
		t.Fatal("root tree not stored")
	}
}

// TestPutTreeMatchesGitMktree checks entry ordering and encoding against git mktree, including an empty subdirectory.
func TestPutTreeMatchesGitMktree(t *testing.T) {
	repo := gitRepo(t)
	s := openStore(t)
	blob, err := s.PutBytes([]byte("content\n"))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.PutTree([]Entry{{Name: "inner", Mode: ModeFile, ID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Name: "a0", Mode: ModeFile, ID: blob},
		{Name: "a", Mode: ModeDir, ID: sub},
		{Name: "a.b", Mode: ModeExec, ID: blob},
		{Name: "empty", Mode: ModeDir, ID: EmptyTree},
		{Name: "lnk", Mode: ModeSymlink, ID: blob},
		{Name: "a-", Mode: ModeFile, ID: blob},
	}
	var in strings.Builder
	for _, e := range entries {
		kind := "blob"
		if e.Mode == ModeDir {
			kind = "tree"
		}
		fmt.Fprintf(&in, "%06o %s %s\t%s\n", e.Mode, kind, e.ID, e.Name)
	}
	want := OID(runGit(t, repo, in.String(), "mktree", "--missing"))
	got, err := s.PutTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("PutTree = %s, git mktree = %s", got, want)
	}
	read, err := s.ReadTree(got)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range read {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "a-,a.b,a,a0,empty,lnk" {
		t.Fatalf("ReadTree order %v", names)
	}
}

// TestPutTreeRejects checks malformed entries are refused.
func TestPutTreeRejects(t *testing.T) {
	s := openStore(t)
	blob := HashBytes("blob", []byte("x"))
	cases := map[string][]Entry{
		"empty name":     {{Name: "", Mode: ModeFile, ID: blob}},
		"slash":          {{Name: "a/b", Mode: ModeFile, ID: blob}},
		"nul":            {{Name: "a\x00b", Mode: ModeFile, ID: blob}},
		"dotdot":         {{Name: "..", Mode: ModeFile, ID: blob}},
		"duplicate":      {{Name: "a", Mode: ModeFile, ID: blob}, {Name: "a", Mode: ModeExec, ID: blob}},
		"dup file + dir": {{Name: "a", Mode: ModeFile, ID: blob}, {Name: "a", Mode: ModeDir, ID: EmptyTree}},
		"bad mode":       {{Name: "a", Mode: 0o100664, ID: blob}},
		"bad id":         {{Name: "a", Mode: ModeFile, ID: "ABC"}},
	}
	for name, entries := range cases {
		if _, err := s.PutTree(entries); err == nil {
			t.Errorf("%s: PutTree accepted %v", name, entries)
		}
	}
	if entries, err := s.ReadTree(EmptyTree); err != nil || len(entries) != 0 {
		t.Fatalf("ReadTree(EmptyTree) = %v, %v", entries, err)
	}
}

// TestPutFileConcurrentRewrite stores a file while another goroutine keeps
// rewriting it, through a clone where the filesystem has one and through the
// single-pass copy; every stored object must hash to the id PutFile returned.
func TestPutFileConcurrentRewrite(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(fmt.Sprintf("copy=%v", copied), func(t *testing.T) {
			testPutFileConcurrentRewrite(t, copied)
		})
	}
}

// testPutFileConcurrentRewrite is TestPutFileConcurrentRewrite for one staging path.
func testPutFileConcurrentRewrite(t *testing.T, copied bool) {
	dir := t.TempDir()
	s := openStore(t)
	p := filepath.Join(dir, "hot")
	if err := os.WriteFile(p, []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if copied {
		f, _, dev, err := openRegular(p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		s.noReflink.Store(dev, struct{}{})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			os.WriteFile(p, bytes.Repeat([]byte{byte('a' + i%26)}, 1+(i*7919)%(cloneFile*5/2)), 0o644)
		}
	})
	defer func() { close(stop); wg.Wait() }()
	ids := map[OID]bool{}
	for range 200 {
		id, size, err := s.PutFile(p)
		if err != nil {
			t.Fatal(err)
		}
		data, err := s.ReadBlob(id)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != size || HashBytes("blob", data) != id {
			t.Fatalf("stored %d bytes hashing to %s; PutFile returned %s size %d", len(data), HashBytes("blob", data), id, size)
		}
		r, err := s.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := io.ReadAll(r)
		r.Close()
		if err != nil || HashBytes("blob", opened) != id {
			t.Fatalf("Open(object) read %d bytes hashing to %s (%v), want %s", len(opened), HashBytes("blob", opened), err, id)
		}
		ids[id] = true
	}
	if len(ids) < 2 {
		t.Logf("writer never observed between puts (%d distinct ids)", len(ids))
	}
}

// TestStageCopyChangedSize stores the bytes actually read under their own id
// and size when the file's length differs from the size it was opened with.
func TestStageCopyChangedSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("now longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, opened := range []int64{4, 100} {
		in, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		stage := filepath.Join(dir, fmt.Sprint("stage", opened))
		id, size, err := stageCopy(in, opened, stage)
		in.Close()
		if err != nil {
			t.Fatal(err)
		}
		if want := HashBytes("blob", []byte("now longer")); id != want || size != 10 {
			t.Fatalf("opened at %d: %s size %d, want %s size 10", opened, id, size, want)
		}
		if data, _ := os.ReadFile(stage); string(data) != "now longer" {
			t.Fatalf("staged %q", data)
		}
	}
}

// TestPutFileRefusesNonRegular checks symlinks and directories are not stored.
func TestPutFileRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t)
	if _, _, err := s.PutFile(dir); err == nil {
		t.Fatal("PutFile stored a directory")
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutFile(link); err == nil {
		t.Fatal("PutFile followed a symlink")
	}
	if _, _, err := HashFile(link); err == nil {
		t.Fatal("HashFile followed a symlink")
	}
}

// TestCloneBlob checks a stored blob materializes with the requested mode and stays independent of the object.
func TestCloneBlob(t *testing.T) {
	s := openStore(t)
	id, err := s.PutBytes([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := s.CloneBlob(id, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := s.ReadBlob(id); err != nil || string(data) != "payload" {
		t.Fatalf("object changed through clone: %q %v", data, err)
	}
	if err := s.CloneBlob(HashBytes("blob", []byte("absent")), filepath.Join(t.TempDir(), "x"), 0o644); err == nil {
		t.Fatal("cloned an absent object")
	}
}

// TestFlattenBuildRoundTrip checks Build and Flatten are inverse, including empty directories.
func TestFlattenBuildRoundTrip(t *testing.T) {
	s := openStore(t)
	blob := func(c string) OID {
		id, err := s.PutBytes([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	files := map[string]File{
		"README":          {Mode: ModeFile, ID: blob("readme")},
		"bin/tool":        {Mode: ModeExec, ID: blob("tool")},
		"bin/link":        {Mode: ModeSymlink, ID: blob("tool")},
		"src/a/b/c.go":    {Mode: ModeFile, ID: blob("c")},
		"src/a.go":        {Mode: ModeFile, ID: blob("a")},
		"empty":           {Mode: ModeDir, ID: EmptyTree},
		"src/a/b/nothing": {Mode: ModeDir, ID: EmptyTree},
	}
	root, err := s.Build(files)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Flatten(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(files) {
		t.Fatalf("Flatten returned %d leaves, want %d: %v", len(got), len(files), got)
	}
	for p, f := range files {
		if got[p] != f {
			t.Errorf("%s: got %+v, want %+v", p, got[p], f)
		}
	}
	again, err := s.Build(got)
	if err != nil || again != root {
		t.Fatalf("rebuild = %s %v, want %s", again, err, root)
	}
	if empty, err := s.Build(nil); err != nil || empty != EmptyTree {
		t.Fatalf("Build(nil) = %s %v", empty, err)
	}
	if leaves, err := s.Flatten(EmptyTree); err != nil || len(leaves) != 0 {
		t.Fatalf("Flatten(EmptyTree) = %v %v", leaves, err)
	}
}

// TestBuildRejects checks leaf/parent conflicts and malformed paths fail.
func TestBuildRejects(t *testing.T) {
	s := openStore(t)
	blob := HashBytes("blob", []byte("x"))
	f := File{Mode: ModeFile, ID: blob}
	cases := map[string]map[string]File{
		"leaf is parent":      {"a": f, "a/b": f},
		"deep leaf is parent": {"a/b": f, "a/b/c/d": f},
		"empty dir is parent": {"a": {Mode: ModeDir, ID: EmptyTree}, "a/b": f},
		"empty path":          {"": f},
		"absolute":            {"/a": f},
		"double slash":        {"a//b": f},
		"dot":                 {"./a": f},
		"dotdot":              {"a/../b": f},
		"trailing slash":      {"a/": f},
		"dir leaf not empty":  {"a": {Mode: ModeDir, ID: blob}},
		"bad mode":            {"a": {Mode: 0o644, ID: blob}},
		"bad id":              {"a": {Mode: ModeFile, ID: "zz"}},
	}
	for name, files := range cases {
		if _, err := s.Build(files); err == nil {
			t.Errorf("%s: Build accepted %v", name, files)
		}
	}
}

// TestSweep checks reachable and fresh objects survive, old unreachable ones and stale staging files go.
func TestSweep(t *testing.T) {
	s := openStore(t)
	put := func(c string) OID {
		id, err := s.PutBytes([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Garbage dominates the pack, so Sweep rewrites it (see TestSweepPackThreshold).
	kept, nested, garbage, fresh := put("kept"), put("nested"), put(string(noisy("garbage", 4096))), put("fresh")
	absent := HashBytes("blob", []byte("never stored"))
	root, err := s.Build(map[string]File{
		"kept":         {Mode: ModeFile, ID: kept},
		"dir/sub/n":    {Mode: ModeFile, ID: nested},
		"dir/ghost":    {Mode: ModeFile, ID: absent},
		"dir/emptydir": {Mode: ModeDir, ID: EmptyTree},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldTree, err := s.PutTree([]Entry{{Name: "g", Mode: ModeFile, ID: garbage}})
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(s.tmp, "crashed-staging")
	staleNear := filepath.Join(s.objects, "ab", stagePrefix+"crashed")
	for _, p := range []string{stale, staleNear} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{stale, staleNear} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.Flatten(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if s.Has(f.ID) {
			age(t, s, f.ID, old)
		}
	}
	for _, id := range []OID{root, garbage, oldTree} {
		age(t, s, id, old)
	}
	n, err := s.Sweep([]OID{root}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("Sweep removed %d objects, want 2 (garbage blob and its tree)", n)
	}
	for _, id := range []OID{root, kept, nested, fresh} {
		if !s.Has(id) {
			t.Errorf("Sweep removed %s", id)
		}
	}
	for _, id := range []OID{garbage, oldTree} {
		if s.Has(id) {
			t.Errorf("Sweep kept %s", id)
		}
		if data, ok := s.Cached(id); ok {
			t.Errorf("swept %s still cached: %q", id, data)
		}
	}
	for _, p := range []string{stale, staleNear} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("stale staging file %s survived: %v", p, err)
		}
	}
	// Re-storing an old object refreshes it so a sweep without its root keeps it.
	age(t, s, kept, old)
	put("kept")
	if _, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !s.Has(kept) {
		t.Error("Sweep removed a just re-stored object")
	}
	if _, err := s.Sweep([]OID{HashBytes("tree", []byte("missing"))}, time.Now()); err == nil {
		t.Error("Sweep accepted a missing root")
	}
}

// TestSweepKeepsBuiltRoots keeps everything reachable from a tree Build returned
// after the sweep's cutoff, even when no caller has recorded it as a root yet,
// and rebuilds a tree removed after an earlier Build stored it.
func TestSweepKeepsBuiltRoots(t *testing.T) {
	s := openStore(t)
	blob, err := s.PutBytes([]byte("in flight"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]File{"dir/f": {Mode: ModeFile, ID: blob}}
	root, err := s.Build(files)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	all := []OID{blob, root}
	entries, err := s.ReadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, entries[0].ID)
	for _, id := range all {
		age(t, s, id, old)
	}
	epoch := s.Epoch()
	if n, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("Sweep removed %d objects of a just built tree: %v", n, err)
	}
	if s.Epoch() != epoch {
		t.Error("a sweep that removed nothing began a new epoch")
	}
	if n, err := s.Sweep(nil, time.Now().Add(time.Minute)); err != nil || n != 3 {
		t.Fatalf("Sweep past the build removed %d objects, want 3: %v", n, err)
	}
	if s.Epoch() == epoch {
		t.Error("removing objects kept the epoch")
	}
	again, err := s.Build(files)
	if err != nil || again != root {
		t.Fatalf("rebuild = %s %v, want %s", again, err, root)
	}
	if _, err := s.Flatten(again); err != nil {
		t.Fatalf("rebuilt tree unreadable: %v", err)
	}
	reopened, err := Open(filepath.Dir(s.objects), 0)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Epoch() != s.Epoch() {
		t.Error("epoch not persisted")
	}
}

// TestHoldDefersSweep keeps Sweep from removing anything until every Hold is released.
func TestHoldDefersSweep(t *testing.T) {
	s := openStore(t)
	id, err := s.PutBytes([]byte("reused"))
	if err != nil {
		t.Fatal(err)
	}
	age(t, s, id, time.Now().Add(-time.Hour))
	release := s.Hold()
	epoch := s.Epoch()
	done := make(chan int)
	go func() {
		n, _ := s.Sweep(nil, time.Now())
		done <- n
	}()
	select {
	case n := <-done:
		t.Fatalf("Sweep removed %d objects during a Hold", n)
	case <-time.After(100 * time.Millisecond):
	}
	if !s.Has(id) || s.Epoch() != epoch {
		t.Fatal("object removed or epoch changed during a Hold")
	}
	release()
	if n := <-done; n != 1 {
		t.Fatalf("Sweep after release removed %d objects, want 1", n)
	}
}

// TestCached checks Cached loads a stored blob on first use, serves it again
// from memory, and refuses absent or malformed ids; ReadBlob copies stay private.
func TestCached(t *testing.T) {
	s := openStore(t)
	src := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(src, []byte("from a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, _, err := s.PutFile(src)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if data, ok := s.Cached(id); !ok || string(data) != "from a file" {
			t.Fatalf("Cached = %q, %v", data, ok)
		}
	}
	// A resident blob stays served after its stored copy is gone, until a Sweep drops it.
	forget(t, s, id)
	if data, ok := s.Cached(id); !ok || string(data) != "from a file" {
		t.Fatalf("Cached after load = %q, %v", data, ok)
	}
	got, err := s.ReadBlob(id)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 'X'
	if data, _ := s.Cached(id); string(data) != "from a file" {
		t.Fatalf("modifying ReadBlob's result changed the cache: %q", data)
	}
	if _, ok := s.Cached(HashBytes("blob", []byte("absent"))); ok {
		t.Error("Cached reported an absent object")
	}
	if _, ok := s.Cached("not-an-id"); ok {
		t.Error("Cached accepted a malformed id")
	}
}

// TestCachedPutBytes checks a stored blob is resident at once, independent of the caller's slice.
func TestCachedPutBytes(t *testing.T) {
	s := openStore(t)
	data := []byte("symlink target")
	id, err := s.PutBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	copy(data, "XXXXXXX")
	forget(t, s, id)
	if got, ok := s.Cached(id); !ok || string(got) != "symlink target" {
		t.Fatalf("Cached = %q, %v", got, ok)
	}
}

// TestCachedPeerBlob checks a blob received through a BlobWriter is resident once committed.
func TestCachedPeerBlob(t *testing.T) {
	s := openStore(t)
	body := []byte("fetched from a peer")
	w, err := s.NewBlobWriter(int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range [][]byte{body[:7], body[7:]} {
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	id := HashBytes("blob", body)
	if err := w.Commit(id); err != nil {
		t.Fatal(err)
	}
	forget(t, s, id)
	if got, ok := s.Cached(id); !ok || !bytes.Equal(got, body) {
		t.Fatalf("Cached = %q, %v", got, ok)
	}
}

// TestCachedLimits checks blobs above the per-object limit or the whole budget stay on disk only.
func TestCachedLimits(t *testing.T) {
	s := openStore(t)
	big := bytes.Repeat([]byte("b"), cacheObjectLimit+1)
	id, err := s.PutBytes(big)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Cached(id); ok {
		t.Error("Cached admitted a blob above the per-object limit")
	}
	if data, err := s.ReadBlob(id); err != nil || !bytes.Equal(data, big) {
		t.Fatalf("ReadBlob of a large blob: %d bytes, %v", len(data), err)
	}
	if n := s.cache.resident(); n != 0 {
		t.Errorf("resident = %d after a large blob, want 0", n)
	}

	small := openStoreCache(t, 100)
	id, err = small.PutBytes(bytes.Repeat([]byte("s"), 101))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := small.Cached(id); ok {
		t.Error("Cached admitted a blob larger than the whole cache")
	}
}

// TestCacheBudget checks resident bytes never exceed the budget while far more
// than the budget is stored and read concurrently, and every read is correct.
func TestCacheBudget(t *testing.T) {
	const budget = 10 << 10
	s := openStoreCache(t, budget)
	var ids []OID
	var bodies [][]byte
	for i := range 64 {
		body := bytes.Repeat([]byte{byte('a' + i%26)}, 1000+i)
		id, err := s.PutBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		if n := s.cache.resident(); n > budget {
			t.Fatalf("resident = %d after %d puts, budget %d", n, i+1, budget)
		}
		ids, bodies = append(ids, id), append(bodies, body)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for round := range 4 {
				for i := range ids {
					k := (i*7 + g + round) % len(ids)
					data, ok := s.Cached(ids[k])
					if !ok || !bytes.Equal(data, bodies[k]) {
						t.Errorf("Cached(%d) = %d bytes, %v", k, len(data), ok)
						return
					}
					if n := s.cache.resident(); n > budget {
						t.Errorf("resident = %d, budget %d", n, budget)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

// TestCacheOff checks a zero budget disables the cache without affecting reads.
func TestCacheOff(t *testing.T) {
	s := openStoreCache(t, 0)
	id, err := s.PutBytes([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Cached(id); ok {
		t.Error("Cached served a blob with the cache off")
	}
	if data, err := s.ReadBlob(id); err != nil || string(data) != "payload" {
		t.Fatalf("ReadBlob = %q, %v", data, err)
	}
}
