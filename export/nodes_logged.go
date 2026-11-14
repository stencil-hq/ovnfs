package export

import (
	"bufio"
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// Logged nodes page out to an index of the identity log: a record per key,
// with the node's id, parent key and name, its logged children and the
// logged holders of its derived ids that live elsewhere, and a slot giving
// where the record and the key's owner index are. An open that replays the
// log builds the index afresh from it. A clean close makes the index whole,
// so that the next open adopts it instead of replaying, until the garbage it
// and the log accumulate warrants a rebuild (rebuildReason). A crash
// discards it.
//
// The index lives in a directory nodes-* beside the owner index instance:
//
//	table    a header, then a slot per key logged when it was built, sorted by key
//	records  the records of those keys, then those evictions and clean closes appended
//	runtime  a slot per key allocated since it was built, at its key minus base
//
// A resident logged directory knows its cold logged children (stubs) and
// every logged holder of its derived ids (held), so lookups and derivation
// never wait for the index. A logged node leaves memory only after its
// listing, leaves and unlogged children, and only without resident
// children, holds or operations, so every resident node's ancestors are
// resident. A changed record is appended to records when its node leaves
// memory, and for the nodes still resident at a clean close (saveIndex); a
// lookup searches the table's keys through a memory map.

// Estimated resident bytes of a logged node (node, dirState and its map
// entries), a stub, and a held entry.
const (
	loggedBytes = 320
	stubBytes   = 64
	heldBytes   = 16
)

// stub is a cold logged child of a resident directory.
type stub struct{ key, id uint64 }

// errNodeIndex reports an index that contradicts itself or the table.
var errNodeIndex = errors.New("invalid logged node index")

// Index layout. A slot is five little-endian words: the key; the record's
// offset in records; the record's length; the owner index size below
// 2^sizeBits with the epoch that wrote it above; the owner's base in the
// pack, with packedOwner and priorOwner. An index from before owner indexes
// were all packed (ownersMagic version 3) may leave packedOwner clear for an
// owner index file of its own, which only a rebuild reads (buildIndex).
const (
	slotSize    = 40
	tableHeader = 32
	packedOwner = 1 << 63 // a slot's owner index is a segment of the pack
	priorOwner  = 1 << 62 // a slot's owner index was adopted when the index was built
	offsetMask  = 1<<62 - 1
	sizeBits    = 40
	sizeMask    = 1<<sizeBits - 1
	// maxEpoch bounds the opens that adopt one index; the next replays.
	maxEpoch = 1<<(64-sizeBits) - 1
)

var tableMagic = []byte("ovnfs keys 2\n\x00\x00\x00")

// nodeOwner is where a key's owner index lives in the pack: size 0 for none.
type nodeOwner struct {
	size  int64
	base  int64
	prior bool
}

// ownerOf returns the owner index location d's state records.
func ownerOf(s *dirState) nodeOwner {
	if !s.cold {
		return nodeOwner{}
	}
	return nodeOwner{size: s.coldBytes, base: s.base, prior: s.prior}
}

// apply installs the owner index location o in s.
func (o nodeOwner) apply(s *dirState) {
	s.cold, s.coldBytes, s.base, s.prior = o.size > 0, o.size, o.base, o.prior
}

// nodeSlot locates a key's record and owner index.
type nodeSlot struct {
	key    uint64
	offset int64
	length int64
	owner  nodeOwner
	// The owner index is a file of its own, which only an index from before
	// owner indexes were all packed has.
	ownerFile bool
	at        int64 // where the slot was read, in the table or runtime file
}

// encode lays out s, its owner written in epoch.
func (s nodeSlot) encode(b []byte, epoch uint64) {
	binary.LittleEndian.PutUint64(b[0:], s.key)
	binary.LittleEndian.PutUint64(b[8:], uint64(s.offset))
	binary.LittleEndian.PutUint64(b[16:], uint64(s.length))
	s.owner.encode(b[24:], epoch)
}

// encode lays out o's two slot words, written in epoch.
func (o nodeOwner) encode(b []byte, epoch uint64) {
	binary.LittleEndian.PutUint64(b[0:], uint64(o.size)|epoch<<sizeBits)
	base := uint64(o.base) | packedOwner
	if o.prior {
		base |= priorOwner
	}
	binary.LittleEndian.PutUint64(b[8:], base)
}

// decodeSlot decodes a slot. Its owner index, if any, is prior when it was
// adopted at the build or written in an epoch other than epoch: by an
// earlier open.
func decodeSlot(b []byte, epoch uint64) nodeSlot {
	size, base := binary.LittleEndian.Uint64(b[24:]), binary.LittleEndian.Uint64(b[32:])
	return nodeSlot{
		key:    binary.LittleEndian.Uint64(b),
		offset: int64(binary.LittleEndian.Uint64(b[8:]) & offsetMask),
		length: int64(binary.LittleEndian.Uint64(b[16:]) & offsetMask),
		owner: nodeOwner{
			size:  int64(size & sizeMask),
			base:  int64(base & offsetMask),
			prior: size&sizeMask != 0 && (base&priorOwner != 0 || size>>sizeBits != epoch),
		},
		ownerFile: size&sizeMask != 0 && base&packedOwner == 0,
	}
}

// nodeIndex is an open logged node index. Its locations do not change after
// open, so I/O uses it without the node mutex. mu protects the files' lifetime:
// an operation releases its read lock before reacquiring the node mutex, so
// close may wait for I/O while holding that mutex. Slots of resident keys are
// written only by their node's spills, eviction or close, and read only
// while the key is cold, so no slot is read while it is written.
type nodeIndex struct {
	mu      sync.RWMutex
	closed  bool
	dir     string
	table   *os.File
	view    *mapped.File // the table, whose size never changes after the build; slots are read here
	records *os.File
	runtime *os.File
	count   int64  // slots in table
	base    uint64 // the first key allocated after the build
	// epoch counts the opens that adopted the index since its build. Owner
	// locations this open writes carry it; any other epoch's are prior.
	epoch uint64
}

// locateLocked returns the file and offset of key's slot, or false when no
// slot can exist for it. The caller holds mu and has checked closed.
func (x *nodeIndex) locateLocked(key uint64) (*os.File, int64, bool, error) {
	if key >= x.base {
		return x.runtime, int64(key-x.base) * slotSize, true, nil
	}
	var b [8]byte
	lo, hi := int64(0), x.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := x.view.ReadAt(b[:], tableHeader+mid*slotSize); err != nil {
			return nil, 0, false, err
		}
		switch k := binary.LittleEndian.Uint64(b[:]); {
		case k < key:
			lo = mid + 1
		case k > key:
			hi = mid
		default:
			return x.table, tableHeader + mid*slotSize, true, nil
		}
	}
	return nil, 0, false, nil
}

// slotLocked reads key's slot; false means no record was ever written for it.
// The caller holds mu and has checked closed.
func (x *nodeIndex) slotLocked(key uint64) (nodeSlot, bool, error) {
	f, off, ok, err := x.locateLocked(key)
	if !ok || err != nil {
		return nodeSlot{}, false, err
	}
	var b [slotSize]byte
	var r io.ReaderAt = f
	if f == x.table {
		r = x.view
	}
	if _, err := r.ReadAt(b[:], off); err != nil {
		if errors.Is(err, io.EOF) {
			return nodeSlot{}, false, nil // a runtime slot past the end
		}
		return nodeSlot{}, false, err
	}
	s := decodeSlot(b[:], x.epoch)
	s.at = off
	return s, s.key == key && s.length > 0, nil
}

// slotFileLocked returns the file and offset of key's slot: in the runtime
// file, at at in the table when known, or else wherever the table has it.
// The caller holds mu and has checked closed.
func (x *nodeIndex) slotFileLocked(key uint64, at int64) (*os.File, int64, error) {
	if key >= x.base || at >= tableHeader {
		f := x.table
		if key >= x.base {
			f, at = x.runtime, int64(key-x.base)*slotSize
		}
		return f, at, nil
	}
	f, at, ok, err := x.locateLocked(key)
	if err == nil && !ok {
		err = errNodeIndex
	}
	return f, at, err
}

// read returns key's record and slot.
func (x *nodeIndex) read(key uint64) (rec loggedRecord, s nodeSlot, found bool, err error) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	if x.closed {
		return rec, s, false, os.ErrClosed
	}
	s, found, err = x.slotLocked(key)
	if !found || err != nil {
		return rec, s, false, err
	}
	b := make([]byte, s.length)
	if _, err = x.records.ReadAt(b, s.offset); err != nil {
		return rec, s, false, err
	}
	rec, err = decodeRecord(b, key)
	return rec, s, err == nil, err
}

// publish writes key's slot, at at in the table when known, locating owner
// and, when rec is not nil, the record rec, first written at offset in
// records.
func (x *nodeIndex) publish(key uint64, at int64, rec []byte, offset int64, owner nodeOwner) error {
	x.mu.RLock()
	defer x.mu.RUnlock()
	if x.closed {
		return os.ErrClosed
	}
	f, off, err := x.slotFileLocked(key, at)
	if err != nil {
		return err
	}
	var b [slotSize]byte
	if rec == nil {
		if f == x.runtime {
			binary.LittleEndian.PutUint64(b[:], key)
			if _, err := f.WriteAt(b[:8], off); err != nil {
				return err
			}
		}
		owner.encode(b[24:], x.epoch)
		_, err = f.WriteAt(b[24:], off+24)
		return err
	}
	if _, err := x.records.WriteAt(rec, offset); err != nil {
		return err
	}
	nodeSlot{key: key, offset: offset, length: int64(len(rec)), owner: owner}.encode(b[:], x.epoch)
	_, err = f.WriteAt(b[:], off)
	return err
}

// close waits for in-flight I/O and releases even a partially opened index.
// Later operations fail without touching the unmapped table or closed files.
func (x *nodeIndex) close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed {
		return nil
	}
	x.closed = true
	var err error
	if x.view != nil {
		err = x.view.Close()
	}
	for _, f := range []*os.File{x.table, x.records, x.runtime} {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
	}
	return err
}

// loggedRecord is a logged node as its record describes it.
type loggedRecord struct {
	id, parent uint64
	name       string
	kids       []recordKid
	away       []recordAway
}

type recordKid struct {
	name    string
	key, id uint64
}

// recordAway is a logged node holding a derived id of the record's key
// away from the record's directory.
type recordAway struct {
	hash uint32
	key  uint64
}

// Child id encodings in a record.
const (
	kidCounter = iota // the id is the key
	kidDerived        // the id derives from the directory and the name
	kidID             // the id follows
)

// appendRecord encodes a record. Its children's ids are relative to key.
func appendRecord(b []byte, key uint64, rec *loggedRecord) []byte {
	b = binary.AppendUvarint(b, rec.id)
	b = binary.AppendUvarint(b, rec.parent)
	b = appendName(b, rec.name)
	b = binary.AppendUvarint(b, uint64(len(rec.kids)))
	for _, k := range rec.kids {
		b = appendName(b, k.name)
		b = binary.AppendUvarint(b, k.key)
		switch {
		case k.id == k.key:
			b = append(b, kidCounter)
		case key < maxDerivingKey && k.id == derivedID(key, nameHash(k.name)):
			b = append(b, kidDerived)
		default:
			b = binary.AppendUvarint(append(b, kidID), k.id)
		}
	}
	b = binary.AppendUvarint(b, uint64(len(rec.away)))
	for _, a := range rec.away {
		b = binary.AppendUvarint(binary.AppendUvarint(b, uint64(a.hash)), a.key)
	}
	return b
}

// decodeRecord decodes the record of key written by appendRecord. Only the
// root's, key 1, has no parent and no name.
func decodeRecord(b []byte, key uint64) (loggedRecord, error) {
	r := logReader{b: b}
	rec := loggedRecord{id: r.uvarint(), parent: r.uvarint()}
	if key == 1 {
		if r.uvarint() != 0 || rec.id != 1 || rec.parent != 0 {
			r.err = errNodeIndex
		}
	} else {
		rec.name = r.name()
	}
	n := r.uvarint()
	if r.err == nil && n > uint64(len(r.b)) {
		r.err = errNodeIndex
	}
	if r.err == nil && n > 0 {
		rec.kids = make([]recordKid, 0, n)
	}
	for range n {
		if r.err != nil {
			break
		}
		k := recordKid{name: r.name(), key: r.uvarint()}
		switch r.uvarint() {
		case kidCounter:
			k.id = k.key
		case kidDerived:
			k.id = derivedID(key, nameHash(k.name))
		case kidID:
			k.id = r.uvarint()
		default:
			r.err = errNodeIndex
		}
		rec.kids = append(rec.kids, k)
	}
	n = r.uvarint()
	if r.err == nil && n > uint64(len(r.b)) {
		r.err = errNodeIndex
	}
	for range n {
		if r.err != nil {
			break
		}
		rec.away = append(rec.away, recordAway{hash: uint32(r.uvarint()), key: r.uvarint()})
	}
	if r.err != nil || len(r.b) != 0 {
		return loggedRecord{}, errNodeIndex
	}
	return rec, nil
}

// priorIndex is what a clean close left for the next open: the index whose
// slots hold every key's owner index location, the instance directory and
// the pack of segments in it, if any.
type priorIndex struct {
	nodes, instance, pack string
	// legacy marks a store of an earlier layout, which only a rebuild reads.
	legacy bool
	owners []keyOwner // a version 1 manifest's owner indexes, without nodes
}

// keyOwner is a key's owner index location in a closed index.
type keyOwner struct {
	key   uint64
	owner nodeOwner
	file  bool // a legacy index's owner index file of its own
}

// readPriorOwners reads the owner fields of every slot of a closed index,
// sorted by key.
func readPriorOwners(dir string) ([]keyOwner, error) {
	var out []keyOwner
	read := func(name string, header bool) error {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		defer f.Close()
		r := bufio.NewReaderSize(f, 1<<20)
		count, base := int64(-1), uint64(0)
		if header {
			var h [tableHeader]byte
			if _, err := io.ReadFull(r, h[:]); err != nil {
				return err
			}
			if string(h[:16]) != string(tableMagic) {
				return errNodeIndex
			}
			base, count = binary.LittleEndian.Uint64(h[16:]), int64(binary.LittleEndian.Uint64(h[24:]))
		}
		var b [slotSize]byte
		for i := int64(0); count < 0 || i < count; i++ {
			if _, err := io.ReadFull(r, b[:]); err != nil {
				if count < 0 && errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			s := decodeSlot(b[:], 0) // carried owners are prior anyway
			if s.key == 0 || s.owner.size == 0 {
				continue
			}
			if n := len(out); n > 0 && out[n-1].key >= s.key || header && s.key >= base {
				return errNodeIndex
			}
			out = append(out, keyOwner{s.key, s.owner, s.ownerFile})
		}
		return nil
	}
	if err := read("table", true); err != nil {
		return nil, err
	}
	if err := read("runtime", false); err != nil {
		return nil, err
	}
	return out, nil
}

// legacyOwnerFile is where a legacy index kept key's owner index file of
// its own below its instance root.
func legacyOwnerFile(root string, key uint64) string {
	return filepath.Join(root, fmt.Sprintf("%02x", key&255), strconv.FormatUint(key, 10))
}

// priorOwners reads the owner indexes a closed index locates, for a rebuild
// to carry over.
type priorOwners struct {
	instance string
	pack     *ownerPack
}

// rows returns the owners and retired hashes the closed index holds for key
// at o, newest winning and sorted by hash, and the one segment they came
// from when it can be copied as it is.
func (p *priorOwners) rows(key uint64, o keyOwner) (rows []ownerRow, seg []byte, err error) {
	if o.file {
		data, err := os.ReadFile(legacyOwnerFile(p.instance, key))
		if err != nil {
			return nil, nil, err
		}
		if int64(len(data)) < o.owner.size {
			return nil, nil, errOwnerIndex
		}
		data = data[:o.owner.size]
		rows, err := segmentOwners(data)
		return rows, data, err
	}
	if p.pack == nil {
		return nil, nil, os.ErrNotExist
	}
	rows, chain, err := chainRows(p.pack, o.owner)
	if err == nil && chain.segments == 1 {
		seg, err = p.pack.segment(o.owner)
	}
	return rows, seg, err
}

// buildIndex writes the index of the logged nodes r replayed and makes only
// the root resident. Owner indexes a clean close left for this log are
// carried to a new instance, each as one segment of its pack, with the
// hashes the log retired since; the prior instance and index are discarded
// whole.
func (t *nodeTable) buildIndex(r *replayTable, prior *priorIndex) error {
	discard := t.coldParent + ".discard"
	var owners []keyOwner
	old := &priorOwners{}
	if prior != nil {
		var err error
		owners = prior.owners
		if prior.nodes != "" {
			if owners, err = readPriorOwners(prior.nodes); err != nil {
				owners = nil // unreadable slots lose the adopted owners, as a crash would
			}
		}
		if owners != nil && prior.pack != "" {
			old.pack, _ = openPack(prior.pack) // unreadable segments are forgotten
		}
		old.instance = prior.instance
	}
	closeOld := func() {
		if old.pack != nil {
			old.pack.close()
			old.pack = nil
		}
	}
	defer closeOld()
	live := make([]int32, 0, len(r.keys))
	for _, i := range r.keys {
		live = append(live, i)
	}
	slices.SortFunc(live, func(a, b int32) int { return cmp.Compare(r.nodes[a].key, r.nodes[b].key) })
	away := map[int32][]recordAway{}
	for _, i := range live {
		n := &r.nodes[i]
		if n.id&derivedBit == 0 {
			continue
		}
		if o, ok := r.keys[n.id>>hashBits&(maxDerivingKey-1)]; ok && o != n.parent {
			away[o] = append(away[o], recordAway{uint32(n.id & (1<<hashBits - 1)), n.key})
		}
	}

	dir, err := os.MkdirTemp(t.coldParent, "nodes-")
	if err != nil {
		return err
	}
	x := &nodeIndex{dir: dir, count: int64(len(live)), base: r.next}
	open := func(name string) *os.File {
		f, e := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		err = errors.Join(err, e)
		return f
	}
	x.table, x.records, x.runtime = open("table"), open("records"), open("runtime")
	if err != nil {
		x.close()
		return err
	}
	fail := func(err error) error {
		x.close()
		return err
	}
	tw, rw := bufio.NewWriterSize(x.table, 1<<20), bufio.NewWriterSize(x.records, 1<<20)
	var header [tableHeader]byte
	copy(header[:], tableMagic)
	binary.LittleEndian.PutUint64(header[16:], x.base)
	binary.LittleEndian.PutUint64(header[24:], uint64(x.count))
	if _, err := tw.Write(header[:]); err != nil {
		return fail(err)
	}

	t.coldRoot = ""
	var pw *bufio.Writer
	appendSegment := func(data []byte) (nodeOwner, error) {
		if t.pack == nil {
			if _, err := t.createPack(); err != nil {
				return nodeOwner{}, err
			}
			pw = bufio.NewWriterSize(t.pack.file, 1<<20)
		}
		o := nodeOwner{size: int64(len(data)), base: t.pack.end, prior: true}
		_, err := pw.Write(data)
		t.pack.end += o.size
		t.pack.size = t.pack.end
		return o, err
	}
	// carry returns where a key's owner index lives in the new instance.
	carry := func(key uint64, o keyOwner, retired []uint32) (nodeOwner, error) {
		var rows []ownerRow
		var seg []byte
		if o.owner.size > 0 {
			var err error
			if rows, seg, err = old.rows(key, o); err != nil {
				rows, seg = nil, nil // an unreadable adopted index holds no owners
			}
		}
		if len(retired) == 0 && seg != nil {
			return appendSegment(seg)
		}
		for _, h := range retired {
			i, found := slices.BinarySearchFunc(rows, h, func(r ownerRow, h uint32) int { return cmp.Compare(r.hash, h) })
			if found {
				rows[i].name = nil
			} else {
				rows = slices.Insert(rows, i, ownerRow{hash: h})
			}
		}
		if len(rows) == 0 {
			return nodeOwner{}, nil
		}
		carried, err := appendSegment(encodeSegment(rows, nodeOwner{}))
		// One holding what the log retired since is no longer only adopted.
		carried.prior = len(retired) == 0
		return carried, err
	}

	var offset int64
	var rec []byte
	next := 0
	for _, i := range live {
		n := &r.nodes[i]
		var o keyOwner
		for ; next < len(owners) && owners[next].key <= n.key; next++ {
			if owners[next].key == n.key {
				o = owners[next]
			}
		}
		owner, err := carry(n.key, o, r.retired[i])
		if err != nil {
			return fail(err)
		}
		t.coldBytes += owner.size
		rec = rec[:0]
		if i != 0 {
			l := loggedRecord{id: n.id, parent: r.nodes[n.parent].key, name: n.name, away: away[i]}
			for c := n.child; c >= 0; c = r.nodes[c].next {
				l.kids = append(l.kids, recordKid{r.nodes[c].name, r.nodes[c].key, r.nodes[c].id})
			}
			rec = appendRecord(rec, n.key, &l)
		}
		var b [slotSize]byte
		nodeSlot{key: n.key, offset: offset, length: int64(len(rec)), owner: owner}.encode(b[:], x.epoch)
		if _, err := tw.Write(b[:]); err != nil {
			return fail(err)
		}
		if _, err := rw.Write(rec); err != nil {
			return fail(err)
		}
		offset += int64(len(rec))
		if i == 0 {
			// The root has the lowest key. Its record waits for close.
			s := t.root.state()
			owner.apply(s)
			s.loaded, s.slot, s.stale = owner, tableHeader, true
		}
	}
	if err := errors.Join(tw.Flush(), rw.Flush()); err != nil {
		return fail(err)
	}
	if pw != nil {
		if err := pw.Flush(); err != nil {
			return fail(err)
		}
	}
	if x.view, err = mapped.Open(x.table.Name()); err != nil {
		return fail(err)
	}
	t.index = x
	t.built, t.recordsEnd = offset, offset
	var bytes int64
	for c := r.nodes[0].child; c >= 0; c = r.nodes[c].next {
		bytes += t.addStub(t.root, r.nodes[c].name, stub{r.nodes[c].key, r.nodes[c].id})
	}
	for _, a := range away[0] {
		bytes += t.addHeld(t.root, a.hash, a.key)
	}
	t.addLogged(t.root, bytes)
	if prior != nil {
		closeOld()
		if prior.nodes != "" {
			os.Rename(prior.nodes, filepath.Join(discard, "store-"+rand.Text()))
		}
		if prior.instance != "" {
			os.Rename(prior.instance, filepath.Join(discard, "store-"+rand.Text()))
		}
	}
	return nil
}

// adoptIndex resumes the logged node index a clean close left, as the
// manifest h and prior describe it, instead of replaying the log: the root
// pages in from its record, every other key stays cold, and the counter, the
// accounting and the garbage still to reclaim come from the manifest. The
// epoch advances, so every owner location the index holds counts as prior.
// It changes nothing in t until the index proves usable.
func (t *nodeTable) adoptIndex(h *ownersHeader, prior *priorIndex) error {
	x := &nodeIndex{dir: prior.nodes, epoch: h.epoch + 1}
	var err error
	open := func(name string) *os.File {
		f, e := os.OpenFile(filepath.Join(x.dir, name), os.O_RDWR, 0)
		err = errors.Join(err, e)
		return f
	}
	x.table, x.records, x.runtime = open("table"), open("records"), open("runtime")
	var pack *ownerPack
	fail := func(err error) error {
		x.close()
		if pack != nil {
			pack.close()
		}
		return err
	}
	if err != nil {
		return fail(err)
	}
	var header [tableHeader]byte
	if _, err := x.table.ReadAt(header[:], 0); err != nil {
		return fail(err)
	}
	x.base, x.count = binary.LittleEndian.Uint64(header[16:]), int64(binary.LittleEndian.Uint64(header[24:]))
	table, err := x.table.Stat()
	if err != nil {
		return fail(err)
	}
	records, err := x.records.Stat()
	if err != nil {
		return fail(err)
	}
	if string(header[:16]) != string(tableMagic) || x.count < 1 || table.Size() != tableHeader+x.count*slotSize ||
		records.Size() != int64(h.records) || h.built > h.records || x.base < 2 || h.next < x.base || h.next >= derivedBit {
		return fail(errNodeIndex)
	}
	if x.view, err = mapped.Open(x.table.Name()); err != nil {
		return fail(err)
	}
	if prior.pack != "" {
		if pack, err = openPack(prior.pack); err != nil {
			return fail(err)
		}
	}
	rec, slot, found, err := x.read(1)
	if err == nil && !found {
		err = errNodeIndex
	}
	if err != nil {
		return fail(err)
	}
	t.index, t.next, t.pack, t.coldRoot = x, h.next, pack, prior.instance
	t.compacted, t.built, t.recordsEnd = int64(h.compacted), int64(h.built), int64(h.records)
	t.coldBytes, t.runtimeBytes = int64(h.coldBytes), int64(h.runtimeBytes)
	t.deadKeys = h.dead
	s := t.root.state()
	slot.owner.apply(s)
	s.loaded, s.slot = slot.owner, slot.at
	var bytes int64
	for _, k := range rec.kids {
		bytes += t.addStub(t.root, k.name, stub{k.key, k.id})
	}
	for _, a := range rec.away {
		bytes += t.addHeld(t.root, a.hash, a.key)
	}
	t.addLogged(t.root, bytes)
	return nil
}

// loggedSize estimates the resident bytes of a logged node named name.
func loggedSize(name string) int64 { return int64(loggedBytes + len(name)) }

// addStub records st as d's cold logged child name, and as holder of the
// derived id it carries if that derives from d. It returns the bytes the
// caller charges.
func (t *nodeTable) addStub(d *node, name string, st stub) int64 {
	s := d.state()
	if s.stubs == nil {
		s.stubs = make(map[string]stub)
	}
	s.stubs[name] = st
	bytes := int64(stubBytes + len(name))
	if st.id&derivedBit != 0 && st.id>>hashBits&(maxDerivingKey-1) == d.key {
		bytes += t.addHeld(d, uint32(st.id&(1<<hashBits-1)), st.key)
	}
	return bytes
}

// addHeld records that the logged node keyed key holds d's derived id of
// hash h, returning the bytes the caller charges.
func (t *nodeTable) addHeld(d *node, h uint32, key uint64) int64 {
	s := d.state()
	if s.held == nil {
		s.held = make(map[uint32]uint64)
	}
	if _, ok := s.held[h]; ok {
		s.held[h] = key
		return 0
	}
	s.held[h] = key
	return heldBytes
}

// stub returns n's cold logged child name.
func (n *node) stub(name string) (stub, bool) {
	if n.dir == nil {
		return stub{}, false
	}
	st, ok := n.dir.stubs[name]
	return st, ok
}

// markStale notes that logged node d's record no longer matches the index.
func (t *nodeTable) markStale(d *node) {
	if d.key != 0 {
		s := d.state()
		s.stale = true
		s.version++
	}
}

// loggedNew charges a node just logged and marks it and its parent stale.
func (t *nodeTable) loggedNew(k *node) {
	k.state().stale = true
	t.addLogged(k, loggedSize(k.name))
	t.markStale(k.parent)
	if k.id&derivedBit != 0 {
		if o := t.byKey[k.id>>hashBits&(maxDerivingKey-1)]; o != nil {
			t.addLogged(o, t.addHeld(o, uint32(k.id&(1<<hashBits-1)), k.key))
			t.markStale(o)
		}
	}
}

// pageID returns the logged node of counter id, paging it in.
func (t *nodeTable) pageID(id uint64) *node {
	if id&derivedBit != 0 || t.index == nil {
		return nil
	}
	if n := t.pageKey(id); n != nil && n.id == id {
		return n
	}
	return nil
}

// pageKey returns the logged node keyed key, paging it in with its cold
// ancestors, or nil if no live node has that key. It may release the mutex.
// A key whose record is read is registered so that nobody pages it in, and
// so changes, meanwhile; a resident parent's stub must then still name it.
func (t *nodeTable) pageKey(key uint64) *node {
	for {
		if n := t.byKey[key]; n != nil {
			return n
		}
		if t.index == nil || t.cacheErr != nil || key < 2 || key >= t.next {
			return nil
		}
		if wait := t.paging[key]; wait != nil {
			t.mu.Unlock()
			<-wait
			t.mu.Lock()
			continue
		}
		done := make(chan struct{})
		if t.paging == nil {
			t.paging = make(map[uint64]chan struct{})
		}
		t.paging[key] = done
		x := t.index
		t.mu.Unlock()
		rec, slot, found, err := x.read(key)
		t.mu.Lock()
		delete(t.paging, key)
		close(done)
		if err != nil {
			t.cacheFailure(err)
			return nil
		}
		if !found || t.cacheErr != nil {
			return nil
		}
		p := t.byKey[rec.parent]
		if p == nil {
			if rec.parent == key || t.pageKey(rec.parent) == nil {
				return nil
			}
			continue // read again: key was not registered meanwhile
		}
		if n := p.kids[rec.name]; n != nil {
			if n.key == key {
				return n
			}
			return nil
		}
		st, ok := p.stub(rec.name)
		if !ok || st.key != key {
			return nil // retired with an ancestor
		}
		if st.id != rec.id {
			t.cacheFailure(errNodeIndex)
			return nil
		}
		return t.materialize(p, key, &rec, slot)
	}
}

// materialize makes the cold child of p described by rec resident.
func (t *nodeTable) materialize(p *node, key uint64, rec *loggedRecord, slot nodeSlot) *node {
	// p's held entry for the node's derived id stays.
	delete(p.dir.stubs, rec.name)
	t.addLogged(p, -int64(stubBytes+len(rec.name)))
	n := &node{id: rec.id, key: key, name: rec.name, parent: p}
	t.link(p, n)
	t.byKey[key] = n
	s := n.state()
	slot.owner.apply(s)
	s.loaded, s.slot = slot.owner, slot.at
	bytes := loggedSize(rec.name)
	for _, k := range rec.kids {
		bytes += t.addStub(n, k.name, stub{k.key, k.id})
	}
	for _, a := range rec.away {
		bytes += t.addHeld(n, a.hash, a.key)
	}
	t.addLogged(n, bytes)
	for _, e := range t.enumerations {
		if _, ok := e.keys[key]; ok {
			e.invalid = true
		}
	}
	t.touch(n)
	return n
}

// trimmed reports whether d keeps nothing but what leaves with its node.
func (t *nodeTable) trimmed(d *node) bool {
	return d.dir.cached == d.dir.logged
}

// evictable reports whether logged node d may leave memory now.
func (t *nodeTable) evictable(d *node) bool {
	s := d.dir
	return t.index != nil && !t.coldSaved && d.key != 0 && d != t.root && d.holds == 0 && len(d.kids) == 0 &&
		s.reading == 0 && !s.writing && s.cached == s.logged
}

// reclaimable reports whether trimming d can free anything now.
func (t *nodeTable) reclaimable(d *node) bool {
	return d.dir.reading == 0 && (!t.trimmed(d) || t.evictable(d))
}

// appendRecordOf appends the record describing resident logged node d: its
// cold logged children, its resident ones (none when it leaves memory) and
// the logged holders of its derived ids elsewhere.
func (t *nodeTable) appendRecordOf(b []byte, d *node) []byte {
	s := d.state()
	l := loggedRecord{id: d.id, name: d.name}
	if d.parent != nil {
		l.parent = d.parent.key
	}
	for name, st := range s.stubs {
		l.kids = append(l.kids, recordKid{name, st.key, st.id})
	}
	for name, k := range d.kids {
		if k.key != 0 {
			l.kids = append(l.kids, recordKid{name, k.key, k.id})
		}
	}
	if len(s.held) == 0 {
		return appendRecord(b, d.key, &l)
	}
	inPlace := make(map[uint64]struct{}, len(l.kids))
	for _, k := range l.kids {
		inPlace[k.key] = struct{}{}
	}
	for h, key := range s.held {
		if _, ok := inPlace[key]; !ok {
			l.away = append(l.away, recordAway{h, key})
		}
	}
	return appendRecord(b, d.key, &l)
}

// evictNode pages out trimmed logged node d, writing its record and slot
// first when they changed. It reports whether d left memory.
func (t *nodeTable) evictNode(d *node) bool {
	s := d.dir
	owner := ownerOf(s)
	if s.stale || owner != s.loaded {
		var rec []byte
		if s.stale {
			rec = t.appendRecordOf(nil, d)
		}
		x, version, location, at := t.index, s.version, locate(d), s.slot
		// The record goes to the end of records, claimed under the mutex.
		offset := t.recordsEnd
		t.recordsEnd += int64(len(rec))
		s.writing = true
		s.reading++
		t.indexWrites++
		t.mu.Unlock()
		err := x.publish(d.key, at, rec, offset, owner)
		t.mu.Lock()
		t.indexWrites--
		s.reading--
		s.writing = false
		if err != nil {
			t.spillFailed(err)
			return false
		}
		s.loaded = owner
		t.slotWritten(d.key)
		if t.byID[d.id] != d || s.version != version || !location.holds(d) || !t.evictable(d) || s.coldDirty || s.recent == t.cacheLRU.Front() {
			return false
		}
		s.stale = false
	}
	p := d.parent
	delete(p.kids, d.name)
	if len(p.kids) == 0 {
		p.kids = nil
	}
	delete(t.byID, d.id)
	delete(t.byKey, d.key)
	t.uncache(d)
	d.moves++ // operations still holding d must not mistake it for the paged-in node
	t.addLogged(p, t.addStub(p, d.name, stub{d.key, d.id}))
	return true
}

// slotWritten accounts the runtime slot of key, just written.
func (t *nodeTable) slotWritten(key uint64) {
	if x := t.index; key >= x.base {
		t.runtimeBytes = max(t.runtimeBytes, int64(key-x.base+1)*slotSize)
	}
}

// saveIndex makes the index locate every resident logged node as it is, at
// close, so that the next open can adopt it. The record of each stale node,
// the root's included, is appended to records, and each slot whose record or
// owner location changed is rewritten in place, adjacent slots in one write.
// The caller holds mu throughout, with no index write running, and flushes
// the index (syncIndex) before the manifest.
func (t *nodeTable) saveIndex() error {
	x := t.index
	x.mu.RLock()
	defer x.mu.RUnlock()
	if x.closed {
		return os.ErrClosed
	}
	var keys []uint64
	for key, d := range t.byKey {
		if s := d.state(); s.stale || ownerOf(s) != s.loaded {
			keys = append(keys, key)
		}
	}
	// In key order, slots come in file order: the table's sorted by key,
	// then the runtime's at their keys.
	slices.Sort(keys)
	type run struct {
		f        *os.File
		at       int64
		from, to int // its bytes in slots
		err      error
	}
	var runs []*run
	var slots, rec []byte
	sizes := make([]int64, len(keys)) // of each key's appended record, or -1
	end := t.recordsEnd
	w := bufio.NewWriterSize(io.NewOffsetWriter(x.records, end), 1<<20)
	for i, key := range keys {
		d := t.byKey[key]
		s := d.dir
		owner := ownerOf(s)
		f, at, err := x.slotFileLocked(key, s.slot)
		if err != nil {
			return err
		}
		from := len(slots)
		if s.stale {
			rec = t.appendRecordOf(rec[:0], d)
			if _, err := w.Write(rec); err != nil {
				return err
			}
			slots = append(slots, make([]byte, slotSize)...)
			nodeSlot{key: key, offset: end, length: int64(len(rec)), owner: owner}.encode(slots[from:], x.epoch)
			sizes[i] = int64(len(rec))
			end += sizes[i]
		} else {
			// The slot already locates the record; only the owner changed.
			at += 24
			slots = append(slots, make([]byte, 16)...)
			owner.encode(slots[from:], x.epoch)
			sizes[i] = -1
		}
		if n := len(runs); n > 0 && runs[n-1].f == f && runs[n-1].at+int64(runs[n-1].to-runs[n-1].from) == at {
			runs[n-1].to = len(slots)
		} else {
			runs = append(runs, &run{f: f, at: at, from: from, to: len(slots)})
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	inParallel(runs, func(r *run) { _, r.err = r.f.WriteAt(slots[r.from:r.to], r.at) })
	for _, r := range runs {
		if r.err != nil {
			return r.err
		}
	}
	for i, key := range keys {
		s := t.byKey[key].dir
		s.loaded = ownerOf(s)
		if sizes[i] >= 0 {
			s.stale = false
			t.slotWritten(key)
		}
	}
	t.recordsEnd = end
	return nil
}

// syncIndex makes durable everything the manifest makes authoritative, at
// close: the index files, the pack trimmed to what was appended, and the
// directories naming them. The caller holds mu, with no index write running.
func (t *nodeTable) syncIndex() error {
	x := t.index
	x.mu.RLock()
	defer x.mu.RUnlock()
	if x.closed {
		return os.ErrClosed
	}
	type flush struct {
		f   func() error
		err error
	}
	files := []*flush{{f: func() error { return durable.Sync(x.table) }}, {f: func() error { return durable.Sync(x.records) }},
		{f: func() error { return durable.Sync(x.runtime) }}, {f: func() error { return durable.Dir(x.dir) }}, {f: func() error { return durable.Dir(t.coldParent) }}}
	if t.pack != nil {
		files = append(files, &flush{f: t.pack.finish}, &flush{f: func() error { return durable.Dir(t.coldRoot) }})
	}
	inParallel(files, func(fl *flush) { fl.err = fl.f() })
	for _, fl := range files {
		if fl.err != nil {
			return fl.err
		}
	}
	return nil
}

// dropStub retires p's cold logged child name and its whole subtree
// without paging it in: the log records the removal, and the subtree's
// index files are reclaimed in the background.
func (t *nodeTable) dropStub(p *node, name string) {
	st := p.dir.stubs[name]
	t.recordRemove(st.key)
	delete(p.dir.stubs, name)
	t.addLogged(p, -int64(stubBytes+len(name)))
	t.markStale(p)
	t.deadKeys = append(t.deadKeys, st.key)
	t.retireID(st.id)
}

// retireID reserves derived id for good in its resident directory.
func (t *nodeTable) retireID(id uint64) {
	if id&derivedBit == 0 {
		return
	}
	d := t.byKey[id>>hashBits&(maxDerivingKey-1)]
	if d == nil {
		return // its cold directory's held entry keeps it reserved
	}
	s := d.state()
	h := uint32(id & (1<<hashBits - 1))
	if _, ok := s.held[h]; ok {
		delete(s.held, h)
		t.addLogged(d, -heldBytes)
		t.markStale(d)
	}
	if s.retire(h) {
		t.charge(d, retiredBytes)
		s.coldDirty = true
	}
}

// enumeration is a read of cold logged subtrees. Paging one of its keys in
// invalidates it: a resident node can change before its record does.
type enumeration struct {
	keys    map[uint64]struct{}
	invalid bool
}

// coldIDs lists the ids issued in the cold logged subtrees keyed roots: the
// logged nodes and the derived owners their indexes hold. The record reads
// release the mutex; false means a fault or a node paged in meanwhile.
func (t *nodeTable) coldIDs(roots []uint64) ([]uint64, bool) {
	e := &enumeration{keys: make(map[uint64]struct{})}
	t.enumerations = append(t.enumerations, e)
	defer func() {
		t.enumerations = slices.DeleteFunc(t.enumerations, func(o *enumeration) bool { return o == e })
	}()
	// coldDir is a cold directory with an owner index.
	type coldDir struct {
		key   uint64
		owner nodeOwner
		held  map[uint32]struct{} // its derived ids logged nodes hold
	}
	var out []uint64
	for frontier := roots; len(frontier) > 0; {
		for _, key := range frontier {
			e.keys[key] = struct{}{}
			if t.byKey[key] != nil {
				return nil, false
			}
		}
		x := t.index
		t.mu.Unlock()
		var next, ids []uint64
		var dirs []coldDir
		var err error
		for _, key := range frontier {
			rec, slot, found, readErr := x.read(key)
			if err = readErr; err != nil {
				break
			}
			if !found {
				continue
			}
			held := make(map[uint32]struct{}, len(rec.kids)+len(rec.away))
			for _, k := range rec.kids {
				ids, next = append(ids, k.id), append(next, k.key)
				if k.id&derivedBit != 0 && k.id>>hashBits&(maxDerivingKey-1) == key {
					held[uint32(k.id&(1<<hashBits-1))] = struct{}{}
				}
			}
			for _, a := range rec.away {
				held[a.hash] = struct{}{}
			}
			if slot.owner.size > 0 {
				dirs = append(dirs, coldDir{key, slot.owner, held})
			}
		}
		t.mu.Lock()
		if err != nil {
			t.cacheFailure(err)
			return nil, false
		}
		for _, d := range dirs {
			rows, _, err := chainRows(t.pack, d.owner)
			if err != nil {
				if d.owner.prior {
					continue // an adopted index that cannot be read holds no owners
				}
				t.cacheFailure(err)
				return nil, false
			}
			for _, r := range rows {
				if _, ok := d.held[r.hash]; r.name != nil && !ok {
					ids = append(ids, derivedID(d.key, r.hash))
				}
			}
		}
		out = append(out, ids...)
		frontier = next
	}
	return out, !e.invalid
}

// deadBatch bounds the retired cold keys one trim reclaims.
const deadBatch = 64

// cleanDead discounts the owner indexes of a batch of retired cold keys from
// the live index bytes, which decide when an open rebuilds the pack, and
// queues their children. Errors leave them counted.
func (t *nodeTable) cleanDead() {
	for range deadBatch {
		if t.coldSaved || len(t.deadKeys) == 0 {
			return
		}
		x := t.index
		// The batch's I/O is in flight: close must not write a manifest that
		// omits what it popped.
		t.indexWrites++
		key := t.deadKeys[len(t.deadKeys)-1]
		t.deadKeys = t.deadKeys[:len(t.deadKeys)-1]
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		rec, slot, found, err := x.read(key)
		t.mu.Lock()
		t.indexWrites--
		if !found || err != nil {
			continue
		}
		if slot.owner.size > 0 {
			chain, _ := walkChain(t.pack, slot.owner, func([]byte) bool { return true })
			t.coldBytes -= chain.bytes
		}
		for _, k := range rec.kids {
			t.deadKeys = append(t.deadKeys, k.key)
		}
	}
}
