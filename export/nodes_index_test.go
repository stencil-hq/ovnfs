package export

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNodeIndexCloseWaitsForReaders(t *testing.T) {
	ns := newNamespace("co/file")
	log := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, log, ns)
	nt.id("co/file")
	key := nt.find("co").key
	closeLogged(t, nt)
	nt = openLogged(t, log, ns)
	x := nt.index

	// Hold the same lifetime lease as a mapped read. Wait until close queues
	// its exclusive lock, without pausing production I/O through a test hook.
	x.mu.RLock()
	held := true
	defer func() {
		if held {
			x.mu.RUnlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- nt.close() }()
	deadline := time.After(2 * time.Second)
	for x.mu.TryRLock() {
		x.mu.RUnlock()
		select {
		case err := <-done:
			t.Fatalf("close did not wait for the mapped reader: %v", err)
		case <-deadline:
			t.Fatal("close did not reach the index")
		default:
			runtime.Gosched()
		}
	}
	if slot, found, err := x.slotLocked(key); err != nil || !found || slot.key != key {
		t.Fatalf("in-flight mapped read failed: found=%t slot=%+v err=%v", found, slot, err)
	}
	x.mu.RUnlock()
	held = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish after its mapped reader")
	}

	// Both table and runtime accessors reject late work before using any
	// file, including publication paths which locate a slot through the map.
	for _, k := range []uint64{key, x.base} {
		if _, _, _, err := x.read(k); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("record after close: %v", err)
		}
		if err := x.publish(k, 0, nil, 0, nodeOwner{}); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("publication after close: %v", err)
		}
	}
	if id := nt.id("co/file"); id != 0 || nt.cacheErr == nil {
		t.Fatal("late request did not fail closed")
	}
}

func TestNodeIndexClosePartial(t *testing.T) {
	for opened := 0; opened <= 3; opened++ {
		t.Run(fmt.Sprint(opened), func(t *testing.T) {
			x := &nodeIndex{}
			files := []**os.File{&x.table, &x.records, &x.runtime}
			for _, f := range files[:opened] {
				var err error
				*f, err = os.CreateTemp(t.TempDir(), "index-")
				if err != nil {
					t.Fatal(err)
				}
			}
			// Construction failures reach close before mapped.Open succeeds.
			if err := x.close(); err != nil {
				t.Fatal(err)
			}
			for _, f := range files[:opened] {
				if _, err := (*f).Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("partial construction leaked a file: %v", err)
				}
			}
			if err := x.close(); err != nil {
				t.Fatalf("repeated close: %v", err)
			}
		})
	}
}

func TestKnownNameGuardRestoresOnlyItsHash(t *testing.T) {
	ns := newNamespace("co/" + collideA)
	nt := newNodeTable(ns.list)
	defer nt.close()
	nt.id("co/" + collideA)
	for i := range 64 {
		p := fmt.Sprintf("co/unrelated-%d", i)
		ns.add(p)
		nt.id(p)
	}
	forceCold(t, nt, ns, "co")
	release, err := nt.guard(false, "co/"+collideB)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	d := nt.find("co")
	if !d.leaf(collideA) || len(d.dir.leaves) != 1 {
		t.Fatalf("known-name guard restored %d owners", len(d.dir.leaves))
	}
}

func TestIndexWriteDoesNotHoldNodeMutex(t *testing.T) {
	ns := newNamespace("co/file", "other/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	id := nt.id("co/file")
	other := nt.id("other/file")
	defer nt.close()
	nt.cacheLimit = 0
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		nt.id("pressure/entry")
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("index write did not start")
	}
	unrelated := make(chan struct{})
	go func() { nt.mu.Lock(); _ = nt.byID[other]; nt.mu.Unlock(); close(unrelated) }()
	select {
	case <-unrelated:
	case <-time.After(time.Second):
		close(resume)
		t.Fatal("index I/O held node mutex")
	}
	// Move the owning directory while the write is in flight. Publication
	// must revalidate ownership instead of evicting stale state.
	nt.mu.Lock()
	d := nt.find("co")
	nt.moved(d)
	delete(d.parent.kids, d.name)
	d.name = "moved"
	d.parent.kids[d.name] = d
	d.state().gen++
	nt.mu.Unlock()
	close(resume)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("index I/O did not finish")
	}
	if got := pathOf(t, nt, id); got != "moved/file" {
		t.Fatal(got)
	}
}

func TestDegradedColdReadsDoNotAccumulateOwners(t *testing.T) {
	ns := newNamespace("pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	var ids []uint64
	for i := range 32 {
		p := fmt.Sprintf("co/d%d/file", i)
		ns.add(p)
		ids = append(ids, nt.id(p))
	}
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.spillErr = fmt.Errorf("unavailable spill")
	nt.spillRetry = time.Now().Add(time.Hour)
	used := nt.cacheUsed
	for i, id := range ids {
		p := fmt.Sprintf("co/d%d/file", i)
		if got := pathOf(t, nt, id); got != p {
			t.Fatal(got)
		}
		if got := nt.id(p); got != id {
			t.Fatalf("%x != %x", got, id)
		}
	}
	refs := nt.subtree("co")
	seen := make(map[uint64]bool)
	for _, ref := range refs {
		seen[ref] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatal("cold subtree omitted an owner")
		}
	}
	if nt.cacheUsed != used {
		t.Fatalf("cold reads grew cache %d -> %d", used, nt.cacheUsed)
	}
	release := nt.retain(ids[0])
	if nt.byID[ids[0]] == nil || nt.byID[ids[0]].holds != 1 {
		t.Fatal("cold live state was not retained")
	}
	release()
}

func TestGuardAcquiredDuringSpill(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			ns := newNamespace("co/file", "pressure/entry")
			nt := newNodeTable(ns.list)
			defer nt.close()
			old := nt.id("co/file")
			nt.cacheLimit = 0
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
			done := make(chan struct{})
			go func() { nt.id("pressure/entry"); close(done) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("spill did not start")
			}
			var release func()
			var err error
			if prepared {
				release, err = nt.prepareRename("co/file", "co/moved")
			} else {
				release, err = nt.guard(false, "co/file")
			}
			if err != nil {
				close(resume)
				t.Fatal(err)
			}
			close(resume)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("spill did not finish")
			}
			nt.mu.Lock()
			present := nt.find("co").dir.reading > 0 && (nt.byID[old] != nil || nt.find("co").leaf("file"))
			nt.mu.Unlock()
			if !present {
				release()
				t.Fatal("spill evicted prepared mutation state")
			}
			ns.move("co/file", "co/moved")
			nt.rename("co/file", "co/moved")
			release()
			if got := pathOf(t, nt, old); got != "co/moved" {
				t.Fatal(got)
			}
		})
	}
}

func TestRecoveryAllowsDeepColdPaths(t *testing.T) {
	ns := newNamespace("pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	p := "co"
	for range 24 {
		p += "/d"
		ns.add(p + "/file")
		nt.id(p + "/file")
	}
	old := nt.id(p + "/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.cacheLimit = 1 << 20
	if got := nt.id(p + "/file"); got != old {
		t.Fatalf("deep recovery %x != %x", got, old)
	}
	release, err := nt.prepareRename(p+"/file", "pressure/moved")
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestRetiredIndexDiscounted(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	nt.id("co/file")
	forceCold(t, nt, ns, "co")
	size := nt.find("co").dir.coldBytes
	before := nt.coldBytes
	nt.remove("co")
	if size == 0 || nt.coldBytes != before-size {
		t.Fatalf("index bytes %d != %d", nt.coldBytes, before-size)
	}
}

func TestCommittedColdMutationsAfterFault(t *testing.T) {
	for _, op := range []string{"remove", "collision"} {
		t.Run(op, func(t *testing.T) {
			ns := newNamespace("co/"+collideA, "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			old := nt.id("co/" + collideA)
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			nt.cacheLimit = 1 << 20
			target := "co/" + collideA
			if op == "collision" {
				target = "co/" + collideB
			}
			release, err := nt.guard(false, target)
			if err != nil {
				t.Fatal(err)
			}
			if op == "remove" {
				ns.del(target)
			} else {
				ns.add(target)
			}
			nt.mu.Lock()
			nt.cacheFailure(os.ErrPermission)
			nt.mu.Unlock()
			if op == "remove" {
				nt.remove(target)
				ns.add(target)
			}
			nt.created(target)
			release()
			if nt.close() == nil {
				t.Fatal("fault disappeared")
			}
			nt = openLogged(t, log, ns)
			defer nt.close()
			fresh := nt.id(target)
			if fresh == 0 || fresh == old || fresh&derivedBit != 0 {
				t.Fatal("committed replacement was not logged")
			}
			if op == "remove" {
				if _, ok := nt.path(old); ok {
					t.Fatal("removed handle names replacement after restart")
				}
			} else if got := pathOf(t, nt, old); got != "co/"+collideA {
				t.Fatal(got)
			}
		})
	}
}

func TestDegradedCheckoutRetirement(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	log := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, log, ns)
	old := nt.id("co/file")
	key := nt.find("co").key
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	nt.spillErr = os.ErrPermission
	nt.spillRetry = time.Now().Add(time.Hour)
	e := &Export{fs: &nodeFS{nodes: nt}}
	e.OnRecall(func(uint64) {})
	e.Retire("co")
	if nt.byKey[key] != nil || pathOf(t, nt, old) != "<stale>" {
		t.Fatal("degraded retirement left old key live")
	}
	ns.del("co")
	ns.add("co/file")
	nt.cacheLimit = 1 << 20
	nt.spillErr = nil
	fresh := nt.id("co/file")
	if fresh == old {
		t.Fatal("checkout name reuse inherited old handle")
	}
	closeLogged(t, nt)
	nt = openLogged(t, log, ns)
	defer nt.close()
	if _, ok := nt.path(old); ok {
		t.Fatal("retired checkout handle recovered after restart")
	}
}

func TestDegradedInvalidationRetiresColdOwners(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(fmt.Sprint(whole), func(t *testing.T) {
			n := newMetadataTestFS(t)
			for _, p := range []string{"co/" + collideA, "pressure/entry"} {
				dir, _ := split(p)
				if err := n.Filesystem.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				f, err := n.Filesystem.Create(p)
				if err != nil {
					t.Fatal(err)
				}
				f.Close()
			}
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt, err := openNodeTable(log, n.Filesystem.ReadDir)
			if err != nil {
				t.Fatal(err)
			}
			n.nodes = nt
			old := nt.id("co/" + collideA)
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			nt.spillErr = os.ErrPermission
			nt.spillRetry = time.Now().Add(time.Hour)
			if err := n.Filesystem.Remove("co/" + collideA); err != nil {
				t.Fatal(err)
			}
			paths := []string{collideA}
			// Explicit paths retire missing handles; tree rescans only recall them.
			if whole {
				(&Export{fs: n}).Invalidate("co", nil, []string{""})
			}
			(&Export{fs: n}).Invalidate("co", paths, nil)
			if nt.cacheErr != nil || nt.spillErr == nil || nt.cacheUsed <= nt.cacheLimit {
				t.Fatal("probe lost degraded pressure")
			}
			if _, ok := nt.path(old); ok {
				t.Fatal("invalidation left cold handle live")
			}
			f, err := n.Filesystem.Create("co/" + collideB)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			nt.cacheLimit = 1 << 20
			nt.spillErr = nil
			nt.created("co/" + collideB)
			if nt.id("co/"+collideB) == old {
				t.Fatal("collision inherited invalidated handle")
			}
			closeLogged(t, nt)
			nt, err = openNodeTable(log, n.Filesystem.ReadDir)
			if err != nil {
				t.Fatal(err)
			}
			defer nt.close()
			if _, ok := nt.path(old); ok {
				t.Fatal("invalidated handle recovered after restart")
			}
		})
	}
}

func TestPreparedDestinationRemainsResident(t *testing.T) {
	for _, fatal := range []bool{false, true} {
		t.Run(fmt.Sprint(fatal), func(t *testing.T) {
			ns := newNamespace("co/src", "co/dst/existing", "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			old := nt.id("co/src")
			release, err := nt.prepareRename("co/src", "co/dst/moved")
			if err != nil {
				t.Fatal(err)
			}
			nt.id("co/dst/existing")
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			d := nt.find("co/dst")
			if d.dir == nil || d.dir.reading == 0 || d.dir.list == nil {
				release()
				t.Fatal("prepared destination escaped guard")
			}
			reads := 0
			nt.indexIO = func() { reads++ }
			ns.move("co/src", "co/dst/moved")
			nt.mu.Lock()
			if fatal {
				nt.cacheFailure(os.ErrPermission)
			} else {
				nt.spillErr = os.ErrPermission
				nt.spillRetry = time.Now().Add(time.Hour)
			}
			nt.mu.Unlock()
			nt.rename("co/src", "co/dst/moved")
			release()
			if reads != 0 || nt.byID[old] == nil || nt.byID[old].fullPath() != "co/dst/moved" {
				t.Fatal("committed move needed index recovery or lost its identity")
			}
			if err := nt.close(); (err != nil) != fatal {
				t.Fatal(err)
			}
			nt = openLogged(t, log, ns)
			defer nt.close()
			if got := pathOf(t, nt, old); got != "co/dst/moved" {
				t.Fatal(got)
			}
		})
	}
}

func TestFirstSpillPreservesGuardedCompletion(t *testing.T) {
	ns := newNamespace("co/"+collideA, "pressure/entry")
	log := filepath.Join(t.TempDir(), "nodes.log")
	nt := openLogged(t, log, ns)
	defer func() { nt.close() }()
	old := nt.id("co/" + collideA)
	nt.cacheLimit = 0
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	nt.indexIO = func() { once.Do(func() { close(entered); <-resume }) }
	done := make(chan struct{})
	go func() { nt.id("pressure/entry"); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first spill did not start")
	}
	release, err := nt.guard(false, "co/"+collideB)
	if err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("spill did not finish")
	}
	ns.add("co/" + collideB)
	nt.mu.Lock()
	nt.cacheFailure(os.ErrPermission)
	nt.mu.Unlock()
	nt.created("co/" + collideB)
	release()
	if err := nt.close(); err == nil {
		t.Fatal("fault disappeared")
	}
	ns.del("co/" + collideA)
	nt = openLogged(t, log, ns)
	if got := pathOf(t, nt, old); got != "<stale>" {
		t.Fatalf("guarded completion lost its collision record: old handle = %s", got)
	}
}
