package checkout

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// TestPathSetMatchesPrefixScan checks pathSet against the map with prefix
// scans it replaced, on names that share byte prefixes without nesting.
func TestPathSetMatchesPrefixScan(t *testing.T) {
	names := []string{"a", "ab", "a b", "a.b", "b"}
	randPath := func(r *rand.Rand) string {
		parts := make([]string, 1+r.IntN(4))
		for i := range parts {
			parts[i] = names[r.IntN(len(names))]
		}
		return strings.Join(parts, "/")
	}
	r := rand.New(rand.NewPCG(1, 2))
	var s pathSet
	ref := map[string]bool{}
	for range 20000 {
		p := randPath(r)
		switch r.IntN(4) {
		case 0, 1:
			s.add(p)
			ref[p] = true
		case 2:
			s.remove(p)
			delete(ref, p)
		case 3:
			var got []string
			s.removeUnder(p, func(q string) { got = append(got, q) })
			var want []string
			for q := range ref {
				if strings.HasPrefix(q, p+"/") {
					want = append(want, q)
					delete(ref, q)
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("removeUnder(%q) = %q, want %q", p, got, want)
			}
		}
	}
	got := slices.Sorted(s.all())
	want := make([]string, 0, len(ref))
	for q := range ref {
		want = append(want, q)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("all() = %d paths, want %d", len(got), len(want))
	}
}
