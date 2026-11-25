# Platforms and frontends

One daemon serves every checkout as a folder of a single mount.

| OS | default frontend | `serve --proto` alternatives | needs |
| --- | --- | --- | --- |
| macOS | NFSv3 | `smb` | nothing |
| Linux | FUSE | `nfs`, `smb` (mounting them needs root) | `fusermount3` |
| Windows | WinFsp where installed and the SMB client dials only port 445, else SMB | `smb`, `winfsp` | [WinFsp](https://winfsp.dev) for `winfsp` |

## NFS

NFS is served by the `nfs` package: NFSv3 with its MOUNT protocol, and
NFSv4.0, on one port or Unix socket. File handles are the export's stable node
ids, so they survive renames and daemon restarts. Automatic mounts use NFSv3
with client-local locks. On macOS the server listens on `nfs.sock` in the state
directory, which only its user can open, and `mount_nfs` dials that socket
(`port=` a path, netid `ticotsord`: undocumented, but in Apple's
`mount_nfs.c`). When that mount fails, or `serve --listen` names a TCP address,
it serves `127.0.0.1` on a free port and mounts with `noresvport`. The macOS
mount uses `locallocks,nobrowse,nordirplus,intr,rwsize=131072` by default
([`nfs_io_size`](configuration.md#where-the-daemon-lives) changes the size), `noatime` unless
[`serve --atime`](configuration.md#where-the-daemon-lives), and normal
attribute caching (the client's default timeouts are 5 to 60 s).
Changes to a directory's entries are serialized per directory, so each reply
carries the directory's attributes from just before the change: the macOS
client keeps its name cache of a directory, negative entries included, only
when they match what it last saw, and otherwise purged it after each of its
own creates. LOOKUP and ACCESS replies leave out their optional NFSv3
attributes. Both NFS versions answer ACCESS from the object's current mode
for the daemon's effective user and groups. A directory's execute bit permits
lookup; changing entries needs write and lookup. This is a mode check, not
an ACL check or authentication of the RPC caller. The backend enforces its
own restrictions when the client performs an operation.
After creating or removing a checkout the daemon `statfs`es its own mount,
making the client revalidate the export root so new checkouts show up
immediately. After changing a checkout's files itself (`pull`), it `chmod`s
each changed file, and every directory from the checkout's root down to it, to
the mode it already has, through the mount: the SETATTR replies carry the
current attributes, and a client that finds them changed drops the file's
cached data and the directory's cached names, missing ones included, instead
of serving them until its attribute timeout. The export root is read-only; cross-checkout rename returns EXDEV
so `mv` copies. It also holds an empty, unlisted `.metadata_never_index`, the
file macOS checks for on every new volume to keep Spotlight from indexing it.

The client's attribute and name caches live in its vnodes, which macOS caps
system-wide at `kern.maxvnodes` (263,168 on a 128 GB Mac). A full walk such as
`git status` takes two per file, the client's and the daemon's for the source
file; past the cap the kernel recycles the walk's own vnodes before the next
walk reaches them, so every walk sends one LOOKUP per file. On a 479,000-file
checkout that made a warm `git status` 27 s; with `kern.maxvnodes=2097152` it
took 2 s. `ovnfs status` warns when the cap is below twice the largest
checkout's file count and prints the `sysctl` that fixes it.

NFSv3 has no extended attributes, so the macOS client keeps a file's in an
AppleDouble `._name` file beside it, and the kernel tags every file and
directory a process creates with `com.apple.provenance`: each new entry costs
a second file, with its own CREATE, WRITE, SETATTR and GETATTR calls. The
export never lists `._` files, and unless [`nfs_xattrs: true`](configuration.md#where-the-daemon-lives)
keeps them, the server answers the calls on them without storing anything: a
lookup finds none, a create hands out a handle that keeps only the size
written, and removing or renaming one changes nothing. Attributes set in a
checkout last only while the client caches them. Four concurrent `bun
install`s of a 13,000-file lockfile took 18 s instead of 27 s and a third less
daemon CPU than with the files kept; a single install, bound by the client's
round trips, was no faster, since the client makes the same calls. Refusing
those creates would save the client's calls too, but fails `setxattr`, which
`ditto` and `xattr -w` report.

The 128 KiB I/O buffers balance bulk transfers with small random writes:
macOS maps a whole buffer for a write and flushes it before a discontiguous
write. Larger buffers make 4 KiB writes disproportionately expensive.
When a regular file shrinks through another frontend or a folder change,
NFSv3 retires its existing handle instead of reporting the smaller size on
that handle. The client receives `ESTALE`; a fresh lookup gets a new handle
with the current attributes. A resize requested on that connection keeps its
handle. A concurrent GETATTR or READ that sampled an earlier size rechecks
it; replies with optional attributes can omit them instead. READ never uses
a previously reported larger size to fill out the end of a smaller file.

On macOS, startup and `restart` keep a retained NFS mount when its options
match, but retire every non-root handle so cached files are recovered through
fresh lookups. A mismatch is logged and forces an unmount and fresh mount,
even when busy. Retiring handles may interrupt operations on files that
remain open across the restart; replacing a mount may additionally lose
unflushed client writes.

Inspection, unmount and mount commands each have a 5 s deadline. Cancellation
kills the command's process group without waiting indefinitely for a stuck
kernel call to finish. Inspection failures leave the mount untouched; a failed
or timed-out replacement fails startup instead of silently keeping old options
or attempting another transport.

NFSv3 retains up to 4096 writable descriptors across write bursts, expiring
them after 10 s idle instead of reopening a host file for every small write.
Cached writers are keyed by file id and path, so renames reopen at the current
path; the export's generation-aware handles preserve snapshot isolation.

With `serve --fsync`, NFSv3 and NFSv4 mark `UNSTABLE` writes as buffered, and
`DATA_SYNC`, `FILE_SYNC` and `COMMIT` (including a byte-range `COMMIT`) flush
the whole addressed file, not every buffered file. For a live upper this
spills a memory file and syncs its data, identity metadata and the
directories on its current path before replying. A failed barrier returns an
NFS error.

Without it (the default) the server syncs for none of them and reports every
write as `FILE_SYNC`, so the client sends no COMMIT at all: the macOS client
otherwise commits every written file when it is closed, which cost a build
more daemon time than all its other calls together and spilled every small
file the memory tier would have kept off the disk. A crash of the daemon or
the host can then lose writes the client was told are stable, as it can lose
any other recent change (see [persistence](internals.md#persistence)).

In this mode NFSv3 can reply before applying a regular-file WRITE. The daemon
copies the bytes before replying and drains them before a reader, namespace
change or direct checkout operation needs them. A backend error after the
reply is logged and reported on the file's next WRITE or COMMIT; the original
caller cannot be notified if it never sends another one. Use `--fsync` when a
write must not be acknowledged before it is applied. See
[write-behind](internals.md#nfsv3-write-behind).

A client can implement `fsync` of dirty data with these requests; NFS has
no standalone file or directory `fsync` RPC. An application `fsync` that
sends no WRITE or COMMIT cannot force an empty create, rename or other
metadata-only change to disk. This is not a transaction over an old and
new pathname, a snapshot durability barrier, or a general power-loss
guarantee for the checkout; see [persistence](internals.md#persistence).

Calls on one file keep their arrival order wherever replies show it. WRITE
and SETATTR run one at a time per file, each reporting the attributes from
just before and just after itself; GETATTR, ACCESS, READ and COMMIT wait for
the changes that arrived before them. The macOS client recognizes its own
writes only through that unbroken chain: when concurrent writes interleaved
it, the client dropped every page it had just written and re-read the whole
file at its next open.

A removal or rename the daemon cannot make on disk fails with `EIO`, never
`ENOENT`, once the checkout shows the name: the macOS client takes `ENOENT`
from REMOVE, RMDIR or RENAME for the reply to a retransmission and reports
success, so `rm` would leave the file and fail later with `Directory not
empty`, and `mv` would leave the file where it was.

The server also accepts NFSv4.0 clients on the same socket or port. These may receive
read delegations, recalled on changes through another protocol, `pull`,
reattach and checkout removal: edits in the source folder never change a
checkout's files (cache and local paths come from a snapshot, see
[configuration](configuration.md#classes)). NFSv4.0 is not the automatic
mount: its stateful open and close traffic substantially slows cold Cargo
builds on macOS.

### When the NFS daemon stops answering

The default NFS mount is hard: requests that need a reply keep retrying.
The 5 s deadline for ovnfs's mount commands does not bound `stat`, `read`
or `write` from a checkout. One stalled daemon can block clients of every
checkout on its mount.

Recover from a shell whose working directory is **outside** the mount:

1. If the daemon was paused, resume that specific process after verifying its
   PID and state directory. If it is busy, let it drain. Restoring the server
   gives pending requests a chance to finish without replacing the mount.
2. If it exited, try `ovnfs --state "$state" --mount "$mount" start` with
   the original absolute paths. Do not delete the state directory.
3. If startup cannot recover the mount and no daemon for that state is
   running, stop clients of those checkouts. Verify the mount point in
   `mount` output before using `umount -f "$mount"` as a last resort, then
   run the same `start` command. A forced unmount invalidates open handles
   and can discard unflushed writes; reopen files and check affected output.

A soft NFS mount trades waiting for errors; it is not a repair for a dead
server. A timed-out request can report `ETIMEDOUT` on macOS or `EIO` with
Linux `soft`. It does not prove a write had no effect: files may be only
partly updated. A failed page-in of a mapped file can deliver `SIGBUS`
instead of a recoverable I/O error. On macOS, a signal that interrupts a
page-in on the default `intr` mount can do the same ([#101](https://github.com/stencil-hq/ovnfs/issues/101)).
With [`nfs_nointr`](configuration.md#where-the-daemon-lives), a signal no
longer aborts that wait with `EINTR`. I/O can block indefinitely even after
Ctrl-C if the daemon cannot reply. This does not protect against a lost
mount or other page-in errors such as the `ENXIO` reported during daemon
handover. To avoid depending on the mount, keep both executables and native
libraries on a local filesystem; writes there are not checkout-isolated.

## FUSE

`serve --proto fuse` (the Linux default) mounts through `fusermount3`, with the
export's file ids as inode numbers and one-second entry and attribute
timeouts. A directory's names are fixed when it is opened, so offsets stay
stable, but READDIRPLUS stats each entry as it answers: the kernel keeps the
attributes it gets for the attribute timeout, so attributes from the time of
opening would hide a file written meanwhile. A mount left behind by a daemon
that died without unmounting (`kill -9`, the OOM killer) is detached when the
next one starts. The `stop` that forces a busy mount aborts its connection
through fusectl (`/sys/fs/fuse/connections`) and then detaches it, so
processes still inside get `ENOTCONN` (`EIO` on reads of uncached data)
instead of keeping the daemon running. Where fusectl was not mounted when the
daemon mounted (some unprivileged containers), the forced `stop` cannot abort
and fails while the mount stays busy, like the first.

A daemon not running as root reports every file as owned by its own user,
with the source's mode bits, so a checkout of another user's folder is
writable wherever the source is readable. A root daemon reports real owners
and gives new files to the user creating them.

## SMB

`serve --proto smb` serves the `ovnfs` share, to guests on macOS and Linux and
to one account on Windows (below), with Unix modes, executable bits, symlinks,
byte-range locks and change notifications; macOS smbfs caches negative lookups
for about two seconds.

SMB2.1+ file leases allow read and handle caching, recalled on conflicting
opens, rename/deletion, and daemon changes. On macOS, `serve --proto smb` also
grants write leases for regular files already owned by a checkout's writable
upper, including memory files and build outputs. Lower files, hard-link aliases and
private git/jj metadata never receive write leases. Other platforms retain
read/handle caching only; exclusive/batch oplocks and directory leasing are
not granted.

The macOS client coalesces small writes in its kernel cache. Before a snapshot,
fork, diff, search, remote-fs request or other direct checkout access, the daemon
blocks new write leases, recalls existing ones, and waits for their dirty writes
and acknowledgements before taking checkout locks. Restart drains these caches
before cutting client connections. These barriers have a 5 s acquisition limit;
a failed or lost dirty owner fails closed instead of publishing incomplete
snapshots. A protected operation keeps new write leases excluded until it ends.
Ordinary operations by a lease's owner preserve its cache where SMB coherence
allows it.

Application fsync still reaches the backing file, subject to the existing
[memory-file fsync policy](configuration.md#where-the-daemon-lives). Repeated
writes to a small working set may exceed native-file benchmark rates because
only the final dirty pages cross the protocol; this is buffered throughput,
not physical-device bandwidth. This does not imply faster builds: measure cold
and incremental compilation separately before switching a development workload.
NFS remains the macOS default. SMB still lacks
[hard links and durable handles](limitations.md). The native macOS SMB client
rejects hard links, so Rust copies artifacts that NFS can link. Without native
named streams, macOS also uses AppleDouble files for xattrs such as provenance;
these are real metadata reads, writes and locks, not dispensable failed probes.
Disabling client caches or dropping those writes is not a correctness-preserving
build optimization. The server avoids unnecessary case-fold directory scans for
negotiated case-sensitive clients and preserves vectored response writes through
its connection wrapper.

The server resolves a CREATE of an existing name with one lookup (a lookup
succeeds only through directories, so symlinked components are walked only
when it fails), opens no backend file for attribute-only and delete opens, and
answers the QUERY_INFO requests Windows sends after each CREATE from that
lookup until any command that could change a file runs, for at most a second.
CREATE and CLOSE of different names, and renames and deletions of files, run in
parallel; renames and deletions of directories, delete-on-close of directories
and creates that follow links run alone.

## Windows

### SMB

The daemon maps its share onto the mount drive (`WNetAddConnection4W`, as
`net use` does) and refuses a drive mapped elsewhere. The SMB client dials
another port only from Windows 11 24H2 (build 26100), so older builds serve on
`127.0.0.1:445`, which the Server service holds until it is stopped
(`sc config LanmanServer start= disabled` and `net stop LanmanServer /y`, from
an elevated prompt; this ends the machine's own file sharing); the daemon says
so when it cannot bind. Drive mappings belong to a logon session, so a daemon
started over ssh maps `O:` for that session only.

Windows 11 24H2 and Server 2025 refuse anonymous NTLM, guest logons and
unsigned sessions by default, so on Windows the server accepts one account,
`ovnfs`, with a random 128-bit password, and its sessions are NTLMv2
authenticated and signed. The password is kept in `smb-password` in the state
directory, readable by the daemon's user alone, so a mapping a previous run
left reconnects to the next; it is passed to the SMB client as an SSPI
identity, never on a command line, and never logged. Deleting the file makes
the next start create another password.

The daemon opens host files with delete sharing and renames with POSIX
semantics, so clients can rename, replace and delete files other handles hold
open, as on Unix; because Windows never renames a directory while a file below
it is open, `snap`, `fork` and `rm` first close the checkout's upper
descriptors, which reopen in the next generation. `apply` replaces source
folder files, and `apply --branch` the folder's git index, the same way, so
read-only files and files other processes hold open with delete sharing are
overwritten; a holder that refuses delete sharing fails the apply with a
sharing violation.

Served on loopback, files are owned by the daemon's user and primary group,
as on the WinFsp drive, so Git's ownership check accepts checkouts with no
`safe.directory` entry. A share served on another address keeps reporting the
Unix-ID owner `S-1-22-1-0`.

### WinFsp

`serve --proto winfsp` mounts the drive through [WinFsp](https://winfsp.dev), a
filesystem driver installed once per machine (installing needs an
administrator; mounting does not), as the network volume `\\ovnfs\O`, with no
SMB and no port. It is the Windows default where WinFsp is installed and
the SMB client dials only port 445; elsewhere SMB stays the default, being faster.

Files look as they do to Windows SMB clients: case-sensitive names,
`FILE_ATTRIBUTE_READONLY` as the owner write bit, file ids as file indexes,
symlinks as described below. Open files can be deleted and renamed over, as on
Unix. The driver caches metadata for one second and never caches data.

### Links

Both frontends report a symlink whose target is a directory with
`FILE_ATTRIBUTE_DIRECTORY`, as NTFS directory symlinks are: Windows tools enter
only links marked so. Opened as itself, such a link counts as a directory, so
`rmdir` removes it. `CreateSymbolicLink`, `mklink /D` and junction tools
create directory links on the drive; a junction is stored as a symlink to its
target. `mklink /J` refuses network drives before asking the daemon.
A link created with a target that names its own checkout through the drive
(`O:\app\x`) or the share (`\\127.0.0.1\ovnfs\app\x`, as Bun writes them) is
stored relative to the link: served as written, it would lead the daemon back
into its own mount.

The SMB server follows links itself, since Windows refuses by default to
follow links between network paths. Over SMB 3 it flags an open that
followed a link at the end of its path with `SMB2_CREATE_FLAG_REPARSEPOINT`;
without the flag, Windows would describe the link by its target for its
metadata cache lifetime, and realpath in tools such as tsc and Bun would
stop seeing it.

A git checkout's private git directory on the host links `objects` to its
base's shared object directory and, with `git_hooks` on, `hooks` to the
folder's hooks directory. An account that may not create symlinks
(no `SeCreateSymbolicLinkPrivilege` and Developer Mode off, as for most
standard users) gets junctions there instead; git and the daemon read them as
the same links. A junction can't point at a network-share (UNC) path, so for a
folder on a share such an account gets a daemon notice and checkouts without
hooks.

### Unix modes and Defender

Unix mode bits that NTFS cannot hold (execute, setuid, setgid, sticky, and any
mode but 0644/0755 or their read-only forms) live in an `:ovnfs.mode` alternate
data stream of each upper and layer file, so they follow renames, hard links
and snapshots; the state directory must be on NTFS or ReFS for such modes. New
files get 0644 and directories 0755, as with a umask of 022. Likewise an
`:ovnfs.id` stream keeps the file id of a file copied up from a lower layer or
written from memory (see [internals](internals.md#file-identity)); a file's
own id is its NTFS file record number. If an interrupted write leaves fewer
than eight bytes in a writable upper's `:ovnfs.id`, ovnfs treats that id as
missing and persists a replacement without changing the file's contents.

Microsoft Defender scans every file the daemon writes into its state and every
file Windows reads over SMB; excluding the state directory
(`Add-MpPreference -ExclusionPath $env:LOCALAPPDATA\ovnfs`, as administrator)
cut a first capture of a 12k-file repository from 4.8 s to 3.7 s on the test
VM.

Defender dominates WinFsp's speed: it reads a file again on every open of a
WinFsp volume, and path exclusions do not match WinFsp volumes. In a Windows 11
arm64 VM a no-op `go build ./...` of this repository took 3.6 s with the drive
as a local disk, 1.2 s as a network volume, which Defender scans less, and
0.85 s over SMB. Excluding the opening process from Defender
(`Add-MpPreference -ExclusionProcess`) cut an open and close from 400 µs to
49 µs, against SMB's 53 µs.

## Cache and local roots and metadata

A cache snapshot takes a local root, and a checkout reads below a cache
root, only while each ancestor in the folder is a real directory, not a
symlink. Below a local root a lookup proves that of the snapshot's
directories once, since its copies never change; below a cache root a proof
of the folder's directories lasts a second, as long as the kernels'
attribute caches. Directory listings take their entries' metadata, the
change time included, from the listing itself; only on Linux a listed
directory costs a `statx` for its birth time. A checkout's own files are
described from its in-memory index, which every change made through ovnfs
refreshes; only a file open for writing is looked up on the host.

A symlink below a cache or local root whose absolute target lies inside the
folder reads as the same path relative to the link, so in a checkout it names
the checkout's copy: Bun on Windows links workspace packages in
`node_modules` by absolute path. Absolute targets outside the folder stay as
stored. Windows junctions, below roots and in source alike, read as the
directory symlinks they act as.
