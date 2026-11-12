package export

import (
	"bufio"
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/stencil-hq/ovnfs/internal/durable"
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
//	records  the records of those keys, then those clean closes appended
//	runtime  a slot per key allocated since it was built, at its key minus base
//	xx/key   a record written when its node left memory, sharded like owner index files
//
// A resident logged directory knows its cold logged children (stubs) and
// every logged holder of its derived ids (held), so lookups and derivation
// never wait for the index. A logged node leaves memory only after its
// listing, leaves and unlogged children, and only without resident
// children, holds or operations, so every resident node's ancestors are
// resident. A changed record is written to a file of its own when its node
// leaves memory, and appended to records for the nodes still resident at a
// clean close (saveIndex).

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
// offset in records, or ownFile; the record's length; the owner index size
// below 2^sizeBits with the epoch that wrote it above; the owner's base in the
// pack, with packedOwner and priorOwner.
const (
	slotSize    = 40
	tableHeader = 32
	ownFile     = 1 << 63 // a slot's record is a file of its own
	packedOwner = 1 << 63 // a slot's owner index is a segment of the pack
	priorOwner  = 1 << 62 // a slot's owner index was adopted when the index was built
	offsetMask  = 1<<62 - 1
	sizeBits    = 40
	sizeMask    = 1<<sizeBits - 1
	// maxEpoch bounds the opens that adopt one index; the next replays.
	maxEpoch = 1<<(64-sizeBits) - 1
)

var tableMagic = []byte("ovnfs keys 2\n\x00\x00\x00")

// nodeOwner is where a key's owner index lives: size 0 for none.
type nodeOwner struct {
	size          int64
	base          int64
	packed, prior bool
}

// ownerOf returns the owner index location d's state records.
func ownerOf(s *dirState) nodeOwner {
	if !s.cold {
		return nodeOwner{}
	}
	return nodeOwner{size: s.coldBytes, base: s.base, packed: s.packed, prior: s.prior}
}

// apply installs the owner index location o in s.
func (o nodeOwner) apply(s *dirState) {
	s.cold, s.coldBytes, s.base, s.packed, s.prior = o.size > 0, o.size, o.base, o.packed, o.prior
}

// nodeSlot locates a key's record and owner index.
type nodeSlot struct {
	key    uint64
	own    bool // the record is a file of its own
	offset int64
	length int64
	owner  nodeOwner
	at     int64 // where the slot was read, in the table or runtime file
}

// encode lays out s, its owner written in epoch.
func (s nodeSlot) encode(b []byte, epoch uint64) {
	binary.LittleEndian.PutUint64(b[0:], s.key)
	record := uint64(s.offset)
	if s.own {
		record |= ownFile
	}
	binary.LittleEndian.PutUint64(b[8:], record)
	binary.LittleEndian.PutUint64(b[16:], uint64(s.length))
	s.owner.encode(b[24:], epoch)
}

// encode lays out o's two slot words, written in epoch.
func (o nodeOwner) encode(b []byte, epoch uint64) {
	binary.LittleEndian.PutUint64(b[0:], uint64(o.size)|epoch<<sizeBits)
	base := uint64(o.base)
	if o.packed {
		base |= packedOwner
	}
	if o.prior {
		base |= priorOwner
	}
	binary.LittleEndian.PutUint64(b[8:], base)
}

// decodeSlot decodes a slot. Its owner index, if any, is prior when it was
// adopted at the build or written in an epoch other than epoch: by an
// earlier open.
func decodeSlot(b []byte, epoch uint64) nodeSlot {
	record := binary.LittleEndian.Uint64(b[8:])
	size, base := binary.LittleEndian.Uint64(b[24:]), binary.LittleEndian.Uint64(b[32:])
	return nodeSlot{
		key:    binary.LittleEndian.Uint64(b),
		own:    record&ownFile != 0,
		offset: int64(record & offsetMask),
		length: int64(binary.LittleEndian.Uint64(b[16:]) & offsetMask),
		owner: nodeOwner{
			size:   int64(size & sizeMask),
			base:   int64(base & offsetMask),
			packed: base&packedOwner != 0,
			prior:  size&sizeMask != 0 && (base&priorOwner != 0 || size>>sizeBits != epoch),
		},
	}
}

// nodeIndex is an open logged node index. Its fields do not change after
// open, so I/O uses it without the node mutex. Slots of resident keys are
// written only by their node's eviction or close, and read only while the
// key is cold, so no slot is read while it is written.
type nodeIndex struct {
	dir     string
	table   *os.File
	records *os.File
	runtime *os.File
	count   int64  // slots in table
	base    uint64 // the first key allocated after the build
	// epoch counts the opens that adopted the index since its build. Owner
	// locations this open writes carry it; any other epoch's are prior.
	epoch uint64
}

// locate returns the file and offset of key's slot, or false when no slot
// can exist for it.
func (x *nodeIndex) locate(key uint64) (*os.File, int64, bool, error) {
	if key >= x.base {
		return x.runtime, int64(key-x.base) * slotSize, true, nil
	}
	var b [8]byte
	lo, hi := int64(0), x.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := x.table.ReadAt(b[:], tableHeader+mid*slotSize); err != nil {
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

// slot reads key's slot; false means no record was ever written for it.
func (x *nodeIndex) slot(key uint64) (nodeSlot, bool, error) {
	f, off, ok, err := x.locate(key)
	if !ok || err != nil {
		return nodeSlot{}, false, err
	}
	var b [slotSize]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		if errors.Is(err, io.EOF) {
			return nodeSlot{}, false, nil // a runtime slot past the end
		}
		return nodeSlot{}, false, err
	}
	s := decodeSlot(b[:], x.epoch)
	s.at = off
	return s, s.key == key && (s.own || s.length > 0), nil
}

// slotFile returns the file and offset of key's slot: in the runtime file,
// at at in the table when at is known, or else wherever the table has it.
func (x *nodeIndex) slotFile(key uint64, at int64) (*os.File, int64, error) {
	if key >= x.base || at >= tableHeader {
		f := x.table
		if key >= x.base {
			f, at = x.runtime, int64(key-x.base)*slotSize
		}
		return f, at, nil
	}
	f, at, ok, err := x.locate(key)
	if err == nil && !ok {
		err = errNodeIndex
	}
	return f, at, err
}

// read returns key's record and slot. A missing record file means its key
// was retired and its garbage removed.
func (x *nodeIndex) read(key uint64) (rec loggedRecord, s nodeSlot, found bool, err error) {
	s, found, err = x.slot(key)
	if !found || err != nil {
		return rec, s, false, err
	}
	var b []byte
	if s.own {
		b, err = os.ReadFile(coldIndexPath(x.dir, key))
		if errors.Is(err, os.ErrNotExist) {
			return rec, s, false, nil
		}
	} else {
		b = make([]byte, s.length)
		_, err = x.records.ReadAt(b, s.offset)
	}
	if err != nil {
		return rec, s, false, err
	}
	rec, err = decodeRecord(b, key)
	return rec, s, err == nil, err
}

// publish writes key's record, when rec is not nil, and its slot.
func (x *nodeIndex) publish(key uint64, rec []byte, owner nodeOwner) error {
	f, off, ok, err := x.locate(key)
	if err != nil {
		return err
	}
	if !ok {
		return errNodeIndex
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
	target := coldIndexPath(x.dir, key)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".record-*")
	if err != nil {
		return err
	}
	if _, err = tmp.Write(rec); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), target)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	nodeSlot{key: key, own: true, length: int64(len(rec)), owner: owner}.encode(b[:], x.epoch)
	_, err = f.WriteAt(b[8:], off+8)
	if err == nil && f == x.runtime {
		_, err = f.WriteAt(b[:8], off)
	}
	return err
}

// close releases the index files; the next open reads their owner fields.
func (x *nodeIndex) close() error {
	return errors.Join(x.table.Close(), x.records.Close(), x.runtime.Close())
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
// slots hold every key's owner index location, and the pack of segments.
type priorIndex struct{ nodes, pack string }

// keyOwner is a key's owner index location in a closed index.
type keyOwner struct {
	key   uint64
	owner nodeOwner
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
			out = append(out, keyOwner{s.key, s.owner})
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

// buildIndex writes the index of the logged nodes r replayed and makes only
// the root resident. Owner indexes a clean close left for this log are
// adopted: their files stay, and their pack segments move to a new pack
// together with the hashes the log retired since, so one pack remains.
func (t *nodeTable) buildIndex(r *replayTable, prior *priorIndex) error {
	discard := t.coldParent + ".discard"
	var owners []keyOwner
	var oldPack *os.File
	if prior != nil {
		var err error
		if owners, err = readPriorOwners(prior.nodes); err != nil {
			// Unreadable slots lose the adopted owners, as a crash would.
			owners = nil
			os.Rename(t.coldRoot, filepath.Join(discard, "store-"+rand.Text()))
			t.coldRoot = ""
		} else if prior.pack != "" {
			oldPack, _ = os.Open(prior.pack) // unreadable segments are forgotten
		}
	}
	closeOld := func() {
		if oldPack != nil {
			oldPack.Close()
			oldPack = nil
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
		for _, f := range []*os.File{x.table, x.records, x.runtime} {
			if f != nil {
				f.Close()
			}
		}
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

	var pack *os.File
	var pw *bufio.Writer
	var packSize int64
	var segment []byte
	appendSegment := func(data []byte) (int64, error) {
		if pack == nil {
			if t.coldRoot == "" {
				root, err := os.MkdirTemp(t.coldParent, "instance-")
				if err != nil {
					return 0, err
				}
				t.coldRoot = root
			}
			var err error
			if pack, err = os.CreateTemp(t.coldRoot, "pack-"); err != nil {
				return 0, err
			}
			pw = bufio.NewWriterSize(pack, 1<<20)
		}
		base := packSize
		_, err := pw.Write(data)
		packSize += int64(len(data))
		return base, err
	}
	// carry returns where a key's owner index lives in this instance.
	carry := func(key uint64, o nodeOwner, retired []uint32) (nodeOwner, error) {
		if o.packed && oldPack == nil {
			o = nodeOwner{}
		}
		if len(retired) == 0 {
			switch {
			case o.size == 0:
				return nodeOwner{}, nil
			case !o.packed:
				return nodeOwner{size: o.size, prior: true}, nil
			}
			segment = slices.Grow(segment[:0], int(o.size))[:o.size]
			if _, err := oldPack.ReadAt(segment, o.base); err != nil {
				return nodeOwner{}, nil
			}
			base, err := appendSegment(segment)
			return nodeOwner{size: o.size, base: base, packed: true, prior: true}, err
		}
		names, gone := map[uint32]string{}, map[uint32]struct{}{}
		if o.size > 0 {
			ix := coldIndex{coldIndexPath(t.coldRoot, key), 0, o.size}
			if o.packed {
				ix = coldIndex{prior.pack, o.base, o.size}
			}
			if n, r, err := readColdNames(ix); err == nil {
				names = n
				for _, h := range r {
					gone[h] = struct{}{}
				}
			}
			if !o.packed {
				t.deadFiles = append(t.deadFiles, ix.file)
			}
		}
		for _, h := range retired {
			gone[h] = struct{}{}
			delete(names, h)
		}
		data := encodeColdNames(names, gone)
		base, err := appendSegment(data)
		return nodeOwner{size: int64(len(data)), base: base, packed: true}, err
	}

	var offset int64
	var rec []byte
	next := 0
	for _, i := range live {
		n := &r.nodes[i]
		var o nodeOwner
		for ; next < len(owners) && owners[next].key <= n.key; next++ {
			if owners[next].key == n.key {
				o = owners[next].owner
			} else if !owners[next].owner.packed {
				t.deadFiles = append(t.deadFiles, coldIndexPath(t.coldRoot, owners[next].key))
			}
		}
		if o, err = carry(n.key, o, r.retired[i]); err != nil {
			return fail(err)
		}
		t.coldBytes += o.size
		rec = rec[:0]
		if i != 0 {
			l := loggedRecord{id: n.id, parent: r.nodes[n.parent].key, name: n.name, away: away[i]}
			for c := n.child; c >= 0; c = r.nodes[c].next {
				l.kids = append(l.kids, recordKid{r.nodes[c].name, r.nodes[c].key, r.nodes[c].id})
			}
			rec = appendRecord(rec, n.key, &l)
		}
		var b [slotSize]byte
		nodeSlot{key: n.key, offset: offset, length: int64(len(rec)), owner: o}.encode(b[:], x.epoch)
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
			o.apply(s)
			s.loaded, s.slot, s.stale = o, tableHeader, true
		}
	}
	for ; next < len(owners); next++ {
		if !owners[next].owner.packed {
			t.deadFiles = append(t.deadFiles, coldIndexPath(t.coldRoot, owners[next].key))
		}
	}
	if err := errors.Join(tw.Flush(), rw.Flush()); err != nil {
		return fail(err)
	}
	if pack != nil {
		if err := errors.Join(pw.Flush(), pack.Close()); err != nil {
			return fail(err)
		}
		t.coldPack = pack.Name()
	}
	t.index = x
	t.nodesBytes = tableHeader + x.count*slotSize + offset
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
		os.Rename(prior.nodes, filepath.Join(discard, "store-"+rand.Text()))
		if prior.pack != "" {
			os.Rename(prior.pack, filepath.Join(discard, "store-"+rand.Text()))
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
	fail := func(err error) error {
		for _, f := range []*os.File{x.table, x.records, x.runtime} {
			if f != nil {
				f.Close()
			}
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
	rec, slot, found, err := x.read(1)
	if err == nil && (!found || slot.own) {
		err = errNodeIndex
	}
	if err != nil {
		return fail(err)
	}
	t.index, t.next = x, h.next
	t.compacted, t.built, t.recordsEnd = int64(h.compacted), int64(h.built), int64(h.records)
	t.coldBytes, t.nodesBytes, t.runtimeBytes = int64(h.coldBytes), int64(h.nodesBytes), int64(h.runtimeBytes)
	t.coldPack = prior.pack
	t.deadKeys = h.dead
	for _, name := range h.garbage {
		t.deadFiles = append(t.deadFiles, filepath.Join(t.coldParent, name))
	}
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
	if slot.own {
		s.recordSize = slot.length
	}
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
		s.reading == 0 && s.indexing == nil && s.cached == s.logged
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
		x, version, location := t.index, s.version, locate(d)
		op := &coldOperation{done: make(chan struct{})}
		s.indexing = op
		s.reading++
		t.indexWrites++
		t.mu.Unlock()
		err := x.publish(d.key, rec, owner)
		t.mu.Lock()
		t.indexWrites--
		s.reading--
		close(op.done)
		s.indexing = nil
		if err != nil {
			t.spillFailed(err)
			return false
		}
		s.loaded = owner
		if rec != nil {
			t.nodesBytes += int64(len(rec)) - s.recordSize
			s.recordSize = int64(len(rec))
			t.recordFiles[d.key] = struct{}{}
		}
		if d.key >= x.base {
			if end := int64(d.key-x.base+1) * slotSize; end > t.runtimeBytes {
				t.nodesBytes += end - t.runtimeBytes
				t.runtimeBytes = end
			}
		}
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

// saveIndex makes the index locate every resident logged node as it is, at
// close, so that the next open can adopt it. The record of each stale node,
// the root's included, is appended to records rather than written to a file
// of its own, and each slot whose record or owner location changed is
// rewritten in place, adjacent slots in one write. A record file of its own
// that an appended record supersedes stays, unread, where it is: the key's
// next eviction overwrites it, its retirement removes it, and a rebuild drops
// it. Removing it here would cost close a directory update per file. The
// caller holds mu throughout, with no index write running, and flushes the
// index (syncIndex) before the manifest.
func (t *nodeTable) saveIndex() error {
	x := t.index
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
		f, at, err := x.slotFile(key, s.slot)
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
		if sizes[i] < 0 {
			continue
		}
		s.stale = false
		if s.recordSize > 0 {
			t.nodesBytes -= s.recordSize // no longer part of the index
			s.recordSize = 0
		}
		t.nodesBytes += sizes[i]
		if key >= x.base {
			if slots := int64(key-x.base+1) * slotSize; slots > t.runtimeBytes {
				t.nodesBytes += slots - t.runtimeBytes
				t.runtimeBytes = slots
			}
		}
	}
	t.recordsEnd = end
	return nil
}

// syncIndex makes durable everything the manifest makes authoritative, at
// close: the index files, the pack, the record and owner index files of
// their own this open wrote, and the directories naming them. Those an
// earlier open wrote were flushed by its close; those removed since were
// garbage. The caller holds mu, with no index write running.
func (t *nodeTable) syncIndex() error {
	x := t.index
	type flush struct {
		path string
		f    *os.File // already open
		err  error
	}
	files := []*flush{{f: x.table}, {f: x.records}, {f: x.runtime}}
	if t.coldPack != "" {
		files = append(files, &flush{path: t.coldPack})
	}
	dirs := map[string]bool{x.dir: true, t.coldParent: true}
	add := func(root string, keys map[uint64]struct{}) {
		for key := range keys {
			file := coldIndexPath(root, key)
			files = append(files, &flush{path: file})
			dirs[filepath.Dir(file)] = true
		}
	}
	add(x.dir, t.recordFiles)
	if t.coldRoot != "" {
		dirs[t.coldRoot] = true
		add(t.coldRoot, t.ownerFiles)
	}
	inParallel(files, func(fl *flush) {
		f := fl.f
		if f == nil {
			if f, fl.err = os.OpenFile(fl.path, os.O_RDWR, 0); fl.err != nil {
				return
			}
			defer f.Close()
		}
		fl.err = durable.Sync(f)
	})
	var flushes []*flush
	for dir := range dirs {
		flushes = append(flushes, &flush{path: dir})
	}
	inParallel(flushes, func(fl *flush) { fl.err = durable.Dir(fl.path) })
	for _, fl := range append(files, flushes...) {
		if fl.err != nil && !errors.Is(fl.err, os.ErrNotExist) {
			return fl.err
		}
	}
	clear(t.recordFiles)
	clear(t.ownerFiles)
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
	if !s.isRetired(h) {
		s.retire(h)
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
// logged nodes and the derived owners their indexes hold. The reads release
// the mutex; false means a fault or a node paged in meanwhile.
func (t *nodeTable) coldIDs(roots []uint64) ([]uint64, bool) {
	e := &enumeration{keys: make(map[uint64]struct{})}
	t.enumerations = append(t.enumerations, e)
	defer func() {
		t.enumerations = slices.DeleteFunc(t.enumerations, func(o *enumeration) bool { return o == e })
	}()
	var out []uint64
	for frontier := roots; len(frontier) > 0; {
		for _, key := range frontier {
			e.keys[key] = struct{}{}
			if t.byKey[key] != nil {
				return nil, false
			}
		}
		x, root, pack := t.index, t.coldRoot, t.coldPack
		t.mu.Unlock()
		var next, ids []uint64
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
			if o := slot.owner; o.size > 0 {
				ix := coldIndex{coldIndexPath(root, key), 0, o.size}
				if o.packed {
					ix = coldIndex{pack, o.base, o.size}
				}
				names, _, readErr := readColdNames(ix)
				if readErr != nil {
					if o.prior {
						continue // an adopted index that cannot be read holds no owners
					}
					err = readErr
					break
				}
				for h := range names {
					if _, ok := held[h]; !ok {
						ids = append(ids, derivedID(key, h))
					}
				}
			}
		}
		t.mu.Lock()
		if err != nil {
			t.cacheFailure(err)
			return nil, false
		}
		out = append(out, ids...)
		frontier = next
	}
	return out, !e.invalid
}

// deadBatch bounds the retired cold keys one trim reclaims.
const deadBatch = 64

// cleanDead removes the index files of a batch of retired cold keys and
// queues their children. Errors leave garbage for the next open.
func (t *nodeTable) cleanDead() {
	for range deadBatch {
		if t.coldSaved || len(t.deadKeys) == 0 && len(t.deadFiles) == 0 {
			return
		}
		x, root := t.index, t.coldRoot
		// The batch's I/O is in flight: close must not write a manifest that
		// omits what it popped.
		t.indexWrites++
		if len(t.deadKeys) == 0 {
			file := t.deadFiles[len(t.deadFiles)-1]
			t.deadFiles = t.deadFiles[:len(t.deadFiles)-1]
			t.mu.Unlock()
			if t.indexIO != nil {
				t.indexIO()
			}
			os.Remove(file)
			t.mu.Lock()
			t.indexWrites--
			continue
		}
		key := t.deadKeys[len(t.deadKeys)-1]
		t.deadKeys = t.deadKeys[:len(t.deadKeys)-1]
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		rec, slot, found, err := x.read(key)
		removed := func(file string) bool {
			err := os.Remove(file)
			return err == nil || errors.Is(err, os.ErrNotExist)
		}
		// A record file the slot no longer locates may remain too.
		recordGone := found && err == nil && removed(coldIndexPath(x.dir, key)) && slot.own
		ownerGone := found && err == nil && slot.owner.size > 0 && (slot.owner.packed || removed(coldIndexPath(root, key)))
		t.mu.Lock()
		t.indexWrites--
		if !found || err != nil {
			continue
		}
		if recordGone {
			t.nodesBytes -= slot.length
		}
		if ownerGone {
			t.coldBytes -= slot.owner.size
		}
		for _, k := range rec.kids {
			t.deadKeys = append(t.deadKeys, k.key)
		}
	}
}
