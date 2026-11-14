# Development

```sh
go build -o bin/ovnfs .
go test ./...
```

`go test ./...` runs the package and in-process protocol checks; on Windows
with WinFsp installed it also mounts real WinFsp drives. The git engines under
`git/internal` are checked against the `git` binary when one is installed
(fixtures and expected output come from it); those tests skip without it.
They are written from git's documentation and observed behaviour, never from
git's or another implementation's source.

## CLI

The command line is a [kong](https://github.com/alecthomas/kong) grammar: one
struct per verb — the root in [`grammar.go`](../grammar.go), each verb's flags
and help beside its implementation, all in struct tags. Help sections are kong
groups, git-style flags take `short:` tags, and values like `--idle 7d` parse
through `encoding.TextUnmarshaler`. [`ui.go`](../ui.go) colours the output, and
`colorHelp` renders kong's help in the same palette.

[kong-completion](https://github.com/jotaen/kong-completion) generates
initialization code for bash, zsh and fish (`ovnfs completion bash -c`): verbs,
a verb's own flags and their enums come from the grammar, checkout and snapshot
names from a daemon that is already running (completion never starts one, see
`predictCheckouts`). The global options stay out of the grammar, so they are
listed in help but not completed.

`run` and `serve` are commands in the same grammar. `splitGlobals` pulls the
global options (`--state`, `--mount`, `--json`, `--color`) out of the arguments
before the `--` separator, so they work before the verb or among its arguments.
The separator preserves the arguments of a command launched by `run`.

## CI and releases

[`ci.yml`](../.github/workflows/ci.yml) runs on pushes to `main` and on pull
requests: `gofmt`, `go mod tidy -diff`, `go vet` for Linux, macOS and Windows,
a snapshot GoReleaser build, then `go test` on all three (race detector on
Linux and macOS, real FUSE mounts on Linux, real WinFsp drives on Windows).

Pushing a `v*` tag runs the same checks, then
[`release.yml`](../.github/workflows/release.yml) has GoReleaser
([`.goreleaser.yaml`](../.goreleaser.yaml)) publish a GitHub release with
archives for Linux, macOS and Windows on amd64 and arm64, and
[`internal/release/upload`](../internal/release/upload/main.go) publish the
same binaries to Stencil's build service, `build.stencil.so`, which takes the
run's GitHub OIDC token as proof of the tag:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

Go stamps the tag into the build info, so `ovnfs version` reports it for release
archives and `go install …@v0.2.0` alike, and a VCS pseudo-version in a clone.

[`install.sh`](../install.sh) and [`install.ps1`](../install.ps1), which
`https://stencil.so/ovnfs.sh` and `/ovnfs.ps1` redirect to on `main`, `update`
and `send` download release binaries from the build service and check their
SHA-256 there; `OVNFS_BUILD_URL` points all of them, and the upload, at another
origin. The service hosts `ovnfs` because its product table
(`apps/build/products.go` in the Stencil repository) lets `stencil-hq/ovnfs`
publish `v<version>` tags.

## Benchmark

`bench.sh REPO [TOUCH]` compares Cargo build times across ways of getting a
second working tree of REPO: a git worktree, an ovnfs checkout over each
protocol (NFS and SMB on macOS; FUSE, NFS and SMB on Linux, mounting the latter
two through passwordless sudo), and a copy-on-write clone. It times create,
snapshot, fork, cold, no-op and incremental builds; TOUCH (default
`src/main.rs`) is the file touched for incremental rows.

Results go to `BENCH_DIR/results.tsv`, by default `ovnfs-bench` beside REPO so
the clone and the daemon's state share its volume. `BUILD` (default
`cargo build`), `OVNFS` (default `bin/ovnfs`) and `PROTOS` override the rest;
run BUILD without cross-tree caches such as sccache, which would hide the
filesystem's effect. `bench.sh --clean REPO` removes `BENCH_DIR` and its
worktree.

`scripts/buildbench` compares ovnfs binaries on build-shaped IO and memory.
Each `-ovnfs` binary gets a private daemon (temporary state, mount and HOME,
so default settings) and a checkout of one fixture repository: the
`-project`'s files (or a synthetic crate) plus `-files` bulk files (default
100,000) for a realistically large base, committed with one `git fast-import`
stream in about five seconds. Every round runs the same work in a host
directory and in each checkout, rotating the order: `io` (the default)
replays the filesystem traffic of a cold `cargo build` of a 149-package
workspace (82 rlibs, build scripts, fingerprints, 256 codegen objects, a 24 MB
linked binary hard-linked into place), a no-op build, a relink after a touch,
and a clean; `cargo` runs `cargo build --offline` cold in the first round and
no-op and after a touch in every round. It prints each phase's median, its
ratio to the host and its range, the capture time, and each daemon's
footprint (macOS) or RSS (Linux), its peak and the cache use from
`ovnfs status`, after capture and after the work (and after `-idle`, if set).
The default run, two binaries and three `io` rounds, takes about 25 seconds:

```sh
(cd "$(ovnfs new --name old --rev main --json | jq -r .checkout.path)" && go build -o /tmp/ovnfs-old .)
go build -o /tmp/ovnfs-new .
go run ./scripts/buildbench -ovnfs /tmp/ovnfs-old -ovnfs /tmp/ovnfs-new -project ~/src/some-crate -modes io,cargo -runs 5
```

## Node cache stress test

`scripts/stress-nodes.py` starts a private Linux daemon and source repository,
opens many checkouts, checks every file's contents and size during repeated
walks, runs `git status`, and samples daemon RSS and `profile heap`. It never
uses an existing state directory or mount. Build the binary first, then run
with root for the kernel NFS mount (FUSE also works without root):

```sh
go build -o bin/ovnfs .
sudo python3 scripts/stress-nodes.py --binary bin/ovnfs --proto nfs --output /tmp/node-stress
```

Defaults are 24 checkouts, 400 directories of 128 files in each of `src` and
`node_modules` (2,457,600 paths total), at least three complete walks and ten
minutes. `--node-cache 4K --workers 4 --mutate --restart` adds concurrent
rename/remove/create operations and a restart while a separate process holds
cwd and a file descriptor; use `--restart` with NFS only. `--proto fuse`
exercises the FUSE frontend. A race-enabled binary checks the same workload
under the race detector. Use the same script with a main-branch binary for a
baseline; old binaries ignore `OVNFS_NODE_CACHE`.

The NFS restart probe also retains opaque wire handles for a moved file and a
held directory whose ancestor moves; GETATTR and READ after restart use those
exact bytes through a fresh TCP connection. The mounted cwd/fd checks remain.
The concurrent cold-mutation tests in `nfs` and `fuse` verify eviction before
mutating files, empty directories, ancestors, removed/recreated names and known
hash collisions against a generation-consistent listing oracle. The upstream
FUSE held-fd rename/content failure is a separate probe, excluded from this
phase. `OVNFS_LATENCY=1 go test ./export ./nfs -run Latency -v` reports wide
directory rewalks and concurrent unrelated lookup latency.

RSS is sampled once a second; index bytes come from the periodic status/heap
snapshots, avoiding a filesystem-wide index scan on each sample. The output
directory contains RSS and index-byte samples, heap profiles, status snapshots,
daemon/holder logs and `results.json` with timings, throughput and errors.
