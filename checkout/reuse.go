package checkout

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/source"
)

// A folder create needs a capture of the folder as it is when the create arrives.
// While the folder has not changed since an earlier capture began, with the same
// rules, that capture is it: the registry keeps the latest one per folder (warm)
// and opens further checkouts on its base without walking the folder or running
// git. The folder watcher tells what changed. Every edit that can make the folder
// differ from a capture bumps the folder's generation, and a create reuses a
// capture only when the generation it arrived at is the one the capture began at.
// Concurrent creates of a folder share one capture the same way: it captures the
// folder as a base with no checkout on it, makes that the warm capture and wakes
// the waiting creates, and every one of them, its own included, opens on it.
//
// A create on the warm capture takes a checkout prepared on it ahead of time (the
// pool): upper, private git directory and overlay, below directories named by a
// random key instead of a checkout name (checkoutMeta.Dir). Naming it writes only
// the git directory's marker. The pool holds as many as the most creates of the
// folder seen running at once, at least poolMin, as far as their git directories
// fit poolBudget. It is refilled in the background and goes with the warm capture,
// or moves onto the next one while the repository's git state stays the same; that
// capture's git template is then written in the background too. Prepared
// checkouts are in no state: the next Open removes any a stop left behind.

const (
	// warmTTL is how long an unused warm capture stays retained.
	warmTTL = 15 * time.Minute
	// poolMin and poolMax bound how many checkouts a folder keeps prepared.
	poolMin, poolMax = 8, 256
	// poolBudget bounds the bytes of the git directories a folder keeps prepared,
	// each a copy of its template; a pool keeps at least one.
	poolBudget = 256 << 20
	// prepareSlots bounds the checkouts prepared at once.
	prepareSlots = 8
	// preparedPrefix starts the directory key of a prepared checkout; no checkout
	// name starts with it (namePattern).
	preparedPrefix = "_p"
)

// reuseFolder is a source folder whose creates reuse captures; the registry keeps it
// watched while it is listed in Registry.folders. Fields but gen and gitGen are
// guarded by r.mu.
type reuseFolder struct {
	root       string
	gen        atomic.Uint64     // bumped by every edit that can change a capture of the folder
	gitGen     atomic.Uint64     // bumped by those in the repository's git directory
	warm       *baseRecord       // the latest reusable capture, retained by the folder; nil for none
	warmGen    uint64            // gen when warm's capture began
	warmGitGen uint64            // gitGen then
	warmLock   string            // hash of the manifest lock warm was classified with
	warmUsed   time.Time         // when a create last reused or made warm
	template   gitTemplate       // what warm's checkouts copy; dir "" for none or while warm.template is pending
	capturing  chan struct{}     // closed when the capture in flight for this folder ends
	pool       []*checkoutRecord // prepared on warm, unnamed and unregistered, each retaining warm
	poolSize   int               // the most creates seen running at once, at least poolMin, at most poolMax (see poolTarget)
	filling    int               // fillPool goroutines running
	preparing  int               // checkouts they are preparing
	running    int               // creates running now
}

// gitTemplate is the private git directory checkouts on a capture copy (see
// Registry.writeTemplate).
type gitTemplate struct {
	dir    string // "" for none
	config string // its config, which prepared checkouts' git directories hold
	bytes  int64  // the size of its files, which each prepared checkout copies
}

// newOnFolder opens a checkout named name (or one derived from root) on a capture of
// folder root classified by m's rules: the warm capture when the folder has not
// changed since it began, else a new one, which concurrent creates share. Every
// folder change made before the create reaches foldersChangedLocked first
// (watch.Watcher.Sync); without that guarantee the folder is captured anew.
func (r *Registry) newOnFolder(ctx context.Context, root string, m manifest.Manifest, name string) (api.NewReply, error) {
	hash := m.Lock().Hash()
	synced := r.watcher.Sync() == nil
	r.mu.Lock()
	f := r.folders[root]
	if f == nil {
		f = &reuseFolder{root: root, poolSize: poolMin}
		r.folders[root] = f
	}
	// Creates made one after another need no more than poolMin however quickly
	// they follow each other; a burst started together finds as many prepared.
	f.running++
	f.poolSize = max(f.poolSize, min(f.running, poolMax))
	defer func() {
		r.mu.Lock()
		f.running--
		r.mu.Unlock()
	}()
	arrived := f.gen.Load()
	for {
		if r.closed {
			r.mu.Unlock()
			return api.NewReply{}, failure("io", "registry is closed")
		}
		if synced && f.warm != nil && f.warmGen == arrived && f.warmLock == hash && r.watcher.Covers(root) {
			return r.openWarmLocked(ctx, f, name)
		}
		ch := f.capturing
		if ch == nil {
			break
		}
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return api.NewReply{}, coded(ctx.Err())
		}
		r.mu.Lock()
	}
	ch := make(chan struct{})
	f.capturing = ch
	r.mu.Unlock()
	// Creates waiting for this capture go on as soon as it is warm (or failed);
	// r.mu is held.
	endCapture := sync.OnceFunc(func() {
		f.capturing = nil
		close(ch)
	})
	defer func() {
		r.mu.Lock()
		endCapture()
		r.mu.Unlock()
	}()
	// Changes made before the capture begins must count before it, not after.
	covered := r.watchFolder(root) && r.watcher.Sync() == nil
	began, beganGit := f.gen.Load(), f.gitGen.Load()
	b, reused, stats, warnings, err := r.captureBase(ctx, root, m)
	if err != nil {
		return api.NewReply{}, err
	}
	reusable := covered && gitdirInside(b, root)
	var template gitTemplate
	r.mu.Lock()
	if reused && reusable && f.warm == b && f.warmLock == hash {
		// The folder holds what its warm capture does after all: only the
		// generations it is known unchanged from move on.
		f.warmGen, f.warmGitGen = began, beganGit
		endCapture()
		reply, err := r.openWarmLocked(ctx, f, name)
		r.mu.Lock()
		r.releaseLocked(b, nil)
		r.mu.Unlock()
		if err != nil {
			return api.NewReply{}, err
		}
		reply.Capture, reply.Warnings = &stats, append(reply.Warnings, warnings...)
		return reply, nil
	}
	// The prepared checkouts move onto the capture when the repository's git
	// state is the one they were prepared from (see setWarmLocked); the private
	// git directory later checkouts copy is then written in the background. A
	// reused base already has one.
	keep := reusable && !reused && f.keepsPoolLocked(b, beganGit, hash)
	if !keep {
		r.mu.Unlock()
		if template, err = r.baseTemplate(b, reused); err != nil {
			r.enqueueNotice(baseNotice{base: b.view.Meta().ID, fault: "reusable git directory: " + err.Error()})
			template, reusable = gitTemplate{}, false
		}
		r.mu.Lock()
	}
	var drained []*checkoutRecord
	var reply api.NewReply
	if reusable {
		if keep {
			// The same git state: the template's config and size stay; its
			// directory follows (templateLater).
			template = gitTemplate{config: f.template.config, bytes: f.template.bytes}
			b.template = make(chan struct{})
		}
		drained = r.setWarmLocked(f, b, began, beganGit, hash, template)
		if keep {
			r.templateLater(f, b)
		}
		endCapture()
		reply, err = r.openWarmLocked(ctx, f, name)
	} else {
		reply, err = r.openOnLocked(ctx, b, template, name, filepath.Base(root), nil)
	}
	r.mu.Lock()
	r.releaseLocked(b, nil)
	r.mu.Unlock()
	r.discardLater(drained)
	if err != nil {
		return api.NewReply{}, err
	}
	reply.Capture, reply.Warnings = &stats, append(reply.Warnings, warnings...)
	return reply, nil
}

// captureBase captures folder root classified by m's rules as a base no checkout
// is on yet and returns it retained once for the caller, with how the folder was
// read. The state names it only once a checkout is on it: a restart before that
// removes it. When an existing base holds the same capture (base.Meta.ReuseKey),
// that one is returned instead, with reused set.
func (r *Registry) captureBase(ctx context.Context, root string, m manifest.Manifest) (b *baseRecord, reused bool, stats api.Capture, warnings []string, err error) {
	fail := func(err error) (*baseRecord, bool, api.Capture, []string, error) {
		return nil, false, api.Capture{}, nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fail(failure("io", "registry is closed"))
	}
	id := ""
	for id == "" || r.bases[id] != nil || exists(filepath.Join(r.config.State, "bases", id)) {
		if id, err = randomID("b_"); err != nil {
			r.mu.Unlock()
			return fail(coded(err))
		}
	}
	r.operations.Add(1)
	r.mu.Unlock()
	defer r.operations.Done()
	bc := r.baseConfig(id)
	fork := time.Now().UTC()
	captured, warnings, err := base.Capture(ctx, bc, source.Options{Root: root, Rules: m.Rules, Cache: r.statCachePath(root)})
	if err != nil {
		return fail(coded(errors.Join(err, os.RemoveAll(bc.StateDir))))
	}
	warnings = append([]string{}, warnings...)
	key := captured.ReuseKey()
	if same := r.retainReusable(root, key, m.Lock()); same != nil {
		if err := os.RemoveAll(bc.StateDir); err != nil {
			r.mu.Lock()
			r.releaseLocked(same, nil)
			r.mu.Unlock()
			return fail(coded(err))
		}
		return same, true, captured.Stats, warnings, nil
	}
	view, err := base.CreateCaptured(bc, captured, fork, key)
	if err != nil {
		return fail(coded(errors.Join(err, os.RemoveAll(bc.StateDir))))
	}
	b = &baseRecord{view: view, lock: m.Lock(), rules: m.Rules}
	gs, err := gitSourceOf(view)
	if err == nil && gs.repo != nil {
		err = gs.repo.PrepareObjects(r.objectsPath(id, ""))
		for _, sub := range gs.modules {
			if err == nil {
				err = sub.repo.PrepareObjects(r.objectsPath(id, sub.Module))
			}
		}
	}
	if err == nil {
		err = writeLock(filepath.Join(bc.StateDir, lockFile), b.lock)
	}
	if err != nil {
		view.Close()
		return fail(coded(errors.Join(err, os.RemoveAll(bc.StateDir))))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		view.Close()
		return fail(coded(errors.Join(failure("io", "registry is closed"), os.RemoveAll(bc.StateDir))))
	}
	r.bases[id] = b
	r.retainLocked(b, nil)
	r.wakeWatch()
	return b, false, captured.Stats, warnings, nil
}

// baseTemplate returns the private git directory checkouts on capture b copy:
// written now for a new capture (writeTemplate), or the one a reused capture
// already keeps, once any still being written is done.
func (r *Registry) baseTemplate(b *baseRecord, reused bool) (gitTemplate, error) {
	if !reused || b.view.Domains().Repo("") == nil {
		return r.writeTemplate(b)
	}
	r.mu.Lock()
	written := b.template
	r.mu.Unlock()
	if written != nil {
		<-written
	}
	t, err := readTemplate(r.templatePath(b.view.Meta().ID))
	if errors.Is(err, fs.ErrNotExist) {
		return r.writeTemplate(b)
	}
	return t, err
}

// keepsPoolLocked reports whether f's prepared checkouts can move onto capture
// b, taken from git generation gitGen with the manifest lock hash: the same git
// state and rules, a repository, and no submodules, whose git directories the
// pool does not rebuild. Their configuration is then f.template's.
func (f *reuseFolder) keepsPoolLocked(b *baseRecord, gitGen uint64, hash string) bool {
	return f.warm != nil && f.template.dir != "" && f.warmGitGen == gitGen && f.warmLock == hash &&
		b.view.Domains().Repo("") != nil && len(submodules(b.view)) == 0
}

// templateLater writes b's private git directory template in the background,
// then lets f's pool fill from it while b is still f's warm capture. Until then
// b.template, which the caller made, is open: checkouts prepared meanwhile build
// their git directories from the repository, whose git state is still the
// template's. r.mu is held.
func (r *Registry) templateLater(f *reuseFolder, b *baseRecord) {
	r.retainLocked(b, nil)
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		template, err := r.writeTemplate(b)
		r.mu.Lock()
		defer r.mu.Unlock()
		defer r.releaseLocked(b, nil)
		close(b.template)
		if err != nil {
			r.enqueueNotice(baseNotice{base: b.view.Meta().ID, fault: "reusable git directory: " + err.Error()})
			return
		}
		if f.warm == b {
			f.template = template
			r.fillPoolLocked(f)
		}
	}()
}

// poolTarget is how many checkouts f keeps prepared: poolSize, as far as their git
// directories fit poolBudget, and at least one. r.mu is held.
func (f *reuseFolder) poolTarget() int {
	if f.template.bytes == 0 {
		return f.poolSize
	}
	return max(1, min(f.poolSize, int(poolBudget/f.template.bytes)))
}

// openWarmLocked opens checkout name (or one derived from the folder) on f's warm
// capture, taking a prepared one when the pool holds one. r.mu is held and released
// before it returns.
func (r *Registry) openWarmLocked(ctx context.Context, f *reuseFolder, name string) (api.NewReply, error) {
	f.warmUsed = time.Now()
	return r.openOnLocked(ctx, f.warm, f.template, name, filepath.Base(f.root), f)
}

// pending reports whether ch, a base's template signal, is still open.
func pending(ch chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return false
	default:
		return true
	}
}

// newOnBase opens checkout name (or one derived from the captured folder) on
// capture id as it was taken: no layers, and the private git directory checkouts
// opened on it then started from. A capture of a repository that kept none (see
// writeTemplate) is refused.
func (r *Registry) newOnBase(ctx context.Context, id, name string) (api.NewReply, error) {
	r.mu.Lock()
	b := r.bases[id]
	if b != nil && pending(b.template) {
		// Its template is being written; checkouts on it start from that.
		r.retainLocked(b, nil)
		r.mu.Unlock()
		<-b.template
		r.mu.Lock()
		r.releaseLocked(b, nil)
		if r.bases[id] != b {
			b = nil
		}
	}
	if b == nil || r.closed {
		r.mu.Unlock()
		return api.NewReply{}, failure("not_found", "no capture %s", id)
	}
	var template gitTemplate
	if b.view.Domains().Repo("") != nil {
		template.dir = r.templatePath(id)
		data, err := os.ReadFile(filepath.Join(template.dir, "config"))
		if err != nil {
			r.mu.Unlock()
			if errors.Is(err, fs.ErrNotExist) {
				return api.NewReply{}, failure("invalid", "capture %s keeps no git directory to start from", id)
			}
			return api.NewReply{}, coded(err)
		}
		template.config = string(data)
	}
	return r.openOnLocked(ctx, b, template, name, filepath.Base(b.view.Stats().Root), nil)
}

// openOnLocked opens checkout name (or one derived from fallback) on capture b,
// whose checkouts copy the private git directory template: one
// taken from f's pool when f is set and it holds one, else one prepared now outside
// the lock while its name is reserved and b retained, made a jj workspace there
// when b's folder is a jj workspace's root. Publication shares a durable state
// write with concurrent creates. r.mu is held and released before it returns.
func (r *Registry) openOnLocked(ctx context.Context, b *baseRecord, template gitTemplate, name, fallback string, f *reuseFolder) (api.NewReply, error) {
	name, err := r.pickNameLocked(name, fallback)
	if err != nil {
		r.mu.Unlock()
		return api.NewReply{}, err
	}
	r.operations.Add(1)
	defer r.operations.Done()
	r.reserved[name] = true
	var h *checkoutRecord
	if f != nil && len(f.pool) > 0 {
		n := len(f.pool)
		h, f.pool = f.pool[n-1], f.pool[:n-1]
	} else {
		r.retainLocked(b, nil)
	}
	if f != nil {
		r.fillPoolLocked(f)
	}
	r.mu.Unlock()
	if h != nil {
		err = r.namePrepared(h, name, template.config)
	} else {
		h, err = r.prepareOnWarm(b, template.dir, name, name)
	}
	var ws jjWorkspace
	var warnings []string
	if err == nil {
		ws, warnings = r.addWorkspace(ctx, b, name)
		err = r.installWorkspace(h, ws)
	}
	defer r.discardWorkspace(ws)
	r.mu.Lock()
	if err == nil && r.closed {
		err = failure("io", "registry is closed")
	}
	if err != nil {
		r.mu.Unlock()
		return api.NewReply{}, r.abandonCheckout(name, h, b, err)
	}
	delete(r.reserved, name)
	r.checkouts[name] = h
	r.moveRootLocked()
	seq := r.requestCommitLocked()
	// The capture was taken before this create, which read nothing of the folder.
	stats := h.base.view.Stats()
	stats.Hashed, stats.HashedBytes, stats.Seconds = 0, 0, 0
	reply := api.NewReply{Checkout: r.checkoutLocked(h), Capture: &stats, Warnings: append([]string{}, warnings...)}
	r.mu.Unlock()
	if err := r.commitState(seq); err != nil {
		r.mu.Lock()
		if r.checkouts[name] == h {
			delete(r.checkouts, name)
			r.reserved[name] = true
			r.moveRootLocked()
		}
		r.mu.Unlock()
		return api.NewReply{}, r.abandonCheckout(name, h, b, err)
	}
	r.created(reply.Checkout)
	return reply, nil
}

// namePrepared names prepared checkout h: it opens now, so its cache paths are
// fenced from now, its git directories mark it as checkout name, and its
// objects are its base's (see setWarmLocked). config is the root git
// directory's config as prepared.
func (r *Registry) namePrepared(h *checkoutRecord, name, config string) error {
	h.meta.Name, h.meta.Created = name, time.Now().UTC()
	h.fs.Rebase(h.base.view.Bottom(h.meta.Created))
	if !h.meta.Git {
		return nil
	}
	gitdir := r.gitdirPath(h.meta.dir())
	link, objects := filepath.Join(gitdir, "objects"), r.objectsPath(h.meta.Base, "")
	if target, err := os.Readlink(link); err != nil || target != objects {
		if err := errors.Join(os.Remove(link), hostfile.Symlink(objects, link)); err != nil {
			return err
		}
	}
	if len(submodules(h.base.view)) == 0 {
		return git.MarkNew(gitdir, config, name)
	}
	return markGitdirs(gitdir, name, h.base.view)
}

// prepareOnWarm writes the files of checkout name ("" to name it later) below
// directories named dir on warm capture b, copying the private git directory
// template, without registering it. At most prepareSlots run at once: many at a
// time contend for the same directories and all finish later than in turns.
func (r *Registry) prepareOnWarm(b *baseRecord, template, name, dir string) (*checkoutRecord, error) {
	gs, err := gitSourceOf(b.view)
	if err != nil {
		return nil, err
	}
	gs.template = template
	r.prepares <- struct{}{}
	defer func() { <-r.prepares }()
	return r.prepareCheckout(name, dir, b, nil, nil, nil, gs, nil)
}

// abandonCheckout removes the files of checkout name, reserved and retaining b,
// which failed with cause before publication (h, nil if it was not prepared; it
// sits on and retains b), then frees the name and releases b. It returns cause
// with any failure to remove the files.
func (r *Registry) abandonCheckout(name string, h *checkoutRecord, b *baseRecord, cause error) error {
	dir := name
	if h != nil {
		h.fs.Discard()
		h.fs.CloseFiles()
		dir, b = h.meta.dir(), h.base
	}
	err := coded(errors.Join(cause, r.removeCheckoutFiles(dir)))
	r.mu.Lock()
	delete(r.reserved, name)
	r.releaseLocked(b, nil)
	r.mu.Unlock()
	return err
}

// fillPoolLocked starts refilling f's pool to poolTarget in the background, with up
// to prepareSlots workers, unless it is full or its git template is pending.
func (r *Registry) fillPoolLocked(f *reuseFolder) {
	if f.warm == nil || r.closed || pending(f.warm.template) {
		return
	}
	for f.filling < prepareSlots && f.filling < f.poolTarget()-len(f.pool) {
		f.filling++
		r.workers.Add(1)
		go r.fillPool(f)
	}
}

// fillPool prepares checkouts on f's warm capture, one at a time, until the pool
// with those being prepared holds poolTarget or the warm capture changes; a failure
// stops it until the next create starts it again.
func (r *Registry) fillPool(f *reuseFolder) {
	defer r.workers.Done()
	for {
		r.mu.Lock()
		b, template := f.warm, f.template.dir
		if r.closed || b == nil || len(f.pool)+f.preparing >= f.poolTarget() || pending(b.template) {
			f.filling--
			r.mu.Unlock()
			return
		}
		f.preparing++
		r.retainLocked(b, nil)
		r.mu.Unlock()
		dir, err := randomID(preparedPrefix)
		var h *checkoutRecord
		if err == nil {
			h, err = r.prepareOnWarm(b, template, "", dir)
		}
		r.mu.Lock()
		f.preparing--
		if err == nil && f.warm == b && !r.closed {
			f.pool = append(f.pool, h)
			r.mu.Unlock()
			continue
		}
		f.filling--
		r.mu.Unlock()
		if err != nil {
			if dir != "" {
				err = errors.Join(err, r.removeCheckoutFiles(dir))
			}
			r.enqueueNotice(baseNotice{base: b.view.Meta().ID, fault: "prepare checkout: " + err.Error()})
			r.mu.Lock()
			r.releaseLocked(b, nil)
			r.mu.Unlock()
		} else {
			r.discardPrepared([]*checkoutRecord{h})
		}
		return
	}
}

// discardPrepared removes prepared checkouts taken out of a pool and releases
// their bases.
func (r *Registry) discardPrepared(pool []*checkoutRecord) {
	for _, h := range pool {
		h.fs.Discard()
		h.fs.CloseFiles()
		dir := h.meta.dir()
		if err := r.removeCheckoutFiles(dir); err != nil {
			r.enqueueNotice(baseNotice{base: h.meta.Base, fault: "remove prepared checkout: " + err.Error()})
		}
	}
	if len(pool) == 0 {
		return
	}
	r.mu.Lock()
	for _, h := range pool {
		r.releaseLocked(h.base, nil)
	}
	r.mu.Unlock()
}

// discardLater removes prepared checkouts in the background: a burst of creates
// waiting for a capture must not also wait for the old pool to go.
func (r *Registry) discardLater(pool []*checkoutRecord) {
	if len(pool) == 0 {
		return
	}
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		r.discardPrepared(pool)
	}()
}

// setWarmLocked makes b, captured with the manifest lock hash from generations gen
// and gitGen on, f's warm capture, whose checkouts copy the private git directory
// template, releasing the capture it replaces. A capture that
// differs from the one it replaces only in the work tree keeps that one's pool:
// the prepared git directories still hold the repository's state, and their
// checkouts move onto b (namePrepared points their objects at b's). It returns
// the pool it does not keep, for discardPrepared.
func (r *Registry) setWarmLocked(f *reuseFolder, b *baseRecord, gen, gitGen uint64, hash string, template gitTemplate) []*checkoutRecord {
	var kept []*checkoutRecord
	if f.warm != nil && f.warmGitGen == gitGen && f.warmLock == hash && template.config == f.template.config && len(submodules(b.view)) == 0 {
		kept, f.pool = f.pool, nil
		for _, h := range kept {
			h.fs.Rebase(b.view.Bottom(h.meta.Created))
			r.releaseLocked(h.base, nil)
			r.retainLocked(b, nil)
			h.base, h.meta.Base = b, b.view.Meta().ID
		}
	}
	drained := r.coolLocked(f)
	r.retainLocked(b, nil)
	f.warm, f.warmGen, f.warmGitGen, f.warmLock, f.warmUsed = b, gen, gitGen, hash, time.Now()
	f.template, f.pool = template, kept
	r.fillPoolLocked(f)
	return drained
}

// coolLocked releases f's warm capture and returns its pool for discardPrepared.
func (r *Registry) coolLocked(f *reuseFolder) []*checkoutRecord {
	if f.warm != nil {
		r.releaseLocked(f.warm, nil)
	}
	drained := f.pool
	f.warm, f.template, f.pool = nil, gitTemplate{}, nil
	return drained
}

// writeTemplate writes the private git directory that checkouts reusing b copy, from
// the repository's state now and b's captured index, below b's own storage; none
// for a capture of no repository. Copying it writes no more than the directory
// holds, where building one walks the repository's refs.
func (r *Registry) writeTemplate(b *baseRecord) (gitTemplate, error) {
	gs, err := gitSourceOf(b.view)
	if err != nil || gs.repo == nil {
		return gitTemplate{}, err
	}
	id := b.view.Meta().ID
	dir := r.templatePath(id)
	if err := os.RemoveAll(dir); err != nil {
		return gitTemplate{}, err
	}
	if err := r.makeGitdirs(dir, id, gs); err != nil {
		return gitTemplate{}, errors.Join(err, os.RemoveAll(dir))
	}
	t, err := readTemplate(dir)
	if err != nil {
		return gitTemplate{}, errors.Join(err, os.RemoveAll(dir))
	}
	return t, nil
}

// readTemplate reads the git template written at dir: its config and the size of
// its files, the shared objects aside.
func readTemplate(dir string) (gitTemplate, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		return gitTemplate{}, err
	}
	t := gitTemplate{dir: dir, config: string(data)}
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			t.bytes += info.Size()
		}
		return err
	})
	return t, err
}

// templatePath is where capture id keeps the private git directory its checkouts
// start from.
func (r *Registry) templatePath(id string) string {
	return filepath.Join(r.config.State, "bases", id, "git-template")
}

// gitdirInside reports whether b's repository keeps its git directory inside folder
// root, where the watcher sees commits, index and ref updates; captures of a linked
// work tree are not reused. A folder outside any repository has none to watch.
func gitdirInside(b *baseRecord, root string) bool {
	repo := b.view.Domains().Repo("")
	if repo == nil {
		return true
	}
	return inside(root, repo.GitDir) && inside(root, repo.CommonDir)
}

// inside reports whether p is dir or lies below it.
func inside(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// foldersChangedLocked bumps the generation of every reusing folder an edit in
// paths can change, and its git generation for edits in the repository's git
// directory (or lost events); r.mu is held for reading.
func (r *Registry) foldersChangedLocked(paths []string) {
	for root, f := range r.folders {
		changed, gitChanged := false, false
		for _, p := range paths {
			rel, ok := below(root, p)
			if !ok || !f.affectedBy(rel) {
				continue
			}
			changed = true
			if rel == "" || rel == ".git" || strings.HasPrefix(rel, ".git/") {
				gitChanged = true
				break
			}
		}
		if gitChanged {
			f.gitGen.Add(1)
		}
		if changed {
			f.gen.Add(1)
		}
	}
}

// affectedBy reports whether an edit at folder path rel can make the folder differ
// from a capture: anything but paths the warm capture serves live or leaves out,
// and the git directory's object, log and lock files. Without a warm capture every
// path counts. r.mu is held for reading.
func (f *reuseFolder) affectedBy(rel string) bool {
	if rel == "" {
		return true // events were lost
	}
	if gitPath, ok := strings.CutPrefix(rel, ".git/"); ok {
		switch {
		case strings.HasPrefix(gitPath, "objects/"), strings.HasPrefix(gitPath, "logs/"),
			strings.HasSuffix(gitPath, ".lock"), gitPath == "FETCH_HEAD", gitPath == git.RemotesFile:
			return false
		}
		return true
	}
	if f.warm != nil {
		return f.warm.view.Affects(rel)
	}
	return true
}

// coolFolders releases warm captures that the folder outgrew or nobody reused for
// warmTTL, with their pools, and forgets folders with no capture left.
func (r *Registry) coolFolders() {
	defer r.workers.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-tick.C:
		}
		var drained []*checkoutRecord
		forgot := false
		r.mu.Lock()
		for root, f := range r.folders {
			if f.capturing != nil || f.filling > 0 {
				continue
			}
			if f.warm != nil && (f.warmGen != f.gen.Load() || time.Since(f.warmUsed) > warmTTL) {
				drained = append(drained, r.coolLocked(f)...)
			}
			if f.warm == nil {
				delete(r.folders, root)
				forgot = true
			}
		}
		r.mu.Unlock()
		r.discardPrepared(drained)
		if forgot {
			r.wakeWatch()
		}
	}
}

// resolveRoot returns the source root of folder dir as source.Resolve does,
// remembering it for a repository's folders: a repeated create resolves without
// running git while the root still holds its .git and no folder between dir and
// the root gained one. Concurrent creates of a folder wait for one resolution.
func (r *Registry) resolveRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		r.rootsMu.Lock()
		root, known := r.roots[abs]
		pending := r.resolving[abs]
		if !known && pending == nil {
			pending = make(chan struct{})
			r.resolving[abs] = pending
			r.rootsMu.Unlock()
			break
		}
		r.rootsMu.Unlock()
		if !known {
			<-pending
			continue
		}
		if exists(filepath.Join(root, ".git")) && !nestedGit(root, abs) {
			return root, nil
		}
		r.rootsMu.Lock()
		if r.roots[abs] == root {
			delete(r.roots, abs)
		}
		r.rootsMu.Unlock()
	}
	root, err := source.Resolve(abs)
	r.rootsMu.Lock()
	if err == nil && exists(filepath.Join(root, ".git")) {
		r.roots[abs] = root
	}
	close(r.resolving[abs])
	delete(r.resolving, abs)
	r.rootsMu.Unlock()
	return root, err
}

// nestedGit reports whether a folder from dir up to, but not including, root holds
// a .git entry.
func nestedGit(root, dir string) bool {
	for inside(root, dir) && filepath.Clean(dir) != filepath.Clean(root) {
		if exists(filepath.Join(dir, ".git")) {
			return true
		}
		dir = filepath.Dir(dir)
	}
	return false
}
