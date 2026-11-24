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

In `status`, `version` remains the integer admin protocol generation;
`build_version` is the running daemon's module version, read from its own build
information. Replacing the binary on disk does not change the daemon's build.
Human-readable status shows both daemon and client versions. Different known
versions produce a warning on stderr, without failing or restarting the daemon;
`ovnfs restart` with the same `--state` and `--mount` options uses the invoking
binary. Older daemons without this field show `unknown`; missing versions and
`(devel)` builds are not compared. `--json status` stays the daemon's raw reply,
without a client version or warning mixed into it.

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

`stats` totals what the daemon spent a period on (`--since`, default 1h) from
the record it keeps itself; see [Statistics](#statistics). A profile shows
where the time goes while it samples, the statistics where it went before
anyone looked.

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

State is `state.json` (version 6), atomically replaced with one durable write
per operation (`snap`, `fork` and `new --from` each sync once); multi-step
operations first write an intent that restart rolls forward or back. A
snapshot's tree id is written without a sync: a restart that lost it hashes the
snapshot again. Layer stacks are written only where they cannot be rebuilt from
a snapshot's parent. Builds that read only version 4 would cut store packs at
frame kinds they do not know (chunks, chunk manifests, tree deltas), so version
5 makes them refuse the state before they open the store. Builds that read
only up to version 5 would show a renamed directory's redirect (below) as an
empty directory, so version 6 makes them refuse it; this build reads versions
4 and 5 and rewrites them as 6 when it opens. State of any other version is
refused. File calls never write state: a checkout's last write through its
folder (`ls` TOUCHED) is saved with the next operation or the planned stop, so
a crash forgets the writes since.

| path under state | holds |
| --- | --- |
| `bases/<id>/` | a capture (`capture.idx`: every captured path with its mode, blob id, stat data, root class, tracking repository and whether it is a Git LFS pointer, memory-mapped read-only; a start replaces the `capture.bin` or `capture.json` of a base from an older build with one), its lock, index copies, the git objects its checkouts commit, `git-template/`, the private git directory its prepared checkouts copy, `lfs/`, the Git LFS objects `git lfs smudge` produced for it, and `caches/<id>/`, the cache snapshots that fix what its checkouts see of cache and local roots (`tree/`, its copies of the local roots at their folder paths; `fence`, where its view of the cache roots ends; `roots.json`, written once complete) |
| `layers/` | frozen uppers until hashed, then only their non-source files and, above 1 KiB, the layer's compressed manifest; smaller manifests live in `state.json`, so such a stored-only layer needs no file or directory |
| `uppers/` | each checkout's writable copy-on-write layer, named after the checkout, or `_p<id>` for one prepared before it had a name |
| `git/` | private git directories, named like the uppers; a snapshot keeps its checkout's as a store tree, so an unchanged index costs nothing per snapshot |
| `jj/` | jj workspaces made for checkouts being created, until their `.jj` moves into the checkout's upper |
| `store/` | git-format objects in append-only `packs/<n>.pack` (a full pack also gets a sorted index, `<n>.idx`, and every eight of those are merged into one `<g>.midx`); large blobs use content-defined chunks, or plain files under `objects/` when reflinks work |
| `sources/` | each source folder's stat cache: which files still match the blob ids of its last capture's tree |
| `trash/` | trees given up, until they are deleted: the uppers and git directories of removed checkouts, the layers and cache snapshots garbage collection released, and leftovers a start found no checkout, snapshot or base naming (a removal a crash interrupted, a create it rolled back). Each is moved here with one rename and deleted by a low-priority thread while the daemon serves, so no removal, start or collection waits on deleting files |
| `stats/` | what the daemon spent its time on, a JSON-lines file a day (see [Statistics](#statistics)) |
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

Renaming a directory moves none of its files. One whose tree the base or a
frozen layer holds in part takes a `.wh..wh.redirect` marker naming where
they hold it, its old name a whiteout, and lookups below it continue there
through every lower level, as overlayfs's `redirect_dir` does; so `mv target
target.old` over a large build directory costs the upper's part of the tree,
not a copy of it. Redirects move with their directories, freeze into layers
(a stored layer keeps them in its manifest), and snapshot trees, hashes and
diffs fold them like any other entry.

Unreferenced layers, bases and store objects are collected in the background.
A layer or cache snapshot that goes is not deleted there, nor is the upper of
a removed checkout by `rm`: each is moved into `trash/` with one rename, and
one thread deletes what the trash holds at
the lowest priority the OS offers (background QoS on macOS, so efficiency
cores; nice 19 and idle I/O on Linux; background mode on Windows). Deleting
is an unlink per entry (about 45 µs each on APFS, 130 µs on an efficiency
core), so a layer holding a build's 500,000 files is a minute of CPU that no
longer competes with the builds the daemon serves. That thread gets what
nothing else wants, and on a saturated machine little: once more than 16
trees wait, a second goroutine deletes the excess at normal priority, so the
trash stays bounded.
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
pack is normally rewritten, its live frames copied without recompressing, once a
quarter of it is garbage. Removing an object also rewrites every pack holding
a shadow copy from an interrupted rewrite, even below that threshold, so a
restart cannot restore collected objects.
Pack lookups binary-search memory-mapped index files sorted by object id, so
the index costs page cache rather than daemon memory; only the pack being
appended to is indexed in memory, and it is sealed at 32 MiB or 16384
objects. Storing an object again records its freshness in its index file,
in batches of 4096. Freshness never moves backward when an index merge
overtakes an older batch. Collection marks live objects in a bitmap over the index
records and walks trees in place, so its peak memory grows by a bit per object
plus the ids of loose objects. Indexes written by releases before sorted
indexes are rewritten sorted when the store opens.

On restart the server binds its persisted port, or the NFS socket a macOS mount
dials, and serves before touching the mount path, so hard-mounted NFS I/O
resumes with existing handles and cwd.
`restart` initially leaves an NFS or SMB mount in place (`stop` with
`keep_mount`): the old daemon cuts its file connections before it closes the
export, and the client retries until the new one serves. The new daemon keeps
a compatible mount; mismatched macOS NFS parameters force a fresh mount, with
5 s deadlines per inspection, unmount and mount command. That replacement may
invalidate open handles and lose unflushed client writes. A configured change
between NFS and SMB likewise replaces the retained mount. With SMB write caching,
the old daemon first excludes new write leases and drains existing client caches;
it holds that gate through its final registry close. A failed drain refuses the
restart. From the cut on the old daemon makes no call into the mount, such as
the NFS root refresh after a
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
renames (ext4's defaults do). Store objects are normally synced when a full
pack is sealed or a pack is rewritten; an NFS COMMIT of a file already in
the store also syncs its backing. `snap --wait` waits for hashing, not an object
durability barrier: a kernel crash or power loss can lose active-pack appends
even when snapshot metadata was synced.

## File identity

A file's id (its NFS fileid and handle, SMB file id, FUSE and WinFsp inode
number) comes from what backs the file, never from its path:

    file id = slot << 41 | identity        identity = space (2 bits) | value (39 bits)

- **Identity** (`internal/ident`). A base entry is numbered by its
  `capture.idx` node (space `Capture`), whether its bytes come from the
  source folder or a blob. A file or directory on the state volume (uppers,
  frozen layers, cache snapshots, and the folder's own files below a cache
  root) is numbered by its inode (space `State`).
  Anything without a usable inode, such as a file held in memory, takes a
  per-checkout counter (space `Synthetic`), whose high-water mark the upper
  root records. Copy-up, spill, freeze and rename carry the entry's identity
  to its new backing file, so writing to a base file or renaming it never
  changes the inode a client sees. An identity that differs from the backing
  file's inode is stored beside the file: as the xattr `com.ovnfs.id`
  (macOS) or `user.ovnfs.id` (Linux), or the NTFS stream `:ovnfs.id`
  (Windows), and in a stored layer's manifest. A file that cannot hold one
  (Linux symlinks and special files, or any file on a volume without
  xattrs) has it recorded by inode number in the `.wh..wh.ids` log at its
  upper's root, which moves with the upper into its frozen layer. Volumes
  other than APFS and HFS+ (ext4, xfs, btrfs, NTFS) give a removed file's
  inode number to later files, so there every entry an upper creates gets a
  new `Synthetic` id, persisted at creation, and inode numbers never become
  ids: a client still holding a removed file's id never reaches a new file.
  On APFS an entry keeps its inode's id at no cost.
- **Slot.** Each checkout has a 12-bit slot, persisted in `state.json`
  beside it, so ids survive restarts and the same base file in two checkouts
  has two ids. A new checkout takes the first free slot after the last one
  taken, so a removed checkout's slot is reused only after all 4095 others
  were. Slot 0 is the export's virtual root: the root is id 1 and
  `.metadata_never_index` id 2. Every id stays below 2^53, so JavaScript
  runtimes see exact inode numbers.

Handing out an id lists no directory and records nothing: it is read off the
file information the request already has. An id resolves back to a path
through its checkout's overlay (`overlay.FS.Locate`): the upper's index and
each frozen layer's map their entries' identities, the base resolves its
capture nodes, and every hit is verified with a lookup. An id that names no
visible entry is stale. A protocol handle that outlives its file, its
checkout or a daemon restart is answered as stale, and clients recover by
looking the name up again.

An NFS handle is 16 bytes: the file id and a generation, the checkout's life
(a counter persisted with the slot, so a handle of a removed checkout never
resolves into a later checkout in the same slot) and how often the server
retired the id's handles. A client caching a larger size than the file has
gets its handle retired, so it drops its cached pages, while the file keeps
its fileid; a restart forgets these retirements. Handles of earlier builds
(8 bytes) are stale, except the export root's, so a mount kept across the
upgrade keeps working and looks everything below the root up again.

A path resolved for a handle can stop holding its file before the request
uses it: a rename moves the file or puts another one there. A client that
renames a file over the name of one it keeps open does both, moving the open
file to a silly name (`.nfs…`) whose handle must keep working, as a page-in
of a mapped file needs. So GETATTR, SETATTR, ACCESS, READ, WRITE and LINK's
source, in both NFS versions, and NFSv4's VERIFY and the reclaiming OPEN,
take a file's attributes and bytes only from an entry, or descriptor, with
the handle's identity, and resolve the id again, past the export's cache of
resolutions, when the path holds another file or none; within an NFSv4
COMPOUND the filehandle then names the new path. Other NFSv3 replies leave
out attributes of a path that no longer holds the handle's file. The
frontends that address files by id resolve them the same way
(`export.Nodes.Describe` and `OpenID`): FUSE for GETATTR, SETATTR, OPEN,
LINK and READLINK, WinFsp for a handle's attributes, read-only bit and
times, and the guest kernel client for every operation on a file id.

Builds that kept a node id table left `STATE/nodes.log*` behind; the daemon
deletes those files in the background when it starts.

## Cache and local roots

Checkouts never read the folder's cache and local roots as they are now: a
host build rewriting `target/` would hand them output newer than their
sources. A cache snapshot (`base.Caches`) fixes both. One is taken when a
checkout opens and the base has none, or the folder watcher reported a change
at, below or above a root since the newest began; checkouts opened meanwhile
share the newest, and a checkout records its snapshot's id in `state.json`
(`caches`). A snapshot no checkout uses is moved to the trash by garbage
collection, and one on disk no checkout names by the next start.

**Local roots are copied** below the base's `caches/<id>/tree/`, in the
background: with `clonefile(2)` on APFS, a reflink per file where the volume
has them, bytes elsewhere; first the roots that are no directory, then the
directories. At most two snapshots copy local directories at once across the
daemon. A snapshot without local directories needs no slot. A stop cancels
queued copies and skips directories not yet started; an in-progress native
copy finishes. `roots.json`, written last, marks the snapshot complete; one a
stop interrupted is taken again when its checkout reopens. Its previous tree
goes to the trash, not its newly recorded fence. A root directory
presents the folder's metadata of when the snapshot began and its capture
node's identity, so listing its parent waits only for the first step;
anything below a local root waits for the copy, before the overlay takes its
lock (`overlay.Awaiter`). The copies are snapshot files with `State`
identities. On macOS and Linux a snapshot holds its `tree/` open and resolves
lookups relative to it (`fstatat`, `openat`, `readlinkat`), so the kernel
does not walk the dozen components of the snapshot's absolute path each time.

On macOS, local directory copies use background I/O priority. APFS keeps the
access times, so the timestamp pass only restores modification times and
skips empty directories whose modification time already matches.

**Cache roots are not copied.** A clone of one is cheap to make and dear to
unmake: `clonefile(2)` takes 10 µs an entry and the unlink that undoes it 45
µs, per entry of every root, for every snapshot, whatever its checkouts read
(of two live snapshots of 810,000 and 540,000 files, they had read 14% and
0.01%); and only APFS clones a tree in one call. A snapshot holds one number
for the cache roots instead, its *fence*: the change time the state volume
gives a file made when the snapshot begins (file `fence`). It is read off
that volume's own clock, which also stamps the folder's entries and may tick
behind the daemon's (a jiffy on Linux, 15 ms on NTFS), and taking it waits
for that clock's next tick, 20 ms at most, so that everything the folder
held before the checkout opened is before the fence. The view below a cache
root is then a function of the folder and the fence, computed at every
lookup (`base/fence.go`):

- a file, symlink or special file is shown while its change time is before
  the fence. Content, metadata and renames move a change time, and no tool
  sets it back, as installers do with mtimes;
- a directory is shown unless it was made after the fence, where the
  filesystem records birth times (APFS, ext4, XFS, Btrfs, NTFS): entries
  coming and going move a directory's change time, which therefore says
  nothing;
- an entry on another volume, such as a mount point below a root, is not
  shown;
- every directory above an entry must be a real directory of the folder,
  checked within the last second. This avoids a path walk per lookup; it is
  not a containment boundary against concurrent folder changes.

An entry the folder changes after the fence thus leaves the view and never
changes in it. Each read checks the open inode's change time before handing
back bytes, even if a writer restored the mtime. A host rewrite, unlink or
replacement that advances that inode's change time makes the descriptor
stale (`ESTALE`); NFSv3, which has no open, sees the name gone at its next
`READ`. Because an NFSv3 client keeps cached filehandles for entries it
looked up before the host changed them, each cache snapshot remembers the
pre-fence path and metadata of non-directory cache entries it handed out in
a bounded lock-free table of 32,768 slots (`base/fenced_memo.go`). Once an
entry vanishes behind the fence — without being removed or shadowed in the
checkout — `GETATTR` and `ACCESS` on its held handle answer with those
pre-fence attributes (never the host's newer mtime or size), and a
truncating `SETATTR` (`size=0`, as `open(O_TRUNC)` sends) materializes an
empty file in the checkout's upper under the same identity, so overwrites,
renames over the name and deletes succeed while `LOOKUP` keeps hiding the
host's newer entry and `READ` still returns `ESTALE`. Removing a name in the
checkout does not change the host inode.
There is no persistent per-entry state: nothing to build when a checkout
opens and nothing to delete when it goes. A lookup
below a cache root costs an `lstat` of the folder: 1.8 µs in a 30,000-entry
directory, against 2.0 µs for the clone it replaced.

A shown file is the folder's own inode, so that inode is its identity
(`State`; the root itself keeps its capture node's). That space numbers the
state volume's inodes, which is why the view needs the folder on that volume
and cache roots are empty directories elsewhere. A reused inode number
cannot name two entries: its new holder was made after the fence, which
hides it (a directory, where the filesystem records birth times). `Locate`
maps an identity back to its path with `fsgetpath(2)` on
macOS. On Linux and Windows a snapshot remembers paths in a bounded table
of 32,768 slots and rechecks a path before returning it. On NTFS a local
or cache entry's State identity uses the record number without the file
reference's sequence bits. On other Windows filesystems it requires the full
file index to fit. WinFsp uses the identity to resolve open directories.

Two names of one folder file, as cargo links an artifact into `deps/` and
beside it, share that identity, where a clone gave each name a file of its
own. While both are the folder's they are one file to a client, as hard
links are. The first name a checkout copies up takes the identity along, so
a handle held on the file follows it through a rename or a write; a second
name copied up is another file by then and gets an identity of its own
(`overlay.FS.inherit`). In between, the name not yet copied still reads the
folder's content by path, while a handle on it reads the copy: visible only
to a tool that rewrites a linked artifact in place instead of replacing it.
`fsgetpath` names one path of such a file, not always one the checkout still
shows, so macOS also remembers where files with several names were looked up
and tries that first.

A snapshot taken by a build that cloned cache roots still holds those clones,
unread, until it goes; its fence is when it was complete.

## Statistics

The daemon records what it spends its time on below `STATE/stats/`
(package `stats`), so a busy hour can be explained after it passed:

- a **sample** of the process per interval: user and kernel CPU time and
  the goroutine count;
- **timers**, the count and total duration of repeated work per interval:
  each checkout's file calls (group `calls`, every call a file server makes
  into a checkout, from pinning it to releasing it), each NFSv3 procedure and
  NFSv4 `COMPOUND` (`nfs`), each admin operation (`admin`);
- **jobs**, one line per run of background work, with what it worked on:
  `snapshot.take` (a cache snapshot: its id and folder), `trash.empty` (one
  tree deleted by the low-priority thread, with the CPU time its thread
  used) and `trash.relieve` (one deleted at normal priority because the
  trash was over its backlog), `base.remove`, `store.sweep`.

Recording costs the caller two atomic additions and two clock reads (30 ns
on a file call that takes microseconds); a job takes a mutex twice. Nothing
is written on those paths. Once a minute, and at a stop, the recorder
appends the interval as one batch of JSON lines to the day's segment,
`stats/2026-11-20.jsonl` (UTC), unsynced: a crash loses that minute, and a
torn last line is skipped. Segments older than 14 days are deleted. An
active checkout adds about a line a minute per kind of call it makes.

    {"t":1795167282,"kind":"sample","dt":53.77,"user_us":186869,"sys_us":2801107,"goroutines":7}
    {"t":1795167282,"kind":"timer","dt":53.77,"group":"nfs","name":"READDIRPLUS","n":1645,"us":182961}
    {"t":1795167374,"kind":"job","name":"trash.empty","subject":"caches-c_6b12972204a0","us":69940139,"cpu_us":21665336}

`t` is the Unix time the interval or job ended, `dt` the seconds an
interval covers, and durations are microseconds. `ovnfs stats --since 2h`
totals a period, including the interval not yet written and the jobs still
running; the files are there for anything else:

```sh
jq -c 'select(.kind == "job" and .us > 10e6)' "$STATE"/stats/*.jsonl   # jobs over 10 s
```

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

Capture, cache snapshot and checkout listings share `internal/search/fsys` with native
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
(least recently used first), before a `chown` or lock, before a
rename over a file already in the upper (so the host rename replaces the old
copy atomically), and before
`snap`, `fork`, `diff` and a planned stop, which therefore cost one host create
per file written in the last 2 s; files deleted sooner never reach the disk.
Directories, symlinks and whiteouts always live in the upper. A deleted name
keeps its whiteout on disk until the new memory file at that name spills.
A hard link to a memory file adds a name to it in memory: every name shows
the one file, and unlinking a name leaves the others. When the file spills,
its first name is installed as any spill installs it and the others follow
as one queued host link change, so a crash can lose those names but never
leaves a second copy of the file (Windows and `--memory 0` link on disk in
the call).
On FUSE, SMB and WinFsp a handle sync spills memory files only with `--fsync`.
[NFS stable writes and COMMIT](platforms.md#nfs) spill and sync their file
regardless of the flag; unstable writes can stay in memory.

With the memory tier the loaded upper index is the truth for names too, and
the upper on disk trails it:

- Directory times live in the index. A name change made only in memory
  stamps the directory in the index and makes no host call; copy-ups, spills
  and deletion markers, which clients must not see as a change, leave the
  time alone and no longer restore it on the host. A directory whose time on
  the host differs from the index's is unsettled. The index's times reach the
  host at `Flush`, at a freeze, before the index unloads, for the ancestors of
  a file an `fsync` syncs, and when a call sets the directory's own
  attributes; a crash leaves a time the host chose, which clients see as one
  more change. Windows, whose native records the index cannot give a time of
  their own, and `--memory 0` keep the host's times: each change stats the
  directory afterwards, and each invisible one restores the time it read
  first.
- Some host changes are queued, on darwin and linux: unlink, rename and hard
  link of upper files on disk, and mkdir and rmdir of directories no lower
  layer has. The call changes the index and returns; up to four workers per
  checkout make the queued changes, one host call each, changes on related
  names (equal, or one below the other, or names of one file) in the order
  the index saw them. The index is the authority for link counts: it shows
  the count after a queued change, which the host does not yet, so a call
  that needs the host state of any name of the file waits like one that
  needs the name's own. A call that needs a name's host state (opening a file
  on disk, creating one on disk, a spill, an `fsync`) waits only for the
  queued changes on that name, which the workers make next; `Flush`, a freeze
  and `Empty` wait for the whole queue, and at most 1,024 changes wait per
  checkout. A directory made by the queue gets a new Synthetic identity,
  persisted on it when it is made. A change the host refuses parks the queue:
  it is retried every second, new changes run in their calls meanwhile, a
  call that would wait for the parked change or one queued behind it
  (`Flush` included) returns the failure instead, and `ovnfs status` shows a
  `queue error` row (`queue_parked` and `queue_error` in JSON).
- Names reach the upper's volume after the call, so the overlay checks them
  against the volume first. When it opens, it probes the volume: whether it
  refuses names that are not valid UTF-8 and whether it takes names that
  differ only in case or Unicode normalization for one entry (APFS: all
  three on its default case-insensitive volumes; Linux volumes usually
  none). A create, mkdir, symlink, mknod, link or rename to a new name then
  fails with `EILSEQ` for a name the volume refuses and with `EEXIST` for a
  name the volume would take for another entry of the directory (where the
  host's create without `O_EXCL` would open that entry and its rename would
  replace it); a rename that changes only the case or normalization of an
  entry's own name goes through. Lookups still match names exactly.
- A spill writes a staged copy and installs it with a rename outside the
  checkout's lock, as do the other host changes that run in their calls
  (opens, renames, removes and links the queue does not take): other calls on
  the checkout go on meanwhile, and the file in memory is replaced only if it
  did not change before the install. Deletion markers are index entries, so a
  create over a deleted name knows without a host call whether a marker is on
  disk; the exception is a memory file, which keeps the marker of its name
  until it spills and records that itself.

A daemon crash loses the queued changes: removed names show again and renamed
files under their old names. Every name shows a state it had since the last
`Flush`, because changes on related names reach the host in the order the
index saw them; a file's contents are the file's own, whatever happened to its
names (see [configuration](configuration.md)).

With `--memory 0` every change goes to the host in the call, as before.

When a file's write to disk fails (a full disk, say), the file stays in memory
and the daemon retries it every second. While any file waits, new files go to
disk, so a full disk fails the next write at the client instead of growing
what only memory holds. The daemon logs the first failure of each checkout,
and `ovnfs status` shows a `memory error` row with the waiting count and the
latest error.

The 256 KiB limit keeps a build's small short-lived files (cargo fingerprints,
dep-info files, temporary files, most rlibs) in memory, while large outputs
(big rlibs, linked binaries) reach the disk after their first 256 KiB instead
of being buffered whole and copied out later. 64 KiB measured 16% slower on a
replayed cold cargo build: mid-sized rlibs then spill while being written.

Writable upper indexes load on demand and expire after a minute without
index-dependent access, checked every ten seconds. Registering a checkout,
metadata queries and directory listings inspect only the requested paths.
Opening an upper file or mutating the overlay loads the full index. Open host
or memory handles, pending writes and staged I/O pin the index; cooling keeps
rename hints and leaves files and exported identities unchanged. Frozen-layer
metadata and simultaneously used upper indexes remain outside cache budgets.
While the index is cold, a lookup lists each upper directory it passes once
and keeps the listing until the index loads or cools (only mutations change
the upper, and they load the index first), so a lookup stats nothing in the
upper unless the upper holds the name: a cold lookup of a base file took
seven host stats along its path. A directory of more than 4,096 entries, or
any past 65,536 kept names or 8,192 kept directories, is asked about name by
name instead.

A base's per-path state is not in memory at all: lookups, listings and
tracked-path facts read its `capture.idx` through a read-only mapping, whose
pages the kernel shares with its file cache and can drop at any time. A path is
found through a hash table of full paths, a directory lists as one contiguous
range of children sorted by name, and git indexes are read only while a capture
runs. Tracking includes ancestor directories under the repository that answers
their facts: a nested repository's root keeps its outer repository's tracking,
while paths below it use the nested repository's facts.

Small store blobs (at most 1 MiB) are served from a second budget
(`--blob-cache`, default 16 MiB) instead of their object files. `ovnfs status` shows both. A
crash or `kill -9` loses the files still in memory and the name changes still
queued for the host; `--memory 0` writes every change to the host as it
happens.

Two smaller caches have fixed budgets: reconstructed git delta bases, 16 MiB
shared by every repository the daemon reads (git's default is 96 MiB per
process, and the daemon keeps one object database per work tree open), and
the NFS READDIR snapshots that keep a paged listing stable, 64 MiB by estimated
size; the newest snapshot stays even when it alone exceeds that. `rm` drops
that checkout's snapshots and saved continuation points before its file-id
slot can be reused. A page already in flight cannot put them back. Memory
becomes reclaimable without waiting for another checkout to fill the cache.

The Go collector runs with its defaults (`GOGC=100`: the heap may grow to twice
the live heap between collections; `GOGC` and `GOMEMLIMIT` apply as usual). A
daemon that allocated less than 16 MiB in the last 10 s returns its garbage
and free heap to the OS, so its footprint falls back to its live heap after a
burst. `ovnfs status` shows the Go heap (live, in use, held from the OS) and the
process footprint the OS charges (`heap_live`, `heap_in_use`, `go_held`,
`footprint` in JSON).

NFSv3 READDIRPLUS keeps the names from the first page but must not return an
old size or mode after a write. A regular-file WRITE or SETATTR stamps a
bucket keyed by file id: continuations restat entries in changed buckets,
including aliases of the same file. Collisions cost a stat, not accuracy.
A namespace change, external recall or uncacheable directory still requires
refreshing the whole page. If the listing was evicted and names changed, v3
resumes by name from the newly read directory instead of reading it twice.
Each connection reuses a bounded pool of request workers,
started on demand; each worker reuses its call and reply-encoder storage,
clearing buffer references after replying. The reading goroutine answers a
quick call itself (GETATTR, LOOKUP, ACCESS, CREATE, WRITE and the like) when
the call waits for no earlier change to its file and no further call is
buffered, which spares a goroutine and often a thread wakeup per call; a call
still running after 1 ms hands reading to another goroutine. Replies finished
together leave in one `writev`. Record markers are decoded directly from
the buffered reader rather than a heap-allocated four-byte temporary.
Disconnect drains accepted calls before releasing the connection.
Its kept writable files expire when idle. NFSv4 state follows client
leases and explicit close/unlock/delegation return. The overlay's `pathIDs` and
`aliases` describe current upper files, are removed with those files, and do
not accumulate paths from read-only walks. These limits do not bound the
registry's live captures or upper indexes.
