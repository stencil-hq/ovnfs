package store

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"slices"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// Index files locate packed objects by id without a resident map: records are
// sorted by id and memory-mapped, so a lookup reads a fanout slot and binary
// searches a handful of records, and an idle index costs clean file pages
// only. A sealed pack gets its own index (<n>.idx); once mergeAfter of those
// accumulate they are merged into one index covering many packs (<g>.midx).
// Layout, integers little-endian:
//
//	0   magic "ovnfsidx"
//	8   format version uint32 (indexVersion)
//	12  record count uint32
//	16  covered pack count uint32
//	20  fanout bits uint32: the leading id bits the fanout table buckets by
//	24  CRC-32C of the records, fanout table and pack table, in that order
//	28  4 zero bytes
//	32  records sorted by id, idxRecord bytes each: id (20 raw bytes), pack
//	    uint32, frame offset uint32, payload length uint32, content length
//	    uint32, flags, 7 zero bytes, delta base id (20 raw bytes, zero unless
//	    the frame is a delta)
//	    fanout table: 1<<bits uint32, entry b the number of records whose id's
//	    leading bits are at most b
//	    pack table: per covered pack ascending, number uint32, 4 zero bytes,
//	    length uint64
//	    freshness: per record an int64 Unix nanoseconds, rewritten in place
//	    when the object is stored again, so it is outside the checksum
const (
	indexMagic   = "ovnfsidx"
	indexVersion = 1
	indexHeader  = 32
	idxRecord    = 64
	idxPack      = 16
)

// mergeAfter is the number of sealed pack indexes that triggers a merge: a
// lookup that misses searches the merged index and at most this many more.
const mergeAfter = 8

// errIndex reports an index file that is damaged or does not match its packs.
var errIndex = errors.New("invalid pack index")

// packLen is a pack an index covers and that pack's length.
type packLen struct {
	n    uint32
	size int64
}

// idxFile is one open index file, or an index encoded in memory.
type idxFile struct {
	path string
	m    *mapped.File // nil when data is a private copy
	w    *os.File     // writes freshness in place; nil for an index only in memory
	// heap marks data as a private copy rather than a read-only mapping, so
	// freshness updates are written to it too.
	heap   bool
	n      int
	bits   uint
	sum    uint32 // the header's checksum
	recs   []byte
	fanout []byte
	table  []byte // the pack table
	fresh  []byte
	packs  []packLen
}

// fanoutBits picks the fanout width for about n records: buckets of about
// eight records, so a search touches one or two cache lines.
func fanoutBits(n int) uint {
	bits := uint(8)
	for bits < 16 && n>>bits > 8 {
		bits++
	}
	return bits
}

// layoutIndex slices data, a complete index file, into its sections, checking
// only that the header matches the length; validate checks the content.
func layoutIndex(data []byte) (*idxFile, error) {
	if len(data) < indexHeader || string(data[:8]) != indexMagic || binary.LittleEndian.Uint32(data[8:]) != indexVersion {
		return nil, errIndex
	}
	n := int(binary.LittleEndian.Uint32(data[12:]))
	np := int(binary.LittleEndian.Uint32(data[16:]))
	bits := uint(binary.LittleEndian.Uint32(data[20:]))
	if bits < 8 || bits > 16 || binary.LittleEndian.Uint32(data[28:]) != 0 || len(data) != indexHeader+n*(idxRecord+8)+4<<bits+np*idxPack {
		return nil, errIndex
	}
	x := &idxFile{n: n, bits: bits, sum: binary.LittleEndian.Uint32(data[24:])}
	off := indexHeader
	x.recs, off = data[off:off+n*idxRecord], off+n*idxRecord
	x.fanout, off = data[off:off+4<<bits], off+4<<bits
	x.table, off = data[off:off+np*idxPack], off+np*idxPack
	x.fresh = data[off:]
	x.packs = make([]packLen, np)
	for i := range x.packs {
		rec := x.table[i*idxPack:]
		x.packs[i] = packLen{binary.LittleEndian.Uint32(rec), int64(binary.LittleEndian.Uint64(rec[8:]))}
	}
	return x, nil
}

// validate checks the checksum, that records are sorted, unique and bucketed
// as the fanout says, that every record is a consistent frame inside a
// covered pack, and that each covered pack has the length size reports.
func (x *idxFile) validate(size func(uint32) (int64, bool)) error {
	if crc32.Update(crc32.Update(crc32.Checksum(x.recs, castagnoli), castagnoli, x.fanout), castagnoli, x.table) != x.sum {
		return errIndex
	}
	lens := make(map[uint32]int64, len(x.packs))
	for i, pl := range x.packs {
		if i > 0 && pl.n <= x.packs[i-1].n {
			return errIndex
		}
		if have, ok := size(pl.n); !ok || have != pl.size {
			return errIndex
		}
		lens[pl.n] = pl.size
	}
	var prev rawID
	bucket := 0
	for i := range x.n {
		rec := x.recs[i*idxRecord : (i+1)*idxRecord]
		id := x.id(i)
		if i > 0 && bytes.Compare(prev[:], id[:]) >= 0 {
			return errIndex
		}
		prev = id
		for b := x.bucket(id); bucket < b; bucket++ {
			if x.fanoutAt(bucket) != i {
				return errIndex
			}
		}
		e := x.entry(i)
		var h [frameHeader]byte
		h[24] = e.flags
		binary.LittleEndian.PutUint32(h[28:], e.raw)
		binary.LittleEndian.PutUint32(h[32:], e.stored)
		_, _, ok := parseHeader(h[:])
		plen, covered := lens[e.pack]
		if !ok || !covered || int64(e.off)+frameHeader+int64(e.stored) > plen ||
			binary.LittleEndian.Uint64(rec[36:])>>8 != 0 ||
			e.flags&frameDelta == 0 && e.base != (rawID{}) {
			return errIndex
		}
	}
	for ; bucket < 1<<x.bits; bucket++ {
		if x.fanoutAt(bucket) != x.n {
			return errIndex
		}
	}
	return nil
}

// openIndex opens the index file at path, mapped where possible; with size
// non-nil it is validated against the packs size reports. An index that
// cannot be mapped is read into memory.
func openIndex(path string, size func(uint32) (int64, bool)) (*idxFile, error) {
	m, err := mapped.Open(path)
	if err != nil {
		return nil, err
	}
	data := m.Mapped()
	heap := data == nil
	if heap {
		data, err = m.Slice(0, m.Size())
		m.Close()
		m = nil
		if err != nil {
			return nil, err
		}
	}
	x, err := layoutIndex(data)
	if err == nil && size != nil {
		err = x.validate(size)
	}
	var w *os.File
	if err == nil {
		w, err = hostfile.OpenFile(path, os.O_RDWR, 0)
	}
	if err != nil {
		if m != nil {
			m.Close()
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	x.path, x.m, x.w, x.heap = path, m, w, heap
	return x, nil
}

// close releases the index's mapping and handle.
func (x *idxFile) close() {
	if x.m != nil {
		x.m.Close()
	}
	if x.w != nil {
		x.w.Close()
	}
}

// bucket is the fanout slot of id.
func (x *idxFile) bucket(id rawID) int { return int(binary.BigEndian.Uint32(id[:4]) >> (32 - x.bits)) }

// fanoutAt is the number of records in buckets up to b.
func (x *idxFile) fanoutAt(b int) int { return int(binary.LittleEndian.Uint32(x.fanout[4*b:])) }

// find returns the record of id, or where it would be inserted.
func (x *idxFile) find(id rawID) (int, bool) {
	b := x.bucket(id)
	lo, hi := 0, x.fanoutAt(b)
	if b > 0 {
		lo = x.fanoutAt(b - 1)
	}
	key := binary.BigEndian.Uint64(id[:8])
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		rec := x.recs[mid*idxRecord : mid*idxRecord+20]
		c := cmp.Compare(binary.BigEndian.Uint64(rec), key)
		if c == 0 {
			c = bytes.Compare(rec[8:], id[8:])
		}
		switch c {
		case 0:
			return mid, true
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false
}

// has reports whether the index holds id.
func (x *idxFile) has(id rawID) bool {
	_, ok := x.find(id)
	return ok
}

// covers reports whether the index covers pack n.
func (x *idxFile) covers(n uint32) bool {
	_, ok := slices.BinarySearchFunc(x.packs, n, func(pl packLen, n uint32) int { return cmp.Compare(pl.n, n) })
	return ok
}

// id returns the id of record i.
func (x *idxFile) id(i int) rawID { return rawID(x.recs[i*idxRecord : i*idxRecord+20]) }

// record returns the bytes of record i.
func (x *idxFile) record(i int) []byte { return x.recs[i*idxRecord : (i+1)*idxRecord] }

// at returns the freshness of record i.
func (x *idxFile) at(i int) int64 { return int64(binary.LittleEndian.Uint64(x.fresh[8*i:])) }

// entry decodes record i but for its freshness, which lives apart (see at),
// so a lookup touches no more than the record.
func (x *idxFile) entry(i int) packed {
	rec := x.record(i)
	return packed{
		pack:   binary.LittleEndian.Uint32(rec[20:]),
		off:    binary.LittleEndian.Uint32(rec[24:]),
		stored: binary.LittleEndian.Uint32(rec[28:]),
		raw:    binary.LittleEndian.Uint32(rec[32:]),
		flags:  rec[36],
		base:   rawID(rec[44:64]),
	}
}

// freshOff is the file offset of record i's freshness.
func (x *idxFile) freshOff(i int) int64 {
	return int64(indexHeader + x.n*idxRecord + 4<<x.bits + len(x.packs)*idxPack + 8*i)
}

// setAt writes the freshness of record i in place; a failed write leaves the
// older freshness, so Sweep keeps the object no longer than before.
func (x *idxFile) setAt(i int, at int64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(at))
	x.writeFresh(i, b[:])
}

// writeFresh writes freshness values, 8 bytes each, from record i on.
func (x *idxFile) writeFresh(i int, b []byte) {
	if x.w != nil {
		x.w.WriteAt(b, x.freshOff(i))
	}
	if x.heap {
		copy(x.fresh[8*i:], b)
	}
}

// freshUpdate is a pending freshness for record i of index file x.
type freshUpdate struct {
	x  *idxFile
	i  int
	at int64
}

// freshGap is the largest run of unchanged freshness bytes a write of
// updates spans rather than splits: the page the kernel writes back anyway.
const freshGap = 4096

// writeUpdates writes updates to their index files, coalescing those within
// freshGap bytes of each other into one write of the span between them; the
// unchanged values in a span are copied from the file. Updates only raise
// freshness: a delayed batch may resolve an index already updated or merged
// with a newer batch. Only one writer may update an index file's freshness at
// a time.
func writeUpdates(updates []freshUpdate) {
	slices.SortFunc(updates, func(a, b freshUpdate) int {
		if a.x != b.x {
			return cmp.Compare(a.x.path, b.x.path)
		}
		return cmp.Compare(a.i, b.i)
	})
	var buf []byte
	for k := 0; k < len(updates); {
		x, first := updates[k].x, updates[k].i
		last := first
		end := k
		for end < len(updates) && updates[end].x == x && 8*(updates[end].i-last) <= freshGap {
			last = updates[end].i
			end++
		}
		buf = append(buf[:0], x.fresh[8*first:8*(last+1)]...)
		for _, u := range updates[k:end] {
			b := buf[8*(u.i-first):]
			at := max(u.at, int64(binary.LittleEndian.Uint64(b)))
			binary.LittleEndian.PutUint64(b, uint64(at))
		}
		x.writeFresh(first, buf)
		k = end
	}
}

// encodeRecord fills rec with the index record of id at e.
func encodeRecord(rec []byte, id rawID, e packed) {
	clear(rec[:idxRecord])
	copy(rec, id[:])
	binary.LittleEndian.PutUint32(rec[20:], e.pack)
	binary.LittleEndian.PutUint32(rec[24:], e.off)
	binary.LittleEndian.PutUint32(rec[28:], e.stored)
	binary.LittleEndian.PutUint32(rec[32:], e.raw)
	rec[36] = e.flags
	if e.flags&frameDelta != 0 {
		copy(rec[44:], e.base[:])
	}
}

// indexWriter streams an index: records in id order, then the tables.
type indexWriter struct {
	w      io.Writer
	crc    uint32
	bits   uint
	counts []uint32
	n      int
	err    error
}

// newIndexWriter starts an index of at most about n records on w, which has
// already received indexHeader placeholder bytes.
func newIndexWriter(w io.Writer, n int) *indexWriter {
	bits := fanoutBits(n)
	return &indexWriter{w: w, bits: bits, counts: make([]uint32, 1<<bits)}
}

// write writes b, remembering the first error.
func (iw *indexWriter) write(b []byte) {
	if iw.err == nil {
		_, iw.err = iw.w.Write(b)
	}
}

// add appends a record; records must come in ascending id order.
func (iw *indexWriter) add(rec []byte) {
	iw.write(rec[:idxRecord])
	iw.crc = crc32.Update(iw.crc, castagnoli, rec[:idxRecord])
	iw.counts[binary.BigEndian.Uint32(rec[:4])>>(32-iw.bits)]++
	iw.n++
}

// finish writes the fanout and pack tables and the freshness of every record
// in order, returning the header to write at the start.
func (iw *indexWriter) finish(packs []packLen, fresh func(yield func(int64) bool)) ([indexHeader]byte, error) {
	table := make([]byte, 4<<iw.bits+len(packs)*idxPack)
	var sum uint32
	for b, c := range iw.counts {
		sum += c
		binary.LittleEndian.PutUint32(table[4*b:], sum)
	}
	for i, pl := range packs {
		rec := table[4<<iw.bits+i*idxPack:]
		binary.LittleEndian.PutUint32(rec, pl.n)
		binary.LittleEndian.PutUint64(rec[8:], uint64(pl.size))
	}
	iw.write(table)
	crc := crc32.Update(iw.crc, castagnoli, table)
	var b [8]byte
	for at := range fresh {
		binary.LittleEndian.PutUint64(b[:], uint64(at))
		iw.write(b[:])
	}
	var h [indexHeader]byte
	copy(h[:], indexMagic)
	binary.LittleEndian.PutUint32(h[8:], indexVersion)
	binary.LittleEndian.PutUint32(h[12:], uint32(iw.n))
	binary.LittleEndian.PutUint32(h[16:], uint32(len(packs)))
	binary.LittleEndian.PutUint32(h[20:], uint32(iw.bits))
	binary.LittleEndian.PutUint32(h[24:], crc)
	return h, iw.err
}

// encodeIndex returns the complete index of the objects in entries, all in
// pack n of length size.
func encodeIndex(entries map[rawID]packed, n uint32, size int64) []byte {
	ids := make([]rawID, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b rawID) int { return bytes.Compare(a[:], b[:]) })
	var buf bytes.Buffer
	buf.Grow(indexHeader + len(ids)*(idxRecord+8) + 4<<fanoutBits(len(ids)) + idxPack)
	buf.Write(make([]byte, indexHeader))
	iw := newIndexWriter(&buf, len(ids))
	var rec [idxRecord]byte
	for _, id := range ids {
		encodeRecord(rec[:], id, entries[id])
		iw.add(rec[:])
	}
	h, _ := iw.finish([]packLen{{n, size}}, func(yield func(int64) bool) {
		for _, id := range ids {
			if !yield(entries[id].at) {
				return
			}
		}
	})
	data := buf.Bytes()
	copy(data, h[:])
	return data
}

// memIndex wraps an index encoded by encodeIndex.
func memIndex(data []byte) *idxFile {
	x, err := layoutIndex(data)
	if err != nil {
		panic(err) // encodeIndex output
	}
	x.heap = true
	return x
}

// mergeRecords calls yield, in id order, with the record that wins for each
// id across srcs: the first in srcs order not in a pack drop holds.
func mergeRecords(srcs []*idxFile, drop map[uint32]bool, yield func(x *idxFile, i int) bool) {
	pos := make([]int, len(srcs))
	for {
		var low rawID
		found := false
		for k, x := range srcs {
			if pos[k] < x.n {
				if id := x.id(pos[k]); !found || bytes.Compare(id[:], low[:]) < 0 {
					low, found = id, true
				}
			}
		}
		if !found {
			return
		}
		var win *idxFile
		wi := 0
		for k, x := range srcs {
			if pos[k] >= x.n || x.id(pos[k]) != low {
				continue
			}
			if win == nil && !drop[binary.LittleEndian.Uint32(x.record(pos[k])[20:])] {
				win, wi = x, pos[k]
			}
			pos[k]++
		}
		if win != nil && !yield(win, wi) {
			return
		}
	}
}

// createAtomic writes the file path through a synced temporary file renamed
// into place, so a crash leaves either no file or a complete one.
func createAtomic(path string, write func(*os.File) error) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		removeFile(tmp)
	}
	return err
}

// writeIndex writes the index of the objects in srcs, as mergeRecords picks
// them, covering packs, to path.
func writeIndex(path string, srcs []*idxFile, drop map[uint32]bool, packs []packLen) error {
	upper := 0
	for _, x := range srcs {
		upper += x.n
	}
	return createAtomic(path, func(f *os.File) error {
		bw := bufio.NewWriterSize(f, 1<<20)
		bw.Write(make([]byte, indexHeader))
		iw := newIndexWriter(bw, upper)
		mergeRecords(srcs, drop, func(x *idxFile, i int) bool {
			iw.add(x.record(i))
			return iw.err == nil
		})
		h, err := iw.finish(packs, func(yield func(int64) bool) {
			mergeRecords(srcs, drop, func(x *idxFile, i int) bool { return yield(x.at(i)) })
		})
		if err == nil {
			err = bw.Flush()
		}
		if err == nil {
			_, err = f.WriteAt(h[:], 0)
		}
		return err
	})
}

// legacyRecord and legacyTrailer size the unsorted index files of earlier
// releases: per frame id, offset, payload length, content length, flags, 3
// zero bytes, freshness and delta base; then the pack length (uint64), the
// record count (uint32) and the CRC-32C of everything before.
const (
	legacyRecord  = 64
	legacyTrailer = 16
)

// migrateIndex replaces the earlier release's index of pack n, of length size,
// at path with a sorted index, reporting whether the old one was intact.
func migrateIndex(path string, n uint32, size int64) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < legacyTrailer || (len(data)-legacyTrailer)%legacyRecord != 0 {
		return false
	}
	body, trailer := data[:len(data)-legacyTrailer], data[len(data)-legacyTrailer:]
	if binary.LittleEndian.Uint64(trailer) != uint64(size) ||
		int(binary.LittleEndian.Uint32(trailer[8:])) != len(body)/legacyRecord ||
		binary.LittleEndian.Uint32(trailer[12:]) != crc32.Checksum(body, castagnoli) {
		return false
	}
	entries := make(map[rawID]packed, len(body)/legacyRecord)
	for rec := range slices.Chunk(body, legacyRecord) {
		e := packed{
			pack:   n,
			off:    binary.LittleEndian.Uint32(rec[20:]),
			stored: binary.LittleEndian.Uint32(rec[24:]),
			raw:    binary.LittleEndian.Uint32(rec[28:]),
			flags:  rec[32],
			at:     int64(binary.LittleEndian.Uint64(rec[36:])),
			base:   rawID(rec[44:64]),
		}
		if e.flags&frameDelta == 0 {
			e.base = rawID{}
		}
		if _, dup := entries[rawID(rec[:20])]; !dup {
			entries[rawID(rec[:20])] = e
		}
	}
	data = encodeIndex(entries, n, size)
	if x, _ := layoutIndex(data); x.validate(func(m uint32) (int64, bool) { return size, m == n }) != nil {
		return false
	}
	return createAtomic(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}) == nil
}
