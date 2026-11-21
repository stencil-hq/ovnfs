# Checkouts, snapshots and apply

## Git repositories

A checkout of a repository gets a private git directory: HEAD, refs, config and
the index are copies taken at open, and objects borrow the repository's through
alternates. Inside the checkout, `git status` matches the folder's, commits land
in the checkout, and they reach the user's repository only when
[`ovnfs land CHECKOUT`](#landing-commits-land) puts them on the folder's
checked-out branch with their files, `ovnfs apply CHECKOUT --branch NAME`
fetches them as a branch, or you fetch the checkout's
[git remote](#git-remotes) yourself.

If NAME is checked out in the folder, `apply --branch` fast-forwards it and
updates only the index entries changed by those commits, never the working-tree
files, which then read as modified: use `land` to move the files too.
Staged changes on those paths or a non-fast-forward are refused; use another
branch name instead. Unrelated staged work stays staged. `--dry-run` previews
this checked-out-branch update without writing. A folder whose repository is a
partial clone takes the commits as a pack of what its refs do not reach; when
the checkout's commits build on history fetched inside it, apply and land name
the remote to fetch in the folder first.

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
snapshots carry them, and ovnfs never writes the folder's `.git/modules`; only
hooks written through a hooks link (see below) land there. A fork
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

### Git hooks

With `git_hooks: true` in `~/.o2/ovnfs.yml` (or `serve --git-hooks`,
`OVNFS_GIT_HOOKS=1`), a checkout's private git directory links `hooks` to the
folder's hooks directory (`.git/hooks`, or the common git directory's when the
folder is a linked worktree), so `pre-commit`, `commit-msg`, `post-checkout`
and the other hooks run in a checkout as they do in a linked worktree of the
folder. An absorbed submodule's private git directory links to the
submodule's own `.git/modules/<name>/hooks`. A `core.hooksPath` set globally,
or in the repository before the checkout was opened, takes precedence, as it
does in the folder. ovnfs tries the link for every git directory and posts one
daemon notice per checkout listing each link it couldn't make; those git
directories stay without hooks and the rest are linked. A git directory whose
source hooks can't be examined (a permission or I/O error) gets no new link and
no notice; a link it already has stays.

The link works both ways: editing a hook in the folder
changes it for every checkout, and a hook installer run in a checkout
(`pre-commit install`, husky v4, `git lfs install`, anything writing to
`$(git rev-parse --git-common-dir)/hooks`) writes the folder's hooks for every
checkout, as it would from a linked worktree. Through the link, a global hook
that chains to `$(git rev-parse --git-common-dir)/hooks/<name>` reaches the
folder's hooks from a checkout. `rm` drops only the link: the
private git directory moves to `trash/` and is deleted later, and neither step
follows the link. Git commands that ovnfs runs itself run with hooks off.

A folder without a hooks directory (a file in its place
counts as none) gets no link, and an existing `hooks` entry in a private git
directory is never replaced. Checkouts without the link, including those
opened before the setting was on or before the folder had hooks, gain it when
the daemon next opens their registry with the setting on, provided the folder
is still the top of a git repository, without changing their last-touched
time; ovnfs removes its own link the same way once the folder's hooks
directory is gone, is not a directory, or can never resolve (a symlink loop, a
path through a file). With the setting on, snapshots
keep no `hooks` symlink, so `fork` and `new --from` link the new checkout to
the folder's hooks, never to a link you made yourself; for hooks of a
checkout's own, use a real `hooks` directory or `core.hooksPath` in its
config.

With the setting off, the default, ovnfs links nothing. A link a checkout
already has stays, and forks and snapshots copy it as they copy any symlink in
a private git directory (on Windows, a junction is not copied). To drop a link,
delete `hooks` in the checkout's private git directory, which
`git rev-parse --git-common-dir` prints inside the checkout. A submodule's link
sits in its own private git directory, which `git rev-parse --git-dir` prints
inside the submodule.

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
`new --base CHECKOUT`. Removing the checkout (`rm`, `prune`, the end of `run`)
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
Hidden rules omit paths even when REV tracks them, using REV's path types and
tracked status. Ignore rules come from REV, not the folder's dirty `.gitignore`
files, including when deciding which ignored folder paths to keep or hide.
The private Git index keeps hidden tracked entries with `skip-worktree`,
so `git status` does not report them deleted and ordinary commits preserve them.
This filters the initial checkout and its ovnfs base, not files a later
`git switch` may check out. Git history still contains the hidden files;
`hidden` is not an access-control mechanism.

The source folder and its git state are never changed, except that LFS objects
REV needs may be downloaded into the repository's LFS storage (see below).
`--rev` cannot be combined
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

Files Git LFS tracks show the objects their pointers name, as `git checkout`
writes them: where REV's attributes (or `GIT_ATTR_SOURCE`'s and `attr.tree`'s)
set `filter=lfs`, the repository's `lfs` filter runs git-lfs without `--skip`
(not after `git lfs install --skip-smudge`), `GIT_LFS_SKIP_SMUDGE` is not set
in the daemon's environment, and `lfs.fetchinclude` and `lfs.fetchexclude`
(from git's configuration, else the folder's `.lfsconfig`, else the index's or
`HEAD`'s, as git-lfs reads them) allow the path.
Listing them reads no object: each is opened from the repository's LFS
storage on first read, or produced by `git lfs smudge`, which downloads a
missing object into that storage; output that is not the object fails the
read (EIO, with a notice). They show the mtime one second before the
checkout opened, so `git status` trusts them unread. Trees, `diff` and the
private index keep the pointers git stores, and content that is a pointer's
object compares as that pointer, as git's clean filter stores it: rewriting
an object unchanged is no change, and `apply` sees the folder's copy of it
as unchanged.

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
Content stays lazy, Git LFS objects included (as with `--rev`; a take from
the index reads the index's attributes first), and the
private git index is unchanged: it still comes from REV with `--rev`, or from
the folder otherwise. Git therefore shows taken
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
`new --base CHECKOUT` on what a live checkout started from (its capture, which
`--base` also takes by the id `ls --json` shows), with the git state that
checkout started from, however the folder and the checkout have changed since:
an untouched baseline to build or test beside it. `rm` removes a checkout and
its snapshots unless another checkout or snapshot uses them.
`rm --sticky-snapshots` keeps them, even if a later checkout reuses the name.
When upgrading older state, snapshots a live checkout made since it opened
still belong to it. Both `rm` and `prune` keep older snapshots whose owner
cannot be established. Use `drop` to release a snapshot explicitly. Nothing
replaces the content of a live folder: rollback means a new checkout from a snapshot.

`rm` discards whatever the checkout holds that was not applied, without
asking, and prints `removed NAME`. The one thing it refuses to discard is a
commit made in the checkout that nothing else keeps: no ref of the folder's
repository reaches it (as after `land`, `apply --branch` or a merge of the
checkout's [git remote](#git-remotes)), and the checkout did not push it. The
message counts those commits and names the newest; `rm --force` removes the
checkout anyway. `prune`, the end of `run` and the socket API's `rm` without
`keep_commits` do not check.

`new` (and `run`) on a checkout's own folder forks that checkout: capturing it
would make the daemon serve the new checkout by reading through its own mount,
which can deadlock the mount. `new` refuses the mount itself, a folder below a
checkout's root, a folder (or repository) holding the mount, and `--rev`,
`--take` or `--rule` with a checkout. These folders are told by file identity,
so a symlink, another letter case or a bind mount does not get around the rule.

## Output for scripts

`new`, `fork` and `snap` print the one value they produce, the checkout's path
or the snapshot's id, alone on stdout and last. On a terminal a summary of what
was opened precedes it on stderr; when stderr is captured, only warnings do, so
both `P=$(ovnfs new)` and `ovnfs new 2>&1 | tail -1` yield the path. `rm`,
`apply`, `land` and `pull` say what they did, also when that is nothing.
`--json` prints the whole reply instead of any of this. Tables show times as
one word (`2026-11-18T05:52`), so their columns split on blanks.

## Listing and cleanup: `ls` and `prune`

`ls` shows each checkout's TOUCHED time: its last write through its folder or
to its private git directory, else its creation. `ls DIR` lists only the
checkouts of DIR: those opened on the folder, on a folder inside it, or on the
folder holding it; inside the mount, those sharing the source of the checkout
DIR is in. DIR needs a path spelling (`.`, `..`, a slash), since a bare word
names a checkout, whose snapshots `ls NAME` lists. When no checkout has that
name, `ls NAME` lists the retained snapshots made under it. Plain `ls` lists
snapshots without a known live owner separately, even if their name was reused.

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
including memory-resident writes, frozen layers and the cache and local
paths as the checkout's cache snapshot fixes them.
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
`node_modules` are skipped, and `.git` is pruned. `--no-ignore`, `--hidden`
and `--node-modules` override the first three defaults; a pattern naming
`node_modules` also enables it. Hidden-class paths remain absent regardless
of these flags. Symlink directories are not descended.

To discover repository markers, including those under ignored directories, run
`ovnfs glob '**/.git' app --git-markers --hidden --no-ignore`. Omit `--type`:
a marker can be a directory or a gitfile. A root `.git` marks the checkout
itself; only paths below that identify nested repositories. Glob lists the
markers without walking their directories or following gitfile pointers.

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

`diff CHECKOUT [SNAP]` compares the checkout now with its base capture (or a
snapshot) and lists the changed source paths, one `STATUS<TAB>PATH` per line.
`--stat` adds each path's added and deleted lines as a third column
(`+3 -1`, or `binary`) and totals them on stderr.

`-p` prints a git-format patch instead of the list, and nothing else, so it
pipes into `git apply` or a file: `diff --git` headers, the modes of added,
deleted and re-moded files, and text hunks. It carries no blob ids and only
names a binary file, which `git apply` then refuses. `--binary` prints the
patch as `git diff --binary` does, with full blob ids and binary content, so
`git apply` takes it whole, also with `--cached`. Like git, it shows each side
as git would store it: under `core.autocrlf` or `text`/`eol` attributes, CRLF
text becomes LF, and a change of line endings only is no change. A changed path
git would store through a filter driver, `ident` or a `working-tree-encoding`
is refused (code `unsupported`).

`diff CHECKOUT [SNAP] -- PATH ...` keeps the list, the counts or the patch to
those files and folders. Paths count from the checkout's root wherever the
command runs, unlike git's; when they match no change while others exist, diff
says so on stderr.

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

`--atomic` writes nothing at all when a path would conflict: apply first
merges every path without writing, and on a conflict lists them and exits 1,
leaving the folder as it was. Without it, the paths that merge are written and
the others get their markers. (Should the folder change between that check and
the writing, the new conflict gets its markers like any other.) Where a pull
can bring the folder's files into the checkout, a refused or dry-run apply says
so: the conflict can then be resolved there instead.

Apply remembers the version of each path the folder took. A later apply merges
against that version instead of the capture, so it brings only what the
checkout changed since: a line applied and then edited again does not conflict
with itself, and a file you changed back in the folder stays as you left it.

## Landing commits: `land`

`land CHECKOUT [--dry-run]` puts the commits made in a checkout on the branch
checked out in its folder, as committing there would have: the branch
fast-forwards to the checkout's HEAD, its index follows, and the checkout's
files for the paths those commits changed merge into the folder, so the
folder's uncommitted edits stay on top. `git status` there then shows those
edits, and whatever the checkout itself left uncommitted in the same files,
which comes along as it is. A path changed in the checkout outside those
commits stays in the checkout; land counts them, and `apply` brings them as
uncommitted edits.

Land refuses before it writes anything: when the folder's HEAD is detached
(`apply --branch NAME` then brings the commits as a branch), when the branch
holds commits the checkout's are not based on (rebase inside the checkout, as
the message spells out), when the commits' paths have staged changes, and when
a file would conflict with the folder's uncommitted edits. For that last case
`land --markers` merges anyway and writes the conflict markers into the folder,
as apply does, leaving the branch where it is: resolve them and land again.
Where the checkout can [pull](#pulling-the-folder-in-pull), pulling the
folder's edits into it resolves the conflict there instead.

The commits are copied before the files are merged, and the branch moves last,
so a failure leaves at most merged files as uncommitted edits, which landing
again completes. `apply CHECKOUT --branch NAME` remains for the other cases: it
creates or fast-forwards a branch that is not checked out, touching no file.

## Pulling the folder in: `pull`

`pull CHECKOUT [--dry-run]` is apply in reverse: it merges into the checkout
what the folder changed since the checkout opened, or last pulled. Use it when
the folder moved on while the checkout was worked in, and above all when
`apply --dry-run` reports conflicts: the conflict markers then land in the
checkout (`<<<<<<< checkout NAME` … `>>>>>>> folder`), not in the user's files.
Resolve them there, and `apply`; until they are gone, apply leaves those paths
out and says so.

A path the checkout left alone takes the folder's version, text changed on both
sides is merged, and what cannot be merged is reported and left untouched.
Only source paths move, as with apply: the checkout's build directories and
ignored files stay its own. The folder's files as pulled become the checkout's
base for the paths they brought in, so `diff` lists, and `apply` merges, only
what the checkout changed beyond them.

Pull merges files, not history: the checkout's HEAD and index stay where they
were. When the folder's HEAD has moved, pull prints the git command that
follows it inside the checkout (`git reset FETCH_HEAD` after a fetch, or a
rebase when the checkout has commits of its own). A checkout opened with
`--rev` or `--take` refuses a pull: its files started from a revision, so the
folder's are not newer versions of them, and its commits follow the folder's
with a rebase instead.

On NFS the daemon refreshes its mount's client for the paths a pull changed
(see [platforms](platforms.md#nfs)), so the checkout shows them at once.
`pull CHECKOUT --from HOST` instead pulls from a machine the checkout is
[attached](remote.md#pull) to; a checkout attached to one machine pulls from
it without `--from`.
