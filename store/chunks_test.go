package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func randomBody(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed+1))
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(r.Uint64())
	}
	return data
}

func packedBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	s.packs.mu.RLock()
	defer s.packs.mu.RUnlock()
	for _, pf := range s.packs.files {
		info, err := pf.f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		n += info.Size()
	}
	return n
}

func copyOnly(t *testing.T, s *Store, path string) {
	t.Helper()
	in, _, dev, err := openRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	in.Close()
	s.noReflink.Store(dev, struct{}{})
}

// Small random overwrites, insertions and appends resynchronize rather than
// storing the remainder of a multi-MiB blob, including after pack rollover.
func TestChunkedFileHistory(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 128 << 10
	path := filepath.Join(t.TempDir(), "file")
	data := randomBody(23, 5<<20)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	copyOnly(t, s, path)
	like := putLikeFile(t, s, data, "")
	versions := map[OID][]byte{like: bytes.Clone(data)}
	for i := range deltaDepth + 4 {
		before := packedBytes(t, s)
		patch := randomBody(uint64(i+100), 100)
		switch i % 3 {
		case 0:
			data = bytes.Clone(data)
			copy(data[len(data)/2:], patch)
		case 1:
			at := len(data) / 3
			data = append(append(append([]byte(nil), data[:at]...), patch...), data[at:]...)
		case 2:
			data = append(bytes.Clone(data), patch...)
		}
		id := putLikeFile(t, s, data, like)
		if growth := packedBytes(t, s) - before; growth > 256<<10 {
			t.Fatalf("edit %d added %d bytes for a %d-byte edit", i, growth, len(patch))
		}
		if e, ok := s.packs.lookup(rawOf(id)); !ok || e.flags != frameManifest {
			t.Fatal("copy-only large blob is not a chunk manifest")
		}
		versions[id] = bytes.Clone(data)
		like = id
	}
	for _, st := range []*Store{s, reopen(t, s)} {
		for id, want := range versions {
			mustRead(t, st, id, want)
		}
		r, err := st.Open(like)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 70<<10)
		if n, err := r.ReadAt(buf, 8177); err != nil || n != len(buf) || !bytes.Equal(buf, data[8177:8177+len(buf)]) {
			t.Fatalf("ReadAt = %d, %v", n, err)
		}
		if _, err := r.Seek(-100, io.SeekEnd); err != nil {
			t.Fatal(err)
		}
		tail, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(tail, data[len(data)-100:]) {
			t.Fatal("seek/read differs", err)
		}
		dst := filepath.Join(t.TempDir(), "materialized")
		if err := st.CloneBlob(like, dst, 0o751); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("materialized bytes differ", err)
		}
	}
	stages, err := os.ReadDir(s.tmp)
	if err != nil || len(stages) != 0 {
		t.Fatal("ingestion left staging files", err)
	}
}

// A chunked parent keeps the same representation even when the next file
// lives on a reflink-capable device (for example after a peer transfer).
func TestChunkedParentKeepsCDC(t *testing.T) {
	s := openStoreCache(t, 0)
	data := randomBody(27, 5<<20)
	base, err := s.PutBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	before := packedBytes(t, s)
	data[len(data)/2] ^= 0xff
	next := putLikeFile(t, s, data, base)
	e, ok := s.packs.lookup(rawOf(next))
	if !ok || e.flags != frameManifest || packedBytes(t, s)-before > 128<<10 {
		t.Fatal("chunked parent lost incremental storage")
	}
	mustRead(t, reopen(t, s), next, data)
}

// Large peer streams and in-memory blobs use CDC even on reflink-capable
// filesystems; the receiving writer verifies the whole Git blob id first.
func TestChunkedBytesAndWriter(t *testing.T) {
	data := randomBody(7, 2<<20)
	id := HashBytes("blob", data)
	for _, streamed := range []bool{false, true} {
		s := openStoreCache(t, 0)
		if streamed {
			w, err := s.NewBlobWriter(int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off < len(data); {
				n := min(7777, len(data)-off)
				if _, err := w.Write(data[off : off+n]); err != nil {
					w.Abort()
					t.Fatal(err)
				}
				off += n
			}
			if err := w.Commit(id); err != nil {
				t.Fatal(err)
			}
		} else if got, err := s.PutBytes(data); err != nil || got != id {
			t.Fatalf("PutBytes = %s, %v", got, err)
		}
		if e, ok := s.packs.lookup(rawOf(id)); !ok || e.flags != frameManifest {
			t.Fatal("large body not chunked")
		}
		cached, err := Open(filepath.Dir(s.objects), 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cached.Cached(id); ok {
			t.Fatal("large manifest admitted to small-blob cache")
		}
		mustRead(t, reopen(t, s), id, data)
		if _, err := s.ReadTree(id); err == nil {
			t.Fatal("chunk manifest accepted as a tree")
		}
	}
}

// uint64 manifest lengths preserve seeks beyond 4 GiB without allocating or
// materializing the complete logical blob. The Git id is hashed by streaming.
func TestChunkedLargeLogicalSize(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 4 GiB through SHA-1")
	}
	s := openStoreCache(t, 0)
	block := randomBody(39, chunkMax)
	leaf := rawOf(HashBytes("chunk", block))
	frame := encodeFrame(leaf, block, time.Now().UnixNano(), frameChunk)
	if _, err := s.packs.add(frame); err != nil {
		t.Fatal(err)
	}
	pageRefs := make([]chunkRef, manifestFanout)
	for i := range pageRefs {
		pageRefs[i] = chunkRef{leaf, int64(len(block))}
	}
	pageData := manifestData(pageRefs)
	page := rawOf(HashBytes("manifest", pageData))
	if _, err := s.packs.add(manifestFrame(page, pageData, frameInternal)); err != nil {
		t.Fatal(err)
	}
	middleRefs := make([]chunkRef, manifestFanout)
	for i := range middleRefs {
		middleRefs[i] = chunkRef{page, int64(len(block) * manifestFanout)}
	}
	middleData := manifestData(middleRefs)
	middle := rawOf(HashBytes("manifest", middleData))
	if _, err := s.packs.add(manifestFrame(middle, middleData, frameInternal)); err != nil {
		t.Fatal(err)
	}
	rootRefs := make([]chunkRef, 8, 9)
	for i := range rootRefs {
		rootRefs[i] = chunkRef{middle, int64(len(block) * manifestFanout * manifestFanout)}
	}
	rootRefs = append(rootRefs, chunkRef{leaf, int64(len(block))})
	size := int64(1<<32) + int64(len(block))
	h := newHash("blob", size)
	for range int64(1<<32)/int64(len(block)) + 1 {
		h.Write(block)
	}
	id := sumOID(h)
	if _, err := s.packs.add(manifestFrame(rawOf(id), manifestData(rootRefs), 0)); err != nil {
		t.Fatal(err)
	}
	for _, st := range []*Store{s, reopen(t, s)} {
		if got, err := st.Size(id); err != nil || got != size {
			t.Fatalf("large Size = %d, %v", got, err)
		}
		r, err := st.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 100)
		if n, err := r.ReadAt(buf, 1<<32+7); err != nil || n != len(buf) || !bytes.Equal(buf, block[7:107]) {
			t.Fatalf("large ReadAt = %d, %v", n, err)
		}
		if _, err := r.Seek(-100, io.SeekEnd); err != nil {
			t.Fatal(err)
		}
		if n, err := r.Read(buf); err != nil || n != len(buf) || !bytes.Equal(buf, block[len(block)-100:]) {
			t.Fatalf("large tail Read = %d, %v", n, err)
		}
		r.Close()
	}
}

// Real reflink support still produces a raw loose file, not copied chunks.
func TestLargeReflinkRemainsLoose(t *testing.T) {
	s := openStoreCache(t, 0)
	data := randomBody(41, 2<<20)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	id, _, err := s.PutFile(path)
	if err != nil {
		t.Fatal(err)
	}
	in, _, dev, err := openRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	in.Close()
	if _, ok := s.reflink.Load(dev); !ok {
		t.Skip("filesystem does not support reflinks")
	}
	r, err := s.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, ok := r.(*os.File); !ok {
		t.Fatal("reflinkable blob lost loose fast path")
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("loose clone differs", err)
	}
}

// A pre-CDC loose parent remains readable while its copy-only successor
// switches representation without dictionary-compressing the entire file.
func TestChunkedLegacyParent(t *testing.T) {
	s := openStoreCache(t, 0)
	data := randomBody(9, 5<<20)
	base := HashBytes("blob", data)
	if err := writeExclusive(s.objectPath(base), data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	copyOnly(t, s, path)
	data[len(data)/2] ^= 0xff
	id := putLikeFile(t, s, data, base)
	if e, ok := s.packs.lookup(rawOf(id)); !ok || e.flags != frameManifest {
		t.Fatal("legacy successor not chunked")
	}
	mustRead(t, reopen(t, s), id, data)
}

// A live manifest keeps every private page and chunk; dead manifests left
// below the rewrite threshold retain their dependencies too. Open readers
// pin packs, so a subsequent sweep may remove everything without breaking them.
func TestChunkedSweepAndOpenReader(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 128 << 10
	// More than manifestFanout leaves exercises transitive private pages.
	data := randomBody(11, 4<<20)
	id, err := s.PutBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, e := range packedObjects(s) {
		if e.flags == frameManifest|frameInternal {
			pages++
		}
	}
	if pages == 0 {
		t.Fatal("large graph did not exercise private manifest pages")
	}
	root, err := s.PutTree([]Entry{{Name: "large", Mode: ModeFile, ID: id}})
	if err != nil {
		t.Fatal(err)
	}
	deadData := randomBody(12, 4<<20)
	dead, err := s.PutBytes(deadData)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for raw := range packedObjects(s) {
		age(t, s, raw.oid(), old)
	}
	if n, err := s.Sweep([]OID{root}, time.Now().Add(-time.Minute)); err != nil || n == 0 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if s.Has(dead) {
		// A small manifest may share a mostly-live final pack. If retained,
		// all its chunks must still be readable until the next rewrite.
		mustRead(t, s, dead, deadData)
	}
	mustRead(t, reopen(t, s), id, data)
	r, err := s.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil || n == 0 {
		t.Fatalf("final Sweep = %d, %v", n, err)
	}
	if len(packedObjects(s)) != 0 {
		t.Fatalf("unreachable graph retained %d objects", len(packedObjects(s)))
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("open reader lost its pack after sweep", err)
	}
}

func TestChunkedConcurrentPublicationAndSweep(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 128 << 10
	data := randomBody(13, 2<<20)
	cutoff := time.Now().Add(-time.Minute)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 3 {
				id, err := s.PutBytes(data)
				if err != nil {
					t.Error(err)
					return
				}
				mustRead(t, s, id, data)
			}
		})
	}
	wg.Go(func() {
		for range 5 {
			if _, err := s.Sweep(nil, cutoff); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Wait()
}

// A Sweep waiting for a chunked ingest to end lets reads and publications
// proceed meanwhile, and keeps the chunks the ingest stored before its manifest.
func TestChunkedIngestDuringSweep(t *testing.T) {
	s := openStoreCache(t, 1<<20)
	small, err := s.PutBytes([]byte("small and resident"))
	if err != nil {
		t.Fatal(err)
	}
	data := randomBody(41, 1<<20)
	id := HashBytes("blob", data)
	root, err := s.PutTree([]Entry{{Name: "large", Mode: ModeFile, ID: id}, {Name: "small", Mode: ModeFile, ID: small}})
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	ingested := make(chan error, 1)
	go func() { ingested <- s.putChunks(id, pr) }()
	if _, err := pw.Write(data[:len(data)/2]); err != nil {
		t.Fatal(err)
	}
	swept := make(chan error, 1)
	go func() {
		_, err := s.Sweep([]OID{root}, time.Now().Add(time.Minute))
		swept <- err
	}()
	// Wait until the Sweep has marked and waits for the ingest.
	for s.ingest.TryRLock() {
		s.ingest.RUnlock()
		select {
		case err := <-swept:
			t.Fatalf("Sweep ended during an ingest: %v", err)
		default:
			runtime.Gosched()
		}
	}
	served := make(chan error, 1)
	go func() {
		if _, ok := s.Cached(small); !ok {
			served <- errors.New("resident blob not served")
			return
		}
		_, err := s.PutBytes([]byte("published meanwhile"))
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		pw.CloseWithError(errors.New("stalled"))
		t.Fatal("reads and publications stalled behind a Sweep waiting for an ingest")
	}
	if _, err := pw.Write(data[len(data)/2:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	if err := <-ingested; err != nil {
		t.Fatal(err)
	}
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	mustRead(t, s, id, data)
	mustRead(t, reopen(t, s), id, data)
}

// Restart scans discard a torn manifest tail while preserving previously
// published chunk graphs, including their references in sealed pack indexes.
func TestChunkedRestartTornManifest(t *testing.T) {
	s := openStoreCache(t, 0)
	s.packs.limit = 128 << 10
	data := randomBody(33, 2<<20)
	id, err := s.PutBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	var leaf rawID
	for raw, e := range packedObjects(s) {
		if e.flags&frameChunk != 0 {
			leaf = raw
			break
		}
	}
	e, ok := s.packs.lookup(leaf)
	if !ok {
		t.Fatal("missing leaf")
	}
	torn := manifestFrame(rawOf(HashBytes("blob", []byte("uncommitted"))), manifestData([]chunkRef{{leaf, int64(e.raw)}}), 0)
	if _, err := s.packs.active.f.WriteAt(torn[:len(torn)-1], s.packs.end); err != nil {
		t.Fatal(err)
	}
	again := reopen(t, s)
	if len(packedObjects(again)) != len(packedObjects(s)) {
		t.Fatal("torn manifest was indexed")
	}
	mustRead(t, again, id, data)
}

func TestChunkedCorruption(t *testing.T) {
	for _, what := range []string{"chunk checksum", "chunk hash", "manifest length", "missing chunk", "manifest cycle"} {
		t.Run(what, func(t *testing.T) {
			s := openStoreCache(t, 0)
			id, err := s.PutBytes(randomBody(15, 2<<20))
			if err != nil {
				t.Fatal(err)
			}
			var leaf rawID
			for raw, e := range packedObjects(s) {
				if e.flags&frameChunk != 0 {
					leaf = raw
					break
				}
			}
			if what == "missing chunk" {
				forget(t, s, leaf.oid())
			} else {
				target := rawOf(id)
				if what == "chunk checksum" || what == "chunk hash" {
					target = leaf
				}
				e, frame, _, err := s.packs.frame(target)
				if err != nil {
					t.Fatal(err)
				}
				switch what {
				case "chunk checksum", "chunk hash":
					frame[len(frame)-1] ^= 0xff
				case "manifest length":
					binary.LittleEndian.PutUint64(frame[frameHeader:], 1<<63)
				case "manifest cycle":
					copy(frame[frameHeader+manifestHeader:], target[:])
				}
				if what != "chunk checksum" {
					sealFrame(frame, target, e.flags, int(e.raw), e.at)
				}
				if _, err := s.packs.files[e.pack].f.WriteAt(frame, int64(e.off)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReadBlob(id); err == nil {
				t.Fatal("corruption read successfully")
			}
		})
	}
}

// Typed tree deltas preserve Git tree IDs, rebase at rollover, and keep full
// chain roots during GC without retaining the children of obsolete trees.
func TestTreeWriterDeltaRollover(t *testing.T) {
	s := openStoreCache(t, 0)
	entries := make([]Entry, 4000)
	for i := range entries {
		entries[i] = Entry{Name: fmt.Sprintf("file-%05d", i), Mode: ModeFile, ID: HashBytes("blob", randomBody(uint64(i), 20))}
	}
	w := s.NewTreeWriter()
	like, err := w.Put(entries)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[OID][]Entry{like: append([]Entry(nil), entries...)}
	for i := range deltaDepth + 4 {
		entries[i].ID = HashBytes("blob", []byte(fmt.Sprint("changed ", i)))
		id, err := w.PutLike(entries, like)
		if err != nil {
			t.Fatal(err)
		}
		e, ok := s.packs.lookup(rawOf(id))
		if !ok || e.flags != frameZstd|frameDelta|frameTree || e.stored > 8<<10 {
			t.Fatalf("tree edit stored as %+v", e)
		}
		if depth := s.packs.depth(rawOf(id)); depth > deltaDepth {
			t.Fatalf("tree chain depth %d", depth)
		}
		versions[id] = append([]Entry(nil), entries...)
		like = id
	}
	for _, st := range []*Store{s, reopen(t, s)} {
		for id, want := range versions {
			got, err := st.ReadTree(id)
			if err != nil || len(got) != len(want) {
				t.Fatal("tree delta unreadable", err)
			}
			body, err := encodeTree(got)
			if err != nil || HashBytes("tree", body) != id {
				t.Fatal("tree delta changed Git ID", err)
			}
		}
	}
	for raw := range packedObjects(s) {
		age(t, s, raw.oid(), time.Now().Add(-time.Hour))
	}
	if _, err := s.Sweep([]OID{like}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := reopen(t, s).ReadTree(like); err != nil {
		t.Fatal(err)
	}
}

// Cyclic delta dictionaries and overlong chains fail within the read bound.
func TestDeltaCycleBounded(t *testing.T) {
	s := openStoreCache(t, 0)
	data := prose(21, 100<<10)
	id := HashBytes("blob", data)
	frame, err := encodeDelta(rawOf(id), data, rawOf(id), data, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.packs.add(frame); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadBlob(id); !errors.Is(err, errCorrupt) {
		t.Fatalf("cyclic delta = %v", err)
	}
	if depth := s.packs.depth(rawOf(id)); depth != deltaDepth+1 {
		t.Fatalf("cyclic depth = %d", depth)
	}
	// Sweeps end whether the cycle is kept fresh, kept reachable, left dead
	// beside enough live data to stay in place, or removed.
	if _, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil || !s.Has(id) {
		t.Fatalf("Sweep keeping a fresh cycle: %v", err)
	}
	age(t, s, id, time.Now().Add(-time.Hour))
	root, err := s.PutTree([]Entry{{Name: "cycle", Mode: ModeFile, ID: id}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep([]OID{root}, time.Now().Add(-time.Minute)); err != nil || !s.Has(id) {
		t.Fatalf("Sweep keeping a reachable cycle: %v", err)
	}
	if _, err := s.PutBytes(noisy("live", 400<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(nil, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(nil, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}
