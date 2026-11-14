package object

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// idxMagic starts a version 2 or later pack index; as a version 1 fan-out
// entry it would be an impossible object count.
var idxMagic = []byte{0xff, 't', 'O', 'c'}

// packIndex is an open .idx file: version 1, or version 2 with its 31-bit
// offset table and 64-bit large-offset table.
type packIndex struct {
	m      *mapped.File
	format Format
	fanout [256]uint32
	count  int
	names  int64 // file offset of the first object name
	stride int64 // distance between consecutive names
	// Version 2 tables; offsets is 0 for version 1, where each name is
	// preceded by its 4-byte pack offset.
	offsets, large int64
	nLarge         int64
	packSum        []byte // the pack checksum the index was built for
}

// PackIndex is a pack index held in memory apart from any database, such as
// the objects/pack/*.idx a dumb HTTP server publishes beside each pack: it
// tells which objects a pack holds before the pack is downloaded.
type PackIndex struct{ x *packIndex }

// ParseIndex parses a version 1 or 2 pack index of format f. Lookups read
// data in place, so it must stay unchanged and mapped while the index is used.
func ParseIndex(data []byte, f Format) (*PackIndex, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("pack index: file too short")
	}
	x, err := parseIndex(mapped.Bytes(data), f)
	if err != nil {
		return nil, fmt.Errorf("pack index: %w", err)
	}
	return &PackIndex{x}, nil
}

// Has reports whether the pack holds id.
func (p *PackIndex) Has(id ID) bool {
	if id.format != p.x.format {
		return false
	}
	_, ok, _ := p.x.find(id) // in-memory lookups cannot fail
	return ok
}

func openIndex(path string, f Format) (*packIndex, error) {
	m, err := mapped.Open(path)
	if err != nil {
		return nil, err
	}
	x, err := parseIndex(m, f)
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("pack index %s: %w", path, err)
	}
	return x, nil
}

func parseIndex(m *mapped.File, f Format) (*packIndex, error) {
	hs := int64(f.Size())
	x := &packIndex{m: m, format: f}
	head, err := m.Slice(0, min(m.Size(), 8+256*4))
	if err != nil || len(head) < 256*4 {
		return nil, fmt.Errorf("file too short")
	}
	version := 1
	fan := head
	if bytes.HasPrefix(head, idxMagic) {
		if len(head) < 8+256*4 {
			return nil, fmt.Errorf("file too short")
		}
		version = int(binary.BigEndian.Uint32(head[4:]))
		if version != 2 {
			return nil, fmt.Errorf("unsupported version %d", version)
		}
		fan = head[8:]
	}
	for i := range x.fanout {
		x.fanout[i] = binary.BigEndian.Uint32(fan[4*i:])
		if i > 0 && x.fanout[i] < x.fanout[i-1] {
			return nil, fmt.Errorf("fan-out table not monotonic")
		}
	}
	n := int64(x.fanout[255])
	x.count = int(n)
	trailer := 2 * hs
	if version == 1 {
		x.names = 256*4 + 4
		x.stride = 4 + hs
		if m.Size() != 256*4+n*x.stride+trailer {
			return nil, fmt.Errorf("size %d does not match %d objects", m.Size(), n)
		}
	} else {
		x.names = 8 + 256*4
		x.stride = hs
		x.offsets = x.names + n*hs + n*4
		x.large = x.offsets + n*4
		rest := m.Size() - trailer - x.large
		if rest < 0 || rest%8 != 0 || rest/8 > n {
			return nil, fmt.Errorf("size %d does not match %d objects", m.Size(), n)
		}
		x.nLarge = rest / 8
	}
	sum, err := m.Slice(m.Size()-trailer, hs)
	if err != nil {
		return nil, err
	}
	x.packSum = bytes.Clone(sum)
	return x, nil
}

// rawName returns the name at position i, using buf when the file is not mapped.
func (x *packIndex) rawName(i int, buf *[32]byte) ([]byte, error) {
	hs := int64(x.format.Size())
	off := x.names + int64(i)*x.stride
	if x.m.Mapped() != nil {
		return x.m.Mapped()[off : off+hs], nil
	}
	if _, err := x.m.ReadAt(buf[:hs], off); err != nil {
		return nil, err
	}
	return buf[:hs], nil
}

// name returns the object name at position i.
func (x *packIndex) name(i int) (ID, error) {
	var buf [32]byte
	raw, err := x.rawName(i, &buf)
	if err != nil {
		return ID{}, err
	}
	id := ID{format: x.format}
	copy(id.raw[:], raw)
	return id, nil
}

// bucket returns the positions of names starting with byte b.
func (x *packIndex) bucket(b byte) (lo, hi int) {
	if b > 0 {
		lo = int(x.fanout[b-1])
	}
	return lo, int(x.fanout[b])
}

// search returns the first position in [lo, hi) whose name is not below raw.
func (x *packIndex) search(lo, hi int, raw []byte) (int, error) {
	var buf [32]byte
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		name, err := x.rawName(mid, &buf)
		if err != nil {
			return 0, err
		}
		if bytes.Compare(name, raw) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// find returns the position of id, if present.
func (x *packIndex) find(id ID) (int, bool, error) {
	raw := id.raw[:x.format.Size()]
	lo, hi := x.bucket(raw[0])
	i, err := x.search(lo, hi, raw)
	if err != nil || i == hi {
		return 0, false, err
	}
	var buf [32]byte
	name, err := x.rawName(i, &buf)
	if err != nil {
		return 0, false, err
	}
	return i, bytes.Equal(name, raw), nil
}

// offset returns the pack offset of the object at position i.
func (x *packIndex) offset(i int) (int64, error) {
	var b [8]byte
	if x.offsets == 0 {
		if _, err := x.m.ReadAt(b[:4], x.names+int64(i)*x.stride-4); err != nil {
			return 0, err
		}
		return int64(binary.BigEndian.Uint32(b[:4])), nil
	}
	if _, err := x.m.ReadAt(b[:4], x.offsets+int64(i)*4); err != nil {
		return 0, err
	}
	off := binary.BigEndian.Uint32(b[:4])
	if off&0x80000000 == 0 {
		return int64(off), nil
	}
	li := int64(off & 0x7fffffff)
	if li >= x.nLarge {
		return 0, fmt.Errorf("pack index: large offset %d out of range", li)
	}
	if _, err := x.m.ReadAt(b[:], x.large+li*8); err != nil {
		return 0, err
	}
	big := binary.BigEndian.Uint64(b[:])
	if big > 1<<62 {
		return 0, fmt.Errorf("pack index: offset %d out of range", big)
	}
	return int64(big), nil
}

// prefix is a parsed abbreviated object name.
type prefix struct {
	raw  [32]byte // the digits as bytes, padded with zeros
	hex  int      // number of hex digits
	full string   // the lower-case digits
}

func parsePrefix(f Format, s string) (prefix, error) {
	var p prefix
	if len(s) < 4 || len(s) > f.HexSize() {
		return p, fmt.Errorf("object name prefix %q must have 4 to %d hex digits", s, f.HexSize())
	}
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'F':
			b[i] = c + 'a' - 'A'
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return p, fmt.Errorf("object name prefix %q is not hexadecimal", s)
		}
	}
	p.full, p.hex = string(b), len(b)
	if len(b)%2 == 1 {
		b = append(b, '0')
	}
	hex.Decode(p.raw[:], b)
	return p, nil
}

func (p prefix) matches(raw []byte) bool {
	n := p.hex / 2
	if !bytes.Equal(raw[:n], p.raw[:n]) {
		return false
	}
	return p.hex%2 == 0 || raw[n]>>4 == p.raw[n]>>4
}

// findPrefix calls yield with each name starting with p, in order, until it
// returns false.
func (x *packIndex) findPrefix(p prefix, yield func(ID) bool) error {
	hs := x.format.Size()
	lo, hi := x.bucket(p.raw[0])
	i, err := x.search(lo, hi, p.raw[:hs])
	if err != nil {
		return err
	}
	var buf [32]byte
	for ; i < hi; i++ {
		raw, err := x.rawName(i, &buf)
		if err != nil {
			return err
		}
		if !p.matches(raw) {
			return nil
		}
		id := ID{format: x.format}
		copy(id.raw[:], raw)
		if !yield(id) {
			return nil
		}
	}
	return nil
}
