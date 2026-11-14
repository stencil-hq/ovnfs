package export

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// resolution is what a table resolves: the path of every issued id, and the
// id of every path of the namespace, 0 for none. Orphans are derived ids
// held away from the directory they derive from, which was removed since:
// they resolve only while their holder is resident, whichever way the table
// was opened.
type resolution struct {
	paths   map[uint64]string
	ids     map[string]uint64
	orphans map[uint64]bool
}

// resolveAll resolves the issued ids and then the namespace's paths, in a
// fixed order: resolution can change what a table holds resident.
func resolveAll(t *testing.T, nt *nodeTable, ns *namespace, issued map[uint64]bool) resolution {
	t.Helper()
	r := resolution{paths: map[uint64]string{}, ids: map[string]uint64{}, orphans: map[uint64]bool{}}
	ids := slices.Sorted(func(yield func(uint64) bool) {
		for id := range issued {
			if !yield(id) {
				return
			}
		}
	})
	for _, id := range ids {
		r.paths[id] = pathOf(t, nt, id)
	}
	for _, p := range sortedEntries(ns) {
		if id, ok := nt.lookup(p); ok {
			r.ids[p] = id
		}
	}
	nt.mu.Lock()
	for _, id := range ids {
		if derived(id) && nt.pageKey(id>>hashBits&(maxDerivingKey-1)) == nil {
			r.orphans[id] = true
		}
	}
	nt.mu.Unlock()
	if nt.cacheErr != nil {
		t.Fatal(nt.cacheErr)
	}
	return r
}

// sameResolution fails unless got resolves every issued id and path as want,
// orphans aside unless both were opened alike.
func sameResolution(t *testing.T, what string, got, want resolution, alike bool) {
	t.Helper()
	for id, p := range want.paths {
		if got.orphans[id] != want.orphans[id] {
			t.Fatalf("%s: %x orphaned %v, want %v", what, id, got.orphans[id], want.orphans[id])
		}
		if got.orphans[id] && !alike {
			continue
		}
		if got.paths[id] != p {
			t.Fatalf("%s: %x → %q, want %q", what, id, got.paths[id], p)
		}
	}
	for p, id := range want.ids {
		if got.ids[p] != id {
			t.Fatalf("%s: %s → %x, want %x", what, p, got.ids[p], id)
		}
	}
	for p, id := range got.ids {
		if want.ids[p] != id {
			t.Fatalf("%s: %s → %x, unissued before", what, p, id)
		}
	}
}

// sortedEntries lists the namespace's paths in order.
func sortedEntries(ns *namespace) []string {
	var out []string
	for p := range ns.entries {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// copyState copies the state directory dir as a crash or a restart would
// find it, without the store's detached garbage, which a worker removes
// meanwhile.
func copyState(t *testing.T, dir string) string {
	t.Helper()
	to := t.TempDir()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.HasSuffix(d.Name(), ".discard") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// churn applies ops random operations through the export: creations,
// colliding names, issues, renames across directories replacing leaves,
// removals of subtrees resident or paged out, and evictions under a tiny
// node cache. It returns the ids it issued.
func churn(t *testing.T, rng *rand.Rand, nt *nodeTable, ns *namespace, ops int, serial *int) []uint64 {
	t.Helper()
	var issued []uint64
	issue := func(p string) {
		if id := nt.id(p); id != 0 {
			issued = append(issued, id)
		} else if nt.cacheErr != nil {
			t.Fatal(nt.cacheErr)
		}
	}
	pick := func() string {
		var out []string
		for _, p := range sortedEntries(ns) {
			if p != "pressure" && !strings.HasPrefix(p, "pressure/") {
				out = append(out, p)
			}
		}
		return out[rng.IntN(len(out))]
	}
	dirOf := func() string {
		if rng.IntN(8) == 0 {
			return ""
		}
		return pick()
	}
	join := func(dir, name string) string {
		if dir == "" {
			return name
		}
		return dir + "/" + name
	}
	hasChildren := func(p string) bool {
		for e := range ns.entries {
			if strings.HasPrefix(e, p+"/") {
				return true
			}
		}
		return false
	}
	for range ops {
		*serial++
		switch op := rng.IntN(10); {
		case op < 3: // a new file, or a new directory with a file
			p := join(dirOf(), fmt.Sprintf("n%d", *serial))
			if rng.IntN(2) == 0 {
				p += "/f"
			}
			ns.add(p)
			if rng.IntN(3) == 0 {
				nt.created(p)
			} else {
				issue(p)
			}
		case op < 4: // two names sharing a hash
			dir := dirOf()
			for _, name := range []string{collideA, collideB} {
				if p := join(dir, name); !ns.entries[p] {
					ns.add(p)
					issue(p)
				}
			}
		case op < 6:
			issue(pick())
		case op < 8: // a rename into another directory, maybe over a leaf
			from := pick()
			dir := dirOf()
			if dir == from || strings.HasPrefix(dir, from+"/") {
				continue
			}
			to := join(dir, fmt.Sprintf("m%d", *serial))
			var leaves []string
			for _, p := range sortedEntries(ns) {
				if d, _ := split(p); d == dir && p != from && !hasChildren(p) && !strings.HasPrefix(from, p+"/") && p != "pressure" {
					leaves = append(leaves, p)
				}
			}
			if len(leaves) > 0 && rng.IntN(3) == 0 {
				to = leaves[rng.IntN(len(leaves))]
			}
			ns.move(from, to)
			nt.rename(from, to)
		case op < 9:
			p := pick()
			ns.del(p)
			nt.remove(p)
		default: // page out everything that can go
			ns.add("pressure/entry")
			nt.mu.Lock()
			limit := nt.cacheLimit
			nt.cacheLimit = 0
			nt.mu.Unlock()
			issue("pressure/entry")
			nt.mu.Lock()
			nt.cacheLimit = limit
			nt.mu.Unlock()
		}
		if nt.cacheErr != nil {
			t.Fatal(nt.cacheErr)
		}
	}
	return issued
}

// TestAdoptedIndexMatchesReplay runs random operations under a tiny node
// cache through several adopt, change and close cycles. After each clean
// close, the adopted index and a replay of the same log must resolve every
// issued id and path as the closed table did, and issue the same new ids,
// none issued before. A crash after adoption must replay to the same ids.
func TestAdoptedIndexMatchesReplay(t *testing.T) {
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 50))
			dir := t.TempDir()
			name := filepath.Join(dir, "nodes.log")
			ns := newNamespace()
			for i := range 12 {
				ns.add(fmt.Sprintf("c%d/d%d/e/file", i%3, i), fmt.Sprintf("c%d/f%d", i%3, i))
			}
			issued := map[uint64]bool{}
			record := func(ids []uint64) {
				for _, id := range ids {
					issued[id] = true
				}
			}
			nt := openLogged(t, name, ns)
			nt.cacheLimit = 8 << 10
			serial := 0
			for _, p := range sortedEntries(ns) {
				record([]uint64{nt.id(p)})
			}
			for cycle := range 4 {
				record(churn(t, rng, nt, ns, 120, &serial))
				live := resolveAll(t, nt, ns, issued)
				for _, id := range live.ids {
					issued[id] = true // ancestors issue implicitly
				}
				crashed := copyState(t, dir)
				closeLogged(t, nt)
				clean := copyState(t, dir)
				if err := os.Remove(filepath.Join(clean, "nodes.log.cache", ownersName)); err != nil {
					t.Fatal(err)
				}

				crash := openLogged(t, filepath.Join(crashed, "nodes.log"), ns)
				if crash.opened.adopted {
					t.Fatalf("cycle %d: adopted after a crash", cycle)
				}
				sameResolution(t, fmt.Sprintf("cycle %d: crash", cycle), resolveAll(t, crash, ns, issued), live, false)
				closeLogged(t, crash)
				replayed := openLogged(t, filepath.Join(clean, "nodes.log"), ns)
				if replayed.opened.adopted {
					t.Fatalf("cycle %d: adopted without a manifest", cycle)
				}
				nt = openLogged(t, name, ns)
				if cycle > 0 && !nt.opened.adopted {
					t.Fatalf("cycle %d: replayed: %s", cycle, nt.opened.why)
				}
				adopted := resolveAll(t, nt, ns, issued)
				sameResolution(t, fmt.Sprintf("cycle %d: adopted", cycle), adopted, live, false)
				sameResolution(t, fmt.Sprintf("cycle %d: replayed", cycle), resolveAll(t, replayed, ns, issued), adopted, true)
				nt.cacheLimit = 8 << 10

				// New names get the same ids from either open, never
				// one issued before.
				for i := range 6 {
					serial++
					dir := sortedEntries(ns)[rng.IntN(len(ns.entries))]
					p := fmt.Sprintf("%s/new%d", dir, serial)
					if i%2 == 0 {
						p += "/f"
					}
					ns.add(p)
					got, want := nt.id(p), replayed.id(p)
					if got == 0 || got != want || issued[got] {
						t.Fatalf("cycle %d: %s → %x adopted, %x replayed (reissued %v)", cycle, p, got, want, issued[got])
					}
					issued[got] = true
				}
				closeLogged(t, replayed)
			}
		})
	}
}

// TestLogGrowthForcesRebuild replays and compacts a log that more than
// doubled since its compaction, instead of adopting the clean-stop index,
// and adopts again after that.
func TestLogGrowthForcesRebuild(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "nodes.log")
	ns := newNamespace("co/a/file", "co/b/file", "other/file")
	nt := openLogged(t, name, ns)
	want := map[string]uint64{}
	for _, p := range sortedEntries(ns) {
		want[p] = nt.id(p)
	}
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	compacted := nt.compacted
	for from, to := "co/a", "co/c"; logSize(t, name) <= 2*compacted+growthSlack; from, to = to, from {
		ns.move(from, to)
		nt.rename(from, to)
		if nt.cacheErr != nil || nt.logErr != nil {
			t.Fatal(nt.cacheErr, nt.logErr)
		}
	}
	at := func(p string) string { return strings.Replace(p, "co/a", "co/c", 1) }
	moved := map[string]uint64{}
	if nt.find("co/c") != nil {
		for p, id := range want {
			moved[at(p)] = id
		}
		want = moved
	}
	grown := logSize(t, name)
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if nt.opened.adopted || !strings.Contains(nt.opened.why, "log grew") {
		t.Fatalf("adopted a log grown from %d to %d bytes: %v", compacted, grown, nt.opened)
	}
	if size := logSize(t, name); size >= grown/10 || nt.compacted != size {
		t.Fatalf("compacted %d bytes to %d (recorded %d)", grown, size, nt.compacted)
	}
	checkIDs(t, nt, want)
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if !nt.opened.adopted {
		t.Fatalf("replayed after the rebuild: %v", nt.opened)
	}
	checkIDs(t, nt, want)
}

// TestUnreadableOwnerFromLastOpenDegrades treats an owner index the previous
// open wrote, then adopted with its index, as prior: unreadable, it counts as
// missing for its directory, which resolves by listing, rather than latching
// the identity fault.
func TestUnreadableOwnerFromLastOpenDegrades(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/file", "pressure/entry")
	nt := openLogged(t, name, ns)
	id := nt.id("co/file")
	key := nt.find("co").key
	if key == 0 {
		t.Fatal("co is not logged")
	}
	forceCold(t, nt, ns, "co")
	if nt.find("co") != nil {
		t.Fatal("co stayed resident")
	}
	at := ownerHead(t, nt, key)
	closeLogged(t, nt)
	breakOwners(t, nt.pack.path, at)
	nt = openLogged(t, name, ns)
	if !nt.opened.adopted {
		t.Fatalf("replayed: %v", nt.opened)
	}
	if got := pathOf(t, nt, id); got != "co/file" || nt.cacheErr != nil {
		t.Fatalf("%x → %q, %v", id, got, nt.cacheErr)
	}
}

// TestNoManifestDuringIndexWrite leaves no manifest while an index write
// runs outside the node mutex: it would change the index after the manifest
// described it.
func TestNoManifestDuringIndexWrite(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/d/file")
	nt := openLogged(t, name, ns)
	nt.id("co/d/file")
	nt.mu.Lock()
	nt.indexWrites++ // as an eviction publishing a record would
	nt.mu.Unlock()
	closeLogged(t, nt)
	if _, err := os.Stat(filepath.Join(name+".cache", ownersName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest written during an index write: %v", err)
	}
	nt = openLogged(t, name, ns)
	if nt.opened.adopted {
		t.Fatal("adopted an index a write was changing")
	}
}

// TestRetiredOwnersForceRebuild discounts the owner indexes of a removed
// paged-out subtree, also after a clean restart that adopted the index
// before they were all discounted, so that the garbage they leave in the
// pack makes a later open rebuild it.
func TestRetiredOwnersForceRebuild(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/keep/file")
	nt := openLogged(t, name, ns)
	nt.id("co/keep/file")
	for i := range 300 {
		for f := range 20 {
			p := fmt.Sprintf("co/big/d%03d/a-file-name-of-some-length-%02d", i, f)
			ns.add(p)
			nt.id(p)
		}
	}
	evictAll(t, nt, ns)
	if nt.find("co/big") != nil {
		t.Fatal("co/big stayed resident")
	}
	live := nt.coldBytes
	ns.del("co/big")
	nt.remove("co/big")
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if !nt.opened.adopted || len(nt.deadKeys) == 0 {
		t.Fatalf("adopted %v with %d retired keys to discount", nt.opened.adopted, len(nt.deadKeys))
	}
	for i := 0; len(nt.deadKeys) > 0; i++ {
		if i > 100 {
			t.Fatalf("%d retired keys left", len(nt.deadKeys))
		}
		nt.id("co/keep/file")
	}
	if nt.coldBytes > live/10 {
		t.Fatalf("%d of %d index bytes still live", nt.coldBytes, live)
	}
	closeLogged(t, nt)
	nt = openLogged(t, name, ns)
	if nt.opened.adopted {
		t.Fatal("adopted a pack of mostly retired owners")
	}
}

// retireBigSubtree issues ids for dirs directories under co/big, pages
// every logged node out and removes co/big, leaving most of its index files
// queued for reclamation.
func retireBigSubtree(t *testing.T, nt *nodeTable, ns *namespace, dirs int) {
	t.Helper()
	for i := range dirs {
		p := fmt.Sprintf("co/big/d%03d/file", i)
		ns.add(p)
		nt.id(p)
	}
	evictAll(t, nt, ns)
	if nt.find("co/big") != nil {
		t.Fatal("co/big stayed resident")
	}
	ns.del("co/big")
	nt.remove("co/big")
	nt.mu.Lock()
	defer nt.mu.Unlock()
	if len(nt.deadKeys) < 2*deadBatch {
		t.Fatalf("%d retired keys queued", len(nt.deadKeys))
	}
	nt.cacheLimit = DefaultNodeCache
}

// TestNoManifestDuringReclamation leaves no manifest when close comes while
// a reclamation batch has a retired key in flight: the manifest would omit
// that key and its subtree, whose files would stay until a rebuild.
func TestNoManifestDuringReclamation(t *testing.T) {
	name := filepath.Join(t.TempDir(), "nodes.log")
	ns := newNamespace("co/keep/file")
	nt := openLogged(t, name, ns)
	nt.id("co/keep/file")
	retireBigSubtree(t, nt, ns, 300)
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
	go func() {
		defer close(done)
		nt.lookup("") // reclaims a batch on its way out
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reclamation did not start")
	}
	closeLogged(t, nt)
	close(resume)
	<-done
	if _, err := os.Stat(filepath.Join(name+".cache", ownersName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest written during reclamation: %v", err)
	}
	nt = openLogged(t, name, ns)
	if nt.opened.adopted {
		t.Fatal("adopted an index whose reclamation was in flight")
	}
}

// TestLegacyIndexMigrates opens the stores clean closes of earlier layouts
// left, and rebuilds each into one pack without losing an issued owner or
// retired hash, so that names replaced behind the export cannot take over
// handles issued before the upgrade, and discards the old files. In
// testdata/owners1 (no logged node index) and testdata/owners3 alike, own's
// owners were evicted to a file of its own, packed's stayed resident until
// close packed them, and the gone names were removed through the export; in
// owners3, deep's records were files of their own too.
func TestLegacyIndexMigrates(t *testing.T) {
	for _, fixture := range []struct {
		dir           string
		packed, owner uint64 // ids of packed and packed's owner
		retired       map[string]uint64
	}{
		{"owners1", 7, 4503599657940000, map[string]uint64{"own/gone": 4503599636300732, "packed/gone": 4503599657272252}},
		{"owners3", 10, 4503599670522912, map[string]uint64{"own/gone": 4503599636300732, "packed/gone": 4503599669855164, "dead/x/y/file": 4503599664757595}},
	} {
		t.Run(fixture.dir, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", fixture.dir))); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(dir, "nodes.log")
			want := map[string]uint64{
				"own": 2, "own/" + collideA: 4503599636968480, "packed": fixture.packed, "packed/" + collideA: fixture.owner,
				"deep": 3, "deep/a": 4, "deep/a/b": 5, "deep/a/b/file": 4503599652174683,
			}
			// Behind the export, the issued names gave way to others of their
			// hash, and the retired ones came back.
			ns := newNamespace("deep/a/b/file", "own/"+collideB, "packed/"+collideB)
			for p := range fixture.retired {
				ns.add(p)
			}
			for restart := range 2 {
				nt := openLogged(t, name, ns)
				if restart == 0 && (nt.opened.adopted || nt.opened.why != "index of an earlier layout") {
					t.Fatalf("open %v", nt.opened)
				}
				if restart == 1 && !nt.opened.adopted {
					t.Fatalf("migrated index not adopted: %v", nt.opened)
				}
				for p, id := range want {
					if got := pathOf(t, nt, id); got != p {
						t.Fatalf("restart %d: %x → %q, want %q", restart, id, got, p)
					}
				}
				for p, id := range fixture.retired {
					if got := pathOf(t, nt, id); got != "<stale>" {
						t.Fatalf("restart %d: retired %x → %q", restart, id, got)
					}
					if nt.id(p) == id {
						t.Fatalf("restart %d: %s took its retired id", restart, p)
					}
				}
				for _, p := range []string{"own/" + collideB, "packed/" + collideB} {
					if id := nt.id(p); id == 0 || id == want[strings.Replace(p, collideB, collideA, 1)] {
						t.Fatalf("restart %d: %s took the issued id", restart, p)
					}
				}
				closeLogged(t, nt)
			}
			var legacy []string
			filepath.WalkDir(name+".cache", func(p string, e fs.DirEntry, err error) error {
				if err == nil && e.IsDir() && len(e.Name()) == 2 {
					legacy = append(legacy, p)
				}
				return err
			})
			if len(legacy) > 0 {
				t.Fatalf("legacy files remain: %v", legacy)
			}
		})
	}
}
