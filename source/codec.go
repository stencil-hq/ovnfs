package source

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// Capture metadata (the stat cache, and the capture file bases kept before
// their capture index) is O(files), so it is stored as a small binary file: a
// magic line naming the format, the CRC-32 of the body, and the body deflated. The body lists sorted paths
// with shared-prefix encoding and then each per-path field as its own column of
// varints, mostly deltas from the previous path's value, which deflate shrinks
// to a few bytes per path. The stat cache names its paths, modes and blob ids by
// the stored tree of the capture that wrote it instead of repeating them.

// Formats of the capture metadata files; the version is part of the magic.
// Version 2 of a capture added each repository's module; version 1 is still read.
const (
	resultMagic    = "ovnfs-capture-2\n"
	resultMagicV1  = "ovnfs-capture-1\n"
	statCacheMagic = "ovnfs-stat-1\n"
)

// errCorrupt reports capture metadata that does not decode.
var errCorrupt = errors.New("corrupt capture metadata")

// UnmarshalBinary decodes the persisted fields of a capture (those with JSON
// names: root, tree, info, roots, repos, blob_repo) from the capture file of a
// base created before bases kept capture indexes, for migrating it; Files and
// the other runtime fields are left alone.
func (r *Result) UnmarshalBinary(data []byte) error {
	fields, magic := 4, resultMagic
	if bytes.HasPrefix(data, []byte(resultMagicV1)) {
		fields, magic = 3, resultMagicV1
	}
	body, err := unpack(magic, data)
	if err != nil {
		return err
	}
	d := decoder{buf: body}
	root, tree := d.str(), store.OID(d.str())
	repos := make([]Repo, d.count(fields))
	for i := range repos {
		repos[i] = Repo{Path: d.str(), Head: d.str(), Index: d.str()}
		if fields == 4 {
			repos[i].Module = d.str()
		}
	}
	roots := d.paths()
	classes := make(map[string]rules.Class, len(roots))
	for _, p := range roots {
		classes[p] = rules.Class(d.str())
	}
	taken := d.paths()
	var blobRepo map[string]string
	if len(taken) > 0 {
		blobRepo = make(map[string]string, len(taken))
		for _, p := range taken {
			blobRepo[p] = d.str()
		}
	}
	infos := d.paths()
	secs := make([]int64, len(infos))
	var prev int64
	for i := range secs {
		prev += d.varint()
		secs[i] = prev
	}
	nanos := make([]int64, len(infos))
	for i := range nanos {
		if nanos[i] = int64(d.uvarint()); nanos[i] >= int64(time.Second) {
			d.fail()
		}
	}
	info := make(map[string]Info, len(infos))
	for i, p := range infos {
		info[p] = Info{Mtime: time.Unix(secs[i], nanos[i]).UTC(), Size: d.varint()}
	}
	prev = 0
	for _, p := range infos {
		prev += d.varint()
		e := info[p]
		e.Ino = uint64(prev)
		info[p] = e
	}
	if err := d.end(); err != nil {
		return err
	}
	r.Root, r.Tree, r.Repos, r.Roots, r.BlobRepo, r.Info = root, tree, repos, classes, blobRepo, info
	return nil
}

// encodeStatCache encodes a stat cache whose entries are leaves of files, the
// capture stored as tree: which sorted leaves have an entry, as alternating run
// lengths of leaves without and with one, then the entries' stat columns.
func encodeStatCache(c statCache, tree store.OID, files map[string]store.File) ([]byte, error) {
	leaves := sortedLeaves(files)
	var e encoder
	e.str(c.Epoch)
	e.str(string(tree))
	e.uvarint(uint64(len(leaves)))
	var runs []uint64
	held := make([]statEntry, 0, len(c.Entries))
	var run uint64
	for _, p := range leaves {
		s, ok := c.Entries[p]
		if ok != (len(runs)%2 == 1) {
			runs = append(runs, run)
			run = 0
		}
		run++
		if ok {
			if f := files[p]; s.OID != f.ID || s.Mode != f.Mode {
				return nil, fmt.Errorf("stat cache: %s is not the captured leaf", p)
			}
			held = append(held, s)
		}
	}
	if len(held) != len(c.Entries) {
		return nil, errors.New("stat cache: entry outside the captured tree")
	}
	runs = append(runs, run)
	e.uvarint(uint64(len(runs)))
	for _, n := range runs {
		e.uvarint(n)
	}
	for _, s := range held {
		e.varint(s.Size)
	}
	for _, field := range []func(statEntry) int64{
		func(s statEntry) int64 { return s.Mtime },
		func(s statEntry) int64 { return s.Ctime },
		func(s statEntry) int64 { return int64(s.Ino) },
	} {
		var prev int64
		for _, s := range held {
			v := field(s)
			e.varint(v - prev)
			prev = v
		}
	}
	return pack(statCacheMagic, e.buf)
}

// decodeStatCache decodes a file written by encodeStatCache, reading the paths,
// modes and blob ids of its entries from the capture's tree in trees.
func decodeStatCache(data []byte, trees *store.Store) (statCache, error) {
	body, err := unpack(statCacheMagic, data)
	if err != nil {
		return statCache{}, err
	}
	d := decoder{buf: body}
	epoch := d.str()
	tree := store.OID(d.str())
	count := d.uvarint()
	runs := make([]uint64, d.count(1))
	var total uint64
	for i := range runs {
		runs[i] = d.uvarint()
		if runs[i] > count-total {
			d.fail()
			break
		}
		total += runs[i]
	}
	if d.err != nil || total != count {
		return statCache{}, errCorrupt
	}
	files, err := trees.Flatten(tree)
	if err != nil {
		return statCache{}, err
	}
	leaves := sortedLeaves(files)
	if uint64(len(leaves)) != count {
		return statCache{}, errCorrupt
	}
	var paths []string
	start := 0
	for i, n := range runs {
		if i%2 == 1 {
			paths = append(paths, leaves[start:start+int(n)]...)
		}
		start += int(n)
	}
	entries := make([]statEntry, len(paths))
	for i := range entries {
		entries[i].Size = d.varint()
	}
	var prev int64
	for i := range entries {
		prev += d.varint()
		entries[i].Mtime = prev
	}
	prev = 0
	for i := range entries {
		prev += d.varint()
		entries[i].Ctime = prev
	}
	prev = 0
	for i := range entries {
		prev += d.varint()
		entries[i].Ino = uint64(prev)
	}
	if err := d.end(); err != nil {
		return statCache{}, err
	}
	c := statCache{Epoch: epoch, Entries: make(map[string]statEntry, len(paths))}
	for i, p := range paths {
		f := files[p]
		entries[i].Mode, entries[i].OID = f.Mode, f.ID
		c.Entries[p] = entries[i]
	}
	return c, nil
}

// sortedLeaves lists the paths of a flattened tree's blobs and symlinks in order.
func sortedLeaves(files map[string]store.File) []string {
	leaves := make([]string, 0, len(files))
	for p, f := range files {
		if f.Mode != store.ModeDir {
			leaves = append(leaves, p)
		}
	}
	slices.Sort(leaves)
	return leaves
}

// pack frames an encoded body: magic, CRC-32 of the body, deflated body.
func pack(magic string, body []byte) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(magic)
	out.Write(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(body)))
	z, err := flate.NewWriter(&out, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := z.Write(body); err != nil {
		return nil, err
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// unpack checks a frame written by pack and returns its body.
func unpack(magic string, data []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(data, []byte(magic))
	if !ok || len(rest) < 4 {
		return nil, errCorrupt
	}
	sum := binary.LittleEndian.Uint32(rest)
	body, err := io.ReadAll(flate.NewReader(bytes.NewReader(rest[4:])))
	if err != nil || crc32.ChecksumIEEE(body) != sum {
		return nil, errCorrupt
	}
	return body, nil
}

// encoder appends varints, strings and path lists to buf.
type encoder struct{ buf []byte }

func (e *encoder) uvarint(v uint64) { e.buf = binary.AppendUvarint(e.buf, v) }

func (e *encoder) varint(v int64) { e.buf = binary.AppendVarint(e.buf, v) }

func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.buf = append(e.buf, s...)
}

// paths writes sorted, distinct paths, each as the length it shares with the
// previous one and the rest.
func (e *encoder) paths(ps []string) {
	e.uvarint(uint64(len(ps)))
	prev := ""
	for _, p := range ps {
		n := 0
		for n < len(p) && n < len(prev) && p[n] == prev[n] {
			n++
		}
		e.uvarint(uint64(n))
		e.str(p[n:])
		prev = p
	}
}

// decoder reads what encoder wrote; the first malformed value sets err and
// every later read returns zero values.
type decoder struct {
	buf []byte
	err error
}

func (d *decoder) fail() {
	if d.err == nil {
		d.err = errCorrupt
	}
	d.buf = nil
}

func (d *decoder) uvarint() uint64 {
	v, n := binary.Uvarint(d.buf)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

func (d *decoder) varint() int64 {
	v, n := binary.Varint(d.buf)
	if n <= 0 {
		d.fail()
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

// take returns the next n bytes.
func (d *decoder) take(n int) []byte {
	if n < 0 || n > len(d.buf) {
		d.fail()
		return nil
	}
	b := d.buf[:n]
	d.buf = d.buf[n:]
	return b
}

func (d *decoder) str() string {
	n := d.uvarint()
	if n > uint64(len(d.buf)) {
		d.fail()
		return ""
	}
	return string(d.take(int(n)))
}

// count reads an element count, rejecting counts the remaining input cannot
// hold at minBytes per element so corrupt input cannot force a huge allocation.
func (d *decoder) count(minBytes int) int {
	n := d.uvarint()
	if n > uint64(len(d.buf)/minBytes) {
		d.fail()
		return 0
	}
	return int(n)
}

// paths reads a path list written by encoder.paths, which must be strictly increasing.
func (d *decoder) paths() []string {
	ps := make([]string, d.count(2))
	var b strings.Builder
	prev := ""
	for i := range ps {
		n := d.uvarint()
		rest := d.str()
		if d.err != nil || n > uint64(len(prev)) {
			d.fail()
			return nil
		}
		b.Reset()
		b.Grow(int(n) + len(rest))
		b.WriteString(prev[:n])
		b.WriteString(rest)
		p := b.String()
		if i > 0 && p <= prev {
			d.fail()
			return nil
		}
		ps[i], prev = p, p
	}
	return ps
}

// end reports a decode error or input left over after the last field.
func (d *decoder) end() error {
	if d.err == nil && len(d.buf) > 0 {
		return errCorrupt
	}
	return d.err
}
