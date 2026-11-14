package base

import (
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// indexFixture is a capture with the shapes the index must keep apart:
// siblings that sort between a directory and its children, leaves of every
// mode, stat data without an mtime and at the epoch, roots below source and
// non-source directories, and a taken blob.
func indexFixture() *source.Result {
	id := func(c string) store.OID { return store.OID(strings.Repeat(c, 40)) }
	return &source.Result{
		Root: "/src", Tree: id("e"),
		Files: map[string]store.File{
			"a":       {Mode: store.ModeFile, ID: id("1")},
			"a.c":     {Mode: store.ModeFile, ID: id("2")},
			"b/c":     {Mode: store.ModeExec, ID: id("3")},
			"b-c":     {Mode: store.ModeSymlink, ID: id("4")},
			"b/empty": {Mode: store.ModeDir, ID: store.EmptyTree},
			"b/ü 日本":  {Mode: store.ModeFile, ID: id("5")},
			"b/d/e":   {Mode: store.ModeFile, ID: id("6")},
		},
		Info: map[string]source.Info{
			"a":           {Mtime: time.Unix(1700000000, 5).UTC(), Size: 3, Ino: 7},
			"a.c":         {Mtime: time.Unix(0, 0).UTC(), Size: 1},
			"b/c":         {Size: 9},
			"local/x/env": {Mtime: time.Unix(1, 2).UTC(), Size: 4, Ino: 1},
		},
		Roots: map[string]rules.Class{
			"b/target":    rules.Cache,
			"b/.secrets":  rules.Hidden,
			"local/x/env": rules.Local,
		},
		Repos:    []source.Repo{{Path: "", Head: "h", Index: "/state/index-0"}},
		BlobRepo: map[string]string{"b/d/e": "vendor/lib"},
	}
}

// TestCaptureIndexServesCapture writes and reopens an index and reads back
// every recorded fact, the directory listings and the paths view lookups find.
func TestCaptureIndexServesCapture(t *testing.T) {
	res := indexFixture()
	dir := t.TempDir()
	if err := writeCaptureIndex(dir, res); err != nil {
		t.Fatal(err)
	}
	if _, err := openCaptureIndex(dir, "other"); err == nil {
		t.Fatal("opened the index of another tree")
	}
	x, err := openCaptureIndex(dir, res.Tree)
	if err != nil {
		t.Fatal(err)
	}
	b := &Base{index: x}
	if x.root != res.Root || !reflect.DeepEqual(x.repos, res.Repos) || !reflect.DeepEqual(b.Roots(), res.Roots) {
		t.Fatalf("meta: root %q repos %+v roots %v", x.root, x.repos, b.Roots())
	}
	for p, f := range res.Files {
		if got, ok := b.Leaf(p); !ok || got != f {
			t.Errorf("leaf %q = %+v %v, want %+v", p, got, ok, f)
		}
	}
	for p, want := range res.Info {
		got, ok := b.Info(p)
		if !ok || !got.Mtime.Equal(want.Mtime) || got.Mtime.IsZero() != want.Mtime.IsZero() || got.Size != want.Size || got.Ino != want.Ino {
			t.Errorf("info %q = %+v %v, want %+v", p, got, ok, want)
		}
	}
	for p, want := range map[string]rules.Class{"b/target": rules.Cache, "local/x/env": rules.Local, "b": "", "local": "", "nope": ""} {
		if got, ok := b.Class(p); got != want || ok != (want != "") {
			t.Errorf("class %q = %q %v, want %q", p, got, ok, want)
		}
	}
	for _, p := range []string{"b", "b/d", "local", "b/target", "nope", "a/x"} {
		if _, ok := b.Leaf(p); ok {
			t.Errorf("%q is a leaf", p)
		}
	}
	if n, _ := x.lookup("b/d/e"); func() string { o, _ := x.origin(n); return o }() != "vendor/lib" {
		t.Error("taken blob lost its repository")
	}
	if n, _ := x.lookup("a"); x.oid(n) != res.Files["a"].ID || x.fileMode(n) != 0o644 {
		t.Errorf("a: oid %s mode %v", x.oid(n), x.fileMode(n))
	}
	for p, want := range map[string]os.FileMode{"b": os.ModeDir | 0o755, "b/c": 0o755, "b-c": os.ModeSymlink | 0o777, "b/empty": os.ModeDir | 0o755} {
		if n, ok := x.lookup(p); !ok || x.fileMode(n) != want {
			t.Errorf("mode of %q = %v, want %v", p, x.fileMode(n), want)
		}
	}

	n, _ := x.lookup("b")
	var names []string
	for first, end := x.children(n); first < end; first++ {
		names = append(names, x.nameOf(first))
	}
	if want := []string{".secrets", "c", "d", "empty", "target", "ü 日本"}; !slices.Equal(names, want) {
		t.Errorf("b lists %q, want %q", names, want)
	}
	var walked []string
	x.walk(func(p string, _ uint32) bool { walked = append(walked, p); return true })
	want := []string{"a", "a.c", "b", "b/.secrets", "b/c", "b/d", "b/d/e", "b/empty", "b/target", "b/ü 日本", "b-c", "local", "local/x", "local/x/env"}
	if !slices.Equal(walked, want) {
		t.Errorf("walk %q, want %q", walked, want)
	}

	for p, want := range map[string]struct {
		root  string
		class rules.Class
	}{
		"":                {},
		"b/d/e":           {},
		"b/new":           {"b/new", ""},
		"b/new/deeper":    {"b/new", ""},
		"a/x":             {"a/x", ""},
		"b/target":        {"b/target", rules.Cache},
		"b/target/out/o":  {"b/target", rules.Cache},
		"b/.secrets/key":  {"b/.secrets", rules.Hidden},
		"local/x/env":     {"local", ""}, // below a directory the capture holds no source in
		"local/x/env/sub": {"local", ""},
	} {
		if _, root, class := b.find(p); root != want.root || class != want.class {
			t.Errorf("find(%q) = %q %q, want %q %q", p, root, class, want.root, want.class)
		}
	}
}

// TestCaptureIndexRefusesDamage opens truncated and altered copies of an
// index: each is refused or still answers lookups without failing.
func TestCaptureIndexRefusesDamage(t *testing.T) {
	res := indexFixture()
	dir := t.TempDir()
	if err := writeCaptureIndex(dir, res); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, captureIndexFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 10, headerSize, len(data) / 2, len(data) - 1} {
		if _, err := parseCaptureIndex(data[:n]); err == nil {
			t.Errorf("parsed an index truncated to %d bytes", n)
		}
	}
	for i := range data {
		damaged := slices.Clone(data)
		damaged[i] ^= 0xa5
		x, err := parseCaptureIndex(damaged)
		if err != nil {
			continue
		}
		b := &Base{index: x}
		for p := range res.Files {
			b.find(p)
			b.find(p + "/x")
		}
		x.walk(func(string, uint32) bool { return true })
		b.Roots()
		trackedBy{x: x, repo: 1}.Tracked("b")
	}
}

// TestCaptureIndexOutlivesClose removes a base while goroutines keep looking
// paths up, listing, walking and reading it and the collector runs: every
// read sees the capture as it was (stale but valid), and the mapping goes only
// once the base is unreachable.
func TestCaptureIndexOutlivesClose(t *testing.T) {
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "target", "out.bin"), "built\n")
	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_close", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	b, _, err := Create(context.Background(), c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}})
	if err != nil {
		t.Fatal(err)
	}
	gone := weak.Make(b.index)
	want := map[string]store.File{}
	b.index.walk(func(p string, n uint32) bool {
		if f, ok := b.index.leaf(n); ok {
			want[p] = f
		}
		return true
	})
	if len(want) == 0 {
		t.Fatal("captured nothing")
	}
	roots := b.Roots()

	var stop atomic.Bool
	var wg sync.WaitGroup
	var failures atomic.Int64
	fail := func(format string, args ...any) {
		if failures.Add(1) <= 5 {
			t.Errorf(format, args...)
		}
	}
	read := func(b *Base) {
		v := b.Bottom(time.Time{})
		for p, f := range want {
			if got, ok := b.Leaf(p); !ok || got != f {
				fail("leaf %q = %+v %v", p, got, ok)
			}
			if _, ok := b.Info(p); !ok {
				fail("info %q missing", p)
			}
			if _, err := v.Lstat(p); err != nil {
				fail("lstat %q: %v", p, err)
			}
		}
		if names, err := v.ReadDir(""); err != nil || len(names) == 0 {
			fail("root lists %d names: %v", len(names), err)
		}
		if f, err := v.Open("src/nested.txt"); err != nil {
			fail("open: %v", err)
		} else {
			data, err := io.ReadAll(io.NewSectionReader(f, 0, math.MaxInt64))
			f.Close()
			if err != nil || string(data) != "nested source\n" {
				fail("read %q: %v", data, err)
			}
		}
		if !b.Domains().Facts(source.Dir(dir), "src/nested.txt", nil).Tracked() {
			fail("tracked file lost")
		}
		if got := b.Roots(); !reflect.DeepEqual(got, roots) {
			fail("roots %v, want %v", got, roots)
		}
		walked := 0
		b.index.walk(func(string, uint32) bool { walked++; return true })
		if walked < len(want) {
			fail("walked %d paths", walked)
		}
	}
	for range 8 {
		wg.Go(func() {
			for !stop.Load() {
				read(b)
			}
		})
	}
	wg.Go(func() {
		for !stop.Load() {
			runtime.GC()
		}
	})
	time.Sleep(50 * time.Millisecond)
	if err := b.Remove(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	read(b)

	b = nil
	for i := 0; gone.Value() != nil; i++ {
		if i == 100 {
			t.Fatal("the capture index stayed reachable")
		}
		runtime.GC()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(c.StateDir); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removed base's directory remains")
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
