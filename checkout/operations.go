package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/jj"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// gitSource says how a new checkout gets its private git directory, if at all.
type gitSource struct {
	repo     *git.Repo     // build one from this repository's state at capture
	index    string        // the captured index copy
	modules  []gitModule   // and one below it for each absorbed submodule, parents first
	template string        // or copy this one built the same way (see Registry.writeTemplate)
	ready    string        // or move this git directory prepared by prepareGit into place
	rev      *git.Revision // optional initial revision, already resolved
}

// gitModule is an absorbed submodule whose private git directory a new checkout
// builds from the submodule's state at capture.
type gitModule struct {
	repo *git.Repo
	source.Repo
}

// New opens a checkout from any folder (captured and classified now), from a
// snapshot, or on an existing capture as it was taken.
func (r *Registry) New(ctx context.Context, args api.NewArgs) (api.NewReply, error) {
	sources := 0
	for _, set := range []bool{args.Dir != "", args.Snapshot != "", args.Base != ""} {
		if set {
			sources++
		}
	}
	if sources != 1 {
		return api.NewReply{}, failure("invalid", "new needs exactly one of a folder, a snapshot or a capture")
	}
	if err := checkContext(ctx); err != nil {
		return api.NewReply{}, err
	}
	if args.Base != "" {
		if args.Rev != "" || len(args.Take) != 0 || len(args.Rules) != 0 {
			return api.NewReply{}, failure("invalid", "a checkout on a capture keeps its revision, takes and rules")
		}
		return r.newOnBase(ctx, args.Base, args.Name)
	}
	if args.Snapshot != "" {
		if args.Rev != "" {
			return api.NewReply{}, failure("invalid", "--rev and --from cannot be combined")
		}
		if len(args.Take) != 0 {
			return api.NewReply{}, failure("invalid", "--take and --from cannot be combined")
		}
		if len(args.Rules) != 0 {
			return api.NewReply{}, failure("invalid", "rules are fixed when a folder is captured; a checkout from a snapshot keeps its base's rules")
		}
		r.mu.Lock()
		s := r.snapshots[args.Snapshot]
		var tree store.OID
		var baseID, name string
		var err error
		switch {
		case r.closed:
			err = failure("io", "registry is closed")
		case s == nil:
			err = failure("not_found", "no snapshot %s", args.Snapshot)
		default:
			tree, baseID = s.git, s.base.view.Meta().ID
			name, err = r.reserveNameLocked(args.Name, s.meta.Checkout)
		}
		r.mu.Unlock()
		if err != nil {
			return api.NewReply{}, err
		}
		reserved := true // until createFromSnapshotLocked takes the name over
		defer func() {
			if reserved {
				r.unreserve(name)
			}
		}()
		ready, err := r.prepareGit(ctx, tree, filepath.Join(r.snapshotDir(s.meta.ID), "git"), baseID)
		if err != nil {
			return api.NewReply{}, coded(err)
		}
		ws, warnings := r.addWorkspace(ctx, s.base, name)
		defer r.discardWorkspace(ws)
		r.mu.Lock()
		var h *checkoutRecord
		switch {
		case r.closed:
			err = failure("io", "registry is closed")
		case r.snapshots[args.Snapshot] != s:
			err = failure("not_found", "no snapshot %s", args.Snapshot)
		default:
			reserved = false
			h, err = r.createFromSnapshotLocked(ctx, name, s, ready, ws)
		}
		var reply api.NewReply
		if err == nil {
			reply = api.NewReply{Checkout: r.checkoutLocked(h), Warnings: append([]string{}, warnings...)}
		}
		r.mu.Unlock()
		if discardErr := discardGit(ready); err == nil && discardErr != nil {
			err = discardErr
		}
		if err != nil {
			return api.NewReply{}, coded(err)
		}
		r.created(reply.Checkout)
		return reply, nil
	}
	if !filepath.IsAbs(args.Dir) {
		return api.NewReply{}, failure("invalid", "folder path must be absolute")
	}
	if reply, overlaps, err := r.newOverlappingMount(ctx, args.Dir, args); overlaps {
		return reply, err
	}
	root, err := r.resolveRoot(args.Dir)
	if err != nil {
		return api.NewReply{}, coded(err)
	}
	if reply, overlaps, err := r.newOverlappingMount(ctx, root, args); overlaps {
		return reply, err
	}
	var revision *git.Revision
	if args.Rev != "" {
		repo, repoErr := git.Open(root)
		if repoErr != nil {
			return api.NewReply{}, failure("invalid", "--rev needs a git repository: %s", root)
		}
		revision, err = repo.ResolveRevision(ctx, args.Rev)
		if err != nil {
			return api.NewReply{}, failure("invalid", "cannot resolve revision %q: %v", args.Rev, err)
		}
	}
	takes, err := resolveTakes(ctx, root, args.Take)
	if err != nil {
		return api.NewReply{}, err
	}
	m, err := manifest.Load(root, r.config.UserHome, args.Rules)
	if err != nil {
		return api.NewReply{}, failure("invalid", "%v", err)
	}
	if revision == nil && len(takes) == 0 && r.watcher != nil {
		return r.newOnFolder(ctx, root, m, args.Name)
	}
	reply, _, err := r.capture(ctx, root, m, args.Name, revision, takes)
	return reply, err
}

// capture opens a checkout named name (or one derived from root) on a new base: a
// capture of folder root classified by m's rules, taken now, at revision when set,
// with takes applied.
func (r *Registry) capture(ctx context.Context, root string, m manifest.Manifest, name string, revision *git.Revision, takes []base.Take) (api.NewReply, *baseRecord, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return api.NewReply{}, nil, failure("io", "registry is closed")
	}
	name, err := r.pickNameLocked(name, filepath.Base(root))
	if err != nil {
		r.mu.Unlock()
		return api.NewReply{}, nil, err
	}
	id := ""
	for id == "" || r.bases[id] != nil || exists(filepath.Join(r.config.State, "bases", id)) {
		if id, err = randomID("b_"); err != nil {
			r.mu.Unlock()
			return api.NewReply{}, nil, coded(err)
		}
	}
	r.operations.Add(1)
	defer r.operations.Done()
	r.reserved[name] = true
	r.intents[name] = diskIntent{Op: "creating", Checkout: name, Layer: id}
	if err = r.persistIntentLocked(); err != nil {
		delete(r.reserved, name)
		delete(r.intents, name)
		r.mu.Unlock()
		return api.NewReply{}, nil, coded(err)
	}
	r.mu.Unlock()
	config := r.baseConfig(id)
	options := source.Options{Root: root, Rules: m.Rules, Cache: r.statCachePath(root)}
	var view *base.Base
	var warnings []string
	if revision == nil && len(takes) == 0 {
		fork := time.Now().UTC()
		var captured source.Result
		if captured, warnings, err = base.Capture(ctx, config, options); err == nil {
			key := captured.ReuseKey()
			if b := r.retainReusable(root, key, m.Lock()); b != nil {
				reply, err := r.publishReused(ctx, name, config.StateDir, b, captured.Stats, warnings)
				return reply, b, err
			}
			view, err = base.CreateCaptured(config, captured, fork, key)
		}
	} else {
		view, warnings, err = base.CreateRevision(ctx, config, options, revision, takes...)
	}
	if err != nil {
		if errors.Is(err, base.ErrTake) {
			err = failure("invalid", "%v", err)
		}
		return api.NewReply{}, nil, r.abortCreate(name, config.StateDir, err)
	}
	b := &baseRecord{view: view, lock: m.Lock(), rules: m.Rules}
	gs, err := gitSourceOf(view)
	gs.rev = revision
	if err == nil && gs.repo != nil {
		err = gs.repo.PrepareObjects(r.objectsPath(id, ""))
		for _, sub := range gs.modules {
			if err == nil {
				err = sub.repo.PrepareObjects(r.objectsPath(id, sub.Module))
			}
		}
	}
	if err == nil {
		err = writeLock(filepath.Join(config.StateDir, lockFile), b.lock)
	}
	if err != nil {
		view.Close()
		return api.NewReply{}, nil, r.abortCreate(name, config.StateDir, err)
	}
	reply, err := r.publishBase(ctx, name, id, b, gs)
	if err != nil {
		return api.NewReply{}, nil, err
	}
	stats := view.Stats()
	reply.Capture, reply.Warnings = &stats, append(append([]string{}, warnings...), reply.Warnings...)
	return reply, b, nil
}

// retainReusable retains and returns a base that an ordinary capture of root
// with this reuse key and resolved manifest lock may share, or nil when the key
// is empty or no base matches. Collection picks bases to remove under the same
// lock, so a base it has picked is never returned.
func (r *Registry) retainReusable(root, key string, lock manifest.Lock) *baseRecord {
	if key == "" {
		return nil
	}
	hash := lock.Hash()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bases {
		meta := b.view.Meta()
		if meta.Dir == root && meta.Remote == nil && meta.ReuseKey == key && b.lock.Hash() == hash {
			r.retainLocked(b, nil)
			return b
		}
	}
	return nil
}

// publishReused opens the reserved checkout on a base from retainReusable,
// discarding the state directory the new capture wrote; the retained reference
// ends once the checkout holds its own.
func (r *Registry) publishReused(ctx context.Context, name, stateDir string, b *baseRecord, stats api.Capture, warnings []string) (api.NewReply, error) {
	defer func() {
		r.mu.Lock()
		r.releaseLocked(b, nil)
		r.mu.Unlock()
	}()
	if err := os.RemoveAll(stateDir); err != nil {
		return api.NewReply{}, r.abortCreate(name, stateDir, err)
	}
	gs, err := gitSourceOf(b.view)
	if err != nil {
		return api.NewReply{}, r.abortCreate(name, stateDir, err)
	}
	reply, err := r.publishBase(ctx, name, b.view.Meta().ID, b, gs)
	if err != nil {
		return api.NewReply{}, err
	}
	reply.Capture, reply.Warnings = &stats, append(append([]string{}, warnings...), reply.Warnings...)
	return reply, nil
}

// gitSourceOf says how checkouts on view get their private git directories: from
// the captured repository and index, with one below for each absorbed submodule.
// A capture of no repository gives none.
func gitSourceOf(view *base.Base) (gitSource, error) {
	var gs gitSource
	repo := view.Domains().Repo("")
	if repo == nil {
		return gs, nil
	}
	gs.repo = repo
	for _, rr := range view.Repos() {
		if rr.Path == "" {
			gs.index = rr.Index
		}
	}
	for _, sub := range submodules(view) {
		repo := view.Domains().Repo(sub.Path)
		if repo == nil {
			return gs, fmt.Errorf("submodule %s: repository unavailable", sub.Path)
		}
		gs.modules = append(gs.modules, gitModule{repo: repo, Repo: sub})
	}
	return gs, nil
}

// submodules lists the absorbed submodules of a base's capture, parents first;
// each gets a private git directory below the root's (source.Repo.Module).
func submodules(view *base.Base) []source.Repo {
	var out []source.Repo
	for _, repo := range view.Repos() {
		if repo.Module != "" {
			out = append(out, repo)
		}
	}
	return out
}

// publishBase registers a new or retained base and opens the reserved checkout
// on it, a jj workspace when b's folder is a jj workspace's root.
func (r *Registry) publishBase(ctx context.Context, name, id string, b *baseRecord, gs gitSource) (api.NewReply, error) {
	ws, warnings := r.addWorkspace(ctx, b, name)
	defer r.discardWorkspace(ws)
	r.mu.Lock()
	r.bases[id] = b
	r.wakeWatch()
	err := r.persistIntentLocked()
	var reply api.NewReply
	if err == nil {
		var h *checkoutRecord
		if h, err = r.installCheckoutLocked(ctx, name, b, nil, nil, gs, ws); err == nil {
			delete(r.reserved, name)
			delete(r.intents, name)
			r.moveRootLocked()
			if err = r.persistLocked(); err == nil {
				reply = api.NewReply{Checkout: r.checkoutLocked(h), Warnings: append([]string{}, warnings...)}
			} else {
				delete(r.checkouts, name)
				r.releaseLocked(b, nil)
			}
		}
	}
	if err != nil {
		cleanupErr := r.removeCheckoutFiles(name)
		delete(r.reserved, name)
		delete(r.intents, name)
		persistErr := r.persistLocked()
		r.wakeGC()
		r.mu.Unlock()
		r.wakeNotices()
		return api.NewReply{}, coded(errors.Join(err, cleanupErr, persistErr))
	}
	r.mu.Unlock()
	r.created(reply.Checkout)
	r.wakeNotices()
	return reply, nil
}

// newOverlappingMount handles New on a folder that overlaps the daemon's own
// mount, reporting overlaps false for any other folder. A base on such a folder
// would be served by reading through the mount from the mount's own request
// handlers, which can deadlock the whole mount, so a checkout's root forks that
// checkout instead; the mount root, folders below a checkout's root and folders
// holding the mount fail.
func (r *Registry) newOverlappingMount(ctx context.Context, dir string, args api.NewArgs) (reply api.NewReply, overlaps bool, err error) {
	rel, inMount, holds := r.mountOverlap(dir)
	switch {
	case holds:
		return api.NewReply{}, true, failure("invalid", "%s holds the checkout mount %s; open a folder outside it", dir, r.config.Mount)
	case !inMount:
		return api.NewReply{}, false, nil
	}
	name, sub, _ := strings.Cut(filepath.ToSlash(rel), "/")
	switch {
	case rel == ".":
		return api.NewReply{}, true, failure("invalid", "%s is the checkout mount; open a folder or fork a checkout", dir)
	case sub != "":
		return api.NewReply{}, true, failure("invalid", "%s is inside checkout %s; `ovnfs fork %s` copies the whole checkout", dir, name, name)
	case args.Rev != "" || len(args.Take) != 0 || len(args.Rules) != 0:
		return api.NewReply{}, true, failure("invalid", "%s is checkout %s, which new forks; --rev, --take and --rule apply only to folders", dir, name)
	}
	f, err := r.Fork(ctx, api.ForkArgs{Checkout: name, Name: args.Name})
	if err != nil {
		return api.NewReply{}, true, err
	}
	return api.NewReply{Checkout: f.Checkout, Warnings: append([]string{fmt.Sprintf("%s is checkout %s: forked it at %s", dir, name, f.Snapshot.ID)}, f.Warnings...)}, true, nil
}

// underMount returns dir relative to the daemon's mount when dir is the mount or below it.
func (r *Registry) underMount(dir string) (string, bool) {
	for _, m := range r.mounts {
		if rel, err := filepath.Rel(m, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel, true
		}
	}
	return "", false
}

// mountOverlap reports how folder dir overlaps the daemon's mount: rel names
// dir from the mount when dir is the mount or below it (inside), and holds
// reports dir holding the mount. Spellings unlike the mount's own (letter case
// a volume ignores, a firmlink, a bind mount) are told by file identity, which
// asks the mount for its root: the caller holds no registry lock, since the
// mount's server takes them.
func (r *Registry) mountOverlap(dir string) (rel string, inside, holds bool) {
	if rel, inside = r.underMount(dir); inside {
		return rel, true, false
	}
	for _, m := range r.mounts {
		if _, holds = below(dir, m); holds {
			return "", false, true
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false, false
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", false, false
	}
	// The resolved spelling, so that every real ancestor of the mount is walked.
	mount := r.mounts[len(r.mounts)-1]
	if _, holds = ancestorAt(filepath.Dir(mount), info); holds {
		return "", false, true
	}
	root, err := os.Stat(mount)
	if err != nil {
		return "", false, false
	}
	top, inside := ancestorAt(real, root)
	if !inside {
		return "", false, false
	}
	rel, err = filepath.Rel(top, real)
	return rel, err == nil, false
}

// ancestorAt returns the nearest of path p and its ancestors that is the file want.
func ancestorAt(p string, want os.FileInfo) (string, bool) {
	for {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, want) {
			return p, true
		}
		up := filepath.Dir(p)
		if up == p {
			return "", false
		}
		p = up
	}
}

// mountPaths spells the mount as configured and with its parent's symlinks
// resolved (macOS /tmp is /private/tmp), without looking inside the mount.
func mountPaths(mount string) []string {
	paths := []string{mount}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(mount)); err == nil {
		if real := filepath.Join(parent, filepath.Base(mount)); real != mount {
			paths = append(paths, real)
		}
	}
	return paths
}

// abortCreate undoes a folder create that failed before its base was published.
func (r *Registry) abortCreate(name, baseDir string, cause error) error {
	r.mu.Lock()
	delete(r.reserved, name)
	delete(r.intents, name)
	persistErr := r.persistLocked()
	r.mu.Unlock()
	return coded(errors.Join(cause, os.RemoveAll(baseDir), persistErr))
}

// writeLock durably records a base's resolved manifest.
func writeLock(name string, lock manifest.Lock) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = durable.Sync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// statCachePath names a source folder's stat cache, shared by every capture of it.
func (r *Registry) statCachePath(root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(r.config.State, "sources", hex.EncodeToString(sum[:8])+".stat")
}

// pickNameLocked validates an explicit name, or derives a free one from hint.
func (r *Registry) pickNameLocked(name, hint string) (string, error) {
	taken := func(n string) bool { return r.checkouts[n] != nil || r.reserved[n] }
	if name != "" {
		if !namePattern.MatchString(name) {
			return "", failure("invalid", "invalid checkout name %q: use lowercase letters, digits, '.', '_' and '-'", name)
		}
		if taken(name) {
			return "", failure("exists", "checkout %s already exists", name)
		}
		return name, nil
	}
	stem := sanitizeName(hint)
	for n := 1; ; n++ {
		candidate := stem
		if n > 1 {
			candidate = stem + "-" + strconv.Itoa(n)
		}
		if !taken(candidate) {
			return candidate, nil
		}
	}
}

// reserveNameLocked picks a name as pickNameLocked does and reserves it for a
// create that leaves the registry lock before registering its checkout, to make
// its jj workspace; createFromSnapshotLocked takes the reservation over, and
// unreserve frees it for a create that fails before.
func (r *Registry) reserveNameLocked(name, hint string) (string, error) {
	name, err := r.pickNameLocked(name, hint)
	if err == nil {
		r.reserved[name] = true
	}
	return name, err
}

// unreserve frees a name reserveNameLocked reserved.
func (r *Registry) unreserve(name string) {
	r.mu.Lock()
	delete(r.reserved, name)
	r.mu.Unlock()
}

// sanitizeName maps a folder name onto the checkout-name grammar.
func sanitizeName(hint string) string {
	var b strings.Builder
	for _, ch := range strings.ToLower(hint) {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '.', ch == '_', ch == '-':
			b.WriteRune(ch)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if len(name) > 56 {
		name = name[:56]
	}
	if name == "" {
		return "checkout"
	}
	return name
}

// installCheckoutLocked creates the upper and private git directory, installs
// the jj workspace ws, and registers the checkout before publication.
func (r *Registry) installCheckoutLocked(ctx context.Context, name string, b *baseRecord, stack []string, parent *string, gs gitSource, ws jjWorkspace) (*checkoutRecord, error) {
	layers, err := r.layerStackLocked(stack, b.view.Meta().ID)
	if err != nil {
		return nil, err
	}
	h, err := r.prepareCheckout(name, name, b, layers, stack, parent, gs, r.lineageCheckoutsLocked(deref(parent)))
	if err == nil {
		err = r.installWorkspace(h, ws)
	}
	if err != nil {
		return nil, err
	}
	r.checkouts[name] = h
	r.retainLocked(b, stack)
	return h, nil
}

// prepareCheckout creates the upper and private git directory of checkout name
// below uppers/dir and git/dir, and opens its overlay on b and layers (stack's),
// without registering it. It reads registry state only for gitfiles the layers
// carry (installGitfiles), so with an empty stack it runs without r.mu while the
// name is reserved; owners are lineageCheckoutsLocked's for parent. An empty name
// prepares a checkout to be named later (see namePrepared): its git directory is
// not marked yet.
func (r *Registry) prepareCheckout(name, dir string, b *baseRecord, layers []*overlay.Layer, stack []string, parent *string, gs gitSource, owners map[string]bool) (*checkoutRecord, error) {
	upper := r.upperPath(dir)
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return nil, err
	}
	hasGit := gs.repo != nil || gs.ready != ""
	if hasGit {
		gitdir := r.gitdirPath(dir)
		err := os.RemoveAll(gitdir)
		switch {
		case err != nil:
		case gs.template != "":
			err = git.CopyGitdir(gs.template, gitdir, r.objectsPath(b.view.Meta().ID, ""))
		case gs.repo != nil:
			err = r.makeGitdirs(gitdir, b.view.Meta().ID, gs)
		default:
			err = os.Rename(gs.ready, gitdir)
		}
		if err == nil && gs.rev != nil {
			err = gs.rev.SetHEAD(gitdir)
		}
		if err == nil && name != "" {
			err = markGitdirs(gitdir, name, b.view)
		}
		if err == nil {
			err = writeGitfile(upper, gitdir)
		}
		if err != nil {
			return nil, err
		}
	}
	created := time.Now().UTC()
	fs, err := overlay.New(upper, layers, b.view.Bottom(created), r.config.Memory)
	if err != nil {
		return nil, err
	}
	var copied map[string]copiedDir
	if hasGit {
		if copied, err = r.installGitfiles(fs, name, dir, b.view, owners); err != nil {
			return nil, err
		}
	}
	meta := checkoutMeta{Name: name, Base: b.view.Meta().ID, Parent: copyString(parent), Git: hasGit, Created: created, Copied: copied}
	if dir != name {
		meta.Dir = dir
	}
	h := &checkoutRecord{meta: meta, base: b, stack: append([]string{}, stack...), upper: upper, fs: fs}
	h.generation.Store(1)
	return h, nil
}

// writeGitfile points a checkout's root at its private git directory.
func writeGitfile(upper, gitdir string) error { return git.WriteGitfile(upper, gitdir) }

// makeGitdirs builds a new checkout's private git directory and, below it where git
// looks for them, one for each absorbed submodule, all from the repositories' state
// at capture and each borrowing its repository's objects through the base.
func (r *Registry) makeGitdirs(gitdir, baseID string, gs gitSource) error {
	if err := gs.repo.MakeGitdir(gitdir, r.objectsPath(baseID, ""), gs.index); err != nil {
		return err
	}
	for _, m := range gs.modules {
		dir := filepath.Join(gitdir, filepath.FromSlash(m.Module))
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		if err := m.repo.MakeGitdir(dir, r.objectsPath(baseID, m.Module), m.Index); err != nil {
			return err
		}
	}
	return nil
}

// markGitdirs marks a checkout's private git directory and those of its absorbed
// submodules that it still has as belonging to checkout name (git.MarkCheckout).
func markGitdirs(gitdir, name string, view *base.Base) error {
	if err := git.MarkCheckout(gitdir, name); err != nil {
		return err
	}
	for _, sub := range submodules(view) {
		err := git.MarkCheckout(filepath.Join(gitdir, filepath.FromSlash(sub.Module)), name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// installGitfiles points a new checkout's submodules at their private git
// directories below its own (git/dir), as the root's .git points at that
// directory, and returns the directories it copied up to hold the gitfiles
// (copiedDir). A gitfile inherited from the snapshot's layers keeps its place (a
// moved submodule stays moved, a deinitialized one stays without .git) and only
// changes its target when that lies below the private git directory of a checkout
// in the snapshot's lineage (owners, by checkoutMeta.dir), whose git state this
// checkout's was copied from: it moves to the same place in this one's. An
// absorbed submodule of the capture that no layer touched gets the gitfile it had
// when its checkout was opened, which was infrastructure and never frozen into a
// layer (see upperEmpty). Writes go through fsys, which copies directories up
// with their metadata.
func (r *Registry) installGitfiles(fsys *overlay.FS, name, dir string, view *base.Base, owners map[string]bool) (map[string]copiedDir, error) {
	gitdir := r.gitdirPath(dir)
	subs := submodules(view)
	if len(subs) == 0 && !exists(filepath.Join(gitdir, "modules")) {
		return nil, nil
	}
	var written []string
	inherited := map[string]bool{}
	for _, l := range fsys.Stack() {
		for _, p := range l.GitEntries() {
			if e, _ := l.Get(p); p != ".git" && e.Kind == overlay.KindFile {
				inherited[p] = true
			}
		}
	}
	// Git writes relative gitfiles from the physical location of the checkout it
	// ran in, the mount with its parent's symlinks resolved, at the same depth.
	mount := filepath.Join(r.mounts[len(r.mounts)-1], name)
	for _, p := range source.SortedPaths(inherited) {
		target, err := readGitfile(fsys, p)
		if err != nil {
			return nil, err
		}
		if target == "" {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(mount, filepath.FromSlash(path.Dir(p)), target)
		}
		owner, rest, ok := r.gitdirOwner(target)
		if !ok || owner == dir || !owners[owner] || rest == "" {
			continue
		}
		if err := writeGitfileAt(fsys, p, filepath.Join(gitdir, rest)); err != nil {
			return nil, err
		}
		written = append(written, p)
	}
	for _, sub := range subs {
		touched := false
		for _, l := range fsys.Stack() {
			_, atDir := l.Get(sub.Path)
			_, atGitfile := l.Get(sub.Path + "/.git")
			touched = touched || atDir || atGitfile
		}
		dir := filepath.Join(gitdir, filepath.FromSlash(sub.Module))
		if touched || !exists(dir) {
			continue
		}
		if info, err := fsys.Lstat(sub.Path); errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
			continue
		} else if err != nil {
			return nil, err
		}
		p := path.Join(sub.Path, ".git")
		if err := writeGitfileAt(fsys, p, dir); err != nil {
			return nil, err
		}
		written = append(written, p)
	}
	if err := fsys.Flush(); err != nil {
		return nil, err
	}
	// The upper was fresh, so every directory in it now was copied up for a gitfile.
	copied := map[string]copiedDir{}
	for _, p := range written {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if _, ok := copied[d]; ok {
				break
			}
			if _, err := os.Lstat(filepath.Join(fsys.Upper(), filepath.FromSlash(d))); err != nil {
				continue
			}
			info, err := fsys.Lstat(d)
			if err != nil {
				return nil, err
			}
			copied[d] = copiedDir{Mode: info.Mode(), Mtime: info.ModTime()}
		}
	}
	if len(copied) == 0 {
		return nil, nil
	}
	return copied, nil
}

// lineageCheckoutsLocked names, by checkoutMeta.dir, the checkouts whose snapshots
// make up snapshot id's lineage, the snapshot's own included: a checkout from it
// inherits their layers and a copy of their git state.
func (r *Registry) lineageCheckoutsLocked(id string) map[string]bool {
	dirs := map[string]bool{}
	for id != "" {
		s := r.lineage[id]
		if s == nil {
			break
		}
		dirs[s.meta.dir()] = true
		id = s.stackParent
	}
	return dirs
}

// gitdirOwner reports, by checkoutMeta.dir, which checkout's private git directory
// holds target, and target's path inside it ("" for the directory itself); ok is false for a target
// outside every one. Paths are compared as spelled, never resolved, so a target
// inside the mount is not looked up through it.
func (r *Registry) gitdirOwner(target string) (owner, rest string, ok bool) {
	root := filepath.Join(r.config.State, "git")
	spellings := []string{root}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		spellings = append(spellings, real)
	}
	target = filepath.Clean(target)
	for _, base := range spellings {
		rel, err := filepath.Rel(base, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		owner, rest, _ = strings.Cut(filepath.ToSlash(rel), "/")
		return owner, filepath.FromSlash(rest), true
	}
	return "", "", false
}

// readGitfile returns the target of the gitfile at checkout path p, "" when p is
// not a regular file holding one.
func readGitfile(fsys *overlay.FS, p string) (string, error) {
	info, err := fsys.Lstat(p)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	f, err := fsys.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", err
	}
	target, _ := git.ParseGitfile(content)
	return target, nil
}

// writeGitfileAt writes a gitfile pointing at gitdir to checkout path p through
// fsys and keeps the mtime of p's directory, so a directory copied up only to
// hold the gitfile still matches what lies beneath it (see upperEmpty).
func writeGitfileAt(fsys *overlay.FS, p, gitdir string) error {
	content, err := git.Gitfile(gitdir)
	if err != nil {
		return err
	}
	dir, err := fsys.Lstat(path.Dir(p))
	if err != nil {
		return err
	}
	f, err := fsys.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return fsys.Chtimes(path.Dir(p), dir.ModTime(), dir.ModTime())
}

// createFromSnapshotLocked attaches a new checkout to a snapshot's stack, to the git
// directory prepareGit made from it (ready, "" for none) and to the jj workspace
// ws, publishing it with one durable state write; the reservation of name
// (reserveNameLocked) ends here. It needs no recovery intent: the
// registry holds its lock throughout, the snapshot already retains the base, and
// reopening after a crash removes the upper and git directory of a checkout the
// state does not name.
func (r *Registry) createFromSnapshotLocked(ctx context.Context, name string, s *snapRecord, ready string, ws jjWorkspace) (*checkoutRecord, error) {
	delete(r.reserved, name)
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	h, err := r.installCheckoutLocked(ctx, name, s.base, s.stack, &s.meta.ID, gitSource{ready: ready}, ws)
	if err == nil {
		r.moveRootLocked()
		if err = r.persistLocked(); err == nil {
			return h, nil
		}
		delete(r.checkouts, name)
		r.releaseLocked(s.base, s.stack)
	}
	return nil, errors.Join(err, r.removeCheckoutFiles(name), r.persistLocked())
}

// created adds the checkout's git remote (syncRemotes) and notifies export
// watchers and subscribers after publication is durable.
func (r *Registry) created(h api.Checkout) {
	r.mu.RLock()
	var repo *git.Repo
	if rec := r.checkouts[h.Name]; rec != nil {
		repo = localRepo(rec.base)
	}
	r.mu.RUnlock()
	r.syncRemotes(repo)
	r.emit(Event{Name: "created", Payload: map[string]any{"checkout": h}})
	if r.config.Changed != nil {
		r.config.Changed(h.Name, true)
	}
}

// Snap freezes a checkout's changes into a snapshot and optionally waits for its tree id.
func (r *Registry) Snap(ctx context.Context, args api.SnapArgs) (api.SnapReply, error) {
	if err := checkContext(ctx); err != nil {
		return api.SnapReply{}, err
	}
	h, _, err := r.liveCheckout(args.Checkout)
	if err != nil {
		return api.SnapReply{}, err
	}
	tree, err := r.saveGit(h)
	if err != nil {
		return api.SnapReply{}, coded(err)
	}
	if err = r.freezeAndLock(h); err != nil {
		return api.SnapReply{}, err
	}
	s, err := r.freezeLocked(ctx, h, args.Label, tree, true)
	h.freeze.Unlock()
	var result snapMeta
	if s != nil {
		result = cloneSnapMeta(s.meta)
	}
	r.mu.Unlock()
	if err != nil {
		return api.SnapReply{}, coded(err)
	}
	r.emit(Event{Name: "snapshotted", Payload: map[string]any{"snapshot": snapshotOf(result)}})
	r.mu.Lock()
	s.announced = true
	r.scheduleHashesLocked()
	r.mu.Unlock()
	if !args.Wait {
		return api.SnapReply{Snapshot: snapshotOf(result)}, nil
	}
	return r.waitHashed(ctx, s)
}

// waitHashed blocks until a snapshot's tree id is known.
func (r *Registry) waitHashed(ctx context.Context, s *snapRecord) (api.SnapReply, error) {
	select {
	case <-ctx.Done():
		return api.SnapReply{}, coded(ctx.Err())
	case <-s.done:
	}
	r.mu.RLock()
	result, err := cloneSnapMeta(s.meta), s.hashErr
	r.mu.RUnlock()
	return api.SnapReply{Snapshot: snapshotOf(result)}, coded(err)
}

// Fork freezes and creates under one registry lock so no competing registry mutation
// interleaves; the child's name is reserved, the git directory stored and the child's
// copy and jj workspace made before it, and the freeze and the child are published by
// one durable state write.
func (r *Registry) Fork(ctx context.Context, args api.ForkArgs) (api.ForkReply, error) {
	if err := checkContext(ctx); err != nil {
		return api.ForkReply{}, err
	}
	h, baseID, err := r.liveCheckout(args.Checkout)
	if err != nil {
		return api.ForkReply{}, err
	}
	r.mu.Lock()
	child, err := r.reserveNameLocked(args.Name, args.Checkout)
	r.mu.Unlock()
	if err != nil {
		return api.ForkReply{}, err
	}
	reserved := true // until createFromSnapshotLocked takes the name over
	defer func() {
		if reserved {
			r.unreserve(child)
		}
	}()
	tree, err := r.saveGit(h)
	if err != nil {
		return api.ForkReply{}, coded(err)
	}
	ready, err := r.prepareGit(ctx, tree, "", baseID)
	if err != nil {
		return api.ForkReply{}, coded(err)
	}
	defer discardGit(ready)
	ws, warnings := r.addWorkspace(ctx, h.base, child)
	defer r.discardWorkspace(ws)
	if err = r.freezeAndLock(h); err != nil {
		return api.ForkReply{}, err
	}
	if h.meta.Base != baseID {
		h.freeze.Unlock()
		r.mu.Unlock()
		return api.ForkReply{}, failure("conflict", "checkout %s moved to another base while forking; retry", h.meta.Name)
	}
	s, err := r.freezeLocked(ctx, h, args.Label, tree, false)
	h.freeze.Unlock()
	if err != nil {
		r.mu.Unlock()
		return api.ForkReply{}, coded(err)
	}
	result := snapshotOf(s.meta)
	reserved = false
	created, createErr := r.createFromSnapshotLocked(ctx, child, s, ready, ws)
	var childCheckout api.Checkout
	if createErr != nil {
		// The freeze stays published even when its child is not.
		createErr = errors.Join(createErr, r.persistLocked())
	} else {
		childCheckout = r.checkoutLocked(created)
	}
	r.mu.Unlock()
	r.emit(Event{Name: "snapshotted", Payload: map[string]any{"snapshot": result}})
	if createErr == nil {
		r.created(childCheckout)
	}
	r.mu.Lock()
	s.announced = true
	r.scheduleHashesLocked()
	r.mu.Unlock()
	if createErr != nil {
		return api.ForkReply{}, coded(createErr)
	}
	return api.ForkReply{Snapshot: result, Checkout: childCheckout, Warnings: append([]string{}, warnings...)}, nil
}

// upperEmpty excludes only infrastructure: the work directory, the root's gitfile,
// the checkout's jj workspace, and the gitfiles installGitfiles wrote into the
// upper (absolute, into the checkout's own private git directory) together with
// the directories it copied up to hold them (checkoutMeta.Copied), as long as
// those keep the metadata they were copied with. Every real change requires a layer.
func (r *Registry) upperEmpty(h *checkoutRecord) (bool, error) {
	in := infra{copied: h.meta.Copied, workspace: h.meta.JJ != ""}
	var workspace []string // memory files there are no change either
	if in.workspace {
		workspace = []string{jj.Dir}
	}
	if h.fs.Unflushed(workspace...) {
		return false, nil
	}
	if h.meta.Git {
		in.gitdir = r.gitdirPath(h.meta.dir())
	}
	empty, _, err := infraOnly(h.fs, h.upper, "", in)
	return empty, err
}

// infra is what of a checkout's upper is infrastructure (see upperEmpty).
type infra struct {
	gitdir    string               // the checkout's private git directory; "" for none
	copied    map[string]copiedDir // checkoutMeta.Copied
	workspace bool                 // the root's .jj is the checkout's jj workspace
}

// infraOnly reports whether the upper directory dir, at checkout path rel, holds
// nothing but infrastructure (see upperEmpty), and whether it holds a gitfile into
// the checkout's private git directory. A directory counts only when it was copied
// up for gitfiles, holds one, and keeps the mode and mtime copied records for it;
// otherwise it is a change (a chmod, a touch, or a removal that left it without a
// gitfile).
func infraOnly(fsys *overlay.FS, dir, rel string, in infra) (empty, gitfiles bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false, err
	}
	for _, entry := range entries {
		p := path.Join(rel, entry.Name())
		switch {
		case p == ".git" || p == ".wh..wh.work" || (p == jj.Dir && in.workspace):
		case in.gitdir != "" && entry.Name() == ".git" && entry.Type().IsRegular():
			content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				return false, false, err
			}
			target, _ := git.ParseGitfile(content)
			if !filepath.IsAbs(target) || !strings.HasPrefix(target, in.gitdir+string(filepath.Separator)) {
				return false, false, nil
			}
			gitfiles = true
		case entry.IsDir():
			was, ok := in.copied[p]
			if !ok {
				return false, false, nil
			}
			info, err := fsys.Lstat(p)
			if err != nil {
				return false, false, err
			}
			if info.Mode() != was.Mode || !info.ModTime().Equal(was.Mtime) {
				return false, false, nil
			}
			ok, held, err := infraOnly(fsys, filepath.Join(dir, entry.Name()), p, in)
			if err != nil || !ok || !held {
				return false, false, err
			}
			gitfiles = true
		default:
			return false, false, nil
		}
	}
	return true, gitfiles, nil
}

// liveCheckout finds a live checkout and its base before an operation's work outside
// the registry lock.
func (r *Registry) liveCheckout(name string) (*checkoutRecord, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, "", failure("io", "registry is closed")
	}
	h := r.checkouts[name]
	if h == nil || h.retired.Load() {
		return nil, "", failure("not_found", "no checkout %s", name)
	}
	return h, h.meta.Base, nil
}

// stillLiveLocked confirms a checkout found by liveCheckout was neither removed nor
// replaced while the registry lock was released.
func (r *Registry) stillLiveLocked(h *checkoutRecord) error {
	if r.closed {
		return failure("io", "registry is closed")
	}
	if r.checkouts[h.meta.Name] != h || h.retired.Load() {
		return failure("not_found", "no checkout %s", h.meta.Name)
	}
	return nil
}

// freezeAndLock takes h's freeze lock exclusively and then the registry lock,
// in that order (see checkoutRecord.freeze), and confirms h is still live; on
// failure it holds neither.
func (r *Registry) freezeAndLock(h *checkoutRecord) error {
	h.freeze.Lock()
	r.mu.Lock()
	if err := r.stillLiveLocked(h); err != nil {
		r.mu.Unlock()
		h.freeze.Unlock()
		return err
	}
	return nil
}

// freezeLocked freezes an upper into a snapshot whose private git directory saveGit
// stored as gitTree; fork defers its durable commit into child publication. Only a
// freeze that moves the upper publishes a recovery intent first; with no layer the
// snapshot is all metadata and simply does not exist until committed. The caller
// holds h's freeze lock and the registry lock (see freezeAndLock).
func (r *Registry) freezeLocked(ctx context.Context, h *checkoutRecord, label string, gitTree store.OID, commit bool) (*snapRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	// The upper directory becomes the layer, so it must hold every change.
	if err := h.fs.Flush(); err != nil {
		return nil, err
	}
	empty, err := r.upperEmpty(h)
	if err != nil {
		return nil, err
	}
	layerID := ""
	if !empty {
		for {
			layerID, err = randomID("l_")
			if err != nil {
				return nil, err
			}
			if r.layers[layerID] == nil && !exists(filepath.Join(r.config.State, "layers", layerID)) {
				break
			}
		}
	}
	r.nextSnapshot++
	id := snapshotID(r.nextSnapshot)
	parent := h.meta.Head
	if parent == nil {
		parent = h.meta.Parent
	}
	stack := append([]string{}, h.stack...)
	if layerID != "" {
		stack = append([]string{layerID}, stack...)
	}
	stackParent := deref(parent)
	s := &snapRecord{meta: snapMeta{ID: id, Checkout: h.meta.Name, Dir: h.meta.Dir, Parent: copyString(parent), Label: label, Created: time.Now().UTC()}, base: h.base, stack: stack, layer: layerID, stackParent: stackParent, git: gitTree, done: make(chan struct{})}
	r.snapshots[id] = s
	r.lineage[id] = s
	if layerID != "" {
		r.intents[h.meta.Name] = diskIntent{Op: "freezing", Checkout: h.meta.Name, Dir: h.meta.Dir, Layer: layerID, Snapshot: id}
		if err = r.persistIntentLocked(); err != nil {
			delete(r.snapshots, id)
			delete(r.lineage, id)
			delete(r.intents, h.meta.Name)
			return nil, err
		}
	}
	oldStack, oldFS, oldHead, oldCopied := h.stack, h.fs, copyString(h.meta.Head), h.meta.Copied
	renamed, refsChanged, closed := false, false, false
	rollback := func(cause error) (*snapRecord, error) {
		var rollbackErr error
		if refsChanged {
			r.releaseLocked(h.base, s.stack)
			r.releaseLocked(h.base, h.stack)
			r.retainLocked(h.base, oldStack)
			h.stack, h.fs, h.meta.Head = oldStack, oldFS, oldHead
		}
		if refsChanged || closed {
			// Open files reopen on their next call, in the restored upper.
			h.generation.Add(1)
		}
		if renamed {
			h.meta.Copied = oldCopied
			if h.meta.JJ != "" {
				rollbackErr = errors.Join(rollbackErr, moveWorkspace(h.upper, filepath.Join(r.config.State, "layers", layerID)))
			}
			if e := removeTree(h.upper); e != nil {
				rollbackErr = errors.Join(rollbackErr, e)
			}
			if e := os.Rename(filepath.Join(r.config.State, "layers", layerID), h.upper); e != nil {
				rollbackErr = errors.Join(rollbackErr, e)
			}
		}
		delete(r.layers, layerID)
		delete(r.snapshots, id)
		delete(r.lineage, id)
		delete(r.intents, h.meta.Name)
		// Reopen the restored upper to recreate staging and its in-memory index.
		if renamed {
			layers, e := r.layerStackLocked(oldStack, h.meta.Base)
			if e == nil {
				h.fs, e = overlay.New(h.upper, layers, h.base.view.Bottom(h.meta.Created), r.config.Memory)
			}
			rollbackErr = errors.Join(rollbackErr, e)
		}
		if rollbackErr == nil {
			rollbackErr = r.persistLocked()
		}
		return nil, errors.Join(cause, rollbackErr)
	}
	if layerID != "" {
		layerDir := filepath.Join(r.config.State, "layers", layerID)
		// Windows cannot rename the upper while a file in it is open; open
		// files move to the next generation's upper on their next call anyway.
		oldFS.CloseFiles()
		closed = true
		if err = os.Rename(h.upper, layerDir); err != nil {
			return rollback(err)
		}
		renamed = true
		if err = os.RemoveAll(filepath.Join(layerDir, ".wh..wh.work")); err != nil {
			return rollback(err)
		}
		layer, indexErr := oldFS.Freeze(layerDir)
		if indexErr != nil {
			return rollback(indexErr)
		}
		// The jj workspace stays the checkout's: it moves to the fresh upper.
		if e, ok := layer.Get(jj.Dir); ok && e.Kind != overlay.KindWhiteout && h.meta.JJ != "" {
			if err = os.MkdirAll(h.upper, 0o755); err == nil {
				err = layer.Extract(jj.Dir, filepath.Join(h.upper, jj.Dir))
			}
			if err != nil {
				return rollback(err)
			}
		}
		r.layers[layerID] = &layerRecord{id: layerID, base: h.meta.Base, layer: layer}
		layers, stackErr := r.layerStackLocked(stack, h.meta.Base)
		if stackErr != nil {
			return rollback(stackErr)
		}
		if err = os.MkdirAll(h.upper, 0o755); err != nil {
			return rollback(err)
		}
		if h.meta.Git {
			if err = writeGitfile(h.upper, r.gitdirPath(h.meta.dir())); err != nil {
				return rollback(err)
			}
		}
		fresh, freshErr := overlay.New(h.upper, layers, h.base.view.Bottom(h.meta.Created), r.config.Memory)
		if freshErr != nil {
			return rollback(freshErr)
		}
		// The copied-up directories went into the layer with the rest of the upper.
		h.fs, h.meta.Copied = fresh, nil
	}
	r.releaseLocked(h.base, oldStack)
	h.stack = stack
	r.retainLocked(h.base, stack)
	r.retainLocked(s.base, s.stack)
	refsChanged = true
	h.meta.Head = copyString(&id)
	h.generation.Add(1)
	delete(r.intents, h.meta.Name)
	if commit {
		if err = r.persistLocked(); err != nil {
			return rollback(err)
		}
	}
	r.hashes.Add(1)
	return s, nil
}

// Rm retires handles while frozen and deletes writable state and unused snapshots.
// The removal is published before the checkout's files go, outside the
// registry lock and with a state write shared with concurrent operations; a crash
// in between leaves files no checkout names, which reopening removes. The name
// stays reserved until they are gone.
func (r *Registry) Rm(ctx context.Context, args api.RmArgs) error {
	_, err := r.remove(ctx, args.Checkout, time.Time{}, args.StickySnapshots)
	return err
}

// remove is Rm, except that with picked set it keeps a checkout whose Touched is
// no longer picked, one changed since prune picked it, and reports false.
func (r *Registry) remove(ctx context.Context, name string, picked time.Time, stickySnapshots bool) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false, failure("io", "registry is closed")
	}
	h := r.checkouts[name]
	if h == nil {
		r.mu.Unlock()
		return false, failure("not_found", "no checkout %s", name)
	}
	r.operations.Add(1)
	defer r.operations.Done()
	r.mu.Unlock()
	if err := r.freezeAndLock(h); err != nil {
		return false, err
	}
	// The freeze lock waits out file calls in flight, so none touches it from here.
	if !picked.IsZero() && !r.touched(h).Equal(picked) {
		r.mu.Unlock()
		h.freeze.Unlock()
		return false, nil
	}
	h.retired.Store(true)
	if r.config.Retire != nil {
		r.config.Retire(name)
	}
	// Taken while the checkout still holds its base, which collection may close once released.
	repo := localRepo(h.base)
	h.fs.Discard()
	h.fs.CloseFiles()
	delete(r.checkouts, name)
	r.reserved[name] = true
	r.releaseLocked(h.base, h.stack)
	r.moveRootLocked()
	seq := r.requestCommitLocked()
	r.mu.Unlock()
	// Retired and gone from the registry, so no file call reaches h: release the
	// freeze before the file server's retire, which waits on clients.
	h.freeze.Unlock()
	err := r.commitState(seq)
	if err != nil {
		// The state on disk still names the checkout: record its removal for the
		// next commit, or for recovery at reopen should none succeed.
		r.mu.Lock()
		r.intents[name] = diskIntent{Op: "removing", Checkout: name, Dir: h.meta.Dir, Layer: h.meta.Base}
		r.mu.Unlock()
	} else if !stickySnapshots {
		err = r.dropCheckoutSnapshots(name)
	}
	removeErr := r.removeCheckoutFiles(h.meta.dir())
	r.mu.Lock()
	if removeErr == nil {
		delete(r.reserved, name)
	}
	r.mu.Unlock()
	r.syncRemotes(repo)
	if err = errors.Join(err, removeErr); err != nil {
		return true, coded(err)
	}
	r.emit(Event{Name: "removed", Payload: map[string]any{"name": name}})
	if r.config.Changed != nil {
		r.config.Changed(name, false)
	}
	return true, nil
}

// dropCheckoutSnapshots keeps snapshots still in another checkout or snapshot's ancestry.
func (r *Registry) dropCheckoutSnapshots(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	used := make(map[string]bool)
	visit := func(id string) {
		for id != "" && !used[id] {
			used[id] = true
			s := r.lineage[id]
			if s == nil {
				break
			}
			id = s.stackParent
		}
	}
	for _, h := range r.checkouts {
		visit(checkoutSnapshot(h.meta))
	}
	for id, s := range r.snapshots {
		if s.meta.Checkout != name {
			visit(id)
		}
	}

	var ids []string
	for id, s := range r.snapshots {
		if s.meta.Checkout == name && !used[id] {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return snapshotLess(ids[i], ids[j]) })
	for _, id := range ids {
		if err := r.dropLocked(id); err != nil {
			return err
		}
	}
	return nil
}

// Drop releases a snapshot and its retained stack.
func (r *Registry) Drop(ctx context.Context, args api.DropArgs) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return failure("io", "registry is closed")
	}
	return r.dropLocked(args.Snapshot)
}

// dropLocked drops one snapshot while the registry lock is held.
func (r *Registry) dropLocked(id string) error {
	s := r.snapshots[id]
	if s == nil {
		return failure("not_found", "no snapshot %s", id)
	}
	delete(r.snapshots, id)
	s.dropped = true
	if err := r.persistLocked(); err != nil {
		s.dropped = false
		r.snapshots[id] = s
		r.lineage[id] = s
		return coded(err)
	}
	r.releaseLocked(s.base, s.stack)
	if !s.hashing && !s.finished {
		s.finished = true
		s.hashErr = failure("not_found", "snapshot %s was dropped", id)
		close(s.done)
		r.hashes.Done()
	}
	if err := os.RemoveAll(filepath.Join(r.snapshotDir(id), "git")); err != nil {
		return coded(err)
	}
	r.scheduleHashesLocked()
	return nil
}

// ancestorTreeLocked finds the nearest hashed ancestor of a stack (following
// snapshot parents from start) whose layers are a suffix of the stack, and the
// newer layers on top of it, oldest first. ready is false while an ancestor hashes.
func (r *Registry) ancestorTreeLocked(b *baseRecord, stack []string, start *string) (parent store.OID, layers []*overlay.Layer, ready bool, err error) {
	parent = b.view.Meta().Tree
	parentStack := []string{}
	visited := make(map[string]bool)
	for ancestor := start; ancestor != nil; {
		if visited[*ancestor] {
			return "", nil, false, fmt.Errorf("snapshot parent cycle at %s", *ancestor)
		}
		visited[*ancestor] = true
		s := r.lineage[*ancestor]
		if s == nil {
			break
		}
		if !s.finished {
			return "", nil, false, nil
		}
		if s.hashErr == nil && s.meta.Tree != nil {
			parent, parentStack = store.OID(*s.meta.Tree), s.stack
			break
		}
		ancestor = s.meta.Parent
	}
	prefix := len(stack) - len(parentStack)
	if prefix < 0 || !sameStack(stack[prefix:], parentStack) {
		parent, prefix = b.view.Meta().Tree, len(stack)
	}
	for i := prefix - 1; i >= 0; i-- {
		l := r.layers[stack[i]]
		if l == nil {
			return "", nil, false, fmt.Errorf("missing layer %s", stack[i])
		}
		layers = append(layers, l.layer)
	}
	return parent, layers, true, nil
}

// scheduleHashesLocked starts only jobs whose nearest retained ancestor has resolved.
func (r *Registry) scheduleHashesLocked() {
	ids := make([]string, 0, len(r.snapshots))
	for id := range r.snapshots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return snapshotLess(ids[i], ids[j]) })
	for _, id := range ids {
		s := r.snapshots[id]
		if s.hashing || s.finished || !s.announced {
			continue
		}
		parent, layers, ready, err := r.ancestorTreeLocked(s.base, s.stack, s.meta.Parent)
		if err != nil {
			r.finishHashLocked(s, "", err)
			continue
		}
		if !ready {
			continue
		}
		meta := s.base.view.Meta()
		stack, err := r.layerStackLocked(s.stack, meta.ID)
		if err != nil {
			r.finishHashLocked(s, "", err)
			continue
		}
		view, err := overlay.New("", stack, s.base.view.Bottom(time.Time{}), nil)
		if err != nil {
			r.finishHashLocked(s, "", err)
			continue
		}
		s.hashing = true
		r.retainLocked(s.base, s.stack)
		job := hashJob{Lineage: meta.ID, Snapshot: id, Base: s.base, Parent: parent, Layers: layers, View: view}
		job.Done = func(tree store.OID, hashErr error) { r.hashDone(s, tree, hashErr) }
		r.hasher.queue(job)
	}
}

// sameStack compares top-to-bottom immutable layer identities.
func sameStack(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// finishHashLocked routes preparation failures through the durable job completion path.
func (r *Registry) finishHashLocked(s *snapRecord, tree store.OID, err error) {
	s.hashing = true
	r.retainLocked(s.base, s.stack)
	go r.hashDone(s, tree, err)
}

// hashDone publishes completed tree ids and releases active-job references.
func (r *Registry) hashDone(s *snapRecord, tree store.OID, hashErr error) {
	r.mu.Lock()
	if hashErr == nil && !s.dropped {
		id := string(tree)
		s.meta.Tree = &id
	}
	s.hashErr = hashErr
	// No durability barrier: reopening hashes a snapshot again when a crash lost its tree id.
	persistErr := r.persistIntentLocked()
	if persistErr != nil {
		s.meta.Tree = nil
		s.hashErr = errors.Join(hashErr, persistErr)
	}
	result := snapshotOf(s.meta)
	reportErr := s.hashErr
	dropped := s.dropped
	r.mu.Unlock()
	if !dropped && persistErr == nil {
		if reportErr == nil {
			r.emit(Event{Name: "hashed", Payload: map[string]any{"snapshot": result}})
		} else {
			r.emit(Event{Name: "hash_failed", Payload: map[string]any{"snapshot": result, "message": reportErr.Error()}})
		}
	}
	r.mu.Lock()
	s.finished = true
	close(s.done)
	r.releaseLocked(s.base, s.stack)
	r.scheduleHashesLocked()
	r.mu.Unlock()
	r.hashes.Done()
}
