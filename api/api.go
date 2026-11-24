// Package api defines the admin operations shared by the daemon and the CLI.
//
// Every CLI verb is exactly one operation with the same name, and its flags
// map 1:1 onto the argument struct below: `ovnfs fork app --name b` sends
// `fork` with ForkArgs{Checkout: "app", Name: "b"} and prints a ForkReply.
package api

import (
	"encoding/json"
	"errors"
	"time"
)

// Ops lists every operation the daemon serves, in CLI help order.
var Ops = []string{"status", "ls", "new", "fork", "snap", "diff", "grep", "glob", "apply", "land", "pull", "rm", "prune", "drop", "explain", "plan", "profile", "stats", "attach", "push", "share", "subscribe", "stop"}

// Features names optional request fields this daemon honors, so clients can rely on
// them without a new protocol generation: "new.base" (NewArgs.Base),
// "diff.binary" (DiffArgs.Binary), "ls.select" (LsArgs.Select),
// "rm.sticky_snapshots" and "prune.sticky_snapshots",
// "stop.keep_mount" (StopArgs.KeepMount),
// "pull.folder" (a pull of a checkout without attachments merges its folder),
// "apply.atomic" (ApplyArgs.Atomic), "rm.keep_commits" (RmArgs.KeepCommits),
// "diff.paths" (DiffArgs.Paths), "diff.stat" (DiffArgs.Stat),
// "new.base_checkout" (NewArgs.Base takes a checkout's name),
// "glob.git_markers" (GlobArgs.GitMarkers), and "rm.if_id" (RmArgs.IfID).
var Features = []string{"new.base", "diff.binary", "ls.select", "rm.sticky_snapshots", "prune.sticky_snapshots", "stop.keep_mount",
	"pull.folder", "apply.atomic", "rm.keep_commits", "diff.paths", "diff.stat", "new.base_checkout", "glob.git_markers", "rm.if_id"}

// CheckoutID identifies one checkout instance, regardless of its name.
type CheckoutID string

// UnmarshalJSON rejects null and empty IDs so a supplied removal guard
// cannot be decoded as an omitted one.
func (id *CheckoutID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value == "" {
		return errors.New("checkout id must be a nonempty string")
	}
	*id = CheckoutID(value)
	return nil
}

// Checkout is an isolated, copy-on-write copy of a source folder that an agent works in.
type Checkout struct {
	Name    string     `json:"name"`
	ID      CheckoutID `json:"id,omitempty"`     // stable through daemon restarts
	Path    string     `json:"path"`             // folder to cd into
	Source  string     `json:"source"`           // folder the checkout was opened from
	Base    string     `json:"base"`             // frozen capture the checkout sits on
	Parent  string     `json:"parent,omitempty"` // snapshot the checkout was created from
	Git     bool       `json:"git"`              // a working .git exists inside the checkout
	JJ      string     `json:"jj,omitempty"`     // the checkout's jj workspace in its source's repository, when it is one
	Dirty   bool       `json:"dirty"`            // changes since the last snapshot
	Created time.Time  `json:"created"`
	Touched time.Time  `json:"touched"` // last change to its files or git state; Created until then
}

// Snapshot is an immutable state of a checkout; Tree is its content id once hashed.
type Snapshot struct {
	ID       string    `json:"id"`
	Checkout string    `json:"checkout"`
	Parent   string    `json:"parent,omitempty"`
	Label    string    `json:"label,omitempty"`
	Created  time.Time `json:"created"`
	Tree     string    `json:"tree,omitempty"`
	Orphan   bool      `json:"orphan,omitempty"` // ls found no live checkout known to own this snapshot
}

// Rule is one `<class> <predicate>` line and where it came from.
type Rule struct {
	Class  string `json:"class"`
	Text   string `json:"text"`
	Origin string `json:"origin"` // "builtin:3", ".o2/ovnfs.yml:6", "flag"
}

// Status describes the running daemon. Version is the admin protocol generation;
// BuildVersion is the daemon's module version, not the CLI's.
type Status struct {
	Version      int     `json:"version"`
	BuildVersion string  `json:"build_version,omitempty"`
	PID          int     `json:"pid"`
	Uptime       float64 `json:"uptime_s"`
	Proto        string  `json:"proto"`
	Port         int     `json:"port"`
	Mount        string  `json:"mount"`
	State        string  `json:"state"`
	Socket       string  `json:"socket"` // admin socket clients dial for these operations
	Checkouts    int     `json:"checkouts"`
	Snapshots    int     `json:"snapshots"`
	Bases        int     `json:"bases"`
	Layers       int     `json:"layers"`
	Memory       Usage   `json:"memory"`                  // checkouts' written files held in memory before they spill to disk
	MemoryParked int     `json:"memory_parked,omitempty"` // memory files whose write to disk failed, retried every second
	MemoryError  string  `json:"memory_error,omitempty"`  // the latest such failure while MemoryParked is not 0
	QueueParked  int     `json:"queue_parked,omitempty"`  // checkouts whose queued changes of the upper on disk failed, retried every second
	QueueError   string  `json:"queue_error,omitempty"`   // one such failure while QueueParked is not 0
	BlobCache    Usage   `json:"blob_cache"`              // content-addressed blobs served from memory
	HeapLive     int64   `json:"heap_live"`               // Go heap the last collection found reachable
	HeapInUse    int64   `json:"heap_in_use"`             // Go heap spans holding objects: live, not yet collected garbage and fragmentation
	GoHeld       int64   `json:"go_held"`                 // memory the Go runtime holds from the OS (heap, stacks, metadata), minus what it returned
	Footprint    int64   `json:"footprint"`               // process memory as the OS charges it (macOS phys_footprint, Linux RssAnon+VmSwap, Windows private bytes); 0 where unknown
	// Warnings name host settings that slow the mount down, each with its fix.
	Warnings []string `json:"warnings,omitempty"`
}

// Usage is resident bytes against a limit. A zero Limit disables the cache.
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
// state checkouts opened on it started from, whatever the folder holds now. Base
// also takes the name of a live checkout and means that checkout's capture: an
// untouched twin of it.
type NewArgs struct {
	Dir      string   `json:"dir,omitempty"`      // absolute; exactly one of Dir, Snapshot and Base
	Snapshot string   `json:"snapshot,omitempty"` // snapshot id
	Base     string   `json:"base,omitempty"`     // capture id or checkout name; a capture of a linked work tree may keep no git state to start from
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

// DiffArgs compares a checkout's current state with a snapshot (default: its
// base: the capture it opened on, with the folder's files as of its last pull
// in place of the paths that pull brought in).
// Patch asks for a git-format patch of text hunks, without blob ids, that names
// binary files instead of carrying them. Binary makes Patch the patch git diff
// --binary writes, which git apply takes whole: full blob ids, modes and binary
// content, each side as git's end-of-line conversion stores it; a path under a
// filter, ident or encoding is refused with code "unsupported".
type DiffArgs struct {
	Checkout string `json:"checkout"`
	From     string `json:"from,omitempty"` // snapshot id; default: the checkout's base
	Patch    bool   `json:"patch,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	// Paths keeps only these paths and what lies below them; each is relative
	// to the checkout's root. Empty keeps every path.
	Paths []string `json:"paths,omitempty"`
	Stat  bool     `json:"stat,omitempty"` // count each path's added and deleted lines
}

// Change is one source-class path that differs; Status is A, M, D or T (type change).
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// DiffStat counts the lines one changed path gained and lost; Binary content
// has no lines to count.
type DiffStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// DiffReply lists changed source paths; Patch is a git-format patch when
// requested, the one git diff --binary writes when Binary says so (a daemon
// that knows DiffArgs.Binary sets it); Stat has one entry per change when
// DiffArgs.Stat asked for it. Left counts the changes DiffArgs.Paths left out.
type DiffReply struct {
	From    string     `json:"from"` // snapshot id or "base"
	Changes []Change   `json:"changes"`
	Patch   string     `json:"patch,omitempty"`
	Binary  bool       `json:"binary,omitempty"`
	Stat    []DiffStat `json:"stat,omitempty"`
	Left    int        `json:"left,omitempty"`
}

// ApplyArgs merges a checkout's source changes back into the folder it came from.
type ApplyArgs struct {
	Checkout string `json:"checkout"`
	DryRun   bool   `json:"dry_run,omitempty"`
	Branch   string `json:"branch,omitempty"` // instead: fetch the checkout's commits into the source repo as this branch
	// Atomic writes nothing unless a first pass finds that every path merges
	// without a conflict; otherwise the reply lists the conflicts with Refused
	// set. It cannot be combined with Branch.
	Atomic bool `json:"atomic,omitempty"`
}

// Conflict is a path apply or pull could not settle: left untouched, or with
// Markers holding git-style conflict markers for the user to resolve; the next
// apply or pull reports it again until they are gone.
type Conflict struct {
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Markers bool   `json:"markers,omitempty"`
	Pending bool   `json:"pending,omitempty"` // the markers are an earlier merge's, not resolved yet
}

// ApplyReply reports what apply wrote (or would write, for a dry run).
type ApplyReply struct {
	Source    string     `json:"source"`
	Applied   []Change   `json:"applied"`
	Merged    []string   `json:"merged"`    // paths changed on both sides and merged cleanly
	Conflicts []Conflict `json:"conflicts"` // written only with conflict markers
	Branch    string     `json:"branch,omitempty"`
	Commit    string     `json:"commit,omitempty"`
	Refused   bool       `json:"refused,omitempty"` // an atomic merge found Conflicts and wrote nothing
	// Pullable reports, with Conflicts, that a pull can merge the folder's
	// files into the checkout instead, so they are resolved there: it cannot
	// for a checkout opened on a revision or with taken paths.
	Pullable bool `json:"pullable,omitempty"`
}

// LandArgs puts a checkout's commits on the branch checked out in its folder,
// as a fast-forward, and merges the checkout's files into the folder for the
// paths those commits changed, so the folder's uncommitted edits stay: what
// committing there would have left. The files merge as an atomic apply does,
// and the branch moves only once they have; a branch that is not an ancestor
// of the checkout's HEAD, staged changes on the commits' paths and a detached
// HEAD in the folder are refused with code "conflict" before anything is
// written.
type LandArgs struct {
	Checkout string `json:"checkout"`
	DryRun   bool   `json:"dry_run,omitempty"`
	// Markers merges the files even when some conflict, writing conflict
	// markers into the folder as an apply does, for the folder's uncommitted
	// edits to be reconciled there; the branch then stays where it is until
	// they are resolved and the checkout is landed again.
	Markers bool `json:"markers,omitempty"`
}

// LandReply reports the files land merged as ApplyReply does, with Branch
// moved from Old to Commit: the two are equal when the checkout held no commit
// to land, and Refused leaves the branch at Old. Left counts the paths the
// checkout changed outside those commits, which stay there for an apply.
type LandReply struct {
	ApplyReply
	Old  string `json:"old"`
	Left int    `json:"left,omitempty"`
}

// RmArgs removes a checkout and its unused snapshots unless StickySnapshots is
// set. KeepCommits refuses, with code "conflict", a checkout whose HEAD or
// branches hold commits made in it that nothing else keeps: no ref of its
// folder's repository reaches them, as after a land or an apply to a branch,
// and the checkout pushed them to no remote. The removal would lose them.
// IfID refuses, with code "conflict", if the named checkout has a different id.
type RmArgs struct {
	Checkout        string     `json:"checkout"`
	StickySnapshots bool       `json:"sticky_snapshots,omitempty"`
	KeepCommits     bool       `json:"keep_commits,omitempty"`
	IfID            CheckoutID `json:"if_id,omitempty"`
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

// PullArgs merges newer source changes into a checkout: those of its folder
// since the checkout opened or last pulled, for a checkout attached to no
// machine and without From, else those of the checkout attached on From.
// After a pull from the folder, the folder's files as pulled are the
// checkout's base for the paths it brought in: diff lists and apply merges
// only what the checkout changed beyond them.
type PullArgs struct {
	Checkout string `json:"checkout"`
	From     string `json:"from,omitempty"` // host passed to attach; optional with one attachment
	DryRun   bool   `json:"dry_run,omitempty"`
}

// PullReply reports the changes merged into the local checkout: ApplyReply's
// Source is the checkout, Snapshot the remote snapshot they came from (zero
// for a pull from the folder, which sets Folder instead).
type PullReply struct {
	ApplyReply
	Snapshot Snapshot `json:"snapshot"`
	Folder   string   `json:"folder,omitempty"`
	// Warnings say what a pull from the folder left for git: the checkout's
	// HEAD when the folder's has moved.
	Warnings []string `json:"warnings,omitempty"`
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
// device or a peer, or with Kernel to a VM's ovnfs guest kernel client
// (`vmon vmm --ovnfs SOCK`) on a socket of its own. A checkout can have one
// share of each kind; sharing again replaces the previous share of that kind.
type ShareArgs struct {
	Checkout string `json:"checkout"`
	Writable bool   `json:"writable,omitempty"`
	Kernel   bool   `json:"kernel,omitempty"`
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

// StatsArgs selects the period stats totals.
type StatsArgs struct {
	Since float64 `json:"since_s,omitempty"` // seconds back from now; an hour when 0
}

// StatsReply totals what the daemon spent a period on, from the record it
// keeps below STATE/stats (package stats): the process's CPU time, its
// background jobs, and the repeated work it timed.
type StatsReply struct {
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Recorded float64      `json:"recorded_s"` // seconds of the period a daemon ran and recorded
	CPU      StatsCPU     `json:"cpu"`
	Jobs     []StatsJob   `json:"jobs"`    // finished background work by kind, longest total first
	Running  []StatsRun   `json:"running"` // background work in progress, longest first
	Timers   []StatsTimer `json:"timers"`  // repeated work by group, then longest total first
}

// StatsCPU is the daemon's processor time over a period.
type StatsCPU struct {
	User   float64   `json:"user_s"`
	Sys    float64   `json:"sys_s"`   // in the kernel: file system calls, mostly
	Peak   float64   `json:"peak"`    // cores busy over the busiest recorded interval (a minute)
	PeakAt time.Time `json:"peak_at"` // when that interval ended; zero without one
}

// StatsJob totals the finished runs of one kind of background work, such as
// "snapshot.take" or "trash.empty".
type StatsJob struct {
	Name           string  `json:"name"`
	Runs           int     `json:"runs"`
	Seconds        float64 `json:"seconds"`         // from start to end, waiting included
	CPU            float64 `json:"cpu_s,omitempty"` // processor time, for kinds that measure it
	Longest        string  `json:"longest"`         // what the longest run worked on
	LongestSeconds float64 `json:"longest_s"`
}

// StatsRun is background work in progress.
type StatsRun struct {
	Name    string  `json:"name"`
	Subject string  `json:"subject"` // what it works on
	Seconds float64 `json:"seconds"` // so far
}

// StatsTimer totals one kind of repeated work: group "calls" is a checkout's
// file calls (Name), "nfs" an NFS procedure, "admin" an admin operation.
type StatsTimer struct {
	Group   string  `json:"group"`
	Name    string  `json:"name"`
	Count   int64   `json:"count"`
	Seconds float64 `json:"seconds"`
}
