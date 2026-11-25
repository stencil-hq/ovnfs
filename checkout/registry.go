package checkout

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/stats"
	"github.com/stencil-hq/ovnfs/store"
	"github.com/stencil-hq/ovnfs/watch"
)

// namePattern is the immutable exported folder-name grammar.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// objectPattern accepts the random on-disk base and layer identifiers.
var objectPattern = regexp.MustCompile(`^[bl]_[0-9a-f]{12}$`)

// stateDirs are the registry's subdirectories below State.
var stateDirs = []string{"bases", "layers", "uppers", "snapshots", "git", "sources", "jj", trashDir}

// lockFile holds a base's resolved manifest next to its capture.
const lockFile = "lock.json"

// gcGrace protects objects written by work still in flight from a concurrent store sweep.
const gcGrace = time.Hour

// openSlots bounds the persisted checkouts Open opens at once. Opening one
// mostly walks its upper on disk, so the bound does not follow GOMAXPROCS.
const openSlots = 8

// openOverlay opens a checkout's overlay; tests replace it to observe Open.
var openOverlay = overlay.New

// Open restores the registry and reconciles durable filesystem mutation intents.
func Open(c Config) (*Registry, error) {
	if !filepath.IsAbs(c.State) || !filepath.IsAbs(c.Mount) {
		return nil, failure("invalid", "state and mount must be absolute paths")
	}
	c.State, c.Mount = filepath.Clean(c.State), filepath.Clean(c.Mount)
	if err := os.MkdirAll(c.State, 0o700); err != nil {
		return nil, coded(err)
	}
	for _, dir := range stateDirs {
		if err := os.MkdirAll(filepath.Join(c.State, dir), 0o700); err != nil {
			return nil, coded(err)
		}
	}
	start := time.Now()
	var phases []string
	mark := start
	// phase records how long the part of the state named what took to load.
	phase := func(what string) {
		now := time.Now()
		phases = append(phases, what+" "+now.Sub(mark).Round(time.Millisecond).String())
		mark = now
	}
	stateLock, err := lockState(c.State)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			stateLock.Close()
		}
	}()
	d := diskState{Version: stateVersion}
	data, err := os.ReadFile(filepath.Join(c.State, "state.json"))
	if err == nil {
		if err = json.Unmarshal(data, &d); err != nil {
			return nil, coded(fmt.Errorf("read state.json: %w", err))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, coded(err)
	}
	if d.Version < oldestStateVersion || d.Version > stateVersion {
		return nil, failure("unsupported", "state in %s has version %d, this ovnfs reads versions %d to %d; move it aside or use another --state", c.State, d.Version, oldestStateVersion, stateVersion)
	}
	if err = validateDisk(d); err != nil {
		return nil, coded(err)
	}
	if len(d.Intents) != 0 {
		if err = recoverIntents(c.State, &d); err != nil {
			return nil, coded(err)
		}
	}
	if err = validateDisk(d); err != nil {
		return nil, coded(err)
	}
	if err = restoreStacks(&d); err != nil {
		return nil, coded(err)
	}
	st, err := store.Open(filepath.Join(c.State, "store"), c.BlobCache)
	if err != nil {
		return nil, coded(err)
	}
	r := &Registry{config: c, store: st, checkouts: make(map[string]*checkoutRecord), snapshots: make(map[string]*snapRecord), lineage: make(map[string]*snapRecord), bases: make(map[string]*baseRecord), layers: make(map[string]*layerRecord), intents: make(map[string]diskIntent), reserved: make(map[string]bool), watched: make(map[string]bool), folders: make(map[string]*reuseFolder), roots: make(map[string]string), nextSnapshot: d.NextSnapshot, nextSlot: d.NextSlot, nextLife: d.NextLife, bySlot: make(map[uint16]*checkoutRecord), proto: d.Proto, port: d.Port, gcWake: make(chan struct{}, 1), noticeWake: make(chan struct{}, 1), trashWake: make(chan chan struct{}, 1), stop: make(chan struct{})}
	r.rootTime.Store(time.Now().UnixNano())
	r.commits.cond.L = &r.commits.mu
	r.prepares = make(chan struct{}, prepareSlots)
	r.resolving = make(map[string]chan struct{})
	r.stateLock = stateLock
	r.node = c.Node
	r.mounts = mountPaths(c.Mount)
	r.mirror = newMirror(filepath.Join(c.State, "mirror.git"), func(message string) { r.enqueueNotice(baseNotice{fault: message}) })
	cleanup := func() {
		close(r.stop)
		for _, b := range r.bases {
			b.view.Close()
		}
		r.mirror.close()
	}
	phase("state")
	if err = r.openBases(d.Bases); err != nil {
		cleanup()
		return nil, coded(err)
	}
	for _, meta := range d.Bases {
		if meta.Remote != nil {
			r.mirror.want(meta.Remote.Origins)
		}
	}
	phase(fmt.Sprintf("%d bases", len(d.Bases)))
	for _, disk := range d.Layers {
		if r.bases[disk.Base] == nil && !disk.Portable {
			cleanup()
			return nil, failure("io", "layer %s references missing base %s", disk.ID, disk.Base)
		}
		dir := filepath.Join(c.State, "layers", disk.ID)
		var layer *overlay.Layer
		var openErr error
		if len(disk.Metadata) != 0 {
			layer, openErr = overlay.RestoreLayer(dir, disk.Metadata, blobFiles{st})
		} else {
			layer, openErr = overlay.OpenLayer(dir, blobFiles{st})
		}
		if openErr != nil {
			cleanup()
			return nil, coded(openErr)
		}
		// A small manifest file left by a crash or an older build moves into state below.
		embedded := len(disk.Metadata) != 0 || embeddable(layer)
		r.layers[disk.ID] = &layerRecord{id: disk.ID, base: disk.Base, layer: layer, portable: disk.Portable, embedded: embedded}
	}
	phase(fmt.Sprintf("%d layers", len(d.Layers)))
	if err = r.openCheckouts(d.Checkouts); err != nil {
		cleanup()
		return nil, coded(err)
	}
	if err = r.orphanCachesLocked(); err != nil {
		cleanup()
		return nil, coded(err)
	}
	phase(fmt.Sprintf("%d checkouts", len(d.Checkouts)))
	var missingLives bool
	for _, disk := range d.Snapshots {
		missingLives = missingLives || disk.Life == 0
		b := r.bases[disk.Base]
		if b == nil {
			cleanup()
			return nil, failure("io", "snapshot %s references missing base %s", disk.ID, disk.Base)
		}
		if _, stackErr := r.layerStackLocked(disk.Stack, disk.Base); stackErr != nil {
			cleanup()
			return nil, coded(stackErr)
		}
		s := &snapRecord{meta: disk.snapMeta, base: b, stack: append([]string{}, disk.Stack...), layer: disk.Layer, stackParent: disk.StackParent, git: store.OID(disk.Git), dropped: disk.Dropped, done: make(chan struct{}), announced: true}
		if disk.Dropped || disk.Tree != nil {
			s.finished = true
			close(s.done)
		} else {
			r.hashes.Add(1)
		}
		r.lineage[disk.ID] = s
		if !disk.Dropped {
			r.snapshots[disk.ID] = s
			r.retainLocked(b, s.stack)
		}
		if n, ok := snapshotNumber(disk.ID); ok && n > r.nextSnapshot {
			r.nextSnapshot = n
		}
	}
	if missingLives {
		r.restoreSnapshotLives()
	}
	phase(fmt.Sprintf("%d snapshots", len(d.Snapshots)))
	if err = r.removeOrphansLocked(); err != nil {
		cleanup()
		return nil, coded(err)
	}
	if err = r.persistLocked(); err != nil {
		cleanup()
		return nil, coded(err)
	}
	for _, rec := range r.layers {
		if rec.embedded {
			if err = rec.layer.RemoveMetadata(); err != nil {
				cleanup()
				return nil, coded(err)
			}
		}
	}
	phase("leftovers")
	r.hasher = newHasher(r.storeLayers)
	r.workers.Add(3)
	go r.collectGarbage()
	go r.processNotices()
	r.trash.Go(r.emptyTrash)
	go r.coolCheckouts()
	// Without folder events, captures are not reused and every checkout takes
	// its own cache snapshot (see useCaches).
	if r.watcher, err = watch.New(r.folderChanged); err == nil {
		r.watchWake = make(chan struct{}, 1)
		r.workers.Add(2)
		go r.watchFolders()
		go r.coolFolders()
		r.wakeWatch()
	} else if !errors.Is(err, errors.ErrUnsupported) {
		r.enqueueNotice(baseNotice{fault: "watch source folders: " + err.Error()})
	}
	r.syncAllRemotes()
	phase("git remotes")
	r.mu.Lock()
	r.scheduleHashesLocked()
	r.storeHashedLayersLocked()
	r.mu.Unlock()
	r.wakeGC()
	r.wakeNotices()
	opened = true
	if c.Logf != nil {
		c.Logf("state loaded in %v: %s", time.Since(start).Round(time.Millisecond), strings.Join(phases, ", "))
	}
	return r, nil
}

// openBases restores the persisted bases into r.bases, several at a time: a
// base from an older build migrates its capture into a capture index once,
// which dominates the first restore of a registry of many large bases. On
// failure it closes the bases it opened and returns the first error in metas'
// order.
func (r *Registry) openBases(metas []base.Meta) error {
	records := make([]*baseRecord, len(metas))
	errs := make([]error, len(metas))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(len(metas), runtime.GOMAXPROCS(0)) {
		wg.Go(func() {
			for i := next.Add(1) - 1; i < int64(len(metas)); i = next.Add(1) - 1 {
				records[i], errs[i] = r.openBase(metas[i])
			}
		})
	}
	wg.Wait()
	if err := cmp.Or(errs...); err != nil {
		for _, b := range records {
			if b != nil {
				b.view.Close()
			}
		}
		return err
	}
	for i, meta := range metas {
		r.bases[meta.ID] = records[i]
	}
	return nil
}

// openCheckouts restores the persisted checkouts into r.checkouts. Opening one
// indexes its whole upper, which dominates the restore of many checkouts, so
// up to openSlots open at once; the records then land in disks' order. It
// returns the first error in disks' order, after every open has ended.
func (r *Registry) openCheckouts(disks []diskCheckout) error {
	type pending struct {
		base   *baseRecord
		stack  []*overlay.Layer
		upper  string
		caches *base.Caches
	}
	plan := make([]pending, len(disks))
	for i, disk := range disks {
		b := r.bases[disk.Base]
		if b == nil {
			return failure("io", "checkout %s references missing base %s", disk.Name, disk.Base)
		}
		stack, err := r.layerStackLocked(disk.Stack, disk.Base)
		if err != nil {
			return err
		}
		caches, err := r.openCaches(b, disk.Caches)
		if err != nil {
			return err
		}
		plan[i] = pending{base: b, stack: stack, upper: r.upperPath(disk.dir()), caches: caches}
	}
	fss := make([]*overlay.FS, len(disks))
	errs := make([]error, len(disks))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(len(disks), openSlots) {
		wg.Go(func() {
			for i := next.Add(1) - 1; i < int64(len(disks)); i = next.Add(1) - 1 {
				disk, p := disks[i], plan[i]
				if disk.Git {
					if errs[i] = git.MarkCheckout(r.gitdirPath(disk.dir()), disk.Name, p.base.view.Meta().Dir); errs[i] != nil {
						continue
					}
				}
				fss[i], errs[i] = openOverlay(p.upper, p.stack, p.base.view.Bottom(p.caches), r.config.Memory)
			}
		})
	}
	wg.Wait()
	if err := cmp.Or(errs...); err != nil {
		return err
	}
	// Checkouts keep their persisted slots; one without (written by an older
	// build) gets a free slot once all of those are taken.
	var unslotted []*checkoutRecord
	for i, disk := range disks {
		p := plan[i]
		h := &checkoutRecord{meta: disk.checkoutMeta, base: p.base, stack: append([]string{}, disk.Stack...), upper: p.upper, fs: fss[i]}
		h.setCaches(p.caches)
		h.generation.Store(1)
		if !disk.Wrote.IsZero() {
			h.wrote.Store(disk.Wrote.UnixNano())
		}
		r.retainLocked(p.base, h.stack)
		if h.meta.Slot == 0 {
			unslotted = append(unslotted, h)
			continue
		}
		if err := r.addCheckoutLocked(disk.Name, h); err != nil {
			return err
		}
	}
	for _, h := range unslotted {
		if err := r.addCheckoutLocked(h.meta.Name, h); err != nil {
			return err
		}
	}
	return nil
}

// slotCount bounds checkoutMeta.Slot: the export packs a slot into the top 12
// of the 53 bits of a file id. Slot 0 is the export's virtual root.
const slotCount = 1 << 12

// addCheckoutLocked registers h as checkout name. A checkout without a slot,
// or whose slot another registered checkout holds, gets a free one and a new
// life; it fails only when every slot is taken.
func (r *Registry) addCheckoutLocked(name string, h *checkoutRecord) error {
	if h.meta.Slot == 0 || r.slotHeldLocked(h.meta.Slot) {
		slot, ok := r.allocSlotLocked()
		if !ok {
			return failure("unavailable", "all %d checkout slots are in use", slotCount-1)
		}
		r.nextLife++
		if r.nextLife == 0 {
			r.nextLife = 1
		}
		h.meta.Slot, h.meta.Life = slot, r.nextLife
	}
	r.checkouts[name] = h
	r.bySlot[h.meta.Slot] = h
	h.calls = r.config.Stats.Timer("calls", name)
	return nil
}

// dropCheckoutLocked unregisters h, checkout name: the reverse of addCheckoutLocked,
// but for the slot, which stays h's until another checkout takes it.
func (r *Registry) dropCheckoutLocked(name string, h *checkoutRecord) {
	delete(r.checkouts, name)
	h.calls.Retire()
}

// slotHeldLocked reports whether a registered checkout holds slot. bySlot may
// keep a record that a failed create or open unregistered; it holds nothing.
func (r *Registry) slotHeldLocked(slot uint16) bool {
	h := r.bySlot[slot]
	return h != nil && r.checkouts[h.meta.Name] == h
}

// allocSlotLocked picks the first free slot after the last one picked, so a
// freed slot is reused only after every other slot was: handles and inode
// numbers of a removed checkout stay unused for as long as possible.
func (r *Registry) allocSlotLocked() (uint16, bool) {
	for range slotCount - 1 {
		slot := r.nextSlot
		if slot == 0 || slot >= slotCount {
			slot = 1
		}
		r.nextSlot = slot + 1
		if !r.slotHeldLocked(slot) {
			return slot, true
		}
	}
	return 0, false
}

// Slot returns the export slot and life of live checkout name (see
// checkoutMeta.Slot); the export builds its file ids and handles from them.
func (r *Registry) Slot(name string) (slot uint16, life uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h := r.checkouts[name]
	if h == nil || h.retired.Load() {
		return 0, 0, false
	}
	return h.meta.Slot, h.meta.Life, true
}

// SlotOwner returns the live checkout holding slot and its life, for the
// export to resolve a file id or handle back to a checkout.
func (r *Registry) SlotOwner(slot uint16) (name string, life uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.slotHeldLocked(slot) {
		return "", 0, false
	}
	h := r.bySlot[slot]
	if h.retired.Load() {
		return "", 0, false
	}
	return h.meta.Name, h.meta.Life, true
}

// EnterSlot is [Registry.Enter] for the checkout holding export slot, which
// the export uses to resolve a file id or handle. It also returns that
// checkout's name and life, which stay current while the session is held.
func (r *Registry) EnterSlot(slot uint16, s *Session) (name string, life uint32, err error) {
	var h *checkoutRecord
	r.mu.RLock()
	if !r.closed && r.slotHeldLocked(slot) {
		h = r.bySlot[slot]
		name, life = h.meta.Name, h.meta.Life
	}
	r.mu.RUnlock()
	if h == nil {
		return "", 0, failure("not_found", "no checkout in slot %d", slot)
	}
	h.freeze.RLock()
	if h.retired.Load() {
		h.freeze.RUnlock()
		return "", 0, failure("not_found", "no checkout in slot %d", slot)
	}
	s.pin(h)
	return name, life, nil
}

// openBase opens one persisted base with its resolved lock.
func (r *Registry) openBase(meta base.Meta) (*baseRecord, error) {
	config := r.baseConfig(meta.ID)
	if meta.Remote != nil {
		config = r.remoteConfig(meta.ID, meta.Remote)
	}
	view, err := base.Open(config, meta)
	if err != nil {
		return nil, err
	}
	b, err := loadBase(view, filepath.Join(r.config.State, "bases", meta.ID, lockFile))
	if err != nil {
		view.Close()
		return nil, err
	}
	return b, nil
}

// storeHashedLayersLocked hashes again, in the background, the hashed
// snapshots whose layers are still raw (a crash or stop came between the hash
// and storing its layers, or they predate stored layers), so their files that
// hashing keeps in the store leave the layers too (see storeLayers).
func (r *Registry) storeHashedLayersLocked() {
	for id, s := range r.lineage {
		rec := r.layers[s.layer]
		if s.meta.Tree == nil || rec == nil || rec.layer.Stored() {
			continue
		}
		parent, layers, ready, err := r.ancestorTreeLocked(s.base, s.stack, s.meta.Parent)
		if err != nil || !ready {
			continue
		}
		meta := s.base.view.Meta()
		stack, err := r.layerStackLocked(s.stack, meta.ID)
		if err != nil {
			continue
		}
		view, err := overlay.New("", stack, s.base.view.Bottom(nil), nil)
		if err != nil {
			continue
		}
		r.retainLocked(s.base, s.stack)
		r.hasher.queue(hashJob{Lineage: meta.ID, Snapshot: id, Base: s.base, Parent: parent, Layers: layers, View: view, Done: func(store.OID, error) {
			r.mu.Lock()
			r.releaseLocked(s.base, s.stack)
			r.mu.Unlock()
		}})
	}
}

// loadBase pairs an opened base with the rules its checkouts were created with.
func loadBase(view *base.Base, name string) (*baseRecord, error) {
	var lock manifest.Lock
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	set, err := lock.Set()
	if err != nil {
		return nil, err
	}
	return &baseRecord{view: view, lock: lock, rules: set}, nil
}

// validateDisk prevents a damaged manifest from escaping the state directory.
func validateDisk(d diskState) error {
	seen := make(map[string]bool)
	for _, b := range d.Bases {
		if !objectPattern.MatchString(b.ID) || !strings.HasPrefix(b.ID, "b_") || seen[b.ID] {
			return fmt.Errorf("invalid or duplicate base id %q", b.ID)
		}
		seen[b.ID] = true
	}
	for _, l := range d.Layers {
		if !objectPattern.MatchString(l.ID) || !strings.HasPrefix(l.ID, "l_") || seen[l.ID] {
			return fmt.Errorf("invalid or duplicate layer id %q", l.ID)
		}
		seen[l.ID] = true
	}
	for _, h := range d.Checkouts {
		if !namePattern.MatchString(h.Name) || seen["h:"+h.Name] {
			return fmt.Errorf("invalid or duplicate checkout %q", h.Name)
		}
		seen["h:"+h.Name] = true
		if h.Caches != "" && !cachesPattern.MatchString(h.Caches) {
			return fmt.Errorf("invalid cache snapshot of checkout %s", h.Name)
		}
	}
	for _, s := range d.Snapshots {
		if !validSnapshotID(s.ID) || seen[s.ID] {
			return fmt.Errorf("invalid or duplicate snapshot %q", s.ID)
		}
		seen[s.ID] = true
		if s.Layer != "" && (!objectPattern.MatchString(s.Layer) || !strings.HasPrefix(s.Layer, "l_")) {
			return fmt.Errorf("invalid layer in snapshot %s", s.ID)
		}
		if s.StackParent != "" && !validSnapshotID(s.StackParent) {
			return fmt.Errorf("invalid stack parent in snapshot %s", s.ID)
		}
		if s.Git != "" && !store.OID(s.Git).Valid() {
			return fmt.Errorf("invalid git tree in snapshot %s", s.ID)
		}
	}
	for _, i := range d.Intents {
		if !namePattern.MatchString(i.Checkout) || (i.Layer != "" && !objectPattern.MatchString(i.Layer)) || (i.Snapshot != "" && !validSnapshotID(i.Snapshot)) {
			return fmt.Errorf("invalid %s intent", i.Op)
		}
	}
	return nil
}

// snapshotID formats the persisted, monotonically allocated snapshot identifier.
func snapshotID(n uint64) string { return "s" + strconv.FormatUint(n, 10) }

// snapshotNumber parses a snapshot identifier's counter.
func snapshotNumber(id string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "s"), 10, 64)
	return n, err == nil && strings.HasPrefix(id, "s")
}

// validSnapshotID checks the snapshot identifier grammar.
func validSnapshotID(id string) bool {
	n, ok := snapshotNumber(id)
	return ok && n != 0 && id == snapshotID(n)
}

// snapshotLess sorts counter identifiers numerically rather than lexically.
func snapshotLess(a, b string) bool {
	na, _ := snapshotNumber(a)
	nb, _ := snapshotNumber(b)
	return na < nb
}

// Close stops new mutations, drains hashing and releases persistent base resources.
// The owner must Quiesce before cutting the file transport and keep its release
// until Close returns. Close cannot recall clients after that transport is gone;
// it only drains daemon-side writes and reports their persistence errors.
func (r *Registry) Close() error {
	r.closeOnce.Do(func() {
		// First, while no registry lock is held: Close waits for an event batch
		// that may be reading the registry.
		if r.watcher != nil {
			r.watcher.Close()
		}
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.operations.Wait()
		r.hashes.Wait()
		r.hasher.close()
		close(r.stop)
		r.workers.Wait()
		r.mirror.close()
		r.mu.Lock()
		var prepared []*checkoutRecord
		for _, f := range r.folders {
			prepared = append(prepared, r.coolLocked(f)...)
		}
		r.mu.Unlock()
		r.discardPrepared(prepared)
		// Closed and drained, the registry gains and loses no checkout from here.
		r.mu.RLock()
		checkouts := slices.Collect(maps.Values(r.checkouts))
		r.mu.RUnlock()
		for _, h := range checkouts {
			h.freeze.Lock()
		}
		r.mu.Lock()
		// A planned stop keeps memory files: they reach the uppers before exit.
		for _, h := range checkouts {
			r.closeErr = errors.Join(r.closeErr, h.fs.Flush())
		}
		r.closeErr = errors.Join(r.closeErr, r.persistLocked())
		for _, b := range r.bases {
			r.closeErr = errors.Join(r.closeErr, b.view.Close())
		}
		for _, h := range checkouts {
			h.retired.Store(true)
			h.freeze.Unlock()
		}
		r.mu.Unlock()
		r.closeErr = errors.Join(r.closeErr, r.stateLock.Close())
	})
	return coded(r.closeErr)
}

// Enter pins a checkout generation under the shared freeze lock for one file call.
// The caller supplies a non-nil, zero or completed session and must call Done
// after using its FS and Gen. Enter leaves the session unchanged on failure.
func (r *Registry) Enter(name string, s *Session) error {
	if barrier := r.writeBarrier.Load(); barrier != nil {
		release, err := (*barrier)(context.Background())
		if err != nil {
			return err
		}
		if err = r.EnterWriter(name, s); err != nil {
			release()
			return err
		}
		s.resume = release
		return nil
	}
	return r.EnterWriter(name, s)
}

// EnterWriter is Enter for the frontend that owns the write barrier: it
// must be able to apply buffered bytes while a direct observer is draining.
func (r *Registry) EnterWriter(name string, s *Session) error {
	r.mu.RLock()
	h := r.checkouts[name]
	closed := r.closed
	r.mu.RUnlock()
	if h == nil || closed {
		return failure("not_found", "no checkout %s", name)
	}
	h.freeze.RLock()
	if h.retired.Load() {
		h.freeze.RUnlock()
		return failure("not_found", "no checkout %s", name)
	}
	s.pin(h)
	return nil
}

// Stats returns the recorder the registry was opened with (Config.Stats), for
// the file servers to time their calls with; nil when it records nothing.
func (r *Registry) Stats() *stats.Recorder { return r.config.Stats }

// Gen returns a checkout generation without waiting for its freeze lock.
func (r *Registry) Gen(name string) (uint64, bool) {
	r.mu.RLock()
	h := r.checkouts[name]
	r.mu.RUnlock()
	if h == nil || h.retired.Load() {
		return 0, false
	}
	return h.generation.Load(), true
}

// Names returns the sorted live export-root children.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.checkouts))
	for name, h := range r.checkouts {
		if !h.retired.Load() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// RootTime is the virtual export-root directory modification time. It takes no
// lock: the file servers ask for it on every call to the root, which must not
// queue behind a registry writer.
func (r *Registry) RootTime() time.Time { return time.Unix(0, r.rootTime.Load()).UTC() }

// moveRootLocked guarantees a strictly advancing directory modification time.
func (r *Registry) moveRootLocked() {
	r.rootTime.Store(max(time.Now().UnixNano(), r.rootTime.Load()+1))
}

// Ls lists the live checkouts args picks with their retained snapshots, or every
// retained snapshot without a selection, in name and id order.
func (r *Registry) Ls(ctx context.Context, args api.LsArgs) (api.LsReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.LsReply{}, err
	}
	defer resume()
	picked, err := r.pick(ctx, args.Select)
	if err != nil {
		return api.LsReply{}, err
	}
	all := args.Select == (api.Select{})
	listed := make(map[string]uint32, len(picked))
	reply := api.LsReply{Checkouts: make([]api.Checkout, 0, len(picked)), Snapshots: []api.Snapshot{}}
	for _, c := range picked {
		listed[c.Name] = c.life
		reply.Checkouts = append(reply.Checkouts, c.Checkout)
	}
	r.mu.RLock()
	for _, s := range r.snapshots {
		owned := s.meta.Life != 0 && listed[s.meta.Checkout] == s.meta.Life
		if all || owned {
			snap := snapshotOf(s.meta)
			snap.Orphan = !owned
			reply.Snapshots = append(reply.Snapshots, snap)
		}
	}
	r.mu.RUnlock()
	sort.Slice(reply.Snapshots, func(i, j int) bool { return snapshotLess(reply.Snapshots[i].ID, reply.Snapshots[j].ID) })
	return reply, nil
}

// Counts fills a status reply with retained object counts, including objects
// awaiting background GC, and how much the memory tiers hold.
func (r *Registry) Counts(s *api.Status) {
	if r.config.Memory != nil {
		s.Memory.Used, s.Memory.Limit = r.config.Memory.Usage()
		s.MemoryParked, s.MemoryError = r.config.Memory.Parked()
		s.QueueParked, s.QueueError = r.config.Memory.QueuesParked()
	}
	s.BlobCache.Used, s.BlobCache.Limit = r.store.CacheUsage()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.checkouts {
		if !h.retired.Load() {
			s.Checkouts++
		}
	}
	s.Snapshots, s.Bases, s.Layers = len(r.snapshots), len(r.bases), len(r.layers)
}

// Largest names the live checkout whose capture holds the most files: the one
// whose full walk, such as git status, makes the kernel cache the most vnodes.
func (r *Registry) Largest() (name string, files int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.checkouts {
		if n := h.base.view.Stats().Files; n > files && !h.retired.Load() {
			name, files = h.meta.Name, n
		}
	}
	return name, files
}

// checkoutLocked describes a checkout for replies. Its Dirty reads the upper, so
// besides r.mu the caller holds h's freeze lock exclusively or registers h in
// this critical section (see upperState.empty); other callers take
// describeLocked and then dirty once r.mu is released.
func (r *Registry) checkoutLocked(h *checkoutRecord) api.Checkout {
	c := r.describeLocked(h)
	c.Dirty = r.upperLocked(h).dirty()
	return c
}

// describeLocked describes a checkout for replies, all but Dirty (see dirty).
// The caller holds r.mu.
func (r *Registry) describeLocked(h *checkoutRecord) api.Checkout {
	return api.Checkout{Name: h.meta.Name, Path: filepath.Join(r.config.Mount, h.meta.Name), Source: h.source(), Base: h.meta.Base, Parent: deref(h.meta.Parent), Git: h.meta.Git, JJ: h.meta.JJ, Session: h.meta.Session, Created: h.meta.Created, Touched: r.touched(h)}
}

// dirty reports whether h's upper holds a change, for api.Checkout.Dirty. Like a
// file call it holds h's freeze lock shared, and the registry lock only to read
// h: a call keeping h's overlay busy holds up this caller, not every checkout
// (see checkoutRecord.freeze).
func (r *Registry) dirty(h *checkoutRecord) bool {
	h.freeze.RLock()
	defer h.freeze.RUnlock()
	if h.retired.Load() {
		return false
	}
	r.mu.RLock()
	u := r.upperLocked(h)
	r.mu.RUnlock()
	return u.dirty()
}

// source is the folder a checkout was opened from; for one attached from another
// machine, that machine's folder as its offer named it.
func (h *checkoutRecord) source() string {
	meta := h.base.view.Meta()
	if meta.Remote != nil {
		return meta.Remote.Source
	}
	return meta.Dir
}

// touched is when a checkout last changed: through its folder (see
// [Session.Touch]) or in its private git directory, which git writes outside the
// folder; its creation when neither changed since.
func (r *Registry) touched(h *checkoutRecord) time.Time {
	t := h.meta.Created
	if wrote := time.Unix(0, h.wrote.Load()); wrote.After(t) {
		t = wrote
	}
	if h.meta.Git {
		// Replacing the index (staging, committing) changes the directory; moving
		// HEAD appends to its log.
		gitdir := r.gitdirPath(h.meta.dir())
		for _, p := range [...]string{gitdir, filepath.Join(gitdir, "logs", "HEAD")} {
			if info, err := os.Lstat(p); err == nil && info.ModTime().After(t) {
				t = info.ModTime()
			}
		}
	}
	return t.UTC()
}

// snapshotOf describes a snapshot for replies.
func snapshotOf(m snapMeta) api.Snapshot {
	return api.Snapshot{ID: m.ID, Checkout: m.Checkout, Parent: deref(m.Parent), Label: m.Label, Created: m.Created, Tree: deref(m.Tree)}
}

// Listen returns the durable transport binding, or a zero port before first listen.
func (r *Registry) Listen() (string, int) { r.mu.RLock(); defer r.mu.RUnlock(); return r.proto, r.port }

// SetListen durably records the server port for mount reconnection after restart.
func (r *Registry) SetListen(proto string, port int) error {
	if proto == "" || port < 0 || port > 65535 {
		return failure("invalid", "invalid listen transport or port")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return failure("io", "registry is closed")
	}
	oldProto, oldPort := r.proto, r.port
	r.proto, r.port = proto, port
	if err := r.persistLocked(); err != nil {
		r.proto, r.port = oldProto, oldPort
		return coded(err)
	}
	return nil
}

// cloneCheckoutMeta protects registry-owned optional fields from callers.
func cloneCheckoutMeta(h checkoutMeta) checkoutMeta {
	h.Parent = copyString(h.Parent)
	h.Head = copyString(h.Head)
	h.MergeBases = maps.Clone(h.MergeBases)
	h.Copied = maps.Clone(h.Copied)
	return h
}

// cloneSnapMeta protects registry-owned optional fields from callers.
func cloneSnapMeta(s snapMeta) snapMeta {
	s.Parent = copyString(s.Parent)
	s.Tree = copyString(s.Tree)
	return s
}

// randomID allocates a cryptographically random 48-bit on-disk identifier.
func randomID(prefix string) (string, error) {
	var data [6]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

// upperPath, gitdirPath and snapshotDir name a checkout's or snapshot's private
// storage; a checkout's is found by checkoutMeta.dir.
func (r *Registry) upperPath(dir string) string {
	return filepath.Join(r.config.State, "uppers", dir)
}
func (r *Registry) gitdirPath(dir string) string { return filepath.Join(r.config.State, "git", dir) }
func (r *Registry) snapshotDir(id string) string {
	return filepath.Join(r.config.State, "snapshots", id)
}

// objectsPath is the git object directory shared by every checkout on one base,
// the root repository's for module "" and an absorbed submodule's for its
// source.Repo.Module; the base's directory mirrors the private git directory's layout.
func (r *Registry) objectsPath(baseID, module string) string {
	return filepath.Join(r.config.State, "bases", baseID, filepath.FromSlash(module), "objects")
}

// layerStackLocked resolves top-to-bottom identifiers and verifies base consistency.
func (r *Registry) layerStackLocked(ids []string, baseID string) ([]*overlay.Layer, error) {
	layers := make([]*overlay.Layer, 0, len(ids))
	for _, id := range ids {
		l := r.layers[id]
		if l == nil || (l.base != baseID && !l.portable) {
			return nil, fmt.Errorf("missing or incompatible layer %s", id)
		}
		layers = append(layers, l.layer)
	}
	return layers, nil
}

// retainLocked acquires a full-stack reference for a checkout, snapshot or hash.
func (r *Registry) retainLocked(b *baseRecord, stack []string) {
	b.refs++
	for _, id := range stack {
		r.layers[id].refs++
	}
}

// releaseLocked schedules no-longer-referenced objects for asynchronous removal.
func (r *Registry) releaseLocked(b *baseRecord, stack []string) {
	b.refs--
	for _, id := range stack {
		r.layers[id].refs--
	}
	r.wakeGC()
}

// wakeGC coalesces garbage collection requests without blocking file operations.
func (r *Registry) wakeGC() {
	select {
	case r.gcWake <- struct{}{}:
	default:
	}
}

// collectGarbage removes unreferenced layers and bases, then the store objects
// no remaining base or snapshot tree reaches.
func (r *Registry) collectGarbage() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case <-r.gcWake:
		}
		r.removeDeadCaches()
		r.mu.Lock()
		var deadLayers []*layerRecord
		deadBases := make(map[string]*baseRecord)
		for id, l := range r.layers {
			if l.refs == 0 {
				deadLayers = append(deadLayers, l)
				delete(r.layers, id)
			}
		}
		// A zero-reference base cannot be reacquired: snapshot creates already retain
		// it, a folder's warm capture is retained by the folder, and capture reuse
		// acquires a reference under this lock, so a base selected for removal
		// cannot be reacquired by a concurrent folder create.
		for id, b := range r.bases {
			if b.refs == 0 && !r.baseReservedLocked(id) {
				deadBases[id] = b
				delete(r.bases, id)
			}
		}
		if len(deadBases) != 0 {
			r.wakeWatch()
		}
		if len(deadLayers) == 0 && len(deadBases) == 0 {
			r.mu.Unlock()
			continue
		}
		if err := r.persistLocked(); err != nil {
			for _, l := range deadLayers {
				r.layers[l.id] = l
			}
			maps.Copy(r.bases, deadBases)
			r.mu.Unlock()
			r.enqueueNotice(baseNotice{fault: "garbage collection: " + err.Error()})
			continue
		}
		roots := r.treeRootsLocked()
		r.mu.Unlock()
		for _, l := range deadLayers {
			if err := r.discard(l.layer.Dir(), "layers-"+l.id); err != nil {
				r.enqueueNotice(baseNotice{fault: "remove layer " + l.id + ": " + err.Error()})
			}
		}
		for id, b := range deadBases {
			job := r.config.Stats.Job("base.remove", id)
			if err := b.view.Remove(); err != nil {
				r.enqueueNotice(baseNotice{fault: "remove base " + id + ": " + err.Error()})
			}
			job.End()
		}
		job := r.config.Stats.Job("store.sweep", "")
		if _, err := r.store.Sweep(roots, time.Now().Add(-gcGrace)); err != nil {
			r.enqueueNotice(baseNotice{fault: "sweep store: " + err.Error()})
		}
		job.End()
	}
}

// treeRootsLocked lists every tree the store must keep: each base capture, each
// hashed snapshot, each snapshot's stored private git directory and each layer's
// stored content.
func (r *Registry) treeRootsLocked() []store.OID {
	roots := make([]store.OID, 0, len(r.bases)+len(r.lineage)+len(r.layers))
	for _, b := range r.bases {
		roots = append(roots, b.view.Meta().Tree)
	}
	for _, l := range r.layers {
		if tree := l.layer.Tree(); tree != "" {
			roots = append(roots, store.OID(tree))
		}
	}
	for _, s := range r.lineage {
		if s.meta.Tree != nil {
			roots = append(roots, store.OID(*s.meta.Tree))
		}
		if s.git != "" {
			roots = append(roots, s.git)
		}
	}
	for _, h := range r.checkouts {
		for _, tree := range h.meta.MergeBases {
			roots = append(roots, store.OID(tree))
		}
	}
	return roots
}

// blobFiles serves stored layer content from the snapshot store.
type blobFiles struct{ st *store.Store }

// Open returns blob id's bytes.
func (b blobFiles) Open(id string) (overlay.Content, error) {
	c, err := b.st.Open(store.OID(id))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// SyncBlob flushes a stored file without copying it back into an upper.
func (b blobFiles) SyncBlob(id string) error { return b.st.SyncBlob(store.OID(id)) }

// CopyTo clones blob id to the new file dst.
func (b blobFiles) CopyTo(id, dst string, perm os.FileMode) error {
	return b.st.CloneBlob(store.OID(id), dst, perm)
}

// storeLayers serves the files snapshot hashing stored from the store instead
// of their raw layers (see overlay.Layer.Store), so their bytes are kept once.
// Each layer is held while it is converted; one garbage collection already
// removed is skipped. A failure leaves the layer raw and is reported.
func (r *Registry) storeLayers(kept layerBlobs) {
	for l, ids := range kept {
		r.mu.Lock()
		var rec *layerRecord
		for _, candidate := range r.layers {
			if candidate.layer == l {
				rec = candidate
				break
			}
		}
		if rec != nil {
			rec.refs++
		}
		r.mu.Unlock()
		if rec == nil {
			continue
		}
		stored := l.Stored()
		err := storeLayer(r.store, l, ids)
		if err == nil && embeddable(l) {
			// The manifest file stays the recoverable copy until state.json carries it.
			r.mu.Lock()
			rec.embedded = true
			if err = r.persistLocked(); err != nil {
				rec.embedded = false
			}
			r.mu.Unlock()
			if err == nil {
				err = l.RemoveMetadata()
			}
		}
		if err == nil && !stored {
			r.restoreStored(rec, l)
		}
		r.mu.Lock()
		rec.refs--
		if rec.refs == 0 {
			r.wakeGC()
		}
		r.mu.Unlock()
		if err != nil {
			r.enqueueNotice(baseNotice{fault: "store layer " + rec.id + ": " + err.Error()})
		}
	}
}

// restoreStored swaps a just-stored layer for one restored from its metadata,
// whose index is dropped while unused (see overlay.Layer.Cool). On failure the
// resident layer keeps serving.
func (r *Registry) restoreStored(rec *layerRecord, old *overlay.Layer) {
	fresh, err := overlay.RestoreLayer(old.Dir(), old.Metadata(), blobFiles{r.store})
	if err != nil {
		return
	}
	r.mu.Lock()
	rec.layer = fresh
	filesystems := make([]*overlay.FS, 0, len(r.checkouts))
	for _, h := range r.checkouts {
		filesystems = append(filesystems, h.fs)
	}
	r.mu.Unlock()
	for _, fs := range filesystems {
		fs.Relayer(old, fresh)
	}
}

// storeLayer writes the tree that keeps a layer's stored blobs alive, then
// converts the layer.
func storeLayer(st *store.Store, l *overlay.Layer, ids map[string]string) error {
	if l.Stored() {
		return nil
	}
	var tree store.OID
	if len(ids) != 0 {
		entries := make([]store.Entry, 0, len(ids))
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				entries = append(entries, store.Entry{Name: id, Mode: store.ModeFile, ID: store.OID(id)})
			}
		}
		var err error
		if tree, err = st.PutTree(entries); err != nil {
			return err
		}
	}
	return l.Store(ids, string(tree), blobFiles{st})
}

// baseReservedLocked protects bases belonging to an unfinished create.
func (r *Registry) baseReservedLocked(id string) bool {
	for _, intent := range r.intents {
		if intent.Layer == id {
			return true
		}
	}
	return false
}

// baseConfig supplies storage and nonblocking event callbacks to a base.
func (r *Registry) baseConfig(id string) base.Config {
	return base.Config{ID: id, StateDir: filepath.Join(r.config.State, "bases", id), Store: r.store, Stats: r.config.Stats, StopTakes: r.stop, DiscardTree: r.discard, FrozenLocal: r.config.FrozenLocal, OnFault: func(message string) { r.enqueueNotice(baseNotice{base: id, fault: message}) }}
}

// enqueueNotice avoids taking registry locks inside base exposure locks.
func (r *Registry) enqueueNotice(n baseNotice) {
	r.noticeMu.Lock()
	r.notices = append(r.notices, n)
	r.noticeMu.Unlock()
	r.wakeNotices()
}

// wakeNotices coalesces deferred base callback processing.
func (r *Registry) wakeNotices() {
	select {
	case r.noticeWake <- struct{}{}:
	default:
	}
}

// coolCheckouts drops upper and restored layer indexes unused for a minute. It
// never retires a checkout or invalidates handles, and never waits on one under r.mu.
func (r *Registry) coolCheckouts() {
	defer r.workers.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var filesystems []*overlay.FS
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
		}
		r.mu.RLock()
		for _, h := range r.checkouts {
			if !h.retired.Load() {
				filesystems = append(filesystems, h.fs)
			}
		}
		for _, rec := range r.layers {
			rec.layer.Cool(6) // six checks: a minute
		}
		r.mu.RUnlock()
		before := time.Now().Add(-time.Minute)
		for _, fs := range filesystems {
			fs.Cool(before)
		}
		clear(filesystems) // keep no retired checkout's FS alive between ticks
		filesystems = filesystems[:0]
	}
}

// processNotices persists base metadata before reporting faults.
func (r *Registry) processNotices() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case <-r.noticeWake:
		}
		r.noticeMu.Lock()
		notices := r.notices
		r.notices = nil
		r.noticeMu.Unlock()
		var deferred []baseNotice
		for _, n := range notices {
			r.mu.Lock()
			if n.base != "" && r.bases[n.base] == nil {
				if r.baseReservedLocked(n.base) {
					deferred = append(deferred, n)
				}
				r.mu.Unlock()
				continue
			}
			err := r.persistLocked()
			r.mu.Unlock()
			if err != nil {
				deferred = append(deferred, n)
				continue
			}
			r.emit(Event{Name: "fault", Payload: map[string]any{"message": n.fault}})
		}
		if len(deferred) != 0 {
			r.noticeMu.Lock()
			r.notices = append(deferred, r.notices...)
			r.noticeMu.Unlock()
		}
	}
}

// emit delivers a durable event without holding registry or freeze locks.
func (r *Registry) emit(event Event) {
	if r.config.Emit != nil {
		r.config.Emit(event)
	}
}

// removeOrphansLocked reclaims filesystem leftovers not named by the recovered
// manifest (see discard), so a leftover costs the start one rename however
// many files it holds. A leftover that cannot be removed is
// reported as a fault and kept for the next open: it is no reason to keep every
// other checkout down. As after a failed Rm, a kept upper or git directory keeps
// its name reserved, so no new checkout inherits its files.
func (r *Registry) removeOrphansLocked() error {
	dirs := make(map[string]bool, len(r.checkouts))
	for _, h := range r.checkouts {
		dirs[h.meta.dir()] = true
	}
	for _, kind := range []string{"bases", "layers", "snapshots", "uppers", "git", "jj"} {
		entries, err := os.ReadDir(filepath.Join(r.config.State, kind))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			retained := false
			switch kind {
			case "bases":
				retained = r.bases[name] != nil
			case "layers":
				retained = r.layers[name] != nil
			case "snapshots":
				retained = r.lineage[name] != nil
			case "uppers", "git":
				retained = dirs[name]
			}
			if !retained {
				leftover := filepath.Join(r.config.State, kind, name)
				if kind == "uppers" || kind == "jj" {
					r.forgetWorkspace(leftover)
				}
				if err = r.discard(leftover, kind+"-"+name); err != nil {
					if kind == "uppers" || kind == "git" {
						r.reserved[name] = true
					}
					r.enqueueNotice(baseNotice{fault: "remove leftover " + kind + "/" + name + ": " + err.Error()})
				}
			}
		}
	}
	return nil
}
