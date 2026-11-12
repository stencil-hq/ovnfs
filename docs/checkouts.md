# Checkouts, snapshots and apply

## Git repositories

A checkout of a repository gets a private git directory: HEAD, refs, config and
the index are copies taken at open, and objects borrow the repository's through
alternates. Inside the checkout, `git status` matches the folder's, commits land
in the checkout, and they reach the user's repository only when
`ovnfs apply CHECKOUT --branch NAME` fetches them as a branch or you fetch the
checkout's [git remote](#git-remotes) yourself.

If NAME is checked out in the folder, apply fast-forwards it and updates only
the index entries changed by those commits, never the working-tree files.
Staged changes on those paths or a non-fast-forward are refused; use another
branch name instead. Unrelated staged work stays staged. `--dry-run` previews
this checked-out-branch update without writing.

On a 49k-file repository, opening a checkout takes about a second and
`git status` inside it under three. Later creates of the same folder reuse that
capture while the folder has not changed in a way a checkout shows, and take a
checkout prepared ahead of time: on a 3,000-file repository on Windows, a create
returns in about 15 ms, and after a source edit the folder is captured again in
about 50 ms, once for all the creates waiting on it (see
[internals](internals.md#fast-creates)).

Repeated ordinary `new` calls share a captured base when the folder's fresh
stat-backed capture, rules and git metadata still match. Each checkout still
gets its own writable layer and private git state. `--rev`, `--take`, nested
repositories and captures whose identity cannot be proved use a fresh base.

A submodule absorbed into the repository's `.git/modules`, as `git submodule
add` and `git submodule update --init` lay it out, gets a private git directory
of its own, built the same way and placed below the checkout's at
`modules/<name>`, where git looks for it; the submodule's `.git` in the checkout
points there. Commits made in the submodule stay in the checkout, forks and
snapshots carry them, and the folder's `.git/modules` is never written. A fork
or a checkout from a snapshot points every submodule `.git` at its own private
git directory, wherever the submodule was moved, and a deinitialized submodule
stays without one.
Submodules absorbed into such a submodule work the same way. Other nested
repositories keep their `.git` as a local copy-on-write directory. A `--rev`
checkout starts with empty submodule directories; `git submodule update --init`
inside it clones them into its private git directory.

Each private gitdir records repo-local `ovnfs.checkout = NAME`, including
forked checkouts with their own names. Existing checkouts gain this marker when
the daemon next opens their registry. The marker is never written to the source
repo.

### Git remotes

The folder's repository gets a remote `ovnfs/NAME` for each live checkout NAME
of it, pointing at the checkout's private git directory; `git_remotes: false`
in `~/.o2/ovnfs.yml` (or `serve --no-git-remotes`, `OVNFS_GIT_REMOTES=0`)
turns this off. `git fetch ovnfs/NAME` (or `git fetch --all`) stores the
checkout's HEAD as `refs/remotes/ovnfs/NAME/HEAD`, so `git log ovnfs/NAME`,
`git diff main...ovnfs/NAME` and `git cherry-pick ovnfs/NAME` see the checkout's commits
without an `apply`. Tags are neither fetched nor pruned, whatever
`fetch.pruneTags` says: a checkout's tags are a stale copy of the folder's.

The remotes live in `.git/ovnfs-remotes`, which ovnfs rewrites whenever a
checkout of the repository is created or removed, and which `.git/config`
includes. Creates stay fast: the folder watcher ignores that file, and
checkouts, whose config is a copy of the folder's, include a file they do not
have, so none sees the others' remotes. Removing a checkout (`rm`, `prune`,
the end of `run`) removes its remote and the tracking refs fetched from it.
Turning the setting off removes every checkout remote, the file and the
include when the daemon restarts, for repositories with live checkouts, and
otherwise at the repository's next create or removal. Manage these remotes
through ovnfs: `git remote remove ovnfs/NAME` deletes the refs but fails to
edit the included file.

## jj repositories

A folder whose root is a [jj](https://jj-vcs.github.io/jj/) workspace,
colocated with git or not, gives each checkout a jj workspace of that
repository named after the checkout: the repository and its operation log are
shared, the working copy is the checkout's own. Its working-copy commit starts
on the parents of the folder workspace's `@`, as `jj workspace add` does, so
`jj st` in a fresh checkout shows no changes and `jj log` shows every
checkout's `@` as `NAME@`. When the repository already has a workspace of that
name, the checkout's is `NAME-2`, or the next free number; `new` and `fork`
print it as `jj: NAME`.

ovnfs runs `jj workspace add --no-colocate` from `jj` on PATH in an empty
directory, `STATE/jj/NAME`, which jj records as the workspace's path (`jj
workspace list` shows it), and moves its `.jj` to the checkout's root; the
checkout's `.git` stays its private git directory, which jj neither reads nor
updates. Like any jj command in the
folder, this snapshots the folder's working copy first, and it writes the full
tree into that directory: about 0.8 s for 5,000 files on a Mac. Every new
checkout gets a fresh workspace: `new`, `run`, `fork`, `new --from SNAP` and
`new --base CAPTURE`. Removing the checkout (`rm`, `prune`, the end of `run`)
runs `jj workspace forget` for it; a repository that is gone has nothing to
forget.

The folder's `.jj` never shows in a checkout, and a checkout's `.jj` belongs to
it alone: `diff`, `apply`, snapshot trees and `plan` leave it out, `snap` and
`fork` keep it with the checkout instead of in the snapshot, and `ls` does not
count its changes. Without jj installed the checkout is created without a
workspace, with a warning. Checkouts attached on another machine get none.

## Starting from a revision: `--rev`

`ovnfs new [DIR] --rev REV` starts with REV's tracked files and index. A local
branch name (including `refs/heads/NAME`) attaches HEAD to that branch; a
commit, tag or `HEAD` detaches it. Untracked nonignored files disappear from
the checkout, while ignored files and build directories remain available.
Hidden paths stay absent, except files REV tracks.
Ignore rules come from REV, not the folder's dirty `.gitignore` files. The
source folder and its git state are never changed. `--rev` cannot be combined
with `--from SNAP`; it requires a repository and a revision resolving to a
commit.

The revision is the checkout's base for `diff`, `snap` and `apply`: an
immediate diff is empty, and applying later work does not undo dirty edits
still in the source folder. Revision checkouts substitute tree metadata and
read git blobs lazily, rather than copying or checking out the whole tree.
The private index records the size and mtime the checkout shows for each file
holding REV's content, so the first `git status` trusts those files instead of
reading them; other files, and any shown with an mtime within the second the
index was written, are read.

## Taking paths: `--take`

`ovnfs new [DIR] --take SRC:PATH` replaces a repo-relative file or directory
recursively; repeat it for more selections, with later ones winning. SRC is `.`
for the captured working tree, empty for the staged index, or a revision
resolving to a commit. An empty PATH selects the whole tree. Paths must be
normalized and relative; a path absent from both source and base is an error.
Only source-class paths change: cache, local and hidden paths keep their
ordinary or `--rev` behavior. `--take` cannot be combined with `--from SNAP`.

Taken content becomes part of the base: the initial diff is empty, snapshots
and forks retain it, and apply never writes the selection back to the folder.
Content stays lazy, and the private git index is unchanged: it still comes from
REV with `--rev`, or from the folder otherwise. Git therefore shows taken
content as working-tree changes relative to that index. For example:

```sh
ovnfs new --rev HEAD --take :                  # the staged index alone, without unstaged edits
ovnfs new --rev BASE --take pr-head:path/to/test  # old code with a PR's test ready to run
```

## Disposable runs: `run`

`ovnfs run [DIR] -- CMD [ARGS]...` opens a checkout, runs CMD directly in the
caller's matching subdirectory, and removes the checkout even when CMD fails or
is interrupted. It preserves CMD's output and exit status and sets
`OVNFS_CHECKOUT`; `--keep` retains the checkout and prints its name and path on
stderr. It accepts `--name`, `--rev`, repeatable `--take SRC:PATH` and
`--rule LINE`; `--` is required.

`--on HOST [--remote-bin PATH]` instead runs over ssh and keeps both checkouts
as a lane for [`pull`](remote.md#pull). Its remote name defaults to the
folder's name, so repeated runs reuse that remote lane. It attaches the folder
directly unless `--rev`, `--take` or `--rule` requires opening a local checkout
first.

## Snapshots and forks

`snap` renames the upper into an immutable layer in O(1) and hashes its source
paths into a git-format tree in the background (`--wait` blocks); hashing
touches only changed paths. Trees and small files can use deltas against their
previous versions; large files share reflink blocks where supported, otherwise
content-defined chunks. Copy-up and hashing still process the whole changed
file. The metadata of a small snapshot (about 15 changed files) lives in the
registry state instead of a manifest file of its own. `fork`
snapshots a checkout and opens a second one on that snapshot; parent and child
diverge. `new --from SNAP` opens a checkout on any snapshot, and
`new --base CAPTURE` on a live checkout's base (`ls --json` shows it) as it was
captured, with the git state that checkout started from, however the folder and
the checkout have changed since. `rm` removes a checkout and its snapshots
unless another checkout or snapshot uses them. `rm --sticky-snapshots` keeps
them; `drop` releases one explicitly. Nothing replaces the content of a live
folder: rollback means a new checkout from a snapshot.

`new` (and `run`) on a checkout's own folder forks that checkout: capturing it
would make the daemon serve the new checkout by reading through its own mount,
which can deadlock the mount. `new` refuses the mount itself, a folder below a
checkout's root, a folder (or repository) holding the mount, and `--rev`,
`--take` or `--rule` with a checkout. These folders are told by file identity,
so a symlink, another letter case or a bind mount does not get around the rule.

## Listing and cleanup: `ls` and `prune`

`ls` shows each checkout's TOUCHED time: its last write through its folder or
to its private git directory, else its creation. `ls DIR` lists only the
checkouts of DIR: those opened on the folder, on a folder inside it, or on the
folder holding it; inside the mount, those sharing the source of the checkout
DIR is in. DIR needs a path spelling (`.`, `..`, a slash), since a bare word
names a checkout, whose snapshots `ls NAME` lists.

`prune [DIR] [--idle DUR] [--no-diff]` removes, as `rm` does, every checkout
matching all the criteria given: of DIR, untouched for DUR (`90m`, `12h`, `7d`,
`2w`), without source changes since its base (an empty `diff`). Without a
criterion it refuses, `--dry-run` lists what it would remove, and a checkout
touched after it was picked stays. It removes unused snapshots of those
checkouts; `--sticky-snapshots` keeps them. Plain `ls` also shows snapshots of
removed checkouts. `ls` takes the same selection flags to preview a prune in
full. `--no-diff` alone also picks
checkouts just opened, before their first write; add `--idle` to spare them.

## Search: `grep` and `glob`

`grep PATTERN [DIR|CHECKOUT]` searches contents; `glob PATTERN [DIR|CHECKOUT]`
lists matching paths. Both run in the daemon. A named checkout, its mounted
path, or a directory below that path reads the merged overlay directly,
including memory-resident writes, frozen layers and live cache/local paths.
There is no recursive traversal through NFS, FUSE or SMB. Other folders use
the daemon's native filesystem. Omit the target to search the current directory.

```sh
ovnfs grep 'func\s+New' app --glob '*.go' -C 2
ovnfs glob '*_test.go' app --limit 100
ovnfs --json glob '*.go' app --mtime
ovnfs grep 'needle' app --path src --mode filesWithMatches
```

Patterns without a slash match at any depth; `--anchored` restricts them to
the search root. Ignore files are honored, dot-prefixed paths and
`node_modules` are skipped, and `.git` is always pruned. `--no-ignore`,
`--hidden` and `--node-modules` override those respective traversal defaults;
a pattern naming `node_modules` also enables it. Hidden-class paths remain
absent regardless of these flags. Symlink directories are not descended.

Grep supports `-F` literal text, `-i`, `--multiline`, file `--type` filters,
`content`, `filesWithMatches` and `count` modes, context (`-A`, `-B`, `-C`),
`--offset`, `--max-count`, `--max-count-per-file` and `--max-columns`.
Grep's `--glob` also searches every file under a directory it matches
(`'{src,test}'`, `'packages/*/src'`), while `'dir/*.ts'` stays limited to
`dir`'s direct children. Repeatable `--exclude GLOB` skips matching files
and does not enter matching directories; a slash-free exclude matches at any
depth and a leading `/` anchors it to the search root. Glob patterns take
`\` as `/` and close unclosed `{` groups. `--max-columns` cuts context lines
to their start but windows a long matching line around its first match,
reporting that match's character `column` in JSON.
Specifying either `-A` or `-B` replaces both sides of `-C`. Each file is
searched over its first 4 MiB by default; `--max-file-size` changes the
window, and JSON reports files whose tails were skipped. The regex dialect
retains gogrep's literal repairs for malformed patterns; unsupported
constructs such as lookaround return an error.

Glob supports `--type file|dir|symlink`, `--limit`, and `--mtime` for
newest-first results with metadata. Both commands accept `--workers`,
`--max-depth` and a root-relative `--path`. Results have deterministic path
order unless glob sorts by time. Empty results succeed.

In a checkout [attached from another machine](remote.md#search), grep scans
the files that machine still holds there and moves only results; see the
remote documentation for what crosses the link.

Ctrl-C cancels daemon-side search work, not just the CLI's wait. Embedded Go
clients cancel an individual `admin.Client.Call` context or set its deadline;
other calls on the connection remain usable. Canceled searches return an
error, not a successful partial result. Cancellation is cooperative: an
in-progress filesystem call, lock acquisition or regexp operation must finish
before worker cleanup can complete.

The same `grep` and `glob` operations are available over the admin socket;
[`api/search.go`](../api/search.go) defines their arguments and JSON replies.
Replies above the protocol's 16 MiB frame limit return `too_large`; use
result limits, narrower patterns or grep's column limit to reduce the reply.

## Diff and apply

`diff CHECKOUT [SNAP] [-p]` compares the checkout now with its base capture (or
a snapshot); `--binary` prints the patch as `git diff --binary` does, with git
headers, full blob ids, modes and binary content, so `git apply` takes it whole,
also with `--cached`. Like git, it shows each side as git would store it: under
`core.autocrlf` or `text`/`eol` attributes, CRLF text becomes LF, and a change of
line endings only is no change. A changed path git would store through a filter
driver, `ident` or a `working-tree-encoding` is refused (code `unsupported`).
`apply CHECKOUT` snapshots the checkout temporarily and merges each changed
source path into the folder against the base; it releases the snapshot after a
successful, conflict-free apply and retains it on errors or conflicts. A path
the user left alone takes
the checkout's version, and a text file changed on both sides is merged the
way `git merge-file` merges it.

Where both sides changed the same lines, the file gets git-style conflict
markers (`<<<<<<< folder` … `>>>>>>> checkout NAME`, in your
`merge.conflictStyle`) and apply exits 1, asking you to resolve them and run it
again. Until the markers are gone, apply reports the file again and leaves it
alone. Once resolved, the checkout's later edits to that file merge against the
version that was marked, so the resolution is kept, across daemon restarts too.
Anything else (deletion against an edit, binary or symlink changes on both
sides) is reported as a conflict and left untouched. Applying twice is a no-op.
`--dry-run` writes nothing. Applies of several checkouts into one folder take
turns, each merging against what the previous one wrote.
