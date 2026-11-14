---
name: ovnfs
description: Isolated copy-on-write checkouts of any folder or git repository with the ovnfs CLI. Use when you need a second copy of a folder, for example a risky experiment, another branch or commit, checking a patch against a clean HEAD, reviewing a PR beside the user's work, or one workspace per parallel agent. Use it instead of `git worktree add`, `git clone` or `cp -r`. Covers `ovnfs new`, `run`, `diff`, `apply`, `snap`, `fork`, `rm`, daemon-side `grep`/`glob`, and path classes and rules.
---

# ovnfs

An ovnfs checkout is a copy-on-write view of a folder, served from one mount
(`~/checkouts/<name>` on macOS and Linux, drive `O:` on Windows). Opening one
takes about a second at any size and copies nothing. Writes stay in the
checkout, and the folder is written only when you `apply`. In a git repository
the checkout gets a private git directory, so commits made inside it stay
there. In a jj repository it is also a jj workspace of that repository, named
after the checkout: use `jj` inside it directly, never `jj git init`.

When you need a second copy of a folder, open an ovnfs checkout instead of
`git worktree add`, `git clone` or `cp -r`, unless the user asks for one of
those.

## Core loop

```sh
cd ~/src/app
cd "$(ovnfs new --name fix-auth)"   # stdout is only the path; the summary goes to stderr
# ...edit, build, test, commit freely...
ovnfs diff fix-auth -p              # source changes since the checkout opened
ovnfs apply fix-auth --dry-run      # preview the merge into ~/src/app
ovnfs apply fix-auth                # merge the changes into ~/src/app as uncommitted edits
ovnfs rm fix-auth                   # remove the checkout and unused snapshots
```

- Names: lowercase `[a-z0-9][a-z0-9._-]*`, up to 64 characters. Without
  `--name`, the checkout is named after the folder (`app`, then `app-2`, ...).
- `ovnfs ls` lists checkouts and snapshots of removed checkouts (TOUCHED is
  the last write through a checkout or to its git state); `ovnfs ls .` lists
  only checkouts of the current folder (a folder needs `.`, `..` or a slash);
  `ovnfs ls NAME` lists one checkout's snapshots.
- `ovnfs prune [DIR] [--idle 7d] [--no-diff] [--dry-run]` removes the
  checkouts matching every criterion given; `ovnfs ls` takes the same flags
  to preview. `rm` and `prune` remove unused snapshots; add
  `--sticky-snapshots` to keep them.
- Any verb starts the daemon if it isn't running. `ovnfs status` and
  `ovnfs stop` manage it, and `ovnfs restart` replaces it with one from the
  current binary without unmounting (NFS, SMB), so shells inside checkouts
  carry on; its log is `serve.log` in the state directory.

## Choosing what the checkout starts from

| Want | Command |
| --- | --- |
| The folder as it is now (uncommitted edits, untracked files, `node_modules`, build dirs) | `ovnfs new` |
| A commit or branch, without the folder's in-flight edits (e.g. to check your patch) | `ovnfs new --rev REV` |
| Only what is staged, without unstaged edits | `ovnfs new --rev HEAD --take :` |
| Old code with one path from another revision (e.g. a PR's test) | `ovnfs new --rev BASE --take pr-head:path/to/test` |
| An earlier snapshot | `ovnfs new --from SNAP` |

- `--rev` with a branch name attaches HEAD to that branch; a commit, tag or
  `HEAD` detaches it. Ignored files and build dirs stay available, so you
  don't need to reinstall dependencies.
- `--take SRC:PATH` can be repeated, and later ones win. SRC is `.` for the
  folder's working tree, empty for the index, or a revision. An empty PATH
  means the whole tree. Taken content becomes part of the base, so `diff` and
  `apply` never treat it as your change.
- `--rev` and `--take` cannot be combined with `--from`.

## Disposable runs

```sh
ovnfs run -- make test                 # open, run, remove; keeps the command's output and exit status
ovnfs run --rev HEAD -- go test ./...  # check the clean commit without touching the folder
ovnfs run --keep -- ./migrate.sh       # keep the checkout; its name and path go to stderr
```

The command runs in the checkout's subdirectory that matches your current
directory, with `OVNFS_CHECKOUT` set. The checkout is removed even when the
command fails or is interrupted.

## Bringing work back

- `ovnfs apply NAME` temporarily snapshots the checkout, then three-way merges
  each changed source path into the folder against the checkout's base. Commits are
  flattened: their changes land as uncommitted edits, and new files arrive
  untracked. Paths the user didn't touch take the checkout's version. Text
  files changed on both sides are merged as `git merge-file` would.
- On overlapping edits, apply writes git-style conflict markers
  (`<<<<<<< folder` … `>>>>>>> checkout NAME`) and exits 1. Resolve the
  markers in the folder, then run `ovnfs apply NAME` again; it refuses the file
  until the markers are gone. A deletion against an edit, or binary or symlink
  changes on both sides, is reported and the file is left untouched.
- Applying twice is a no-op, so you can keep working in the checkout and apply
  again later.
- `ovnfs apply NAME --branch B` turns the checkout's **commits** (not its
  uncommitted edits) into branch `B` in the folder's repository; the folder's
  files stay untouched. If `B` is checked out in the folder, apply
  fast-forwards it and updates only the changed index entries. A
  non-fast-forward, or staged changes on those paths, is refused; use another
  branch name.

## Snapshots, forks and rollback

```sh
ovnfs snap fix-auth -m before-refactor   # freeze the current state in O(1); --wait blocks until hashed
ovnfs fork fix-auth --name fix-auth-alt  # snapshot, then open a second checkout that diverges from it
ovnfs new --from s7 --name retry         # roll back = open a new checkout from a snapshot
ovnfs diff fix-auth s7                   # changes since a snapshot
ovnfs drop s7                            # release a snapshot
```

Snapshot IDs (`s7`) come from `ovnfs ls NAME`. Nothing rewrites a live
checkout or folder in place.

## Searching a checkout

`ovnfs grep` and `ovnfs glob` run inside the daemon and read the checkout
directly, without walking NFS/FUSE/SMB. On large trees, prefer them over
`rg`/`find` through the mount.

```sh
ovnfs grep 'func\s+New' fix-auth --glob '*.go' -C 2
ovnfs grep needle fix-auth --path src --mode filesWithMatches
ovnfs glob '*_test.go' fix-auth --limit 100
ovnfs --json glob '*.go' fix-auth --mtime  # newest first, with metadata
```

The target can be a checkout name, its mounted path, a directory below that
path, or any folder (defaults to `.`). Ignore files are honored, and `.git`
is always pruned.

## What a checkout contains: path classes

| Class | In the checkout | Seen by `diff`/`apply`/snapshots |
| --- | --- | --- |
| `source` | as captured at open | yes |
| `cache` (ignored dirs, `node_modules`, ...) | the live folder until the checkout writes there, minus files the folder changed after the open (a build redoes those); writes go to the checkout only | no |
| `local` (ignored files, secrets, files over 100 MB) | the live folder until the checkout writes there; writes go to the checkout only | no |
| `hidden` (`.DS_Store`, Python environments, ...) | absent | no |

- Build outputs and dependency installs inside a checkout never reach the
  folder, and `apply` never carries them back. To keep a generated file, make
  it `source`. A checkout that built or installed keeps its own copy of every
  path it wrote or deleted, so it no longer sees the folder's later builds or
  installs there.
- A checkout starts without the folder's Python environments (`.venv`, any
  directory holding `pyvenv.cfg`, `.tox`, `.nox`): their scripts and editable
  installs would run the folder's code. Create its own with `uv sync` (`uv run`
  does it on first use).
- Files the user adds to the folder after you open the checkout do not
  appear in it. Open a new checkout to see them.
- `ovnfs explain PATH` names the rule that decided a path's class, with its
  file and line.
- Override classes per folder in `.o2/ovnfs.yml` at the source root, per user
  in `~/.o2/ovnfs.yml`, or per checkout with `--rule 'CLASS PREDICATE'`. The
  first matching rule wins:

  ```yaml
  rules: |
    source  .env.local   # carry one ignored file
    local   data/**      # keep a dataset out of snapshots
  ```

  `ovnfs config set --append rules 'source .env.local'` adds a rule line to
  the folder's file without editing YAML (`--global` for the user's);
  `ovnfs config list` shows both files' settings.

## Scripting and multiple agents

- Give each agent or job a unique name, such as a role prefix plus a random
  suffix (`agent-auth-3f9c1a`). Opening an existing name with `--name` fails
  with `exists: checkout NAME already exists`. Never `rm` a checkout you
  didn't open, nor `prune` unless the user asks, because that deletes
  another run's work.
- `rm` on a missing checkout fails with `not_found`; treat that as already
  removed. Record the names you open so a crashed run can still clean up.
- `--json` (before the verb or among its arguments) prints the raw reply. For
  example, `ovnfs --json new` returns `{"checkout":{"name","path","base","git",...},"capture":{...},"warnings":[]}`.
  `ovnfs start --json` makes sure the daemon runs and prints its admin socket,
  for embedding.
- Output is coloured on a terminal only: a captured or piped stream is plain
  text. `--color auto|always|never` overrides it. Parse `--json`, never the
  human tables, which pad and colour by terminal width.
- To tell whether you are in a checkout, run `git config --get ovnfs.checkout`
  inside it (it prints the name; the folder's repository never gets this key),
  or check `$OVNFS_CHECKOUT` under `ovnfs run`.
- In a folder other agents share (the user's main checkout), don't run git
  commands that discard work or move HEAD: `stash`, `reset --hard`,
  `switch`/`checkout BRANCH`, or `restore`/`checkout -- PATHS`. Do that in a
  checkout of your own, where it affects only you.

## Pitfalls

- ovnfs isolates files, not processes. It is not a sandbox: absolute paths
  and symlinks still reach the host.
- Run `ovnfs new` from the user's folder, not from inside the mount. In a
  checkout's root, `new` forks that checkout (with a warning). It refuses
  folders below a checkout's root and folders holding the mount, and refuses
  `--rev`, `--take` or `--rule` on a checkout.
- Keep the daemon's state directory on the same volume as your repositories.
  Otherwise `new` warns, and files are copied instead of cloned.
- On macOS (NFSv3), changes to `cache`/`local` paths in the folder can take up
  to 60 s to appear in the checkout.
- Linux needs `fusermount3` (package `fuse3`). Windows uses SMB or WinFsp, and
  the mount must be a free drive letter.
- More: [checkouts](https://github.com/stencil-hq/ovnfs/blob/main/docs/checkouts.md),
  [configuration](https://github.com/stencil-hq/ovnfs/blob/main/docs/configuration.md),
  [remote checkouts](https://github.com/stencil-hq/ovnfs/blob/main/docs/remote.md) (`attach`, `pull`, `push`, `share`),
  [limitations](https://github.com/stencil-hq/ovnfs/blob/main/docs/limitations.md).
