package export

import (
	"os"
	"slices"
	"testing"

	"github.com/go-git/go-billy/v5"
)

type noStatFS struct{ billy.Filesystem }

func (noStatFS) Lstat(string) (os.FileInfo, error) { panic("tree invalidation statted an issued path") }

// A subtree rescan must recall resident and indexed owners without recovering
// unrelated directories or statting paths which may already have disappeared.
func TestColdSubtreeInvalidation(t *testing.T) {
	for _, state := range []string{"evicted", "partial", "held"} {
		t.Run(state, func(t *testing.T) {
			ns := newNamespace("co/t/"+collideA, "co/t/keep", "co/s/file")
			nt := newNodeTable(ns.list)
			old, keep := nt.id("co/t/"+collideA), nt.id("co/t/keep")
			if state == "held" {
				defer nt.retain(keep)()
			}
			forceCold(t, nt, ns, "co/t")
			nt.cacheLimit = 1 << 20
			sibling := nt.id("co/s/file")
			if state == "partial" {
				nt.mu.Lock()
				nt.addLeaf(nt.find("co/t"), "keep")
				nt.mu.Unlock()
			}
			n := &nodeFS{nodes: nt, Filesystem: noStatFS{}}
			e := &Export{fs: n}
			var recalled []uint64
			e.OnRecall(func(id uint64) { recalled = append(recalled, id) })
			ns.del("co/t/" + collideA)
			ns.add("co/t/" + collideB)
			before := ns.lists
			e.Invalidate("co", nil, []string{"t"})
			if ns.lists != before {
				t.Fatal("tree invalidation listed the backend")
			}
			for _, id := range []uint64{old, keep} {
				if !slices.Contains(recalled, id) {
					t.Fatalf("missed owner %x in %v", id, recalled)
				}
				if count := len(slices.DeleteFunc(slices.Clone(recalled), func(v uint64) bool { return v != id })); count != 1 {
					t.Fatalf("recalled owner %x %d times", id, count)
				}
			}
			if slices.Contains(recalled, sibling) {
				t.Fatal("recalled a sibling")
			}
			if nt.find("co/t").dir.list != nil || nt.find("co/s").dir.list == nil {
				t.Fatal("invalidated the wrong listings")
			}
			if got := nt.id("co/t/" + collideB); got == old {
				t.Fatal("collision inherited a cold owner")
			}
			if got := pathOf(t, nt, old); got != "co/t/"+collideA {
				t.Fatal(got)
			}
			if got := nt.id("co/t/keep"); got != keep {
				t.Fatal("relisting replaced a retained owner")
			}
		})
	}
}

func TestColdLeafTreeInvalidation(t *testing.T) {
	ns := newNamespace("co/empty")
	nt := newNodeTable(ns.list)
	id := nt.id("co/empty")
	forceCold(t, nt, ns, "co")
	e := &Export{fs: &nodeFS{nodes: nt, Filesystem: noStatFS{}}}
	var recalled []uint64
	e.OnRecall(func(id uint64) { recalled = append(recalled, id) })
	e.Invalidate("co", nil, []string{"empty"})
	if !slices.Equal(recalled, []uint64{id}) {
		t.Fatal(recalled)
	}
}
