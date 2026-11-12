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
	"path"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/durable"
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

// captureFile persists the classified capture next to the base's pins
// (source.Result.MarshalBinary). Bases created before it hold legacyCaptureFile,
// the same fields as JSON, and keep loading from it.
const (
	captureFile       = "capture.bin"
	legacyCaptureFile = "capture.json"
)

// entry is a captured blob, symlink or directory.
type entry struct {
	mode os.FileMode
	oid  store.OID
}

// Base serves one capture of a folder to every checkout opened on it.
type Base struct {
	config    Config
	meta      Meta
	capture   *source.Result
	domains   *source.Domains
	sources   map[string]entry
	children  map[string][]string // sorted root-relative paths, borrowed from sources
	classes   map[string]rules.Class
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
	if err := saveCapture(c.StateDir, &res); err != nil {
		return nil, err
	}
	m := Meta{ID: c.ID, Dir: res.Root, ForkTime: fork, Tree: res.Tree, ReuseKey: reuseKey}
	return load(c, m, &res)
}

// Open rebuilds the captured-tree index while retaining every existing pin.
func Open(c Config, m Meta) (*Base, error) {
	if c.Store == nil {
		return nil, errors.New("base: missing store")
	}
	res, err := loadCapture(c.StateDir)
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	files, err := c.Store.Flatten(m.Tree)
	if err != nil {
		return nil, fmt.Errorf("base %s: %w", m.ID, err)
	}
	res.Files = files
	return load(c, m, res)
}

// load indexes the capture's leaves and their implied directories. A capture
// just taken brings the domains it read; one loaded from disk reads them again.
func load(c Config, m Meta, res *source.Result) (*Base, error) {
	m.ForkTime = m.ForkTime.UTC()
	domains := res.Domains
	if domains == nil {
		// A repository that vanished since capture only loses its facts and objects;
		// the base stays readable from pins and the store.
		var repos []source.Repo
		for _, r := range res.Repos {
			if _, err := git.Open(filepath.Join(m.Dir, filepath.FromSlash(r.Path))); err == nil {
				repos = append(repos, r)
			} else if c.OnFault != nil {
				c.OnFault(fmt.Sprintf("base %s: repository %q unavailable: %v", m.ID, r.Path, err))
			}
		}
		var err error
		if domains, err = source.NewDomains(m.Dir, repos); err != nil {
			return nil, err
		}
		res.Domains = domains
	}
	b := &Base{config: c, meta: m, capture: res, domains: domains, sources: map[string]entry{"": {mode: os.ModeDir | 0o755}}, children: make(map[string][]string), classes: res.Roots, objects: make(map[string]*git.Objects)}
	if b.classes == nil {
		b.classes = map[string]rules.Class{}
	}
	for p, f := range res.Files {
		e := entry{oid: f.ID}
		switch f.Mode {
		case store.ModeDir:
			e.mode = os.ModeDir | 0o755
		case store.ModeSymlink:
			e.mode = os.ModeSymlink | 0o777
		case store.ModeExec:
			e.mode = 0o755
		default:
			e.mode = 0o644
		}
		b.sources[p] = e
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if _, ok := b.sources[dir]; !ok {
				b.sources[dir] = entry{mode: os.ModeDir | 0o755}
			}
		}
	}
	for p := range b.sources {
		if p != "" {
			parent := path.Dir(p)
			if parent == "." {
				parent = ""
			}
			b.children[parent] = append(b.children[parent], p)
		}
	}
	if m.Remote != nil {
		b.remote = newRemoteState(b)
	}
	for p := range b.children {
		sort.Strings(b.children[p])
	}
	return b, nil
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

// Capture returns the classified capture the base serves; callers must not modify it.
func (b *Base) Capture() *source.Result { return b.capture }

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
	if origin, overridden := b.capture.BlobRepo[p]; overridden {
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

// Bottom exposes the immutable base through the overlay read-only interface.
func (b *Base) Bottom() overlay.Bottom {
	if b.remote != nil {
		return remoteBottom{b}
	}
	return bottom{b}
}

// Close commits pending pin metadata and stops object readers and the group-commit timer.
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

// Remove closes the base and removes its private storage.
func (b *Base) Remove() error {
	err := b.Close()
	removeErr := os.RemoveAll(b.config.StateDir)
	if err != nil {
		return err
	}
	return removeErr
}

// saveCapture persists the capture a base serves in its state directory.
func saveCapture(stateDir string, res *source.Result) error {
	data, err := res.MarshalBinary()
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(stateDir, captureFile), data)
}

// loadCapture reads the persisted capture of a base, from the JSON file of a
// base created before the binary format when that is all it has.
func loadCapture(stateDir string) (*source.Result, error) {
	var res source.Result
	data, err := os.ReadFile(filepath.Join(stateDir, captureFile))
	if errors.Is(err, os.ErrNotExist) {
		data, err := os.ReadFile(filepath.Join(stateDir, legacyCaptureFile))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &res); err != nil {
			return nil, fmt.Errorf("%s: %w", legacyCaptureFile, err)
		}
		return &res, nil
	}
	if err != nil {
		return nil, err
	}
	if err := res.UnmarshalBinary(data); err != nil {
		return nil, fmt.Errorf("%s: %w", captureFile, err)
	}
	return &res, nil
}

// writeFile replaces metadata atomically and syncs it before publication.
func writeFile(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), ".meta-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = durable.Sync(f)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), p); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(p))
}
