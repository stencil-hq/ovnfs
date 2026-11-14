package store

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// reopen opens the store s lives in again, as a restarted process would, with the cache off.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	again, err := Open(filepath.Dir(s.objects), 0)
	if err != nil {
		t.Fatal(err)
	}
	return again
}

// packFiles lists the pack and index files of s by name.
func packFiles(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	names, err := os.ReadDir(s.packs.dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]int64{}
	for _, e := range names {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = info.Size()
	}
	return files
}

// noisy returns prefix followed by n random (incompressible) bytes.
func noisy(prefix string, n int) []byte {
	b := make([]byte, len(prefix)+n)
	copy(b, prefix)
	rand.Read(b[len(prefix):])
	return b
}

// mustRead checks id reads back as want through ReadBlob, Open and Size.
func mustRead(t *testing.T, s *Store, id OID, want []byte) {
	t.Helper()
	got, err := s.ReadBlob(id)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadBlob(%s) = %d bytes, %v; want %d bytes", id, len(got), err, len(want))
	}
	r, err := s.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err = io.ReadAll(r)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Open(%s) read %d bytes, %v; want %d bytes", id, len(got), err, len(want))
	}
	if size, err := s.Size(id); err != nil || size != int64(len(want)) {
		t.Fatalf("Size(%s) = %d, %v; want %d", id, size, err, len(want))
	}
}

// TestPackCompresses checks compressible objects take less room than their
// content and incompressible ones are stored raw, both reading back intact.
func TestPackCompresses(t *testing.T) {
	s := openStoreCache(t, 0)
	text := bytes.Repeat([]byte("all work and no play makes jack a dull boy\n"), 2000)
	noise := make([]byte, 64<<10)
	rand.Read(noise)
	textID, err := s.PutBytes(text)
	if err != nil {
		t.Fatal(err)
	}
	noiseID, err := s.PutBytes(noise)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := s.packs.lookup(rawOf(textID)); e.flags != frameZstd || e.stored >= uint32(len(text))/10 {
		t.Errorf("text stored as flags %d, %d bytes of %d", e.flags, e.stored, len(text))
	}
	if e, _ := s.packs.lookup(rawOf(noiseID)); e.flags != 0 || e.stored != uint32(len(noise)) {
		t.Errorf("noise stored as flags %d, %d bytes of %d", e.flags, e.stored, len(noise))
	}
	for _, s := range []*Store{s, reopen(t, s)} {
		mustRead(t, s, textID, text)
		mustRead(t, s, noiseID, noise)
		if r, _ := s.Open(textID); r != nil {
			if _, isFile := r.(*os.File); isFile {
				t.Error("Open returned a file for a packed object")
			}
			r.Close()
		}
	}
	empty, err := s.PutBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	mustRead(t, s, empty, nil)
}

// TestPackTornTail drops an incomplete or garbled tail of the active pack at
// Open, keeping every complete frame before it, and appends after the cut.
func TestPackTornTail(t *testing.T) {
	s := openStoreCache(t, 0)
	var ids []OID
	var bodies [][]byte
	for i := range 5 {
		body := fmt.Appendf(nil, "object %d %s", i, bytes.Repeat([]byte("x"), i*100))
		id, err := s.PutBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		ids, bodies = append(ids, id), append(bodies, body)
	}
	last, _ := s.packs.lookup(rawOf(ids[4]))
	pack := s.packs.packPath(last.pack)
	full, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	for name, tail := range map[string][]byte{
		"cut in payload": full[:len(full)-3],
		"cut in header":  full[:int(last.off)+10],
		"garbage after":  append(full[:last.off:last.off], bytes.Repeat([]byte{0xa5}, 200)...),
		"zeroed tail":    append(full[:last.off:last.off], make([]byte, 4096)...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(pack, tail, 0o644); err != nil {
				t.Fatal(err)
			}
			again := reopen(t, s)
			for i := range 4 {
				mustRead(t, again, ids[i], bodies[i])
			}
			if again.Has(ids[4]) {
				t.Fatal("torn object reported stored")
			}
			if size := packFiles(t, again)[filepath.Base(pack)]; size != int64(last.off) {
				t.Fatalf("pack is %d bytes after recovery, want %d", size, last.off)
			}
			if _, err := again.PutBytes(bodies[4]); err != nil {
				t.Fatal(err)
			}
			mustRead(t, reopen(t, again), ids[4], bodies[4])
		})
	}
}

// TestPackConcurrentWriters stores overlapping objects from many goroutines
// across several packs; every object reads back once stored, is packed once,
// and survives a reopen that loads sealed packs from their indexes.
func TestPackConcurrentWriters(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 64 << 10
	const objects = 400
	body := func(i int) []byte {
		b := fmt.Appendf(nil, "blob %d\n", i)
		noise := make([]byte, 200+i%7*100)
		rand.Read(noise)
		return append(b, noise...)
	}
	bodies := make([][]byte, objects)
	for i := range bodies {
		bodies[i] = body(i)
	}
	ids := make([]OID, objects)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for k := range objects {
				i := (k + g*37) % objects
				id, err := s.PutBytes(bodies[i])
				if err != nil {
					t.Error(err)
					return
				}
				if g == 0 {
					ids[i] = id
				}
				if got, err := s.ReadBlob(id); err != nil || !bytes.Equal(got, bodies[i]) {
					t.Errorf("read back %d: %v", i, err)
					return
				}
				if k%50 == 0 {
					if _, err := s.PutTree([]Entry{{Name: fmt.Sprint(i), Mode: ModeFile, ID: id}}); err != nil {
						t.Error(err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	var frames int
	for name, size := range packFiles(t, s) {
		if filepath.Ext(name) == ".pack" {
			n, err := countFrames(filepath.Join(s.packs.dir, name), size)
			if err != nil {
				t.Fatal(err)
			}
			frames += n
		}
	}
	if want := len(packedObjects(s)); frames != want {
		t.Errorf("packs hold %d frames for %d objects", frames, want)
	}
	files := packFiles(t, s)
	if files["0.idx"] == 0 || len(files) < 4 {
		t.Fatalf("expected several packs with sealed indexes, got %v", files)
	}
	again := reopen(t, s)
	if len(packedObjects(again)) != len(packedObjects(s)) {
		t.Fatalf("reopened index holds %d objects, want %d", len(packedObjects(again)), len(packedObjects(s)))
	}
	for i, id := range ids {
		mustRead(t, again, id, bodies[i])
	}
}

// countFrames counts the intact frames of the pack file at p of length size.
func countFrames(p string, size int64) (int, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	n := 0
	for off := 0; off < len(data); n++ {
		_, e, ok := parseHeader(data[off:])
		if !ok {
			return 0, fmt.Errorf("bad frame at %d", off)
		}
		off += frameHeader + int(e.stored)
	}
	return n, nil
}

// TestSweepPacked removes old unreachable packed objects by rewriting their
// packs, keeping reachable and recently stored ones readable, now and after a reopen.
func TestSweepPacked(t *testing.T) {
	s := openStore(t)
	s.packs.limit = 4 << 10
	put := func(c string) OID {
		id, err := s.PutBytes(noisy(c, 1000))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	files := map[string]File{}
	var garbage, fresh []OID
	for i := range 40 {
		if i%2 == 0 {
			files[fmt.Sprint("f", i)] = File{Mode: ModeFile, ID: put(fmt.Sprint("live ", i))}
		} else {
			garbage = append(garbage, put(fmt.Sprint("garbage ", i)))
		}
	}
	root, err := s.Build(files)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for id := range packedObjects(s) {
		age(t, s, id.oid(), old)
	}
	cutoff := time.Now().Add(-time.Minute)
	for i := range 3 {
		fresh = append(fresh, put(fmt.Sprint("fresh ", i)))
	}
	victims := map[string]bool{}
	for _, id := range garbage {
		e, _ := s.packs.lookup(rawOf(id))
		victims[filepath.Base(s.packs.packPath(e.pack))] = true
	}
	epoch := s.Epoch()
	n, err := s.Sweep([]OID{root}, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(garbage) {
		t.Fatalf("Sweep removed %d objects, want %d", n, len(garbage))
	}
	if s.Epoch() == epoch {
		t.Error("removing packed objects kept the epoch")
	}
	after := packFiles(t, s)
	for name := range victims {
		if _, ok := after[name]; ok {
			t.Errorf("pack %s holding garbage survived the sweep", name)
		}
	}
	for _, st := range []*Store{s, reopen(t, s)} {
		for p, f := range files {
			if !st.Has(f.ID) {
				t.Fatalf("live %s lost", p)
			}
			if _, err := st.ReadBlob(f.ID); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range fresh {
			if _, err := st.ReadBlob(id); err != nil {
				t.Fatalf("fresh object lost: %v", err)
			}
		}
		for _, id := range garbage {
			if st.Has(id) {
				t.Fatalf("garbage %s kept", id)
			}
		}
		if _, err := st.Flatten(root); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	for name, size := range packFiles(t, s) {
		if filepath.Ext(name) == ".pack" {
			total += size
		}
	}
	var live int64
	for _, e := range packedObjects(s) {
		live += frameHeader + int64(e.stored)
	}
	if total != live {
		t.Errorf("packs hold %d bytes for %d bytes of live frames", total, live)
	}
}

// TestSweepPackedReadDuringRewrite keeps reads of live objects working while
// Sweep rewrites the packs that hold them.
func TestSweepPackedReadDuringRewrite(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 4 << 10
	var live []OID
	var bodies [][]byte
	for i := range 200 {
		body := noisy(fmt.Sprint("object ", i), 300)
		id, err := s.PutBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			age(t, s, id, time.Now().Add(-time.Hour))
			continue
		}
		live, bodies = append(live, id), append(bodies, body)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				k := i % len(live)
				if got, err := s.ReadBlob(live[k]); err != nil || !bytes.Equal(got, bodies[k]) {
					t.Errorf("read during sweep: %v", err)
					return
				}
			}
		})
	}
	if _, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil {
		t.Error(err)
	}
	close(stop)
	wg.Wait()
}

// TestLegacyLooseObjects reads, clones, caches and sweeps loose object files
// as older builds wrote them, of any size.
func TestLegacyLooseObjects(t *testing.T) {
	s := openStore(t)
	small := []byte("written by an older build")
	big := bytes.Repeat([]byte("L"), cloneFile/2)
	var ids []OID
	for _, body := range [][]byte{small, big} {
		id := HashBytes("blob", body)
		if err := writeExclusive(s.objectPath(id), body); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i, body := range [][]byte{small, big} {
		if !s.Has(ids[i]) {
			t.Fatal("legacy object not found")
		}
		mustRead(t, s, ids[i], body)
		r, err := s.Open(ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, isFile := r.(*os.File); !isFile {
			t.Error("Open of a loose object is not its file")
		}
		r.Close()
		dst := filepath.Join(t.TempDir(), "out")
		if err := s.CloneBlob(ids[i], dst, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(dst); !bytes.Equal(got, body) {
			t.Fatal("clone of a legacy object differs")
		}
		if got, ok := s.Cached(ids[i]); !ok || !bytes.Equal(got, body) {
			t.Fatal("legacy object not cached")
		}
	}
	// Storing the same content again refreshes the loose copy rather than packing a second one.
	if _, err := s.PutBytes(small); err != nil {
		t.Fatal(err)
	}
	if len(packedObjects(s)) != 0 {
		t.Error("re-storing a loose object packed it")
	}
	for _, id := range ids {
		age(t, s, id, time.Now().Add(-time.Hour))
	}
	if n, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil || n != 2 {
		t.Fatalf("Sweep removed %d legacy objects, %v; want 2", n, err)
	}
	if s.Has(ids[0]) || s.Has(ids[1]) {
		t.Error("swept legacy objects still stored")
	}
}

// TestCorruptFrameDetected refuses to return content from a damaged frame,
// in a sealed pack read through its index and in the active pack.
func TestCorruptFrameDetected(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 4 << 10
	var ids []OID
	for i := range 12 {
		id, err := s.PutBytes(noisy(fmt.Sprint(i), 900))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	sealed, _ := s.packs.lookup(rawOf(ids[0]))
	active, _ := s.packs.lookup(rawOf(ids[len(ids)-1]))
	if sealed.pack == active.pack {
		t.Fatal("objects did not span packs")
	}
	for _, e := range []packed{sealed, active} {
		f, err := os.OpenFile(s.packs.packPath(e.pack), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		at := int64(e.off) + frameHeader + int64(e.stored)/2
		var b [1]byte
		f.ReadAt(b[:], at)
		b[0] ^= 0xff
		f.WriteAt(b[:], at)
		f.Close()
	}
	for _, id := range []OID{ids[0], ids[len(ids)-1]} {
		if _, err := s.ReadBlob(id); !errors.Is(err, errCorrupt) {
			t.Errorf("ReadBlob of a corrupt frame: %v", err)
		}
		if _, err := s.Open(id); !errors.Is(err, errCorrupt) {
			t.Errorf("Open of a corrupt frame: %v", err)
		}
		if _, ok := s.Cached(id); ok {
			t.Error("Cached served a corrupt frame")
		}
		if err := s.CloneBlob(id, filepath.Join(t.TempDir(), "x"), 0o644); !errors.Is(err, errCorrupt) {
			t.Errorf("CloneBlob of a corrupt frame: %v", err)
		}
	}
	// A reopen scans the active pack and cuts it at the damaged frame; the
	// sealed pack's index still lists its damaged object, which stays unreadable.
	again := reopen(t, s)
	if again.Has(ids[len(ids)-1]) {
		t.Error("damaged frame of the active pack survived the scan")
	}
	if _, err := again.ReadBlob(ids[0]); !errors.Is(err, errCorrupt) {
		t.Errorf("ReadBlob of a corrupt sealed frame after reopen: %v", err)
	}
	if _, err := again.ReadBlob(ids[1]); err != nil {
		t.Errorf("intact neighbour unreadable: %v", err)
	}
}

// TestOpenIndexesManyObjects keeps Open fast for a store of 100k packed
// objects, whose sealed packs load from their indexes.
func TestOpenIndexesManyObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("stores 100k objects")
	}
	s := openStoreCache(t, 0)
	s.packs.limit = 1 << 20
	for i := range 100_000 {
		if _, err := s.PutBytes(fmt.Appendf(nil, "object number %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	again := reopen(t, s)
	took := time.Since(start)
	if len(packedObjects(again)) != 100_000 {
		t.Fatalf("reopened index holds %d objects", len(packedObjects(again)))
	}
	t.Logf("Open indexed 100k objects in %v", took)
	if took > 2*time.Second {
		t.Errorf("Open took %v", took)
	}
}

// nextPack seals the active pack, so later objects go to a new one.
func nextPack(t *testing.T, s *Store) {
	t.Helper()
	s.packs.appendMu.Lock()
	err := s.packs.seal()
	s.packs.appendMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// TestSweepPackThreshold leaves a little garbage in its pack, still stored and
// without a new epoch, rewrites the pack once garbage reaches a quarter of
// it, and deletes a pack holding nothing live without copying.
func TestSweepPackThreshold(t *testing.T) {
	s := openStoreCache(t, 0)
	old := time.Now().Add(-time.Hour)
	put := func(b []byte) OID {
		id, err := s.PutBytes(b)
		if err != nil {
			t.Fatal(err)
		}
		age(t, s, id, old)
		return id
	}
	tree := func(ids ...OID) OID {
		var entries []Entry
		for i, id := range ids {
			entries = append(entries, Entry{Name: fmt.Sprint(i), Mode: ModeFile, ID: id})
		}
		id, err := s.PutTree(entries)
		if err != nil {
			t.Fatal(err)
		}
		age(t, s, id, old)
		return id
	}
	live, dropped := noisy("live", 3000), noisy("dropped later", 3000)
	liveID, droppedID, dead := put(live), put(dropped), put(noisy("dead", 500))
	root := tree(liveID, droppedID)
	before, epoch := packFiles(t, s), s.Epoch()
	if n, err := s.Sweep([]OID{root}, time.Now().Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("Sweep removed %d objects (%v) for garbage below the threshold", n, err)
	}
	if !s.Has(dead) || s.Epoch() != epoch {
		t.Fatal("garbage below the threshold removed, or the epoch changed")
	}
	if _, err := s.ReadBlob(dead); err != nil {
		t.Fatal(err)
	}
	if after := packFiles(t, s); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("packs changed from %v to %v", before, after)
	}

	smaller := tree(liveID)
	if n, err := s.Sweep([]OID{smaller}, time.Now().Add(-time.Minute)); err != nil || n != 3 {
		t.Fatalf("Sweep past the threshold removed %d objects, %v; want 3", n, err)
	}
	if s.Has(dead) || s.Has(droppedID) || s.Has(root) || s.Epoch() == epoch {
		t.Fatal("garbage past the threshold kept, or the epoch kept")
	}
	for name := range before {
		if _, ok := packFiles(t, s)[name]; ok && filepath.Ext(name) == ".pack" {
			t.Errorf("rewritten pack %s kept", name)
		}
	}
	mustRead(t, s, liveID, live)
	if _, err := s.Flatten(smaller); err != nil {
		t.Fatal(err)
	}

	if n, err := s.Sweep(nil, time.Now().Add(time.Minute)); err != nil || n != 2 {
		t.Fatalf("Sweep of everything removed %d objects, %v; want 2", n, err)
	}
	for name := range packFiles(t, s) {
		t.Errorf("%s left after its pack held only garbage", name)
	}
}

// TestSweepKeepsBaseOfStayingDelta keeps the base of an unreachable delta
// left in a pack below the threshold, even while rewriting the base's pack.
func TestSweepKeepsBaseOfStayingDelta(t *testing.T) {
	s := openStoreCache(t, 0)
	baseData := prose(5, 100<<10)
	base := putLikeFile(t, s, baseData, "")
	garbage, err := s.PutBytes(noisy("garbage", 400<<10))
	if err != nil {
		t.Fatal(err)
	}
	nextPack(t, s)
	next := edit(baseData, 1)
	delta := putLikeFile(t, s, next, base)
	if s.packs.depth(rawOf(delta)) != 1 {
		t.Fatal("edit not stored as a delta")
	}
	keep, err := s.PutBytes(noisy("keep", 100<<10))
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.PutTree([]Entry{{Name: "k", Mode: ModeFile, ID: keep}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []OID{base, garbage, delta, keep, root} {
		age(t, s, id, time.Now().Add(-time.Hour))
	}
	if n, err := s.Sweep([]OID{root}, time.Now().Add(-time.Minute)); err != nil || n != 1 {
		t.Fatalf("Sweep removed %d objects, %v; want 1 (the garbage beside the base)", n, err)
	}
	if s.Has(garbage) {
		t.Fatal("garbage kept")
	}
	mustRead(t, s, delta, next)
	mustRead(t, reopen(t, s), delta, next)
}

// TestOpenMigratesUnsortedIndex replaces the unsorted index an earlier release
// wrote for a sealed pack with a sorted one at Open, keeping every object and
// its freshness, and scans a pack whose old index is damaged.
func TestOpenMigratesUnsortedIndex(t *testing.T) {
	s := openStoreCache(t, 0)
	bodies := map[OID][]byte{}
	for i := range 50 {
		body := noisy(fmt.Sprint("object ", i), 300)
		id, err := s.PutBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		bodies[id] = body
	}
	old := time.Now().Add(-time.Hour).UnixNano()
	s.packs.mu.Lock()
	for id, e := range s.packs.entries {
		e.at = old
		s.packs.entries[id] = e
	}
	s.packs.mu.Unlock()
	nextPack(t, s)
	idx := s.packs.idxPath(0)
	pack, err := os.Stat(s.packs.packPath(0))
	if err != nil {
		t.Fatal(err)
	}
	// The earlier format: records in no particular order, then a trailer.
	var body []byte
	for id, e := range packedObjects(s) {
		var rec [legacyRecord]byte
		copy(rec[:], id[:])
		binary.LittleEndian.PutUint32(rec[20:], e.off)
		binary.LittleEndian.PutUint32(rec[24:], e.stored)
		binary.LittleEndian.PutUint32(rec[28:], e.raw)
		rec[32] = e.flags
		binary.LittleEndian.PutUint64(rec[36:], uint64(e.at))
		body = append(body, rec[:]...)
	}
	trailer := binary.LittleEndian.AppendUint64(nil, uint64(pack.Size()))
	trailer = binary.LittleEndian.AppendUint32(trailer, uint32(len(body)/legacyRecord))
	trailer = binary.LittleEndian.AppendUint32(trailer, crc32.Checksum(body, castagnoli))
	legacy := append(body, trailer...)
	for _, damaged := range []bool{false, true} {
		data := slices.Clone(legacy)
		if damaged {
			data[3] ^= 1
		}
		if err := os.WriteFile(idx, data, 0o644); err != nil {
			t.Fatal(err)
		}
		again := reopen(t, s)
		for id, want := range bodies {
			mustRead(t, again, id, want)
		}
		for _, e := range packedObjects(again) {
			// A scan finds the freshness each frame was appended with.
			if !damaged && e.at != old {
				t.Fatalf("freshness %d after migration, want %d", e.at, old)
			}
		}
		if got, err := os.ReadFile(idx); !damaged && (err != nil || !bytes.HasPrefix(got, []byte(indexMagic))) {
			t.Fatalf("index not rewritten sorted: %v", err)
		}
	}
}

// TestPackIndexMerge merges sealed pack indexes into one, keeps freshness
// written to an index across a reopen, and recovers from an interrupted merge.
func TestPackIndexMerge(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 4 << 10
	bodies := map[OID][]byte{}
	for i := range 200 {
		body := noisy(fmt.Sprint("object ", i), 500)
		id, err := s.PutBytes(body)
		if err != nil {
			t.Fatal(err)
		}
		bodies[id] = body
	}
	files := packFiles(t, s)
	idxs := 0
	var merged string
	for name := range files {
		switch filepath.Ext(name) {
		case ".idx":
			idxs++
		case ".midx":
			merged = name
		}
	}
	if merged == "" || idxs >= mergeAfter {
		t.Fatalf("sealed indexes not merged: %v", files)
	}
	var first OID
	for id := range bodies {
		first = id
		break
	}
	old := time.Now().Add(-time.Hour)
	age(t, s, first, old)
	again := reopen(t, s)
	for id, want := range bodies {
		mustRead(t, again, id, want)
	}
	if e := packedObjects(again)[rawOf(first)]; e.at != old.UnixNano() {
		t.Fatalf("freshness %d after reopen, want %d", e.at, old.UnixNano())
	}
	// A newer, torn merged index is a leftover of a crash during a merge.
	data, err := os.ReadFile(filepath.Join(s.packs.dir, merged))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.packs.dir, "99.midx"), data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	again = reopen(t, s)
	if _, ok := packFiles(t, again)["99.midx"]; ok {
		t.Fatal("torn merged index kept")
	}
	for id, want := range bodies {
		mustRead(t, again, id, want)
	}
}
