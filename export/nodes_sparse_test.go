package export

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A client may retain just one handle in each of several wide directories.
// Recovering every owner on each alternating GETATTR makes work proportional
// to directory width even though only four names are requested.
func TestSparseColdHandles(t *testing.T) {
	testSparseColdLookups(t, func(t *testing.T, nt *nodeTable, ns *namespace) func(string, uint64) {
		return func(p string, id uint64) {
			if got := pathOf(t, nt, id); got != p {
				t.Fatalf("%q != %q", got, p)
			}
		}
	})
}

func TestSparseColdIDs(t *testing.T) {
	testSparseColdLookups(t, func(t *testing.T, nt *nodeTable, ns *namespace) func(string, uint64) {
		n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
		return func(p string, id uint64) {
			if got := n.ID(p); got != id {
				t.Fatalf("ID(%q) = %x, want %x", p, got, id)
			}
		}
	})
}

func testSparseColdLookups(t *testing.T, lookup func(*testing.T, *nodeTable, *namespace) func(string, uint64)) {
	t.Helper()
	for _, budget := range []int64{0, 4096, 1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace("pressure/entry")
			nt := newNodeTable(ns.list)
			defer nt.close()
			var ids []uint64
			var paths []string
			for d := range 4 {
				for f := range 4096 {
					ns.add(fmt.Sprintf("d%d/f%04d", d, f))
				}
			}
			for d := range 4 {
				for f := range 4096 {
					p := fmt.Sprintf("d%d/f%04d", d, f)
					id := nt.id(p)
					if f == 0 {
						ids = append(ids, id)
						paths = append(paths, p)
					}
				}
			}
			check := lookup(t, nt, ns)
			nt.cacheLimit = 0
			nt.id("pressure/entry")
			// Keep a real listing pinned just as mutation preparation does. This
			// consumes the larger allowance without allowing trim to free it and
			// accidentally turning this into an under-budget batch test.
			if budget > 4096 {
				nt.mu.Lock()
				d := nt.find("pressure")
				for i := 0; nt.cacheUsed < budget-4096; i++ {
					nt.addLeaf(d, fmt.Sprintf("p%06d-%s", i, strings.Repeat("p", 232)))
				}
				d.dir.reading++
				nt.mu.Unlock()
				defer func() { d.dir.reading-- }()
			}
			nt.cacheLimit = budget
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			for i := range 512 {
				j := i % len(ids)
				check(paths[j], ids[j])
			}
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			t.Logf("512 alternating handles: %s allocated=%d", time.Since(start), allocated)
			if allocated > 4<<20 {
				t.Fatalf("sparse recovery allocated %d bytes", allocated)
			}
			for d := range 4 {
				s := nt.find(fmt.Sprintf("d%d", d)).dir
				if len(s.leaves) > 1 {
					t.Fatal("sparse recovery admitted unrelated owners")
				}
			}
		})
	}
}

func TestSparseColdPreRestartHandle(t *testing.T) {
	for _, budget := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			ns := newNamespace("co/first", "co/second", "pressure/entry")
			log := filepath.Join(t.TempDir(), "nodes.log")
			nt := openLogged(t, log, ns)
			first, second := nt.id("co/first"), nt.id("co/second")
			closeLogged(t, nt)
			nt = openLogged(t, log, ns)
			if got := pathOf(t, nt, first); got != "co/first" {
				t.Fatal(got)
			}
			forceCold(t, nt, ns, "co")
			if budget != 0 {
				d := nt.find("pressure")
				for i := 0; nt.cacheUsed < budget-4096; i++ {
					nt.addLeaf(d, fmt.Sprintf("p%06d-%s", i, strings.Repeat("p", 232)))
				}
				d.dir.reading++
				defer func() { d.dir.reading-- }()
			}
			nt.cacheLimit = budget
			if got := pathOf(t, nt, second); got != "co/second" {
				t.Fatalf("pre-restart handle: %q", got)
			}
			if got := pathOf(t, nt, first); got != "co/first" {
				t.Fatal(got)
			}
			if nt.cacheErr != nil {
				t.Fatal(nt.cacheErr)
			}
		})
	}
}

// A partially recovered directory can contain leaves and materialized nodes.
// Looking up either must not recover the directory's other issued owners.
func TestSparseResidentIDs(t *testing.T) {
	for _, budget := range []int64{0, 4096, 1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			for _, materialize := range []bool{false, true} {
				ns := newNamespace("co/file", "co/other", "pressure/entry")
				nt := newNodeTable(ns.list)
				defer nt.close()
				id := nt.id("co/file")
				nt.id("co/other")
				forceCold(t, nt, ns, "co")
				nt.cacheLimit = budget
				d := nt.find("co")
				nt.addLeaf(d, "file")
				if materialize {
					nt.child(d, "file")
				}
				nt.mu.Lock()
				nt.trimCache()
				nt.mu.Unlock()
				n := (&Export{fs: &nodeFS{nodes: nt}}).Nodes()
				if got := n.ID("co/file"); got != id {
					t.Fatalf("resident ID = %x, want %x", got, id)
				}
				if len(d.dir.leaves) > 1 {
					t.Fatal("resident ID loaded unrelated owners")
				}
			}
		})
	}
}
