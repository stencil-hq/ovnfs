package index

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// edit is one --index-info line: "<mode> <oid> [<stage>]\t<path>"; mode 0
// removes the path.
type edit struct {
	mode    object.Mode
	path    string
	content string // blob content; ignored for removals
	stage   uint8
}

func (r *testRepo) indexInfo(edits []edit) (string, []Entry) {
	var b strings.Builder
	var entries []Entry
	for _, e := range edits {
		id := r.format.Zero()
		if e.mode != 0 {
			id = r.blob(e.content)
		}
		stage := ""
		if e.stage != 0 {
			stage = " " + strconv.Itoa(int(e.stage))
		}
		fmt.Fprintf(&b, "%o %s%s\t%s\x00", e.mode, id, stage, e.path)
		entries = append(entries, Entry{Path: e.path, Mode: e.mode, ID: id, Stage: e.stage})
	}
	return b.String(), entries
}

// applyBoth applies edits to two copies of base: one with git update-index
// --index-info (one process per batch), one with Add/Remove and Write. It
// fails unless the files are byte-identical and git sees them identically.
func applyBoth(t *testing.T, r *testRepo, base string, batches ...[]edit) {
	t.Helper()
	dir := t.TempDir()
	gitIndex, ourIndex := filepath.Join(dir, "git"), filepath.Join(dir, "ours")
	copyFile(t, base, gitIndex)
	copyFile(t, base, ourIndex)
	for _, batch := range batches {
		input, entries := r.indexInfo(batch)
		r.run([]string{"GIT_INDEX_FILE=" + gitIndex}, input, "update-index", "-z", "--index-info")

		idx := mustRead(t, ourIndex, r.format, ReadOptions{})
		for _, e := range entries {
			var err error
			if e.Mode == 0 {
				_, err = idx.Remove(e.Path)
			} else {
				err = idx.Add(e)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := Write(ourIndex, idx, WriteOptions{Worktree: r.dir}); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"ls-files", "--stage", "-z"},
		{"ls-files", "--resolve-undo", "-z"},
		{"diff-index", "--cached", "-z", "HEAD"},
		{"--no-optional-locks", "status", "--porcelain", "-z"},
	} {
		if got, want := r.withIndex(ourIndex, args...), r.withIndex(gitIndex, args...); got != want {
			t.Errorf("git %s:\n got %q\nwant %q", strings.Join(args, " "), got, want)
		}
	}
	if got, want := readFileBytes(t, ourIndex), readFileBytes(t, gitIndex); !bytes.Equal(got, want) {
		t.Errorf("index bytes differ:\n got %x\nwant %x", got, want)
	}
}

func TestEditsMatchUpdateIndex(t *testing.T) {
	cases := map[string][]edit{
		"add in new directory": {{mode: object.ModeFile, path: "n/e/w", content: "new"}},
		"replace":              {{mode: object.ModeFile, path: "a/y/z", content: "changed"}},
		"replace unchanged":    {{mode: object.ModeFile, path: "a/x", content: "content a/x\n"}},
		"remove":               {{path: "b/2"}},
		"remove missing":       {{path: "nothere/x"}},
		"remove directory":     {{path: "c/d"}},
		"file over directory":  {{mode: object.ModeFile, path: "c", content: "file"}},
		"directory over file":  {{mode: object.ModeFile, path: "a.c/inner", content: "x"}},
		"deep over file":       {{mode: object.ModeFile, path: "top/x/y", content: "x"}},
		"modes": {
			{mode: object.ModeExec, path: "m1", content: "1"},
			{mode: object.ModeSymlink, path: "m2", content: "target"},
			{mode: 0o100664, path: "m3", content: "3"},
			{mode: 0o100700, path: "m4", content: "4"},
		},
		"batch": {
			{mode: object.ModeFile, path: "b/3", content: "3"},
			{path: "a/x"},
			{mode: object.ModeFile, path: "a/y", content: "over dir"},
			{mode: object.ModeExec, path: "zz", content: "z"},
		},
	}
	forEachFormat(t, func(t *testing.T, f object.Format) {
		r := populate(t, f)
		base := r.path(".git/index")
		for name, edits := range cases {
			t.Run(name, func(t *testing.T) { applyBoth(t, r, base, edits) })
		}

		t.Run("version", func(t *testing.T) {
			// update-index on a v4 index keeps v4 and its compression.
			v4 := filepath.Join(t.TempDir(), "v4")
			copyFile(t, base, v4)
			r.withIndex(v4, "update-index", "--index-version", "4")
			applyBoth(t, r, v4, cases["batch"])
		})

		t.Run("conflicts", func(t *testing.T) {
			conflicted := filepath.Join(t.TempDir(), "conflicted")
			copyFile(t, base, conflicted)
			setup, _ := r.indexInfo([]edit{
				{path: "top"},
				{mode: object.ModeFile, path: "top", content: "base", stage: 1},
				{mode: object.ModeFile, path: "top", content: "ours", stage: 2},
				{mode: object.ModeExec, path: "top", content: "theirs", stage: 3},
				{path: "b/1"},
				{mode: object.ModeFile, path: "b/1", content: "ours", stage: 2},
				{mode: object.ModeFile, path: "b/1", content: "theirs", stage: 3},
			})
			r.run([]string{"GIT_INDEX_FILE=" + conflicted}, setup, "update-index", "-z", "--index-info")

			applyBoth(t, r, conflicted,
				[]edit{{mode: object.ModeFile, path: "top", content: "resolved"}}, // resolves, saving resolve-undo
				[]edit{{path: "b/1"}},
				[]edit{{mode: object.ModeFile, path: "x", content: "x", stage: 2}},
				[]edit{{mode: object.ModeFile, path: "x/y", content: "y", stage: 2}}, // same-stage D/F
				[]edit{{mode: object.ModeFile, path: "x/y", content: "y0"}},          // other stages untouched
			)
		})
	})
}

func TestAddRejects(t *testing.T) {
	idx := New(object.SHA1)
	id := object.Hash(object.SHA1, object.Blob, nil)
	for _, e := range []Entry{
		{Path: "", Mode: object.ModeFile, ID: id},
		{Path: "/abs", Mode: object.ModeFile, ID: id},
		{Path: "dir/", Mode: object.ModeFile, ID: id},
		{Path: "a//b", Mode: object.ModeFile, ID: id},
		{Path: "a/../b", Mode: object.ModeFile, ID: id},
		{Path: "./b", Mode: object.ModeFile, ID: id},
		{Path: ".GIT/config", Mode: object.ModeFile, ID: id},
		{Path: "d", Mode: object.ModeTree, ID: id},
		{Path: "s", Mode: object.ModeFile, ID: id, Stage: 4},
		{Path: "f", Mode: object.ModeFile, ID: object.Hash(object.SHA256, object.Blob, nil)},
	} {
		if err := idx.Add(e); err == nil {
			t.Errorf("Add(%q, %s, stage %d) succeeded", e.Path, e.Mode, e.Stage)
		}
	}
	if len(idx.Entries) != 0 {
		t.Fatalf("entries added: %v", idx.Entries)
	}
}

func TestEditInsideSparseDirectory(t *testing.T) {
	r := populate(t, object.SHA1)
	r.git("sparse-checkout", "set", "--cone", "a")
	r.git("config", "index.sparse", "true")
	r.git("sparse-checkout", "reapply")
	idx := mustRead(t, r.path(".git/index"), object.SHA1, ReadOptions{})
	id := object.Hash(object.SHA1, object.Blob, nil)
	if err := idx.Add(Entry{Path: "c/d/new", Mode: object.ModeFile, ID: id}); !errors.Is(err, ErrSparseDirectory) {
		t.Fatalf("Add inside sparse dir: %v", err)
	}
	if _, err := idx.Remove("b/1"); !errors.Is(err, ErrSparseDirectory) {
		t.Fatalf("Remove inside sparse dir: %v", err)
	}
	if err := idx.ExpandSparse(r.readTree); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add(Entry{Path: "c/d/new", Mode: object.ModeFile, ID: id}); err != nil {
		t.Fatal(err)
	}
}

// TestReplaceMatchesSequentialEdits checks Replace against the Remove and Add
// sequence it stands for, including the edits that displace entries
// (conflict stages, file/directory swaps, duplicates) and sparse directories.
func TestReplaceMatchesSequentialEdits(t *testing.T) {
	names := []string{"a", "b", "a b", "a.b"}
	ids := []object.ID{object.Hash(object.SHA1, object.Blob, []byte("1")), object.Hash(object.SHA1, object.Blob, []byte("2"))}
	r := rand.New(rand.NewPCG(3, 4))
	randPath := func() string {
		parts := make([]string, 1+r.IntN(3))
		for i := range parts {
			parts[i] = names[r.IntN(len(names))]
		}
		return strings.Join(parts, "/")
	}
	randEntry := func() Entry {
		e := Entry{Path: randPath(), Mode: object.ModeFile, ID: ids[r.IntN(2)]}
		if r.IntN(8) == 0 {
			e.Stage = uint8(1 + r.IntN(3))
		}
		if r.IntN(10) == 0 {
			e.Mode = 0o100700 // canonicalized to 100755
		}
		return e
	}
	var cacheTree func(depth int) *CacheTree
	cacheTree = func(depth int) *CacheTree {
		t := &CacheTree{Entries: 1, ID: ids[0]}
		if depth < 3 {
			for _, n := range names {
				c := cacheTree(depth + 1)
				c.Name = n
				t.Children = append(t.Children, c)
			}
			slices.SortFunc(t.Children, func(a, b *CacheTree) int { return compareChildName(a.Name, b.Name) })
		}
		return t
	}
	for round := range 3000 {
		base := New(object.SHA1)
		for range r.IntN(40) {
			e := randEntry()
			if r.IntN(20) == 0 {
				e = Entry{Path: randPath() + "/", Mode: object.ModeTree, ID: ids[0], SkipWorktree: true}
			}
			if _, ok := base.Find(e.Path, e.Stage); !ok {
				i, _ := base.Find(e.Path, e.Stage)
				base.Entries = slices.Insert(base.Entries, i, e)
			}
		}
		if r.IntN(2) == 0 {
			base.CacheTree = cacheTree(0)
		}
		var remove []string
		for range r.IntN(10) {
			remove = append(remove, randPath())
		}
		var add []Entry
		for range r.IntN(10) {
			add = append(add, randEntry())
		}
		if r.IntN(2) == 0 {
			// The FetchBranch shape: re-add paths just removed.
			for _, p := range remove {
				if r.IntN(2) == 0 {
					add = append(add, Entry{Path: p, Mode: object.ModeFile, ID: ids[1]})
				}
			}
		}

		want, got := clone(base), clone(base)
		var wantErr error
		for _, p := range remove {
			if _, wantErr = want.Remove(p); wantErr != nil {
				break
			}
		}
		for _, e := range add {
			if wantErr != nil {
				break
			}
			wantErr = want.Add(e)
		}
		gotErr := got.Replace(remove, add)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("round %d: Replace error %v, sequential error %v", round, gotErr, wantErr)
		}
		if wantErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: remove %q add %v:\n got %+v\nwant %+v", round, remove, add, got.Entries, want.Entries)
		}
	}
}

func clone(idx *Index) *Index {
	c := *idx
	c.Entries = slices.Clone(idx.Entries)
	c.ResolveUndo = slices.Clone(idx.ResolveUndo)
	var cloneTree func(*CacheTree) *CacheTree
	cloneTree = func(t *CacheTree) *CacheTree {
		if t == nil {
			return nil
		}
		n := *t
		n.Children = make([]*CacheTree, len(t.Children))
		for i, k := range t.Children {
			n.Children[i] = cloneTree(k)
		}
		return &n
	}
	c.CacheTree = cloneTree(idx.CacheTree)
	return &c
}
