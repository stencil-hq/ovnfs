package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	ovnfuse "github.com/stencil-hq/ovnfs/fuse"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
	"github.com/stencil-hq/ovnfs/winfsp"
)

// memoryFileMax is the largest written file kept in memory; bigger files go
// to disk, where writes already land in the page cache. 256 KiB keeps a
// build's small short-lived files in memory (cargo fingerprints, dep-info
// and temp files, and the median rlib of about 130 KB) while large outputs
// (rlibs' 90th percentile is 1.7 MB, linked binaries tens of MB) leave
// memory at their first 256 KiB instead of being buffered whole, so they do
// not crowd the budget. 64 KiB made a replayed cold cargo build 16% slower
// (scripts/buildbench): each mid-sized rlib then paid a spill's host create
// while being written.
const memoryFileMax = 256 << 10

// Default serve budgets; see docs/internals.md (memory tiers).
const (
	defaultMemory    byteSize = 64 << 20
	defaultBlobCache byteSize = 16 << 20
	defaultNodeCache byteSize = export.DefaultNodeCache
)

// memoryIdle is how long a file sits untouched in memory before it is written
// back to its upper, bounding what a snapshot must flush and a crash loses.
const memoryIdle = 2 * time.Second

// byteSize is a flag holding a byte count: a number with an optional binary
// K, M, G or T suffix ("512M", "1G", "0").
type byteSize int64

// String renders the byte count in the largest suffix that divides it ("1G").
func (b byteSize) String() string {
	n := int64(b)
	for _, u := range []struct {
		shift  uint
		suffix string
	}{{40, "T"}, {30, "G"}, {20, "M"}, {10, "K"}} {
		if n != 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// Set parses a size such as "512M"; "B", "KB" and "KiB" spellings are accepted.
func (b *byteSize) Set(v string) error {
	s := strings.TrimSuffix(strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(v)), "IB"), "B")
	shift := 0
	if i := len(s) - 1; i >= 0 {
		switch s[i] {
		case 'K':
			shift = 10
		case 'M':
			shift = 20
		case 'G':
			shift = 30
		case 'T':
			shift = 40
		}
		if shift != 0 {
			s = s[:i]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64>>shift {
		return fmt.Errorf("invalid size %q", v)
	}
	*b = byteSize(n << shift)
	return nil
}

// UnmarshalText parses a kong size option.
func (b *byteSize) UnmarshalText(text []byte) error {
	return b.Set(string(text))
}

// serveDefaults supplies user, environment and platform defaults to kong.
func serveDefaults(conf manifest.Daemon) kong.Option {
	return kong.Vars{
		"default_proto":       cmp.Or(conf.Proto, defaultProto()),
		"memory_default":      cmp.Or(os.Getenv("OVNFS_MEMORY"), conf.Memory, defaultMemory.String()),
		"fsync_default":       cmp.Or(os.Getenv("OVNFS_FSYNC"), conf.Fsync, "false"),
		"node_cache_default":  cmp.Or(os.Getenv("OVNFS_NODE_CACHE"), conf.NodeCache, defaultNodeCache.String()),
		"blob_cache_default":  cmp.Or(os.Getenv("OVNFS_BLOB_CACHE"), conf.BlobCache, defaultBlobCache.String()),
		"atime_default":       cmp.Or(os.Getenv("OVNFS_ATIME"), conf.Atime, "false"),
		"nfs_io_size_default": cmp.Or(os.Getenv("OVNFS_NFS_IO_SIZE"), conf.NFSIOSize, byteSize(defaultNFSIOSize).String()),
		"git_remotes_default": cmp.Or(os.Getenv("OVNFS_GIT_REMOTES"), conf.GitRemotes, "true"),
	}
}

// serveCmd configures the foreground daemon.
type serveCmd struct {
	Proto      string   `help:"File server protocol: nfs, smb, fuse or winfsp." default:"${default_proto}" enum:"nfs,smb,fuse,winfsp"`
	Listen     string   `help:"File server address; defaults to a free local port or a Unix socket for NFS on macOS."`
	Memory     byteSize `help:"Memory budget for small written files; 0 writes every file to disk." default:"${memory_default}"`
	Fsync      bool     `help:"Spill and sync memory files on fsync (NFS does not forward fsync)." default:"${fsync_default}"`
	NodeCache  byteSize `help:"Estimated cache bytes for derived node ids and listings; keeps at least the current directory." default:"${node_cache_default}"`
	BlobCache  byteSize `help:"Memory budget for small content-addressed blobs; 0 disables." default:"${blob_cache_default}"`
	Atime      bool     `help:"Allow access time updates on the mount." default:"${atime_default}"`
	NFSIOSize  byteSize `name:"nfs-io-size" help:"NFS read/write transfer size (power of two, 4K to 1M)." default:"${nfs_io_size_default}"`
	GitRemotes bool     `help:"Give each source repository a git remote ovnfs/NAME per checkout of it." default:"${git_remotes_default}" negatable:""`
}

// eventHub fans persisted events out to connected admin subscribers.
type eventHub struct {
	mu          sync.Mutex
	subscribers map[chan admin.Event]context.CancelFunc
}

// emit delivers events without holding up registry operations for slow clients.
func (h *eventHub) emit(e checkout.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, cancel := range h.subscribers {
		select {
		case ch <- admin.Event{Name: e.Name, Payload: e.Payload}:
		default:
			cancel()
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}

// events registers a bounded subscription whose cancellation releases its resources.
func (h *eventHub) events(ctx context.Context) <-chan admin.Event {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan admin.Event, 256)
	h.mu.Lock()
	if h.subscribers == nil {
		h.subscribers = make(map[chan admin.Event]context.CancelFunc)
	}
	h.subscribers[ch] = cancel
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		if _, ok := h.subscribers[ch]; ok {
			delete(h.subscribers, ch)
			close(ch)
		}
		h.mu.Unlock()
		cancel()
	}()
	return ch
}

// Run opens the persistent export, attaches its native client, and handles shutdown.
func (cmd *serveCmd) Run(c *cli) (err error) {
	proto := cmd.Proto
	if proto == "" {
		proto = defaultProto()
	}
	if cmd.NFSIOSize > 1<<20 {
		return fmt.Errorf("NFS I/O size must not exceed 1 MiB")
	}
	settings := mountSettings{Atime: cmd.Atime, NFSIOSize: int(cmd.NFSIOSize)}
	if err := settings.validate(); err != nil {
		return err
	}
	go releaseIdle()
	var mem *overlay.Memory
	if cmd.Memory > 0 {
		mem = overlay.NewMemory(int64(cmd.Memory), memoryFileMax, memoryIdle)
		mem.SyncOnFsync = cmd.Fsync
	}
	state, err := filepath.Abs(c.state)
	if err != nil {
		return fmt.Errorf("state path: %w", err)
	}
	mount, err := filepath.Abs(c.mount)
	if err != nil {
		return fmt.Errorf("mount path: %w", err)
	}
	if err = os.MkdirAll(state, 0700); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	// Claim the admin socket before the state loads, which takes minutes on
	// a large state after a reboot: a client then finds this daemon and
	// waits for its hello, instead of starting a second one.
	adminListener, err := admin.Listen(filepath.Join(state, "admin.sock"))
	if err != nil {
		return fmt.Errorf("admin listen: %w", err)
	}
	defer adminListener.Close()
	adminServer := admin.NewServer()
	served := make(chan error, 2)
	go func() {
		served <- fmt.Errorf("admin serve: %w", adminServer.Serve(adminListener))
	}()
	hub := &eventHub{}
	var e *export.Export
	// Folder events may invalidate before the export exists; they are dropped then.
	var exported atomic.Pointer[export.Export]
	// The system client's connections, from listenAndMount on; nil for in-process mounts.
	var files atomic.Pointer[fileListener]
	var smbServer atomic.Pointer[smb.Server]
	var checkoutShares *shares
	userHome, _ := os.UserHomeDir()
	lk := newLinks(hub.emit)
	node, err := loadNode(state)
	if err != nil {
		return fmt.Errorf("node id: %w", err)
	}
	reg, err := checkout.Open(checkout.Config{State: state, Mount: mount, UserHome: userHome, Node: node, Emit: hub.emit, Fetch: lk.fetch, Search: lk.search,
		Memory: mem, BlobCache: int64(cmd.BlobCache), GitRemotes: cmd.GitRemotes, Logf: log.Printf,
		Quiesce: func(ctx context.Context) (func(), error) {
			if s := smbServer.Load(); s != nil {
				return s.Quiesce(ctx)
			}
			return func() {}, nil
		},
		Retire: func(name string) {
			lk.detach(name)
			if checkoutShares != nil {
				checkoutShares.Close(name)
			}
			if e != nil {
				e.Retire(name)
			}
		},
		Invalidate: func(name string, paths, trees []string) {
			if x := exported.Load(); x != nil {
				x.Invalidate(name, paths, trees)
			}
		},
		Changed: func(name string, added bool) {
			if s := smbServer.Load(); s != nil {
				if added {
					s.Changed(name, smb.NotifyAdded)
				} else {
					s.Retire(name)
				}
			}
			if l := files.Load(); l != nil && proto == "nfs" {
				l.serving(func() { refreshRoot(mount) })
			}
		}})
	if err != nil {
		return fmt.Errorf("checkouts: %w", err)
	}
	var releaseCaches func()
	defer func() {
		if releaseCaches != nil {
			releaseCaches()
		}
	}()
	defer func() { err = errors.Join(err, reg.Close()) }()
	lk.reg = reg
	defer lk.close()
	e, err = export.New(export.NewRouter(reg), filepath.Join(state, "nodes.log"), state, log.Printf)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	e.SetNodeCache(int64(cmd.NodeCache))
	exported.Store(e)
	defer e.Close()
	if checkoutShares, err = newShares(state, reg, e); err != nil {
		return fmt.Errorf("shares: %w", err)
	}
	defer checkoutShares.CloseAll()
	port := 0
	release := func(force bool) error { return unmount(mount, force) }
	if proto == "fuse" || proto == "winfsp" {
		srv, mountErr := mountInProcess(proto, e, mount, cmd.Atime)
		if mountErr != nil {
			return mountErr
		}
		release = srv.Unmount
		go func() { srv.Wait(); served <- fmt.Errorf("%s mount at %s ended", proto, mount) }()
		if err = reg.SetListen(proto, 0); err != nil {
			_ = release(true)
			return fmt.Errorf("persist listen: %w", err)
		}
	} else {
		var l *fileListener
		if port, l, err = listenAndMount(reg, e, proto, cmd.Listen, state, mount, settings, served, smbServer.Store); err != nil {
			return err
		}
		files.Store(l)
		defer l.Close() // removes the NFS socket, once: a keep-mount cut already has
	}
	log.Printf("mounted at %s", mount)
	// Shutdown closes the socket first: stop waits for it to close, then for
	// the process to exit, and the registry's final flush can be slow.
	defer adminListener.Close()
	linkListener, err := linkListen(state)
	if err != nil {
		_ = release(true)
		return fmt.Errorf("link listen: %w", err)
	}
	defer linkListener.Close()
	go lk.listen(linkListener)
	stops := make(chan stopRequest)
	welcome := admin.Welcome{Version: protocolVersion, Ops: api.Ops, Features: api.Features, Proto: proto, Port: port, Mount: mount, State: state}
	handler := &opsHandler{nodeCache: e.NodeCache, nodeIndexBytes: e.NodeIndexBytes, nodeCacheError: e.NodeCacheError, reg: reg, shares: checkoutShares, links: lk, welcome: welcome, started: time.Now(), stop: func(ctx context.Context, a api.StopArgs) error {
		done := make(chan error, 1)
		select {
		case stops <- stopRequest{keepMount: a.KeepMount, ctx: ctx, done: done}:
			return <-done
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	adminServer.Ready(welcome, handler, hub.events)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	force := false
	for {
		var req stopRequest
		select {
		case err = <-served:
			_ = release(true)
			return err
		case req = <-stops:
		case <-sigs:
		}
		var flushRelease func()
		if !force || req.keepMount {
			ctx := req.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			flushRelease, err = reg.Quiesce(ctx)
			if err != nil {
				err = fmt.Errorf("flushing client write caches: %w", err)
				log.Print(err)
				if req.done != nil {
					req.done <- err
				}
				force = true
				continue
			}
		}
		if l := files.Load(); req.keepMount && l != nil {
			// Cut before the export closes, which would answer the kernel
			// client "not found": it retries the cut connections instead, until
			// the next daemon serves this port.
			releaseCaches = flushRelease
			l.cut()
			log.Printf("left %s mounted for the next daemon", mount)
			req.done <- nil
			return nil
		}
		if err = release(force); err != nil {
			if flushRelease != nil {
				flushRelease()
			}
			err = fmt.Errorf("unmount: %w", err)
			if !force {
				err = fmt.Errorf("%w; stop again to force", err)
			}
			log.Print(err)
			force = true
		}
		if req.done != nil {
			req.done <- err
		}
		if err == nil {
			releaseCaches = flushRelease
			return nil
		}
	}
}

// stopRequest is a stop op awaiting serve's answer on done: nil once the
// daemon exits, the unmount error while it keeps running.
type stopRequest struct {
	keepMount bool
	ctx       context.Context
	done      chan<- error
}

// fileListener tracks the connections a system NFS or SMB client makes, so a
// stop that keeps the mount can cut them all at once.
type fileListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[*fileConn]struct{} // nil once cut
	calls sync.RWMutex           // held shared by serving's calls, exclusively by cut
}

// serving runs f, a call into the mount such as refreshRoot, unless the
// listener is cut, and holds off the cut until f returns. Once cut, the mount
// has no server until the next daemon starts, which waits for this one to
// exit: a call then would hang this daemon's shutdown, and the restart with it.
func (l *fileListener) serving(f func()) {
	l.calls.RLock()
	defer l.calls.RUnlock()
	l.mu.Lock()
	cut := l.conns == nil
	l.mu.Unlock()
	if !cut {
		f()
	}
}

// Accept tracks the connection, or refuses it once the listener is cut.
func (l *fileListener) Accept() (net.Conn, error) {
	nc, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	c := &fileConn{Conn: nc, l: l}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns == nil {
		nc.Close()
		return nil, net.ErrClosed
	}
	l.conns[c] = struct{}{}
	return c, nil
}

// cut closes the listener and every connection it accepted, once the calls
// serving runs have returned.
func (l *fileListener) cut() {
	l.calls.Lock()
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	l.calls.Unlock()
	l.Listener.Close()
	for c := range conns {
		c.Conn.Close()
	}
}

// fileConn is a connection fileListener forgets once it closes.
type fileConn struct {
	net.Conn
	l *fileListener
}

// WriteBuffers preserves scatter/gather writes through the listener wrapper.
func (c *fileConn) WriteBuffers(buffers net.Buffers) (int64, error) {
	return buffers.WriteTo(c.Conn)
}

// Close forgets and closes the connection.
func (c *fileConn) Close() error {
	c.l.mu.Lock()
	delete(c.l.conns, c)
	c.l.mu.Unlock()
	return c.Conn.Close()
}

// defaultProto picks the frontend an unprivileged user can mount on this OS.
// On Windows SMB, whose client caches under leases, is faster than WinFsp,
// which sends every open to the daemon; WinFsp wins only where installed and
// the SMB client dials port 445 alone, which the Server service holds (smbClientPort).
func defaultProto() string {
	switch runtime.GOOS {
	case "linux":
		return "fuse"
	case "windows":
		if smbClientPort() == 445 && winfsp.Installed() {
			return "winfsp"
		}
		return "smb"
	}
	return "nfs"
}

// inProcessMount is a mount whose filesystem driver calls back into this
// process (FUSE, WinFsp), so it ends with the daemon.
type inProcessMount interface {
	// Unmount ends the mount. Without force it fails while the mount is busy;
	// with force it ends it anyway.
	Unmount(force bool) error
	Wait()
}

// mountInProcess mounts e at mount through FUSE or WinFsp; atime applies to FUSE.
func mountInProcess(proto string, e *export.Export, mount string, atime bool) (inProcessMount, error) {
	if proto == "winfsp" {
		if err := makeMountPoint(mount); err != nil {
			return nil, fmt.Errorf("mount directory: %w", err)
		}
		srv, err := winfsp.Mount(e, mount)
		if err != nil {
			return nil, fmt.Errorf("mount: %w", err)
		}
		return srv, nil
	}
	srv, err := ovnfuse.Mount(e, mount, ovnfuse.Options{Atime: atime})
	if err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	return srv, nil
}

// listenAndMount serves NFS or SMB and mounts it at mount with the system
// client (see [attach]). A mount a previous run left keeps dialing what it was
// made with: the persisted port, or the state directory's NFS socket
// ([mountLocal]). A new NFS mount on macOS gets the socket unless address asks
// for TCP, and falls back to TCP when the socket mount fails. An SMB client
// that dials one port only pins it, overriding an earlier run's port. SMB
// authenticates with the state directory's account where the OS has one
// (smbAccount), otherwise as guest. It returns the TCP port, 0 on the socket,
// and the listener whose connections the system client holds.
func listenAndMount(reg *checkout.Registry, e *export.Export, proto, address, state, mount string, settings mountSettings, served chan<- error, smbStarted func(*smb.Server)) (int, *fileListener, error) {
	savedProto, savedPort := reg.Listen()
	remount := mounted(mount)
	socket := savedProto == "nfs" && savedPort == 0 // the previous run served the NFS socket
	if savedProto != "" && savedProto != proto {
		if remount {
			log.Printf("replacing %s mount at %s with %s; forcing unmount", savedProto, mount, proto)
			if err := unmount(mount, true); err != nil {
				return 0, nil, fmt.Errorf("changing mount protocol: %w", err)
			}
			remount = false
		}
		savedPort, socket = 0, false
	}
	if proto == "nfs" && runtime.GOOS == "darwin" && remount {
		log.Printf("resetting handles for retained NFS mount at %s", mount)
		if err := e.ResetNFSHandles(); err != nil {
			return 0, nil, fmt.Errorf("resetting retained NFS handles: %w", err)
		}
	}
	if proto == "nfs" && runtime.GOOS == "darwin" && (remount && socket || !remount && address == "") {
		files, err := mountLocal(reg, e, state, mount, settings, served)
		if err == nil || remount || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return 0, files, err
		}
		log.Printf("NFS over %s failed, using TCP: %v", nfsSocket(state), err)
	}
	address = cmp.Or(address, net.JoinHostPort("127.0.0.1", strconv.Itoa(smbClientPort())))
	pinned := 0
	var user, password string
	if proto == "smb" {
		pinned = smbClientPort()
		var err error
		if user, password, err = smbAccount(state); err != nil {
			return 0, nil, err
		}
	}
	if want := cmp.Or(pinned, savedPort); want != 0 {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return 0, nil, fmt.Errorf("listen address: %w", splitErr)
		}
		address = net.JoinHostPort(host, strconv.Itoa(want))
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		if pinned != 0 {
			return 0, nil, fmt.Errorf("listen %s: %w; this Windows SMB client dials only port %d (Windows 11 24H2 and later dial any port), "+
				"which the Server service (LanmanServer) normally holds: install WinFsp (https://winfsp.dev), which ovnfs then mounts instead, "+
				"or from an elevated prompt run \"sc config LanmanServer start= disabled\" and \"net stop LanmanServer /y\" (this ends local file and printer sharing), then start ovnfs again",
				address, err, pinned)
		}
		if savedPort != 0 {
			return 0, nil, fmt.Errorf("listen: cannot bind persisted %s port %d at %s (existing mounts require this port): %w", proto, savedPort, address, err)
		}
		return 0, nil, fmt.Errorf("listen %s: %w", address, err)
	}
	files := &fileListener{Listener: listener, conns: map[*fileConn]struct{}{}}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = reg.SetListen(proto, port); err != nil {
		listener.Close()
		return 0, nil, fmt.Errorf("persist listen: %w", err)
	}
	if proto == "smb" {
		s := &smb.Server{FS: e.SMB(), Share: "ovnfs", MountRoots: mountRoots(mount), WriteCache: runtime.GOOS == "darwin"}
		if user != "" {
			s.Users = map[string]string{user: password}
		}
		nodes := e.Nodes()
		e.OnRecall(func(id uint64) {
			if p, ok := nodes.Path(id); ok {
				go s.Changed(p, smb.NotifyModified)
			}
		})
		if os.Getenv("LOG_LEVEL") != "" {
			s.Logf = log.Printf
		}
		smbStarted(s)
		go func() { served <- fmt.Errorf("smb serve: %w", s.Serve(files)) }()
	} else {
		go func() { served <- fmt.Errorf("nfs serve: %w", serveNFS(files, e)) }()
	}
	if err = attach(proto, listener.Addr(), mount, user, password, settings); err != nil {
		files.cut()
		return 0, nil, err
	}
	return port, files, nil
}

// mountLocal serves NFS on the state directory's Unix socket and mounts it at
// mount; mount_nfs dials such a socket only on macOS. A failed mount stops
// the server again, leaving the caller free to fall back to TCP.
func mountLocal(reg *checkout.Registry, e *export.Export, state, mount string, settings mountSettings, served chan<- error) (*fileListener, error) {
	l, err := admin.Listen(nfsSocket(state))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	files := &fileListener{Listener: l, conns: map[*fileConn]struct{}{}}
	if err = reg.SetListen("nfs", 0); err != nil {
		l.Close()
		return nil, fmt.Errorf("persist listen: %w", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- serveNFS(files, e) }()
	if err = attach("nfs", l.Addr(), mount, "", "", settings); err != nil {
		files.cut()
		<-stopped
		return nil, err
	}
	go func() { served <- fmt.Errorf("nfs serve: %w", <-stopped) }()
	return files, nil
}

// attach mounts the server at addr or reconciles a retained mount's options.
// A failed reconciliation is a startup error, not a silently stale mount.
func attach(proto string, addr net.Addr, mount, user, password string, settings mountSettings) error {
	if mounted(mount) {
		if err := matchMount(proto, addr, mount, settings); err != nil {
			return fmt.Errorf("matching mount options: %w", err)
		}
		return nil
	}
	if err := makeMountPoint(mount); err != nil {
		return fmt.Errorf("mount directory: %w", err)
	}
	if err := mountServer(proto, addr, mount, user, password, settings); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	return nil
}
