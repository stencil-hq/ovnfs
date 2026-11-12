package checkout

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// hashJob describes an immutable snapshot and the ancestor tree it extends.
type hashJob struct {
	Lineage  string
	Snapshot string
	Base     *baseRecord
	Parent   store.OID
	Layers   []*overlay.Layer // oldest first
	View     *overlay.FS      // the snapshot's frozen view, read for rule facts
	Done     func(tree store.OID, err error)
}

// layerBlobs maps each raw layer a snapshot hash read to the blobs it stored
// for that layer's files and symlinks, by path.
type layerBlobs map[*overlay.Layer]map[string]string

// keepQuiet is how long snapshot hashing must have paused before layers are
// stored: storing syncs each layer's manifest, which would otherwise delay the
// syncs of snapshots taken meanwhile.
const keepQuiet = 200 * time.Millisecond

// hasher serializes each lineage without holding up independent lineages.
type hasher struct {
	mu      sync.Mutex
	queues  map[string][]hashJob
	closed  bool
	stop    chan struct{} // closed with closed: pending layers are stored at once
	workers sync.WaitGroup
	keep    func(layerBlobs) // moves stored content out of the layers a job read
	pending []layerBlobs     // successful jobs' stored content awaiting keep
	active  time.Time        // when a job last started or finished
	keeping bool             // the keeper goroutine runs
}

// newHasher creates an idle queue; workers exist only while jobs are pending.
// keep receives each successful job's stored layer content after its
// callback, one call at a time, once hashing has paused for keepQuiet.
func newHasher(keep func(layerBlobs)) *hasher {
	return &hasher{queues: make(map[string][]hashJob), stop: make(chan struct{}), keep: keep}
}

// queue appends a snapshot in FIFO order, starting its lineage when idle.
func (h *hasher) queue(j hashJob) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		if j.Done != nil {
			j.Done("", errors.New("snapshot hasher is closed"))
		}
		return
	}
	_, running := h.queues[j.Lineage]
	h.queues[j.Lineage] = append(h.queues[j.Lineage], j)
	if !running {
		h.workers.Add(1)
		go h.run(j.Lineage)
	}
	h.mu.Unlock()
}

// run owns one lineage until its queue is empty, including completion callbacks.
func (h *hasher) run(lineage string) {
	defer h.workers.Done()
	for {
		h.mu.Lock()
		jobs := h.queues[lineage]
		if len(jobs) == 0 {
			delete(h.queues, lineage)
			h.mu.Unlock()
			return
		}
		j := jobs[0]
		jobs[0] = hashJob{}
		h.queues[lineage] = jobs[1:]
		h.active = time.Now()
		h.mu.Unlock()

		id, kept, err := hashSnapshot(j)
		if j.Done != nil {
			j.Done(id, err)
		}
		h.mu.Lock()
		h.active = time.Now()
		if err == nil && len(kept) != 0 && h.keep != nil {
			h.pending = append(h.pending, kept)
			if !h.keeping {
				h.keeping = true
				h.workers.Add(1)
				go h.keeper()
			}
		}
		h.mu.Unlock()
	}
}

// keeper hands pending stored content to keep, one job's at a time, whenever
// no lineage is hashing and none has for keepQuiet, or at once after close.
// It exits when nothing is pending.
func (h *hasher) keeper() {
	defer h.workers.Done()
	for {
		h.mu.Lock()
		if len(h.pending) == 0 {
			h.keeping = false
			h.mu.Unlock()
			return
		}
		wait := keepQuiet - time.Since(h.active)
		if h.closed || (len(h.queues) == 0 && wait <= 0) {
			kept := h.pending[0]
			h.pending[0] = nil
			h.pending = h.pending[1:]
			h.mu.Unlock()
			h.keep(kept)
			continue
		}
		h.mu.Unlock()
		if wait <= 0 {
			wait = keepQuiet // a lineage is hashing: look again later
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-h.stop:
			t.Stop()
		}
	}
}

// close prevents new jobs and waits for all accepted jobs and callbacks,
// storing pending layers without waiting for a pause.
func (h *hasher) close() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.stop)
	}
	h.mu.Unlock()
	h.workers.Wait()
}

// hashSnapshot stores the snapshot's source-class changes and writes its tree,
// reading and writing only the directories along the paths its layers change.
// It also returns the blobs it stored for each raw layer.
func hashSnapshot(j hashJob) (store.OID, layerBlobs, error) {
	st := j.Base.view.Store()
	for {
		w := st.NewTreeWriter()
		t := newSnapTree(j.Base, st, j.Parent, overlayView{j.View})
		for _, l := range j.Layers {
			if err := t.apply(l); err != nil {
				return "", nil, err
			}
		}
		root, err := t.write(w)
		if err != nil {
			return "", nil, err
		}
		// Objects were removed meanwhile, perhaps trees this derivation reused: derive again.
		if w.Commit(root) {
			return root, t.kept, nil
		}
	}
}

// overlayView reads rule facts through an overlay.
type overlayView struct{ fs *overlay.FS }

// Lstat returns merged metadata without following a final symlink.
func (v overlayView) Lstat(p string) (os.FileInfo, error) { return v.fs.Lstat(p) }

// ReadFile reads a merged regular file.
func (v overlayView) ReadFile(p string) ([]byte, error) {
	f, err := v.fs.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// leafSet is the set of leaves (files, symlinks, empty directories) layers are
// folded into, keyed by slash path; directories with leaves are implied.
type leafSet interface {
	// leaf returns the leaf at p.
	leaf(p string) (store.File, bool, error)
	// put sets the leaf at p, whose content layer l supplied.
	put(p string, f store.File, l *overlay.Layer) error
	// drop removes the leaf at p, keeping anything below it.
	drop(p string) error
	// dropUnder removes every leaf strictly below p.
	dropUnder(p string) error
}

// fold applies layers to a parent tree's leaves: source-class files are hashed
// (and stored unless store is nil), every other class is left out, and paths
// the leaves already held stay source whatever the rules now say.
type fold struct {
	base  *baseRecord
	store *store.Store
	view  source.View
	set   leafSet
	dirs  pathSet // source directories layers created, kept when empty
	class map[string]rules.Class
	kept  layerBlobs // with a store: blobs stored per raw layer
}

// newFold starts folding into set.
func newFold(b *baseRecord, st *store.Store, view source.View, set leafSet) fold {
	return fold{base: b, store: st, view: view, set: set, class: map[string]rules.Class{"": rules.Source}, kept: layerBlobs{}}
}

// apply folds one layer's entries, visited parents first, into the leaves.
func (t *fold) apply(l *overlay.Layer) error {
	if t.store != nil && !l.Stored() && t.kept[l] == nil {
		t.kept[l] = make(map[string]string)
	}
	// Resolve rename bases before whiteouts or opaque directories remove them. An
	// origin the parent lacks (a temporary file renamed over its target) or that
	// is not a regular file keeps the same-path base.
	var renamed map[string]store.File
	if t.store != nil {
		if err := l.Walk(func(p string, e overlay.Entry) error {
			if e.Origin == "" {
				return nil
			}
			prev, ok, err := t.set.leaf(e.Origin)
			if err != nil || !ok || (prev.Mode != store.ModeFile && prev.Mode != store.ModeExec) {
				return err
			}
			if renamed == nil {
				renamed = make(map[string]store.File)
			}
			renamed[p] = prev
			return nil
		}); err != nil {
			return err
		}
	}
	return l.Walk(func(p string, e overlay.Entry) error {
		if p == "" || hashExcluded(p) {
			return nil
		}
		// A non-source directory is one unit: nothing below it is classified or hashed.
		if pc := parentClass(t.class, p); pc != rules.Source {
			if e.Kind == overlay.KindDir {
				t.class[p] = pc
			}
			return nil
		}
		switch e.Kind {
		case overlay.KindWhiteout:
			return t.remove(p)
		case overlay.KindDir:
			if e.Opaque {
				if err := t.removeUnder(p); err != nil {
					return err
				}
			}
			class, err := t.classify(p, e.Info)
			if err != nil {
				return err
			}
			t.class[p] = class
			f, ok, err := t.set.leaf(p)
			if err != nil {
				return err
			}
			if ok && f.Mode != store.ModeDir {
				if err := t.set.drop(p); err != nil {
					return err
				}
			}
			if class != rules.Source {
				return t.remove(p)
			}
			t.dirs.add(p)
		case overlay.KindFile, overlay.KindSymlink:
			// A socket or FIFO has no content: like a source folder's, it
			// stays out of the tree, hiding whatever p held before.
			if _, ok := store.ModeOf(e.Info.Mode()); !ok {
				return t.remove(p)
			}
			class, err := t.classify(p, e.Info)
			if err != nil || class != rules.Source {
				return err
			}
			prev, _, err := t.set.leaf(p)
			if err != nil {
				return err
			}
			if source, ok := renamed[p]; ok {
				prev = source
			}
			if err := t.removeUnder(p); err != nil {
				return err
			}
			t.dirs.remove(p)
			f, err := t.hash(l, p, e, prev)
			if err != nil {
				return err
			}
			return t.set.put(p, f, l)
		}
		return nil
	})
}

// parentClass returns the class decided for p's parent directory (the root is source).
func parentClass(classes map[string]rules.Class, p string) rules.Class {
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	if c, ok := classes[parent]; ok {
		return c
	}
	return rules.Source
}

// classify decides a layer path whose parents are source: a path the leaves
// already hold stays source, a base root keeps the class the capture gave it,
// and anything new is decided by the base's rules.
func (t *fold) classify(p string, info os.FileInfo) (rules.Class, error) {
	if _, ok, err := t.set.leaf(p); ok || err != nil {
		return rules.Source, err
	}
	if class, ok := t.base.view.Capture().Roots[p]; ok {
		return class, nil
	}
	if rule, ok := t.base.rules.Match(p, t.base.view.Domains().Facts(t.view, p, info)); ok {
		return rule.Class, nil
	}
	return rules.Source, nil
}

// hash computes (and, with a store, keeps) the blob of layer l's file or
// symlink p; a stored layer entry already names its blob. prev is the leaf p
// held before (zero if none): a regular file is stored as a delta against it.
func (t *fold) hash(l *overlay.Layer, p string, e overlay.Entry, prev store.File) (store.File, error) {
	mode, ok := store.ModeOf(e.Info.Mode())
	if !ok {
		return store.File{}, &os.PathError{Op: "hash", Path: p, Err: os.ErrInvalid}
	}
	var id store.OID
	err := l.Content(p, func(host, blob string) error {
		if blob != "" {
			id = store.OID(blob)
			return nil
		}
		var err error
		var like store.OID
		if prev.Mode == store.ModeFile || prev.Mode == store.ModeExec {
			like = prev.ID
		}
		if id, err = t.hashHost(host, mode, like); err != nil || t.store == nil {
			return err
		}
		ids := t.kept[l]
		if ids == nil {
			ids = make(map[string]string)
			t.kept[l] = ids
		}
		ids[p] = string(id)
		return nil
	})
	return store.File{Mode: mode, ID: id}, err
}

// hashHost computes (and, with a store, keeps) the blob of a raw layer file or
// symlink; a file is kept as a delta against like when that is smaller.
func (t *fold) hashHost(host string, mode uint32, like store.OID) (store.OID, error) {
	if mode == store.ModeSymlink {
		target, err := os.Readlink(host)
		if err != nil {
			return "", err
		}
		data := []byte(filepath.ToSlash(target)) // Windows returns backslashes; snapshots are portable
		if t.store == nil {
			return store.HashBytes("blob", data), nil
		}
		return t.store.PutBytes(data)
	}
	if t.store == nil {
		id, _, err := store.HashFile(host)
		return id, err
	}
	if like != "" {
		id, _, err := t.store.PutFileLike(host, like)
		return id, err
	}
	id, _, err := t.store.PutFile(host)
	return id, err
}

// remove drops p and everything below it.
func (t *fold) remove(p string) error {
	if err := t.set.drop(p); err != nil {
		return err
	}
	t.dirs.remove(p)
	return t.removeUnder(p)
}

// removeUnder drops everything strictly below p.
func (t *fold) removeUnder(p string) error {
	t.dirs.removeUnder(p, nil)
	return t.set.dropUnder(p)
}

// tree is a flattened fold for read-only passes (diff, plan, attach): every
// leaf of the parent tree is listed, nothing is written.
type tree struct {
	fold
	files  map[string]store.File
	sorted []string                  // parent paths, for prefix removal without full scans
	added  pathSet                   // paths set by layers
	layers map[string]*overlay.Layer // path → layer whose file supplied its content
}

// newTree starts from a flattened parent tree.
func newTree(b *baseRecord, st *store.Store, parent store.OID, view source.View) (*tree, error) {
	files, err := b.view.Store().Flatten(parent)
	if err != nil {
		return nil, err
	}
	t := &tree{files: files, sorted: source.SortedPaths(files), layers: map[string]*overlay.Layer{}}
	t.fold = newFold(b, st, view, t)
	return t, nil
}

// leaf returns the listed leaf at p.
func (t *tree) leaf(p string) (store.File, bool, error) {
	f, ok := t.files[p]
	return f, ok, nil
}

// put lists the leaf at p.
func (t *tree) put(p string, f store.File, l *overlay.Layer) error {
	t.files[p], t.layers[p] = f, l
	t.added.add(p)
	return nil
}

// drop unlists the leaf at p.
func (t *tree) drop(p string) error {
	delete(t.files, p)
	t.added.remove(p)
	delete(t.layers, p)
	return nil
}

// dropUnder unlists every leaf strictly below p.
func (t *tree) dropUnder(p string) error {
	prefix := p + "/"
	for i := sort.SearchStrings(t.sorted, prefix); i < len(t.sorted) && strings.HasPrefix(t.sorted[i], prefix); i++ {
		delete(t.files, t.sorted[i])
	}
	t.added.removeUnder(p, func(q string) {
		delete(t.files, q)
		delete(t.layers, q)
	})
	return nil
}

// finish turns created directories left without leaves into empty-directory
// leaves and drops empty-directory leaves that hold other leaves.
func (t *tree) finish() map[string]store.File {
	parents := make(map[string]bool)
	for p := range t.files {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if parents[d] {
				break
			}
			parents[d] = true
		}
	}
	for d := range t.dirs.all() {
		if !parents[d] {
			t.files[d] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
			for q := path.Dir(d); q != "." && !parents[q]; q = path.Dir(q) {
				parents[q] = true
			}
		}
	}
	for p, f := range t.files {
		if f.Mode == store.ModeDir && parents[p] {
			delete(t.files, p)
		}
	}
	return t.files
}

// snapDir is one directory of a snapshot tree being derived: the stored tree
// it was read from, read on first use and rewritten only when changed.
type snapDir struct {
	id        store.OID           // stored tree it started as; "" when layers created it
	kids      map[string]*snapKid // nil until read
	changed   bool                // differs from id and must be written
	emptyLeaf bool                // also an empty-directory leaf: written as one when nothing remains below
}

// snapKid is a directory entry: a leaf, or a subdirectory holding leaves.
type snapKid struct {
	leaf store.File // when dir is nil
	dir  *snapDir
}

// snapTree folds layers into a stored parent tree without flattening it: only
// directories along changed paths are read, and only changed ones written.
type snapTree struct {
	fold
	root *snapDir
}

// newSnapTree starts from the stored parent tree.
func newSnapTree(b *baseRecord, st *store.Store, parent store.OID, view source.View) *snapTree {
	t := &snapTree{root: &snapDir{id: parent}}
	t.fold = newFold(b, st, view, t)
	return t
}

// load reads d's entries once; an empty-tree entry is an empty-directory leaf.
func (t *snapTree) load(d *snapDir) error {
	if d.kids != nil {
		return nil
	}
	entries, err := t.base.view.Store().ReadTree(d.id)
	if err != nil {
		return err
	}
	d.kids = make(map[string]*snapKid, len(entries))
	for _, e := range entries {
		if e.Mode == store.ModeDir && e.ID != store.EmptyTree {
			d.kids[e.Name] = &snapKid{dir: &snapDir{id: e.ID}}
		} else {
			d.kids[e.Name] = &snapKid{leaf: store.File{Mode: e.Mode, ID: e.ID}}
		}
	}
	return nil
}

// find returns the directories from the root to p's parent, and p's entry
// (nil when absent). With create, missing directories on the way are created
// and empty-directory leaves become directories that remember being leaves;
// without, a path through a leaf or a missing directory returns nil dirs.
func (t *snapTree) find(p string, create bool) ([]*snapDir, *snapKid, error) {
	dirs := []*snapDir{t.root}
	names := strings.Split(p, "/")
	for _, name := range names[:len(names)-1] {
		d := dirs[len(dirs)-1]
		if err := t.load(d); err != nil {
			return nil, nil, err
		}
		k := d.kids[name]
		switch {
		case k != nil && k.dir != nil:
		case !create:
			return nil, nil, nil
		case k == nil:
			k = &snapKid{dir: &snapDir{kids: map[string]*snapKid{}, changed: true}}
			d.kids[name] = k
		case k.leaf.Mode == store.ModeDir:
			k.dir = &snapDir{kids: map[string]*snapKid{}, changed: true, emptyLeaf: true}
		default:
			return nil, nil, fmt.Errorf("build: %q is both a leaf and a directory", strings.Join(names[:len(dirs)], "/"))
		}
		dirs = append(dirs, k.dir)
	}
	d := dirs[len(dirs)-1]
	if err := t.load(d); err != nil {
		return nil, nil, err
	}
	return dirs, d.kids[names[len(names)-1]], nil
}

// touch marks dirs changed.
func touch(dirs []*snapDir) {
	for _, d := range dirs {
		d.changed = true
	}
}

// leaf returns the leaf at p; a directory that was an empty-directory leaf still is one.
func (t *snapTree) leaf(p string) (store.File, bool, error) {
	_, k, err := t.find(p, false)
	switch {
	case err != nil || k == nil:
		return store.File{}, false, err
	case k.dir == nil:
		return k.leaf, true, nil
	case k.dir.emptyLeaf:
		return store.File{Mode: store.ModeDir, ID: store.EmptyTree}, true, nil
	}
	return store.File{}, false, nil
}

// put sets the leaf at p, replacing whatever was there.
func (t *snapTree) put(p string, f store.File, _ *overlay.Layer) error {
	dirs, _, err := t.find(p, true)
	if err != nil {
		return err
	}
	dirs[len(dirs)-1].kids[path.Base(p)] = &snapKid{leaf: f}
	touch(dirs)
	return nil
}

// drop removes the leaf at p, keeping a directory's contents.
func (t *snapTree) drop(p string) error {
	dirs, k, err := t.find(p, false)
	switch {
	case err != nil || k == nil:
		return err
	case k.dir == nil:
		delete(dirs[len(dirs)-1].kids, path.Base(p))
	case k.dir.emptyLeaf:
		k.dir.emptyLeaf = false
		k.dir.changed = true
	default:
		return nil
	}
	touch(dirs)
	return nil
}

// dropUnder empties the directory at p without reading it.
func (t *snapTree) dropUnder(p string) error {
	dirs, k, err := t.find(p, false)
	if err != nil || k == nil || k.dir == nil {
		return err
	}
	k.dir.kids, k.dir.changed = map[string]*snapKid{}, true
	touch(dirs)
	return nil
}

// write ends the fold: created directories without leaves become
// empty-directory leaves, empty-directory leaves holding leaves become plain
// directories, and the changed directories are stored bottom-up. It returns the
// root, which the caller registers through w.
func (t *snapTree) write(w *store.TreeWriter) (store.OID, error) {
	for d := range t.dirs.all() {
		dirs, k, err := t.find(d, true)
		if err != nil {
			return "", err
		}
		switch {
		case k == nil:
			dirs[len(dirs)-1].kids[path.Base(d)] = &snapKid{leaf: store.File{Mode: store.ModeDir, ID: store.EmptyTree}}
			touch(dirs)
		case k.dir != nil && !k.dir.emptyLeaf && !k.dir.holds():
			k.dir.emptyLeaf, k.dir.changed = true, true
			touch(dirs)
		}
	}
	id, ok, err := t.writeDir(w, t.root)
	if err != nil || ok {
		return id, err
	}
	return w.Put(nil)
}

// holds reports whether any leaf lies below d.
func (d *snapDir) holds() bool {
	if d.kids == nil {
		return d.id != store.EmptyTree // a stored subtree other than the empty tree has leaves
	}
	for _, k := range d.kids {
		if k.dir == nil || k.dir.emptyLeaf || k.dir.holds() {
			return true
		}
	}
	return false
}

// writeDir stores d if changed and returns its id; ok is false when d holds
// no leaf, so its parent lists it only if it is itself an empty-directory leaf.
func (t *snapTree) writeDir(w *store.TreeWriter, d *snapDir) (id store.OID, ok bool, err error) {
	if !d.changed {
		return d.id, d.id != store.EmptyTree, nil
	}
	entries := make([]store.Entry, 0, len(d.kids))
	for name, k := range d.kids {
		if k.dir == nil {
			entries = append(entries, store.Entry{Name: name, Mode: k.leaf.Mode, ID: k.leaf.ID})
			continue
		}
		sub, ok, err := t.writeDir(w, k.dir)
		if err != nil {
			return "", false, err
		}
		if !ok && k.dir.emptyLeaf {
			sub, ok = store.EmptyTree, true
		}
		if ok {
			entries = append(entries, store.Entry{Name: name, Mode: store.ModeDir, ID: sub})
		}
	}
	if len(entries) == 0 {
		return "", false, nil
	}
	id, err = w.PutLike(entries, d.id)
	return id, err == nil, err
}

// hashExcluded keeps .git entries (owned by the git adapter), the root's .jj
// (the jj adapter's) and AppleDouble sidecars out of trees.
func hashExcluded(p string) bool {
	if source.IsGitPath(p) || source.IsJJPath(p) {
		return true
	}
	for component := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(component, "._") {
			return true
		}
	}
	return false
}
