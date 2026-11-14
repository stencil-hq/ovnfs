package export

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// forceCold fills another directory so the requested directory is evicted,
// its listing at least, or its whole node when it pages out to the index.
func forceCold(t *testing.T, nt *nodeTable, ns *namespace, dir string) {
	t.Helper()
	ns.add("pressure/entry")
	t.Cleanup(func() {
		if err := nt.close(); err != nil {
			t.Error(err)
		}
	})
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	if d := nt.find(dir); d != nil && (d.dir == nil || d.dir.list != nil || len(d.dir.leaves) != 0) {
		t.Fatalf("%s was not evicted", dir)
	}
}

// ownerHead returns where key's owner index starts in the pack: its head
// segment, as its resident directory or its slot locates it.
func ownerHead(t *testing.T, nt *nodeTable, key uint64) int64 {
	t.Helper()
	nt.mu.Lock()
	defer nt.mu.Unlock()
	if d := nt.byKey[key]; d != nil {
		if d.dir == nil || !d.dir.cold {
			t.Fatalf("key %d has no owner index", key)
		}
		return d.dir.base
	}
	_, s, _, err := nt.index.read(key)
	if err != nil || s.key != key || s.owner.size == 0 {
		t.Fatalf("key %d has no owner index: %v", key, err)
	}
	return s.owner.base
}

// breakOwners claims more rows than the segment at at in pack holds, making
// its owner index unreadable, and returns a function restoring it.
func breakOwners(t *testing.T, pack string, at int64) (restore func()) {
	t.Helper()
	f, err := os.OpenFile(pack, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	var saved [4]byte
	if _, err := f.ReadAt(saved[:], at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff, 0xff, 0xff, 0xff}, at); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := f.WriteAt(saved[:], at); err != nil {
			t.Fatal(err)
		}
	}
}

// TestColdDerivedHandle resolves an evicted leaf and an evicted full node.
func TestColdDerivedHandle(t *testing.T) {
	for _, full := range []bool{false, true} {
		ns := newNamespace("co/d/file")
		nt := newNodeTable(ns.list)
		id := nt.id("co/d/file")
		if full {
			nt.mu.Lock()
			nt.node("co/d/file")
			nt.mu.Unlock()
		}
		forceCold(t, nt, ns, "co/d")
		before := ns.lists
		if got := pathOf(t, nt, id); got != "co/d/file" {
			t.Fatal(got)
		}
		if ns.lists != before {
			t.Fatal("cold handle reread a changed namespace")
		}
		if got := nt.id("co/d/file"); got != id {
			t.Fatalf("id changed %x -> %x", id, got)
		}
	}
}

// TestColdMutations preserves names before their backing listing changes,
// including an old derived hash that a newly created colliding name shares.
func TestColdMutations(t *testing.T) {
	for _, op := range []string{"rename", "remove", "collision"} {
		t.Run(op, func(t *testing.T) {
			ns := newNamespace("co/" + collideA)
			name := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, name, ns)
			old := nt.id("co/" + collideA)
			forceCold(t, nt, ns, "co")
			target := "co/" + collideA
			if op == "collision" {
				target = "co/" + collideB
			}
			// Cold ownership survives the backend namespace change.
			switch op {
			case "rename":
				ns.move(target, "co/new")
				nt.rename(target, "co/new")
			case "remove":
				ns.del(target)
				nt.remove(target)
				ns.add(target)
				nt.created(target)
			case "collision":
				ns.add(target)
				nt.created(target)
			}
			want := "co/" + collideA
			if op == "rename" {
				want = "co/new"
			}
			if op == "remove" {
				want = "<stale>"
			}
			if got := pathOf(t, nt, old); got != want {
				t.Fatalf("old handle = %s, want %s", got, want)
			}
			if op != "rename" && nt.id(target) == old {
				t.Fatal("reused old handle")
			}
			closeLogged(t, nt)
			for range 2 {
				nt = openLogged(t, name, ns)
				if got := pathOf(t, nt, old); got != want {
					t.Fatalf("after restart: %s, want %s", got, want)
				}
				closeLogged(t, nt)
			}
		})
	}
}

// TestColdStateIsHeld keeps live protocol state visible to subtree recalls,
// then permits eviction once the last state is released.
func TestColdStateIsHeld(t *testing.T) {
	ns := newNamespace("co/file")
	nt := newNodeTable(ns.list)
	id := nt.id("co/file")
	release := nt.retain(id)
	forceCold(t, nt, ns, "co")
	if n := nt.byID[id]; n == nil || n.key != 0 || n.holds != 1 {
		t.Fatal("state was evicted or logged")
	}
	if got := nt.subtree("co"); len(got) != 2 {
		t.Fatal(got)
	}
	release()
	nt.id("pressure/entry")
	if nt.byID[id] != nil {
		t.Fatal("released state stayed pinned")
	}
}

// TestColdCacheBound checks accounting and a plateau while touching new paths.
func TestColdCacheBound(t *testing.T) {
	ns := newNamespace()
	nt := newNodeTable(ns.list)
	nt.cacheLimit = 8192
	for dir := range 100 {
		d := fmt.Sprintf("co/d%d", dir)
		for f := range 30 {
			ns.add(fmt.Sprintf("%s/f%d", d, f))
		}
		for f := range 30 {
			nt.id(fmt.Sprintf("%s/f%d", d, f))
		}
		if nt.cacheUsed > nt.cacheLimit+8192 {
			t.Fatalf("cache grew to %d", nt.cacheUsed)
		}
		var total int64
		for _, n := range nt.byID {
			if s := n.dir; s != nil {
				total += s.cached
				if s.cached < 0 {
					t.Fatal("negative cache charge")
				}
			}
		}
		if total != nt.cacheUsed {
			t.Fatalf("accounted %d, actual %d", nt.cacheUsed, total)
		}
	}
	nt.remove("co")
	if nt.cacheUsed != 0 {
		t.Fatalf("removed cache retained %d", nt.cacheUsed)
	}
}

// TestColdListingGeneration rejects an observe that crosses eviction.
func TestColdListingGeneration(t *testing.T) {
	ns := newNamespace("co/file")
	nt := newNodeTable(ns.list)
	nt.id("co/file")
	l := nt.startListing("co")
	infos, _ := ns.list("co")
	forceCold(t, nt, ns, "co")
	nt.observe(l, infos)
	if nt.find("co").dir.list != nil {
		t.Fatal("late listing resurrected cache")
	}
}

// TestColdListingDoesNotHoldMutex checks the re-resolution I/O lock boundary.
func TestColdListingDoesNotHoldMutex(t *testing.T) {
	ns := newNamespace("co/file")
	nt := newNodeTable(ns.list)
	id := nt.id("co/file")
	forceCold(t, nt, ns, "co")
	nt.list = func(dir string) ([]os.FileInfo, error) {
		nt.mu.Lock()
		nt.mu.Unlock()
		return ns.list(dir)
	}
	if got := pathOf(t, nt, id); got != "co/file" {
		t.Fatal(got)
	}
}

// TestColdCollisionAfterUnobservedCreate covers intermediate MkdirAll entries
// and generated TempFile names: neither can be preserved before its creation.
func TestColdCollisionAfterUnobservedCreate(t *testing.T) {
	ns := newNamespace("co/" + collideA)
	nt := newNodeTable(ns.list)
	old := nt.id("co/" + collideA)
	forceCold(t, nt, ns, "co")
	ns.add("co/" + collideB + "/child")
	nt.created("co/" + collideB + "/child")
	if got := nt.id("co/" + collideA); got != old {
		t.Fatal("issued name changed identity")
	}
	other := nt.id("co/" + collideB)
	if other == old || pathOf(t, nt, old) != "co/"+collideA {
		t.Fatal("colliding create stole identity")
	}
}

// TestRecoverySurvivesPressure exercises a handle from before a restart, when
// the disposable index has no record yet. I/O pressure must not evict its read.
func TestRecoverySurvivesPressure(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	old := nt.id("co/file")
	d := nt.find("co")
	d.dir = nil
	nt.cacheUsed = 0
	nt.cacheLRU.Init()
	nt.cacheLimit = 0
	original := nt.list
	nt.list = func(dir string) ([]os.FileInfo, error) {
		if dir == "co" {
			nt.id("pressure/entry")
		}
		return original(dir)
	}
	if got := pathOf(t, nt, old); got != "co/file" {
		t.Fatal(got)
	}
	if nt.id("co/file") != old {
		t.Fatal("recovery minted a new identity")
	}
	t.Cleanup(func() { nt.close() })
}

// TestRecoveryRejectsRelocatedListing switches a directory's path during its
// unlocked read, then puts another tree at the old path.
func TestRecoveryRejectsRelocatedListing(t *testing.T) {
	ns := newNamespace("co/d/file")
	nt := newNodeTable(ns.list)
	old := nt.id("co/d/file")
	d := nt.find("co/d")
	d.dir = nil
	nt.cacheUsed = 0
	nt.cacheLRU.Init()
	original := nt.list
	moved := false
	nt.list = func(dir string) ([]os.FileInfo, error) {
		if !moved && dir == "co/d" {
			moved = true
			ns.move("co/d", "co/moved")
			nt.rename("co/d", "co/moved")
			ns.add("co/d/replacement")
		}
		return original(dir)
	}
	if got := pathOf(t, nt, old); got != "co/moved/file" {
		t.Fatal(got)
	}
	t.Cleanup(func() { nt.close() })
}

// TestColdCreateKeepsIdentity covers O_CREATE on an already existing file.
func TestColdCreateKeepsIdentity(t *testing.T) {
	ns := newNamespace("co/file")
	nt := newNodeTable(ns.list)
	old := nt.id("co/file")
	forceCold(t, nt, ns, "co")
	nt.created("co/file")
	if got := nt.id("co/file"); got != old {
		t.Fatalf("O_CREATE changed %x to %x", old, got)
	}
	ns.del("co/file")
	nt.remove("co/file")
	ns.add("co/file")
	nt.created("co/file")
	if _, ok := nt.path(old); ok {
		t.Fatal("old handle survived remove and recreation")
	}
}

// TestColdGuardRejectsReadFailure prevents backend changes until ownership is
// known, and keeps a successful preflight resident through concurrent pressure.
func TestColdGuardRejectsReadFailure(t *testing.T) {
	ns := newNamespace("co/file")
	nt := newNodeTable(ns.list)
	old := nt.id("co/file")
	forceCold(t, nt, ns, "co")
	d := nt.find("co")
	restore := breakOwners(t, nt.pack.path, ownerHead(t, nt, d.key))
	if release, err := nt.guard(false, "co/file"); err == nil {
		release()
		t.Fatal("unreadable ownership allowed a mutation")
	}
	restore()
	nt.cacheErr = nil
	nt.fault.Store(nil)
	release, err := nt.guard(false, "co/file")
	if err != nil {
		t.Fatal(err)
	}
	nt.id("pressure/entry")
	if !d.leaf("file") {
		t.Fatal("preflight owner was evicted")
	}
	ns.del("co/file")
	nt.remove("co/file")
	release()
	ns.add("co/file")
	nt.created("co/file")
	if _, ok := nt.path(old); ok {
		t.Fatal("failed preflight lost retirement")
	}
}

// TestObservationRejectsRelocation covers ordinary ReadDir, including a move
// away and back while another tree briefly occupied its original path.
func TestObservationRejectsRelocation(t *testing.T) {
	ns := newNamespace("co/d/file")
	nt := newNodeTable(ns.list)
	nt.id("co/d/file")
	l := nt.startListing("co/d")
	ns.move("co/d", "co/moved")
	nt.rename("co/d", "co/moved")
	ns.add("co/d/replacement")
	infos, _ := ns.list("co/d")
	ns.del("co/d")
	ns.move("co/moved", "co/d")
	nt.rename("co/moved", "co/d")
	nt.observe(l, infos)
	s := nt.find("co/d").dir
	if s.list != nil && s.list.index("replacement") >= 0 {
		t.Fatal("installed another directory's listing")
	}
}

// TestMutationCacheBound checks callers which do not request a file id.
func TestMutationCacheBound(t *testing.T) {
	ns := newNamespace("a/file", "b/file", "c/file")
	nt := newNodeTable(ns.list)
	nt.cacheLimit = 8192
	for _, dir := range []string{"a", "b", "c"} {
		nt.id(dir + "/file")
	}
	for i := range 1000 {
		name := fmt.Sprintf("a/new%d", i)
		ns.add(name)
		nt.created(name)
	}
	if nt.cacheUsed > nt.cacheLimit+100000 {
		t.Fatalf("mutation cache grew to %d", nt.cacheUsed)
	}
	nt.close()
}

// TestEmptyDirectoryIsEvictable avoids logging directories merely because
// they were listed, and removes their listing from the LRU with their node.
func TestEmptyDirectoryIsEvictable(t *testing.T) {
	ns := newNamespace("co/empty")
	nt := newNodeTable(ns.list)
	old := nt.id("co/empty")
	l := nt.startListing("co/empty")
	nt.observe(l, nil)
	if nt.byID[old].key != 0 {
		t.Fatal("listing logged an empty directory")
	}
	forceCold(t, nt, ns, "co")
	if nt.byID[old] != nil {
		t.Fatal("cold empty directory was retained")
	}
	if got := pathOf(t, nt, old); got != "co/empty" {
		t.Fatal(got)
	}
}

// TestColdReadFailureDoesNotAllocate prevents a failed restoration from
// permanently replacing an unchanged file's derived id with a counter id.
func TestColdReadFailureDoesNotAllocate(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := openLogged(t, filepath.Join(t.TempDir(), "nodes.log"), ns)
	old := nt.id("co/file")
	key := nt.find("co").key
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	restore := breakOwners(t, nt.pack.path, ownerHead(t, nt, key))
	before, next := logSize(t, nt.log.Name()), nt.next
	if id := nt.id("co/file"); id != 0 {
		t.Fatalf("failed read allocated %x", id)
	}
	if nt.cacheErr == nil || nt.next != next || logSize(t, nt.log.Name()) != before {
		t.Fatal("read failure changed logged identities")
	}
	restore()
	// A recovered host read cannot clear the fault or permit promotion.
	ns.del("co/file")
	nt.unlist("co", true)
	nt.subtree("co")
	ns.add("co/file/child")
	nt.startListing("co/file")
	if id := nt.id("co/file/child"); id != 0 {
		t.Fatalf("fault permitted allocation: %x", id)
	}
	if nt.byID[old] != nil {
		t.Fatal("fault resurrected the deleted identity")
	}
	if _, ok := nt.path(old); ok {
		t.Fatal("deleted identity names the replacement")
	}
	if nt.close() == nil {
		t.Fatal("close hid the index failure")
	}
}

// TestCommittedRenameFinishesAfterFault prepares a previously unobserved
// destination, then faults after backend commit and verifies restart's handle.
func TestCommittedRenameFinishesAfterFault(t *testing.T) {
	ns := newNamespace("co/source/file", "destination/deep")
	name := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, name, ns)
	child := nt.id("co/source/file")
	old := nt.id("co/source")
	nt.id("unrelated/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.cacheLimit = 1 << 20
	release, err := nt.prepareRename("co/source", "destination/deep/moved")
	if err != nil {
		t.Fatal(err)
	}
	ns.move("co/source", "destination/deep/moved")
	nt.mu.Lock()
	nt.cacheFailure(os.ErrPermission)
	nt.mu.Unlock()
	nt.rename("co/source", "destination/deep/moved")
	release()
	if nt.close() == nil {
		t.Fatal("fault disappeared")
	}
	nt = openLogged(t, name, ns)
	if got := pathOf(t, nt, old); got != "destination/deep/moved" {
		t.Fatal(got)
	}
	if got := pathOf(t, nt, child); got != "destination/deep/moved/file" {
		t.Fatal(got)
	}
}

// TestColdCloseKeepsAbsolutePaths protects unrelated files from a late
// invalidation after Close: discarded index paths must never become relative.
func TestColdCloseKeepsAbsolutePaths(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	nt.id("co/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	key := nt.find("co").key
	dir := t.TempDir()
	t.Chdir(dir)
	foreign := fmt.Sprint(key)
	if err := os.WriteFile(foreign, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := nt.close(); err != nil {
		t.Fatal(err)
	}
	nt.remove("co")
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "unrelated" {
		t.Fatalf("late invalidation touched unrelated file: %q %v", data, err)
	}
}

// TestDegradedBelowBudgetDoesNotLogWalks covers invalidation freeing the failed
// spill candidate. Recovery cannot depend on crossing the budget again.
func TestDegradedBelowBudgetDoesNotLogWalks(t *testing.T) {
	for _, repaired := range []bool{false, true} {
		t.Run(fmt.Sprint(repaired), func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := openLogged(t, filepath.Join(t.TempDir(), "nodes.log"), ns)
			t.Cleanup(func() { nt.close() })
			nt.id("co/file")
			blocked := filepath.Join(t.TempDir(), "blocked")
			if err := os.WriteFile(blocked, nil, 0600); err != nil {
				t.Fatal(err)
			}
			nt.coldRoot = blocked
			// The listings exceed 210 bytes; the logged co and pressure stay.
			nt.cacheLimit = 210 + loggedSize("co") + loggedSize("pressure") + 2*heldBytes
			nt.id("pressure/entry")
			if nt.spillErr == nil {
				t.Fatal("spill did not fail")
			}
			nt.unlist("co", true)
			if nt.cacheUsed > nt.cacheLimit {
				t.Fatal("invalidation did not free the listing")
			}
			if repaired {
				nt.coldRoot = ""
			}
			if repaired {
				nt.spillRetry = time.Time{}
			}
			next, before := nt.next, logSize(t, nt.log.Name())
			for i := range 1000 {
				p := fmt.Sprintf("fresh/f%d", i)
				ns.add(p)
				id := nt.id(p)
				if id == 0 {
					if repaired {
						t.Fatal("repaired storage refused admission")
					}
					continue
				}
				if id&derivedBit == 0 {
					t.Fatalf("read-only walk allocated counter %x", id)
				}
				// One current directory may exceed the budget, never permanent nodes.
				var current int64
				if d := nt.find("fresh"); d != nil && d.dir != nil {
					current = d.dir.cached
				}
				if nt.cacheUsed > nt.cacheLimit+current {
					t.Fatalf("cache grew to %d", nt.cacheUsed)
				}
			}
			if nt.next-next > 2 || len(nt.byID) > 6 || len(nt.byKey) > 6 || logSize(t, nt.log.Name())-before > 256 {
				t.Fatalf("walk grew permanent state: counters %d nodes %d keys %d log bytes %d", nt.next-next, len(nt.byID), len(nt.byKey), logSize(t, nt.log.Name())-before)
			}
			if repaired && nt.spillErr != nil {
				t.Fatalf("degradation did not clear: %v", nt.spillErr)
			}
			t.Logf("repaired=%v cache=%d counters=%d nodes=%d keys=%d log_growth=%d degraded=%v", repaired, nt.cacheUsed, nt.next-next, len(nt.byID), len(nt.byKey), logSize(t, nt.log.Name())-before, nt.spillErr)
		})
	}
}
