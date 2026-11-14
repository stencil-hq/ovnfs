package object

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// Pack entry types beyond the four object kinds.
const (
	typeOfsDelta = 6
	typeRefDelta = 7
)

// maxDeltaChain bounds delta chains and base lookups across packs, so a
// corrupt pack whose ref-deltas form a cycle fails instead of looping.
const maxDeltaChain = 10000

// pack is an open pack and its index. It is reference counted: a rescan that
// finds the files gone retires the pack, and its mappings are released only
// once the last reader holding it is done.
type pack struct {
	path string // the .pack file
	idx  *packIndex
	data *mapped.File
	refs atomic.Int64
	// dropped is set once the pack is retired; guarded by deltaBases.mu.
	dropped bool
}

func openPack(idxPath string, f Format) (*pack, error) {
	idx, err := openIndex(idxPath, f)
	if err != nil {
		return nil, err
	}
	path := strings.TrimSuffix(idxPath, ".idx") + ".pack"
	data, err := mapped.Open(path)
	if err != nil {
		idx.m.Close()
		return nil, err
	}
	p := &pack{path: path, idx: idx, data: data}
	if err := p.check(); err != nil {
		idx.m.Close()
		data.Close()
		return nil, fmt.Errorf("pack %s: %w", path, err)
	}
	p.refs.Store(1)
	return p, nil
}

// check verifies the pack header and that the index was built for this pack.
func (p *pack) check() error {
	hs := int64(p.idx.format.Size())
	if p.data.Size() < 12+hs {
		return fmt.Errorf("file too short")
	}
	hdr, err := p.data.Slice(0, 12)
	if err != nil {
		return err
	}
	if !bytes.Equal(hdr[:4], []byte("PACK")) {
		return fmt.Errorf("bad signature")
	}
	if v := binary.BigEndian.Uint32(hdr[4:]); v != 2 && v != 3 {
		return fmt.Errorf("unsupported version %d", v)
	}
	if n := binary.BigEndian.Uint32(hdr[8:]); int(n) != p.idx.count {
		return fmt.Errorf("pack has %d objects, index %d", n, p.idx.count)
	}
	sum, err := p.data.Slice(p.data.Size()-hs, hs)
	if err != nil {
		return err
	}
	if !bytes.Equal(sum, p.idx.packSum) {
		return fmt.Errorf("index does not match pack checksum")
	}
	return nil
}

// acquire takes a reference for a reader; false means the pack is retired.
func (p *pack) acquire() bool {
	for {
		n := p.refs.Load()
		if n <= 0 {
			return false
		}
		if p.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// release drops a reference, unmapping the files with the last one. The
// owning object directory holds one reference until it retires the pack.
func (p *pack) release() {
	if p.refs.Add(-1) == 0 {
		p.idx.m.Close()
		p.data.Close()
	}
}

// entryHeader is a decoded pack entry header.
type entryHeader struct {
	off     int64 // the entry's own offset
	typ     byte
	size    int64 // inflated size: of the object, or of the delta
	data    int64 // offset of the zlib stream
	baseOff int64 // ofs-delta base entry
	baseID  ID    // ref-delta base object
}

func (p *pack) entryAt(off int64) (entryHeader, error) {
	f := p.idx.format
	end := p.data.Size() - int64(f.Size())
	if off < 12 || off >= end {
		return entryHeader{}, fmt.Errorf("pack %s: entry offset %d out of range", p.path, off)
	}
	b, err := p.data.Slice(off, min(end-off, int64(20+f.Size())))
	if err != nil {
		return entryHeader{}, err
	}
	h, err := parseEntryHeader(b, off, f)
	if err != nil {
		return entryHeader{}, fmt.Errorf("pack %s: entry at %d: %w", p.path, off, err)
	}
	return h, nil
}

// parseEntryHeader decodes the header of the entry at off from b, which holds
// at least the header's bytes.
func parseEntryHeader(b []byte, off int64, f Format) (entryHeader, error) {
	h := entryHeader{off: off}
	if len(b) == 0 {
		return h, fmt.Errorf("truncated header")
	}
	c := b[0]
	h.typ = c >> 4 & 7
	size := uint64(c & 15)
	shift, i := 4, 1
	for c&0x80 != 0 {
		if i >= len(b) || shift > 60 {
			return h, fmt.Errorf("bad size")
		}
		c = b[i]
		i++
		size |= uint64(c&0x7f) << shift
		shift += 7
	}
	if size > 1<<62 {
		return h, fmt.Errorf("bad size")
	}
	h.size = int64(size)
	switch h.typ {
	case byte(Commit), byte(Tree), byte(Blob), byte(Tag):
	case typeOfsDelta:
		var rel uint64
		for first := true; first || c&0x80 != 0; first = false {
			if i >= len(b) || rel > 1<<55 {
				return h, fmt.Errorf("bad base offset")
			}
			if !first {
				rel++
			}
			c = b[i]
			i++
			rel = rel<<7 | uint64(c&0x7f)
		}
		if rel == 0 || rel > uint64(off-12) {
			return h, fmt.Errorf("base offset out of range")
		}
		h.baseOff = off - int64(rel)
	case typeRefDelta:
		if i+f.Size() > len(b) {
			return h, fmt.Errorf("truncated base name")
		}
		h.baseID = ID{format: f}
		copy(h.baseID.raw[:], b[i:i+f.Size()])
		i += f.Size()
	default:
		return h, fmt.Errorf("invalid type %d", h.typ)
	}
	h.data = off + int64(i)
	return h, nil
}

// inflateAt decompresses an entry's data.
func (p *pack) inflateAt(h entryHeader) ([]byte, error) {
	if h.size > (p.data.Size()-h.data)*maxDeflateRatio+64 {
		return nil, fmt.Errorf("pack %s: entry at %d claims impossible size %d", p.path, h.off, h.size)
	}
	b, err := inflate(p.data.Reader(h.data), h.size)
	if err != nil {
		return nil, fmt.Errorf("pack %s: entry at %d: %w", p.path, h.off, err)
	}
	return b, nil
}

// read reconstructs the object whose entry is at off. Bases outside the pack
// are read from db; depth counts such cross-pack hops.
func (p *pack) read(db *DB, off int64, depth int) (Kind, []byte, error) {
	var (
		chain  []entryHeader
		kind   Kind
		data   []byte
		shared bool // data is cached and must not be handed out
	)
walk:
	for {
		if len(chain) > maxDeltaChain {
			return 0, nil, fmt.Errorf("pack %s: delta chain too long at %d", p.path, off)
		}
		if k, d, ok := deltaBases.get(baseKey{p, off}); ok {
			kind, data, shared = k, d, true
			break
		}
		h, err := p.entryAt(off)
		if err != nil {
			return 0, nil, err
		}
		switch h.typ {
		case typeOfsDelta:
			chain = append(chain, h)
			off = h.baseOff
		case typeRefDelta:
			chain = append(chain, h)
			i, ok, err := p.idx.find(h.baseID)
			if err != nil {
				return 0, nil, err
			}
			if ok {
				if off, err = p.idx.offset(i); err != nil {
					return 0, nil, err
				}
				continue
			}
			// On-disk packs should be self-contained, but tolerate a base
			// stored elsewhere.
			if kind, data, err = db.read(h.baseID, depth+1); err != nil {
				return 0, nil, fmt.Errorf("pack %s: delta base %s: %v", p.path, h.baseID, err)
			}
			break walk
		default:
			kind = Kind(h.typ)
			if data, err = p.inflateAt(h); err != nil {
				return 0, nil, err
			}
			if len(chain) > 0 {
				deltaBases.put(baseKey{p, off}, kind, data)
				shared = true
			}
			break walk
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		delta, err := p.inflateAt(chain[i])
		if err != nil {
			return 0, nil, err
		}
		if data, err = applyDelta(data, delta); err != nil {
			return 0, nil, fmt.Errorf("pack %s: entry at %d: %w", p.path, chain[i].off, err)
		}
		shared = false
		if i > 0 {
			deltaBases.put(baseKey{p, chain[i].off}, kind, data)
			shared = true
		}
	}
	if shared {
		data = bytes.Clone(data)
	}
	return kind, data, nil
}

// header returns the kind and size of the object whose entry is at off. For a
// delta the size comes from the delta's own header and only the chain's entry
// headers are visited to find the kind.
func (p *pack) header(db *DB, off int64, depth int) (Kind, int64, error) {
	h, err := p.entryAt(off)
	if err != nil {
		return 0, 0, err
	}
	if h.typ != typeOfsDelta && h.typ != typeRefDelta {
		return Kind(h.typ), h.size, nil
	}
	size, err := p.deltaResultSize(h)
	if err != nil {
		return 0, 0, err
	}
	// A chain of maxDeltaChain deltas takes one more step to reach its base.
	for range maxDeltaChain + 1 {
		if k, _, ok := deltaBases.get(baseKey{p, h.off}); ok {
			return k, size, nil
		}
		switch h.typ {
		case typeOfsDelta:
			off = h.baseOff
		case typeRefDelta:
			i, ok, err := p.idx.find(h.baseID)
			if err != nil {
				return 0, 0, err
			}
			if !ok {
				k, _, err := db.header(h.baseID, depth+1)
				if err != nil {
					return 0, 0, fmt.Errorf("pack %s: delta base %s: %v", p.path, h.baseID, err)
				}
				return k, size, nil
			}
			if off, err = p.idx.offset(i); err != nil {
				return 0, 0, err
			}
		default:
			return Kind(h.typ), size, nil
		}
		if h, err = p.entryAt(off); err != nil {
			return 0, 0, err
		}
	}
	return 0, 0, fmt.Errorf("pack %s: delta chain too long at %d", p.path, off)
}

// deltaResultSize inflates just enough of a delta to read its result size.
func (p *pack) deltaResultSize(h entryHeader) (int64, error) {
	z, err := openZlib(p.data.Reader(h.data))
	if err != nil {
		return 0, fmt.Errorf("pack %s: entry at %d: zlib: %w", p.path, h.off, err)
	}
	defer closeZlib(z)
	var buf [20]byte
	n, err := io.ReadFull(z, buf[:min(h.size, int64(len(buf)))])
	if err != nil {
		return 0, fmt.Errorf("pack %s: entry at %d: zlib: %w", p.path, h.off, err)
	}
	_, result, _, err := deltaSizes(buf[:n])
	if err != nil || result > 1<<62 {
		return 0, fmt.Errorf("pack %s: entry at %d: %w", p.path, h.off, errBadDelta)
	}
	return int64(result), nil
}
