package export

import (
	"math/bits"
	"os"
	"slices"
)

// dirList is a directory's latest listing, plus the names created through
// the export since: a row per entry with its name hash, length and offset in
// names, sorted so that the entries of one hash are adjacent. A row costs 8
// bytes beside its name, a fraction of a map entry, so the node cache holds
// several times as many listings. A name longer than maxListedName is left
// out, so its entry takes a counter id; no file system allows one anyway.
// Name hashes are uniform, so buckets by their top bits, of about four rows
// each, find a hash's rows in a few comparisons: a READDIR checks every entry
// it returns against the listing.
type dirList struct {
	rows  []uint64 // hash<<42 | length<<32 | offset
	names []byte
	// starts holds the first row of each bucket, then len(rows); nil for a
	// list short enough to scan.
	starts []uint32
}

const (
	maxListedName  = 1<<10 - 1
	maxListedBytes = 1<<32 - 1
	listOverhead   = 80 // the list: three slice headers, as allocated
	scannedRows    = 16 // the longest list without buckets
)

// listRow encodes the row of a name of hash h, length bytes at offset.
func listRow(h uint32, length, offset int) uint64 {
	return uint64(h)<<42 | uint64(length)<<32 | uint64(offset)
}

// newDirList lists the entries infos names.
func newDirList(infos []os.FileInfo) *dirList {
	size := 0
	for _, info := range infos {
		if n := len(info.Name()); n <= maxListedName {
			size += n
		}
	}
	l := &dirList{rows: make([]uint64, 0, len(infos)), names: make([]byte, 0, min(size, maxListedBytes))}
	for _, info := range infos {
		name := info.Name()
		if len(name) > maxListedName || len(l.names)+len(name) > maxListedBytes {
			continue
		}
		l.rows = append(l.rows, listRow(nameHash(name), len(name), len(l.names)))
		l.names = append(l.names, name...)
	}
	slices.Sort(l.rows)
	l.bucket()
	return l
}

// bucket indexes the rows by the top bits of their hashes, a power of two
// buckets for about four rows each.
func (l *dirList) bucket() {
	if len(l.rows) <= scannedRows {
		l.starts = nil
		return
	}
	buckets := 1 << min(bits.Len(uint(len(l.rows)/4)), hashBits)
	shift := hashBits - bits.TrailingZeros(uint(buckets))
	l.starts = slices.Grow(l.starts[:0], buckets+1)
	for i, r := range l.rows {
		for b := int(uint32(r>>42) >> shift); len(l.starts) <= b; {
			l.starts = append(l.starts, uint32(i))
		}
	}
	for len(l.starts) <= buckets {
		l.starts = append(l.starts, uint32(len(l.rows)))
	}
}

// span returns the rows of hash h, from lo up to hi.
func (l *dirList) span(h uint32) (lo, hi int) {
	end := len(l.rows)
	if n := len(l.starts); n > 0 {
		b := h >> (hashBits - bits.TrailingZeros(uint(n-1)))
		lo, end = int(l.starts[b]), int(l.starts[b+1])
	}
	for lo < end && uint32(l.rows[lo]>>42) < h {
		lo++
	}
	for hi = lo; hi < end && uint32(l.rows[hi]>>42) == h; hi++ {
	}
	return lo, hi
}

// name returns row i's name, a view of the list's bytes that callers copy
// before keeping it.
func (l *dirList) name(i int) []byte {
	r := l.rows[i]
	offset, length := int(uint32(r)), int(r>>32&maxListedName)
	return l.names[offset : offset+length]
}

// index returns the row of name, or -1.
func (l *dirList) index(name string) int {
	lo, hi := l.span(nameHash(name))
	for i := lo; i < hi; i++ {
		if string(l.name(i)) == name {
			return i
		}
	}
	return -1
}

// matches reports whether l lists exactly the entries infos names.
func (l *dirList) matches(infos []os.FileInfo) bool {
	listed := 0
	for _, info := range infos {
		if len(info.Name()) > maxListedName {
			continue
		}
		if l.index(info.Name()) < 0 {
			return false
		}
		listed++
	}
	return listed == len(l.rows)
}

// add lists name, created since the listing was read. A name colliding with
// a listed one unlists its whole hash instead: deciding between them takes a
// fresh listing.
func (l *dirList) add(name string) {
	h := nameHash(name)
	lo, hi := l.span(h)
	switch {
	case hi > lo:
		if l.index(name) >= 0 {
			return
		}
		l.rows = slices.Delete(l.rows, lo, hi)
	case len(name) <= maxListedName && len(l.names)+len(name) <= maxListedBytes:
		l.rows = slices.Insert(l.rows, lo, listRow(h, len(name), len(l.names)))
		l.names = append(l.names, name...)
	default:
		return
	}
	l.bucket()
}

// bytes is the memory l holds.
func (l *dirList) bytes() int64 {
	return int64(listOverhead + 8*cap(l.rows) + cap(l.names) + 4*cap(l.starts))
}
