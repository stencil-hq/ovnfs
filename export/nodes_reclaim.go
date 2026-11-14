package export

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/internal/durable"
)

// ownersName is the manifest a clean close leaves in the store once the
// logged node index holds every logged node as it was and every issued owner
// and retired hash (ownersHeader).
const ownersName = "owners"

var ownersMagic = []byte("ovnfs owners 4\n")

// Manifests of earlier layouts start with these. An open rebuilds what they
// describe (rebuildReason), carrying its owners over. Version 3's owner
// indexes and records could be files of their own; version 1 had no logged
// node index, so its manifest lists every directory's owner index itself,
// each a file of its own or a segment of its pack.
var (
	legacyOwnersMagic = []byte("ovnfs owners 3\n")
	firstOwnersMagic  = []byte("ovnfs owners 1\n")
)

// takeOwners reads the manifest a clean close left and removes it durably
// before the open changes anything, so that a crash from then on discards
// the index. It returns the manifest when it describes the identity log of
// logSize bytes, else nil.
func (t *nodeTable) takeOwners(logSize int64) *ownersHeader {
	manifest := filepath.Join(t.coldParent, ownersName)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil
	}
	if os.Remove(manifest) != nil || durable.Dir(t.coldParent) != nil {
		return nil
	}
	head, ok := parseOwners(data)
	if !ok || head.logSize != uint64(logSize) {
		return nil
	}
	return &head
}

// openColdStore keeps the store a clean close left, as head describes it,
// returning its closed logged node index, or else, without head, detaches
// the entire previous store without visiting its files. The state lock
// excludes other users of coldParent. The discard directory is separate:
// even a cleaner still returning from I/O after close cannot reach the next
// instance's authoritative owners. Windows never renames an open index:
// readers belong to the previous, already closed export.
func (t *nodeTable) openColdStore(head *ownersHeader) (*priorIndex, error) {
	discard := t.coldParent + ".discard"
	if err := os.MkdirAll(discard, 0700); err != nil {
		return nil, err
	}
	var prior *priorIndex
	if head != nil {
		prior = t.adoptColdStore(head, discard)
	}
	if prior == nil {
		// Publish the detached tree with one rename. No empty container is
		// exposed to an older cleaner still returning from a filesystem call.
		old := filepath.Join(discard, "store-"+rand.Text())
		if err := os.Rename(t.coldParent, old); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.MkdirAll(t.coldParent, 0700); err != nil {
			return nil, err
		}
	}
	t.coldStop = make(chan struct{})
	go func() {
		// Errors leave garbage for the next open; they cannot invalidate
		// any live owner or turn a cache cleanup failure into a serving fault.
		reclaimCold(discard, t.coldStop, t.reclaimIO)
	}()
	return prior, nil
}

// adoptColdStore finds the owner index instance and the closed logged node
// index the manifest head names, which the open adopts, or reads and
// detaches when it rebuilds, and detaches every other entry of the store.
// It returns nil when the store does not hold what head names.
func (t *nodeTable) adoptColdStore(head *ownersHeader, discard string) *priorIndex {
	if !t.detachExcept(t.coldParent, discard, head.instance, head.nodes) {
		return nil
	}
	prior := &priorIndex{owners: head.owners, legacy: head.legacy}
	if head.nodes != "" {
		prior.nodes = filepath.Join(t.coldParent, head.nodes)
	}
	if head.instance != "" {
		prior.instance = filepath.Join(t.coldParent, head.instance)
		// A legacy instance keeps its owner index files until the rebuild
		// discards it whole.
		if !head.legacy && !t.detachExcept(prior.instance, discard, head.pack) {
			return nil
		}
		if head.pack != "" {
			prior.pack = filepath.Join(prior.instance, head.pack)
		}
	}
	return prior
}

// detachExcept moves into discard every entry of dir that is not kept, and
// reports whether every kept name exists. An empty name exists trivially.
func (t *nodeTable) detachExcept(dir, discard string, keep ...string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	found := 0
	for _, name := range keep {
		if name == "" {
			found++
		}
	}
	for _, e := range entries {
		if slices.Contains(keep, e.Name()) {
			found++
			continue
		}
		if os.Rename(filepath.Join(dir, e.Name()), filepath.Join(discard, "store-"+rand.Text())) != nil {
			return false
		}
	}
	return found == len(keep)
}

// ownersHeader is what a manifest records: the store a clean close left, and
// what a replay of the log would derive that the index does not hold.
type ownersHeader struct {
	logSize   uint64 // of the durable identity log the index was completed for
	compacted uint64 // the log's size when it was last compacted
	next      uint64 // the counter watermark: the next counter id or key
	epoch     uint64 // of the open that closed the index
	records   uint64 // the size of the index's records file
	built     uint64 // the size of its records file when it was built

	coldBytes    uint64 // of the live owner index segments
	runtimeBytes uint64 // of the runtime slots

	instance string // the owner index directory below the store, if any
	pack     string // the pack file in instance, if any directory has owners
	nodes    string // the logged node index below the store

	// Retired cold keys the closed open had yet to discount from coldBytes.
	dead []uint64

	legacy bool       // read from a manifest of an earlier layout
	owners []keyOwner // a version 1 manifest's directories, sorted by key
}

// parseOwners decodes a manifest written by saveColdStore, or one of an
// earlier layout, of which it keeps what a rebuild needs.
func parseOwners(data []byte) (h ownersHeader, ok bool) {
	rest, ok := bytes.CutPrefix(data, ownersMagic)
	first := false
	if !ok {
		if rest, ok = bytes.CutPrefix(data, legacyOwnersMagic); !ok {
			if rest, first = bytes.CutPrefix(data, firstOwnersMagic); !first {
				return h, false
			}
		}
		h.legacy = true
	}
	next := func() (uint64, bool) {
		v, n := binary.Uvarint(rest)
		if n <= 0 {
			return 0, false
		}
		rest = rest[n:]
		return v, true
	}
	name := func() (string, bool) {
		length, ok := next()
		if !ok || length > uint64(len(rest)) {
			return "", false
		}
		s := string(rest[:length])
		rest = rest[length:]
		return s, true
	}
	if first {
		return parseFirstOwners(h, next, name)
	}
	var nodesBytes uint64
	fields := []*uint64{&h.logSize, &h.compacted, &h.next, &h.epoch, &h.records, &h.built, &h.coldBytes, &h.runtimeBytes}
	if h.legacy {
		fields = slices.Insert(fields, 7, &nodesBytes)
	}
	for _, field := range fields {
		if *field, ok = next(); !ok {
			return h, false
		}
	}
	for _, field := range []*string{&h.instance, &h.pack, &h.nodes} {
		if *field, ok = name(); !ok {
			return h, false
		}
	}
	count, ok := next()
	if !ok || count > uint64(len(rest)) {
		return h, false
	}
	h.dead = make([]uint64, count)
	for i := range h.dead {
		if h.dead[i], ok = next(); !ok {
			return h, false
		}
	}
	if h.legacy {
		if count, ok = next(); !ok || count > uint64(len(rest)) {
			return h, false
		}
		for range count {
			if _, ok = name(); !ok {
				return h, false
			}
		}
	}
	return h, len(rest) == 0 && h.nodes != "" && (h.pack == "" || h.instance != "")
}

// parseFirstOwners decodes the rest of a version 1 manifest through next
// and name: the log size, the instance and its pack, then each directory's
// key, owner index size, and offset in the pack plus one, or zero for a file
// of its own. A rebuild carries them over like the slots of a closed index.
func parseFirstOwners(h ownersHeader, next func() (uint64, bool), name func() (string, bool)) (ownersHeader, bool) {
	var ok bool
	if h.logSize, ok = next(); !ok {
		return h, false
	}
	if h.instance, ok = name(); !ok || h.instance == "" {
		return h, false
	}
	if h.pack, ok = name(); !ok {
		return h, false
	}
	count, ok := next()
	if !ok {
		return h, false
	}
	for range count {
		key, okKey := next()
		size, okSize := next()
		base, okBase := next()
		if !okKey || !okSize || !okBase || base != 0 && h.pack == "" || size == 0 || size > sizeMask || base > offsetMask {
			return h, false
		}
		o := keyOwner{key: key, owner: nodeOwner{size: int64(size), prior: true}, file: base == 0}
		if base != 0 {
			o.owner.base = int64(base - 1)
		}
		h.owners = append(h.owners, o)
	}
	slices.SortFunc(h.owners, func(a, b keyOwner) int { return cmp.Compare(a.key, b.key) })
	_, more := next()
	return h, !more
}

// growthSlack is the garbage an index may carry beyond its live size and
// still be adopted: a log this small replays in milliseconds anyway.
const growthSlack = 64 << 10

// rebuildReason returns why the store head describes should be rebuilt from a
// replay of the log rather than adopted as is, or "" to adopt it. A replay
// compacts the log and rebuilds the index and pack from the live keys alone,
// dropping what each adoption leaves behind: log records of moved and
// retired keys, records clean closes superseded and slots of retired keys,
// pack segments rewritten since. Each must stay within about twice its live
// size, which bounds the work and disk a series of adoptions defers. pack is
// the size of the pack head names.
func (h *ownersHeader) rebuildReason(pack int64) string {
	switch {
	case h.legacy:
		return "index of an earlier layout"
	case h.logSize > 2*h.compacted+growthSlack:
		return fmt.Sprintf("log grew from %d to %d bytes since its compaction", h.compacted, h.logSize)
	case h.records > 2*h.built+growthSlack:
		return fmt.Sprintf("index records grew from %d to %d bytes since the build", h.built, h.records)
	case uint64(pack) > 2*h.coldBytes+growthSlack:
		return fmt.Sprintf("owner pack holds %d bytes for %d live", pack, h.coldBytes)
	case h.epoch >= maxEpoch:
		return "index adopted too often"
	}
	return ""
}

// completeIndex appends every resident owner and retired hash the index
// lacks, at close, as spills would, in one write. The caller holds mu
// throughout: nothing changes or reads the index meanwhile, and a crash
// before the manifest discards it all anyway. It reports whether the index
// now holds every owner.
func (t *nodeTable) completeIndex() bool {
	type job struct {
		d     *node
		at    int64 // in data
		size  int64
		grown int64
	}
	if t.cacheErr != nil {
		return false
	}
	var jobs []job
	var data []byte
	for _, d := range t.byKey {
		s := d.dir
		if s == nil || s.cold && !s.coldDirty {
			continue
		}
		seg, grown, ok := t.ownerSegment(d)
		if !ok {
			return false
		}
		if seg == nil {
			s.coldDirty = false
			continue
		}
		jobs = append(jobs, job{d, int64(len(data)), int64(len(seg)), grown})
		data = append(data, seg...)
	}
	if len(jobs) == 0 {
		return true
	}
	pack := t.pack
	var err error
	if pack == nil {
		if pack, err = t.createPack(); err != nil {
			return false
		}
	}
	base, err := pack.reserve(int64(len(data)))
	if err != nil {
		return false
	}
	if _, err := pack.file.WriteAt(data, base); err != nil {
		return false
	}
	for _, j := range jobs {
		s := j.d.dir
		t.coldBytes += j.grown
		nodeOwner{size: j.size, base: base + j.at}.apply(s)
		s.coldDirty = false
	}
	return true
}

// indexWorkers bounds the index writes and flushes close runs at once.
const indexWorkers = 16

// inParallel calls f on every item with at most indexWorkers goroutines.
func inParallel[T any](items []T, f func(T)) {
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(len(items), indexWorkers) {
		wg.Go(func() {
			for i := next.Add(1) - 1; i < int64(len(items)); i = next.Add(1) - 1 {
				f(items[i])
			}
		})
	}
	wg.Wait()
}

// saveColdStore durably writes the manifest for an index completeIndex and
// saveIndex completed and an identity log of logSize bytes made durable, then
// stops all index writes: a request still running after close must not
// change what the manifest describes. Garbage still queued for reclamation
// passes to the next open in the manifest. A failure leaves the index to be
// discarded by the next open.
func (t *nodeTable) saveColdStore(logSize int64) error {
	t.coldSaved = true
	if t.index == nil {
		return nil // nothing was ever indexed
	}
	h := ownersHeader{
		logSize:      uint64(logSize),
		compacted:    uint64(t.compacted),
		next:         t.next,
		epoch:        t.index.epoch,
		records:      uint64(t.recordsEnd),
		built:        uint64(t.built),
		coldBytes:    uint64(max(t.coldBytes, 0)),
		runtimeBytes: uint64(t.runtimeBytes),
		nodes:        filepath.Base(t.index.dir),
		dead:         t.deadKeys,
	}
	if t.coldRoot != "" {
		h.instance = filepath.Base(t.coldRoot)
	}
	if t.pack != nil {
		h.pack = filepath.Base(t.pack.path)
	}
	f, err := os.CreateTemp(t.coldParent, ".owners-*")
	if err != nil {
		return err
	}
	if _, err = f.Write(h.append(bytes.Clone(ownersMagic))); err == nil {
		err = durable.Sync(f)
	}
	if err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(t.coldParent, ownersName))
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	return durable.Dir(t.coldParent)
}

// append encodes h after the magic, as parseOwners decodes it.
func (h *ownersHeader) append(b []byte) []byte {
	for _, v := range []uint64{h.logSize, h.compacted, h.next, h.epoch, h.records, h.built, h.coldBytes, h.runtimeBytes} {
		b = binary.AppendUvarint(b, v)
	}
	for _, name := range []string{h.instance, h.pack, h.nodes} {
		b = appendName(b, name)
	}
	b = binary.AppendUvarint(b, uint64(len(h.dead)))
	for _, key := range h.dead {
		b = binary.AppendUvarint(b, key)
	}
	return b
}

// reclaimCold removes detached stores in bounded batches. It leaves its root
// in place so an older worker cannot remove a newer opener's discard directory.
// A stop or crash leaves the remaining entries for the next open. Directory
// streams close before their directory is removed, including on Windows.
func reclaimCold(root string, stop <-chan struct{}, beforeRemove func()) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		f, err := os.Open(root)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for {
			names, err := f.Readdirnames(128)
			if err != nil && err != io.EOF {
				f.Close()
				return err
			}
			removed := 0
			for _, name := range names {
				if beforeRemove != nil {
					beforeRemove()
				}
				select {
				case <-stop:
					f.Close()
					return nil
				default:
				}
				path := filepath.Join(root, name)
				info, err := os.Lstat(path)
				if os.IsNotExist(err) {
					removed++
					continue
				}
				if err != nil {
					continue // retry inaccessible garbage on a later open
				}
				if info.IsDir() {
					if err := reclaimCold(path, stop, beforeRemove); err != nil {
						continue
					}
				}
				// Never follow a symlink; removal failures remain garbage.
				if err := os.Remove(path); err == nil || os.IsNotExist(err) {
					removed++
				}
			}
			if len(names) < 128 {
				f.Close()
				return nil
			}
			if removed > 0 {
				// Deletion can reshuffle directory offsets. Reopen after
				// progress so the next batch cannot skip surviving entries.
				f.Close()
				break
			}
			// A full batch of failures must advance, not reopen and spin.
		}
	}
}
