package export

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
)

// nodeLogMagic starts the binary identity log. Records follow back to back:
// an op byte, then uvarint fields, names as uvarint length and bytes, each
// relative to the parent directory's key.
var nodeLogMagic = []byte("ovnfs nodes 2\n")

// Identity log operations.
const (
	opCounter byte = 1 + iota // id (= key), parent key, name
	opPin                     // parent key, name, key; the id derives from parent key and name
	opPinAt                   // id, parent key, name, key: a derived id away from its name
	opMove                    // key, parent key, name
	opRemove                  // key
	opNext                    // counter watermark
)

// errNodeLog reports a log that is inconsistent rather than torn.
var errNodeLog = errors.New("invalid node identity log")

// openNodeTable loads the logged identities, leaving only the root resident.
// After a clean close it adopts the index the close left, reading neither
// the log nor the index beyond the root, unless the garbage they accumulated
// warrants a rebuild (rebuildReason). Otherwise it replays complete records,
// atomically compacts the logged identities and builds the index they page
// out to, carrying over the owner indexes a clean close left. A log of the
// earlier JSON format contributes only its counter watermark: its ids go
// stale rather than ever naming another file.
func openNodeTable(name string, list func(dir string) ([]os.FileInfo, error)) (*nodeTable, error) {
	start := time.Now()
	t := newNodeTable(list)
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, err
	}
	var logSize int64
	switch info, err := os.Stat(name); {
	case err == nil:
		logSize = info.Size()
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	// The registry owns the state lock before opening the export. Distinct
	// logs use distinct stores; only a store a clean close left for exactly
	// this log is kept, and its manifest goes before anything changes.
	t.coldParent = name + ".cache"
	head := t.takeOwners(logSize)
	prior, err := t.openColdStore(head)
	if err != nil {
		return nil, err
	}
	t.opened = nodeLoad{logBytes: logSize, why: "no clean-stop index"}
	if prior != nil {
		var pack int64
		if info, err := os.Stat(prior.pack); err == nil {
			pack = info.Size()
		}
		if t.opened.why = head.rebuildReason(pack); t.opened.why == "" {
			if err := t.adoptIndex(head, prior); err != nil {
				t.opened.why = fmt.Sprintf("clean-stop index unusable: %v", err)
			} else {
				t.opened.adopted, t.opened.keys = true, t.next-2
			}
		}
	}
	if !t.opened.adopted {
		if err := t.replay(name, prior); err != nil {
			return nil, err
		}
		t.deadFiles = append(t.deadFiles, garbageFiles(t.coldParent, head)...)
	}
	t.log, err = os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.index.close()
		return nil, err
	}
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	go t.syncLoop()
	t.opened.total = time.Since(start)
	return t, nil
}

// garbageFiles returns the files below the store at parent that the manifest
// head, if any, left to reclaim.
func garbageFiles(parent string, head *ownersHeader) []string {
	if head == nil {
		return nil
	}
	files := make([]string, len(head.garbage))
	for i, name := range head.garbage {
		files[i] = filepath.Join(parent, name)
	}
	return files
}

// replay loads the identity log name into t, compacts it and builds the
// index, carrying over the owner indexes prior locates, recording each
// phase's duration in t.opened.
func (t *nodeTable) replay(name string, prior *priorIndex) error {
	start := time.Now()
	data, err := os.ReadFile(name)
	r := newReplayTable(0)
	if bytes.HasPrefix(data, nodeLogMagic) {
		r = newReplayTable(len(data))
	}
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && len(data) == 0:
	case err != nil:
		return err
	case bytes.HasPrefix(data, nodeLogMagic):
		if err := r.replay(data[len(nodeLogMagic):]); err != nil {
			return err
		}
	case data[0] == '{':
		r.next = max(r.next, legacyWatermark(data))
	default:
		return errNodeLog
	}
	t.opened.logBytes, t.opened.keys = int64(len(data)), uint64(len(r.keys)-1)
	data = nil // the replayed names are copies
	t.next = r.next
	t.opened.replay = time.Since(start)
	start = time.Now()
	if t.compacted, err = r.compact(name); err != nil {
		return err
	}
	t.opened.compact = time.Since(start)
	start = time.Now()
	if err := t.buildIndex(r, prior); err != nil {
		return err
	}
	t.opened.index = time.Since(start)
	return nil
}

// nodeLoad describes how openNodeTable loaded the identities, for the
// startup log.
type nodeLoad struct {
	adopted  bool   // the index a clean close left, without replaying the log
	why      string // the log was replayed instead
	logBytes int64
	keys     uint64 // live keys replayed, or every key issued when adopted, but the root's
	// Durations of the replay phases and of the whole open.
	replay, compact, index, total time.Duration
}

// String is the startup log line.
func (l nodeLoad) String() string {
	mb := float64(l.logBytes) / 1e6
	if l.adopted {
		return fmt.Sprintf("node ids: adopted clean-stop index (%d keys issued, log %.1f MB) in %v", l.keys, mb, roundDuration(l.total))
	}
	return fmt.Sprintf("node ids: replayed %.1f MB (%d live keys; %s) in %v, compacted in %v, indexed in %v",
		mb, l.keys, l.why, roundDuration(l.replay), roundDuration(l.compact), roundDuration(l.index))
}

// roundDuration rounds d to about three significant digits.
func roundDuration(d time.Duration) time.Duration {
	unit := time.Nanosecond
	for d >= 1000*unit && unit < time.Millisecond {
		unit *= 10
	}
	return d.Round(unit)
}

// legacyWatermark returns the counter after every id a JSON-format log issued.
func legacyWatermark(data []byte) uint64 {
	var next uint64
	for line := range bytes.Lines(data) {
		var rec struct {
			Op string `json:"op"`
			ID uint64 `json:"id"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		switch rec.Op {
		case "alloc":
			next = max(next, rec.ID+1)
		case "next":
			next = max(next, rec.ID)
		}
	}
	return next
}

// replayed is a logged node while the log replays at open. Its tree links
// are indexes into replayTable.nodes, -1 for none, so the whole logged tree
// costs about a hundred bytes per node until the index is built from it.
type replayed struct {
	id, key uint64
	name    string
	parent  int32
	child   int32 // first child
	next    int32 // next sibling
	prev    int32 // previous sibling
	same    int32 // next node in the same name bucket
}

// replayTable is the logged tree the log describes, discarded once the log
// is compacted and the index built.
type replayTable struct {
	nodes   []replayed
	keys    map[uint64]int32 // live keys; the root is node 0
	names   map[uint64]int32 // first node of each (parent key, name) hash bucket
	derived []uint64         // every derived id placed, each at most once
	retired map[int32][]uint32
	next    uint64
}

// newReplayTable returns an empty logged tree sized for a log of logSize
// bytes.
func newReplayTable(logSize int) *replayTable {
	n := logSize/placeBytes + 1
	r := &replayTable{
		nodes:   make([]replayed, 1, n),
		keys:    make(map[uint64]int32, n),
		names:   make(map[uint64]int32, n),
		retired: map[int32][]uint32{},
		next:    2,
	}
	r.nodes[0] = replayed{id: 1, key: 1, parent: -1, child: -1, next: -1, prev: -1, same: -1}
	r.keys[1] = 0
	return r
}

// placeBytes is a little less than a record placing a node takes in a
// large compacted log, so that its replay never grows the tables, which
// would otherwise dominate it.
const placeBytes = 16

// replay applies records up to a torn tail: a record cut short by a crash,
// or the zeros a filesystem may leave after one. A derived id placed twice
// contradicts the log: retired derived ids are never issued again.
func (r *replayTable) replay(b []byte) error {
	lr := logReader{b: b}
	for len(lr.b) > 0 {
		if err := r.apply(&lr); err != nil {
			if !errors.Is(err, errTorn) && bytes.Count(lr.rest, []byte{0}) != len(lr.rest) {
				return err
			}
			break
		}
	}
	slices.Sort(r.derived)
	for i := 1; i < len(r.derived); i++ {
		if r.derived[i] == r.derived[i-1] {
			return errNodeLog
		}
	}
	r.derived = nil
	return nil
}

// errTorn reports a record that ends with the file.
var errTorn = errors.New("torn node log record")

// logReader decodes records, remembering where the current one began.
type logReader struct {
	b    []byte
	rest []byte // from the start of the record being decoded
	err  error
}

// uvarint decodes a field, recording errTorn once the bytes run out.
func (r *logReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	switch {
	case n == 0:
		r.err = errTorn
	case n < 0:
		r.err = errNodeLog
	default:
		r.b = r.b[n:]
	}
	return v
}

// name decodes a length-prefixed basename.
func (r *logReader) name() string {
	n := r.uvarint()
	if r.err != nil {
		return ""
	}
	if n > uint64(len(r.b)) {
		r.err = errTorn
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	if s == "" || s == "." || s == ".." || strings.Contains(s, "/") {
		r.err = errNodeLog
	}
	return s
}

// apply decodes and replays one record.
func (r *replayTable) apply(lr *logReader) error {
	lr.rest = lr.b
	op := lr.b[0]
	lr.b = lr.b[1:]
	switch op {
	case opCounter:
		id, parent, name := lr.uvarint(), lr.uvarint(), lr.name()
		if lr.err != nil {
			return lr.err
		}
		if id >= derivedBit {
			return errNodeLog
		}
		return r.place(id, id, parent, name)
	case opPin:
		parent, name, key := lr.uvarint(), lr.name(), lr.uvarint()
		if lr.err != nil {
			return lr.err
		}
		if parent >= maxDerivingKey {
			return errNodeLog
		}
		return r.place(derivedID(parent, nameHash(name)), key, parent, name)
	case opPinAt:
		id, parent, name, key := lr.uvarint(), lr.uvarint(), lr.name(), lr.uvarint()
		if lr.err != nil {
			return lr.err
		}
		if id&derivedBit == 0 || id >= idLimit {
			return errNodeLog
		}
		return r.place(id, key, parent, name)
	case opMove:
		key, parent, name := lr.uvarint(), lr.uvarint(), lr.name()
		if lr.err != nil {
			return lr.err
		}
		return r.move(key, parent, name)
	case opRemove:
		key := lr.uvarint()
		if lr.err != nil {
			return lr.err
		}
		if i, ok := r.keys[key]; ok && i != 0 {
			r.drop(i)
		}
		return nil
	case opNext:
		next := lr.uvarint()
		if lr.err != nil {
			return lr.err
		}
		r.next = max(r.next, next)
		return nil
	}
	return errNodeLog
}

// bucket hashes a parent key and a name for the names index.
func bucket(parent uint64, name string) uint64 {
	h := uint64(14695981039346656037)
	for i := range 8 {
		h ^= parent >> (8 * i) & 255
		h *= 1099511628211
	}
	for i := range len(name) {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return h
}

// child returns parent's child name, or -1.
func (r *replayTable) child(parent int32, name string) int32 {
	i, ok := r.names[bucket(r.nodes[parent].key, name)]
	for ; ok && i >= 0; i = r.nodes[i].same {
		if r.nodes[i].parent == parent && r.nodes[i].name == name {
			return i
		}
	}
	return -1
}

// attach links node i under parent.
func (r *replayTable) attach(i, parent int32) {
	n := &r.nodes[i]
	n.parent, n.prev, n.next = parent, -1, r.nodes[parent].child
	if n.next >= 0 {
		r.nodes[n.next].prev = i
	}
	r.nodes[parent].child = i
	h := bucket(r.nodes[parent].key, n.name)
	n.same = -1
	if head, ok := r.names[h]; ok {
		n.same = head
	}
	r.names[h] = i
}

// detach unlinks node i from its parent.
func (r *replayTable) detach(i int32) {
	n := r.nodes[i]
	if n.prev >= 0 {
		r.nodes[n.prev].next = n.next
	} else {
		r.nodes[n.parent].child = n.next
	}
	if n.next >= 0 {
		r.nodes[n.next].prev = n.prev
	}
	h := bucket(r.nodes[n.parent].key, n.name)
	if head := r.names[h]; head == i {
		if n.same >= 0 {
			r.names[h] = n.same
		} else {
			delete(r.names, h)
		}
	} else {
		for j := head; j >= 0; j = r.nodes[j].same {
			if r.nodes[j].same == i {
				r.nodes[j].same = n.same
				break
			}
		}
	}
}

// place replays a logged node under the directory keyed parent.
func (r *replayTable) place(id, key, parent uint64, name string) error {
	p, ok := r.keys[parent]
	if !ok || id < 2 || key < 2 || r.child(p, name) >= 0 {
		return errNodeLog
	}
	if _, live := r.keys[key]; live {
		return errNodeLog
	}
	if id&derivedBit != 0 {
		r.derived = append(r.derived, id)
	}
	i := int32(len(r.nodes))
	r.nodes = append(r.nodes, replayed{id: id, key: key, name: name, child: -1})
	r.attach(i, p)
	r.keys[key] = i
	r.next = max(r.next, key+1)
	return nil
}

// move replays a logged node's move, retiring whatever it replaced.
func (r *replayTable) move(key, parent uint64, name string) error {
	n, ok := r.keys[key]
	d, okParent := r.keys[parent]
	if !ok || !okParent || n == 0 {
		return errNodeLog
	}
	for c := d; c >= 0; c = r.nodes[c].parent {
		if c == n {
			return errNodeLog
		}
	}
	if dst := r.child(d, name); dst >= 0 && dst != n {
		r.drop(dst)
	}
	r.detach(n)
	r.nodes[n].name = name
	r.attach(n, d)
	return nil
}

// drop retires node i's subtree: its keys die, and the derived ids it held
// stay reserved in the live directories they derive from.
func (r *replayTable) drop(i int32) {
	r.detach(i)
	stack, dropped := []int32{i}, []int32(nil)
	for len(stack) > 0 {
		j := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		dropped = append(dropped, j)
		delete(r.keys, r.nodes[j].key)
		delete(r.retired, j)
		for c := r.nodes[j].child; c >= 0; c = r.nodes[c].next {
			stack = append(stack, c)
		}
	}
	for _, j := range dropped {
		if id := r.nodes[j].id; id&derivedBit != 0 {
			if o, ok := r.keys[id>>hashBits&(maxDerivingKey-1)]; ok {
				r.retired[o] = append(r.retired[o], uint32(id&(1<<hashBits-1)))
			}
		}
	}
}

// compact atomically replaces the log with the logged nodes, parents first,
// and the counter watermark, returning the new log's size.
func (r *replayTable) compact(name string) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(name), ".nodes-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)
	b := append([]byte(nil), nodeLogMagic...)
	var size int64
	queue := []int32{0}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for c := r.nodes[p].child; c >= 0; c = r.nodes[c].next {
			k := &r.nodes[c]
			b = appendPlace(b, k.id, k.key, r.nodes[p].key, k.name)
			queue = append(queue, c)
		}
		if len(b) >= 1<<16 {
			if _, err := w.Write(b); err != nil {
				tmp.Close()
				return 0, err
			}
			size += int64(len(b))
			b = b[:0]
		}
	}
	b = binary.AppendUvarint(append(b, opNext), r.next)
	size += int64(len(b))
	_, err = w.Write(b)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = durable.Sync(tmp)
	}
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return 0, err
	}
	return size, syncNodeDirectory(filepath.Dir(name))
}

// appendNode encodes the record that places logged node k at its position.
func appendNode(b []byte, k *node) []byte {
	return appendPlace(b, k.id, k.key, k.parent.key, k.name)
}

// appendPlace encodes the record placing a logged node under the directory
// keyed parent.
func appendPlace(b []byte, id, key, parent uint64, name string) []byte {
	switch {
	case id == key:
		b = binary.AppendUvarint(append(b, opCounter), id)
		b = binary.AppendUvarint(b, parent)
		return appendName(b, name)
	case parent < maxDerivingKey && id == derivedID(parent, nameHash(name)):
		b = binary.AppendUvarint(append(b, opPin), parent)
	default:
		b = binary.AppendUvarint(append(b, opPinAt), id)
		b = binary.AppendUvarint(b, parent)
	}
	b = appendName(b, name)
	return binary.AppendUvarint(b, key)
}

// appendName encodes a basename.
func appendName(b []byte, name string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(name))), name...)
}

// recordNode queues the record placing logged node k.
func (t *nodeTable) recordNode(k *node) {
	if t.log != nil && t.logErr == nil {
		t.pending = appendNode(t.pending, k)
	}
}

// recordMove queues the move of logged node k to its current position.
func (t *nodeTable) recordMove(k *node) {
	if t.log != nil && t.logErr == nil {
		b := binary.AppendUvarint(append(t.pending, opMove), k.key)
		b = binary.AppendUvarint(b, k.parent.key)
		t.pending = appendName(b, k.name)
	}
}

// recordRemove queues the retirement of the subtree of the logged node keyed key.
func (t *nodeTable) recordRemove(key uint64) {
	if t.log != nil && t.logErr == nil {
		t.pending = binary.AppendUvarint(append(t.pending, opRemove), key)
	}
}

// flush appends the current operation's records in one write while the node
// mutex is held, so a crash tears at most the last operation.
func (t *nodeTable) flush() {
	if len(t.pending) == 0 {
		return
	}
	if t.log != nil && t.logErr == nil {
		if _, err := t.log.Write(t.pending); err != nil {
			t.logErr = err
		} else {
			t.dirty = true
		}
	}
	t.pending = t.pending[:0]
}

// syncLoop groups durable flushes without retaining records in userspace
// buffers. It flushes outside the node mutex: an fsync (FlushFileBuffers on
// Windows, often milliseconds) must not stall every lookup and allocation.
// Records appended meanwhile mark the log dirty again for the next flush.
func (t *nodeTable) syncLoop() {
	defer close(t.done)
	timer := time.NewTicker(20 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			t.mu.Lock()
			dirty := t.dirty
			t.dirty = false
			t.mu.Unlock()
			if !dirty {
				continue
			}
			if err := durable.Sync(t.log); err != nil {
				t.mu.Lock()
				if t.logErr == nil {
					t.logErr = err
				}
				t.mu.Unlock()
			}
		case <-t.stop:
			return
		}
	}
}

// close stops the flush worker and makes every appended operation durable.
func (t *nodeTable) close() error {
	t.closeOnce.Do(func() {
		if t.coldStop != nil {
			close(t.coldStop) // reclamation never delays the next daemon
		}
		if t.stop != nil {
			close(t.stop)
			<-t.done
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.log != nil {
			// A write still running would change the index after the
			// manifest describes it.
			complete := t.indexWrites == 0 && t.completeIndex()
			if complete && t.index != nil {
				complete = t.saveIndex() == nil && t.syncIndex() == nil
			}
			if err := durable.Sync(t.log); err != nil && t.logErr == nil {
				t.logErr = err
			}
			info, statErr := t.log.Stat()
			if err := t.log.Close(); err != nil && t.logErr == nil {
				t.logErr = err
			}
			if complete && statErr == nil && t.logErr == nil {
				// Without the manifest the next open discards the index.
				_ = t.saveColdStore(info.Size())
			}
			t.coldSaved = true // a request still running never pages a node out
		}
		if t.index != nil {
			t.index.close()
		}
		if t.coldParent == "" && t.coldRoot != "" {
			// Tables without a log own a private temporary store, not state.
			go os.RemoveAll(t.coldRoot)
		}
		// State-backed indexes stay in place until the next open detaches
		// them. Neither deletion nor its completion holds up close.
	})
	return errors.Join(t.logErr, t.cacheErr)
}
