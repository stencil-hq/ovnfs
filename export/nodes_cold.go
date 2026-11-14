package export

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// Cold identities retain their exact names in a disk index, beside the hashes
// of retired names. A listing alone cannot reconstruct an issued id after an
// external removal or a new name with the same hash. The index is not the
// identity log: a clean close completes it for the next open to adopt
// (nodes_reclaim.go), while a crash discards it and handles issued before it
// resolve by listing again.
//
// A directory's index is a chain of segments in its instance's pack, newest
// first. A segment is a sorted array of 16-byte (hash, name length, offset)
// rows, preceded by their count and followed by name bytes; a zero length
// marks a retired hash. A segment that extends an earlier one ends with a
// link row of linkHash, which no name has, giving the earlier segment's size
// and offset; a newer row overrides an older one of its hash. Segments are
// only ever appended: a spill appends the owners and retired hashes the
// chain lacks, then they leave memory, or merges the chain into one segment
// once its extensions outgrow its base, so a directory's segments, live and
// superseded, stay within a few times its live size. Lookups binary-search
// the chain through a memory map of the pack under the node mutex: a cold
// name costs a few page reads and no heap. Resident state overrides the
// chain, whose owners a known-name operation admits one hash at a time;
// generated-name mutations retain every owner in their destination instead.
// Segments superseded by merges stay in the pack until an open rebuilds it
// (rebuildReason).

// errOwnerIndex reports an owner index segment that contradicts itself.
var errOwnerIndex = errors.New("invalid owner index")

const (
	// linkHash is the hash of the link row, above every name hash.
	linkHash = math.MaxUint32
	// maxChain bounds the segments a directory's index spans.
	maxChain = 4
	// packChunk is the least a pack's file grows by, so that its mapping is
	// replaced only every so many appends.
	packChunk = 1 << 20
)

// ownerPack is the file owner index segments are appended to. Appends claim
// their range under the node mutex and write it outside; the mapping lookups
// read through is replaced under the mutex only, so a view taken under the
// mutex stays valid until the mutex is released.
type ownerPack struct {
	path   string
	file   *os.File     // appends and close's flush
	view   *mapped.File // the file when last mapped; nil until a lookup needs it
	end    int64        // bytes claimed
	size   int64        // the file's length, at least end
	closed bool
}

// openPack opens the pack at path, appending after what it holds.
func openPack(path string) (*ownerPack, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &ownerPack{path: path, file: f, end: info.Size(), size: info.Size()}, nil
}

// createPack starts the pack of t's instance, creating the instance first.
func (t *nodeTable) createPack() (*ownerPack, error) {
	if t.coldRoot == "" {
		root, err := os.MkdirTemp(t.coldParent, "instance-")
		if err != nil {
			return nil, err
		}
		t.coldRoot = root
	}
	f, err := os.CreateTemp(t.coldRoot, "pack-")
	if err != nil {
		return nil, err
	}
	t.pack = &ownerPack{path: f.Name(), file: f}
	return t.pack, nil
}

// reserve claims n bytes at the end of the pack for a write outside the
// mutex, growing the file ahead of its use.
func (p *ownerPack) reserve(n int64) (int64, error) {
	if p.closed {
		return 0, os.ErrClosed
	}
	base := p.end
	if base+n > p.size {
		size := max(base+n, p.size+p.size/2, packChunk)
		p.unmap() // Windows cannot resize a mapped file
		if err := p.file.Truncate(size); err != nil {
			return 0, err
		}
		p.size = size
	}
	p.end = base + n
	return base, nil
}

// segment returns the segment at o, under mu: a view of the mapping, or a
// copy where the pack cannot be mapped.
func (p *ownerPack) segment(o nodeOwner) ([]byte, error) {
	if p.closed {
		return nil, os.ErrClosed
	}
	if o.base < 0 || o.size < 4 || o.base > p.end-o.size {
		return nil, errOwnerIndex
	}
	if p.view == nil || o.base+o.size > p.view.Size() {
		p.unmap()
		view, err := mapped.Open(p.path)
		if err != nil {
			return nil, err
		}
		p.view = view
		if o.base+o.size > view.Size() {
			return nil, errOwnerIndex
		}
	}
	return p.view.Slice(o.base, o.size)
}

// unmap drops the mapping; the next lookup maps the file again.
func (p *ownerPack) unmap() {
	if p.view != nil {
		p.view.Close()
		p.view = nil
	}
}

// finish trims the pack to the bytes appended and makes it durable, at close.
func (p *ownerPack) finish() error {
	if p.size != p.end {
		p.unmap()
		if err := p.file.Truncate(p.end); err != nil {
			return err
		}
		p.size = p.end
	}
	return durable.Sync(p.file)
}

// close releases the pack's file and mapping; lookups fail from then on.
func (p *ownerPack) close() error {
	p.unmap()
	p.closed = true
	return p.file.Close()
}

// ownerRow is a segment's entry for hash: its owner's name, nil if retired.
type ownerRow struct {
	hash uint32
	name []byte
}

// segmentRows validates a segment's header and returns its rows, the link
// row included, and the segment it extends, if any.
func segmentRows(seg []byte) (int, nodeOwner, error) {
	if len(seg) < 4 {
		return 0, nodeOwner{}, errOwnerIndex
	}
	n := binary.LittleEndian.Uint32(seg)
	if uint64(n)*16+4 > uint64(len(seg)) {
		return 0, nodeOwner{}, errOwnerIndex
	}
	var link nodeOwner
	if n > 0 {
		if row := seg[4+16*(n-1):]; binary.LittleEndian.Uint32(row) == linkHash {
			link = nodeOwner{size: int64(binary.LittleEndian.Uint32(row[4:])), base: int64(binary.LittleEndian.Uint64(row[8:]) & offsetMask)}
		}
	}
	return int(n), link, nil
}

// segmentRow decodes row i of a segment with more than i rows.
func segmentRow(seg []byte, i int) (ownerRow, error) {
	row := seg[4+16*i:]
	r := ownerRow{hash: binary.LittleEndian.Uint32(row)}
	size, offset := uint64(binary.LittleEndian.Uint32(row[4:])), binary.LittleEndian.Uint64(row[8:])
	if offset > uint64(len(seg)) || size > uint64(len(seg))-offset {
		return r, errOwnerIndex
	}
	if size > 0 {
		r.name = seg[offset : offset+size]
	}
	return r, nil
}

// findOwner binary-searches a segment for hash h, returning its owner's
// name, a view of seg, or retired, and otherwise the segment it extends.
func findOwner(seg []byte, h uint32) (name []byte, retired bool, link nodeOwner, err error) {
	n, link, err := segmentRows(seg)
	if err != nil {
		return nil, false, link, err
	}
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		switch rh := binary.LittleEndian.Uint32(seg[4+16*mid:]); {
		case rh < h:
			lo = mid + 1
		case rh > h:
			hi = mid
		default:
			r, err := segmentRow(seg, mid)
			return r.name, err == nil && r.name == nil, nodeOwner{}, err
		}
	}
	return nil, false, link, nil
}

// encodeSegment lays out rows, sorted by hash with one row per hash, ending
// with a link to the segment at link if it has a size.
func encodeSegment(rows []ownerRow, link nodeOwner) []byte {
	count := len(rows)
	if link.size > 0 {
		count++
	}
	size := 4 + 16*count
	for _, r := range rows {
		size += len(r.name)
	}
	data := make([]byte, 4+16*count, size)
	binary.LittleEndian.PutUint32(data, uint32(count))
	for i, r := range rows {
		row := data[4+16*i:]
		binary.LittleEndian.PutUint32(row, r.hash)
		binary.LittleEndian.PutUint32(row[4:], uint32(len(r.name))) // zero for a retired hash
		binary.LittleEndian.PutUint64(row[8:], uint64(len(data)))
		data = append(data, r.name...)
	}
	if link.size > 0 {
		row := data[4+16*len(rows):]
		binary.LittleEndian.PutUint32(row, linkHash)
		binary.LittleEndian.PutUint32(row[4:], uint32(link.size))
		binary.LittleEndian.PutUint64(row[8:], uint64(link.base))
	}
	return data
}

// ownerChain is the shape of a directory's index.
type ownerChain struct {
	segments int
	bytes    int64 // of every segment
	base     int64 // of the oldest segment, which the others extend
}

// walkChain calls f with each segment of the index at o, newest first,
// under mu, until f returns false.
func walkChain(p *ownerPack, o nodeOwner, f func(seg []byte) bool) (ownerChain, error) {
	var c ownerChain
	for o.size > 0 {
		if p == nil || c.segments == 2*maxChain {
			return c, errOwnerIndex
		}
		seg, err := p.segment(o)
		if err != nil {
			return c, err
		}
		_, link, err := segmentRows(seg)
		if err != nil {
			return c, err
		}
		if link.size > 0 && link.base+link.size > o.base {
			return c, errOwnerIndex // a link only ever reaches back
		}
		c.segments++
		c.bytes += o.size
		c.base = o.size
		if !f(seg) {
			return c, nil
		}
		o = link
	}
	return c, nil
}

// segmentOwners returns a segment's rows but its link.
func segmentOwners(seg []byte) ([]ownerRow, error) {
	n, link, err := segmentRows(seg)
	if err != nil {
		return nil, err
	}
	if link.size > 0 {
		n--
	}
	rows := make([]ownerRow, n)
	for i := range rows {
		if rows[i], err = segmentRow(seg, i); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// chainRows returns the rows of the index at o, newest winning, sorted by
// hash and without link rows, with the chain's shape.
func chainRows(p *ownerPack, o nodeOwner) ([]ownerRow, ownerChain, error) {
	var rows []ownerRow
	var err error
	c, walkErr := walkChain(p, o, func(seg []byte) bool {
		var more []ownerRow
		more, err = segmentOwners(seg)
		rows = append(rows, more...)
		return err == nil
	})
	if err = cmp.Or(walkErr, err); err != nil {
		return nil, c, err
	}
	// A stable sort keeps each hash's newest row first.
	slices.SortStableFunc(rows, func(a, b ownerRow) int { return cmp.Compare(a.hash, b.hash) })
	rows = slices.CompactFunc(rows, func(a, b ownerRow) bool { return a.hash == b.hash })
	return rows, c, nil
}

// unreadable handles d's index failing with err. An index adopted from
// before this open loses its owners, as a crash would have lost them: the
// directory resolves by listing again. One this open wrote owns evicted
// identities, so the table faults. It reports whether lookups may go on.
func (t *nodeTable) unreadable(d *node, err error) bool {
	if d.dir.prior {
		t.forgetPrior(d)
		return true
	}
	t.cacheFailure(err)
	return false
}

// coldOwner returns what d's index records for hash h: its owner's name,
// valid until the mutex is released, or retired. ok is false after a fault,
// which no index read follows: a restart clears it.
func (t *nodeTable) coldOwner(d *node, h uint32) (name []byte, retired, ok bool) {
	s := d.dir
	if s == nil || !s.cold {
		return nil, false, true
	}
	if t.cacheErr != nil {
		return nil, false, false
	}
	var err error
	_, walkErr := walkChain(t.pack, ownerOf(s), func(seg []byte) bool {
		name, retired, _, err = findOwner(seg, h)
		return err == nil && name == nil && !retired
	})
	if err = cmp.Or(walkErr, err); err != nil {
		return nil, false, t.unreadable(d, err)
	}
	return name, retired, true
}

// coldIssued reports whether d's index names name as the owner of its hash,
// and no resident identity took that hash since. Callers check cacheErr.
func (t *nodeTable) coldIssued(d *node, name string) bool {
	h := nameHash(name)
	owner, _, _ := t.coldOwner(d, h)
	return owner != nil && string(owner) == name && !t.takenResident(d, h)
}

// coldTaken reports whether d's index owns or retired hash h, or cannot be
// read.
func (t *nodeTable) coldTaken(d *node, h uint32) bool {
	owner, retired, ok := t.coldOwner(d, h)
	return owner != nil || retired || !ok
}

// coldRows returns d's index rows as chainRows does; ok is false after a
// fault, and an unreadable adopted index has none.
func (t *nodeTable) coldRows(d *node) ([]ownerRow, ownerChain, bool) {
	if s := d.dir; s == nil || !s.cold {
		return nil, ownerChain{}, true
	}
	if t.cacheErr != nil {
		return nil, ownerChain{}, false
	}
	rows, c, err := chainRows(t.pack, ownerOf(d.dir))
	if err != nil {
		return nil, ownerChain{}, t.unreadable(d, err)
	}
	return rows, c, true
}

// spillNames appends what d's index lacks of its resident unlogged owners
// and retired hashes to the pack before their memory is reclaimed. It
// reports whether the published index then holds everything resident;
// false without an error means d is busy or changed meanwhile.
func (t *nodeTable) spillNames(d *node) (bool, error) {
	if t.coldSaved {
		return false, nil // the next open adopts the index as close left it
	}
	s := d.dir
	if s.writing {
		return false, nil
	}
	if s.cold && !s.coldDirty {
		return true, nil
	}
	seg, grown, ok := t.ownerSegment(d)
	if !ok {
		return false, t.cacheErr
	}
	if seg == nil {
		s.coldDirty = false
		return true, nil
	}
	pack := t.pack
	var err error
	if pack == nil {
		if pack, err = t.createPack(); err != nil {
			return false, err
		}
	}
	base, err := pack.reserve(int64(len(seg)))
	if err != nil {
		return false, err
	}
	owner := nodeOwner{size: int64(len(seg)), base: base}
	x := t.index
	// Whatever changes ownership from here on marks d dirty again.
	s.coldDirty = false
	s.writing = true
	s.reading++
	t.indexWrites++
	at := s.slot
	t.mu.Unlock()
	if t.indexIO != nil {
		t.indexIO()
	}
	_, err = pack.file.WriteAt(seg, base)
	if err == nil && x != nil && d.key != 0 {
		// The slot locates the segment at once, so the index on disk always
		// finds a key's owners; its key is resident, so nothing reads it.
		err = x.publish(d.key, at, nil, 0, owner)
	}
	t.mu.Lock()
	t.indexWrites--
	s.reading--
	s.writing = false
	if err != nil {
		s.coldDirty = true
		return false, err
	}
	if t.byID[d.id] != d {
		return false, nil // the segment never owned anything
	}
	t.coldBytes += grown
	owner.apply(s)
	if x != nil && d.key != 0 {
		s.loaded = owner
		t.slotWritten(d.key)
	}
	return !s.coldDirty, nil
}

// ownerSegment returns the segment d's index must gain to hold its resident
// leaves, unlogged children and retired hashes, without the hashes logged
// nodes hold: one extending the chain with what it lacks, or merging the
// chain once its extensions would outgrow its base. It is nil when the
// chain holds everything already; grown is the change in live index bytes,
// and ok is false after a fault.
func (t *nodeTable) ownerSegment(d *node) (seg []byte, grown int64, ok bool) {
	s := d.dir
	kept := func(h uint32) bool {
		_, held := s.held[h]
		k := t.byID[derivedID(d.key, h)]
		return !held && (k == nil || k.key == 0)
	}
	var delta []ownerRow
	add := func(h uint32, name string, retired bool) {
		owner, gone, ok := t.coldOwner(d, h)
		switch {
		case !ok:
		case gone:
			// Retired for good; a resident owner cannot outlive that.
		case retired:
			delta = append(delta, ownerRow{hash: h})
		case kept(h) && (owner == nil || string(owner) != name):
			delta = append(delta, ownerRow{h, []byte(name)})
		}
	}
	for h, name := range s.leaves {
		add(h, name, false)
	}
	for name, k := range d.kids {
		if k.key == 0 {
			add(nameHash(name), name, false)
		}
	}
	for h := range s.retired {
		add(h, "", true)
	}
	if t.cacheErr != nil {
		return nil, 0, false
	}
	if len(delta) == 0 {
		return nil, 0, true
	}
	// A retired hash sorts first, and wins over an owner of the same hash.
	slices.SortFunc(delta, func(a, b ownerRow) int {
		return cmp.Or(cmp.Compare(a.hash, b.hash), cmp.Compare(len(a.name), len(b.name)))
	})
	delta = slices.CompactFunc(delta, func(a, b ownerRow) bool { return a.hash == b.hash })
	head := ownerOf(s)
	rows, chain, ok := t.coldRows(d)
	if !ok {
		return nil, 0, false
	}
	if !s.cold {
		head = nodeOwner{} // an unreadable adopted index was forgotten
	}
	seg = encodeSegment(delta, head)
	if head.size == 0 || chain.segments < maxChain && chain.bytes-chain.base+int64(len(seg)) < chain.base {
		return seg, int64(len(seg)), true
	}
	merged := make([]ownerRow, 0, len(rows)+len(delta))
	for i, j := 0, 0; i < len(rows) || j < len(delta); {
		switch {
		case j == len(delta) || i < len(rows) && rows[i].hash < delta[j].hash:
			if rows[i].name == nil || kept(rows[i].hash) {
				merged = append(merged, rows[i])
			}
			i++
		case i == len(rows) || delta[j].hash < rows[i].hash:
			merged = append(merged, delta[j])
			j++
		default:
			merged = append(merged, delta[j])
			i++
			j++
		}
	}
	seg = encodeSegment(merged, nodeOwner{})
	return seg, int64(len(seg)) - chain.bytes, true
}

// cacheFailure fails closed only when an authoritative cold owner cannot be read.
// Spill write failures leave ownership resident and use a separate retry state.
func (t *nodeTable) cacheFailure(err error) {
	if t.cacheErr == nil {
		t.cacheErr = fmt.Errorf("node cache: %w: %v", syscall.EIO, err)
		t.fault.Store(&nodeCacheFault{t.cacheErr})
	}
}

// forgetPrior drops an adopted index that cannot be read. Its owners are lost
// as a crash would have lost them: the directory resolves by listing again.
// An index written by this process stays authoritative instead.
func (t *nodeTable) forgetPrior(d *node) {
	if d.dir.prior {
		t.dropOwners(d)
	}
}

// dropOwners forgets d's index, whose segments become garbage the next
// rebuild drops, as far as they can be read.
func (t *nodeTable) dropOwners(d *node) {
	s := d.dir
	if s == nil || !s.cold {
		return
	}
	chain, _ := walkChain(t.pack, ownerOf(s), func([]byte) bool { return true })
	t.coldBytes -= max(chain.bytes, s.coldBytes)
	s.cold, s.coldBytes, s.base, s.prior = false, 0, 0, false
}

type nodeCacheFault struct{ err error }

// IdentityError reports an unusable index without taking the node
// mutex on each protocol request. Restart is required to clear a fault.
func (n *nodeFS) IdentityError() error {
	if fault := n.nodes.fault.Load(); fault != nil {
		return fault.err
	}
	return nil
}

// recoverPaths makes the issued owners of the names along paths resident
// and pins their directories until the caller finishes its in-memory
// operation.
func (t *nodeTable) recoverPaths(paths ...string) (*pathPins, error) {
	if t.coldBytes == 0 && t.index == nil {
		return nil, nil
	}
	return t.keepPaths(recoverOwners, paths...)
}

// pagePaths pages in and pins the logged nodes along paths, without
// recovering cold owners, until the caller finishes.
func (t *nodeTable) pagePaths(paths ...string) (*pathPins, error) {
	if t.index == nil {
		return nil, nil
	}
	return t.keepPaths(pageLogged, paths...)
}

// recovery is what keepPaths makes resident along its paths.
type recovery int

const (
	pageLogged             recovery = iota // the logged nodes
	recoverOwners                          // also each name's cold owner
	recoverDirectoryOwners                 // also all owners of the destination directory
)

// pathPins retains directory state while an operation may release the node mutex.
type pathPins struct {
	states []*dirState
}

var pathPinPool = sync.Pool{New: func() any {
	return &pathPins{states: make([]*dirState, 0, 16)}
}}

// release drops the pins under the node mutex and clears references before reuse.
func (p *pathPins) release() {
	if p == nil {
		return
	}
	for _, s := range p.states {
		s.reading--
	}
	clear(p.states)
	if cap(p.states) <= 256 {
		p.states = p.states[:0]
		pathPinPool.Put(p)
	}
}

// keepPaths pins the state of every resident directory along paths and pages
// in their cold logged nodes, which releases the mutex, restarting from the
// root after each. With recoverOwners it also admits each name's issued cold
// owner as a leaf, and checks the index of a directory a path ends in, so a
// fault rejects a mutation before it changes the namespace. Generated names
// use recoverDirectoryOwners to retain every possible destination hash.
func (t *nodeTable) keepPaths(how recovery, paths ...string) (*pathPins, error) {
	pins := pathPinPool.Get().(*pathPins)
	for failures := 0; failures < 16; {
		restart := false
		for _, p := range paths {
			d := t.root
			for {
				if s := d.state(); !slices.Contains(pins.states, s) {
					s.reading++
					pins.states = append(pins.states, s)
				}
				name, rest, _ := strings.Cut(p, "/")
				p = rest
				if how != pageLogged {
					var err error
					if how == recoverDirectoryOwners && name == "" {
						err = t.recoverDirectory(d)
					} else {
						err = t.recoverOwner(d, name)
					}
					if err != nil {
						return pins, err
					}
				}
				k := d.kids[name]
				if st, cold := d.stub(name); k == nil && cold {
					if t.pageKey(st.key) == nil {
						if t.cacheErr == nil && t.byID[d.id] == d {
							if again, ok := d.stub(name); ok && again == st {
								t.cacheFailure(errNodeIndex) // a live stub without a record
							}
						}
						if t.cacheErr != nil {
							return pins, t.cacheErr
						}
						failures++
					}
					restart = true
					break
				}
				d = k
				if d == nil || name == "" {
					break
				}
			}
			if restart {
				break
			}
		}
		if !restart {
			return pins, nil
		}
	}
	return pins, syscall.EAGAIN
}

// recoverOwner makes what d's index records for the hash of name resident:
// its owner, whatever its name, as a leaf, or its retirement. An operation
// that changes name then finishes from resident state, even after a fault.
// With name empty it checks that d's index can be read, unless a fault
// already rejects new mutations. A degraded cache admits no owner.
func (t *nodeTable) recoverOwner(d *node, name string) error {
	if s := d.dir; s == nil || !s.cold {
		return nil
	}
	if _, cold := d.stub(name); cold || d.kids[name] != nil {
		return nil
	}
	h := nameHash(name)
	if name == "" && t.cacheErr != nil || name != "" && t.takenResident(d, h) {
		return nil
	}
	owner, retired, ok := t.coldOwner(d, h)
	switch {
	case !ok:
		return t.cacheErr
	case name == "":
	case retired:
		if d.dir.retire(h) {
			t.charge(d, retiredBytes)
		}
	case owner == nil:
	case t.spillErr != nil:
		return syscall.ENOSPC
	default:
		t.addLeaf(d, string(owner))
	}
	return nil
}

// recoverDirectory makes every cold owner and retirement resident before a
// generated-name mutation. Its pinned state then needs no index read to
// record a collision, even if an unrelated directory faults during backend I/O.
func (t *nodeTable) recoverDirectory(d *node) error {
	rows, _, ok := t.coldRows(d)
	if !ok {
		return t.cacheErr
	}
	for _, r := range rows {
		if t.takenResident(d, r.hash) {
			continue
		}
		if r.name == nil {
			if d.dir.retire(r.hash) {
				t.charge(d, retiredBytes)
			}
		} else {
			if t.spillErr != nil {
				return syscall.ENOSPC
			}
			t.addLeaf(d, string(r.name))
		}
	}
	return nil
}

// residentID resolves an issued name with one resident path walk and no I/O.
func (t *nodeTable) residentID(p string) (uint64, bool) {
	if p == "" {
		return t.root.id, true
	}
	dir, name := split(p)
	d := t.find(dir)
	if d == nil {
		return 0, false
	}
	if n := d.kids[name]; n != nil {
		t.touch(d)
		return n.id, true
	}
	if d.leaf(name) {
		t.touch(d)
		return derivedID(d.key, nameHash(name)), true
	}
	if st, ok := d.stub(name); ok {
		return st.id, true
	}
	return 0, false
}

// issuedID finds an existing name without recovering unrelated cold owners.
// admit makes a cold owner it finds a leaf; transient callers leave admission
// to their own operations.
func (t *nodeTable) issuedID(p string, admit bool) (uint64, bool) {
	if id, ok := t.residentID(p); ok {
		return id, true
	}
	dir, name := split(p)
	d := t.find(dir)
	if d == nil || !t.coldIssued(d, name) {
		return 0, false
	}
	if admit && t.spillErr == nil {
		t.addLeaf(d, name)
	}
	return derivedID(d.key, nameHash(name)), true
}
