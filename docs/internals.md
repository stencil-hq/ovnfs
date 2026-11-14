# Internals

## Admin protocol

Every CLI verb is one admin operation with the same name and argument shape
([`api/api.go`](../api/api.go)); programs embed ovnfs through the same
operations. `ovnfs --json start` makes sure the daemon runs (concurrent starts
share one) and prints its status, whose `socket` is the admin socket.

On it, frames are JSON objects behind a little-endian u32 length:

1. the client sends `{"hello": {"client": "…"}}`;
2. the daemon replies `{"welcome": {"version": 5, "ops": […], "features": […], …}}`,
   where `features` names optional request fields it honors (`api.Features`:
   `new.base`, `diff.binary`, `ls.select`, `stop.keep_mount`);
3. each request is `{"id": N, "<op>": {args}}`, answered by
   `{"id": N, "ok": reply}` or `{"id": N, "error": {"code", "message"}}`.

To cancel one outstanding request, send `{"cancel": N}` on the same
connection. This control frame has no acknowledgment of its own; the original
request returns error code `canceled` once its handler stops. Completion may
race with cancellation. Unknown or completed IDs are ignored, and request IDs
must be unique while active. Other requests and subscriptions remain live;
disconnecting cancels every active request on that connection.

`admin.Client.Call` sends cancellation when its context is canceled or its
deadline expires, returning that context's error. Cancellation is cooperative:
it does not roll back completed mutations or interrupt an I/O call, lock
acquisition or individual regexp operation already in progress. Search
workers finish cleanup before returning, closing files, unmapping buffers and
releasing the checkout's shared freeze lock.

`ovnfs ctl OP 'JSON'` sends one raw request; `--json` makes any verb print the
raw reply, `restart` the new daemon's status and `version` its build
(`{"version", "go", "os", "arch"}`); `run` keeps stdout for its command. Only
the socket owner's uid (on Windows, the same user SID) may
connect, on this socket and on the link socket.

`grep` and `glob` use the shapes in [`api/search.go`](../api/search.go).
For example, `ovnfs ctl grep '{"checkout":"app","pattern":"TODO","glob":"*.go"}'`
searches the live merged checkout without reading through the mount. A `dir`
under the configured mount is resolved to the same direct overlay access;
other absolute directories use native host I/O. Searches hold the checkout's
shared freeze lock until their workers finish, so snapshot/removal waits for
an active search; ordinary writes can continue. That wait never holds the
registry lock, so a slow file call or search stalls only its own checkout.

Searches through the daemon skip AppleDouble `._` sidecars, which the mount
does not list.

`profile` captures one Go runtime profile from the daemon (`cpu`, `heap`,
`allocs`, `goroutine`, `block`, `mutex` or `threadcreate`) and returns it as
base64 pprof bytes in its reply; `cpu`, `block` and `mutex` sample over a window
(`--seconds`, default 30), the rest are snapshots. `ovnfs profile cpu -o cpu.pb.gz`
writes the decoded bytes, which `go tool pprof cpu.pb.gz` reads.

Frames are limited to 16 MiB. An oversized reply returns error code
`too_large` without closing the connection; search clients can reduce their
result count, file filters or reported line width.

When omp finds the ovnfs binary, it tells its agents to open ovnfs checkouts
instead of git worktrees or copies, and uses this protocol to give
each isolated subagent its own checkout (`new` when it starts, `rm` when its
changes are merged back). The checkout's capture is the state the subagent
started from, so omp takes no baseline of it: the subagent's patch is
`diff {binary}`, and a branch commit reads its baseline from a second checkout
on the same capture (`new {base}`). A capture holding nested repositories or
submodules, or a daemon without those features, gets omp's own baseline
instead. Its `grep` and `glob` tools learn the daemon's mount
and admin socket from `ovnfs --json status` (nothing is read from the mount)
and send `grep`/`glob` to the daemon for search paths below that mount,
falling back to reading through the mount when it refuses.

## Persistence

State is `state.json` (version 5), atomically replaced with one durable write
per operation (`snap`, `fork` and `new --from` each sync once); multi-step
operations first write an intent that restart rolls forward or back. A
snapshot's tree id is written without a sync: a restart that lost it hashes the
snapshot again. Layer stacks are written only where they cannot be rebuilt from
a snapshot's parent. Builds that read only version 4 would cut store packs at
frame kinds they do not know (chunks, chunk manifests, tree deltas), so version
5 makes them refuse the state before they open the store; this build reads
version 4 and rewrites it as 5 when it opens. State of any other version is
refused. File calls never write state: a checkout's last write through its
folder (`ls` TOUCHED) is saved with the next operation or the planned stop, so
a crash forgets the writes since.

| path under state | holds |
| --- | --- |
| `bases/<id>/` | a capture (`capture.idx`: every captured path with its mode, blob id, stat data, root class and tracking repository, memory-mapped read-only; a start replaces the `capture.bin` or `capture.json` of a base from an older build with one), its lock, index copies, the git objects its checkouts commit, and `git-template/`, the private git directory its prepared checkouts copy |
| `layers/` | frozen uppers until hashed, then only their non-source files and, above 1 KiB, the layer's compressed manifest; smaller manifests live in `state.json`, so such a stored-only layer needs no file or directory |
| `uppers/` | each checkout's writable copy-on-write layer, named after the checkout, or `_p<id>` for one prepared before it had a name |
| `git/` | private git directories, named like the uppers; a snapshot keeps its checkout's as a store tree, so an unchanged index costs nothing per snapshot |
| `jj/` | jj workspaces made for checkouts being created, until their `.jj` moves into the checkout's upper |
| `store/` | git-format objects in append-only `packs/<n>.pack` (a full pack also gets a sorted index, `<n>.idx`, and every eight of those are merged into one `<g>.midx`); large blobs use content-defined chunks, or plain files under `objects/` when reflinks work |
| `sources/` | each source folder's stat cache: which files still match the blob ids of its last capture's tree |
| `trash/` | leftovers a start found no checkout, snapshot or base naming (a removal a crash interrupted, a create it rolled back): each is moved here with one rename and deleted while the daemon serves, so a start never waits on deleting files |
| `nodes.log` | stable node ids (NFS handles, inode numbers); survive restarts and follow renames. A file's id is derived from its directory and name, so reading a tree logs only its directories; moved files and the rare name-hash collision are logged too. Ids stay below 2^53 so JavaScript runtimes see exact inode numbers |
| `nodes.log.cache/` | the index logged ids page out to, and the names behind derived ids and the hashes of removed ones, by directory (see [memory tiers](#memory-tiers)); a clean stop completes both, so the next start adopts them without replaying `nodes.log`; a crash discards them, and the next start rebuilds the index from the log |
| `mirror.git/` | shallow commits fetched from git upstreams of attached checkouts (see [remote](remote.md#committed-content-from-the-git-upstream)) |
| `node_id` | this daemon's random id; attach offers carry it so the remote tells this daemon's folders apart from other machines' |

Repeated ordinary `new` captures share an existing base when a fresh,
stat-cache-backed capture matches its tree, file/directory metadata, rules,
manifest lock and git HEAD/index/config/refs. The metadata (full mode, mtime,
size and inode of every walked entry) is hashed as the capture walks, so the
check costs no second pass over the folder. Private git directories remain
independent. Revision/take captures, nested repositories, linked worktrees,
busy repositories and captures whose identity cannot be proved are not reused.
Simultaneous first captures can still create distinct bases.

Hashing a snapshot reads and writes only the trees along its changed paths.
Changed trees and small files can be zstd deltas against their same-path parent;
renamed files use their recorded source before layer deletions are applied.
Deltas must halve storage, and chains contain at most 8 deltas. At the cap, the
next delta uses the chain's full root rather than forcing a full new object.
A snapshot's private git index uses the same blob storage.

Blobs over 1 MiB that cannot be reflinked, including streamed/byte input, are
content-defined chunks of 8/approximately 16/32 KiB minimum/average/maximum,
cut where a gear hash of the last 64 bytes has its top 13 bits clear. A chunk
already stored is only marked fresh, not compressed again.
Private chunk hashes deduplicate their bytes while a manifest retains the whole
Git blob id and logical size. Manifest pages hold up to 128 references and reuse
unchanged pages. Overwrites, insertions and appends therefore store changed
chunks rather than full copies; insertions can regroup subsequent metadata
pages. Random reads decode only the requested chunks. Reflinkable files stay
loose, and an already-chunked history stays chunked.

Once hashing pauses for 200 ms (or the daemon stops), each hashed layer writes
its manifest and drops the source files the store now holds; non-source files
remain raw, and the layer's own store tree keeps its stored blobs. A manifest of
up to 1 KiB (a snapshot of about 15 changed files) then moves into `state.json`,
which is synced before the file is removed: a file of its own would take a
whole filesystem block. Larger manifests stay files, since every registry change
rewrites `state.json` with everything embedded in it. Small manifest files left
by a crash or an older build move into `state.json` on reopen.
Copy-up and Git hashing still process whole changed files; chunking reduces
retained history, not the work of computing a Git SHA-1.

Unreferenced layers, bases and store objects are collected in the background.
Captures read the source once: files over 1 MiB are cloned where the filesystem
shares blocks (APFS `clonefile`, Linux `FICLONE` reflinks on btrfs, XFS and
bcachefs, ReFS and Dev Drive block cloning on Windows), otherwise staged and
streamed into chunks; smaller objects are compressed into the current pack.
Packed reads check CRC-32C and the
frame id; rebuilt deltas also verify the typed Git hash. Chunk reads verify
private content hashes, internal manifest pages verify theirs, and whole chunked
blob reads/materialization verify the Git hash. Plain loose reads do not rehash
content. There is no optional verify-on-read mode. A restart cuts a pack's torn
tail. Collection retains transitive delta bases and chunk dependencies,
following each dependency once per sweep. It waits for running captures and,
without holding up reads or other writes, for chunked ingests in flight, and
replaces `store/epoch` before it removes anything; a folder's stat cache trusts its
object ids without checking the store only while that epoch is unchanged. A
pack is rewritten, its live frames copied without recompressing, only once a
quarter of it is garbage.
Pack lookups binary-search memory-mapped index files sorted by object id, so
the index costs page cache rather than daemon memory; only the pack being
appended to is indexed in memory, and it is sealed at 32 MiB or 16384
objects. Storing an object again records its freshness in its index file,
in batches of 4096. Collection marks live objects in a bitmap over the index
records and walks trees in place, so its peak memory grows by a bit per object
plus the ids of loose objects. Indexes written by releases before sorted
indexes are rewritten sorted when the store opens.

On restart the server binds its persisted port, or the NFS socket a macOS mount
dials, and serves before touching the mount path, so hard-mounted NFS I/O
resumes with existing handles and cwd.
`restart` therefore leaves an NFS or SMB mount in place (`stop` with
`keep_mount`): the old daemon cuts its file connections before it closes the
export, and the client retries until the new one serves. From the cut on the
old daemon makes no call into the mount, such as the NFS root refresh after a
create or removal: with no server it would wait for the new daemon, which
waits for the old one to exit. A plain `stop`
unmounts before it replies; while the mount is busy it fails and the daemon
keeps running, and the next `stop` forces the unmount. The daemon closes its
admin socket before its final flush, hash drain and state write, which can take
a while under load; `stop` and `restart` wait for its process to exit, when the
state lock is released, and `restart` holds the start lock throughout, so
auto-starting clients wait for the new daemon rather than failing on the state
lock. A starting daemon opens its admin socket before it loads its state, which
takes minutes on a large state, and holds each hello until it has mounted: a
client that arrives during the load waits for it, up to five minutes, rather
than starting a second daemon. Bases load several at a time, since each
flattens its captured tree, and the daemon logs how long each part of the
state took (`state loaded in 5.2s: state 3ms, 199 bases 4.1s, …`).
Shell completion gives up after a second.
Daemons starting together take turns replacing a stale socket, so only one
listens, and a client whose daemon fails its load quotes the daemon's log.
Metadata uses `fsync(2)`, not `F_FULLFSYNC`: it survives server crashes, not
power loss. `state.json` is synced after its rename, so a kernel crash during
that sync can leave it empty on filesystems that do not order data before
renames (ext4's defaults do). Store objects are synced only when a full pack is
sealed or a pack is rewritten. `snap --wait` waits for hashing, not an object
durability barrier: a kernel crash or power loss can lose active-pack appends
even when snapshot metadata was synced.

## Fast creates

A folder create needs the folder as it is when the create arrives. The daemon
keeps the latest capture of each folder it created checkouts of and watches the
folder; a create first waits until every change made before it was reported
(`watch.Watcher.Sync`), then reuses that capture unless a change since it began
can alter it. Edits below cache, local or hidden roots, in the root's `.jj`, and
in git's object, log and lock files, cannot. Creates that arrive while a capture
runs share it: the capture is a base no checkout is on yet, and once it is the
folder's capture every waiting create opens on it.

A capture of an unchanged file reads no content and no repository data: the
folder's stat cache (`sources/`) holds each file's blob id, including tracked
files a capture found to hold their index blob unconverted, whose blob the
repository keeps while its index still names it. Attribute and blob-size
checks run only for a tracked file the cache does not know. Attribute fallback
reuses the capture's decoded index, including stage-zero `.gitattributes`
entries excluded from its checked-out files. Narrow packed-reference lookups
retain only matching refs while still validating the entire file.
Attribute matching slices directory prefixes from the input path and folds
case once, rather than rebuilding each ancestor's path.

The walk classifies a directory's entries, then lists the source directories
among them on four readers on macOS and Windows, eight on Linux. Directories
it leaves alone are never listed, since listing one can update its access time,
which the watcher reports as an edit.

Checkouts on such a capture are prepared ahead of time, as many as the most
creates of the folder seen running at once (at least 8, at most 256), as far as
their private git directories fit 256 MiB (at least one): upper, private git
directory copied from the capture's `git-template/`, and overlay, below
directories named `_p<id>`. A create names one, which writes only its git directory's
`ovnfs.checkout` marker, and publishes it with a state write shared with
concurrent creates. When only work-tree files changed, the prepared checkouts
move onto the new capture, whose `git-template/` is written after the creates
waiting on it have their checkouts; a change below `.git` discards them.
Prepared checkouts are in no state: a restart removes them, and an unused
capture is released after 15 minutes.

## Native filesystem reads

Capture and live checkout listings share `internal/search/fsys` with native
search. Search keeps compact names, types and optional size/mtime records;
`fsys.ReadDir` supplies complete metadata and per-entry errors without adding
full stat records to compact listings.

Source listings merge their immutable root-relative child paths with native
metadata, retaining captured metadata for changed or missing host entries
without statting or rebuilding every child's path. A bottom-only overlay
takes ownership of the bottom's sorted listing slice and filters it in place;
neither it nor NFS copies or sorts that listing again. The bottom validates
the directory once, including rejecting final symlinks. Junction metadata
constructs host paths only for Windows reparse entries.

Native metadata names share compact, immutable batch storage rather than a
string allocation per entry. Metadata records share batch-sized slabs; a
listing remains valid independently of later session reuse.

Both modes use the same native enumeration loops and reusable buffers:
Darwin's EOF-aware `getdirentries64` and `getattrlistbulk`, Linux's
`getdents64` and descriptor-relative metadata, and Windows'
`NtQueryDirectoryFile`. Unix children open with `openat`; Windows children
use handle-relative `NtCreateFile`. Linux requests `O_NOATIME` until denied,
uses minimal `statx` masks for search, and falls back to `fstatat` when
`statx` is unavailable. Freshness checks do not use `AT_STATX_DONT_SYNC`.

Darwin bulk metadata is checked against native stat fields; entries it cannot
describe exactly use descriptor-relative stat. The search-permission check is
relative to the opened directory, and metadata slabs follow each bulk batch's
entry count rather than allocating a minimum slab for every small directory.
Windows reparse entries use
`Lstat`, and capture retains its inode pass and fresh directory timestamps.
An entry's metadata error remains attached to that entry, so capture can
report it without discarding the rest of the directory.

Native names reject embedded NULs. Windows preserves WTF-8 names and Win32
normalization before direct NT path queries. Positional reads are bounded per
syscall, timestamps saturate instead of wrapping, and symlink-cycle checks
identify an already-open parent by its handle.

## Memory tiers

Written files start in memory, not in the upper: new and truncated files, and
files of at most 256 KiB copied up for writing, share one budget across all
checkouts (`--memory`, default 64 MiB). A file moves into its upper once it has been left
untouched for 2 s, when it outgrows 256 KiB, when newer files need the budget
(least recently used first), before a hard link, `chown` or lock, and before
`snap`, `fork`, `diff` and a planned stop, which therefore cost one host create
per file written in the last 2 s; files deleted sooner never reach the disk.
Directories, symlinks and whiteouts always live in the upper. With
`--fsync` enabled, syncing a named memory file also spills and syncs it;
the default leaves it in memory. NFS does not forward application fsync calls.
This sync does not include the parent directory or snapshot data.

The 256 KiB limit keeps a build's small short-lived files (cargo fingerprints,
dep-info files, temporary files, most rlibs) in memory, while large outputs
(big rlibs, linked binaries) reach the disk after their first 256 KiB instead
of being buffered whole and copied out later. 64 KiB measured 16% slower on a
replayed cold cargo build: mid-sized rlibs then spill while being written.

A base's per-path state is not in memory at all: lookups, listings and
tracked-path facts read its `capture.idx` through a read-only mapping, whose
pages the kernel shares with its file cache and can drop at any time. A path is
found through a hash table of full paths, a directory lists as one contiguous
range of children sorted by name, and git indexes are read only while a capture
runs.

Small store blobs (at most 1 MiB) are served from a second budget
(`--blob-cache`, default 16 MiB) instead of their object files. `ovnfs status` shows both. A
crash or `kill -9` loses the files still in memory; `--memory 0` writes every
file to the upper as it happens.

Two smaller caches have fixed budgets: reconstructed git delta bases, 16 MiB
shared by every repository the daemon reads (git's default is 96 MiB per
process, and the daemon keeps one object database per work tree open), and
the NFS READDIR snapshots that keep a paged listing stable, 8 MiB by estimated
size; the newest snapshot stays even when it alone exceeds that.

The Go collector runs with its defaults (`GOGC=100`: the heap may grow to twice
the live heap between collections; `GOGC` and `GOMEMLIMIT` apply as usual). A
daemon that allocated less than 16 MiB in the last 10 s returns its garbage
and free heap to the OS, so its footprint falls back to its live heap after a
burst. `ovnfs status` shows the Go heap (live, in use, held from the OS) and the
process footprint the OS charges (`heap_live`, `heap_in_use`, `go_held`,
`footprint` in JSON).

Node ids and their directories' cached listings share a third budget,
`--node-cache` (default 16 MiB; `OVNFS_NODE_CACHE`, user-file `node_cache`).
The least recently used directory loses its listing, unlogged leaves, retired
hashes and unlogged nodes. Its issued names move to a sorted disk index first,
beside the hashes of names removed through the export. A cold handle keeps its
exact name even if a later creation has the same hash, and so does every handle
across a clean restart, which adopts the index. After a crash, handles issued
before it resolve by listing their logged parent, so a name that replaced theirs
behind the export with the same 22-bit hash can take them over. Listing I/O never
holds the node-table mutex, and a listing read across a change or relocation of
its directory is not installed.

A directory's owner index is a chain of segments in its instance's one
append-only `pack-*` file: each a sorted array of 16-byte (hash, name length,
offset) rows followed by the names, a zero length marking a retired hash, and
a last link row locating the segment it extends. A spill appends only what the
chain lacks, as one write, or merges the chain into one segment once its
extensions would outgrow its base or it spans four segments, so a directory's
live and superseded segments stay within a few times its live size. Lookups
binary-search the chain through a read-only memory map of the pack under the
node mutex: a cold name costs a few page reads and no heap, the pages belong to
the file cache rather than the daemon's footprint, and no directory is ever
loaded whole. An operation admits only the names it touches: a lookup the name
itself, a mutation whatever owns or retired the hash of the name it changes, so
that the change can complete from memory even after an index fault.

Logged ids page out too, to an index `nodes.log.cache/nodes-*`: a record per
logged key with its id, parent key and name, its logged children and the logged
holders of its derived ids elsewhere, and fixed-size slots locating each record
and the key's owner index, sorted by key for the keys the index was built with
and at their key for keys allocated since. A start after a clean stop adopts
the index that stop left (below); any other start replays the log into a compact
table (about a hundred bytes per logged node, freed before serving), compacts
the log, and builds the index from it afresh. Only the root is resident after a
start.
An operation pages a logged node in when it reaches it: a path walk reads a cold
directory's record through its parent's entry, and a handle walks up parent keys
to a resident ancestor. A resident directory keeps the names, keys and ids of
its cold logged children, so lookups and new derived ids never wait for the
index. A logged node leaves memory after its listing, as least recently used,
once it has no resident children, holds or operations in flight; using a path
uses its ancestors, so they leave last, and the root never does. A node whose
record changed while resident (a creation, move or removal below or of it) has
it appended to the index's `records` file when it leaves; eviction never
renames, reuses or retires an id. Slots of the keys the index was built with
are read through a memory map of the table. Removing a directory logs the
removal without paging in its cold subtree: a record is only paged in under a
parent that still names it, so its descendants stop resolving at once, and
their owner indexes are discounted from the live index size a batch per
operation. A record that cannot be read or contradicts its parent latches the
identity fault below; a failed record write backs off like a failed spill.

The index lives in an instance directory below `STATE/nodes.log.cache`, on
its state volume, rather than `$TMPDIR`: system temporary storage may be tmpfs
and consume RAM, or be removed by temporary-file cleaners during a long run.
Keep the state directory on a disk-backed filesystem to bound memory use.

A clean close completes the index under the node mutex. The owners and
retired hashes still only in memory are appended to the pack in one write, the
record of every resident logged node that changed since it paged in, the
root's included, is appended to `records`, and every slot whose record or
owner index moved is rewritten in place; spills and evictions rewrote theirs
already. Closing thus writes two files whatever the number of directories.
Close then flushes the index files, the pack, trimmed to what was appended, and
their directories, in parallel, and only then writes the manifest
`nodes.log.cache/owners` durably: the identity log's size, its size when last
compacted, the counter watermark, the open's epoch, the index's sizes, the
instance, its pack, the logged node index, and the retired keys whose owner
indexes are still counted live, which the next open discounts. A close that
finds an index write or discount still running, or a failed index or flush,
leaves no manifest.

The next open removes the manifest durably before changing anything, so a
crash from then on discards the index, and keeps the store only when the
manifest matches the log's size. It then adopts the index as it is, reading
neither the log nor any record but the root's, while the log is at most twice
its compacted size, the records at most twice their size when built, and the
pack at most twice its live owners (each plus 64 KiB): 10 million logged
directories with a 186 MB log open in under a millisecond instead of about six
seconds. Past those bounds it replays, compacts and rebuilds, copying the pack
segments the slots of live keys locate into a new pack, merged with any hashes
the log retired since; this drops the garbage adoptions accumulate (log records
of moved and retired keys, superseded records, slots of retired keys, superseded
and retired pack segments). A store an earlier layout left, with owner index
and record files of their own, or with a manifest from before the logged node
index that lists every directory's owner index itself, is always rebuilt: its
owner indexes are carried into the new pack wherever they were, so derived
handles keep their names across the upgrade, and the old instance and index are
discarded whole. Each adopting open advances an epoch that slots record with
every owner index location they are given: one written by an earlier open, like
one carried into a rebuilt index, is prior. A prior owner index that cannot be
read counts as missing for its directory, which then resolves by listing as
after a crash, instead of latching the fault below. The startup log reports
which path the open took and its timings, for example `node ids: adopted
clean-stop index (10000200 keys issued, log 185.7 MB) in 211µs`.

The registry's state lock excludes other daemons before startup moves the rest
of the store, or all of it without a matching manifest, into
`nodes.log.cache.discard/store-*`. The move does not visit index files; a background worker
reclaims detached trees in bounded batches and resumes abandoned cleanup on the
next start. Close cancels that worker without waiting and leaves its instance
for the next open. Cleanup errors leave garbage for a later start without
faulting live owners. Independent logs have independent cache and discard trees.
The index files hold names only, never file contents, and are not part of the
identity log; they stay open, and mapped, while the export runs. Status reports
the pack's and the logged node index's bytes as `node index`
(`node_index_bytes` in JSON), separately from the resident estimate and RSS.
Index writes and record reads release the node mutex and retain operation
state; a write is published only if its directory is still live, and leaves it
dirty if its ownership changed meanwhile. NFSv4 prepares identity holds before
its state transaction and releases them after unlocking; slow index I/O does
not hold the global protocol state lock.

The 16 MiB default holds a build's working set: about 170,000 issued names
(a leaf costs about 64 bytes plus its name), or listings of about 400,000
entries (8 bytes plus the name each). Past it, a directory costs a mapped index
search on its next use rather than memory: walking a 475,000-file tree under
the default, a name or handle the cache had evicted resolves in about 0.5 µs,
against 0.1 µs resident (and 26 µs when the earlier per-directory index files
were read whole), while the table's heap stays near the budget instead of about
70 MB for the whole tree. A larger budget only saves those searches. It does
not bound total daemon RSS.

Resident name lookups walk their parent path once without paging or retaining
unneeded operation state. Listed child identities accept a directory and
basename separately, avoiding full-path construction. A listing is a row of 8
bytes per entry (name hash, length and offset into the listing's names),
sorted by hash and bucketed by its top bits, so a READDIR checks each entry in
a few comparisons; an unchanged listing stays as it is, a changed one is
replaced whole. Leaves and nodes own copies of their names, so no listing or
index buffer outlives its directory's eviction.

An in-flight node operation snapshots the node and its maximum ancestral move
stamp, not an allocated ancestor list. Each relocation receives a table-wide
increasing stamp: even an ancestor moving away and back raises the maximum,
while unrelated moves leave it unchanged. Retained nodes prevent their
ancestors from paging out during that operation.

Filesystem calls use caller-owned checkout session guards with idempotent
release. Operations that may release the node mutex reuse pooled pin sets;
release clears node references, and oversized sets are not retained by the pool.

The budget estimates map, node and name bytes (about 320 per resident logged
node, 160 per unlogged node, 64 per leaf or cold logged child of a resident
directory, 16 per retired hash in memory, 8 per listed entry, besides names),
rather than measuring the Go heap. The current directory stays cached even when it exceeds the budget,
so identifying a large READDIR's entries does not reread the directory for every
entry. Retained reconstructible state is at most the budget or one directory
with its ancestors, whichever is larger; zero keeps just those. `ovnfs status` reports the estimate and
budget as `node cache` (`node_cache` in JSON). Failed spill writes leave the
old index and resident ownership intact, report a degraded `node_cache_error`,
and retry on subsequent operations with exponential backoff (100 ms to 30 s).
While reclamation is blocked above the budget, new identities, mutations
requiring fresh ownership and uncached listings are refused per operation. Existing handles and
open writable descriptors can still resolve, read, write, sync and close. A
successful retry, or invalidation that frees enough resident cache, reopens
normal listing admission without logging every newly observed file. The budget may stay above its limit by the
already resident directory and in-flight work, but failed writes do not permit
new walks to grow ownership indefinitely.

Namespace mutations preflight affected cold owners and keep them resident until
bookkeeping finishes. An unreadable authoritative index rejects the mutation
before changing the backend and latches an identity fault: allocation and cold
promotion stop and protocols report I/O errors, including faults discovered by
an operation's final trim or hold acquisition. Restart clears this fatal fault.
A failed optional spill write never sets the fatal latch.

Retirement records of resident directories and live protocol state are outside
this budget; held nodes and their ancestors stay resident. Nodes with children
are logged; empty directories can be evicted. NFSv4 opens, locks and
delegations, and open SMB, FUSE and WinFsp handles retain their
identities until released, keeping them available to invalidation and recall
scans. The cold index also supplies the issued paths for removals, renames and
invalidation, including recursive and temporary creations with colliding names.
Removed ids remain retired, across clean restarts through the index; eviction
never reuses their hashes or changes the identity log format.

The NFS server separately keeps at most 1024 listing snapshots for READDIR
cookies; eviction from the identity cache does not change those cookies or
verifiers. READDIR checks reply budgets before allocating an identity or encoding
an entry that cannot fit. Continuations still refresh metadata first, so a
removed entry cannot block a later name that fits. Each connection reuses a
bounded pool of request workers, started
on demand; each worker reuses its call and reply-encoder storage, clearing
buffer references after replying. Record markers are decoded directly from
the buffered reader rather than a heap-allocated four-byte temporary.
Disconnect drains accepted calls before releasing the connection.
Its kept writable files expire when idle. NFSv4 state follows client
leases and explicit close/unlock/delegation return. The overlay's `pathIDs` and
`aliases` describe current upper files, are removed with those files, and do
not accumulate paths from read-only walks. These limits do not bound the
registry's live captures or upper indexes.
