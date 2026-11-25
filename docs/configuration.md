# Configuration

## Where the daemon lives

| | state | mount |
| --- | --- | --- |
| macOS, Linux | `~/.local/state/ovnfs` (or `$XDG_STATE_HOME/ovnfs`) | `~/checkouts` |
| Windows | `%LOCALAPPDATA%\ovnfs` | drive `O:` |

On Linux, distribution FUSE confinement only allows mounts below `$HOME`,
`/mnt`, `/media` and `/tmp`; on Windows the mount must be a free drive letter.
`state` and `mount` in `~/.o2/ovnfs.yml` replace the defaults,
`OVNFS_STATE`/`OVNFS_MOUNT` replace those, and `--state`/`--mount` win over
all. Keep state on the volume holding your repositories: `new` warns when they
differ, since files are then copied instead of cloned.

Human output is coloured on a terminal and plain everywhere else, so a pipe,
a log or a redirect gets the text a script parses. `--color when` overrides
the choice: `auto` (the default), `always` or `never`; `NO_COLOR` and
`CLICOLOR_FORCE` are honoured. Colours are downsampled to the terminal's
profile, so 16-colour terminals and pagers see no stray escapes. `diff -p` and
`--binary` patches come out with the usual green additions, red deletions and
cyan hunk headers, and every verb that prints a table aligns it by visible
width.

`serve --memory SIZE` (`OVNFS_MEMORY`, default `64M`) and `serve --blob-cache
SIZE` (`OVNFS_BLOB_CACHE`, default `16M`) size the memory tiers described in
[internals](internals.md#memory-tiers); `start` passes the environment on.
An individual written file can stay in memory up to half the `--memory`
budget (32 MiB by default). Past 256 KiB of capacity it can grow only into
spare space, not by evicting other files. Raising the budget can avoid disk
writes to larger active files, but puts more unspilled data at risk on a crash.
`serve --fsync` (or `OVNFS_FSYNC=1`) makes a handle sync on FUSE, SMB or
WinFsp spill a named memory-backed file to the upper and sync its host file,
and makes NFS [stable writes and COMMIT](platforms.md#nfs) sync theirs.
It is off by default and does not make snapshots durable. Off, an application
`fsync` or `close` on any mount is no durability barrier: a crash of the
daemon or the host can lose data the call acknowledged.

With the memory tier on, a daemon crash or `kill -9` loses what the daemon
had not yet put on the host: written files that had not spilled, and the name
changes (unlink, rename and hard link of files on disk, mkdir and rmdir of
directories no lower layer has) it had queued for the host (see
[internals](internals.md#memory-tiers)). Removed names can then reappear and
renamed files show their old names, but every name shows a state it had since
the last flush, which `snap`, `fork`, `diff` and a planned stop each do. File
contents are per file, not per name: data written through a descriptor reaches
the file whatever happened to its names. `--memory 0` keeps every change
synchronous. A queued change the host refuses is retried every second, and
`ovnfs status` shows a `queue error` row while one waits.
`serve --atime` (`OVNFS_ATIME=1`) lets the mount update access times; by
default it is mounted `noatime`, since the macOS NFS client otherwise sends a
SETATTR for every exec of a checkout binary. A restart applies the setting to
the existing mount; macOS NFS uses [mount reconciliation](platforms.md#nfs).

`serve --idle-exit DUR` (`OVNFS_IDLE_EXIT`, default `10m`) unmounts and
exits a daemon that has had no checkouts and no admin connection for that
long, as a daemon auto-started for a test or under a scratch `HOME` ends up;
the next command starts another. `0` keeps the daemon running.

`serve --nfs-io-size SIZE` (`OVNFS_NFS_IO_SIZE`, user key `nfs_io_size`)
sets NFS read/write transfers, default `128K`: powers of two from `4K` to `1M`
are accepted; `0` uses the default. It applies to Unix-socket and TCP NFS mounts,
not SMB. A restart replaces a macOS NFS mount whose negotiated options differ.

On macOS, `serve --nfs-nointr` (`OVNFS_NFS_NOINTR=1`, user key
`nfs_nointr: true`) opts into `hard,nointr` on both Unix-socket and TCP NFS
mounts. The default stays interruptible. To switch policies, stop checkout
clients, leave the mount, then `ovnfs stop` and `ovnfs start`. Changing the
policy with `restart` can force a remount. See [what happens during a
stall](platforms.md#when-the-nfs-daemon-stops-answering).

On macOS, the NFS client keeps extended attributes in a `._` file beside every
file and directory created in a checkout, and the daemon stores none of them:
attributes set in a checkout (`xattr -w`, Finder tags, quarantine) last only
while the client caches them, and creating files costs the daemon less.
`serve --nfs-xattrs` (`OVNFS_NFS_XATTRS=1`, user key `nfs_xattrs: true`)
keeps them; see [AppleDouble files](platforms.md#nfs).

`ovnfs tune` measures isolated mount configurations and asks before applying a
balanced result. Add `--project DIR` for a real Cargo workspace; see
[the tuning methodology](development.md#balanced-configuration-tuning).

On macOS, `proto: smb` in the user file selects SMB's client write caching on
the next `restart`; NFS remains the default. See the [frontend tradeoffs](platforms.md#smb),
including SMB's lack of hard links and durable open handles. Changing protocols
forces a new mount rather than retaining one that speaks the old protocol.

By default a repository gets a git remote per checkout of it;
`serve --no-git-remotes` (`OVNFS_GIT_REMOTES=0`) turns that off, see
[checkouts](checkouts.md#git-remotes). A checkout's private git directory has
no hooks by default; `serve --git-hooks` (`OVNFS_GIT_HOOKS=1`) links it to the
folder's hooks, as a linked worktree uses them, see
[checkouts](checkouts.md#git-hooks).

## Classes

Opening a folder classifies every path into one of four classes:

| class | in the checkout | in a snapshot | on another machine |
| --- | --- | --- | --- |
| `source` | as captured at open | hashed: the snapshot's identity | sent by content id |
| `cache` | the folder's own files, as far as it has not changed them since the checkout opened; copy-on-write | not hashed | an empty directory, rebuilt there |
| `local` | the folder's own files, live until written; copied at open with `local: frozen`; copy-on-write | not hashed | absent |
| `hidden` | absent | — | absent |

Nothing is copied to make the source view. A source file is the folder's own
file while its size, mtime and inode still match the capture, and its
captured content (from the store or the repository's objects) once they do
not; a file the user adds later stays out. If the folder truncates the file
after a checkout reader opened it, the reader continues at the same offset
in the captured content. Copy-up uses the same check.

**Local** paths (ignored files, secrets such as `.env`, files over 100 MB)
are **virtual by default** (`local: virtual`): nothing is copied or
cloned when a checkout opens. If the checkout has not written the path, reads
forward straight to the folder with no locks and no fence; host edits may show
through until the checkout writes its own copy (copy-on-write into its upper).
Local paths stay out of snapshots and transfers.

To opt into isolated, frozen local copies instead, run
`ovnfs config set local frozen` then `ovnfs restart` (or
`serve --local frozen` / `OVNFS_LOCAL=frozen`). New checkouts then
copy local roots below `STATE/bases/BASE/caches/` in the background — sharing
blocks where the volume can (APFS `clonefile`, reflinks on Btrfs and XFS,
block cloning on ReFS and Dev Drive), and copying bytes with a warning
elsewhere — so an `.env` edited on the host stays unchanged in open checkouts.
Existing checkouts keep the mode they opened with.

**Cache** paths (ignored directories: build output, `node_modules`) are read
where they are. Nothing of them is copied, indexed or deleted again, so a
`target/` of 500,000 files costs a checkout what the checkout touches of it,
on any filesystem, and creating or removing a checkout takes the same time
whatever the folder has built. The snapshot is a fence in time, the instant
the checkout opened:

- a file is there while the folder has not changed it since. That is told
  by its change time, which content, metadata and renames move and, unlike
  the mtime, no tool can set back;
- one the folder rewrites, replaces or removes afterwards disappears from
  the checkout, as an evicted cache entry would. It never changes there;
- nothing the folder creates afterwards appears.

So a host build writing the folder never hands a checkout output newer than
the checkout's sources, which build tools that trust mtimes, such as cargo,
would mistake for their own; what the host build takes away, the checkout's
build makes again, in the checkout. Two things follow from reading in place.
A build in the folder can take an artifact away under a build running in a
checkout: the tool then fails to open a file it saw a moment ago, and
running it again rebuilds the file. And reinstalling dependencies in the
folder removes the replaced packages from the checkouts already open, which
then install their own.

A checkout opened after the folder changed at those paths gets a newer
snapshot; one opened while they did not change, as the folder watcher reports
it, shares the newest. A snapshot goes once no checkout uses it.

Cache paths are served only from a folder on the state's volume (`--state`),
whose files the checkout's file ids can name; a folder elsewhere gets its
cache directories empty, as on another machine, and they are rebuilt in the
checkout, while local paths are copied whole. Writes in the checkout copy up
into its own upper, so the folder is never written: a cache or local path the
checkout wrote or deleted is its own, and is ignored by `diff` and `apply`.

Ignored directories (build output, scratch copies of repositories) are cache
or local, so each whole-tree walk (`rsync`, `find`, `rg --no-ignore`) in each
checkout repeats their metadata work. A `hidden DIR/` rule in `.o2/ovnfs.yml`
leaves such a directory out of checkouts, `--rev` ones included.

Python environments are hidden instead: their scripts and editable installs
name the folder by absolute path, so a copied one would run the folder's
interpreter and code. A checkout starts without them and creates its own
(`uv sync`, or `uv run` on first use).

## Rules

Only `source` bytes ever leave the machine. Rules are lines of
`<class> <predicate>`; the first match wins, and a directory that gets a
non-source class is decided as one unit. The built-ins come last:

```
hidden   .DS_Store | ._* | Thumbs.db | desktop.ini
local    secret
hidden   !tracked & (venvdir | dir & (.venv | .tox | .nox))
cache    cachedir | dir & (node_modules | __pycache__ | .next | .nuxt | .turbo | .gradle | .pytest_cache | .mypy_cache | .ruff_cache)
cache    ignored & dir
local    ignored
local    size > 100MB
source   *
```

Above them, in order of precedence: `--rule` lines, the folder's
`.o2/ovnfs.yml`, then `~/.o2/ovnfs.yml`.

`ovnfs explain [DIR|CHECKOUT] PATH` names the deciding rule and its file and
line. `ovnfs plan [DIR|CHECKOUT]` shows what a launch elsewhere would take from
a git origin by content id, send from this machine, rebuild, and leave behind,
and warns when a secret-looking path is `source`. The resolved rules are frozen
with each checkout (`lock.json` in its base).

### Predicates

| predicate | holds for |
| --- | --- |
| `*.log`, `Makefile` | a glob without `/`: the base name, at any depth |
| `/build`, `docs/*.md` | a glob with a leading or inner `/`: the path from the source root |
| `out/` | a trailing `/`: directories only |
| `**` | zero or more path components; one or more at the end of a glob |
| `tracked`, `untracked`, `ignored` | in a git index; neither tracked nor ignored; excluded by gitignore rules |
| `dir`, `cachedir`, `venvdir` | a directory; a directory holding `CACHEDIR.TAG`; a directory holding `pyvenv.cfg` (a Python virtual environment) |
| `secret` | credential names: `.env`, `.env.*` (not `.example`, `.sample`, `.template`, `.dist`), `.netrc`, `.npmrc`, `.pypirc`, `.git-credentials`, `credentials`, `credentials.json`, `credentials.toml`, `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`, `*.pem`, `*.key`, `*.p12`, `*.pfx`; directories `.ssh`, `.gnupg`, `.aws`, `.kube`, `.docker` |
| `size > 100MB` | a file (never a directory) compared with `>`, `>=`, `<` or `<=` against an integer in `B`, `KB`, `MB`, `GB`, `TB` (powers of 1000) or `KiB`, `MiB`, `GiB`, `TiB`, any case |

Globs use `*`, `?` and `[…]` (`[!…]` negates). `!` binds tightest, then `&`,
then `|`; parentheses group. A glob matches a path itself: what lies below a
directory follows from the directory, since a non-source directory decides its
whole subtree.

## Git

Git is a fact source, never a requirement. `.gitignore` files apply in any
folder, as with `rg --no-require-git`; inside a repository git's own rules
apply (`info/exclude`, `core.excludesFile`), per repository for nested ones.
Tracked files whose stat data matches the index take their ids from it without
being read, and a per-folder stat cache does the same for everything else, so
reopening a large folder hashes only what changed.

Files git converts between blob and working tree are read instead, since the
blob is not their content: filter drivers such as LFS, `ident`,
`working-tree-encoding`, and end-of-line conversion (`text`, `eol`,
`core.autocrlf`) unless the blob's size equals the file's. The walk decides
this from each repository's gitattributes and configuration, and for
end-of-line candidates from the blob's size in the object database.
The first capture of an LFS repository therefore stores its LFS content, and on
Windows with Git's default `core.autocrlf=true` the first capture hashes every
text file.

## Configuration file

`.o2/ovnfs.yml` at the source root (the repository's top level when the folder
is inside one, else the folder itself) configures that folder's checkouts.
`~/.o2/ovnfs.yml` in the daemon user's home has the same schema, applies to
every folder and ranks below it; it alone may also set the daemon's folders and
serve defaults.
Both are optional. They are read when a checkout opens (`new`, `run`,
`attach DIR`) and frozen into its lock, so an edit affects only checkouts
opened afterwards; `explain` and `plan` read a folder's files as they are now
and a checkout's from its lock.

```yaml
rules: |
  source  .env.local          # carry one ignored file
  local   data/**             # keep the dataset here
  hidden  *.log & !tracked
cache:
  node_modules: { rebuild: bun install --frozen-lockfile, key: [bun.lock] }
  web/.next: { rebuild: bun run build, key: [package.json] }
env:
  image: linux-dev
  setup: [bun install]
```

| key | type | meaning |
| --- | --- | --- |
| `rules` | block of rule lines | classification rules, checked before the user file's and the built-ins |
| `cache.ROOT.rebuild` | string | command that recreates cache root ROOT on another machine |
| `cache.ROOT.key` | list of paths | files whose content identifies ROOT's contents |
| `env.image` | string | environment the checkout should run in |
| `env.setup` | list of strings | commands that prepare that environment |
| `state` | path | `~/.o2/ovnfs.yml` only: where the daemon keeps its state |
| `mount` | path | `~/.o2/ovnfs.yml` only: where checkouts appear |
| `proto` | string | user file only: default file server protocol (`nfs`, `smb`, `fuse`, or `winfsp`) |
| `memory` | size | user file only: memory budget for written files (default 64M) |
| `fsync` | boolean | user file only: spill and sync files on fsync, NFS stable writes and COMMIT |
| `blob_cache` | size | user file only: memory budget for content-addressed blobs (default 16M) |
| `atime` | boolean | user file only: allow access time updates |
| `nfs_io_size` | size | user file only: NFS transfer size, power of two from 4K to 1M (default 128K; 0 uses default) |
| `nfs_nointr` | boolean | user file only: disable signal interruption on macOS NFS (default false) |
| `nfs_xattrs` | boolean | user file only: keep the `._` files macOS NFS clients store extended attributes in (default false) |
| `git_remotes` | boolean | user file only: give a repository a git remote `ovnfs/NAME` per checkout (default true) |
| `git_hooks` | boolean | user file only: link each checkout's private git directory to the repository's hooks (default false) |
| `local` | string | user file only: how new checkouts serve `local` paths — `virtual` (default, read live from the folder until written) or `frozen` (clone/copy at open) |

`rules` is a block scalar (`rules: |`) with one `<class> <predicate>` per line.
Blank lines and lines starting with `#` are skipped; `#` after whitespace
starts a comment, so `a#b` stays a glob. `explain` names a rule by file and
line (`.o2/ovnfs.yml:3`).

`state` and `mount` take an absolute path or one starting with `~/`, and are
read by every `ovnfs` command before it reaches the daemon, so changing them
points the commands at another daemon; `ovnfs stop` the old one first. For
example, to keep everything on a data volume:

```yaml
state: /mnt/data/.ovnfs
mount: /mnt/data/checkouts
```

The same user file can set defaults for `serve` (including the background
daemon launched by `start`):

```yaml
proto: smb
memory: 2G
fsync: true
blob_cache: 64M
atime: false
git_remotes: true
git_hooks: false
```

For each setting, the explicit `serve` flag wins over its environment variable,
which wins over the user file, which wins over the built-in default. The
environment variables are `OVNFS_MEMORY`, `OVNFS_FSYNC`, `OVNFS_BLOB_CACHE`,
`OVNFS_ATIME`, `OVNFS_NFS_IO_SIZE`, `OVNFS_NFS_NOINTR`, `OVNFS_NFS_XATTRS`,
`OVNFS_GIT_REMOTES`, `OVNFS_GIT_HOOKS` and `OVNFS_IDLE_EXIT`;
`proto` has no environment variable.
Serve settings take effect the next time the daemon starts.

### `ovnfs config`

`ovnfs config` reads and writes these files by key, as `git config` does its
files:

```sh
ovnfs config set --append rules 'source .env.local'    # add a rule line to the folder's file
ovnfs config set cache.node_modules.rebuild 'bun install --frozen-lockfile'
ovnfs config set env.setup 'bun install' 'bun run build'
ovnfs config set proto smb                              # a daemon setting: ~/.o2/ovnfs.yml
ovnfs config get rules --show-origin
ovnfs config list
ovnfs config unset cache.node_modules.rebuild
ovnfs config edit --global
```

The keys are `rules`, `cache.ROOT.rebuild`, `cache.ROOT.key`, `env.image`,
`env.setup` and the daemon settings `state`, `mount`, `proto`, `memory`,
`fsync`, `blob_cache`, `atime`, `nfs_io_size`, `nfs_nointr`, `nfs_xattrs`,
`git_remotes`, `git_hooks` and `local`.
A cache root may hold dots (`cache.web/.next.key`) and matches its entry
however it is spelled (`cache.web/dist.rebuild` finds `./web/dist`).
`rules`, `env.setup` and
`cache.ROOT.key` take several values, one rule line or item each, and
`set --append` adds to them; every other key takes one.

`--local` picks the `.o2/ovnfs.yml` at the source root of the current
directory, `--global` picks `~/.o2/ovnfs.yml`. Without either, `list` prints
the settings of both as `KEY=VALUE`, the folder's first, and `get` prints a
key's values as a checkout opened here would take them: rules from both files,
a `cache.ROOT` field from the first file with an entry for ROOT, any other key
from the first file that sets it. Writes go to the folder's file, except daemon
settings, which go to the user's. `--show-origin` prefixes each value with its
file and a tab; `--json` prints `[{"file", "key", "value"}, …]`.

`set` and `unset` keep comments but rewrite indentation and blank lines, and
refuse, writing nothing, a result that would not load, such as an invalid
predicate; `unset` also removes the sections it empties. `edit` opens the file
in `$VISUAL` or `$EDITOR` (default `vi`, `notepad` on Windows), which creates
it if missing, then reports any error in it. As with git, `get` exits 1 when
the key is unset and `unset` exits 5 when the file does not set it. `config`
needs no loadable `~/.o2/ovnfs.yml` to start, so `ovnfs config edit --global`
repairs one that makes every other verb fail.

### Cache and env

A `cache` key names a cache root: a path from the source root (`web/.next`),
or a bare name (`node_modules`) for every root with that base name; an exact
path wins. An entry does not make anything a cache path, the rules do; it says
how a cache root comes back on another machine. `plan` lists each cache root
with `rebuild: CMD` or `starts empty`, plus a hash of its `key` files, which
are relative to the root's parent directory (`web/package.json` above) and must
stay inside the source; a missing key file is hashed as missing. ovnfs does not
run `rebuild` itself: cache roots arrive elsewhere as empty directories.

`env` is recorded in the lock, and so changes the lock hash `plan` prints, but
ovnfs does not act on it.

### Precedence and errors

Rules concatenate (`--rule`, folder file, user file, built-ins) and the first
match wins. A folder `cache` entry replaces the user entry with the same key;
`env.image` and `env.setup` each replace the user's when set. A folder whose
source root is the home directory reads `~/.o2/ovnfs.yml` once, as its own
file. Parsing is strict: unknown keys, a list for `rules`, wrong types and
invalid predicates fail `new`, `explain` and `plan` with `FILE:LINE: message`,
e.g. `.o2/ovnfs.yml:3: unknown key "env.imge"`.
