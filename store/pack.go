package store

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// Pack frame layout, integers little-endian:
//
//	0  CRC-32C (Castagnoli) of bytes 4 to the end of the frame
//	4  object id, 20 raw bytes
//	24 flags (compression, typed delta, chunk, or manifest; see constants below)
//	25 3 zero bytes
//	28 content length, uint32
//	32 payload length, uint32
//	36 freshness when appended, int64 Unix nanoseconds
//	44 payload: the content, zstd-compressed when frameZstd is set; with
//	   frameDelta, the raw id of the base object followed by the content
//	   compressed with the base's content as zstd raw dictionary
//	   (frameTree selects Git tree rather than blob hash verification).
//	   frameChunk selects the private chunk hash domain; frameManifest stores
//	   a uint64 total followed by (20-byte id, uint64 length) references.
//	   frameInternal selects private manifest nodes rather than blob roots.
//	   A manifest's header content length is zero; its uint64 total supports
//	   files larger than the pack header's uint32 length.
const (
	frameHeader = 44
	// frameZstd marks a payload compressed as one zstd frame.
	frameZstd = 1 << 0
	// frameDelta marks a payload compressed against a base object (see delta.go).
	frameDelta = 1 << 1
	// frameTree gives delta verification the Git tree hash domain.
	frameTree = 1 << 2
	// frameChunk is a content-defined leaf in the private chunk hash domain.
	frameChunk = 1 << 3
	// frameManifest stores length-prefixed chunk references, not object bytes.
	frameManifest = 1 << 4
	// frameInternal distinguishes private manifest nodes from Git blob roots.
	frameInternal = 1 << 5
)

// packLimit is the length past which the active pack is sealed and a new one
// started; it bounds both the scan at Open and the bytes Sweep copies to
// rewrite one pack.
const packLimit = 32 << 20

// activeLimit is the number of objects past which the active pack is sealed
// too, bounding the memory its index takes until then.
const activeLimit = 1 << 14

// castagnoli is the CRC-32C table, hardware-accelerated on amd64 and arm64.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// errCorrupt reports a frame whose checksum or header does not match its index entry.
var errCorrupt = errors.New("corrupt pack frame")

// encoders recycles single-goroutine pack encoders. Payloads are at most
// cloneFile bytes, so a cloneFile window finds every match the default 8 MiB
// one would while an encoder keeps 2 MiB of history instead of 16 MiB; pooled,
// idle encoders go at the next GC instead of one per CPU staying for good.
var encoders = sync.Pool{New: func() any {
	e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderCRC(false), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(cloneFile))
	if err != nil {
		panic(err) // constant options
	}
	return e
}}

// encodeSlots bounds concurrent pack compression, and with it the encoders
// the pool creates, to the CPUs that can run them.
var encodeSlots = make(chan struct{}, runtime.GOMAXPROCS(0))

// decoder decompresses pack payloads; DecodeAll is safe for concurrent use and,
// with the cap limit, never produces more than the content length a frame declares.
var decoder = sync.OnceValue(func() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecodeAllCapLimit(true), zstd.WithDecoderMaxMemory(2*deltaMax))
	if err != nil {
		panic(err) // constant options
	}
	return d
})

// rawID is an object id as 20 raw bytes, the pack index key.
type rawID [20]byte

// rawOf decodes a valid id.
func rawOf(id OID) rawID {
	var r rawID
	for i := range r {
		r[i] = unhex(id[2*i])<<4 | unhex(id[2*i+1])
	}
	return r
}

// unhex decodes one lowercase hex digit.
func unhex(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

// oid encodes r as an object id.
func (r rawID) oid() OID { return OID(hex.EncodeToString(r[:])) }

// packed locates one object in a pack.
type packed struct {
	pack   uint32
	off    uint32 // of the frame header
	stored uint32 // payload length
	raw    uint32 // content length
	flags  uint8
	// at is the object's freshness for Sweep, Unix nanoseconds: when it was
	// appended or last stored again, whichever is later. Set only for the
	// active pack's objects; index files keep it apart.
	at   int64
	base rawID // the base of a delta
}

// encodeFrame returns data as a complete frame for id, fresh at at; the
// payload is zstd-compressed when that makes it smaller.
func encodeFrame(id rawID, data []byte, at int64, flags uint8) []byte {
	buf := make([]byte, frameHeader, frameHeader+len(data))
	encodeSlots <- struct{}{}
	enc := encoders.Get().(*zstd.Encoder)
	buf = enc.EncodeAll(data, buf)
	encoders.Put(enc)
	<-encodeSlots
	if len(buf)-frameHeader >= len(data) {
		buf = append(buf[:frameHeader], data...)
	} else {
		flags |= frameZstd
	}
	sealFrame(buf, id, flags, len(data), at)
	return buf
}

// sealFrame fills in the header of frame, whose payload follows it, and its checksum.
func sealFrame(frame []byte, id rawID, flags uint8, raw int, at int64) {
	copy(frame[4:24], id[:])
	frame[24] = flags
	binary.LittleEndian.PutUint32(frame[28:], uint32(raw))
	binary.LittleEndian.PutUint32(frame[32:], uint32(len(frame)-frameHeader))
	binary.LittleEndian.PutUint64(frame[36:], uint64(at))
	binary.LittleEndian.PutUint32(frame, crc32.Checksum(frame[4:], castagnoli))
}

// parseHeader decodes a frame header, reporting whether its fields are
// consistent; the payload checksum is not verified.
func parseHeader(h []byte) (rawID, packed, bool) {
	var r rawID
	copy(r[:], h[4:24])
	e := packed{
		flags:  h[24],
		raw:    binary.LittleEndian.Uint32(h[28:]),
		stored: binary.LittleEndian.Uint32(h[32:]),
		at:     int64(binary.LittleEndian.Uint64(h[36:])),
	}
	ok := h[25] == 0 && h[26] == 0 && h[27] == 0 && e.stored <= cloneFile
	switch e.flags {
	case 0, frameChunk:
		ok = ok && e.stored == e.raw && e.raw <= cloneFile
	case frameZstd, frameZstd | frameChunk:
		ok = ok && e.raw <= cloneFile
	case frameZstd | frameDelta, frameZstd | frameDelta | frameTree:
		ok = ok && e.raw <= deltaMax && e.stored >= uint32(len(rawID{}))
	case frameManifest, frameManifest | frameInternal:
		ok = ok && e.raw == 0 && e.stored >= manifestHeader && e.stored <= manifestHeader+manifestFanout*manifestRecord
	default:
		ok = false
	}
	return r, e, ok
}

// deltaBase returns the base id of a delta frame's payload.
func deltaBase(frame []byte) rawID {
	var base rawID
	copy(base[:], frame[frameHeader:])
	return base
}

// checkFrame verifies frame, read at e's location, is intact and holds id as e describes.
func checkFrame(id rawID, e packed, frame []byte) error {
	if len(frame) != frameHeader+int(e.stored) || binary.LittleEndian.Uint32(frame) != crc32.Checksum(frame[4:], castagnoli) {
		return errCorrupt
	}
	r, h, ok := parseHeader(frame)
	if !ok || r != id || h.flags != e.flags || h.raw != e.raw || h.stored != e.stored {
		return errCorrupt
	}
	return nil
}

// decodeFrame returns the content of a checked frame other than a delta in a new slice.
func decodeFrame(e packed, frame []byte) ([]byte, error) {
	payload := frame[frameHeader:]
	if e.flags&frameZstd == 0 {
		return payload, nil
	}
	data, err := decoder().DecodeAll(payload, make([]byte, 0, e.raw))
	if err != nil || len(data) != int(e.raw) {
		return nil, errCorrupt
	}
	return data, nil
}

// packFile is one open pack.
type packFile struct {
	n uint32
	f *os.File
	// refs counts the set's listing plus in-flight reads; the file closes at zero,
	// so a read racing Sweep's rewrite finishes on the old pack.
	refs atomic.Int32
}

// release drops one reference, closing the file with the last.
func (pf *packFile) release() {
	if pf.refs.Add(-1) == 0 {
		pf.f.Close()
	}
}

// packSet holds the store's packs and the index of their objects: memory-mapped
// index files for sealed packs (see index.go) and a map for the active pack
// only. A lookup takes the first copy of an object in the merged index, the
// sealed packs' indexes in order, then the active pack; duplicate frames, left
// by a crash during a rewrite, resolve the same way for reads and for Sweep.
type packSet struct {
	dir   string
	limit int64 // packLimit; smaller in tests
	// mu guards files, merged, sealed and entries; readers hold it only to look up.
	mu      sync.RWMutex
	files   map[uint32]*packFile
	merged  *idxFile         // the index merged from earlier sealed packs; nil before the first merge
	sealed  []*idxFile       // indexes of the packs sealed since, oldest first
	entries map[rawID]packed // the objects of the active pack
	// touchMu serializes freshness updates and guards pending and flushing;
	// seal and merge hold it while they read freshness into a new index, so
	// none is lost.
	touchMu sync.Mutex
	// pending holds the freshness of sealed objects stored again, until
	// pendingLimit of them are written to their index files together;
	// flushing holds the batch being written.
	pending, flushing map[rawID]int64
	// flushMu serializes writing batches of pending freshness; it is taken
	// before touchMu, and merges and Sweep hold it so the index files they
	// read are current.
	flushMu sync.Mutex
	// appendMu serializes appends, sealing, merges and rewrites and guards the fields below.
	appendMu sync.Mutex
	active   *packFile // the pack appends go to; nil until the next append creates one
	end      int64     // active's length
	next     uint32    // number of the next new pack
	gen      uint64    // number of the next merged index
	// pinned defers merges while a Sweep marks objects by their position in
	// the current index files.
	pinned bool
}

// packPath is the file of pack n.
func (p *packSet) packPath(n uint32) string {
	return filepath.Join(p.dir, strconv.FormatUint(uint64(n), 10)+".pack")
}

// idxPath is the index file of sealed pack n.
func (p *packSet) idxPath(n uint32) string {
	return filepath.Join(p.dir, strconv.FormatUint(uint64(n), 10)+".idx")
}

// midxPath is merged index file number g.
func (p *packSet) midxPath(g uint64) string {
	return filepath.Join(p.dir, strconv.FormatUint(g, 10)+".midx")
}

// openPacks loads the packs under dir (created if needed): sealed packs from
// the newest intact merged index or their own index, migrating an index of an
// earlier release, any other by scanning its frames, cutting it at the first
// incomplete or corrupt one (a torn tail). The newest unsealed pack with room
// becomes the active one; other unsealed packs are sealed.
func openPacks(dir string) (*packSet, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := &packSet{dir: dir, limit: packLimit, files: map[uint32]*packFile{}, entries: map[rawID]packed{}}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var packs []uint32
	var gens []uint64
	idxs := map[uint32]bool{}
	for _, e := range names {
		base, ext, _ := strings.Cut(e.Name(), ".")
		n, err := strconv.ParseUint(base, 10, 64)
		switch {
		case err == nil && n <= math.MaxUint32 && ext == "pack":
			packs = append(packs, uint32(n))
		case err == nil && n <= math.MaxUint32 && ext == "idx":
			idxs[uint32(n)] = true
		case err == nil && ext == "midx":
			gens = append(gens, n)
		default:
			// Leftover index staging files and anything else unknown.
			removeFile(filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(packs)
	if len(packs) > 0 {
		p.next = packs[len(packs)-1] + 1
	}
	sizes := make(map[uint32]int64, len(packs))
	for _, n := range packs {
		f, err := hostfile.OpenFile(p.packPath(n), os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		pf := &packFile{n: n, f: f}
		pf.refs.Store(1)
		p.files[n] = pf
		sizes[n] = info.Size()
	}
	// The newest intact merged index stands; older ones, and the indexes of
	// packs it covers, are leftovers of an interrupted merge.
	slices.Sort(gens)
	for i := len(gens) - 1; i >= 0; i-- {
		path := p.midxPath(gens[i])
		if p.merged == nil {
			x, err := openIndex(path, func(n uint32) (int64, bool) { size, ok := sizes[n]; return size, ok })
			if err == nil {
				p.merged = x
				continue
			}
		}
		removeFile(path)
	}
	if len(gens) > 0 {
		p.gen = gens[len(gens)-1] + 1
	}
	var unsealed []uint32
	for _, n := range packs {
		has := idxs[n]
		delete(idxs, n)
		switch {
		case p.merged != nil && p.merged.covers(n):
			if has {
				removeFile(p.idxPath(n))
			}
		case has:
			if x := p.loadIdx(n, sizes[n]); x != nil {
				p.sealed = append(p.sealed, x)
				continue
			}
			removeFile(p.idxPath(n))
			unsealed = append(unsealed, n)
		default:
			unsealed = append(unsealed, n)
		}
	}
	for n := range idxs {
		removeFile(p.idxPath(n))
	}
	for i, n := range unsealed {
		pf := p.files[n]
		end, err := p.scan(pf)
		if err == nil && end < sizes[n] {
			err = pf.f.Truncate(end)
		}
		if err != nil {
			return nil, fmt.Errorf("pack %d: %w", n, err)
		}
		if end == 0 {
			delete(p.files, n)
			pf.f.Close()
			removeFile(p.packPath(n))
			continue
		}
		p.active, p.end = pf, end
		if i == len(unsealed)-1 && end < p.limit && len(p.entries) < activeLimit {
			break
		}
		if err := p.seal(); err != nil {
			return nil, err
		}
	}
	if len(p.sealed) >= mergeAfter {
		// A failed merge only leaves lookups more indexes to search.
		p.merge(nil)
	}
	return p, nil
}

// loadIdx opens the index of sealed pack n of length size, first migrating
// one an earlier release wrote; nil means it is missing or damaged.
func (p *packSet) loadIdx(n uint32, size int64) *idxFile {
	path := p.idxPath(n)
	check := func(m uint32) (int64, bool) { return size, m == n }
	x, err := openIndex(path, check)
	if err != nil && migrateIndex(path, n, size) {
		x, err = openIndex(path, check)
	}
	if err != nil {
		return nil
	}
	if len(x.packs) != 1 {
		x.close()
		return nil
	}
	return x
}

// scan indexes the first frame of each object in pf as the active pack's,
// returning the length of its intact prefix. Shadows of earlier indexes stay
// recorded so Sweep can remove their bytes before they become canonical.
func (p *packSet) scan(pf *packFile) (int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(pf.f, 0, 1<<62), 1<<20)
	var off int64
	frame := make([]byte, frameHeader, frameHeader+cloneFile)
	for {
		if _, err := io.ReadFull(r, frame[:frameHeader]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return off, nil
			}
			return 0, err
		}
		id, e, ok := parseHeader(frame)
		if !ok {
			return off, nil
		}
		frame = frame[:frameHeader+int(e.stored)]
		if _, err := io.ReadFull(r, frame[frameHeader:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return off, nil
			}
			return 0, err
		}
		if binary.LittleEndian.Uint32(frame) != crc32.Checksum(frame[4:], castagnoli) {
			return off, nil
		}
		e.pack, e.off = pf.n, uint32(off)
		if _, dup := p.entries[id]; !dup {
			if e.flags&frameManifest != 0 && checkManifest(id, frame) != nil {
				return off, nil
			}
			if e.flags&frameDelta != 0 {
				e.base = deltaBase(frame)
			}
			p.entries[id] = e
		}
		off += int64(len(frame))
	}
}

// checkManifest reports a manifest frame whose references do not parse or,
// for a private page, whose content does not hash to id.
func checkManifest(id rawID, frame []byte) error {
	data := frame[frameHeader:]
	if _, _, err := parseManifest(data); err != nil || frame[24]&frameInternal != 0 && rawOf(HashBytes("manifest", data)) != id {
		return errCorrupt
	}
	return nil
}

// seal ends appends to the active pack: it is made durable and its objects
// move from memory to an index file, so Open need not scan it. Should the
// index fail to write, it is kept in memory, and the next Open scans the pack.
// Enough sealed indexes are merged. p.appendMu must be held.
func (p *packSet) seal() error {
	pf := p.active
	if pf == nil {
		return nil
	}
	if err := pf.f.Sync(); err != nil {
		return err
	}
	p.touchMu.Lock()
	data := encodeIndex(p.entries, pf.n, p.end)
	x := memIndex(data)
	path := p.idxPath(pf.n)
	if createAtomic(path, func(f *os.File) error { _, err := f.Write(data); return err }) == nil {
		if mapped, err := openIndex(path, nil); err == nil {
			x = mapped
		}
	}
	p.mu.Lock()
	p.sealed = append(p.sealed, x)
	p.entries = map[rawID]packed{}
	p.mu.Unlock()
	p.touchMu.Unlock()
	p.active, p.end = nil, 0
	if !p.pinned && len(p.sealed) >= mergeAfter {
		// A failed merge only leaves lookups more indexes to search.
		p.merge(nil)
	}
	return nil
}

// sources lists the index files in lookup order; p.mu is held.
func (p *packSet) sources() []*idxFile {
	srcs := make([]*idxFile, 0, len(p.sealed)+1)
	if p.merged != nil {
		srcs = append(srcs, p.merged)
	}
	return append(srcs, p.sealed...)
}

// merge replaces the merged index and the sealed packs' indexes with one index
// of their objects outside the packs in drop, made durable before the old
// files are removed. p.appendMu must be held.
func (p *packSet) merge(drop map[uint32]bool) error {
	p.mu.RLock()
	srcs := p.sources()
	p.mu.RUnlock()
	var packs []packLen
	for _, x := range srcs {
		for _, pl := range x.packs {
			if !drop[pl.n] {
				packs = append(packs, pl)
			}
		}
	}
	slices.SortFunc(packs, func(a, b packLen) int { return cmp.Compare(a.n, b.n) })
	packs = slices.CompactFunc(packs, func(a, b packLen) bool { return a.n == b.n })
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	p.touchMu.Lock()
	defer p.touchMu.Unlock()
	p.flush()
	var x *idxFile // no index without packs to cover
	if len(packs) > 0 {
		path := p.midxPath(p.gen)
		if err := writeIndex(path, srcs, drop, packs); err != nil {
			return err
		}
		syncDir(p.dir)
		var err error
		if x, err = openIndex(path, nil); err != nil {
			removeFile(path)
			return err
		}
		p.gen++
	}
	p.mu.Lock()
	p.merged, p.sealed = x, nil
	p.mu.Unlock()
	for _, old := range srcs {
		old.close()
		if old.path != "" {
			removeFile(old.path)
		}
	}
	return nil
}

// reserve makes the active pack able to take n more bytes, sealing a full one
// and starting a new one as needed; p.appendMu must be held.
func (p *packSet) reserve(n int) error {
	p.mu.RLock()
	full := len(p.entries) >= activeLimit
	p.mu.RUnlock()
	if p.active != nil && !full && (p.end == 0 || p.end+int64(n) <= p.limit) {
		return nil
	}
	if err := p.seal(); err != nil {
		return err
	}
	num := p.next
	f, err := hostfile.OpenFile(p.packPath(num), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	p.next++
	pf := &packFile{n: num, f: f}
	pf.refs.Store(1)
	p.mu.Lock()
	p.files[num] = pf
	p.mu.Unlock()
	p.active, p.end = pf, 0
	return nil
}

// write appends frame to the active pack, returning where it went; a failed
// write is cut off again. p.appendMu must be held.
func (p *packSet) write(frame []byte) (uint32, uint32, error) {
	if err := p.reserve(len(frame)); err != nil {
		return 0, 0, err
	}
	off := p.end
	if _, err := p.active.f.WriteAt(frame, off); err != nil {
		p.active.f.Truncate(off)
		return 0, 0, err
	}
	p.end += int64(len(frame))
	return p.active.n, uint32(off), nil
}

// add appends frame, encoded by encodeFrame, unless its object is already
// packed, which is then refreshed instead; it reports whether it was.
func (p *packSet) add(frame []byte) (bool, error) {
	id, e, _ := parseHeader(frame)
	p.appendMu.Lock()
	defer p.appendMu.Unlock()
	if p.touch(id, e.at) {
		return true, nil
	}
	var err error
	if e.pack, e.off, err = p.write(frame); err != nil {
		return false, err
	}
	if e.flags&frameDelta != 0 {
		e.base = deltaBase(frame)
	}
	p.mu.Lock()
	p.entries[id] = e
	p.mu.Unlock()
	return false, nil
}

// locate finds the copy of id lookups use: x and i name its index record, x
// nil for the active pack. Only the active pack's e carries its freshness;
// an index file's is read with x.at(i). p.mu is held.
func (p *packSet) locate(id rawID) (x *idxFile, i int, e packed, ok bool) {
	if x := p.merged; x != nil {
		if i, ok := x.find(id); ok {
			return x, i, x.entry(i), true
		}
	}
	for _, x := range p.sealed {
		if i, ok := x.find(id); ok {
			return x, i, x.entry(i), true
		}
	}
	e, ok = p.entries[id]
	return nil, 0, e, ok
}

// depth returns the number of deltas to decode to read packed object id.
func (p *packSet) depth(id rawID) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for n := range deltaDepth + 1 {
		_, _, e, ok := p.locate(id)
		if !ok || e.flags&frameDelta == 0 {
			return n
		}
		id = e.base
	}
	return deltaDepth + 1
}

// chainRoot returns the full object at the bottom of a bounded delta chain.
func (p *packSet) chainRoot(id rawID) (rawID, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for range deltaDepth + 1 {
		_, _, e, ok := p.locate(id)
		if !ok || e.flags&frameDelta == 0 {
			return id, true
		}
		id = e.base
	}
	return rawID{}, false
}

// lookup returns the location of packed object id.
func (p *packSet) lookup(id rawID) (packed, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, _, e, ok := p.locate(id)
	return e, ok
}

// pendingLimit is the number of sealed objects whose new freshness is kept in
// memory before it is written to their index files.
const pendingLimit = 1 << 12

// touch raises packed object id's freshness to at, reporting whether it is
// packed. A sealed object's new freshness is written to its index in batches,
// so it outlasts a restart once written.
func (p *packSet) touch(id rawID, at int64) bool {
	p.touchMu.Lock()
	p.mu.RLock()
	x, i, e, ok := p.locate(id)
	if x != nil {
		e.at = max(x.at(i), p.pending[id], p.flushing[id])
	}
	p.mu.RUnlock()
	switch {
	case !ok || e.at >= at:
	case x == nil || x.heap:
		// Readers see the active pack's map or an index in memory directly.
		p.mu.Lock()
		if x != nil {
			x.setAt(i, at)
		} else {
			e.at = at
			p.entries[id] = e
		}
		p.mu.Unlock()
	default:
		if p.pending == nil {
			p.pending = map[rawID]int64{}
		}
		p.pending[id] = at
		if len(p.pending) >= pendingLimit && p.flushing == nil {
			p.flushing, p.pending = p.pending, nil
			p.touchMu.Unlock()
			p.flushBatch()
			return true
		}
	}
	p.touchMu.Unlock()
	return ok
}

// flushBatch writes the flushing batch to the index files, then lets a new
// batch start.
func (p *packSet) flushBatch() {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	p.writePending(p.flushing)
	p.touchMu.Lock()
	p.flushing = nil
	p.touchMu.Unlock()
}

// flush writes all pending freshness to the index files; p.flushMu and
// p.touchMu are held, so no batch is being written.
func (p *packSet) flush() {
	p.writePending(p.pending)
	p.pending = nil
}

// writePending writes the freshness in batch, which no one modifies, to the
// index records lookups use; p.flushMu is held, so those stay in place.
func (p *packSet) writePending(batch map[rawID]int64) {
	updates := make([]freshUpdate, 0, len(batch))
	p.mu.RLock()
	for id, at := range batch {
		if x, i, _, ok := p.locate(id); x != nil && ok {
			updates = append(updates, freshUpdate{x, i, at})
		}
	}
	p.mu.RUnlock()
	writeUpdates(updates)
}

// frame reads and verifies the frame of packed object id; ok is false when it is not packed.
func (p *packSet) frame(id rawID) (packed, []byte, bool, error) {
	p.mu.RLock()
	_, _, e, ok := p.locate(id)
	var pf *packFile
	if ok {
		pf = p.files[e.pack]
		if pf == nil || e.stored > cloneFile {
			p.mu.RUnlock()
			return e, nil, true, errCorrupt
		}
		pf.refs.Add(1)
	}
	p.mu.RUnlock()
	if !ok {
		return packed{}, nil, false, nil
	}
	frame := make([]byte, frameHeader+int(e.stored))
	_, err := pf.f.ReadAt(frame, int64(e.off))
	pf.release()
	if err == nil {
		err = checkFrame(id, e, frame)
	}
	if err != nil {
		return e, nil, true, fmt.Errorf("pack %d offset %d: %w", e.pack, e.off, err)
	}
	return e, frame, true, nil
}
