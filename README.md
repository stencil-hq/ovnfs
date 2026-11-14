# ovnfs

[![ci](https://github.com/stencil-hq/ovnfs/actions/workflows/ci.yml/badge.svg)](https://github.com/stencil-hq/ovnfs/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/stencil-hq/ovnfs.svg)](https://pkg.go.dev/github.com/stencil-hq/ovnfs)

Checkouts for agents: isolated, copy-on-write views of any folder, a git
repository or not, opened without copying it. Agents work in
the checkout; you review with `diff` and merge back with a three-way `apply`.
Checkouts can be snapshotted, forked, and attached to another machine, where
their content arrives on first read.

One daemon serves every checkout as a folder of a single mount: NFSv3 on
macOS, FUSE on Linux, SMB or WinFsp on Windows. No root, and no kernel
extension beyond WinFsp where you install it.

## Install

Linux and macOS:

```sh
curl -fsSL https://stencil.so/ovnfs.sh | sh
```

Windows (PowerShell):

```powershell
irm https://stencil.so/ovnfs.ps1 | iex
```

The installers download the latest release binary (Linux, macOS and Windows on
amd64 and arm64) from `build.stencil.so`, check its SHA-256 and put it in
`~/.local/bin`, or on Windows in `%LOCALAPPDATA%\Programs\ovnfs`, added to the
user PATH. `OVNFS_INSTALL_DIR` picks another folder, `--version v0.2.3`
(`-Version` on Windows) another release. With Go (see `go.mod`):

```sh
go install github.com/stencil-hq/ovnfs@latest
```

`go build -o bin/ovnfs .` builds a clone. ovnfs reads and writes git
repositories itself, so it needs no git installation (fetches use git when it
is on the PATH); on Linux it needs
`fusermount3` (the `fuse3` package). `ovnfs version` prints the build;
`ovnfs update` replaces it with the latest, through `go install` when that
built it, and `ovnfs restart` then moves the daemon onto it.
[GitHub releases](https://github.com/stencil-hq/ovnfs/releases) carry the same
binaries as archives.

## Quick start

```sh
cd ~/src/app
cd "$(ovnfs new)"              # starts the daemon, prints the checkout's path
git status                     # same status as ~/src/app; nothing is written there
# ...let the agent work...
ovnfs diff app -p              # what changed
ovnfs apply app                # merge it back into ~/src/app
cd ~/src/app && ovnfs rm app   # remove the checkout and its unused snapshots
```

More, from `~/src/app`:

```sh
ovnfs run -- make test         # a disposable checkout: run, then remove
ovnfs new --rev main           # start from a revision instead of the working tree
ovnfs attach app --to devbox   # the same checkout on another machine, fetched lazily
ovnfs send . devbox:src/app    # attach over ssh, installing ovnfs there first if missing
ovnfs push app                 # copy what devbox still reads from here, so it works with this machine off
ovnfs grep 'TODO' app          # search in the daemon, without traversing the mount
ovnfs glob '*.go' app          # matching paths, recursively and ignore-aware
ovnfs ls .                     # the checkouts of this folder
ovnfs prune . --idle 7d        # remove those untouched for a week (--dry-run first)
ovnfs bench                   # compare mounted ovnfs and disk I/O in under 30 seconds
ovnfs tune                    # try isolated mount configs; ask before applying a balanced winner
```

`ovnfs help` lists every verb, grouped by what they do and coloured on a
terminal (`--color auto|always|never`, `NO_COLOR` honoured; a pipe gets plain
text). Bash, zsh and fish completion come from the same grammar:
`eval "$(ovnfs completion bash -c)"`, or `ovnfs completion zsh -c` for the
shell to source. Verbs start the daemon when none is running;
`start`, `stop`, `restart` and `status` manage it, `serve` runs it in the
foreground. `restart` swaps in the daemon of the binary you run, e.g. after an
upgrade, leaving an NFS or SMB mount and the shells inside it in place. `stop`
fails while something uses the mount; a second `stop` forces the unmount.

## How it works

Opening a folder sorts every path into a class. `source` paths are captured:
they are what `diff`, `apply` and snapshots see, and the only bytes that leave
the machine. `cache` and `local` paths (build outputs, ignored files, secrets)
show the live folder until the checkout writes them: a path it writes or
deletes stays its own from then on, and `apply` never carries it back. A
`cache` file the folder changes after the open disappears from the checkout
instead, so a build there redoes it rather than linking the folder's output.
`hidden` paths are absent. Built-in rules
handle the common cases; `.o2/ovnfs.yml` and `--rule` override them, and
`ovnfs explain PATH` says which rule decided.

Writes go to the checkout's own copy-on-write layer, so the folder is never
written until you `apply`. In a git repository the checkout gets a private git
directory: commit freely, then `apply --branch NAME` to bring the commits back,
or `git fetch ovnfs/NAME` them from its git remote. In a
[jj](https://jj-vcs.github.io/jj/) repository each checkout is also a jj
workspace of it.

ovnfs isolates files, not processes: it is not a sandbox.

## Documentation

- [Configuration](docs/configuration.md): classes, rules, `.o2/ovnfs.yml` and `ovnfs config`, where state and checkouts live
- [Checkouts](docs/checkouts.md): `--rev`, `--take`, `run`, snapshots, forks, `diff` and `apply`
- [Remote checkouts](docs/remote.md): `attach`, `send`, `pull`, `push`, `share`
- [Platforms](docs/platforms.md): NFS, FUSE, SMB and WinFsp frontends
- [Internals](docs/internals.md): admin protocol for embedding, persistence, memory tiers
- [Limitations](docs/limitations.md)
- [Development](docs/development.md): tests and benchmarks
- [Agent skill](skills/ovnfs/SKILL.md): teaches coding agents to work in checkouts; copy `skills/ovnfs` into your agent's skills directory

## License

[MIT](LICENSE)
