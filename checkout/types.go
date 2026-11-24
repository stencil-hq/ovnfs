// Package checkout manages checkouts: isolated, copy-on-write copies of any folder that
// agents work in, plus their snapshots, forks and the way back to the source.
//
// A checkout is one overlay (upper → frozen layers → base). The base is a capture
// of the source folder classified by the checkout's rules (see package rules): only
// source-class paths are part of a snapshot's identity; cache and local paths
// are served copy-on-write from a snapshot of the folder's that ovnfs owns
// (see useCaches); hidden paths do not exist.
// A checkout of a git repository also gets a private git directory, so git works
// inside it without writing to the user's repository.
//
// Every method on Registry is one admin operation (package api) with the same
// name and argument struct as the CLI verb.
package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/stats"
	"github.com/stencil-hq/ovnfs/store"
	"github.com/stencil-hq/ovnfs/watch"
)

// Error is an admin-visible failure with a stable protocol code.
type Error struct{ Code, Message string }

// Error returns the underlying explanation without a protocol prefix.
func (e *Error) Error() string { return e.Message }

// ErrorCode returns the stable admin protocol error code.
func (e *Error) ErrorCode() string { return e.Code }

// checkoutMeta is the persisted description of a live checkout.
type checkoutMeta struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"` // stable identity of this checkout instance
	// Dir names the checkout's directories below uppers and git when they are not
	// named after the checkout: a checkout prepared before its name was known.
	Dir     string    `json:"dir,omitempty"`
	Base    string    `json:"base"`
	Parent  *string   `json:"parent"`       // snapshot the checkout was created from
	Head    *string   `json:"head"`         // the checkout's latest snapshot
	Git     bool      `json:"git"`          // the checkout has a private git directory
	JJ      string    `json:"jj,omitempty"` // its jj workspace's name, its .jj at the upper's root (see addWorkspace)
	Created time.Time `json:"created"`
	// MergeBases maps a merge source to a store tree of the leaves later merges
	// take as their paths' base in place of the capture's. Between the checkout
	// and its folder these are the versions the two last agreed on (see
	// folderBases); for pullSource of an attached lane, the incoming leaves a
	// pull left conflict markers for, so a conflict the user resolved is not
	// merged again.
	MergeBases map[string]string `json:"merge_bases,omitempty"`
	// Copied lists the directories of the upper that the checkout copied up to hold
	// submodule gitfiles, with the metadata they had then; while they keep it, they
	// are infrastructure, not changes (see upperState.empty).
	Copied map[string]copiedDir `json:"copied,omitempty"`
	// Slot numbers the checkout in the export's file ids (export.Nodes), which
	// clients keep as inode numbers, so it is persisted for ids that survive
	// restarts. Life tells this checkout from earlier holders of the same slot,
	// so a protocol handle of a removed checkout never resolves into a newer
	// one. Both are assigned when the checkout is registered (addCheckoutLocked).
	Slot uint16 `json:"slot,omitempty"`
	Life uint32 `json:"life,omitempty"`
	// Caches names the cache snapshot of its base (base.Caches) the checkout
	// reads cache and local roots from; "" on remote bases.
	Caches string `json:"caches,omitempty"`
}

// dir names the checkout's directories below uppers and git.
func (m checkoutMeta) dir() string {
	if m.Dir != "" {
		return m.Dir
	}
	return m.Name
}

// copiedDir is the metadata of a directory copied up to hold a submodule gitfile.
type copiedDir struct {
	Mode  os.FileMode `json:"mode"`
	Mtime time.Time   `json:"mtime"`
}

// snapMeta is the persisted description of a snapshot; Tree is set once hashed.
// Life identifies the checkout instance; zero means its owner is unknown.
type snapMeta struct {
	ID       string    `json:"id"`
	Checkout string    `json:"checkout"`
	Dir      string    `json:"dir,omitempty"` // the checkout's checkoutMeta.Dir
	Life     uint32    `json:"life,omitempty"`
	Parent   *string   `json:"parent"`
	Label    string    `json:"label"`
	Created  time.Time `json:"created"`
	Tree     *string   `json:"tree"`
}

// dir names the directories below uppers and git of the checkout snapshotted.
func (m snapMeta) dir() string {
	if m.Dir != "" {
		return m.Dir
	}
	return m.Checkout
}

// Event is emitted only after its corresponding registry state is durable.
type Event struct {
	Name    string
	Payload any
}

// Config connects persistent storage to the export and admin event stream.
type Config struct {
	State, Mount string
	UserHome     string // folder holding the user-wide ~/.o2/ovnfs.yml; "" ignores it
	// Node is this daemon's stable node id, by which remote machines
	// know its offers; "" means this registry cannot offer checkouts.
	Node string
	// Fetch asks the link currently connected for key (see LinkKey) for blobs a
	// remote checkout needs; nil means remote checkouts can read only what is cached.
	Fetch func(ctx context.Context, key string, wants []base.Want) error
	// Search asks the link currently connected for key to scan blobs a remote
	// checkout has not fetched on the machine holding them; nil makes grep in
	// remote checkouts fetch what it reads.
	Search func(ctx context.Context, key string, q grep.Query, wants []base.Want) ([]grep.Outcome, error)
	Emit   func(Event)
	// Retire tells the file servers that checkout name, which held export slot
	// (checkoutMeta.Slot), is going away, while the registry lock is held: it
	// must not call back into the registry.
	Retire  func(name string, slot uint16)
	Changed func(name string, added bool)
	// Memory keeps checkouts' small written files in memory until they spill
	// into their uppers (see overlay.Memory); nil writes every file to disk,
	// which survives a crash.
	Memory *overlay.Memory
	// BlobCache bounds the bytes of small store blobs served from memory; 0 disables it.
	BlobCache int64
	// Invalidate reports checkout paths the daemon changed behind the file
	// protocols (pull, reattach), and
	// trees changed anywhere at or below them ("" for the whole checkout), so
	// the export recalls clients' caches. It is called without registry locks
	// held.
	Invalidate func(checkout string, paths, trees []string)
	// Quiesce recalls client write caches and waits for their writes to drain.
	// Its release keeps new write-cache grants disabled through the operation.
	// Concurrent and nested calls must coexist, not serialize: Apply calls Snap.
	// It is called before registry/freeze locks; draining writes must still Enter.
	// Nil means the export never delegates writes to clients.
	Quiesce func(context.Context) (func(), error)
	// GitRemotes gives a source repository a git remote ovnfs/<name> for each
	// live checkout of it (see Registry.syncRemotes); off, ovnfs removes them.
	GitRemotes bool
	// Logf reports how long Open spent on each part of the state, so a slow
	// start names its cause; nil is silent.
	Logf func(format string, args ...any)
	// Stats records what the registry's background work and each checkout's
	// file calls cost (see package stats); nil records nothing.
	Stats *stats.Recorder
}

// Registry owns the live checkout namespace and all retained snapshot objects.
type Registry struct {
	mu           sync.RWMutex
	config       Config
	mounts       []string // config.Mount and its resolved spelling; see underMount
	store        *store.Store
	node         string
	stateLock    *os.File
	checkouts    map[string]*checkoutRecord
	snapshots    map[string]*snapRecord
	lineage      map[string]*snapRecord
	bases        map[string]*baseRecord
	layers       map[string]*layerRecord
	intents      map[string]diskIntent
	reserved     map[string]bool
	nextSnapshot uint64
	nextSlot     uint16                     // where the next slot search starts (see allocSlotLocked)
	nextLife     uint32                     // the last Life assigned
	bySlot       map[uint16]*checkoutRecord // registered checkouts by checkoutMeta.Slot
	proto        string
	port         int
	rootTime     atomic.Int64 // unix nanoseconds of the export root's mtime (see RootTime); written under mu
	closed       bool
	operations   sync.WaitGroup
	hashes       sync.WaitGroup
	hasher       *hasher
	gcWake       chan struct{}
	trashWake    chan chan struct{} // nil wakes the reaper; a non-nil channel is closed after that pass
	relieving    atomic.Bool        // a relieveTrash runs
	noticeWake   chan struct{}
	stop         chan struct{}
	workers      sync.WaitGroup
	remotesMu    sync.Mutex // serializes rewrites of source repositories' checkout remotes
	noticeMu     sync.Mutex
	notices      []baseNotice
	closeOnce    sync.Once
	closeErr     error
	watcher      *watch.Watcher // nil where folder events are unsupported
	watchWake    chan struct{}
	watchMu      sync.Mutex              // held while the watched folder set changes
	watched      map[string]bool         // watchMu: folders the watcher was given
	folders      map[string]*reuseFolder // r.mu: folders whose creates reuse captures, by source root
	rootsMu      sync.Mutex
	roots        map[string]string        // rootsMu: folders a create named, by their resolved source root
	resolving    map[string]chan struct{} // rootsMu: folders being resolved, closed when done
	applying     folderLocks              // source folders an apply is reading and writing
	mirror       *mirror                  // git upstream content of attached checkouts
	commits      stateCommits             // durable state commits requested outside r.mu
	writes       stateWrites              // orders state.json writes
	prepares     chan struct{}            // a slot per checkout being prepared on a warm capture
	cachesMu     sync.Mutex               // guards baseRecord.caches and deadCaches; taken after r.mu, never before
	deadCaches   []deadCaches             // cachesMu: cache snapshots no checkout uses, for collectGarbage
}

// checkoutRecord holds one freeze lock for all calls against a live folder.
type checkoutRecord struct {
	meta  checkoutMeta
	base  *baseRecord
	stack []string
	upper string
	fs    *overlay.FS
	// freeze is held shared by every file call (Session) and exclusively to
	// change the checkout under them. It is taken before Registry.mu, never
	// while holding it: a file call may be slow (a source folder on a network
	// share), and waiting for it under the registry lock would stall every
	// checkout instead of this one. For the same reason nothing waits on the
	// checkout's overlay under Registry.mu without holding freeze exclusively:
	// file calls keep the overlay busy too (see upperState.empty).
	freeze     sync.RWMutex
	generation atomic.Uint64
	retired    atomic.Bool
	merge      sync.Mutex   // serializes apply and pull, which rewrite meta.MergeBases; taken before freeze and Registry.mu
	git        gitCache     // stored files of the private git directory, for the next snapshot
	wrote      atomic.Int64 // unix nanoseconds of the last change through the checkout's folder (see Session.Touch); 0 for none
	caches     *base.Caches // the cache snapshot meta.Caches names; nil on remote bases and while prepared (see useCaches)
	calls      *stats.Timer // the file calls made while registered (see Session); nil before, and without Config.Stats
}

// snapRecord retains its full stack until explicitly dropped.
type snapRecord struct {
	meta        snapMeta
	base        *baseRecord
	stack       []string
	layer       string
	stackParent string
	git         store.OID // stored private git directory tree; "" for none or a legacy copy
	done        chan struct{}
	hashing     bool
	announced   bool
	finished    bool
	dropped     bool
	hashErr     error
}

// baseRecord counts checkout, snapshot and active hash references to a base, and
// holds the rules every checkout on it was opened with.
type baseRecord struct {
	view  *base.Base
	lock  manifest.Lock
	rules rules.Set
	refs  int
	// template is closed once the private git directory checkouts on the base
	// start from is written in the background (or failed); nil when it was
	// written with the capture, or the base keeps none.
	template chan struct{}
	caches   baseCaches // Registry.cachesMu: the base's cache snapshots in use
}

// layerRecord counts checkout, snapshot and active hash references to a layer.
type layerRecord struct {
	id, base string
	layer    *overlay.Layer
	refs     int
	portable bool
	embedded bool // state.json carries the layer's metadata in place of its manifest file
}

// baseNotice carries a base's fault out of its locks before touching registry state.
type baseNotice struct{ base, fault string }

// Session holds a checkout's freeze lock shared for exactly one filesystem call.
// Its caller owns the storage and must not copy it after passing it to Enter.
// FS and Gen are valid until Done. Reuse is allowed only after all calls using
// the session, including concurrent Done calls, have returned.
type Session struct {
	FS     *overlay.FS
	Gen    uint64
	record *checkoutRecord
	began  stats.Tick // when Enter returned, for the checkout's call timer
	done   atomic.Bool
}

// pin makes s a session of h, whose freeze lock the caller holds shared for
// it, and starts timing the call.
func (s *Session) pin(h *checkoutRecord) {
	s.FS = h.fs
	s.Gen = h.generation.Load()
	s.record = h
	s.began = h.calls.Start()
	s.done.Store(false)
}

// Touch records that the call changed the checkout, for [api.Checkout.Touched].
func (s *Session) Touch() { s.record.wrote.Store(time.Now().UnixNano()) }

// Done releases the freeze lock; repeated and concurrent calls are harmless.
// A nil or zero Session has nothing to release.
func (s *Session) Done() {
	if s != nil && s.record != nil && s.done.CompareAndSwap(false, true) {
		s.record.calls.Since(s.began)
		s.record.freeze.RUnlock()
	}
}

// coded converts filesystem and capture errors to the public error taxonomy.
func coded(err error) error {
	if err == nil {
		return nil
	}
	if public, ok := errors.AsType[*Error](err); ok {
		return public
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "unavailable", Message: err.Error()}
	}
	if errors.Is(err, os.ErrNotExist) {
		return &Error{Code: "not_found", Message: err.Error()}
	}
	return &Error{Code: "io", Message: err.Error()}
}

// failure formats a stable coded error.
func failure(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// checkContext avoids starting a mutation after its caller has cancelled it.
func checkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return coded(ctx.Err())
	default:
		return nil
	}
}

// copyString makes returned metadata independent of mutable registry records.
func copyString(s *string) *string {
	if s == nil {
		return nil
	}
	value := *s
	return &value
}

// deref returns a possibly absent string field's value.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
