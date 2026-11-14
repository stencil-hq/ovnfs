package object

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// checkAllObjects compares Has, Header and Read on every object git lists.
func checkAllObjects(t *testing.T, db *DB, objs map[ID]gitObject) {
	t.Helper()
	if len(objs) == 0 {
		t.Fatal("no objects to check")
	}
	for id, want := range objs {
		if !db.Has(id) {
			t.Errorf("Has(%s) = false", id)
		}
		kind, size, err := db.Header(id)
		if err != nil || kind != want.kind || size != int64(len(want.data)) {
			t.Errorf("Header(%s) = %v %d %v, want %v %d", id, kind, size, err, want.kind, len(want.data))
		}
		kind, data, err := db.Read(id)
		if err != nil || kind != want.kind || !bytes.Equal(data, want.data) {
			t.Errorf("Read(%s) = %v %d bytes %v, want %v %d bytes", id, kind, len(data), err, want.kind, len(want.data))
		}
	}
}

// maxChain returns the longest delta chain git verify-pack reports for the
// repository's packs.
func maxChain(r *testRepo) int {
	idxs, _ := filepath.Glob(filepath.Join(r.objects(), "pack", "*.idx"))
	best := 0
	re := regexp.MustCompile(`(?m)^chain length = (\d+):`)
	for _, idx := range idxs {
		for _, m := range re.FindAllStringSubmatch(r.gitString("verify-pack", "-v", idx), -1) {
			n, _ := strconv.Atoi(m[1])
			best = max(best, n)
		}
	}
	return best
}

func TestReadMaintenanceLoosePack(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			src := newRepo(t, "--object-format="+format)
			src.fastImportLoose(history(src.format, 5, "maintenance"))
			want := src.allObjects()
			id := src.id(src.gitString("rev-parse", "main"))

			dst := newRepo(t, "--bare", "--object-format="+format)
			db := dst.open()
			prefix := filepath.Join(dst.objects(), "pack", "loose")
			hash := strings.TrimSpace(string(src.git(nil, "pack-objects", "--all", "-q", prefix)))
			if _, err := os.Stat(prefix + "-" + hash + ".idx"); err != nil {
				t.Fatalf("loose pack index: %v", err)
			}
			if _, _, err := db.Read(id); err != nil {
				t.Fatalf("Read after loose pack creation: %v", err)
			}
			checkAllObjects(t, db, want)
			checkAllObjects(t, dst.open(), want)
		})
	}
}

func TestReadAggressivePack(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			r := newRepo(t, "--object-format="+format)
			r.fastImport(history(r.format, 60, "agg"))
			r.git(nil, "tag", "-a", "-m", "nested", "nested", "v10")
			r.git(nil, "gc", "--aggressive", "--prune=now", "--quiet")
			if n := maxChain(r); n < 10 {
				t.Fatalf("longest delta chain %d; want a deep one", n)
			}
			checkAllObjects(t, r.open(), r.allObjects())
		})
	}
}

func TestReadLooseMultiplePacksAndAlternates(t *testing.T) {
	alt := newRepo(t)
	alt.fastImport(history(alt.format, 15, "alt"))
	alt.git(nil, "repack", "-a", "-d", "-q")

	r := newRepo(t)
	rel, err := filepath.Rel(r.objects(), alt.objects())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.objects(), "info", "alternates"), []byte("# borrowed\n"+rel+"\n"), 0o644)
	for i, seed := range []string{"one", "two", "three"} {
		stream := strings.ReplaceAll(history(r.format, 8, seed), "refs/heads/main", "refs/heads/"+seed)
		if i < 2 {
			r.fastImport(stream) // one pack each
		} else {
			r.fastImportLoose(stream)
		}
	}
	counts := r.gitString("count-objects", "-v")
	if !strings.Contains(counts, "packs: 2\n") || strings.HasPrefix(counts, "count: 0\n") {
		t.Fatalf("want loose objects and two packs:\n%s", counts)
	}
	want := r.allObjects()
	for id := range alt.allObjects() {
		if _, ok := want[id]; !ok {
			t.Fatalf("git does not list alternate object %s", id)
		}
	}
	// A cycle back to the repository must be tolerated.
	os.WriteFile(filepath.Join(alt.objects(), "info", "alternates"), []byte(r.objects()+"\n/does/not/exist\n"), 0o644)
	db := r.open()
	if got, want := db.Dirs(), []string{r.objects(), alt.objects()}; !slices.Equal(got, want) {
		t.Fatalf("Dirs() = %q, want %q", got, want)
	}
	checkAllObjects(t, db, want)
}

func TestReadIndexVersionsAndRefDeltas(t *testing.T) {
	src := newRepo(t)
	src.fastImport(history(src.format, 30, "idx"))
	// Without --delta-base-offset pack-objects writes ref-deltas.
	pack := src.git([]byte{}, "pack-objects", "--all", "--stdout", "-q")
	want := src.allObjects()
	for _, version := range []string{"1", "2", "2,64"} { // 2,64: 64-bit offsets above 64
		t.Run(version, func(t *testing.T) {
			r := newRepo(t, "--bare")
			r.git(pack, "index-pack", "--stdin", "--index-version="+version)
			db := r.open()
			checkAllObjects(t, db, want)
			p := (*db.dirs[0].packs.Load())[0]
			if refs := countEntries(t, p, typeRefDelta); refs == 0 {
				t.Error("pack has no ref-deltas")
			}
			if v1 := p.idx.offsets == 0; v1 != (version == "1") {
				t.Errorf("index read as version 1: %v", v1)
			}
			if version == "2,64" && p.idx.nLarge == 0 {
				t.Error("index has no 64-bit offsets")
			}
		})
	}
}

// countEntries counts a pack's entries of pack type typ.
func countEntries(t *testing.T, p *pack, typ byte) int {
	n := 0
	for i := range p.idx.count {
		off, err := p.idx.offset(i)
		if err != nil {
			t.Fatal(err)
		}
		h, err := p.entryAt(off)
		if err != nil {
			t.Fatal(err)
		}
		if h.typ == typ {
			n++
		}
	}
	return n
}

func TestReadWithoutMmap(t *testing.T) {
	mapped.Enabled = false
	t.Cleanup(func() { mapped.Enabled = true })
	r := newRepo(t)
	r.fastImport(history(r.format, 30, "nommap"))
	r.git(nil, "gc", "--aggressive", "--quiet")
	r.fastImportLoose(strings.ReplaceAll(history(r.format, 3, "loose"), "refs/heads/main", "refs/heads/loose"))
	db := r.open()
	if p := (*db.dirs[0].packs.Load())[0]; p.data.Mapped() != nil || p.idx.m.Mapped() != nil {
		t.Fatal("pack is mapped")
	}
	objs := r.allObjects()
	checkAllObjects(t, db, objs)
	for id := range objs {
		if got, err := db.FindPrefix(id.String()[:7], 0); err != nil || !slices.Contains(got, id) {
			t.Fatalf("FindPrefix(%s) = %v %v", id.String()[:7], got, err)
		}
	}
}

func TestMissingAndPeel(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 10, "peel"))
	r.git(nil, "tag", "-a", "-m", "nested", "nested", "v10")
	db := r.open()

	missing := r.id(strings.Repeat("ab", 20))
	if db.Has(missing) {
		t.Error("Has(missing) = true")
	}
	if _, _, err := db.Header(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Header(missing) error %v, want ErrNotExist", err)
	}
	if _, _, err := db.Read(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read(missing) error %v, want ErrNotExist", err)
	}

	nested := r.id(r.gitString("rev-parse", "nested"))
	for _, tc := range []struct {
		want Kind
		rev  string
	}{
		{Tag, "nested"},
		{Commit, "nested^{commit}"},
		{Tree, "nested^{tree}"},
	} {
		got, err := db.Peel(nested, tc.want)
		if want := r.id(r.gitString("rev-parse", tc.rev)); err != nil || got != want {
			t.Errorf("Peel(nested, %v) = %s %v, want %s", tc.want, got, err, want)
		}
	}
	if _, err := db.Peel(nested, Blob); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("Peel(tag, Blob) error %v, want a non-ErrNotExist error", err)
	}
	if _, err := db.Peel(missing, Tree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Peel(missing) error %v, want ErrNotExist", err)
	}
}

func TestFindPrefixMatchesDisambiguate(t *testing.T) {
	r := newRepo(t)
	var s strings.Builder
	for i := range 3000 {
		b := fmt.Sprintf("blob %d", i)
		fmt.Fprintf(&s, "blob\ndata %d\n%s\n", len(b), b)
	}
	r.fastImport(s.String() + "done\n")
	r.fastImportLoose(history(r.format, 5, "loose"))
	db := r.open()

	byPrefix := map[string]int{}
	for id := range r.allObjects() {
		byPrefix[id.String()[:4]]++
	}
	var prefixes []string
	for p, n := range byPrefix {
		if n > 1 && len(prefixes) < 25 {
			prefixes = append(prefixes, p, strings.ToUpper(p))
		}
	}
	if len(prefixes) == 0 {
		t.Fatal("no ambiguous prefixes to test")
	}
	for p, n := range byPrefix {
		if n == 1 && len(prefixes) < 60 {
			prefixes = append(prefixes, p, p+"0")
		}
	}
	prefixes = append(prefixes, "0000", "fffff")
	for _, p := range prefixes {
		got, err := db.FindPrefix(p, 0)
		if err != nil {
			t.Fatal(err)
		}
		var gotHex []string
		for _, id := range got {
			gotHex = append(gotHex, id.String())
		}
		want := strings.Fields(r.gitString("rev-parse", "--disambiguate="+strings.ToLower(p)))
		slices.Sort(want)
		if !slices.Equal(gotHex, want) {
			t.Errorf("FindPrefix(%s) = %v, want %v", p, gotHex, want)
		}
		if len(want) > 1 {
			if one, _ := db.FindPrefix(p, 1); len(one) != 1 {
				t.Errorf("FindPrefix(%s, 1) returned %d names", p, len(one))
			}
		}
	}
	for _, bad := range []string{"abc", "xyzw", strings.Repeat("a", 41)} {
		if _, err := db.FindPrefix(bad, 0); err == nil {
			t.Errorf("FindPrefix(%q) succeeded", bad)
		}
	}
}

func TestRescanAfterRepack(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 10, "before"))
	r.git(nil, "repack", "-a", "-d", "-q")
	db := r.open()
	before := r.allObjects()
	checkAllObjects(t, db, before)

	// Another process fetches and repacks everything into one new pack,
	// deleting the pack db has open.
	r.fastImport(strings.ReplaceAll(history(r.format, 10, "after"), "refs/heads/main", "refs/heads/after"))
	r.git(nil, "repack", "-a", "-d", "-q")
	r.git(nil, "prune-packed")
	all := r.allObjects()
	newID := r.id(r.gitString("rev-parse", "after"))
	if ids, err := db.FindPrefix(newID.String()[:8], 0); err != nil || !slices.Contains(ids, newID) {
		t.Errorf("FindPrefix after repack = %v %v", ids, err)
	}
	checkAllObjects(t, db, all)
}

func TestContainsDoesNotRescan(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 3, "old"))
	db := r.open()
	old := r.id(r.gitString("rev-parse", "main"))
	if !db.Contains(old) {
		t.Fatal("Contains(packed object) = false")
	}
	r.fastImport(strings.ReplaceAll(history(r.format, 3, "new"), "refs/heads/main", "refs/heads/new"))
	fresh := r.id(r.gitString("rev-parse", "new"))
	if db.Contains(fresh) {
		t.Fatal("Contains saw a pack written after the last scan")
	}
	if err := db.Rescan(); err != nil {
		t.Fatal(err)
	}
	if !db.Contains(fresh) {
		t.Error("Contains after Rescan = false")
	}
	if db.Contains(r.id(strings.Repeat("ab", 20))) {
		t.Error("Contains(missing) = true")
	}
	db.Close()
	if db.Contains(old) || db.Rescan() == nil {
		t.Error("Contains/Rescan after Close succeeded")
	}
}

func TestConcurrentReadsDuringRepack(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 40, "conc"))
	r.git(nil, "repack", "-a", "-d", "-q")
	objs := r.allObjects()
	db := r.open()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := range 8 {
		wg.Go(func() {
			for round := range 3 {
				for id, want := range objs {
					kind, data, err := db.Read(id)
					if err != nil || kind != want.kind || !bytes.Equal(data, want.data) {
						errs <- fmt.Errorf("goroutine %d round %d: Read(%s): %v", g, round, id, err)
						return
					}
					if _, _, err := db.Header(id); err != nil {
						errs <- err
						return
					}
				}
			}
		})
	}
	if runtime.GOOS != "windows" { // Windows cannot delete packs that are open
		r.git(nil, "repack", "-a", "-d", "-f", "-q")
		db.rescan()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClosedDB(t *testing.T) {
	r := newRepo(t)
	r.fastImport(history(r.format, 2, "closed"))
	db := r.open()
	id := r.id(r.gitString("rev-parse", "main"))
	db.Close()
	if _, _, err := db.Read(id); err == nil {
		t.Error("Read after Close succeeded")
	}
}
