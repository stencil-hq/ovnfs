package checkout

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// flatTree is the snapshot hash before it became incremental: the parent tree
// flattened, every layer folded into the flat leaf map, the whole tree built.
// It is kept verbatim as the reference for snapTree, with one fix: created
// directories holding only empty directories were empty-directory leaves too,
// which made Build fail ("both a leaf and a directory").
type flatTree struct {
	base   *baseRecord
	store  *store.Store
	view   source.View
	files  map[string]store.File
	sorted []string
	added  map[string]bool
	dirs   map[string]bool
	class  map[string]rules.Class
}

// flatHash is the reference snapshot hash.
func flatHash(j hashJob) (store.OID, error) {
	st := j.Base.view.Store()
	files, err := st.Flatten(j.Parent)
	if err != nil {
		return "", err
	}
	t := &flatTree{base: j.Base, store: st, view: overlayView{j.View}, files: files, sorted: source.SortedPaths(files), added: map[string]bool{}, dirs: map[string]bool{}, class: map[string]rules.Class{"": rules.Source}}
	for _, l := range j.Layers {
		if err := t.apply(l); err != nil {
			return "", err
		}
	}
	return st.Build(t.finish())
}

func (t *flatTree) apply(l *overlay.Layer) error {
	return l.Walk(func(p string, e overlay.Entry) error {
		if p == "" || hashExcluded(p) {
			return nil
		}
		if pc := parentClass(t.class, p); pc != rules.Source {
			if e.Kind == overlay.KindDir {
				t.class[p] = pc
			}
			return nil
		}
		switch e.Kind {
		case overlay.KindWhiteout:
			t.remove(p)
		case overlay.KindDir:
			if e.Opaque {
				t.removeUnder(p)
			}
			class := t.classify(p, e.Info)
			t.class[p] = class
			if f, ok := t.files[p]; ok && f.Mode != store.ModeDir {
				delete(t.files, p)
			}
			if class != rules.Source {
				t.remove(p)
				return nil
			}
			t.dirs[p] = true
		case overlay.KindFile, overlay.KindSymlink:
			if t.classify(p, e.Info) != rules.Source {
				return nil
			}
			t.removeUnder(p)
			delete(t.dirs, p)
			var f store.File
			err := l.Content(p, func(host, _ string) error {
				var err error
				f, err = t.hash(host, e)
				return err
			})
			if err != nil {
				return err
			}
			t.files[p], t.added[p] = f, true
		}
		return nil
	})
}

func (t *flatTree) classify(p string, info os.FileInfo) rules.Class {
	if _, ok := t.files[p]; ok {
		return rules.Source
	}
	if class, ok := t.base.view.Capture().Roots[p]; ok {
		return class
	}
	if rule, ok := t.base.rules.Match(p, t.base.view.Domains().Facts(t.view, p, info)); ok {
		return rule.Class
	}
	return rules.Source
}

func (t *flatTree) hash(host string, e overlay.Entry) (store.File, error) {
	mode, ok := store.ModeOf(e.Info.Mode())
	if !ok {
		return store.File{}, &os.PathError{Op: "hash", Path: host, Err: os.ErrInvalid}
	}
	if mode == store.ModeSymlink {
		target, err := os.Readlink(host)
		if err != nil {
			return store.File{}, err
		}
		id, err := t.store.PutBytes([]byte(filepath.ToSlash(target)))
		return store.File{Mode: mode, ID: id}, err
	}
	id, _, err := t.store.PutFile(host)
	return store.File{Mode: mode, ID: id}, err
}

func (t *flatTree) remove(p string) {
	delete(t.files, p)
	delete(t.added, p)
	delete(t.dirs, p)
	t.removeUnder(p)
}

func (t *flatTree) removeUnder(p string) {
	prefix := p + "/"
	for i := sort.SearchStrings(t.sorted, prefix); i < len(t.sorted) && strings.HasPrefix(t.sorted[i], prefix); i++ {
		delete(t.files, t.sorted[i])
	}
	for q := range t.added {
		if strings.HasPrefix(q, prefix) {
			delete(t.files, q)
			delete(t.added, q)
		}
	}
	for d := range t.dirs {
		if strings.HasPrefix(d, prefix) {
			delete(t.dirs, d)
		}
	}
}

func (t *flatTree) finish() map[string]store.File {
	parents := make(map[string]bool)
	for p := range t.files { // the fix: empty-directory leaves imply their parents too
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if parents[d] {
				break
			}
			parents[d] = true
		}
	}
	for d := range t.dirs {
		if !parents[d] {
			t.files[d] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
			for q := path.Dir(d); q != "." && !parents[q]; q = path.Dir(q) { // the fix
				parents[q] = true
			}
		}
	}
	for p, f := range t.files {
		if f.Mode == store.ModeDir && parents[p] {
			delete(t.files, p)
		}
	}
	return t.files
}

// layerNames is the name pool random layers draw from: rule-classified names
// (build is a cache directory, *.tmp local), excluded names, and plain ones.
var layerNames = []string{"a", "b", "c", "keep", "build", "node_modules", "x.tmp", ".git", "._d"}

// randomLayer fills dir with a random frozen upper: files with varied content
// and modes, symlinks, directories (some opaque, some empty) and whiteouts.
func randomLayer(t *testing.T, rng *rand.Rand, dir string) {
	t.Helper()
	contents := []string{"one", "two", "three", ""}
	var fill func(rel string, depth int)
	fill = func(rel string, depth int) {
		for range rng.IntN(6) {
			name := layerNames[rng.IntN(len(layerNames))]
			host := filepath.Join(dir, rel, name)
			if _, err := os.Lstat(host); err == nil {
				continue
			}
			switch rng.IntN(7) {
			case 0, 1:
				perm := os.FileMode(0o644)
				if rng.IntN(4) == 0 {
					perm = 0o755
				}
				if err := os.WriteFile(host, []byte(contents[rng.IntN(len(contents))]), perm); err != nil {
					t.Fatal(err)
				}
			case 2:
				if err := os.Symlink([]string{"a", "../b", "missing"}[rng.IntN(3)], host); err != nil {
					t.Fatal(err)
				}
			case 3:
				if err := os.WriteFile(filepath.Join(dir, rel, ".wh."+name), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(host, 0o755); err != nil {
					t.Fatal(err)
				}
				if rng.IntN(5) == 0 {
					if err := os.WriteFile(filepath.Join(host, ".wh..wh..opq"), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if depth < 3 {
					fill(path.Join(rel, name), depth+1)
				}
			}
		}
	}
	fill("", 0)
}

// specLayer fills dir from path → spec: "dir", "opaque" (an opaque
// directory), "wh" (a whiteout), "link:<target>", "exec:<content>" or
// "file:<content>"; parents are created as plain directories.
func specLayer(t *testing.T, dir string, spec map[string]string) {
	t.Helper()
	for _, p := range slices.Sorted(maps.Keys(spec)) {
		host := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			t.Fatal(err)
		}
		kind, arg, _ := strings.Cut(spec[p], ":")
		var err error
		switch kind {
		case "dir":
			err = os.MkdirAll(host, 0o755)
		case "opaque":
			if err = os.MkdirAll(host, 0o755); err == nil {
				err = os.WriteFile(filepath.Join(host, ".wh..wh..opq"), nil, 0o644)
			}
		case "wh":
			err = os.WriteFile(filepath.Join(filepath.Dir(host), ".wh."+path.Base(p)), nil, 0o644)
		case "link":
			err = os.Symlink(arg, host)
		case "exec":
			err = os.WriteFile(host, []byte(arg), 0o755)
		default:
			err = os.WriteFile(host, []byte(arg), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// hashBench is a checkout's base for comparing snapshot hashes on layer
// stacks built outside the registry.
type hashBench struct {
	t      *testing.T
	b      *baseRecord
	st     *store.Store
	parent store.OID
	stack  []*overlay.Layer // every layer so far, top first, for rule facts
}

// newHashBench opens a checkout on a folder with nested, empty, cache and
// local paths, under rules classifying build directories as cache and *.tmp as local.
func newHashBench(t *testing.T) *hashBench {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a/x": "1", "a/b/y": "2", "b": "3", "c/d/e": "4", "build/out": "5", "node_modules/m/i.js": "6", "x.tmp": "7"})
	for _, d := range []string{"keep", "a/empty"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.newCheckout(dir, "c", "cache dir & build", "local *.tmp")
	h, err := f.reg.lookupCheckout("c")
	if err != nil {
		t.Fatal(err)
	}
	return &hashBench{t: t, b: h.base, st: h.base.view.Store(), parent: h.base.view.Meta().Tree}
}

// reset starts a new chain of snapshots from the base.
func (hb *hashBench) reset() { hb.parent, hb.stack = hb.b.view.Meta().Tree, nil }

// snap hashes the layers filled by fill (oldest first) over the current parent
// with both algorithms, then again once the layers are stored, and makes the
// result the next parent. It reports false when both algorithms failed.
func (hb *hashBench) snap(name string, fills ...func(dir string)) bool {
	t := hb.t
	t.Helper()
	layers := make([]*overlay.Layer, len(fills))
	for i, fill := range fills {
		d := t.TempDir()
		fill(d)
		l, err := overlay.IndexLayer(d)
		if err != nil {
			t.Fatal(err)
		}
		layers[i] = l
		hb.stack = append([]*overlay.Layer{l}, hb.stack...)
	}
	view, err := overlay.New("", hb.stack, hb.b.view.Bottom(time.Time{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	j := hashJob{Base: hb.b, Parent: hb.parent, Layers: layers, View: view}
	want, wantErr := flatHash(j)
	got, kept, gotErr := hashSnapshot(j)
	if (wantErr != nil) != (gotErr != nil) || got != want {
		t.Fatalf("%s: incremental %s %v, reference %s %v", name, got, gotErr, want, wantErr)
	}
	if wantErr != nil {
		return false
	}
	for _, l := range layers {
		for p, id := range kept[l] {
			if !hb.st.Has(store.OID(id)) {
				t.Fatalf("%s: kept %s for %q but the store lacks it", name, id, p)
			}
		}
		if err := storeLayer(hb.st, l, kept[l]); err != nil {
			t.Fatal(err)
		}
	}
	again, kept, err := hashSnapshot(j)
	if err != nil || again != want {
		t.Fatalf("%s: over stored layers %s %v, want %s", name, again, err, want)
	}
	if len(kept) != 0 {
		t.Fatalf("%s: stored layers kept blobs again: %v", name, kept)
	}
	hb.parent = want
	return true
}

// TestIncrementalHashMatchesFlatten checks on randomized layer stacks that the
// incremental snapshot hash writes exactly the tree the flatten-and-build
// reference does, also once the layers serve stored content, and that it
// reports every raw source file it stored.
func TestIncrementalHashMatchesFlatten(t *testing.T) {
	hb := newHashBench(t)
	rng := rand.New(rand.NewPCG(1, 2))
	for chain := range 25 {
		hb.reset()
		for round := range 8 {
			fills := make([]func(string), 1+rng.IntN(3))
			for i := range fills {
				fills[i] = func(d string) { randomLayer(t, rng, d) }
			}
			if !hb.snap(fmt.Sprintf("chain %d round %d", chain, round), fills...) {
				break
			}
		}
	}
}

// TestIncrementalHashScenarios checks the incremental snapshot hash against
// the reference on stacks built for each rule of the fold.
func TestIncrementalHashScenarios(t *testing.T) {
	spec := func(s map[string]string) func(string) { return func(d string) { specLayer(t, d, s) } }
	for name, rounds := range map[string][][]map[string]string{
		"held paths stay source": {
			{{"a/build": "file:x"}},
			{{"a/build": "dir"}},
			{{"a/build/f": "file:y"}, {"a/build": "dir", "a/build/g": "file:z"}},
		},
		"non-source directories are one unit": {
			{{"a/build/f": "file:x", "a/build/g/h": "file:y", "c/x.tmp/i": "file:z"}},
			{{"a/build": "dir", "a/build/j": "file:w"}},
		},
		"nested empty directories": {
			{{"n/m/o": "dir", "p": "dir"}},
			{{"n/m/o/q": "dir"}},
			{{"n/m/o/q": "wh"}},
		},
		"opaque directories hide the parent": {
			{{"c/d/e2": "file:x"}},
			{{"c": "opaque", "c/z": "file:y"}},
			{{"c": "opaque"}},
		},
		"type changes": {
			{{"b": "dir", "b/q": "file:x", "a/x": "link:../b"}},
			{{"b": "wh", "a/x": "exec:now a file"}},
			{{"c": "file:was a directory", "a/b": "link:x"}},
		},
		"empty-directory leaves": {
			{{"keep": "wh"}},
			{{"keep": "dir", "keep/f": "file:x"}, {"keep/f": "wh"}},
			{{"a/empty/f": "file:x"}, {"a/empty/f": "wh"}},
			{{"a/empty": "opaque"}, {"a/empty/g": "file:y"}},
		},
		"excluded paths": {
			{{".git/HEAD": "file:x", "._d": "file:y", "a/._e": "file:z", "a/.git": "file:w"}},
		},
		"whiteouts of missing paths": {
			{{"nothing": "wh", "a/nothing/deeper": "wh", "x.tmp": "wh"}},
		},
		"removing every leaf": {
			{{"a": "wh", "b": "wh", "c": "wh", "keep": "wh", "x.tmp": "wh"}},
			{{"z/y": "dir"}},
		},
	} {
		hb := newHashBench(t)
		for i, layers := range rounds {
			fills := make([]func(string), len(layers))
			for k, s := range layers {
				fills[k] = spec(s)
			}
			if !hb.snap(fmt.Sprintf("%s: round %d", name, i), fills...) {
				t.Fatalf("%s: round %d failed in both algorithms", name, i)
			}
		}
	}
}

// TestSnapshotKeepsNestedEmptyDirectories checks a snapshot of directories
// created empty inside each other, which the flatten-and-build hash failed.
func TestSnapshotKeepsNestedEmptyDirectories(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"x": "1"}), "c")
	var s Session
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.MkdirAll("a/b/c", 0o755); err != nil {
		t.Fatal(err)
	}
	s.Done()
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.reg.store.Flatten(store.OID(reply.Snapshot.Tree))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]store.File{"x": files["x"], "a/b/c": {Mode: store.ModeDir, ID: store.EmptyTree}}
	if len(files) != len(want) || files["a/b/c"] != want["a/b/c"] {
		t.Fatalf("snapshot leaves %v, want x and the empty a/b/c", files)
	}
}

// TestSnapshotStoresEditAsDelta bounds history growth across edits and renames,
// including replaced destinations, directory moves whose source sorts first,
// and saves that write a temporary file and rename it over the target.
func TestSnapshotStoresEditAsDelta(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	for b.Len() < 300<<10 {
		fmt.Fprintf(&b, "line %d %x\n", b.Len(), rng.Uint64())
	}
	original := b.String()
	edited := strings.Replace(original, "line 0 ", "line 0 edited ", 1)
	for _, scenario := range []struct {
		name  string
		moves [][2]string
		temp  string // the edit is saved here, then renamed to path
		path  string
	}{
		{name: "edit", path: "a/big.txt"},
		{name: "atomic save", temp: "a/.big.txt.tmp", path: "a/big.txt"},
		{name: "rename", moves: [][2]string{{"a/big.txt", "z/big.txt"}}, path: "z/big.txt"},
		{name: "rename chain", moves: [][2]string{{"a/big.txt", "mid.txt"}, {"mid.txt", "z/big.txt"}}, path: "z/big.txt"},
		{name: "directory", moves: [][2]string{{"a", "renamed"}}, path: "renamed/big.txt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newFixture(t)
			f.newCheckout(folder(t, map[string]string{"a/big.txt": original, "z/big.txt": "replaced"}), "c")
			before := storeBytes(t, f.state)
			rename := func(from, to string) {
				t.Helper()
				var s Session
				if err := f.reg.Enter("c", &s); err != nil {
					t.Fatal(err)
				}
				defer s.Done()
				if err := s.FS.Rename(from, to); err != nil {
					t.Fatal(err)
				}
			}
			for _, move := range scenario.moves {
				rename(move[0], move[1])
			}
			if scenario.temp == "" {
				f.put("c", scenario.path, edited)
			} else {
				f.put("c", scenario.temp, edited)
				rename(scenario.temp, scenario.path)
			}
			reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true})
			if err != nil {
				t.Fatal(err)
			}
			if grew := storeBytes(t, f.state) - before; grew > 8<<10 {
				t.Fatalf("an edit to a %d-byte file grew the store by %d bytes", len(original), grew)
			}
			if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: reply.Snapshot.ID, Name: "view"}); err != nil {
				t.Fatal(err)
			}
			if got := f.read("view", scenario.path); got != edited {
				t.Fatalf("snapshot content differs: got %d bytes, want %d", len(got), len(edited))
			}
			if len(scenario.moves) != 0 && f.read("view", "a/big.txt") != "<absent>" {
				t.Fatal("snapshot resurrected the rename source")
			}
			if scenario.temp != "" && f.read("view", scenario.temp) != "<absent>" {
				t.Fatal("snapshot resurrected the temporary file")
			}
		})
	}
}

// storeBytes accounts for stored history without depending on the store's layout.
func storeBytes(t *testing.T, state string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(filepath.Join(state, "store"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			n += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// snapLayer snapshots checkout name and returns the layer it froze, once
// hashing has moved its stored content out of the layer directory.
func (f *fixture) snapLayer(name string) (api.Snapshot, *overlay.Layer) {
	f.t.Helper()
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: name, Wait: true})
	if err != nil {
		f.t.Fatal(err)
	}
	f.reg.mu.RLock()
	rec := f.reg.layers[f.reg.snapshots[reply.Snapshot.ID].layer]
	f.reg.mu.RUnlock()
	if rec == nil {
		f.t.Fatalf("snapshot %s froze no layer", reply.Snapshot.ID)
	}
	// Stored turns true before Store removes the raw files; the keeper runs
	// until Store returns, so wait for it too before tests read the directory.
	eventually(f.t, "layer "+rec.id+" stored", func() bool {
		h := f.reg.hasher
		h.mu.Lock()
		defer h.mu.Unlock()
		return rec.layer.Stored() && !h.keeping
	})
	return reply.Snapshot, rec.layer
}

// hostFiles lists the regular files and symlinks below dir, slash-separated.
func hostFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// TestStoredLayerKeepsOnlyRawLeftovers checks stored and non-source content,
// metadata, and collection roots across restart.
func TestStoredLayerKeepsOnlyRawLeftovers(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"src/main.go": "package main", "gone": "x", "dir/old": "o"}), "c", "local *.log")
	f.put("c", "src/main.go", "package main // edited")
	f.put("c", "src/new.txt", "new")
	f.put("c", "debug.log", "local only")
	f.remove("c", "gone")
	f.remove("c", "dir/old")
	var s Session
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.Symlink("src/new.txt", "link"); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.MkdirAll("empty/inner", 0o755); err != nil {
		t.Fatal(err)
	}
	s.Done()
	snap, layer := f.snapLayer("c")
	check := func() {
		t.Helper()
		for p, want := range map[string]string{"src/main.go": "package main // edited", "src/new.txt": "new", "debug.log": "local only", "gone": "<absent>", "dir/old": "<absent>", "link": "new"} {
			if got := f.read("c", p); got != want {
				t.Fatalf("%s reads %q, want %q", p, got, want)
			}
		}
		var s Session
		if err := f.reg.Enter("c", &s); err != nil {
			t.Fatal(err)
		}
		defer s.Done()
		if info, err := s.FS.Lstat("empty/inner"); err != nil || !info.IsDir() {
			t.Fatalf("empty/inner: %v %v", info, err)
		}
	}
	check()
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	check()
	// The layer's own tree is a collection root and keeps its blobs without the snapshot's.
	h, err := f.reg.lookupCheckout("c")
	if err != nil {
		t.Fatal(err)
	}
	f.reg.mu.Lock()
	layer = f.reg.layers[f.reg.snapshots[snap.ID].layer].layer
	roots := f.reg.treeRootsLocked()
	f.reg.mu.Unlock()
	if layer.Tree() == "" || !slices.Contains(roots, store.OID(layer.Tree())) {
		t.Fatalf("layer tree %q is not among the roots %v", layer.Tree(), roots)
	}
	if _, err := f.reg.store.Sweep([]store.OID{h.base.view.Meta().Tree, store.OID(layer.Tree())}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	check()
}

// TestRestartStoresHashedRawLayers checks that a layer left raw after its
// snapshot was hashed (a stop or crash before storing it) is stored once the
// registry opens again.
func TestRestartStoresHashedRawLayers(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "c", "local *.log")
	f.put("c", "a", "edited")
	f.put("c", "b.log", "local")
	f.reg.hasher.keep = nil // the stop comes before storing
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	f.reg.mu.RLock()
	id := f.reg.snapshots[reply.Snapshot.ID].layer
	f.reg.mu.RUnlock()
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	if got := hostFiles(t, filepath.Join(f.state, "layers", id)); !slices.Equal(got, []string{"a", "b.log"}) {
		t.Fatalf("raw layer holds %v", got)
	}
	f.open()
	f.reg.mu.RLock()
	layer := f.reg.layers[id].layer
	f.reg.mu.RUnlock()
	eventually(t, "layer stored after restart", layer.Stored)
	if got := f.read("c", "a"); got != "edited" {
		t.Fatalf("a reads %q", got)
	}
}

// TestRestartIndexesRenamedDirectoryWithDeletion reopens a raw layer whose
// renamed directory carried the deletion of a source file: the registry opens
// and the snapshot reads back with the deletion and the move intact.
func TestRestartIndexesRenamedDirectoryWithDeletion(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"d/x": "x", "d/y": "y"}), "c")
	var s Session
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.Remove("d/x"); err != nil {
		s.Done()
		t.Fatal(err)
	}
	if err := s.FS.Rename("d", "e"); err != nil {
		s.Done()
		t.Fatal(err)
	}
	s.Done()
	f.reg.hasher.keep = nil // the stop comes before storing
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if _, err := f.reg.New(context.Background(), api.NewArgs{Snapshot: reply.Snapshot.ID, Name: "view"}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{"e/y": "y", "e/x": "<absent>", "d/y": "<absent>", "d/x": "<absent>"} {
		if got := f.read("view", p); got != want {
			t.Fatalf("%s reads %q, want %q", p, got, want)
		}
	}
}

// TestCloseStoresPendingLayers checks that stopping the registry right after
// a snapshot stores its layer without waiting for hashing to pause.
func TestCloseStoresPendingLayers(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "c")
	f.put("c", "a", "edited")
	reply, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "c", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	f.reg.mu.RLock()
	layer := f.reg.layers[f.reg.snapshots[reply.Snapshot.ID].layer].layer
	f.reg.mu.RUnlock()
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	if !layer.Stored() {
		t.Fatal("close left snapshot content unstored")
	}
	f.open()
	if got := f.read("c", "a"); got != "edited" {
		t.Fatalf("a reads %q", got)
	}
}

// TestStoredLayerCopyUpAndDiff checks copy-up content and metadata, and diffs
// against stored layers.
func TestStoredLayerCopyUpAndDiff(t *testing.T) {
	f := newFixture(t)
	f.newCheckout(folder(t, map[string]string{"a": "base"}), "c")
	f.put("c", "a", "layer")
	f.put("c", "run.sh", "#!/bin/sh")
	var s Session
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.Chmod("run.sh", 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := s.FS.Lstat("run.sh")
	s.Done()
	if err != nil {
		t.Fatal(err)
	}
	f.snapLayer("c")
	diff, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "c", Patch: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Patch, "+layer") || !strings.Contains(diff.Patch, "+#!/bin/sh") {
		t.Fatalf("patch over stored layer:\n%s", diff.Patch)
	}
	if err := f.reg.Enter("c", &s); err != nil {
		t.Fatal(err)
	}
	defer s.Done()
	file, err := s.FS.OpenFile("run.sh", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("\necho hi\n")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := s.FS.Lstat("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() || after.Size() != before.Size()+int64(len("\necho hi\n")) {
		t.Fatalf("copied up %v %d, was %v %d", after.Mode(), after.Size(), before.Mode(), before.Size())
	}
	if got := f.read("c", "run.sh"); got != "#!/bin/sh\necho hi\n" {
		t.Fatalf("run.sh reads %q", got)
	}
}
