// Package base provides persistent, fork-time views of a captured source folder.
//
// A base is the bottom of every checkout opened from one capture. Source-class
// paths come from the capture's tree: from the live folder while unchanged
// since the fork time (cloned on first exposure), else from the store or the
// folder's git objects. Cache- and local-class roots are the live folder,
// read-only and as it is now: nothing is copied or frozen, and a checkout's writes
// copy up into its own upper. Hidden roots and source paths that appeared
// after the fork do not exist in the view.
package base

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// Meta describes a capture.
type Meta struct {
	ID       string    `json:"id"`
	Dir      string    `json:"dir"` // "" for a remote base
	ForkTime time.Time `json:"fork_time"`
	Tree     store.OID `json:"tree"`
	ReuseKey string    `json:"reuse_key,omitempty"` // ordinary capture identity; empty disables reuse
	Remote   *Remote   `json:"remote,omitempty"`
}

// Remote marks a base attached from another machine: there is no local folder,
// and content is fetched from that machine on first read.
type Remote struct {
	Node   string `json:"node"`   // the source daemon's id
	Source string `json:"source"` // display, e.g. "laptop:/src/app"
	// Origins are commits on the source's git upstreams; content they hold is
	// read from a local mirror of them (Config.Mirror) instead of the link.
	Origins []git.Origin `json:"origins,omitempty"`
}

// Want is a blob a remote base needs before it can serve a read.
type Want struct {
	ID   store.OID
	Path string
	Size int64
}

// Fetcher makes wanted blobs present in the store, from the machine a remote base came from.
type Fetcher func(ctx context.Context, wants []Want) error

// Mirror holds blobs fetched from remote bases' git origins (Remote.Origins) on
// this machine. A remote base reads what it holds locally; the link serves the rest.
type Mirror interface {
	Has(id store.OID) bool
	Blob(id store.OID) ([]byte, error)
}

// Config supplies storage and event sinks.
type Config struct {
	ID, StateDir string
	Store        *store.Store
	Fetch        Fetcher // remote bases only
	Mirror       Mirror  // remote bases only; nil reads everything through Fetch
	OnFault      func(msg string)
}

// Base serves one capture of a folder to every checkout opened on it.
type Base struct {
	config    Config
	meta      Meta
	index     *captureIndex // every captured path; see captureIndexFile
	stats     api.Capture
	domains   *source.Domains
	objectsMu sync.Mutex
	objects   map[string]*git.Objects // object readers by work tree top
	proofs    dirProofs
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
	remote    *remoteState
}

// Create marks the fork time and captures o.Root (with c.Store and the base's
// own state for index copies) before exposing the view.
func Create(ctx context.Context, c Config, o source.Options) (*Base, []string, error) {
	return CreateRevision(ctx, c, o, nil)
}

// CreateRevision captures a folder but, when revision is set, serves the
// revision's tracked tree and index instead. Only metadata changes: unchanged
// paths still read the folder and other tracked content comes from git lazily.
// Ordered takes replace only source paths and leave the captured index's
// entries unchanged; a revision index carries stat data only where content is
// still REV's.
func CreateRevision(ctx context.Context, c Config, o source.Options, revision *git.Revision, takes ...Take) (*Base, []string, error) {
	fork := time.Now().UTC()
	res, warnings, err := Capture(ctx, c, o)
	if err != nil {
		return nil, nil, err
	}
	work := res
	if revision != nil {
		if err := reviseCapture(ctx, c, &res, revision); err != nil {
			return nil, nil, err
		}
	}
	if err := takeCapture(ctx, c, o.Rules, &res, work, takes); err != nil {
		return nil, nil, err
	}
	if revision != nil {
		if err := seedRevisionIndex(ctx, c, res, revision, fork); err != nil {
			return nil, nil, err
		}
	}
	key := ""
	if revision == nil && len(takes) == 0 {
		key = res.ReuseKey()
	}
	b, err := CreateCaptured(c, res, fork, key)
	return b, warnings, err
}

// Capture prepares a capture and private index copies without persisting capture metadata.
func Capture(ctx context.Context, c Config, o source.Options) (source.Result, []string, error) {
	if c.Store == nil {
		return source.Result{}, nil, errors.New("base: missing store")
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return source.Result{}, nil, err
	}
	lowerDev, err := volumeID(o.Root)
	if err != nil {
		return source.Result{}, nil, err
	}
	stateDev, err := volumeID(c.StateDir)
	if err != nil {
		return source.Result{}, nil, err
	}
	var warnings []string
	if lowerDev != stateDev {
		warnings = append(warnings, "source is on another volume than the state; pins and stored blobs are byte copies instead of clones")
	}
	o.Store, o.State = c.Store, c.StateDir
	res, err := source.Capture(ctx, o)
	return res, append(warnings, res.Warnings...), err
}

// CreateCaptured persists a prepared capture; an empty reuseKey prevents later reuse.
func CreateCaptured(c Config, res source.Result, fork time.Time, reuseKey string) (*Base, error) {
	if c.Store == nil {
		return nil, errors.New("base: missing store")
	}
	m := Meta{ID: c.ID, Dir: res.Root, ForkTime: fork, Tree: res.Tree, ReuseKey: reuseKey}
	return create(c, m, &res)
}

// Open serves a base from its capture index, migrating the capture file of a
// base created before the index into one first.
func Open(c Config, m Meta) (*Base, error) {
	if c.Store == nil {
		return nil, errors.New("base: missing store")
	}
	x, err := openCaptureIndex(c.StateDir, m.Tree)
	if errors.Is(err, os.ErrNotExist) {
		return migrate(c, m)
	}
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	// A migration that crashed after publishing the index may have left these.
	removeLegacyCaptures(c.StateDir)
	domains, err := source.NewDomains(m.Dir, available(c, m, x.repos), x.trackedOf)
	if err != nil {
		return nil, err
	}
	return newBase(c, m, x, api.Capture{}, domains), nil
}

// migrate writes the capture index of a base created before it, from its
// capture file, its tree and its repositories' index copies, then removes the
// capture file.
func migrate(c Config, m Meta) (*Base, error) {
	res, err := loadCapture(c.StateDir)
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	if res.Files, err = c.Store.Flatten(m.Tree); err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	b, err := create(c, m, res)
	if err != nil {
		return nil, err
	}
	removeLegacyCaptures(c.StateDir)
	return b, nil
}

// create publishes the capture index of res and serves the base from it. A
// capture just taken brings the domains it read; one loaded from disk reads
// them again. Either way their indexes are released once the capture index
// holds the tracked paths.
func create(c Config, m Meta, res *source.Result) (*Base, error) {
	if res.Domains == nil {
		domains, err := source.NewDomains(m.Dir, available(c, m, res.Repos), nil)
		if err != nil {
			return nil, err
		}
		res.Domains = domains
	}
	if err := writeCaptureIndex(c.StateDir, res); err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	x, err := openCaptureIndex(c.StateDir, m.Tree)
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	res.Domains.Release(x.trackedOf)
	return newBase(c, m, x, res.Stats, res.Domains), nil
}

// available lists the repositories still present in the folder. One that
// vanished since capture only loses its facts and objects; the base stays
// readable from pins and the store.
func available(c Config, m Meta, repos []source.Repo) []source.Repo {
	var out []source.Repo
	for _, r := range repos {
		if _, err := git.Open(filepath.Join(m.Dir, filepath.FromSlash(r.Path))); err == nil {
			out = append(out, r)
		} else if c.OnFault != nil {
			c.OnFault(fmt.Sprintf("base %s: repository %q unavailable: %v", m.ID, r.Path, err))
		}
	}
	return out
}

// newBase serves an open capture index.
func newBase(c Config, m Meta, x *captureIndex, stats api.Capture, domains *source.Domains) *Base {
	m.ForkTime = m.ForkTime.UTC()
	b := &Base{config: c, meta: m, index: x, stats: stats, domains: domains, objects: make(map[string]*git.Objects)}
	if m.Remote != nil {
		b.remote = &remoteState{b: b, prefetched: map[string]bool{}}
	}
	return b
}

// Meta returns an independent snapshot of the base's persisted metadata.
func (b *Base) Meta() Meta {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.meta
	if m.Remote != nil {
		r := *m.Remote
		r.Origins = slices.Clone(r.Origins)
		m.Remote = &r
	}
	return m
}

// Repos lists the repositories of the captured folder; callers must not modify it.
func (b *Base) Repos() []source.Repo { return b.index.repos }

// Stats summarizes the capture that created the base; zero once reopened from disk.
func (b *Base) Stats() api.Capture { return b.stats }

// Leaf returns the captured leaf at p: a blob, symlink or empty directory.
func (b *Base) Leaf(p string) (store.File, bool) {
	if n, ok := b.index.lookup(p); ok {
		return b.index.leaf(n)
	}
	return store.File{}, false
}

// Info returns the stat data the capture recorded for p, a leaf or a non-source root.
func (b *Base) Info(p string) (source.Info, bool) {
	if n, ok := b.index.lookup(p); ok {
		return b.index.info(n)
	}
	return source.Info{}, false
}

// Class returns the class of p when the capture recorded it as a non-source root.
func (b *Base) Class(p string) (rules.Class, bool) {
	if n, ok := b.index.lookup(p); ok {
		class := b.index.class(n)
		return class, class != ""
	}
	return "", false
}

// Roots returns a new map of the capture's non-source roots and their classes.
func (b *Base) Roots() map[string]rules.Class {
	roots := map[string]rules.Class{}
	b.index.eachRoot(func(p string, class rules.Class) { roots[p] = class })
	return roots
}

// Domains answers rule facts for paths of the captured folder.
func (b *Base) Domains() *source.Domains { return b.domains }

// Store returns the object store holding the capture's blobs.
func (b *Base) Store() *store.Store { return b.config.Store }

// Blob reads a captured blob: from the store, else from the git objects of the
// repository holding p, else (remote bases) from the source machine.
func (b *Base) Blob(p string, id store.OID) ([]byte, error) {
	if b.config.Store.Has(id) {
		return b.config.Store.ReadBlob(id)
	}
	if b.remote != nil {
		if err := b.remote.ensure(p, id); err != nil {
			return nil, err
		}
		return b.config.Store.ReadBlob(id)
	}
	repo := b.domains.RepoOf(p)
	top := ""
	origin, overridden := "", false
	if n, ok := b.index.lookup(p); ok {
		origin, overridden = b.index.origin(n)
	}
	if overridden {
		top = filepath.Join(b.meta.Dir, filepath.FromSlash(origin))
	} else if repo != nil {
		top = repo.Top
	} else {
		return nil, fmt.Errorf("content of %s (%s) is no longer available", p, id)
	}
	b.objectsMu.Lock()
	defer b.objectsMu.Unlock()
	objects := b.objects[top]
	if objects == nil {
		var err error
		if repo == nil || repo.Top != top {
			if repo, err = git.Open(top); err != nil {
				return nil, err
			}
		}
		if objects, err = repo.Objects(); err != nil {
			return nil, err
		}
		b.objects[top] = objects
	}
	return objects.Blob(string(id))
}

// Bottom exposes the immutable base through the overlay read-only interface,
// for a checkout opened at opened: its cache paths leave out what the folder
// changed since (see [changedAfter]). A zero opened fences nothing, for views
// that never read cache paths (snapshot hashing).
func (b *Base) Bottom(opened time.Time) overlay.Bottom {
	if b.remote != nil {
		return remoteBottom{b}
	}
	v := bottom{b: b}
	if !opened.IsZero() {
		v.fence = opened.UnixNano()
	}
	return v
}

// Close commits pending pin metadata and stops object readers and the
// group-commit timer. The capture index stays mapped until the base is
// unreachable, so a lookup racing Close (or Remove) reads the capture as it
// was, never unmapped memory.
func (b *Base) Close() error {
	b.closeOnce.Do(func() {
		var errs []error
		b.objectsMu.Lock()
		for _, objects := range b.objects {
			errs = append(errs, objects.Close())
		}
		b.objectsMu.Unlock()
		b.closeErr = errors.Join(errs...)
	})
	return b.closeErr
}

// Remove closes the base and removes its private storage. Where a mapped
// file cannot be deleted (Windows), the directory goes once the capture index
// is unmapped.
func (b *Base) Remove() error {
	err := b.Close()
	dir := b.config.StateDir
	b.index.m.orphan.Store(&dir)
	removeErr := os.RemoveAll(dir)
	if removeErr != nil {
		if left, _ := os.ReadDir(dir); len(left) == 1 && left[0].Name() == captureIndexFile {
			removeErr = nil
		}
	}
	if err != nil {
		return err
	}
	return removeErr
}

// loadCapture reads the capture file of a base created before the capture
// index: the binary one, else the JSON one of bases older still.
func loadCapture(stateDir string) (*source.Result, error) {
	var res source.Result
	data, err := os.ReadFile(filepath.Join(stateDir, legacyCaptureFile))
	if errors.Is(err, os.ErrNotExist) {
		data, err := os.ReadFile(filepath.Join(stateDir, jsonCaptureFile))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("%s: %w", jsonCaptureFile, err)
		}
		return &res, nil
	}
	if err != nil {
		return nil, err
	}
	if err := res.UnmarshalBinary(data); err != nil {
		return nil, fmt.Errorf("%s: %w", legacyCaptureFile, err)
	}
	return &res, nil
}

// removeLegacyCaptures deletes the capture files a published capture index replaced.
func removeLegacyCaptures(stateDir string) {
	for _, name := range []string{legacyCaptureFile, jsonCaptureFile} {
		os.Remove(filepath.Join(stateDir, name))
	}
}
