package base

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/lowprio"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/rules"
)

// A checkout of a local base never reads the folder's cache and local roots
// (build output, dependency trees; secrets, large files) as they are now: a
// cache snapshot fixes what it sees of them, so a host build writing the
// folder hands no checkout newer output and no folder event invalidates one.
// The two classes are fixed differently, by what they hold:
//
//   - Local roots are few and must stay whole (an .env the folder edits must
//     not leave a running checkout), so the snapshot copies them below the
//     base's state (cachesDir/ID/treeDir/ROOT), sharing their blocks where
//     the volume can (APFS clonefile(2), FICLONE, block cloning on ReFS) and
//     copying bytes elsewhere. They are taken in the background: checkout
//     creation never waits, the first access at a local root does.
//   - Cache roots hold hundreds of thousands of files a checkout mostly never
//     reads, so nothing of them is copied: the snapshot is a fence, the
//     instant it began, and the checkout reads the folder's own entries that
//     did not change since (see fence.go). That needs the folder on the
//     state's volume; elsewhere cache roots start empty, as on remote bases,
//     and are rebuilt in the checkout.
//
// A snapshot is complete once rootsFile is written. A daemon stopped before
// that retakes it on the next start (OpenCaches), with a new fence; nothing
// at its local roots was served yet.
const (
	cachesDir = "caches"
	treeDir   = "tree"
	rootsFile = "roots.json"
	fenceFile = "fence"
)

// Directory copies share two slots across all bases. A snapshot with no
// local directories never queues, no matter how many cache roots it fences.
var takeSlots = make(chan struct{}, 2)

// Caches is one cache snapshot of a local base: the copies of its local roots
// and the fence of its cache roots, which the checkouts the registry opens on
// it read instead of the folder as it is now (see [Base.Bottom]). The
// registry decides which checkouts share one: every checkout opened while the
// folder did not change at those roots.
type Caches struct {
	id    string
	dir   string // the snapshot's directory below the base's state
	tree  string // dir's treeDir, the root-relative image of the folder's local roots
	files *tree  // tree, resolving lookups below it
	// fence is where the view of the cache roots ends, set before the
	// snapshot is served.
	fence fence
	// listed is closed once roots is set: every local root is known, and
	// those that are no directory are already in tree.
	listed chan struct{}
	// done is closed once every local root is in tree; ready then reads true.
	done  chan struct{}
	ready atomic.Bool
	roots map[string]cacheRoot // the local roots the snapshot holds; written before listed closes
	// proofs holds directories below a local root proven real (no symlink)
	// in tree. The copies never change, so a proof never expires.
	proofsMu sync.Mutex
	proofs   map[string]bool
	locator  locator    // resolves State identities to view paths (platform)
	seen     fencedMemo // pre-fence metadata of cache-root files handed out
}

// cacheRoot is a local root the snapshot holds, as the folder had it when the
// snapshot began. A directory root presents this metadata (see [Caches.lstat]).
type cacheRoot struct {
	Mode  os.FileMode `json:"mode"`
	Mtime time.Time   `json:"mtime"`
}

// ID returns the snapshot's name below the base's state, as the registry persists it.
func (c *Caches) ID() string { return c.id }

// Wait blocks until the snapshot is complete. The registry's tests use it;
// views wait by themselves (see [bottom.Await]).
func (c *Caches) Wait() { <-c.done }

// newCaches describes snapshot id of b, not yet fenced, listed or complete.
// Its fence can show something only where the folder is on the state's volume.
func (b *Base) newCaches(id string) *Caches {
	dir := filepath.Join(b.config.StateDir, cachesDir, id)
	t := filepath.Join(dir, treeDir)
	c := &Caches{id: id, dir: dir, tree: t, files: newTree(t), locator: newLocator(b.config.StateDir), listed: make(chan struct{}), done: make(chan struct{})}
	folder, errFolder := volumeID(b.meta.Dir)
	if state, errState := volumeID(b.config.StateDir); errFolder == nil && errState == nil && folder == state {
		c.fence.volume = folder
	}
	return c
}

// shared reports whether the folder is on the state's volume, so that the
// snapshot shows the folder's cache roots.
func (c *Caches) shared() bool { return c.fence.volume != 0 }

// TakeCaches starts snapshot id of a local base: it sets the fence of the
// cache roots now, copies the local roots in the background and returns at
// once; any earlier content of id is replaced. The registry calls it when it
// opens a checkout and the newest snapshot is stale or missing.
func (b *Base) TakeCaches(id string) *Caches {
	c := b.newCaches(id)
	if c.shared() {
		at, err := c.stamp()
		if err != nil {
			b.fault(fmt.Sprintf("cache snapshot %s: %v; cache roots start empty", id, err))
		}
		c.fence.at = at
	}
	b.serve(c)
	go c.take(b)
	return c
}

// tickWait bounds how long stamp waits for the state volume's clock to move
// on: past a jiffy of a Linux kernel (10 ms at most) and the 15.6 ms of a
// Windows one, not past the second of HFS+.
const tickWait = 20 * time.Millisecond

// stamp sets the snapshot's fence and records it in fenceFile. The fence is
// read off the state volume's own clock, the change time it gives a file
// made now, since that clock stamps the folder's entries and may tick behind
// the daemon's. It is the first tick after the one stamp was called in: a
// view shows what changed before its fence, so everything the folder held
// when stamp was called is shown, also what was written in the same tick. On
// a clock that does not move within tickWait, what changed in the call's
// tick stays hidden.
func (c *Caches) stamp() (int64, error) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return 0, err
	}
	name := filepath.Join(c.dir, fenceFile)
	called, err := volumeNow(name)
	if err != nil {
		return 0, err
	}
	at := called
	for deadline := time.Now().Add(tickWait); ; time.Sleep(100 * time.Microsecond) {
		if at, err = volumeNow(name); err != nil {
			return 0, err
		}
		if at != called || !time.Now().Before(deadline) {
			break
		}
	}
	return at, os.WriteFile(name, strconv.AppendInt(nil, at, 10), 0o600)
}

// volumeNow returns the change time, in Unix nanoseconds, that the volume
// gives a file it makes now at name, replacing any file there.
func volumeNow(name string) (int64, error) {
	// A new file: truncating an empty one need not change it.
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fsys.ChangeTime(name, info)
}

// stamped returns the fence fenceFile records. A complete snapshot without
// one was taken by a build that cloned the cache roots into it: its fence is
// when it was complete, and those clones stay unread until the snapshot goes.
func (c *Caches) stamped() int64 {
	if data, err := os.ReadFile(filepath.Join(c.dir, fenceFile)); err == nil {
		at, _ := strconv.ParseInt(string(data), 10, 64)
		return at
	}
	if info, err := os.Stat(filepath.Join(c.dir, rootsFile)); err == nil {
		return info.ModTime().UnixNano()
	}
	return 0
}

// OpenCaches returns snapshot id: the one already served, else the one on
// disk, reopened after a restart. Complete, it serves at once; otherwise it is
// taken anew (see [Base.TakeCaches]).
func (b *Base) OpenCaches(id string) *Caches {
	b.mu.Lock()
	c := b.caches[id]
	b.mu.Unlock()
	if c != nil {
		return c
	}
	c = b.newCaches(id)
	data, err := os.ReadFile(filepath.Join(c.dir, rootsFile))
	if err == nil {
		err = json.Unmarshal(data, &c.roots)
	}
	if err == nil {
		err = c.files.open()
	}
	if err != nil {
		return b.TakeCaches(id)
	}
	if c.shared() {
		c.fence.at = c.stamped()
	}
	close(c.listed)
	c.ready.Store(true)
	close(c.done)
	b.serve(c)
	return c
}

// serve records c as served, so removing it or closing the base releases it.
func (b *Base) serve(c *Caches) {
	b.mu.Lock()
	b.caches[c.id] = c
	b.mu.Unlock()
}

// CachesIDs lists the snapshots on disk, complete or not, so the registry can
// remove those no checkout names.
func (b *Base) CachesIDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(b.config.StateDir, cachesDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	return ids, nil
}

// ReleaseCaches stops serving snapshot id, once a take in progress has ended,
// closes its open tree and returns its directory, which is the caller's from
// then on. The registry calls it for snapshots no checkout uses any more and
// moves the directory to its trash.
func (b *Base) ReleaseCaches(id string) string {
	b.mu.Lock()
	c := b.caches[id]
	delete(b.caches, id)
	b.mu.Unlock()
	if c != nil {
		c.Wait()
		c.files.close()
	}
	return filepath.Join(b.config.StateDir, cachesDir, id)
}

// served lists the snapshots served.
func (b *Base) served() []*Caches {
	b.mu.Lock()
	defer b.mu.Unlock()
	served := make([]*Caches, 0, len(b.caches))
	for _, c := range b.caches {
		served = append(served, c)
	}
	return served
}

// take runs in its own goroutine. It copies b's local roots into the snapshot:
// first the roots that are no directory, quickly, so listings can present
// every root, then the directory roots. A root is taken while every ancestor
// in the folder is a real directory, so a symlink cannot pull in outside content.
func (c *Caches) take(b *Base) {
	defer b.config.Stats.Job("snapshot.take", c.id+" of "+b.meta.Dir).End()
	roots := make(map[string]cacheRoot)
	defer func() {
		c.ready.Store(true)
		close(c.done)
	}()
	listed := sync.OnceFunc(func() {
		c.roots = roots
		close(c.listed)
	})
	defer listed()
	if err := c.prepareTree(b); err != nil {
		b.fault(fmt.Sprintf("cache snapshot %s: %v; local roots are missing", c.id, err))
		return
	}
	var dirs []string
	for _, p := range b.cacheRoots {
		if class, _ := b.Class(p); class != rules.Local {
			continue
		}
		info, ok := b.folderRoot(p)
		if !ok {
			continue
		}
		if info.IsDir() {
			roots[p] = cacheRoot{Mode: info.Mode(), Mtime: info.ModTime()}
			dirs = append(dirs, p)
		} else if c.copyRoot(b, p, info) {
			roots[p] = cacheRoot{Mode: info.Mode(), Mtime: info.ModTime()}
		}
	}
	listed()
	if len(dirs) != 0 {
		select {
		case takeSlots <- struct{}{}:
		case <-b.closing:
			return
		case <-b.config.StopTakes:
			return
		}
		defer func() { <-takeSlots }()
		if b.stoppingTakes() {
			return
		}
		if runtime.GOOS == "darwin" {
			// APFS directory clones and timestamp restoration can issue a
			// burst of metadata writes. Let macOS favor foreground I/O.
			// The demoted thread exits with this goroutine, not into Go's pool.
			runtime.LockOSThread()
			if err := lowprio.Thread(); err != nil {
				runtime.UnlockOSThread()
				b.fault("lower the priority of local directory copies: " + err.Error())
			}
		}
		for _, p := range dirs {
			// Incomplete takes must not write rootsFile.
			if b.stoppingTakes() {
				return
			}
			c.copyRoot(b, p, roots[p])
		}
	}
	data, err := json.Marshal(roots)
	if err == nil {
		tmp := filepath.Join(c.dir, rootsFile+".tmp")
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, filepath.Join(c.dir, rootsFile))
		}
	}
	if err != nil {
		b.fault(fmt.Sprintf("cache snapshot %s: %v; it is taken again at the next start", c.id, err))
	}
}

// prepareTree replaces an interrupted take's tree without touching its new
// fence. The registry moves the old tree to its trash instead of making
// listings wait for a recursive deletion (possibly of a pre-fence snapshot).
func (c *Caches) prepareTree(b *Base) error {
	if err := os.Remove(filepath.Join(c.dir, rootsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(c.tree); err == nil {
		if discard := b.config.DiscardTree; discard != nil {
			err = discard(c.tree, "caches-"+c.id)
		} else {
			err = os.RemoveAll(c.tree)
		}
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(c.tree, 0o700); err != nil {
		return err
	}
	return c.files.open()
}

// stoppingTakes reports whether this base or the entire registry is closing.
func (b *Base) stoppingTakes() bool {
	select {
	case <-b.closing:
	case <-b.config.StopTakes:
	default:
		return false
	}
	return true
}

// dirKind is what copyRoot needs of a root's metadata: whether it is a directory.
type dirKind interface{ IsDir() bool }

// IsDir reports a directory root.
func (r cacheRoot) IsDir() bool { return r.Mode.IsDir() }

// copyRoot copies local root p into the snapshot, cloning where the volume
// can, and reports whether it is there. A failed directory root stays as an
// empty directory; a failed file is left out.
func (c *Caches) copyRoot(b *Base, p string, kind dirKind) bool {
	src, dst := b.lower(p), c.path(p)
	err := os.MkdirAll(filepath.Dir(dst), 0o700)
	if err == nil {
		err = clone.Tree(src, dst)
	}
	if err == nil {
		return true
	}
	os.RemoveAll(dst)
	b.fault(fmt.Sprintf("cache snapshot of %s: %v; it starts empty", p, err))
	return kind.IsDir() && os.Mkdir(dst, 0o755) == nil
}

// folderRoot returns the folder's entry at root p while every ancestor of p
// in the folder is a real directory.
func (b *Base) folderRoot(p string) (os.FileInfo, bool) {
	for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
		if info, err := os.Lstat(b.lower(dir)); err != nil || !info.IsDir() {
			return nil, false
		}
	}
	info, err := hostfile.Lstat(b.lower(p))
	return info, err == nil
}

// path names p's entry in the snapshot's tree.
func (c *Caches) path(p string) string { return filepath.Join(c.tree, filepath.FromSlash(p)) }

// lstat serves p, at or below local root root (index node n), as the snapshot
// holds it. A directory root presents the folder's metadata of when the
// snapshot began and its Capture identity, so listing its parent never waits
// for the copy. A parent listing can also describe an already copied file
// root; accessing its content still waits for the snapshot to finish.
func (c *Caches) lstat(p, root string, n uint32, listing bool) (os.FileInfo, error) {
	<-c.listed
	r, ok := c.roots[root]
	if !ok {
		return nil, absent(p)
	}
	if p == root && r.Mode.IsDir() {
		return capturedInfo{name: path.Base(p), mode: r.Mode, mtime: r.Mtime, id: captureID(n)}, nil
	}
	if !listing {
		<-c.done
	}
	if p != root && (!r.Mode.IsDir() || !c.dirsBelow(root, p)) {
		return nil, absent(p)
	}
	return c.files.lstat(p, &c.locator)
}

// list returns the entries of snapshot directory p, once complete.
func (c *Caches) list(p string) ([]os.FileInfo, error) {
	<-c.done
	dir := c.path(p)
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		host := filepath.Join(dir, e.Name())
		out = append(out, c.locator.listedInfo(hostfile.Linked(info, host), host, p))
	}
	return out, nil
}

// dirsBelow reports whether every directory from below root down to p's
// parent is a real directory in the snapshot.
func (c *Caches) dirsBelow(root, p string) bool {
	var unproven []string
	c.proofsMu.Lock()
	for dir := path.Dir(p); dir != root; dir = path.Dir(dir) {
		if c.proofs[dir] {
			break
		}
		unproven = append(unproven, dir)
	}
	c.proofsMu.Unlock()
	for _, dir := range unproven {
		if info, err := c.files.lstat(dir, nil); err != nil || !info.IsDir() {
			return false
		}
	}
	if len(unproven) == 0 {
		return true
	}
	c.proofsMu.Lock()
	defer c.proofsMu.Unlock()
	if c.proofs == nil || len(c.proofs) >= 1<<16 {
		c.proofs = make(map[string]bool)
	}
	for _, dir := range unproven {
		c.proofs[dir] = true
	}
	return true
}

// await blocks until p can be served without waiting: the local roots are
// known and, at or below one, copied. Cache roots are never waited for.
func (c *Caches) await(b *Base, p string) {
	<-c.listed
	if _, root, class := b.find(p); root != "" && class == rules.Local {
		<-c.done
	}
}

// snapshotted reports a class of roots a cache snapshot fixes.
func snapshotted(class rules.Class) bool { return class == rules.Cache || class == rules.Local }

// cacheRootsOf lists, sorted, the cache and local roots of capture index x.
func cacheRootsOf(x *captureIndex) []string {
	var roots []string
	x.eachRoot(func(p string, class rules.Class) {
		if snapshotted(class) {
			roots = append(roots, p)
		}
	})
	slices.Sort(roots)
	return roots
}

// CachesAffectedBy reports whether a folder change at p can make a cache
// snapshot taken before it differ from one taken after: p is at or below a
// cache or local root, or above one, or "" (the whole folder, when events
// were lost). The registry's watcher marks the newest snapshot stale with it,
// so the next checkout gets a newer fence and fresh copies.
func (b *Base) CachesAffectedBy(p string) bool {
	if p == "" {
		return len(b.cacheRoots) != 0
	}
	if _, root, class := b.find(p); root != "" && snapshotted(class) {
		return true
	}
	prefix := p + "/"
	i, _ := slices.BinarySearch(b.cacheRoots, prefix)
	return i < len(b.cacheRoots) && len(b.cacheRoots[i]) > len(prefix) && b.cacheRoots[i][:len(prefix)] == prefix
}

// fault reports a problem through the configured sink.
func (b *Base) fault(msg string) {
	if b.config.OnFault != nil {
		b.config.OnFault(fmt.Sprintf("base %s: %s", b.meta.ID, msg))
	}
}
