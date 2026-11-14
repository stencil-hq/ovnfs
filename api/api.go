// Package api defines the admin operations shared by the daemon and the CLI.
//
// Every CLI verb is exactly one operation with the same name, and its flags
// map 1:1 onto the argument struct below: `ovnfs fork app --name b` sends
// `fork` with ForkArgs{Checkout: "app", Name: "b"} and prints a ForkReply.
package api

import "time"

// Ops lists every operation the daemon serves, in CLI help order.
var Ops = []string{"status", "ls", "new", "fork", "snap", "diff", "grep", "glob", "apply", "rm", "prune", "drop", "explain", "plan", "profile", "attach", "pull", "push", "share", "subscribe", "stop"}

// Features names optional request fields this daemon honors, so clients can rely on
// them without a new protocol generation: "new.base" (NewArgs.Base),
// "diff.binary" (DiffArgs.Binary), "ls.select" (LsArgs.Select),
// "rm.sticky_snapshots" and "prune.sticky_snapshots", and
// "stop.keep_mount" (StopArgs.KeepMount).
var Features = []string{"new.base", "diff.binary", "ls.select", "rm.sticky_snapshots", "prune.sticky_snapshots", "stop.keep_mount"}

// Checkout is an isolated, copy-on-write copy of a source folder that an agent works in.
type Checkout struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`             // folder to cd into
	Source  string    `json:"source"`           // folder the checkout was opened from
	Base    string    `json:"base"`             // frozen capture the checkout sits on
	Parent  string    `json:"parent,omitempty"` // snapshot the checkout was created from
	Git     bool      `json:"git"`              // a working .git exists inside the checkout
	JJ      string    `json:"jj,omitempty"`     // the checkout's jj workspace in its source's repository, when it is one
	Dirty   bool      `json:"dirty"`            // changes since the last snapshot
	Created time.Time `json:"created"`
	Touched time.Time `json:"touched"` // last change to its files or git state; Created until then
}

// Snapshot is an immutable state of a checkout; Tree is its content id once hashed.
type Snapshot struct {
	ID       string    `json:"id"`
	Checkout string    `json:"checkout"`
	Parent   string    `json:"parent,omitempty"`
	Label    string    `json:"label,omitempty"`
	Created  time.Time `json:"created"`
	Tree     string    `json:"tree,omitempty"`
}

// Rule is one `<class> <predicate>` line and where it came from.
type Rule struct {
	Class  string `json:"class"`
	Text   string `json:"text"`
	Origin string `json:"origin"` // "builtin:3", ".o2/ovnfs.yml:6", "flag"
}

// Status describes the running daemon.
type Status struct {
	Version        int     `json:"version"`
	PID            int     `json:"pid"`
	Uptime         float64 `json:"uptime_s"`
	Proto          string  `json:"proto"`
	Port           int     `json:"port"`
	Mount          string  `json:"mount"`
	State          string  `json:"state"`
	Socket         string  `json:"socket"` // admin socket clients dial for these operations
	Checkouts      int     `json:"checkouts"`
	Snapshots      int     `json:"snapshots"`
	Bases          int     `json:"bases"`
	Layers         int     `json:"layers"`
	Memory         Usage   `json:"memory"` // checkouts' written files held in memory before they spill to disk
	NodeIndexBytes int64   `json:"node_index_bytes"`
	NodeCacheError string  `json:"node_cache_error,omitempty"`
	NodeCache      Usage   `json:"node_cache"`  // estimated reconstructible node and listing bytes
	BlobCache      Usage   `json:"blob_cache"`  // content-addressed blobs served from memory
	HeapLive       int64   `json:"heap_live"`   // Go heap the last collection found reachable
	HeapInUse      int64   `json:"heap_in_use"` // Go heap spans holding objects: live, not yet collected garbage and fragmentation
	GoHeld         int64   `json:"go_held"`     // memory the Go runtime holds from the OS (heap, stacks, metadata), minus what it returned
	Footprint      int64   `json:"footprint"`   // process memory as the OS charges it (macOS phys_footprint, Linux RssAnon+VmSwap, Windows private bytes); 0 where unknown
}

// Usage is resident bytes against a limit. A zero Limit disables file/blob
// caching; the node cache still keeps the current directory.
type Usage struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

// Select picks checkouts for ls and prune: every criterion set must hold, and
// the zero Select picks every checkout.
type Select struct {
	// Dir (absolute) picks the checkouts of folders related to it: the folder
	// itself, a folder inside it, or the folder holding it. Inside the mount it
	// picks the checkouts sharing the source of the checkout it is in.
	Dir string `json:"dir,omitempty"`
	// Idle picks checkouts untouched (Checkout.Touched) for at least this many seconds.
	Idle float64 `json:"idle_s,omitempty"`
	// NoDiff picks checkouts without source changes since their base: an empty diff.
	NoDiff bool `json:"no_diff,omitempty"`
}

// LsArgs selects the checkouts ls lists.
type LsArgs struct {
	Select
}

// LsReply lists live checkouts and retained snapshots; under a selection, only
// the checkouts picked and their snapshots.
type LsReply struct {
	Checkouts []Checkout `json:"checkouts"`
	Snapshots []Snapshot `json:"snapshots"`
}

// NewArgs opens a checkout from a folder (any folder, repo or not), from a snapshot,
// or on an existing capture (Checkout.Base) as it was taken: its files and the git
// state checkouts opened on it started from, whatever the folder holds now.
type NewArgs struct {
	Dir      string   `json:"dir,omitempty"`      // absolute; exactly one of Dir, Snapshot and Base
	Snapshot string   `json:"snapshot,omitempty"` // snapshot id
	Base     string   `json:"base,omitempty"`     // capture id; a capture of a linked work tree may keep no git state to start from
	Rev      string   `json:"rev,omitempty"`      // commit or local branch; folder only
	Take     []string `json:"take,omitempty"`     // "SRC:PATH" overrides of the starting tree; SRC is "." (folder), "" (index) or a revision
	Name     string   `json:"name,omitempty"`     // default: derived from the folder name
	Rules    []string `json:"rules,omitempty"`    // extra rule lines, highest precedence
}

// Capture summarizes how a source folder was classified when a checkout was opened.
type Capture struct {
	Root        string   `json:"root"`         // source root (a repo's top level when inside one)
	Files       int      `json:"files"`        // source-class files in the snapshot
	Bytes       int64    `json:"bytes"`        // their total size
	Hashed      int      `json:"hashed"`       // files read and hashed (stat-cache misses)
	HashedBytes int64    `json:"hashed_bytes"` // bytes read for hashing
	Cache       int      `json:"cache"`        // cache-class roots
	Local       int      `json:"local"`        // local-class roots
	Hidden      int      `json:"hidden"`       // hidden-class roots
	Repos       []string `json:"repos"`        // git repo roots found ("" is the source root)
	Seconds     float64  `json:"seconds"`
}

// NewReply returns the created checkout. Capture describes the capture it sits
// on, which a create of an unchanged folder reuses from an earlier one.
type NewReply struct {
	Checkout Checkout `json:"checkout"`
	Capture  *Capture `json:"capture,omitempty"` // nil when opened from a snapshot
	Warnings []string `json:"warnings"`
}

// ForkArgs snapshots a checkout and opens a second checkout on that snapshot.
type ForkArgs struct {
	Checkout string `json:"checkout"`
	Name     string `json:"name,omitempty"` // default: <checkout>-<n>
	Label    string `json:"label,omitempty"`
}

// ForkReply returns the parent's new snapshot and the child checkout, with
// warnings about the child.
type ForkReply struct {
	Snapshot Snapshot `json:"snapshot"`
	Checkout Checkout `json:"checkout"`
	Warnings []string `json:"warnings"`
}

// SnapArgs freezes a checkout's changes into a snapshot.
type SnapArgs struct {
	Checkout string `json:"checkout"`
	Label    string `json:"label,omitempty"`
	Wait     bool   `json:"wait,omitempty"` // block until the tree id is known
}

// SnapReply returns the snapshot (Tree is empty unless Wait was set).
type SnapReply struct {
	Snapshot Snapshot `json:"snapshot"`
}

// DiffArgs compares a checkout's current state with a snapshot (default: its base).
// Binary makes Patch the patch git diff --binary writes, which git apply takes
// whole: git headers, full blob ids, modes and binary content, each side as git's
// end-of-line conversion stores it; a path under a filter, ident or encoding is
// refused with code "unsupported".
type DiffArgs struct {
	Checkout string `json:"checkout"`
	From     string `json:"from,omitempty"` // snapshot id; default: the checkout's base capture
	Patch    bool   `json:"patch,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
}

// Change is one source-class path that differs; Status is A, M, D or T (type change).
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// DiffReply lists changed source paths; Patch is a unified diff when requested,
// a git patch when Binary says so (a daemon that knows DiffArgs.Binary sets it).
type DiffReply struct {
	From    string   `json:"from"` // snapshot id or "base"
	Changes []Change `json:"changes"`
	Patch   string   `json:"patch,omitempty"`
	Binary  bool     `json:"binary,omitempty"`
}

// ApplyArgs merges a checkout's source changes back into the folder it came from.
type ApplyArgs struct {
	Checkout string `json:"checkout"`
	DryRun   bool   `json:"dry_run,omitempty"`
	Branch   string `json:"branch,omitempty"` // instead: fetch the checkout's commits into the source repo as this branch
}

// Conflict is a path apply or pull could not settle: left untouched, or with
// Markers holding git-style conflict markers for the user to resolve; the next
// apply or pull reports it again until they are gone.
type Conflict struct {
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Markers bool   `json:"markers,omitempty"`
}

// ApplyReply reports what apply wrote (or would write, for a dry run).
type ApplyReply struct {
	Source    string     `json:"source"`
	Applied   []Change   `json:"applied"`
	Merged    []string   `json:"merged"`    // paths changed on both sides and merged cleanly
	Conflicts []Conflict `json:"conflicts"` // written only with conflict markers
	Branch    string     `json:"branch,omitempty"`
	Commit    string     `json:"commit,omitempty"`
}

// RmArgs removes a checkout and its unused snapshots unless StickySnapshots is set.
type RmArgs struct {
	Checkout        string `json:"checkout"`
	StickySnapshots bool   `json:"sticky_snapshots,omitempty"`
}

// PruneArgs removes the checkouts Select picks and their unused snapshots unless
// StickySnapshots is set. It needs a criterion; a checkout touched after selection stays.
type PruneArgs struct {
	Select
	DryRun          bool `json:"dry_run,omitempty"`
	StickySnapshots bool `json:"sticky_snapshots,omitempty"`
}

// PruneReply lists the checkouts prune removed (or would remove, for a dry run).
type PruneReply struct {
	Removed []Checkout `json:"removed"`
}

// DropArgs releases a snapshot.
type DropArgs struct {
	Snapshot string `json:"snapshot"`
}

// ExplainArgs asks which class a path gets and which rule decided it.
// Exactly one of Dir and Checkout is set; Path is relative to it.
type ExplainArgs struct {
	Dir      string   `json:"dir,omitempty"`
	Checkout string   `json:"checkout,omitempty"`
	Path     string   `json:"path"`
	Rules    []string `json:"rules,omitempty"`
}

// ExplainReply names the deciding rule; At is the ancestor that decided when it is not Path.
type ExplainReply struct {
	Path   string `json:"path"`
	Class  string `json:"class"`
	Rule   Rule   `json:"rule"`
	At     string `json:"at"`
	Exists bool   `json:"exists"`
}

// AttachArgs makes a checkout (or a folder, captured now) appear on another machine.
// Names, sizes and modes are there at once; content moves on first read and
// stays cached there. The link is `ssh To RemoteBin link`, reconnected until
// the checkout is removed or this daemon stops.
type AttachArgs struct {
	Checkout  string `json:"checkout,omitempty"` // exactly one of Checkout and Dir
	Dir       string `json:"dir,omitempty"`
	To        string `json:"to"`                   // ssh destination, e.g. "devbox" or "me@host"
	Name      string `json:"name,omitempty"`       // checkout name on the remote machine
	RemoteBin string `json:"remote_bin,omitempty"` // ovnfs on the remote machine; default "ovnfs"
}

// AttachReply says where the checkout appeared.
type AttachReply struct {
	Checkout       string   `json:"checkout"` // the checkout on this machine that was offered
	Snapshot       Snapshot `json:"snapshot"`
	Host           string   `json:"host"`
	RemoteCheckout string   `json:"remote_checkout"`
	RemotePath     string   `json:"remote_path"`
}

// PullArgs merges an attached remote checkout's source changes into its local checkout.
type PullArgs struct {
	Checkout string `json:"checkout"`
	From     string `json:"from,omitempty"` // host passed to attach; optional with one attachment
	DryRun   bool   `json:"dry_run,omitempty"`
}

// PullReply reports the remote snapshot and its changes merged into the local checkout.
type PullReply struct {
	ApplyReply
	Snapshot Snapshot `json:"snapshot"`
}

// PushArgs has the machines checkouts are attached to fetch every blob they
// still read from this one over the link, so their checkouts keep working once
// it is gone. Empty fields match every attachment.
type PushArgs struct {
	Checkout string `json:"checkout,omitempty"` // the local checkout attached
	To       string `json:"to,omitempty"`       // host passed to attach
}

// PushReply reports each matching attachment's push, sorted by checkout and host.
// While it runs, subscribers receive its progress as "pushing" events, each one
// attachment's Pushed without Error.
type PushReply struct {
	Pushed []Pushed `json:"pushed"`
}

// Pushed is how far one attachment's push got: the distinct blobs its machine
// fetched and their offered sizes, out of those it lacked when the push started.
type Pushed struct {
	Checkout   string `json:"checkout"`
	To         string `json:"to"`
	Blobs      int64  `json:"blobs"`
	Bytes      int64  `json:"bytes"`
	TotalBlobs int64  `json:"total_blobs"`
	TotalBytes int64  `json:"total_bytes"`
	Error      string `json:"error,omitempty"` // set when the push failed; blobs fetched before stay
}

// ShareArgs serves one checkout over remote-fs on a Unix socket, for a VM's `home`
// device or a peer. Sharing again replaces the previous share of the checkout.
type ShareArgs struct {
	Checkout string `json:"checkout"`
	Writable bool   `json:"writable,omitempty"`
}

// ShareReply names the socket; it lives until the checkout is removed or the daemon stops.
type ShareReply struct {
	Socket   string `json:"socket"`
	Writable bool   `json:"writable"`
}

// PlanArgs computes what a launch elsewhere would transfer, rebuild and leave behind.
// Exactly one of Dir and Checkout is set.
type PlanArgs struct {
	Dir      string   `json:"dir,omitempty"`
	Checkout string   `json:"checkout,omitempty"`
	On       string   `json:"on"` // "cloud" or "peer"
	Rules    []string `json:"rules,omitempty"`
}

// PlanRepo is one git repo found under the source and how far it is pushed.
type PlanRepo struct {
	Path     string `json:"path"` // relative to the source root; "" is the root
	Origin   string `json:"origin,omitempty"`
	Head     string `json:"head,omitempty"`
	Upstream string `json:"upstream,omitempty"` // remote-tracking ref used as the fetch base
	Pushed   bool   `json:"pushed"`             // HEAD is contained in Upstream
}

// PlanSet counts paths of one kind; Paths lists at most 20 of them.
type PlanSet struct {
	Files int      `json:"files"`
	Bytes int64    `json:"bytes"`
	Paths []string `json:"paths,omitempty"`
}

// PlanCache is one cache root and how it is recreated elsewhere.
type PlanCache struct {
	Root    string   `json:"root"`
	Rebuild string   `json:"rebuild,omitempty"` // empty: starts empty
	Key     []string `json:"key,omitempty"`
	KeyHash string   `json:"key_hash,omitempty"`
}

// PlanReply splits a source by channel: origin, this machine, rebuilt, or staying here.
type PlanReply struct {
	Root     string      `json:"root"`
	On       string      `json:"on"`
	Lock     string      `json:"lock"` // hash of the resolved manifest
	Repos    []PlanRepo  `json:"repos"`
	Source   PlanSet     `json:"source"` // every source-class file
	Origin   PlanSet     `json:"origin"` // fetched from a git origin by id
	Here     PlanSet     `json:"here"`   // sent from this machine
	Cache    []PlanCache `json:"cache"`
	Local    PlanSet     `json:"local"`
	Hidden   PlanSet     `json:"hidden"`
	Warnings []string    `json:"warnings"`
}

// StopArgs ends the daemon. It unmounts first and fails, leaving the daemon
// running, when the mount is busy; the next stop then forces the unmount.
type StopArgs struct {
	// KeepMount leaves an NFS or SMB mount for the next daemon, which serves
	// the same port and file handles, so restart keeps open files and working
	// directories inside it. FUSE and WinFsp mounts end with the daemon anyway.
	KeepMount bool `json:"keep_mount,omitempty"`
}

// ProfileArgs captures one Go runtime profile from the running daemon.
type ProfileArgs struct {
	// Kind names the profile: "cpu" (default), "heap", "allocs", "goroutine",
	// "block", "mutex" or "threadcreate". cpu, block and mutex sample over a
	// window and report nothing accumulated outside it; the rest are snapshots.
	Kind string `json:"kind,omitempty"`
	// Seconds bounds a sampling profile's window; snapshots ignore it.
	Seconds float64 `json:"seconds,omitempty"`
}

// ProfileReply is a pprof profile: gzip-compressed protobuf, base64 so it fits a
// JSON reply. `go tool pprof` reads the decoded bytes.
type ProfileReply struct {
	Kind    string  `json:"kind"`    // the profile captured
	Seconds float64 `json:"seconds"` // sampling window; 0 for a snapshot
	Bytes   int     `json:"bytes"`   // decoded profile size
	Data    string  `json:"data"`    // base64 pprof bytes
}
