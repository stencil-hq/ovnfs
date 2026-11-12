package export

import (
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Cold identities retain their exact names in a disk index, beside the hashes
// of retired names. A listing alone cannot reconstruct an issued id after an
// external removal or a new name with the same hash. These files are not the
// identity log: a clean close completes them for the next open to adopt
// (nodes_reclaim.go), while a crash discards them and handles issued before it
// resolve by listing again. Each file is a sorted array of 16-byte (hash, name
// length, offset) records, followed by name bytes; a zero length marks a
// retired hash. Recovery reads at most one directory at a time.
func (t *nodeTable) coldPath(d *node) string {
	return coldIndexPath(t.coldRoot, d.key)
}

// coldIndex locates a directory's published index: its own file, or a segment
// of the pack a clean close wrote for the next open.
type coldIndex struct {
	file       string
	base, size int64
}

// coldIndexOf returns where d's published index lives, under mu.
func (t *nodeTable) coldIndexOf(d *node) coldIndex {
	s := d.dir
	if s.packed {
		return coldIndex{t.coldPack, s.base, s.coldBytes}
	}
	return coldIndex{t.coldPath(d), 0, s.coldBytes}
}

// Low key bits distribute consecutive directory keys across 256 shards.
func coldIndexPath(root string, key uint64) string {
	return filepath.Join(root, fmt.Sprintf("%02x", key&255), strconv.FormatUint(key, 10))
}

// readColdName searches the sorted index without allocating its other owners.
// ix is the last published location, validated again by the caller after I/O.
// retired reports a removed name's hash, which has no owner.
func readColdName(ix coldIndex, hash uint32) (name string, retired bool, err error) {
	f, err := os.Open(ix.file)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	size := ix.size
	var row [16]byte
	if _, err := f.ReadAt(row[:4], ix.base); err != nil {
		return "", false, err
	}
	count := int64(binary.LittleEndian.Uint32(row[:4]))
	end := 4 + count*16
	if end > size {
		return "", false, io.ErrUnexpectedEOF
	}
	lo, hi := int64(0), count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := f.ReadAt(row[:], ix.base+4+mid*16); err != nil {
			return "", false, err
		}
		h := binary.LittleEndian.Uint32(row[:4])
		if h < hash {
			lo = mid + 1
			continue
		}
		if h > hash {
			hi = mid
			continue
		}
		length, offset := uint64(binary.LittleEndian.Uint32(row[4:8])), binary.LittleEndian.Uint64(row[8:])
		if offset < uint64(end) || offset > uint64(size) || length > uint64(size)-offset {
			return "", false, io.ErrUnexpectedEOF
		}
		if length == 0 {
			return "", true, nil
		}
		name := make([]byte, int(length))
		if _, err := f.ReadAt(name, ix.base+int64(offset)); err != nil {
			return "", false, err
		}
		return string(name), false, nil
	}
	return "", false, nil
}

// readColdNames reads one directory's issued identities and retired hashes for
// a rewrite or subtree invalidation. The temporary allocation is at most one
// directory's identities.
func readColdNames(ix coldIndex) (names map[uint32]string, retired []uint32, err error) {
	data, err := readColdIndex(ix)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < 4 {
		return nil, nil, io.ErrUnexpectedEOF
	}
	n := binary.LittleEndian.Uint32(data[:4])
	if uint64(n)*16+4 > uint64(len(data)) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	names = make(map[uint32]string, n)
	// SAFETY: readColdIndex returned exclusively owned bytes. Nothing mutates
	// them after this point; name substrings keep that allocation alive.
	text := unsafe.String(unsafe.SliceData(data), len(data))
	for i := uint32(0); i < n; i++ {
		row := data[4+int(i)*16:]
		h, size, offset := binary.LittleEndian.Uint32(row[:4]), binary.LittleEndian.Uint32(row[4:8]), binary.LittleEndian.Uint64(row[8:16])
		if offset > uint64(len(data)) || uint64(size) > uint64(len(data))-offset {
			return nil, nil, io.ErrUnexpectedEOF
		}
		if size == 0 {
			retired = append(retired, h)
			continue
		}
		names[h] = text[offset : offset+uint64(size)]
	}
	return names, retired, nil
}

// readColdIndex reads a published index whole, after checking the file holds
// it: an adopted manifest's size is not trusted for an allocation.
func readColdIndex(ix coldIndex) ([]byte, error) {
	f, err := os.Open(ix.file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if ix.base < 0 || ix.size < 0 || ix.size > info.Size()-ix.base {
		return nil, io.ErrUnexpectedEOF
	}
	data := make([]byte, ix.size)
	if _, err := f.ReadAt(data, ix.base); err != nil {
		return nil, err
	}
	return data, nil
}

// spillNames preserves issued unlogged ownership before reclaiming its memory.
func (t *nodeTable) spillNames(d *node) (bool, error) {
	if t.coldSaved {
		return false, nil // the next open adopts the index as close left it
	}
	s := d.dir
	if s.cold && !s.coldDirty {
		return true, nil
	}
	names := make(map[uint32]string)
	var retired []uint32
	var err error
	if s.cold && !s.coldLoaded {
		gen, location, ix := s.gen, locate(d), t.coldIndexOf(d)
		if s.indexing != nil {
			return false, nil
		}
		s.reading++
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		names, retired, err = readColdNames(ix)
		t.mu.Lock()
		s.reading--
		if t.byID[d.id] != d || s.gen != gen || !location.holds(d) {
			return false, nil
		}
	}
	if err != nil {
		if !s.prior {
			t.cacheFailure(err) // this index already owns evicted identities
			return false, err
		}
		t.forgetPrior(d)
		names, retired = make(map[uint32]string), nil
	}
	if !t.ownersToWrite(d, names, retired) {
		return true, nil
	}
	if t.coldRoot == "" {
		if t.coldInit {
			return false, nil
		}
		t.coldInit = true
		parent := t.coldParent
		d.dir.reading++
		t.mu.Unlock()
		root, err := os.MkdirTemp(parent, "instance-")
		t.mu.Lock()
		d.dir.reading--
		t.coldInit = false
		if err != nil {
			return false, err
		}
		t.coldRoot = root
		if t.byID[d.id] != d {
			return false, nil
		}
		// Ownership may have changed during directory creation. Start over.
		return t.spillNames(d)
	}
	if s.indexing != nil {
		return false, nil
	}
	gen, version, location := s.gen, s.version, locate(d)
	resident := !s.cold || s.coldLoaded
	target := t.coldPath(d)
	gone := maps.Clone(s.retired)
	s.indexing = &coldOperation{done: make(chan struct{})}
	s.reading++
	t.indexWrites++
	t.mu.Unlock()
	// Only this operation owns the temporary file. Publish it after revalidation.
	if t.indexIO != nil {
		t.indexIO()
	}
	err = os.MkdirAll(filepath.Dir(target), 0700)
	var file string
	var size int64
	if err == nil {
		file, size, err = writeColdNames(filepath.Dir(target), names, gone)
	}
	t.mu.Lock()
	t.indexWrites--
	s.reading--
	close(s.indexing.done)
	s.indexing = nil
	valid := t.byID[d.id] == d && s.gen == gen && s.version == version && location.holds(d)
	if err != nil {
		return false, err
	}
	if !valid {
		// The uncommitted replacement never owns any handles.
		t.mu.Unlock()
		os.Remove(file)
		t.mu.Lock()
		return false, nil
	}
	// Renaming the temporary file may block too. Keep indexing set so readers
	// use resident owners, and leave coldDirty set if anything changes meanwhile.
	s.indexing = &coldOperation{done: make(chan struct{})}
	s.reading++
	t.indexWrites++
	t.mu.Unlock()
	err = os.Rename(file, target)
	if err != nil {
		os.Remove(file)
	}
	t.mu.Lock()
	t.indexWrites--
	s.reading--
	close(s.indexing.done)
	s.indexing = nil
	if err != nil {
		return false, err
	}
	t.coldBytes += size - s.coldBytes
	s.coldBytes, s.cold, s.prior, s.packed = size, true, false, false
	t.ownerFiles[d.key] = struct{}{}
	// Publishing the first index does not discard its resident owners. A
	// guard acquired during the write must still finish without recovery.
	s.coldLoaded = s.coldLoaded || resident
	valid = t.byID[d.id] == d && s.gen == gen && s.version == version && location.holds(d)
	s.coldDirty = !valid
	return valid, nil
}

// ownersToWrite completes names, read from d's index file with its retired
// hashes (both empty when nothing was read), into the owners d's file must
// hold: resident leaves and unlogged children, without retired or logged
// hashes. It reports whether the file must be rewritten, else clears coldDirty.
func (t *nodeTable) ownersToWrite(d *node, names map[uint32]string, retired []uint32) bool {
	s := d.dir
	for _, h := range retired {
		s.retire(h)
	}
	// Resident owners of a loaded or unindexed directory are complete, and
	// coldDirty says they differ from the file. Otherwise compare with it.
	changed := !s.cold || s.coldLoaded || len(s.retired) != len(retired)
	for h, name := range s.leaves {
		changed = changed || names[h] != name
		names[h] = name
	}
	for name, k := range d.kids {
		if k.key == 0 {
			changed = changed || names[nameHash(name)] != name
			names[nameHash(name)] = name
		}
	}
	for h := range names {
		if _, held := s.held[h]; held || s.isRetired(h) || t.byID[derivedID(d.key, h)] != nil && t.byID[derivedID(d.key, h)].key != 0 {
			delete(names, h)
			changed = true
		}
	}
	if !changed || len(names) == 0 && len(s.retired) == 0 {
		s.coldDirty = false
		return false
	}
	return true
}

// writeColdNames writes a directory's owners and retired hashes to a new
// temporary file below root.
func writeColdNames(root string, names map[uint32]string, retired map[uint32]struct{}) (file string, size int64, err error) {
	data := encodeColdNames(names, retired)
	f, err := os.CreateTemp(root, ".names-*")
	if err != nil {
		return "", 0, err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		os.Remove(f.Name())
		return "", 0, err
	}
	return f.Name(), int64(len(data)), nil
}

// encodeColdNames lays out an index file. names and retired are disjoint.
func encodeColdNames(names map[uint32]string, retired map[uint32]struct{}) []byte {
	hashes := make([]uint32, 0, len(names)+len(retired))
	for h := range names {
		hashes = append(hashes, h)
	}
	for h := range retired {
		hashes = append(hashes, h)
	}
	slices.Sort(hashes)
	data := make([]byte, 4+16*len(hashes))
	binary.LittleEndian.PutUint32(data[:4], uint32(len(hashes)))
	for i, h := range hashes {
		row := data[4+16*i:]
		binary.LittleEndian.PutUint32(row[:4], h)
		binary.LittleEndian.PutUint32(row[4:8], uint32(len(names[h]))) // zero for a retired hash
		binary.LittleEndian.PutUint64(row[8:16], uint64(len(data)))
		data = append(data, names[h]...)
	}
	return data
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
	s := d.dir
	if !s.prior {
		return
	}
	t.coldBytes -= s.coldBytes
	s.prior, s.packed, s.cold, s.coldLoaded, s.coldBytes = false, false, false, false, 0
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

// loadCold recovers a whole directory, retaining its state while I/O runs
// outside mu. Namespace changes discard the snapshot; callers restart their
// path traversal before using any pointers obtained before the unlock.
func (t *nodeTable) loadCold(d *node) bool {
	if t.cacheErr != nil {
		return false
	}
	s := d.dir
	if s == nil || !s.cold || s.coldLoaded {
		return true
	}
	names, bytes, valid := t.readOwners(d)
	if !valid {
		return false
	}
	if s.coldLoaded {
		return true
	}
	adopted := false
	for h, name := range names {
		if !t.takenExceptCold(d, h) {
			t.addLeaf(d, name)
			adopted = true
		}
	}
	if adopted {
		// Any retained name owns the immutable index buffer, including its
		// row table. Keep the whole allocation charged until eviction.
		t.charge(d, bytes)
	}
	s.coldLoaded = true
	return true
}

// recoverPaths pins directory caches until the caller finishes its in-memory
// operation. It restarts from the root after each read, including ancestor
// moves, rather than allowing a stale pointer to publish a path identity.
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
	pageLogged    recovery = iota // the logged nodes
	recoverOwners                 // also each directory's cold owners
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

func (t *nodeTable) keepPaths(how recovery, paths ...string) (*pathPins, error) {
	pins := pathPinPool.Get().(*pathPins)
	for failures := 0; failures < 16; {
		restart := false
		for _, p := range paths {
			d := t.root
			for {
				if s := d.state(); s != nil {
					if !slices.Contains(pins.states, s) {
						s.reading++
						pins.states = append(pins.states, s)
					}
					if how == recoverOwners && s.cold && !s.coldLoaded {
						if t.spillErr != nil {
							return pins, syscall.ENOSPC
						}
						if !t.loadCold(d) {
							if t.cacheErr != nil {
								return pins, t.cacheErr
							}
							failures++
						}
						restart = true
						break
					}
				}
				name, rest, _ := strings.Cut(p, "/")
				p = rest
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

// readOwners keeps the authoritative index and operation state alive, without
// admitting the recovered names. Degraded lookups use this transient result so
// an unavailable spill volume cannot turn retained-handle reads into growth.
// coldOperation remains owned by its waiters after it leaves dirState.
// Read results are immutable and shared, so serialization alone never consumes
// a namespace retry or makes an issued owner appear absent.
type coldOperation struct {
	done     chan struct{}
	read     bool
	single   bool
	prior    bool // read an adopted index, whose failure only loses its owners
	hash     uint32
	name     string
	gone     bool // a single read found hash retired
	gen      uint64
	location nodeLocation
	names    map[uint32]string
	bytes    int64
	retired  []uint32
	err      error
}

func (t *nodeTable) readOwners(d *node) (map[uint32]string, int64, bool) {
	s := d.dir
	for {
		if t.cacheErr != nil || t.byID[d.id] != d {
			return nil, 0, false
		}
		if op := s.indexing; op != nil {
			t.mu.Unlock()
			if t.indexWait != nil {
				t.indexWait()
			}
			<-op.done
			t.mu.Lock()
			if !op.read || op.single {
				continue
			}
			return t.readResult(d, op)
		}
		op := &coldOperation{done: make(chan struct{}), read: true, prior: s.prior, gen: s.gen, location: locate(d)}
		s.indexing = op
		s.reading++
		ix := t.coldIndexOf(d)
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		names, retired, err := readColdNames(ix)
		t.mu.Lock()
		s.reading--
		op.names, op.bytes, op.retired, op.err = names, ix.size, retired, err
		close(op.done)
		s.indexing = nil
		return t.readResult(d, op)
	}
}

// readResult validates a finished read for d. Retired hashes it found join
// d's resident ones; an unreadable adopted index yields no owners.
func (t *nodeTable) readResult(d *node, op *coldOperation) (map[uint32]string, int64, bool) {
	if t.cacheErr != nil || t.byID[d.id] != d || d.dir.gen != op.gen || !op.location.holds(d) {
		return nil, 0, false
	}
	if op.err != nil {
		if op.prior {
			t.forgetPrior(d)
			return nil, 0, true
		}
		t.cacheFailure(op.err)
		return nil, 0, false
	}
	for _, h := range op.retired {
		d.dir.retire(h)
	}
	if op.gone {
		d.dir.retire(op.hash)
	}
	return op.names, op.bytes, true
}

// batchRecovery distinguishes a directory rewalk from sparse retained handles.
// Four consecutive different misses in one directory warrant a batch even
// under pressure. The history is constant-size and carries no owner pointers.
func (t *nodeTable) batchRecovery(d *node, hash uint32) bool {
	if t.recoveryKey != d.key || t.recoveryHash == hash {
		t.recoveryRun = 1
	} else if t.recoveryRun < 4 {
		t.recoveryRun++
	}
	t.recoveryKey, t.recoveryHash = d.key, hash
	return t.spillErr == nil && (t.recoveryRun >= 4 || d.dir.coldBytes <= (t.cacheLimit-t.cacheUsed)/4)
}

// readOwner shares an in-flight full read when available, but otherwise reads
// only the requested hash. Partial admission never marks the index fully loaded.
func (t *nodeTable) readOwner(d *node, hash uint32) (string, bool) {
	s := d.dir
	for {
		if t.cacheErr != nil || t.byID[d.id] != d {
			return "", false
		}
		if op := s.indexing; op != nil {
			t.mu.Unlock()
			if t.indexWait != nil {
				t.indexWait()
			}
			<-op.done
			t.mu.Lock()
			if !op.read {
				continue
			}
			if !op.single {
				names, _, valid := t.readResult(d, op)
				// A sparse owner must not retain an entire shared bulk read.
				return strings.Clone(names[hash]), valid
			}
			if op.hash != hash {
				continue
			}
			_, _, valid := t.readResult(d, op)
			return op.name, valid
		}
		op := &coldOperation{done: make(chan struct{}), read: true, single: true, prior: s.prior, hash: hash, gen: s.gen, location: locate(d)}
		s.indexing = op
		s.reading++
		ix := t.coldIndexOf(d)
		t.mu.Unlock()
		if t.indexIO != nil {
			t.indexIO()
		}
		name, gone, err := readColdName(ix, hash)
		t.mu.Lock()
		s.reading--
		op.name, op.gone, op.err = name, gone, err
		close(op.done)
		s.indexing = nil
		_, _, valid := t.readResult(d, op)
		return name, valid
	}
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
// Healthy ID requests admit partial results and batch dense rewalks; transient
// callers leave admission and mutation preparation to their own operations.
func (t *nodeTable) issuedID(p string, admit bool) (uint64, bool) {
	dir, name := split(p)
	for {
		if id, ok := t.residentID(p); ok {
			return id, true
		}
		d := t.find(dir)
		if d == nil || d.dir == nil || !d.dir.cold || d.dir.coldLoaded {
			return 0, false
		}
		h := nameHash(name)
		if admit && t.batchRecovery(d, h) {
			if !t.loadCold(d) && t.cacheErr != nil {
				return 0, false
			}
			continue
		}
		owner, valid := t.readOwner(d, h)
		if t.find(p) != nil {
			continue
		}
		if _, ok := t.leafID(p); ok {
			continue
		}
		if !valid {
			if t.cacheErr != nil {
				return 0, false
			}
			continue
		}
		if t.find(dir) != d {
			continue
		}
		if owner == name && !t.takenExceptCold(d, h) {
			if admit && t.spillErr == nil {
				t.addLeaf(d, owner)
			}
			return derivedID(d.key, h), true
		}
		return 0, false
	}
}

// Retired directory keys are never reused. Delete their indexes, and the
// records of retired logged nodes, after any in-flight replacement has
// finished, outside the node mutex. Nothing is deleted after close, which
// leaves the remaining garbage to the next open: a request still running
// must not change the store the manifest describes.
type coldGarbage struct {
	path   string
	dir    *node
	record bool // the logged record, not the owner index
}

func (t *nodeTable) cleanCold() {
	for !t.coldSaved && len(t.coldGarbage) > 0 {
		old := t.coldGarbage[0]
		t.coldGarbage[0] = coldGarbage{}
		t.coldGarbage = t.coldGarbage[1:]
		if len(t.coldGarbage) == 0 {
			t.coldGarbage = nil
		}
		// In flight from here: close must not write a manifest without it.
		t.indexWrites++
		s := old.dir.dir
		for s.indexing != nil {
			done := s.indexing.done
			t.mu.Unlock()
			<-done
			t.mu.Lock()
		}
		size := s.coldBytes
		if old.record {
			size = s.recordSize
		}
		t.mu.Unlock()
		err := os.Remove(old.path)
		t.mu.Lock()
		t.indexWrites--
		if err != nil && !os.IsNotExist(err) {
			t.coldGarbage = append(t.coldGarbage, old)
			return
		}
		if old.record {
			t.nodesBytes -= size
			s.recordSize = 0
			continue
		}
		t.coldBytes -= size
		s.coldBytes = 0
		s.cold = false
	}
	t.cleanDead()
}
