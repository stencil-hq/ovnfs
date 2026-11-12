package export

import (
	"container/list"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Node ids come in two kinds. Counter ids, below derivedBit, are allocated
// in sequence and recorded in the identity log. Derived ids are derivedBit,
// the key of the parent directory and a 22-bit hash of the name: reading a
// fresh view hands them out without writing anything. After a clean restart
// the owner index (nodes_cold.go) still names the owner of each issued or
// retired hash; after a crash such an id is resolved again by listing the
// parent directory. A derived id is only handed out while its name is the
// one entry of that hash in the directory that no other node owns, the rule
// resolution applies.
//
// Every node with children is logged, under a counter id or under its
// derived id pinned in place, and owns a key: its id for counter nodes, a
// counter value for pinned ones. Records and derived ids name directories by
// key, so a directory keeps its children's ids wherever it moves; a node
// moved away from its derived name is pinned with its id instead.
//
// An issued unlogged child is held as a leaf: its name under its hash in
// the parent's dirState, a fraction of a node's memory, since most of the
// paths a tree walk hands out are such files. A leaf owns its derived id
// exactly as a node would, and becomes a node only when an operation needs
// one: listing it, moving it, retiring it or giving it a child.
//
// Logged nodes page out to the index of the identity log (nodes_logged.go)
// under the same budget, and are paged in when an operation reaches them.
//
// Every id stays below 1<<53: JavaScript runtimes report inode numbers as
// doubles, so larger ids would lose precision and distinct files could
// compare equal. A directory whose key does not fit 30 bits gives its new
// children logged counter ids. The split favours keys: running out of them
// would log every later view in full, while a hash collision costs one
// logged id.
const (
	derivedBit = 1 << 52
	hashBits   = 22
	// maxDerivingKey bounds the keys a derived id can carry.
	maxDerivingKey = 1 << (52 - hashBits)
	// idLimit bounds every id the table issues or accepts.
	idLimit = derivedBit << 1
	// ambiguous marks a name hash with several entries in a directory.
	ambiguous = "/"
)

// nodeTable gives every path the server has handed out a stable id, used as
// both its NFS file handle and its fileid. Ids live in a tree of nodes and
// leaves mirroring the namespace, so a rename re-parents one node in
// O(depth) and every handle below it keeps working, as with an inode-based
// server. Logged ids and namespace changes of logged nodes are appended to
// the identity log.
type nodeTable struct {
	mu           sync.Mutex
	indexIO      func() // optional I/O barrier for deterministic fault/latency checks
	indexWait    func() // optional wait barrier
	cacheLimit   int64
	cacheUsed    int64
	cacheLRU     list.List
	coldInit     bool
	coldRoot     string
	coldParent   string
	coldSaved    bool   // close completed the index for the next open; nothing rewrites it
	coldPack     string // the pack holding packed directories' indexes, if any
	coldStop     chan struct{}
	reclaimIO    func() // optional reclamation barrier
	coldBytes    int64
	coldGarbage  []coldGarbage
	recoveryKey  uint64 // recent consecutive cold-handle misses
	recoveryHash uint32
	recoveryRun  uint8
	spillErr     error
	spillRetry   time.Time
	spillBackoff time.Duration
	cacheErr     error
	fault        atomic.Pointer[nodeCacheFault]
	root         *node
	byID         map[uint64]*node // resident nodes
	byKey        map[uint64]*node // resident logged nodes
	next         uint64           // next counter id or key
	moveSerial   uint64           // monotonic stamp assigned to every relocation
	index        *nodeIndex       // where logged nodes page out; nil keeps them resident
	nodesBytes   int64            // size of the logged node index
	runtimeBytes int64            // size of its slots for keys allocated since the index was built
	recordsEnd   int64            // size of the index's records file; close appends there
	built        int64            // size of the records file when the index was built
	compacted    int64            // size of the identity log when it was last compacted
	indexWrites  int              // index writes and removals running outside the mutex; close leaves no manifest meanwhile
	// Keys whose record file or owner index file of its own this open wrote,
	// each once however often it was rewritten: close flushes them before the
	// manifest makes them authoritative.
	recordFiles, ownerFiles map[uint64]struct{}
	opened                  nodeLoad // how open loaded the identities
	paging                  map[uint64]chan struct{}
	enumerations            []*enumeration
	deadKeys                []uint64 // retired cold keys whose index files remain
	deadFiles               []string // index files no live key's slot locates any longer
	list                    func(dir string) ([]os.FileInfo, error)
	pending                 []byte // records of the current operation, written by flush
	log                     *os.File
	logErr                  error
	dirty                   bool
	stop                    chan struct{}
	done                    chan struct{}
	closeOnce               sync.Once
}

// node links an identity to its current parent and basename.
type node struct {
	holds  int    // live protocol state, protected by the table mutex
	moves  uint64 // table-wide stamp of the last relocation
	id     uint64
	key    uint64 // 0 while the node is unlogged (derived, never recorded)
	name   string
	parent *node
	kids   map[string]*node
	dir    *dirState
}

// dirState is what the table knows about a directory's entries, enough to
// tell whether a child's derived id is unambiguous.
type dirState struct {
	// gen advances whenever entries may have appeared or the listing was
	// dropped; a listing read across a change is not installed.
	gen        uint64
	version    uint64         // cache/ownership changes during an index replacement
	indexing   *coldOperation // one index operation per directory
	cold       bool
	coldLoaded bool  // all disk owners are resident until the next eviction
	coldBytes  int64 // size of the directory index on disk
	coldDirty  bool  // issued names or retired hashes not yet present in the index
	prior      bool  // the index was adopted from before this open and not yet rewritten
	packed     bool  // the index is the segment at base of the adopted pack, not a file of its own
	base       int64
	reading    int
	cached     int64 // estimated bytes of listing, leaves and unlogged children
	recent     *list.Element
	// entries maps each name hash of the latest listing, plus names created
	// through the export since, to its name, or to ambiguous when several
	// entries share it. nil until listed.
	entries map[uint32]string
	// names owns the first batch of new basenames until eviction. Later
	// listings reuse canonical names; namespace additions own only new names.
	names string
	// dups lists every name of each ambiguous hash of the latest listing.
	dups map[uint32][]string
	// retired holds the hashes of retired derived children: their ids are
	// never handed out again while the directory's key lives. The index
	// carries them across clean restarts.
	retired map[uint32]struct{}
	// leaves maps the name hash of each leaf to its name, shared with the
	// listing it was derived from where possible.
	leaves map[uint32]string
	// stubs are the cold logged children of a resident logged directory.
	stubs map[string]stub
	// held maps each derived id hash of this key that a logged node holds,
	// resident or cold, wherever it lives, to that node's key.
	held       map[uint32]uint64
	stale      bool      // the logged record differs from the index
	loaded     nodeOwner // the owner index location the index holds
	recordSize int64     // size of the record file of its own, if any
	slot       int64     // offset of the key's slot in the index table, once known
	logged     int64     // estimated bytes that leave memory only with the node
}

// derivation is how ensure may identify a new child.
type derivation int

const (
	useCounter  derivation = iota // a logged counter id
	useDerived                    // the derived id
	needListing                   // unknown until the directory is listed
)

// newNodeTable creates the permanent export root identity. list reads a
// directory's entries for derived ids; nil hands out logged counter ids only.
func newNodeTable(list func(dir string) ([]os.FileInfo, error)) *nodeTable {
	root := &node{id: 1, key: 1}
	return &nodeTable{
		root:        root,
		byID:        map[uint64]*node{root.id: root},
		byKey:       map[uint64]*node{root.key: root},
		next:        root.id + 1,
		list:        list,
		cacheLimit:  DefaultNodeCache,
		recordFiles: map[uint64]struct{}{},
		ownerFiles:  map[uint64]struct{}{},
	}
}

// id returns p's id, allocating ids for p and its ancestors on first use.
func (t *nodeTable) id(p string) (id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() {
		t.trimCache()
		if t.cacheErr != nil {
			id = 0
		}
	}()
	defer t.flush()
	if t.cacheErr != nil {
		return 0
	}
	if id, ok := t.residentID(p); ok {
		return id
	}
	paged, err := t.pagePaths(p)
	defer paged.release()
	if err != nil {
		return 0
	}
	// Stop admitting new identities while a failed spill prevents reclamation.
	// Existing resident and cold owners remain usable; retry only after backoff.
	if t.spillErr != nil {
		t.trimCache()
		if t.spillErr != nil && t.cacheUsed > t.cacheLimit {
			if n := t.find(p); n != nil {
				return n.id
			}
			if id, ok := t.leafID(p); ok {
				return id
			}
			if id, ok := t.issuedID(p, false); ok {
				return id
			}
			return 0
		}
	}
	// Existing names need only their issued owner, even when the rest of the
	// directory is cold. New allocations still recover all competing owners.
	if id, ok := t.issuedID(p, true); ok {
		return id
	}
	release, err := t.recoverPaths(p)
	defer release.release()
	if err != nil {
		return 0
	}
	var listed *node
	for try := 0; ; try++ {
		id, unlisted := t.ensure(p, listed, try < 3)
		if unlisted == nil {
			return id
		}
		if ok, err := t.relist(unlisted); ok || err != nil {
			listed = unlisted
		}
	}
}

// childID resolves listed children directly under a resident directory. Cold
// recovery and an unknown listing still use id's full namespace validation.
func (t *nodeTable) childID(dir, name string) uint64 {
	t.mu.Lock()
	if t.spillErr != nil {
		t.trimCache()
	}
	if t.cacheErr != nil {
		t.mu.Unlock()
		return 0
	}
	d := t.find(dir)
	if d == nil || d.key == 0 || d.dir != nil && d.dir.cold && !d.dir.coldLoaded {
		t.mu.Unlock()
		return t.id(path.Join(dir, name))
	}
	id, unlisted := t.ensureChild(d, name, nil, true)
	t.flush()
	t.trimCache()
	if t.cacheErr != nil {
		id, unlisted = 0, nil
	}
	t.mu.Unlock()
	if unlisted != nil {
		return t.id(path.Join(dir, name))
	}
	return id
}

// lookup returns p's id without allocating: a path the server never handed
// out has no handle a client could cache.
func (t *nodeTable) lookup(p string) (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	paged, err := t.pagePaths(p)
	defer paged.release()
	if err != nil {
		return 0, false
	}
	if n := t.find(p); n != nil {
		return n.id, true
	}
	if id, ok := t.leafID(p); ok {
		return id, true
	}
	return t.issuedID(p, false)
}

// leafID returns p's id if p is a leaf.
func (t *nodeTable) leafID(p string) (uint64, bool) {
	if p == "" {
		return 0, false
	}
	dir, name := split(p)
	d := t.find(dir)
	if d == nil || !d.leaf(name) {
		return 0, false
	}
	return derivedID(d.key, nameHash(name)), true
}

// subtree lists the ids issued for p and everything below it.
func (t *nodeTable) subtree(p string) []uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	paged, err := t.pagePaths(p)
	defer paged.release()
	if err != nil {
		return nil
	}
	for {
		n := t.find(p)
		if n == nil {
			if id, ok := t.leafID(p); ok {
				return []uint64{id}
			}
			if id, ok := t.issuedID(p, false); ok {
				return []uint64{id}
			}
			return nil
		}
		type snapshot struct {
			n            *node
			s            *dirState
			gen, version uint64
			location     nodeLocation
		}
		var dirs []snapshot
		var out, cold []uint64
		var walk func(*node)
		walk = func(n *node) {
			out = append(out, n.id)
			for _, k := range n.kids {
				walk(k)
			}
			if s := n.dir; s != nil {
				s.reading++
				dirs = append(dirs, snapshot{n, s, s.gen, s.version, locate(n)})
				for h := range s.leaves {
					out = append(out, derivedID(n.key, h))
				}
				for _, st := range s.stubs {
					out, cold = append(out, st.id), append(cold, st.key)
				}
			}
		}
		walk(n)
		valid := true
		if len(cold) > 0 {
			ids, ok := t.coldIDs(cold)
			out, valid = append(out, ids...), ok
		}
		for _, old := range dirs {
			if !valid || !old.s.cold || old.s.coldLoaded {
				continue
			}
			names, _, ok := t.readOwners(old.n)
			if !ok {
				valid = false
				break
			}
			for h := range names {
				if !t.takenExceptCold(old.n, h) {
					out = append(out, derivedID(old.n.key, h))
				}
			}
		}
		for _, old := range dirs {
			old.s.reading--
			if t.byID[old.n.id] != old.n || old.s.gen != old.gen || old.s.version != old.version || !old.location.holds(old.n) {
				valid = false
			}
		}
		if t.find(p) != n {
			valid = false
		}
		if t.cacheErr != nil {
			return nil
		}
		if valid {
			return out
		}
	}
}

// path returns the current export-relative path of id, or false if the id is
// unknown (never issued, removed, or replaced by a rename). The export root
// is the empty path. A derived id issued before a restart is resolved
// through its parent directory.
func (t *nodeTable) path(id uint64) (p string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer func() {
		t.trimCache()
		if t.cacheErr != nil {
			p, ok = "", false
		}
	}()
	if t.cacheErr != nil {
		return "", false
	}
	if n := t.byID[id]; n != nil {
		if n.key == 0 {
			t.touch(n.parent)
		}
		return n.fullPath(), true
	}
	if n := t.pageID(id); n != nil {
		return n.fullPath(), true
	}
	d, name := t.resolve(id)
	if d == nil {
		return "", false
	}
	return d.childPath(name), true
}

// pathStack is the ancestor count joinPath holds without allocating.
const pathStack = 32

// fullPath returns n's export-relative path, empty for the root, while the
// mutex is held. Paths are never cached: renames change them.
func (n *node) fullPath() string { return n.joinPath("", false) }

// childPath returns the path of name under n.
func (n *node) childPath(name string) string { return n.joinPath(name, true) }

// joinPath builds the path of n, followed by tail when tailed, in one
// exactly sized allocation. The chain of ancestors is walked leaf-first into
// stack storage, which spills to the heap only beyond pathStack levels.
func (n *node) joinPath(tail string, tailed bool) string {
	var buf [pathStack]*node
	chain := buf[:0]
	size := 0
	for c := n; c.parent != nil; c = c.parent {
		chain = append(chain, c)
		size += len(c.name) + 1
	}
	if tailed {
		size += len(tail) + 1
	}
	if size == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(size - 1)
	for i := len(chain) - 1; i >= 0; i-- {
		b.WriteString(chain[i].name)
		if i > 0 || tailed {
			b.WriteByte('/')
		}
	}
	b.WriteString(tail)
	return b.String()
}

// rename moves from's subtree to to, retiring whatever ids to held.
func (t *nodeTable) rename(from, to string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	defer t.flush()
	release, err := t.recoverPaths(from, to)
	defer release.release()
	if err != nil {
		return
	}
	if from == to {
		return
	}
	src := t.node(from)
	if src == t.root {
		return
	}
	dir, name := split(to)
	d := t.dirNode(dir)
	if d == nil {
		return
	}
	if _, ok := d.stub(name); ok {
		return // keepPaths paged the destination in; nothing replaced it since
	}
	if dst := t.child(d, name); dst != src {
		t.drop(dst)
	}
	if src != nil {
		t.moved(src)
		if src.key == 0 && src.holds == 0 {
			t.charge(src.parent, -int64(160+len(src.name)))
		}
		if src.key != 0 {
			t.markStale(src.parent)
			t.addLogged(src, int64(len(name)-len(src.name)))
		}
		delete(src.parent.kids, src.name)
		src.name, src.parent = strings.Clone(name), d
		t.link(d, src)
		if src.key == 0 {
			// Its id no longer derives from its path.
			t.pin(src)
		} else {
			t.recordMove(src)
			t.markStale(src)
			t.markStale(d)
		}
	}
	t.appeared(d, name)
}

// remove retires the ids of p's subtree.
func (t *nodeTable) remove(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	defer t.flush()
	dir, name := split(p)
	paged, err := t.pagePaths(dir)
	defer paged.release()
	if err != nil {
		return
	}
	// Logged directories can retire their whole key without reading any child
	// index, or paging in a cold one. A cold leaf needs only its exact
	// transient owner, not admission.
	if n := t.find(p); n != nil {
		t.drop(n)
		return
	}
	if d := t.find(dir); d != nil && p != "" {
		if _, ok := d.stub(name); ok {
			t.dropStub(d, name)
			return
		}
	}
	if n := t.node(p); n != nil {
		t.drop(n)
		return
	}
	id, ok := t.issuedID(p, false)
	if !ok {
		return
	}
	if n := t.node(p); n != nil {
		t.drop(n)
		return
	}
	d := t.find(dir)
	if d == nil {
		return
	}
	n := &node{id: id, parent: d, name: strings.Clone(name)}
	t.link(d, n)
	t.drop(n)
}

// created notes a directory entry made through the export at p, keeping the
// parent's listing complete. A name whose derived id another node holds, or
// one retired, is logged with a counter id right away, so a restart cannot
// resolve that id to it before it is ever handed out.
func (t *nodeTable) created(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	defer t.flush()
	release, err := t.recoverPaths(p)
	defer release.release()
	if err != nil {
		return
	}
	dir, name := split(p)
	if d := t.find(dir); d != nil && d.kids[name] == nil && !d.leaf(name) {
		if _, cold := d.stub(name); !cold && t.child(d, name) == nil {
			t.appeared(d, name)
		}
	}
}

// listing identifies a directory read in progress: the node and the
// generation its listing must still have when the entries arrive.
type listing struct {
	dir      *node
	gen      uint64
	location nodeLocation
}

// nodeLocation identifies a node and its most recent ancestral relocation.
// Each move gets a table-wide increasing stamp, so moving any ancestor,
// including away and back, necessarily raises the maximum on this chain.
// Unrelated moves do not invalidate it. No ancestor slice needs to be retained.
type nodeLocation struct {
	node  *node
	moves uint64
}

// moved records a relocation while the table mutex is held.
func (t *nodeTable) moved(n *node) {
	t.moveSerial++
	n.moves = t.moveSerial
}

// locate snapshots n's location. In-flight operations retain n, preventing
// its ancestors from paging out and losing their transient relocation stamps.
func locate(n *node) nodeLocation {
	loc := nodeLocation{node: n}
	for p := n; p != nil; p = p.parent {
		loc.moves = max(loc.moves, p.moves)
	}
	return loc
}

// holds rejects relocation of n or an ancestor while the mutex is held.
func (loc *nodeLocation) holds(n *node) bool {
	return *loc == locate(n)
}

// startListing is called before a directory read whose entries observe
// will record; reading a directory never handed out records nothing.
func (t *nodeTable) startListing(dir string) listing {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	release, err := t.recoverPaths(dir)
	defer release.release()
	if err != nil {
		return listing{}
	}
	n := t.node(dir)
	if n == nil {
		return listing{}
	}
	t.touch(n)
	return listing{dir: n, gen: n.state().gen, location: locate(n)}
}

// observe installs the entries a directory read returned, letting the
// children it lists take derived ids without listing the directory again.
func (t *nodeTable) observe(l listing, infos []os.FileInfo) {
	if l.dir == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.trimCache()
	if !l.location.holds(l.dir) {
		return
	}
	t.install(l.dir, l.gen, infos)
}

// unlist drops the listings of p, and with deep of its whole subtree, after
// changes made behind the export.
func (t *nodeTable) unlist(p string, deep bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var walk func(*node)
	walk = func(n *node) {
		if s := n.dir; s != nil {
			s.gen++
			t.charge(n, -s.listBytes())
			s.entries, s.dups = nil, nil
		}
		if deep {
			for _, k := range n.kids {
				walk(k)
			}
		}
	}
	if n := t.find(p); n != nil {
		walk(n)
	}
}

// ensure returns p's id, creating missing nodes and leaves, or the directory
// that must be listed before p's derived id can be decided. A directory just
// listed, or derive false, falls back to a counter id instead.
func (t *nodeTable) ensure(p string, listed *node, derive bool) (id uint64, unlisted *node) {
	release, err := t.recoverPaths(p)
	defer release.release()
	if err != nil {
		return 0, nil
	}

	if t.cacheErr != nil {
		return 0, nil
	}
	if p == "" {
		return t.root.id, nil
	}
	dir, name := split(p)
	d := t.dirNode(dir)
	if d == nil {
		return 0, nil
	}
	return t.ensureChild(d, name, listed, derive)
}

// ensureChild is ensure's resident-parent path; the table mutex is held.
func (t *nodeTable) ensureChild(d *node, name string, listed *node, derive bool) (uint64, *node) {
	t.touch(d)
	if k := d.kids[name]; k != nil {
		return k.id, nil
	}
	if st, ok := d.stub(name); ok {
		return st.id, nil
	}
	if d.leaf(name) {
		return derivedID(d.key, nameHash(name)), nil
	}
	if t.spillErr != nil && t.cacheUsed > t.cacheLimit {
		return 0, nil
	}
	if derive {
		switch id, kept, how := t.derive(d, name); {
		case how == useDerived:
			d.state().coldDirty = true
			t.addLeaf(d, kept)
			return id, nil
		case how == needListing && d != listed:
			return 0, d
		}
	}
	if t.cacheErr != nil {
		return 0, nil
	}
	return t.alloc(d, name).id, nil
}

// derive decides whether name, new in directory d, takes its derived id,
// and returns the listing's copy of name for the leaf to keep.
func (t *nodeTable) derive(d *node, name string) (uint64, string, derivation) {
	if t.list == nil || d.key >= maxDerivingKey {
		return 0, "", useCounter
	}
	h := nameHash(name)
	if t.taken(d, h) {
		return 0, "", useCounter
	}
	id := derivedID(d.key, h)
	s := d.dir
	if s == nil || s.entries == nil {
		return id, "", needListing
	}
	switch v, ok := s.entries[h]; {
	case v == name:
		return id, v, useDerived
	case !ok || v != ambiguous:
		return id, "", needListing // the listing predates name
	}
	names := s.dups[h]
	i := slices.Index(names, name)
	if i < 0 {
		return id, "", needListing
	}
	for _, other := range names {
		if other != name && !d.named(other) {
			// Resolution could not tell the two apart.
			return 0, "", useCounter
		}
	}
	return id, names[i], useDerived
}

// taken reports whether derived id hash h of keyed directory d is held by a
// node or leaf, or retired.
func (t *nodeTable) taken(d *node, h uint32) bool {
	return t.takenExceptCold(d, h)
}

func (t *nodeTable) takenExceptCold(d *node, h uint32) bool {
	if t.byID[derivedID(d.key, h)] != nil {
		return true
	}
	s := d.dir
	if s == nil {
		return false
	}
	_, leaf := s.leaves[h]
	_, held := s.held[h]
	return leaf || held || s.isRetired(h)
}

// resolve finds the entry a derived id that no node holds names, as its
// directory and name: a leaf, or for an id issued before a restart the one
// entry of its hash in the keyed directory that no other node owns, which
// becomes a leaf.
func (t *nodeTable) resolve(id uint64) (*node, string) {
	if id&derivedBit == 0 || id >= idLimit || t.list == nil {
		return nil, ""
	}
	key, h := id>>hashBits&(maxDerivingKey-1), uint32(id&(1<<hashBits-1))
	fresh := false
	for tries := 0; tries < 4; {
		if n := t.byID[id]; n != nil {
			return n.parent, n.name
		}
		d := t.byKey[key]
		if d == nil {
			if t.pageKey(key) == nil {
				return nil, ""
			}
			continue
		}
		if s := d.dir; s != nil {
			if holder, ok := s.held[h]; ok {
				// A logged node holds the id, wherever it moved.
				if n := t.pageKey(holder); n != nil && n.id == id {
					return n.parent, n.name
				}
				return nil, ""
			}
			if s.isRetired(h) {
				return nil, ""
			}
			if s.cold && t.cacheErr != nil {
				return nil, ""
			}
			if name, ok := s.leaves[h]; ok {
				t.touch(d)
				return d, name
			}
			if s.cold && !s.coldLoaded {
				// A leaf costs 64 bytes plus its name, versus 16 plus its name on
				// disk. Only batch a handle recovery when all owners fit without
				// evicting another directory, or consecutive distinct misses indicate
				// a rewalk. Listings and mutation guards still
				// recover the complete set even at zero budget.
				if t.batchRecovery(d, h) {
					if !t.loadCold(d) && t.cacheErr != nil {
						return nil, ""
					}
					continue
				}
				name, valid := t.readOwner(d, h)
				if n := t.byID[id]; n != nil {
					return n.parent, n.name
				}
				if !valid {
					if t.cacheErr != nil {
						return nil, ""
					}
					continue
				}
				// Ownership can change while the read is unlocked. Prefer the live
				// owner, and never revive a retired hash or replace another node.
				if s.isRetired(h) {
					return nil, ""
				}
				if current, ok := s.leaves[h]; ok {
					t.touch(d)
					return d, current
				}
				if name != "" {
					if !t.takenExceptCold(d, h) {
						if t.spillErr == nil {
							t.addLeaf(d, name)
						}
						return d, name
					}
					return nil, ""
				}
				if t.spillErr != nil {
					return nil, ""
				}
				// This process may not have recovered every pre-restart handle.
				// An absent index record still needs ordinary listing resolution.

			}
			if name, ok := s.candidate(d, h); ok {
				s.coldDirty = true
				t.addLeaf(d, name)
				return d, name
			}
		}
		if fresh {
			return nil, ""
		}
		tries++
		installed, err := t.relist(d)
		if err != nil {
			return nil, ""
		}
		if n := t.byID[id]; n != nil {
			return n.parent, n.name
		}
		fresh = installed
	}
	return nil, ""
}

// candidate returns the entry of hash h in d's listing that no node owns,
// if the listing knows exactly one.
func (s *dirState) candidate(d *node, h uint32) (string, bool) {
	v, ok := s.entries[h]
	switch {
	case !ok:
		return "", false
	case v != ambiguous:
		return v, !d.named(v)
	}
	match, n := "", 0
	for _, name := range s.dups[h] {
		if !d.named(name) {
			match, n = name, n+1
		}
	}
	return match, n == 1
}

// relist reads d again with the mutex released and installs the entries
// unless d changed meanwhile.
func (t *nodeTable) relist(d *node) (bool, error) {
	s := d.state()
	gen := s.gen
	s.reading++
	dir := d.fullPath()
	location := locate(d)
	t.mu.Unlock()
	infos, err := t.list(dir)
	t.mu.Lock()
	s.reading--
	if !location.holds(d) {
		return false, nil
	}
	if d.fullPath() != dir {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return t.install(d, gen, infos), nil
}

// install replaces d's listing if d is live and unchanged since gen.
func (t *nodeTable) install(d *node, gen uint64, infos []os.FileInfo) bool {
	s := d.dir
	if t.byID[d.id] != d || s == nil || s.gen != gen {
		return false
	}
	if t.spillErr != nil {
		return false
	}
	location := locate(d)
	if !t.loadCold(d) || t.byID[d.id] != d || s.gen != gen || !location.holds(d) {
		return false
	}
	count := len(s.entries)
	for _, names := range s.dups {
		count += len(names) - 1
	}
	unchanged := s.entries != nil && count == len(infos)
	var newBytes int
	for _, info := range infos {
		name := info.Name()
		if s.listedName(name) == "" {
			unchanged = false
			if s.leaves[nameHash(name)] != name && d.kids[name] == nil {
				newBytes += len(name)
			}
		}
	}
	if unchanged {
		return true
	}
	oldEntries, oldDups := s.entries, s.dups
	var names strings.Builder
	packNames := s.names == "" && newBytes != 0
	if packNames {
		names.Grow(newBytes)
	}
	t.charge(d, -s.listBytes())
	s.entries, s.dups = make(map[uint32]string, len(infos)), nil
	for _, info := range infos {
		name := info.Name()
		h := nameHash(name)
		old := oldEntries[h]
		if old == ambiguous {
			if i := slices.Index(oldDups[h], name); i >= 0 {
				old = oldDups[h][i]
			}
		}
		switch {
		case old == name:
			name = old
		case s.leaves[h] == name:
			name = s.leaves[h]
		case d.kids[name] != nil:
			name = d.kids[name].name
		case packNames:
			at := names.Len()
			names.WriteString(name)
			name = names.String()[at:]
		default:
			name = strings.Clone(name)
		}
		prev, ok := s.entries[h]
		if !ok {
			s.entries[h] = name
			continue
		}
		if s.dups == nil {
			s.dups = make(map[uint32][]string)
		}
		if prev != ambiguous {
			s.dups[h] = []string{prev}
			s.entries[h] = ambiguous
		}
		s.dups[h] = append(s.dups[h], name)
	}
	if packNames {
		s.names = names.String()
		t.charge(d, int64(len(s.names)))
	}
	t.charge(d, s.listBytes())
	return true
}

// listedName returns a listing's owned spelling, including colliding names.
func (s *dirState) listedName(name string) string {
	h := nameHash(name)
	if s.entries[h] == name {
		return s.entries[h]
	}
	if i := slices.Index(s.dups[h], name); i >= 0 {
		return s.dups[h][i]
	}
	return ""
}

// appeared records that name became an entry of d under the mutex.
func (t *nodeTable) appeared(d *node, name string) {
	h := nameHash(name)
	s := d.dir
	if s != nil {
		s.gen++
		if s.entries != nil {
			switch v, ok := s.entries[h]; {
			case !ok:
				s.entries[h] = strings.Clone(name)
				t.charge(d, int64(64+len(name)))
			case v == name, v == ambiguous && slices.Contains(s.dups[h], name):
			default:
				s.entries[h] = ambiguous
				t.charge(d, int64(len(ambiguous)-len(v)))
				t.charge(d, -s.dupBytes(h))
				delete(s.dups, h)
			}
		}
	}
	if _, cold := d.stub(name); cold || d.kids[name] != nil || d.leaf(name) || t.list == nil || d.key == 0 || d.key >= maxDerivingKey {
		return
	}
	if t.taken(d, h) {
		t.alloc(d, name)
	}
}

// dirNode returns the logged node of directory p, allocating missing
// directories and pinning unlogged ones, which are gaining a child.
func (t *nodeTable) dirNode(p string) *node {
	n := t.root
	for rest := p; rest != ""; {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		k := t.child(n, name)
		if _, cold := n.stub(name); k == nil && (cold || t.cacheErr != nil) {
			return nil // a cold logged directory is paged in first, never replaced
		}
		switch {
		case k == nil:
			k = t.alloc(n, name)
		case k.key == 0:
			t.pin(k)
		}
		n = k
	}
	return n
}

// alloc logs a new child of the logged directory d under a counter id.
func (t *nodeTable) alloc(d *node, name string) *node {
	k := &node{id: t.next, key: t.next, name: strings.Clone(name), parent: d}
	t.next++
	t.link(d, k)
	t.byKey[k.key] = k
	t.recordNode(k)
	t.loggedNew(k)
	return k
}

// pin logs an unlogged node under its derived id and gives it a key.
func (t *nodeTable) pin(k *node) {
	if k.holds == 0 {
		t.charge(k.parent, -int64(160+len(k.name)))
	}
	k.key = t.next
	t.next++
	t.byKey[k.key] = k
	t.recordNode(k)
	t.loggedNew(k)
}

// link attaches k under d and indexes its id.
func (t *nodeTable) link(d, k *node) {
	if d.kids == nil {
		d.kids = make(map[string]*node)
	}
	d.kids[k.name] = k
	t.byID[k.id] = k
	if k.key == 0 && k.holds == 0 {
		t.charge(d, int64(160+len(k.name)))
	}
}

// named reports whether a resident or cold logged child of n owns name.
func (n *node) named(name string) bool {
	_, cold := n.stub(name)
	return cold || n.kids[name] != nil
}

// leaf reports whether name is one of n's leaves.
func (n *node) leaf(name string) bool {
	if n.dir == nil {
		return false
	}
	v, ok := n.dir.leaves[nameHash(name)]
	return ok && v == name
}

// addLeaf holds name as a leaf of the keyed directory n.
func (t *nodeTable) addLeaf(n *node, name string) {
	s := n.state()
	if s.leaves == nil {
		s.leaves = make(map[uint32]string)
	}
	if _, ok := s.leaves[nameHash(name)]; !ok {
		t.charge(n, int64(64+len(name)))
	}
	s.leaves[nameHash(name)] = name
}

// child returns d's child name, turning a leaf into a node.
func (t *nodeTable) child(d *node, name string) *node {
	if k := d.kids[name]; k != nil {
		return k
	}
	if !d.leaf(name) {
		return nil
	}

	h := nameHash(name)
	// A materialized/held child can outlive the directory's name arena.
	k := &node{id: derivedID(d.key, h), name: strings.Clone(d.dir.leaves[h]), parent: d}
	t.charge(d, -int64(64+len(k.name)))
	delete(d.dir.leaves, h)
	if len(d.dir.leaves) == 0 {
		d.dir.leaves = nil
	}
	t.link(d, k)
	return k
}

// retire reserves hash h of a removed derived child.
func (s *dirState) retire(h uint32) {
	if s.retired == nil {
		s.retired = make(map[uint32]struct{})
	}
	s.retired[h] = struct{}{}
}

// isRetired reports whether hash h of a removed derived child is reserved.
func (s *dirState) isRetired(h uint32) bool {
	_, gone := s.retired[h]
	return gone
}

// state returns n's directory state, creating it on first use.
func (n *node) state() *dirState {
	if n.dir == nil {
		n.dir = &dirState{}
	}
	return n.dir
}

// node resolves p like find, turning a leaf into a node for an operation
// that needs one.
func (t *nodeTable) node(p string) *node {
	if p == "" {
		return t.root
	}
	dir, name := split(p)
	if d := t.find(dir); d != nil {
		return t.child(d, name)
	}
	return nil
}

// find resolves a path without allocating under the mutex.
func (t *nodeTable) find(p string) *node {
	n := t.root
	for rest := p; rest != "" && n != nil; {
		var name string
		name, rest, _ = strings.Cut(rest, "/")
		n = n.kids[name]
	}
	return n
}

// drop retires an entire subtree under the mutex. Its derived ids stay
// reserved in their directories, across clean restarts through the index;
// the keys of dropped directories are never issued again, so ids derived from
// them die with them.
func (t *nodeTable) drop(n *node) {
	if n == nil || n == t.root {
		return
	}
	if n.key != 0 {
		t.recordRemove(n.key)
		t.markStale(n.parent)
	}
	if n.key == 0 && n.holds == 0 {
		t.charge(n.parent, -int64(160+len(n.name)))
	}
	delete(n.parent.kids, n.name)
	var forget, retire func(*node)
	forget = func(n *node) {
		t.uncache(n)
		if s := n.dir; s != nil {
			if (s.cold || s.indexing != nil) && t.coldRoot != "" {
				t.coldGarbage = append(t.coldGarbage, coldGarbage{path: t.coldPath(n), dir: n})
			}
			if n.key != 0 && t.index != nil {
				// Even a record file no slot locates any longer.
				t.coldGarbage = append(t.coldGarbage, coldGarbage{path: coldIndexPath(t.index.dir, n.key), dir: n, record: true})
			}
			for _, st := range s.stubs {
				t.deadKeys = append(t.deadKeys, st.key)
			}
		}
		delete(t.byID, n.id)
		if n.key != 0 {
			delete(t.byKey, n.key)
		}
		for _, k := range n.kids {
			forget(k)
		}
	}
	retire = func(n *node) {
		t.retireID(n.id)
		for _, k := range n.kids {
			retire(k)
		}
		if s := n.dir; s != nil {
			for _, st := range s.stubs {
				t.retireID(st.id)
			}
		}
	}
	forget(n)
	retire(n)
}

// derivedID is the id of the entry with name hash h in the directory keyed key.
func derivedID(key uint64, h uint32) uint64 {
	return derivedBit | key<<hashBits | uint64(h)
}

// nameHash is the 22-bit FNV-1a hash of name that derived ids carry; it must
// never change, or ids issued before an upgrade would resolve elsewhere.
func nameHash(name string) uint32 {
	h := uint64(14695981039346656037)
	for i := range len(name) {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return uint32(h^h>>32) & (1<<hashBits - 1)
}

// split separates an export-relative path at its last slash.
func split(p string) (dir, name string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}
