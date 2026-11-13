# Limitations

- This is not a sandbox: absolute paths and symlinks can reach the host.
- The view is best effort, not a frozen snapshot: a source file the user
  rewrites while the agent is reading it can show either version, and cache
  and local paths always show the folder as it is.
- A tracked file the user changes after open is read from the repository's
  objects; pruning those objects can make it unreadable.
- ovnfs reads git repositories with its own implementation, not a git
  installation. It reads the system configuration and attributes from
  `GIT_CONFIG_SYSTEM` or `/etc/gitconfig` and `/etc/gitattributes` (none on
  Windows), not from the prefix a particular git was installed to (such as
  Homebrew's or Git for Windows'), and cannot expand `%(prefix)/` paths.
  Fetches (`apply --branch`, the remote's git mirror) run `git fetch` when git
  is on `PATH`. Without git, ovnfs fetches itself over the same transports,
  finding credential helpers named in configuration on `PATH` or in
  `GIT_EXEC_PATH`, and reading client certificates (`http.sslCert`) only as
  PEM or DER files.
- Remote checkouts have no git directory and cannot be attached onward. Bring
  their edits back with `pull` on the source machine, then `apply` the local
  checkout to its folder. `fstat` on a lazily fetched open file reports the
  cached blob's metadata; path-based stat is exact.
- A link is one ssh session per checkout; cold reads of many files are bound by
  its throughput.
- Sockets and FIFOs can be made in NFS and FUSE checkouts, not SMB or WinFsp
  ones, and stay in the checkout: like a folder's own, they never reach
  `diff`, `apply` or a snapshot's tree. Device nodes are refused.
- NFSv3 locks are client-local. Explicit NFSv4 clients use byte-range locks
  held in the daemon's memory, lost when it restarts. SMB has no hard links
  and no durable handles.
- A file's id derives from its directory and name. Within one daemon run a
  removed file's id is never reissued, unless the daemon could not tell which
  paths changed (a reattach, or folder change events the watcher lost): it
  then recalls every handle below the affected directory without checking
  that its file still exists. In that case, and after a restart, a file
  recreated under the same name can take the id an NFS client still holds for
  the removed one; a hash collision with a name created behind the export while
  the daemon was down makes an earlier handle stale (`ESTALE`), never another
  file's.
- NFSv3 cannot push cache invalidations: folder edits in cache and local
  paths, `pull`, and reattach can remain cached until the client's attribute
  timeout (up to 60 s). NFSv4 read delegations reduce that window for opened
  files, but do not cover directories or files only inspected with `stat`.
  A recursive delete of a directory the folder just added a file to can
  therefore fail with `ENOTEMPTY`, since the client lists it from its cache;
  repeating the delete succeeds.
- One SMB daemon per Windows logon session: the SMB client keeps one set of
  credentials per server name, whatever the port, so a second daemon, whose
  state directory holds another password, cannot map `\\127.0.0.1\ovnfs`
  (error 1219). Run further daemons with `--proto winfsp`.
- WinFsp mounts get no recalls or change notifications for changes made
  behind them (`pull`, reattach, folder edits): they show once the one-second
  metadata cache expires, and directory watchers miss them.
