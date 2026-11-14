package base

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/internal/mapped"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// A base keeps its capture in captureIndexFile: every captured path with its
// mode, blob id and stat data, the classes of non-source roots, and which
// paths each repository tracks. It is written once when the base is created and
// then memory-mapped read-only, so per-path state is clean page cache rather
// than heap, and a lookup touches only the pages it reads.
//
// Layout, little endian:
//
//	header  magic; node count u32; CRC-32 of meta u32; paths offset, paths
//	        length, meta offset, meta length u64; log2 of the table's slots
//	        u32; 4 reserved bytes
//	nodes   nodeSize bytes per node, breadth first from the root (node 0):
//	        path offset u32, path length u32, name length u16 (the name ends
//	        the path), kind u8, class u8 (index into rootClasses), first child
//	        u32, child count u32, parent u32. A node's children are
//	        consecutive, after it, and sorted by name, so a listing is a range
//	        and a path can also be found one directory at a time.
//	attrs   attrSize bytes per node, same order: blob id [20]; store mode
//	        u32, with the tracking repository (1-based into meta's list, 0:
//	        untracked) above attrTrackedShift and attrLeaf and attrOID in the
//	        top bits; blob repository u32
//	        (1-based into meta's list, 0: none); nanoseconds u32, with attrInfo and attrMtime in the
//	        top bits; seconds i64; size i64; inode u64
//	table   open-addressed hash table of every node's path (pathHash),
//	        probed linearly: per slot, node+1 u32 (0: empty) and the hash's
//	        top 32 bits. An exact lookup costs a hash, a slot and one path
//	        comparison instead of a binary search per directory.
//	paths   the nodes' full root-relative paths, concatenated
//	meta    varints: root; tree; repositories (path, head, index, module);
//	        blob repository names; roots (sorted paths, each with its class)
//
// A path the capture holds no source at (kindNone) is in the index only as a
// root, a path with stat data, a tracked path, or an ancestor of one; to a
// view it does not exist unless it is a root.
const (
	captureIndexFile = "capture.idx"
	captureMagic     = "ovnfs-capture-3\n"
	headerSize       = 64
	nodeSize         = 24
	attrSize         = 56
	nameLimit        = 1<<16 - 1
	sectionLimit     = 1<<32 - 1
)

// Capture files of bases created before captureIndexFile: the binary capture
// (source.Result.UnmarshalBinary) and, before it, JSON. Open migrates them.
const (
	legacyCaptureFile = "capture.bin"
	jsonCaptureFile   = "capture.json"
)

// Node kinds: what the capture serves at a path. Paths only a root or stat data
// names, and their ancestors that hold no source, are kindNone.
const (
	kindNone byte = iota
	kindDir
	kindFile
	kindSymlink
)

// Fields above the store mode bits of an attr's mode, and flags in the top bits
// of its nanoseconds.
const (
	attrLeaf         = 1 << 30 // mode: a captured leaf (source.Result.Files)
	attrOID          = 1 << 31 // mode: the leaf has a blob id
	attrModes        = 1<<attrTrackedShift - 1
	attrTrackedShift = 16 // mode: the repository tracking the path, or a path below it
	attrTrackedMax   = attrLeaf>>attrTrackedShift - 1
	attrInfo         = 1 << 31 // nanoseconds: the capture recorded stat data
	attrMtime        = 1 << 30 // nanoseconds: ... with an mtime
	attrNanos        = attrMtime - 1
)

// rootClasses numbers the classes of non-source roots; 0 is none.
var rootClasses = [...]rules.Class{"", rules.Cache, rules.Local, rules.Hidden}

// captureIndex is an open captureIndexFile. It owns the mapping, which is
// unmapped once the index is unreachable (see openCaptureIndex), so a reader
// can never fault on it: every method keeps x alive (runtime.KeepAlive) until
// its last read through the mapping, and returns copies, never slices of it.
type captureIndex struct {
	data    []byte
	m       *mapping
	nodes   uint32
	table   []byte
	mask    uint64 // slots in table - 1
	paths   []byte
	root    string
	tree    store.OID
	repos   []source.Repo
	origins []string // blob repositories
	roots   []byte   // meta's encoded roots, decoded on demand
}

// openCaptureIndex maps the capture index of a base and checks that it holds
// the capture of tree. A missing file is os.ErrNotExist.
func openCaptureIndex(stateDir string, tree store.OID) (*captureIndex, error) {
	data, unmap, err := mapped.Map(filepath.Join(stateDir, captureIndexFile))
	if err != nil {
		return nil, err
	}
	x, err := parseCaptureIndex(data)
	if err == nil && x.tree != tree {
		err = fmt.Errorf("capture of tree %s, not the base's %s", x.tree, tree)
	}
	if err != nil {
		unmap()
		return nil, fmt.Errorf("%s: %w", captureIndexFile, err)
	}
	x.m = &mapping{unmap: unmap}
	runtime.AddCleanup(x, (*mapping).release, x.m)
	return x, nil
}

// mapping is what unmapping an index needs; it must not reach the index.
type mapping struct {
	unmap func() error
	// orphan is the state directory of a removed base, deleted once the index
	// is unmapped: Windows cannot delete a mapped file.
	orphan atomic.Pointer[string]
}

// release unmaps an unreachable index and finishes the removal of its base.
func (m *mapping) release() {
	m.unmap()
	if dir := m.orphan.Load(); dir != nil {
		os.RemoveAll(*dir)
	}
}

// errCorruptIndex reports a capture index that does not decode.
var errCorruptIndex = errors.New("corrupt capture index")

// parseCaptureIndex checks the header and decodes the meta of a capture index.
// Node and attr fields are bounds-checked as they are read.
func parseCaptureIndex(data []byte) (*captureIndex, error) {
	if len(data) < headerSize || string(data[:len(captureMagic)]) != captureMagic {
		return nil, errCorruptIndex
	}
	le := binary.LittleEndian
	nodes := le.Uint32(data[16:])
	sum := le.Uint32(data[20:])
	pathsOff, pathsLen := le.Uint64(data[24:]), le.Uint64(data[32:])
	metaOff, metaLen := le.Uint64(data[40:]), le.Uint64(data[48:])
	tableBits := le.Uint32(data[56:])
	size := uint64(len(data))
	tableOff := headerSize + uint64(nodes)*(nodeSize+attrSize)
	if nodes == 0 || tableBits > 40 || pathsOff != tableOff+8<<tableBits || pathsOff > size || pathsLen > size-pathsOff ||
		metaLen > size || metaOff != size-metaLen || metaOff != pathsOff+pathsLen {
		return nil, errCorruptIndex
	}
	meta := data[metaOff:]
	if crc32.ChecksumIEEE(meta) != sum {
		return nil, errCorruptIndex
	}
	x := &captureIndex{data: data, nodes: nodes, table: data[tableOff:pathsOff], mask: 1<<tableBits - 1, paths: data[pathsOff : pathsOff+pathsLen]}
	d := metaDecoder{buf: meta}
	x.root, x.tree = d.str(), store.OID(d.str())
	x.repos = make([]source.Repo, d.count())
	for i := range x.repos {
		x.repos[i] = source.Repo{Path: d.str(), Head: d.str(), Index: d.str(), Module: d.str()}
	}
	x.origins = make([]string, d.count())
	for i := range x.origins {
		x.origins[i] = d.str()
	}
	x.roots = d.buf
	if d.err != nil {
		return nil, d.err
	}
	return x, nil
}

// trackedBy answers source.Tracked for one repository from the index.
type trackedBy struct {
	x    *captureIndex
	repo uint32 // 1-based into x.repos; 0 tracks nothing
}

// Tracked looks p up and checks the repository that tracks it.
func (t trackedBy) Tracked(p string) bool {
	n, ok := t.x.lookup(p)
	return ok && n != 0 && t.repo != 0 && t.x.mode(n)>>attrTrackedShift&attrTrackedMax == t.repo
}

// trackedOf answers Tracked for the repository at root-relative path repo.
func (x *captureIndex) trackedOf(repo string) source.Tracked {
	i := slices.IndexFunc(x.repos, func(r source.Repo) bool { return r.Path == repo })
	return trackedBy{x: x, repo: uint32(i + 1)}
}

// The readers below take a node below x.nodes, which parseCaptureIndex
// bounds the node and attr records by. Each reads the mapping through x.data
// and keeps x alive past the read.

// nodeAt and attrAt return the offsets of node n's records in x.data.
func nodeAt(n uint32) int { return headerSize + int(n)*nodeSize }
func (x *captureIndex) attrAt(n uint32) int {
	return headerSize + int(x.nodes)*nodeSize + int(n)*attrSize
}

func (x *captureIndex) u8(off int) byte {
	v := x.data[off]
	runtime.KeepAlive(x)
	return v
}

func (x *captureIndex) u16(off int) uint16 {
	v := binary.LittleEndian.Uint16(x.data[off:])
	runtime.KeepAlive(x)
	return v
}

func (x *captureIndex) u32(off int) uint32 {
	v := binary.LittleEndian.Uint32(x.data[off:])
	runtime.KeepAlive(x)
	return v
}

func (x *captureIndex) u64(off int) uint64 {
	v := binary.LittleEndian.Uint64(x.data[off:])
	runtime.KeepAlive(x)
	return v
}

// span returns the offsets in x.paths of node n's path and of its name, which ends it.
func (x *captureIndex) span(n uint32) (start, name, end int) {
	off, length, nameLen := uint64(x.u32(nodeAt(n))), uint64(x.u32(nodeAt(n)+4)), uint64(x.u16(nodeAt(n)+8))
	if off+length > uint64(len(x.paths)) || nameLen > length {
		return 0, 0, 0
	}
	return int(off), int(off + length - nameLen), int(off + length)
}

// nameOf returns a copy of node n's name.
func (x *captureIndex) nameOf(n uint32) string {
	_, name, end := x.span(n)
	s := string(x.paths[name:end])
	runtime.KeepAlive(x)
	return s
}

// compareName compares node n's name with s.
func (x *captureIndex) compareName(n uint32, s string) int {
	_, name, end := x.span(n)
	c := strings.Compare(string(x.paths[name:end]), s)
	runtime.KeepAlive(x)
	return c
}

// pathIs reports whether node n's path is p.
func (x *captureIndex) pathIs(n uint32, p string) bool {
	start, _, end := x.span(n)
	is := string(x.paths[start:end]) == p
	runtime.KeepAlive(x)
	return is
}

// kind returns what the capture serves at node n.
func (x *captureIndex) kind(n uint32) byte { return x.u8(nodeAt(n) + 10) }

// class returns node n's root class, "" when it is no root.
func (x *captureIndex) class(n uint32) rules.Class {
	if c := int(x.u8(nodeAt(n) + 11)); c < len(rootClasses) {
		return rootClasses[c]
	}
	return ""
}

// children returns the range of node n's children. Children always follow
// their parent, so walking down ends even in a damaged index.
func (x *captureIndex) children(n uint32) (first, end uint32) {
	first, count := x.u32(nodeAt(n)+12), x.u32(nodeAt(n)+16)
	if count == 0 || first <= n || first >= x.nodes || count > x.nodes-first {
		return 0, 0
	}
	return first, first + count
}

// child finds the child of dir named name.
func (x *captureIndex) child(dir uint32, name string) (uint32, bool) {
	lo, hi := x.children(dir)
	for lo < hi {
		m := lo + (hi-lo)/2
		switch c := x.compareName(m, name); {
		case c == 0:
			return m, true
		case c < 0:
			lo = m + 1
		default:
			hi = m
		}
	}
	return 0, false
}

// parent returns node n's parent; the root is its own.
func (x *captureIndex) parent(n uint32) uint32 {
	if p := x.u32(nodeAt(n) + 20); p < n {
		return p
	}
	return 0
}

// lookup finds the node of clean root-relative path p in the hash table.
func (x *captureIndex) lookup(p string) (uint32, bool) {
	if p == "" {
		return 0, true
	}
	h := pathHash(p)
	tag := uint32(h >> 32)
	for i, probes := h&x.mask, uint64(0); probes <= x.mask; i, probes = (i+1)&x.mask, probes+1 {
		e := binary.LittleEndian.Uint64(x.table[8*i:])
		if e == 0 {
			break
		}
		if uint32(e>>32) == tag {
			if n := uint32(e) - 1; n < x.nodes && x.pathIs(n, p) {
				return n, true
			}
		}
	}
	runtime.KeepAlive(x)
	return 0, false
}

// pathHash is the table's hash of a path: a multiply-xor over 8-byte words
// with a final avalanche. It is part of the file format and must not change.
func pathHash(p string) uint64 {
	const k = 0x9e3779b97f4a7c15
	h := uint64(len(p)) * k
	for ; len(p) >= 8; p = p[8:] {
		w := uint64(p[0]) | uint64(p[1])<<8 | uint64(p[2])<<16 | uint64(p[3])<<24 | uint64(p[4])<<32 | uint64(p[5])<<40 | uint64(p[6])<<48 | uint64(p[7])<<56
		h = (h ^ w) * k
		h ^= h >> 32
	}
	var w uint64
	for i := range len(p) {
		w |= uint64(p[i]) << (8 * i)
	}
	h = (h ^ w) * k
	// murmur3's fmix64
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// mode returns node n's attr mode word: the store mode and the fields above it.
func (x *captureIndex) mode(n uint32) uint32 { return x.u32(x.attrAt(n) + 20) }

// leaf returns node n's captured leaf (source.Result.Files); ok is false for other nodes.
func (x *captureIndex) leaf(n uint32) (f store.File, ok bool) {
	mode := x.mode(n)
	if mode&attrLeaf == 0 {
		return store.File{}, false
	}
	return store.File{Mode: mode & attrModes, ID: x.oid(n)}, true
}

// oid returns the blob id of node n, "" when it has none.
func (x *captureIndex) oid(n uint32) store.OID {
	if x.mode(n)&attrOID == 0 {
		return ""
	}
	at := x.attrAt(n)
	id := store.OID(hex.EncodeToString(x.data[at : at+20]))
	runtime.KeepAlive(x)
	return id
}

// info returns the stat data the capture recorded for node n; ok is false when there is none.
func (x *captureIndex) info(n uint32) (source.Info, bool) {
	at := x.attrAt(n)
	nanos := x.u32(at + 28)
	if nanos&attrInfo == 0 {
		return source.Info{}, false
	}
	info := source.Info{Size: int64(x.u64(at + 40)), Ino: x.u64(at + 48)}
	if nanos&attrMtime != 0 {
		info.Mtime = time.Unix(int64(x.u64(at+32)), int64(nanos&attrNanos)).UTC()
	}
	return info, true
}

// origin returns the root-relative repository holding node n's blob, when a take overrode it.
func (x *captureIndex) origin(n uint32) (string, bool) {
	i := x.u32(x.attrAt(n) + 24)
	if i == 0 || int(i) > len(x.origins) {
		return "", false
	}
	return x.origins[i-1], true
}

// fileMode is the mode a view presents for node n.
func (x *captureIndex) fileMode(n uint32) os.FileMode {
	switch x.kind(n) {
	case kindDir:
		return os.ModeDir | 0o755
	case kindSymlink:
		return os.ModeSymlink | 0o777
	}
	if x.mode(n)&attrModes == store.ModeExec {
		return 0o755
	}
	return 0o644
}

// walk calls fn with every node below the root and its path, depth first in
// name order, until fn returns false.
func (x *captureIndex) walk(fn func(p string, n uint32) bool) {
	type frame struct {
		dir       string
		next, end uint32
	}
	first, end := x.children(0)
	stack := []frame{{next: first, end: end}}
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		if f.next == f.end {
			stack = stack[:len(stack)-1]
			continue
		}
		n := f.next
		f.next++
		p := x.nameOf(n)
		if f.dir != "" {
			p = f.dir + "/" + p
		}
		if !fn(p, n) {
			return
		}
		if first, end := x.children(n); first < end {
			stack = append(stack, frame{dir: p, next: first, end: end})
		}
	}
}

// eachRoot calls fn with every non-source root and its class, in path order.
func (x *captureIndex) eachRoot(fn func(p string, class rules.Class)) {
	defer runtime.KeepAlive(x) // d reads the mapping
	d := metaDecoder{buf: x.roots}
	for n, prev := d.count(), ""; n > 0 && d.err == nil; n-- {
		shared := d.uvarint()
		rest := d.str()
		class := d.uvarint()
		if shared > uint64(len(prev)) || class >= uint64(len(rootClasses)) {
			return
		}
		prev = prev[:shared] + rest
		fn(prev, rootClasses[class])
	}
}

// writeCaptureIndex writes res's capture index for a base into stateDir and
// publishes it atomically. Tracked paths come from res.Domains, which must
// still hold the indexes it read (see source.Domains.Release).
func writeCaptureIndex(stateDir string, res *source.Result) error {
	tree, err := buildCaptureTree(res)
	if err != nil {
		return fmt.Errorf("write %s: %w", captureIndexFile, err)
	}
	f, err := os.CreateTemp(stateDir, ".capture-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = tree.write(f, res)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = durable.Sync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", captureIndexFile, err)
	}
	if err := os.Rename(f.Name(), filepath.Join(stateDir, captureIndexFile)); err != nil {
		return err
	}
	return syncDirectory(stateDir)
}

// captureTree is a capture's paths laid out breadth first, while it is written.
type captureTree struct {
	paths  []string // sorted, "" first
	order  []int32  // breadth-first position → index into paths
	first  []int32  // by index into paths: position of the first child
	count  []int32  // by index into paths: number of children
	up     []int32  // by index into paths: position of the parent
	source []bool   // by index into paths: a captured leaf or an ancestor of one
	repo   []uint16 // by index into paths: the tracking repository, 1-based into res.Repos
}

// buildCaptureTree collects every path res names, with its ancestors, and lays
// them out breadth first with each directory's children consecutive.
func buildCaptureTree(res *source.Result) (*captureTree, error) {
	seen := make(map[string]struct{}, len(res.Files)+len(res.Roots))
	add := func(p string) {
		for ; p != ""; p = parentOf(p) {
			if _, ok := seen[p]; ok {
				return
			}
			seen[p] = struct{}{}
		}
	}
	for p := range res.Files {
		add(p)
	}
	for p := range res.Info {
		add(p)
	}
	for p := range res.Roots {
		add(p)
	}
	for p := range res.BlobRepo {
		add(p)
	}
	if res.Domains != nil {
		for p := range res.Domains.Tracked() {
			add(p)
		}
	}
	t := &captureTree{paths: make([]string, 1, len(seen)+1)}
	for p := range seen {
		t.paths = append(t.paths, p)
	}
	clear(seen)
	slices.Sort(t.paths[1:])
	n := len(t.paths)
	parents := make([]int32, n)
	t.count = make([]int32, n)
	t.source = make([]bool, n)
	t.source[0] = true
	for i := 1; i < n; i++ {
		j, _ := slices.BinarySearch(t.paths, parentOf(t.paths[i]))
		parents[i] = int32(j)
		t.count[j]++
	}
	for i, p := range t.paths {
		if _, ok := res.Files[p]; ok {
			for j := int32(i); !t.source[j]; j = parents[j] {
				t.source[j] = true
			}
		}
	}
	t.repo = make([]uint16, n)
	if res.Domains != nil {
		ids := map[string]uint16{}
		for i, r := range res.Repos {
			if i >= attrTrackedMax {
				return nil, fmt.Errorf("more than %d repositories", attrTrackedMax)
			}
			ids[r.Path] = uint16(i + 1)
		}
		for p, repo := range res.Domains.Tracked() {
			j, _ := slices.BinarySearch(t.paths, p)
			t.repo[j] = ids[repo]
		}
	}
	// Bucket children by parent, in path order (which is name order), then
	// number them breadth first.
	start := make([]int32, n+1)
	for i := range n {
		start[i+1] = start[i] + t.count[i]
	}
	children := make([]int32, n)
	fill := slices.Clone(start[:n])
	for i := 1; i < n; i++ {
		children[fill[parents[i]]] = int32(i)
		fill[parents[i]]++
	}
	t.order = make([]int32, 1, n)
	t.first = make([]int32, n)
	for k := 0; k < len(t.order); k++ {
		i := t.order[k]
		t.first[i] = int32(len(t.order))
		t.order = append(t.order, children[start[i]:start[i+1]]...)
	}
	pos := children // reused: by index into paths, position
	for k, i := range t.order {
		pos[i] = int32(k)
	}
	t.up = parents
	for i := 1; i < n; i++ {
		t.up[i] = pos[parents[i]]
	}
	return t, nil
}

// write streams the index to f.
func (t *captureTree) write(f *os.File, res *source.Result) error {
	if _, err := f.Write(make([]byte, headerSize)); err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	le := binary.LittleEndian
	var rec [attrSize]byte
	var pathsLen uint64
	for _, i := range t.order {
		p := t.paths[i]
		name := p[strings.LastIndexByte(p, '/')+1:]
		if len(name) > nameLimit {
			return fmt.Errorf("name of %q is too long", p)
		}
		kind := kindNone
		if leaf, ok := res.Files[p]; ok {
			kind = kindOf(leaf.Mode)
		} else if t.source[i] {
			kind = kindDir
		}
		class := 0
		if c, ok := res.Roots[p]; ok {
			if class = slices.Index(rootClasses[:], c); class <= 0 {
				return fmt.Errorf("root %q has class %q", p, c)
			}
		}
		first := uint32(0)
		if t.count[i] > 0 {
			first = uint32(t.first[i])
		}
		r := rec[:nodeSize]
		le.PutUint32(r, uint32(pathsLen))
		le.PutUint32(r[4:], uint32(len(p)))
		le.PutUint16(r[8:], uint16(len(name)))
		r[10], r[11] = kind, byte(class)
		le.PutUint32(r[12:], first)
		le.PutUint32(r[16:], uint32(t.count[i]))
		le.PutUint32(r[20:], uint32(t.up[i]))
		if _, err := w.Write(r); err != nil {
			return err
		}
		if pathsLen += uint64(len(p)); pathsLen > sectionLimit {
			return errors.New("too many paths")
		}
	}
	origins := map[string]uint32{}
	var originList []string
	for _, i := range t.order {
		p := t.paths[i]
		clear(rec[:])
		mode := uint32(t.repo[i]) << attrTrackedShift
		if leaf, ok := res.Files[p]; ok {
			mode |= leaf.Mode | attrLeaf
			if leaf.Mode&^attrModes != 0 {
				return fmt.Errorf("%q has mode %o", p, leaf.Mode)
			}
			if leaf.ID != "" {
				if _, err := hex.Decode(rec[:20], []byte(leaf.ID)); err != nil || !leaf.ID.Valid() {
					return fmt.Errorf("%q has blob id %q", p, leaf.ID)
				}
				mode |= attrOID
			}
		}
		le.PutUint32(rec[20:], mode)
		if origin, ok := res.BlobRepo[p]; ok {
			if origins[origin] == 0 {
				originList = append(originList, origin)
				origins[origin] = uint32(len(originList))
			}
			le.PutUint32(rec[24:], origins[origin])
		}
		if info, ok := res.Info[p]; ok {
			nanos := uint32(attrInfo)
			if !info.Mtime.IsZero() {
				nanos |= attrMtime | uint32(info.Mtime.Nanosecond())
				le.PutUint64(rec[32:], uint64(info.Mtime.Unix()))
			}
			le.PutUint32(rec[28:], nanos)
			le.PutUint64(rec[40:], uint64(info.Size))
			le.PutUint64(rec[48:], info.Ino)
		}
		if _, err := w.Write(rec[:]); err != nil {
			return err
		}
	}
	bits := t.tableBits()
	table := make([]uint64, 1<<bits)
	mask := uint64(len(table) - 1)
	for k, i := range t.order[1:] {
		h := pathHash(t.paths[i])
		j := h & mask
		for table[j] != 0 {
			j = (j + 1) & mask
		}
		table[j] = h>>32<<32 | uint64(k+2) // node k+1, plus one
	}
	for _, e := range table {
		if _, err := w.Write(le.AppendUint64(rec[:0], e)); err != nil {
			return err
		}
	}
	for _, i := range t.order {
		if _, err := w.WriteString(t.paths[i]); err != nil {
			return err
		}
	}
	pathsOff := uint64(headerSize) + uint64(len(t.order))*(nodeSize+attrSize) + 8<<bits
	metaOff := pathsOff + pathsLen
	var meta metaEncoder
	meta.str(res.Root)
	meta.str(string(res.Tree))
	meta.uvarint(uint64(len(res.Repos)))
	for _, r := range res.Repos {
		meta.str(r.Path)
		meta.str(r.Head)
		meta.str(r.Index)
		meta.str(r.Module)
	}
	meta.uvarint(uint64(len(originList)))
	for _, o := range originList {
		meta.str(o)
	}
	roots := source.SortedPaths(res.Roots)
	meta.uvarint(uint64(len(roots)))
	prev := ""
	for _, p := range roots {
		shared := 0
		for shared < len(p) && shared < len(prev) && p[shared] == prev[shared] {
			shared++
		}
		meta.uvarint(uint64(shared))
		meta.str(p[shared:])
		meta.uvarint(uint64(slices.Index(rootClasses[:], res.Roots[p])))
		prev = p
	}
	if _, err := w.Write(meta.buf); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var h [headerSize]byte
	copy(h[:], captureMagic)
	le.PutUint32(h[16:], uint32(len(t.order)))
	le.PutUint32(h[20:], crc32.ChecksumIEEE(meta.buf))
	le.PutUint64(h[24:], pathsOff)
	le.PutUint64(h[32:], pathsLen)
	le.PutUint64(h[40:], metaOff)
	le.PutUint64(h[48:], uint64(len(meta.buf)))
	le.PutUint32(h[56:], uint32(bits))
	_, err := f.WriteAt(h[:], 0)
	return err
}

// tableBits sizes the hash table for at most 70% of its slots in use.
func (t *captureTree) tableBits() int {
	bits := 1
	for (1<<bits)*7/10 < len(t.order) {
		bits++
	}
	return bits
}

// kindOf maps a captured leaf's store mode to its node kind.
func kindOf(mode uint32) byte {
	switch mode {
	case store.ModeDir:
		return kindDir
	case store.ModeSymlink:
		return kindSymlink
	}
	return kindFile
}

// parentOf returns a root-relative path's parent, "" for top-level names.
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// metaEncoder appends varints and strings.
type metaEncoder struct{ buf []byte }

func (e *metaEncoder) uvarint(v uint64) { e.buf = binary.AppendUvarint(e.buf, v) }

func (e *metaEncoder) str(s string) {
	e.uvarint(uint64(len(s)))
	e.buf = append(e.buf, s...)
}

// metaDecoder reads what metaEncoder wrote; the first malformed value sets err
// and every later read returns zero values.
type metaDecoder struct {
	buf []byte
	err error
}

func (d *metaDecoder) uvarint() uint64 {
	v, n := binary.Uvarint(d.buf)
	if n <= 0 {
		d.err, d.buf = errCorruptIndex, nil
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

func (d *metaDecoder) str() string {
	n := d.uvarint()
	if n > uint64(len(d.buf)) {
		d.err, d.buf = errCorruptIndex, nil
		return ""
	}
	s := string(d.buf[:n])
	d.buf = d.buf[n:]
	return s
}

// count reads an element count no larger than the bytes left, so damaged input
// cannot force a huge allocation.
func (d *metaDecoder) count() int {
	n := d.uvarint()
	if n > uint64(len(d.buf)) {
		d.err, d.buf = errCorruptIndex, nil
		return 0
	}
	return int(n)
}
