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

`serve --memory SIZE` (`OVNFS_MEMORY`, default `1G`) and `serve --blob-cache
SIZE` (`OVNFS_BLOB_CACHE`, default `512M`) size the memory tiers described in
[internals](internals.md#memory-tiers); `start` passes the environment on.
`serve --fsync` (or `OVNFS_FSYNC=1`) makes a sync of a named memory-backed
file spill it to the upper and sync its host file. It is off by default: sync
on a memory-backed file otherwise does not write it to disk. NFS does not
forward application fsync calls, so this option cannot provide that guarantee
over NFS. It does not sync parent directories or make snapshots durable.
`serve --atime` (`OVNFS_ATIME=1`) lets the mount update access times; by
default it is mounted `noatime`, since the macOS NFS client otherwise sends a
SETATTR for every exec of a checkout binary. A restart applies the setting to
the mount the previous daemon left in place without unmounting it.

By default a repository gets a git remote per checkout of it;
`serve --no-git-remotes` (`OVNFS_GIT_REMOTES=0`) turns that off, see
[checkouts](checkouts.md#git-remotes).

## Classes

Opening a folder classifies every path into one of four classes:

| class | in the checkout | in a snapshot | on another machine |
| --- | --- | --- | --- |
| `source` | as captured at open | hashed: the snapshot's identity | sent by content id |
| `cache` | the folder, live until the checkout writes the path | not hashed | an empty directory, rebuilt there |
| `local` | the folder, live until the checkout writes the path | not hashed | absent |
| `hidden` | absent | — | absent |

Nothing is copied to make this view. A source file is the folder's own file
while its size, mtime and inode still match the capture, and its captured
content (from the store or the repository's objects) once they do not; a file
the user adds later stays out. Cache and local paths are the folder as it is
now. Writes in the checkout copy up into its own upper, so the folder is never
written: a cache or local path the checkout wrote or deleted is its own from
then on, no longer follows the folder, and is ignored by `diff` and `apply`.

Ignored directories (build output, scratch copies of repositories) are cache
or local, so every checkout inherits them live, and each whole-tree walk
(`rsync`, `find`, `rg --no-ignore`) in each checkout repeats their metadata
work. A `hidden DIR/` rule in `.o2/ovnfs.yml` leaves such a directory out of
checkouts, `--rev` ones included.

Python environments are hidden instead: their scripts and editable installs
name the folder by absolute path, so a live one would run the folder's
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
| `memory` | size | user file only: memory budget for written files |
| `fsync` | boolean | user file only: spill and sync memory-backed files on fsync |
| `node_cache` | size | user file only: estimated cache budget for derived ids and directory listings (default 512M) |
| `blob_cache` | size | user file only: memory budget for content-addressed blobs |
| `atime` | boolean | user file only: allow access time updates |
| `git_remotes` | boolean | user file only: give a repository a git remote `ovnfs/NAME` per checkout (default true) |

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
blob_cache: 512M
node_cache: 512M
atime: false
git_remotes: true
```

For each setting, the explicit `serve` flag wins over its environment variable,
which wins over the user file, which wins over the built-in default. The
environment variables are `OVNFS_MEMORY`, `OVNFS_FSYNC`, `OVNFS_BLOB_CACHE`,
`OVNFS_NODE_CACHE`, `OVNFS_ATIME` and `OVNFS_GIT_REMOTES`; `proto` has no environment variable.
Serve settings take effect when the daemon starts; restart it after changing
them.

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
`fsync`, `blob_cache`, `node_cache`, `atime` and `git_remotes`. A cache root may hold dots
(`cache.web/.next.key`) and matches its entry however it is spelled
(`cache.web/dist.rebuild` finds `./web/dist`). `rules`, `env.setup` and
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

The node cache's ownership index lives below `STATE/nodes.log.cache`; a clean
stop completes it for the next start, and it is reclaimed after a crash when the
state lock is next acquired. Keep state
on a disk-backed volume, since tmpfs uses RAM. `status` reports index bytes
separately from estimated resident node-cache bytes. Failed spill writes leave
existing handles usable and temporarily refuse fresh identity allocation while
resident usage exceeds the budget. Invalidation that frees enough cache resumes
normal listing admission; an unreadable authoritative index instead fails closed.
