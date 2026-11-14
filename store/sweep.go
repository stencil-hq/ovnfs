package store

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/bits"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// Sweep deletes objects not reachable from roots (trees walked recursively) last stored before before (a loose object's mtime, a packed one's append or re-store time), dropping them from the cache too; blobs referenced by trees but absent from the store are ignored. Returns the number removed.
// Packed objects are removed only from packs where removable objects make up
// at least a quarter of the bytes: such a pack is rewritten without them, its
// other frames copied as they are, and deleted once the copies are durable.
// Elsewhere unreachable packed objects stay stored (and uncounted) until a
// later sweep finds enough garbage beside them. The delta bases of every
// object still stored are kept too, as are every manifest's transitive private
// pages and content-defined chunks.
// A missing root or subtree (other than EmptyTree) is an error and nothing is
// removed, since its unknown children cannot be proven unreachable. Staging
// files older than before that no goroutine of this process owns (leftovers of
// a crash) are removed too but not counted. Objects published or re-stored
// during the sweep carry a fresh mtime and survive when before precedes the
// sweep, and so does everything reachable from a tree Build returned at or
// after before. Sweep waits for every Hold to be released, then, once
// marking ends, for chunked ingests in flight, and a new epoch begins before
// the first object is removed.
func (s *Store) Sweep(roots []OID, before time.Time) (int, error) {
	s.holds.Lock()
	defer s.holds.Unlock()
	loose, err := s.listLoose()
	if err != nil {
		return 0, fmt.Errorf("sweep: %w", err)
	}
	live := s.packs.pin(loose)
	defer s.packs.unpin()
	for _, root := range roots {
		if err := s.mark(root, live); err != nil {
			return 0, fmt.Errorf("sweep: %w", err)
		}
	}
	s.ingest.Lock()
	defer s.ingest.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Builds register under s.mu, so every root registered so far is seen here.
	s.rootsMu.Lock()
	var recent []OID
	for id, at := range s.roots {
		if at.Before(before) {
			delete(s.roots, id)
		} else {
			recent = append(recent, id)
		}
	}
	s.rootsMu.Unlock()
	for _, root := range recent {
		if err := s.mark(root, live); err != nil {
			return 0, fmt.Errorf("sweep: %w", err)
		}
	}
	removed := 0
	// begin starts a new epoch before the first removal.
	begin := func() error {
		if removed > 0 {
			return nil
		}
		if err := s.renewEpoch(); err != nil {
			return err
		}
		s.trees.clear()
		return nil
	}
	uncache := func(rawID) {}
	if s.cache != nil {
		uncache = func(id rawID) { s.cache.remove(id.oid()) }
	}
	removed, err = s.packs.sweep(live, before.UnixNano(), begin, uncache)
	if err != nil {
		return removed, fmt.Errorf("sweep: %w", err)
	}
	fanouts, err := os.ReadDir(s.objects)
	if err != nil {
		return removed, fmt.Errorf("sweep: %w", err)
	}
	for _, fan := range fanouts {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		dir := filepath.Join(s.objects, fan.Name())
		names, err := os.ReadDir(dir)
		if err != nil {
			return removed, fmt.Errorf("sweep: %w", err)
		}
		for _, e := range names {
			if strings.HasPrefix(e.Name(), stagePrefix) {
				if err := s.sweepStage(filepath.Join(dir, e.Name()), before); err != nil {
					return removed, fmt.Errorf("sweep: %w", err)
				}
				continue
			}
			id := OID(fan.Name() + e.Name())
			if !id.Valid() {
				continue
			}
			// Objects stored since marking began are fresh.
			if isLive, listed := live.loose[rawOf(id)]; !listed || isLive {
				continue
			}
			ok, err := olderThan(filepath.Join(dir, e.Name()), before)
			if err != nil {
				return removed, fmt.Errorf("sweep: %w", err)
			}
			if !ok {
				continue
			}
			if err := begin(); err != nil {
				return removed, fmt.Errorf("sweep: %w", err)
			}
			if err := removeFile(filepath.Join(dir, e.Name())); err != nil {
				return removed, fmt.Errorf("sweep: %w", err)
			}
			s.cache.remove(id)
			removed++
		}
	}
	if err := s.sweepStaging(before); err != nil {
		return removed, fmt.Errorf("sweep: %w", err)
	}
	return removed, nil
}

// listLoose lists the loose objects stored now, none of them marked live yet.
func (s *Store) listLoose() (map[rawID]bool, error) {
	loose := map[rawID]bool{}
	fanouts, err := os.ReadDir(s.objects)
	if err != nil {
		return nil, err
	}
	for _, fan := range fanouts {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		names, err := os.ReadDir(filepath.Join(s.objects, fan.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range names {
			if id := OID(fan.Name() + e.Name()); id.Valid() {
				loose[rawOf(id)] = false
			}
		}
	}
	return loose, nil
}

// mark adds id and, for trees, everything below it to live, walking tree
// bodies in place. Roots and ModeDir entries are trees; other entries are
// blobs and are not read.
func (s *Store) mark(id OID, live *liveSet) error {
	if id.Valid() && !s.packs.markLive(live, rawOf(id)) || id == EmptyTree {
		return nil
	}
	data, err := s.treeBody(id)
	if err != nil {
		return err
	}
	var below error // a subtree's error, already described
	err = walkTree(data, func(mode uint32, _ []byte, child rawID) error {
		if mode != ModeDir {
			s.packs.markLive(live, child)
			return nil
		}
		below = s.mark(child.oid(), live)
		return below
	})
	if err != nil && err != below {
		return fmt.Errorf("read tree %s: %w", id, err)
	}
	return err
}

// liveSet records the objects a Sweep keeps: a bit per record of the index
// files current when marking began, which keep their positions until the
// Sweep ends, and a map for those found elsewhere (the active pack, packs
// sealed since). Loose objects listed beforehand map to whether they are
// live. Ids stored nowhere when marked are kept as raw ids, should they be
// stored before the Sweep removes anything.
type liveSet struct {
	srcs  []*idxFile // the pinned index files, in lookup order
	offs  []int      // the position of each one's first record
	bits  []uint64
	other map[rawID]struct{}
	loose map[rawID]bool
	// absent is sorted and unique up to sorted, then appended to; compact
	// sorts it again once it has doubled.
	absent []rawID
	sorted int
}

// compact sorts absent and drops repeats.
func (l *liveSet) compact() {
	slices.SortFunc(l.absent, func(a, b rawID) int { return bytes.Compare(a[:], b[:]) })
	l.absent = slices.Clip(slices.Compact(l.absent))
	l.sorted = len(l.absent)
}

// pinnedIndex returns the position of x among the pinned index files, or -1.
func (l *liveSet) pinnedIndex(x *idxFile) int {
	for k, y := range l.srcs {
		if y == x {
			return k
		}
	}
	return -1
}

// set makes id live, reporting whether it was not yet. packed tells whether it
// is packed, at record i of pinned index file k, or with k -1 elsewhere. An
// object neither packed nor listed loose is not recorded, and always new.
func (l *liveSet) set(k, i int, id rawID, packed bool) bool {
	added := false
	if isLive, listed := l.loose[id]; listed {
		if !isLive {
			l.loose[id] = true
			added = true
		}
	} else if !packed {
		l.absent = append(l.absent, id)
		if len(l.absent) >= 2*l.sorted+1024 {
			l.compact()
		}
		return true
	}
	switch {
	case !packed:
	case k >= 0:
		b := l.offs[k] + i
		if l.bits[b/64]&(1<<(b%64)) == 0 {
			l.bits[b/64] |= 1 << (b % 64)
			added = true
		}
	default:
		if _, ok := l.other[id]; !ok {
			l.other[id] = struct{}{}
			added = true
		}
	}
	return added
}

// get reports whether id, packed at record i of pinned index file k (k -1
// when elsewhere), is live.
func (l *liveSet) get(k, i int, id rawID) bool {
	if l.loose[id] {
		return true
	}
	if k >= 0 {
		b := l.offs[k] + i
		return l.bits[b/64]&(1<<(b%64)) != 0
	}
	if _, ok := l.other[id]; ok {
		return true
	}
	_, ok := slices.BinarySearchFunc(l.absent[:l.sorted], id, func(a, b rawID) int { return bytes.Compare(a[:], b[:]) })
	return ok
}

// pin starts a Sweep's marking against the current index files, deferring
// merges until unpin so their records keep their positions; loose lists the
// loose objects.
func (p *packSet) pin(loose map[rawID]bool) *liveSet {
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	p.pinned = true
	p.mu.RLock()
	l := &liveSet{srcs: p.sources(), other: map[rawID]struct{}{}, loose: loose}
	p.mu.RUnlock()
	total := 0
	for _, x := range l.srcs {
		l.offs = append(l.offs, total)
		total += x.n
	}
	l.bits = make([]uint64, (total+63)/64)
	return l
}

// unpin ends a Sweep, merging the sealed indexes deferred meanwhile.
func (p *packSet) unpin() {
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	p.pinned = false
	if len(p.sealed) >= mergeAfter {
		// A failed merge only leaves lookups more indexes to search.
		p.merge(nil)
	}
}

// markLive makes id live while objects may still be added, reporting whether
// it was not yet.
func (p *packSet) markLive(l *liveSet, id rawID) bool {
	p.mu.RLock()
	x, i, _, ok := p.locate(id)
	p.mu.RUnlock()
	k := -1
	if x != nil {
		k = l.pinnedIndex(x)
	}
	return l.set(k, i, id, ok)
}

// rewriteShare is the inverse of the share of a pack's bytes that must be
// garbage before Sweep rewrites it: below a quarter, dead objects stay in
// place (and stored) until later sweeps have found enough of them.
const rewriteShare = 4

// sweep removes the packed objects outside live whose freshness predates
// before, but only from packs where they and frames lookups do not use make
// up at least 1/rewriteShare of the pack. Dead objects left in other packs
// stay stored, as do the dependencies (delta bases, manifest pages and
// chunks) of every object kept. begin runs before anything is removed, and
// removed with each object removed. A pack that loses objects is rewritten:
// its other frames are copied verbatim (still compressed, keeping their
// freshness) to the active pack and made durable, the indexes are merged
// without the old pack, and only then is it deleted. Reads in flight on an
// old pack finish on its open file. Nothing is added or refreshed meanwhile:
// the caller holds Store.mu and Store.ingest exclusively.
func (p *packSet) sweep(l *liveSet, before int64, begin func() error, removed func(rawID)) (int, error) {
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	p.flushMu.Lock()
	p.touchMu.Lock()
	p.flush()
	p.touchMu.Unlock()
	p.flushMu.Unlock()
	l.compact()
	p.mu.RLock()
	v := &sweepView{p: p, l: l, before: before, srcs: p.sources()}
	if len(p.entries) > 0 {
		v.srcs = append(v.srcs, memIndex(encodeIndex(p.entries, p.active.n, p.end)))
	}
	p.mu.RUnlock()
	for _, x := range v.srcs {
		v.offs = append(v.offs, v.total)
		v.total += x.n
	}
	if err := v.keepBases(); err != nil {
		return 0, err
	}
	dead, victims, err := v.removals()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, w := range dead {
		n += bits.OnesCount64(w)
	}
	if n == 0 {
		return 0, nil
	}
	if err := begin(); err != nil {
		return 0, err
	}
	v.eachDead(dead, func(k, i int) { removed(v.srcs[k].id(i)) })
	if p.active != nil && victims[p.active.n] {
		if err := p.seal(); err != nil {
			return n, err
		}
	}
	for _, num := range slices.Sorted(maps.Keys(victims)) {
		if err := v.rewrite(num, dead); err != nil {
			return n, err
		}
	}
	if p.active != nil {
		if err := p.active.f.Sync(); err != nil {
			return n, err
		}
	}
	if err := p.merge(victims); err != nil {
		return n, err
	}
	for num := range victims {
		p.mu.Lock()
		pf := p.files[num]
		delete(p.files, num)
		p.mu.Unlock()
		if err := hostfile.Remove(p.packPath(num)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return n, err
		}
		pf.release()
	}
	return n, nil
}

// sweepView is every index while a Sweep removes objects: the index files and
// the active pack's objects, encoded as one more index. Records are numbered
// across them in order.
type sweepView struct {
	p      *packSet
	l      *liveSet
	before int64
	srcs   []*idxFile
	offs   []int
	total  int
}

// find returns where the copy of id lookups use is.
func (v *sweepView) find(id rawID) (k, i int, ok bool) {
	for k, x := range v.srcs {
		if i, ok := x.find(id); ok {
			return k, i, true
		}
	}
	return -1, 0, false
}

// canonical reports whether lookups use the copy of id in index k.
func (v *sweepView) canonical(k int, id rawID) bool {
	for _, x := range v.srcs[:k] {
		if x.has(id) {
			return false
		}
	}
	return true
}

// pinned maps index k to its position among the pinned index files, which
// lead srcs: merges wait for the Sweep, and seals only append.
func (v *sweepView) pinned(k int) int {
	if k < len(v.l.srcs) {
		return k
	}
	return -1
}

// live reports whether record i of index k is live.
func (v *sweepView) live(k, i int) bool { return v.l.get(v.pinned(k), i, v.srcs[k].id(i)) }

// mark makes id live, reporting whether it was not yet and, when it is
// packed, whether it is fresh.
func (v *sweepView) mark(id rawID) (added, packed, fresh bool) {
	k, i, ok := v.find(id)
	if !ok {
		return v.l.set(-1, 0, id, false), false, false
	}
	return v.l.set(v.pinned(k), i, id, true), true, v.srcs[k].at(i) >= v.before
}

// keepBases makes live the dependencies, transitively, of every packed object
// the Sweep keeps (live, or fresh), so no kept object loses one. Afterwards
// live is closed under dependencies.
func (v *sweepView) keepBases() error {
	for k, x := range v.srcs {
		for i := range x.n {
			if x.record(i)[36]&(frameDelta|frameManifest) == 0 {
				continue
			}
			id := x.id(i)
			if !v.canonical(k, id) || x.at(i) < v.before && !v.live(k, i) {
				continue
			}
			// A dependency already live, or fresh, is followed from here anyway.
			err := v.follow(id, func(dep rawID) bool {
				added, packed, fresh := v.mark(dep)
				return added && packed && !fresh
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// follow walks the dependencies (a delta's base, a manifest's references) of
// packed object id depth first, passing each to descend, which reports
// whether to walk its own; descend must report each object at most once, so
// corrupt cycles end.
func (v *sweepView) follow(id rawID, descend func(rawID) bool) error {
	stack := []rawID{id}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		k, i, ok := v.find(id)
		if !ok {
			continue
		}
		e := v.srcs[k].entry(i)
		if e.flags&frameDelta != 0 {
			if descend(e.base) {
				stack = append(stack, e.base)
			}
			continue
		}
		if e.flags&frameManifest == 0 {
			continue
		}
		_, frame, ok, err := v.p.frame(id)
		if err == nil && (!ok || checkManifest(id, frame) != nil) {
			err = errCorrupt
		}
		if err != nil {
			return fmt.Errorf("manifest %s: %w", id.oid(), err)
		}
		refs, _, _ := parseManifest(frame[frameHeader:])
		for _, ref := range refs {
			if descend(ref.id) {
				stack = append(stack, ref.id)
			}
		}
	}
	return nil
}

// removals finds the canonical records the Sweep removes, as a bit set over
// the view, and every pack holding a copy of them. live must be closed under
// dependencies, as keepBases leaves it.
func (v *sweepView) removals() ([]uint64, map[uint32]bool, error) {
	dead := make([]uint64, (v.total+63)/64)
	indexed := map[uint32]int64{}
	for k, x := range v.srcs {
		for i := range x.n {
			if k > 0 && !v.canonical(k, x.id(i)) {
				continue
			}
			e := x.entry(i)
			indexed[e.pack] += frameHeader + int64(e.stored)
			if x.at(i) < v.before && !v.live(k, i) {
				b := v.offs[k] + i
				dead[b/64] |= 1 << (b % 64)
			}
		}
	}
	sizes := map[uint32]int64{}
	for {
		deadBytes := map[uint32]int64{}
		v.eachDead(dead, func(k, i int) {
			e := v.srcs[k].entry(i)
			deadBytes[e.pack] += frameHeader + int64(e.stored)
		})
		victims := map[uint32]bool{}
		for n, d := range deadBytes {
			size, ok := sizes[n]
			if !ok {
				if v.p.active != nil && v.p.active.n == n {
					size = v.p.end
				} else {
					info, err := v.p.files[n].f.Stat()
					if err != nil {
						return nil, nil, err
					}
					size = info.Size()
				}
				sizes[n] = size
			}
			if (size-(indexed[n]-d))*rewriteShare >= size {
				victims[n] = true
			}
		}
		// A dead object left in place keeps its dependencies, which may make
		// other packs hold less garbage: repeat until nothing changes. Its
		// pack only loses garbage from here on, so it stays in place and is
		// followed once. Live and fresh dependencies are closed already.
		changed := false
		var err error
		v.eachDead(dead, func(k, i int) {
			b := v.offs[k] + i
			if err != nil || dead[b/64]&(1<<(b%64)) == 0 || victims[v.srcs[k].entry(i).pack] {
				return
			}
			dead[b/64] &^= 1 << (b % 64)
			err = v.follow(v.srcs[k].id(i), func(dep rawID) bool {
				added, _, _ := v.mark(dep)
				dk, di, ok := v.find(dep)
				if !added || !ok {
					return false
				}
				db := v.offs[dk] + di
				if dead[db/64]&(1<<(db%64)) == 0 {
					return false
				}
				dead[db/64] &^= 1 << (db % 64)
				changed = true
				return true
			})
		})
		if err != nil {
			return nil, nil, err
		}
		if !changed {
			// An interrupted rewrite can leave shadow copies in later
			// indexes or the active pack. Once an id is removed, every
			// pack holding it must be rewritten, even below the threshold,
			// or merging without its canonical pack resurrects a shadow.
			// rewrite copies only surviving canonical records, so these
			// extra victims preserve live objects without copying shadows.
			v.eachDead(dead, func(k, i int) {
				id := v.srcs[k].id(i)
				for _, x := range v.srcs[k+1:] {
					if j, ok := x.find(id); ok {
						victims[x.entry(j).pack] = true
					}
				}
			})
			return dead, victims, nil
		}
	}
}

// eachDead calls fn with the index and record of every bit set in dead.
func (v *sweepView) eachDead(dead []uint64, fn func(k, i int)) {
	k := 0
	for w, word := range dead {
		for word != 0 {
			b := w*64 + bits.TrailingZeros64(word)
			word &= word - 1
			for k+1 < len(v.offs) && v.offs[k+1] <= b {
				k++
			}
			fn(k, b-v.offs[k])
		}
	}
}

// rewrite copies the frames of sealed pack n that lookups use and the Sweep
// keeps to the active pack, in their order, with their freshness;
// p.appendMu is held.
func (v *sweepView) rewrite(n uint32, dead []uint64) error {
	type moved struct {
		id rawID
		e  packed
	}
	var frames []moved
	for k, x := range v.srcs {
		if !x.covers(n) {
			continue
		}
		for i := range x.n {
			b := v.offs[k] + i
			if binary.LittleEndian.Uint32(x.record(i)[20:]) != n || dead[b/64]&(1<<(b%64)) != 0 {
				continue
			}
			if id := x.id(i); v.canonical(k, id) {
				e := x.entry(i)
				e.at = x.at(i)
				frames = append(frames, moved{id, e})
			}
		}
	}
	slices.SortFunc(frames, func(a, b moved) int { return cmp.Compare(a.e.off, b.e.off) })
	p := v.p
	p.mu.RLock()
	pf := p.files[n]
	p.mu.RUnlock()
	buf := make([]byte, 0, frameHeader+cloneFile)
	for _, m := range frames {
		buf = buf[:frameHeader+int(m.e.stored)]
		if _, err := pf.f.ReadAt(buf, int64(m.e.off)); err != nil {
			return fmt.Errorf("rewrite pack %d: %w", n, err)
		}
		num, off, err := p.write(buf)
		if err != nil {
			return fmt.Errorf("rewrite pack %d: %w", n, err)
		}
		m.e.pack, m.e.off = num, off
		p.mu.Lock()
		p.entries[m.id] = m.e
		p.mu.Unlock()
	}
	return nil
}

// sweepStaging removes stale staging files of the staging directory.
func (s *Store) sweepStaging(before time.Time) error {
	names, err := os.ReadDir(s.tmp)
	if err != nil {
		return err
	}
	for _, e := range names {
		if err := s.sweepStage(filepath.Join(s.tmp, e.Name()), before); err != nil {
			return err
		}
	}
	return nil
}

// sweepStage removes staging file p if it is older than before and not owned by an in-flight put.
func (s *Store) sweepStage(p string, before time.Time) error {
	if _, busy := s.staging.Load(p); busy {
		return nil
	}
	ok, err := olderThan(p, before)
	if err != nil || !ok {
		return err
	}
	return removeFile(p)
}

// olderThan reports whether p was last modified before t; a vanished file is not old.
func olderThan(p string, t time.Time) (bool, error) {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.ModTime().Before(t), nil
}
