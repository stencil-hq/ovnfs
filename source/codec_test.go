package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// deepPath is a path hundreds of directories deep.
var deepPath = strings.Repeat("d/", 300) + "leaf.txt"

// sameResult compares the persisted fields of two captures, mtimes by instant.
func sameResult(t *testing.T, got, want Result) {
	t.Helper()
	if got.Root != want.Root || got.Tree != want.Tree || len(got.Repos) != len(want.Repos) || len(got.Roots) != len(want.Roots) || len(got.BlobRepo) != len(want.BlobRepo) || len(got.Info) != len(want.Info) {
		t.Fatalf("decoded %+v, want %+v", got, want)
	}
	for i := range want.Repos {
		if got.Repos[i] != want.Repos[i] {
			t.Fatalf("repo %d: %+v, want %+v", i, got.Repos[i], want.Repos[i])
		}
	}
	for p, c := range want.Roots {
		if got.Roots[p] != c {
			t.Fatalf("root %q: %q, want %q", p, got.Roots[p], c)
		}
	}
	for p, r := range want.BlobRepo {
		if got.BlobRepo[p] != r {
			t.Fatalf("blob repo %q: %q, want %q", p, got.BlobRepo[p], r)
		}
	}
	for p, i := range want.Info {
		g, ok := got.Info[p]
		if !ok || !g.Mtime.Equal(i.Mtime) || g.Size != i.Size || g.Ino != i.Ino {
			t.Fatalf("info %q: %+v, want %+v", p, g, i)
		}
	}
}

// TestResultBinaryRoundTrip keeps every persisted field, whatever the paths
// and stat values, including an empty capture.
func TestResultBinaryRoundTrip(t *testing.T) {
	full := Result{
		Root: "/home/ü/项目",
		Tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		Repos: []Repo{
			{Path: "", Head: "0123456789abcdef0123456789abcdef01234567", Index: "repo-0"},
			{Path: "vendor/lib", Head: "", Index: ""},
			{Path: "vendor/sub", Head: "89abcdef0123456789abcdef0123456789abcdef", Index: "repo-2", Module: "modules/vendor/sub"},
		},
		Roots: map[string]rules.Class{
			"node_modules": rules.Cache, ".env": rules.Local, "secret/ключ": rules.Hidden, ".git": rules.Local,
		},
		BlobRepo: map[string]string{"taken/файл.go": "vendor/lib", "taken/a": ""},
		Info: map[string]Info{
			"a":                 {Mtime: time.Unix(1_700_000_000, 123_456_789), Size: 1, Ino: 42},
			"a/b":               {Mtime: time.Unix(1_700_000_000, 0), Size: 0, Ino: 41},
			"ab":                {Mtime: time.Unix(-86_400, 999_999_999), Size: math.MaxInt64, Ino: math.MaxUint64},
			"日本/ファイル.txt":       {Mtime: time.Unix(1_600_000_000, 5).In(time.FixedZone("x", 3600)), Size: 77, Ino: 0},
			"emoji/🙂/\u00e9.md": {Mtime: time.Unix(1_700_000_001, 1), Size: 2, Ino: 7},
			"zero-time":         {Size: 3},
			deepPath:            {Mtime: time.Unix(1_700_000_002, 2), Size: 4, Ino: 8},
		},
	}
	for name, r := range map[string]Result{"empty": {}, "full": full} {
		t.Run(name, func(t *testing.T) {
			data, err := r.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			var got Result
			if err := got.UnmarshalBinary(data); err != nil {
				t.Fatal(err)
			}
			sameResult(t, got, r)
		})
	}
}

// TestResultBinaryReadsVersion1 decodes a capture persisted before repositories
// recorded their module, as a base created by an older build holds it.
func TestResultBinaryReadsVersion1(t *testing.T) {
	var e encoder
	e.str("/src")
	e.str("4b825dc642cb6eb9a060e54bf8d69288fbee4904")
	e.uvarint(1)
	e.str("")
	e.str("0123456789abcdef0123456789abcdef01234567")
	e.str("repo-0")
	for range 3 {
		e.paths(nil) // roots, taken files, infos
	}
	data, err := pack(resultMagicV1, e.buf)
	if err != nil {
		t.Fatal(err)
	}
	var got Result
	if err := got.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	sameResult(t, got, Result{Root: "/src", Tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904", Repos: []Repo{{Head: "0123456789abcdef0123456789abcdef01234567", Index: "repo-0"}}})
}

// TestResultBinaryRejectsDamage fails cleanly on truncated, altered or
// foreign input instead of decoding a different capture.
func TestResultBinaryRejectsDamage(t *testing.T) {
	r := Result{Root: "/src", Tree: "4b825dc642cb6eb9a060e54bf8d69288fbee4904", Info: map[string]Info{"a.txt": {Mtime: time.Unix(1, 2), Size: 3, Ino: 4}}, Roots: map[string]rules.Class{"tmp": rules.Cache}}
	data, err := r.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	damaged := map[string][]byte{"empty": nil, "legacy json": legacy, "checksum": bytes.Clone(data)}
	damaged["checksum"][len(resultMagic)] ^= 1
	for n := range len(data) {
		damaged[fmt.Sprintf("truncated to %d", n)] = data[:n]
	}
	for name, d := range damaged {
		var got Result
		if err := got.UnmarshalBinary(d); err == nil {
			t.Fatalf("%s: decoded %+v", name, got)
		}
	}
}

// storedTree stores files' blobs and tree for stat cache tests.
func storedTree(t *testing.T, st *store.Store, paths ...string) (store.OID, map[string]store.File) {
	t.Helper()
	files := map[string]store.File{}
	for _, p := range paths {
		id, err := st.PutBytes([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		files[p] = store.File{Mode: store.ModeFile, ID: id}
	}
	tree, err := st.Build(files)
	if err != nil {
		t.Fatal(err)
	}
	return tree, files
}

// TestStatCacheRoundTrip reloads exactly the entries that were not racily
// fresh, taking their paths, modes and ids from the capture's stored tree.
func TestStatCacheRoundTrip(t *testing.T) {
	st, err := store.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	tree, files := storedTree(t, st, "a.txt", "dir/ü.txt", "dir/日本.md", deepPath, "uncached", "racy")
	link, err := st.PutBytes([]byte("a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	files["link"] = store.File{Mode: store.ModeSymlink, ID: link}
	files["empty"] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
	if tree, err = st.Build(files); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-time.Hour).UnixNano()
	entries := map[string]statEntry{
		"a.txt":     {Size: 5, Mtime: old, Ctime: old + 1, Ino: math.MaxUint64, Mode: store.ModeFile, OID: files["a.txt"].ID},
		"dir/ü.txt": {Size: 0, Mtime: -old, Ctime: 0, Ino: 0, Mode: store.ModeFile, OID: files["dir/ü.txt"].ID},
		"dir/日本.md": {Size: 1 << 40, Mtime: old - 7, Ctime: old, Ino: 3, Mode: store.ModeFile, OID: files["dir/日本.md"].ID},
		deepPath:    {Size: 9, Mtime: old, Ctime: old, Ino: 2, Mode: store.ModeFile, OID: files[deepPath].ID},
		"racy":      {Size: 1, Mtime: now.UnixNano(), Ctime: now.UnixNano(), Ino: 9, Mode: store.ModeFile, OID: files["racy"].ID},
	}
	name := filepath.Join(t.TempDir(), "sources", "x.stat")
	if err := saveStatCache(name, tree, files, entries, now, "epoch-1"); err != nil {
		t.Fatal(err)
	}
	got := loadStatCache(name, st)
	delete(entries, "racy")
	if got.Epoch != "epoch-1" || !reflect.DeepEqual(got.Entries, entries) {
		t.Fatalf("loaded %+v, want %+v", got, entries)
	}

	empty := filepath.Join(t.TempDir(), "empty.stat")
	if err := saveStatCache(empty, store.EmptyTree, map[string]store.File{}, map[string]statEntry{}, now, "e"); err != nil {
		t.Fatal(err)
	}
	if got := loadStatCache(empty, st); got.Epoch != "e" || len(got.Entries) != 0 {
		t.Fatalf("empty cache loaded as %+v", got)
	}

	stray := map[string]statEntry{"not-captured": {Mode: store.ModeFile, OID: files["a.txt"].ID}}
	if err := saveStatCache(filepath.Join(t.TempDir(), "stray.stat"), tree, files, stray, now, "e"); err == nil {
		t.Fatal("saved an entry that is not a leaf of the capture")
	}
}

// TestStatCacheConcurrentSave keeps the published cache decodable when many
// writers save at once. A shared name+".tmp" could drop the first writer's
// file with ENOENT or let the second write into the inode after it was
// published; each writer now uses its own temp file.
func TestStatCacheConcurrentSave(t *testing.T) {
	st, err := store.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	tree, files := storedTree(t, st, "a.txt")
	now := time.Now()
	old := now.Add(-time.Hour).UnixNano()
	name := filepath.Join(t.TempDir(), "x.stat")
	const writers = 32
	want := make([]statCache, writers)
	errc := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		entries := map[string]statEntry{
			"a.txt": {Size: int64(i + 1), Mtime: old, Ctime: old, Ino: uint64(i + 1), Mode: store.ModeFile, OID: files["a.txt"].ID},
		}
		want[i] = statCache{Epoch: fmt.Sprintf("w-%d", i), Entries: entries}
		wg.Add(1)
		go func(entries map[string]statEntry, epoch string) {
			defer wg.Done()
			errc <- saveStatCache(name, tree, files, entries, now, epoch)
		}(entries, want[i].Epoch)
	}
	wg.Wait()
	close(errc)
	saved := 0
	for err := range errc {
		switch {
		case err == nil:
			saved++
		// Windows refuses to replace a file that another rename is replacing
		// at that instant (ERROR_ACCESS_DENIED): that save is lost, not torn.
		case runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission):
		default:
			t.Fatal(err)
		}
	}
	if saved == 0 {
		t.Fatal("no save was published")
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeStatCache(raw, st)
	if err != nil {
		t.Fatalf("published cache does not decode: %v", err)
	}
	matched := false
	for _, w := range want {
		if got.Epoch == w.Epoch && reflect.DeepEqual(got.Entries, w.Entries) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("loaded %+v, which is not any writer's cache", got)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(name), filepath.Base(name)+".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("leftover temp files %v", leftovers)
	}
}

// TestDamagedStatCacheIsIgnored hashes everything again when the cache is
// damaged, in an older format, or names a tree the store no longer has, and
// replaces it with one the next capture uses.
func TestDamagedStatCacheIsIgnored(t *testing.T) {
	st, err := store.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cache := filepath.Join(t.TempDir(), "x.stat")
	old := time.Now().Add(-time.Minute)
	for _, n := range []string{"a.txt", "ü/b.txt"} {
		write(t, filepath.Join(dir, n), n)
		if err := os.Chtimes(filepath.Join(dir, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	capture(t, dir, st, cache)
	good, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"epoch":"","entries":{"a.txt":{"s":5,"m":0,"c":0,"i":0,"x":33188,"o":"2e65efe2a145dda7ee51d1741299f848e5bf752e"}}}`
	for name, data := range map[string][]byte{
		"truncated": good[:len(good)/2],
		"garbage":   []byte("not a stat cache"),
		"empty":     {},
		"legacy":    []byte(legacy),
	} {
		if err := os.WriteFile(cache, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if res := capture(t, dir, st, cache); res.Stats.Hashed != 2 {
			t.Fatalf("%s cache: hashed %d, want 2", name, res.Stats.Hashed)
		}
		if res := capture(t, dir, st, cache); res.Stats.Hashed != 0 {
			t.Fatalf("%s cache: rewritten cache unused, hashed %d", name, res.Stats.Hashed)
		}
	}
	other, err := store.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res := capture(t, dir, other, cache); res.Stats.Hashed != 2 {
		t.Fatalf("cache naming a tree another store lacks: hashed %d, want 2", res.Stats.Hashed)
	}
}

// TestHashOnlyCaptureReadsCacheTrees lets a capture without a store (plan)
// reuse the stat cache through CacheTrees, and hash everything without it.
func TestHashOnlyCaptureReadsCacheTrees(t *testing.T) {
	st, err := store.Open(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cache := filepath.Join(t.TempDir(), "x.stat")
	write(t, filepath.Join(dir, "a.txt"), "a")
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "a.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	stored := capture(t, dir, st, cache)
	root, err := Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		trees  *store.Store
		hashed int
	}{{st, 0}, {nil, 1}} {
		res, err := Capture(context.Background(), Options{Root: root, Rules: rules.Set{Rules: rules.Builtins()}, Cache: cache, CacheTrees: c.trees})
		if err != nil {
			t.Fatal(err)
		}
		if res.Stats.Hashed != c.hashed || res.Files["a.txt"] != stored.Files["a.txt"] {
			t.Fatalf("CacheTrees %v: hashed %d, file %+v; want %d, %+v", c.trees != nil, res.Stats.Hashed, res.Files["a.txt"], c.hashed, stored.Files["a.txt"])
		}
	}
}

// MarshalBinary encodes a capture in the format UnmarshalBinary migrates from.
func (r *Result) MarshalBinary() ([]byte, error) {
	var e encoder
	e.str(r.Root)
	e.str(string(r.Tree))
	e.uvarint(uint64(len(r.Repos)))
	for _, repo := range r.Repos {
		e.str(repo.Path)
		e.str(repo.Head)
		e.str(repo.Index)
		e.str(repo.Module)
	}
	roots := SortedPaths(r.Roots)
	e.paths(roots)
	for _, p := range roots {
		e.str(string(r.Roots[p]))
	}
	taken := SortedPaths(r.BlobRepo)
	e.paths(taken)
	for _, p := range taken {
		e.str(r.BlobRepo[p])
	}
	infos := SortedPaths(r.Info)
	e.paths(infos)
	var prev int64
	for _, p := range infos {
		sec := r.Info[p].Mtime.Unix()
		e.varint(sec - prev)
		prev = sec
	}
	for _, p := range infos {
		e.uvarint(uint64(r.Info[p].Mtime.Nanosecond()))
	}
	for _, p := range infos {
		e.varint(r.Info[p].Size)
	}
	prev = 0
	for _, p := range infos {
		ino := int64(r.Info[p].Ino)
		e.varint(ino - prev)
		prev = ino
	}
	return pack(resultMagic, e.buf)
}
