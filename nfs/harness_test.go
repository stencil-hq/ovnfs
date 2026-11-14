package nfs

import (
	"bufio"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"

	"github.com/stencil-hq/ovnfs/smb"
)

// testFS exports a temporary directory. Like the export's node table, it
// keeps ids across renames and retires them on removal.
type testFS struct {
	billy.Filesystem
	root string
	// cacheable is Backend.Cacheable; nil admits nothing.
	cacheable func(p string) bool

	mu    sync.Mutex
	ids   map[string]uint64
	paths map[uint64]string
	next  uint64
}

// newTestFS exports a fresh temporary directory; its root has id 1.
func newTestFS(t testing.TB) *testFS {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &testFS{
		Filesystem: osfs.New(root),
		root:       root,
		ids:        map[string]uint64{"": 1},
		paths:      map[uint64]string{1: ""},
		next:       2,
	}
}

// host is p's path on the host.
func (f *testFS) host(p string) string { return filepath.Join(f.root, filepath.FromSlash(p)) }

// clean is the overlay-clean form of p.
func clean(p string) string { return strings.TrimPrefix(path.Clean("/"+p), "/") }

// FileID allocates ids on first use.
func (f *testFS) FileID(p string) uint64 {
	p = clean(p)
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.ids[p]; ok {
		return id
	}
	id := f.next
	f.next++
	f.ids[p], f.paths[id] = id, p
	return id
}

// ChildID gives listed names the same identities as direct lookup.
func (f *testFS) ChildID(dir, name string) uint64 {
	return f.FileID(path.Join(dir, name))
}

// Path resolves a live id.
func (f *testFS) Path(id uint64) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.paths[id]
	return p, ok
}

// RetireHandle makes id stale while preserving its path for the next FileID.
func (f *testFS) RetireHandle(id uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.paths[id]
	if !ok || p == "" {
		return false
	}
	f.retireLocked(p)
	return true
}

// Rename moves the ids of from and its descendants to to, retiring to's.
func (f *testFS) Rename(from, to string) error {
	if err := f.Filesystem.Rename(from, to); err != nil {
		return err
	}
	from, to = clean(from), clean(to)
	if from == to {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retireLocked(to)
	for p, id := range f.ids {
		if p == from || strings.HasPrefix(p, from+"/") {
			np := to + p[len(from):]
			delete(f.ids, p)
			f.ids[np], f.paths[id] = id, np
		}
	}
	return nil
}

// Remove retires p's id.
func (f *testFS) Remove(p string) error {
	if err := f.Filesystem.Remove(p); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retireLocked(clean(p))
	return nil
}

// retireLocked forgets the ids of p and its descendants.
func (f *testFS) retireLocked(p string) {
	for q, id := range f.ids {
		if q == p || strings.HasPrefix(q, p+"/") {
			delete(f.ids, q)
			delete(f.paths, id)
		}
	}
}

// Chmod follows symlinks, as billy's Chmod does.
func (f *testFS) Chmod(p string, mode os.FileMode) error { return os.Chmod(f.host(p), mode) }

// Lchown changes a symlink itself.
func (f *testFS) Lchown(p string, uid, gid int) error { return os.Lchown(f.host(p), uid, gid) }

// Chown follows symlinks.
func (f *testFS) Chown(p string, uid, gid int) error { return os.Chown(f.host(p), uid, gid) }

// Chtimes sets access and modification times.
func (f *testFS) Chtimes(p string, atime, mtime time.Time) error {
	return os.Chtimes(f.host(p), atime, mtime)
}

// Link makes a hard link.
func (f *testFS) Link(oldname, newname string) error {
	return os.Link(f.host(oldname), f.host(newname))
}

// Cacheable consults the test's policy.
func (f *testFS) Cacheable(p string) bool { return f.cacheable != nil && f.cacheable(p) }

// Statfs returns fixed capacity.
func (f *testFS) Statfs() (smb.Statfs, error) {
	return smb.Statfs{BlockSize: 4096, Blocks: 1 << 20, Free: 1 << 19, Avail: 1 << 18, Files: 1 << 20, FilesFree: 1 << 19}, nil
}

// The test filesystem is a complete backend.
var _ Backend = (*testFS)(nil)

// startServer serves s on a loopback port until the test ends and returns
// its address.
func startServer(t testing.TB, s *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = s.Serve(l)
		close(done)
	}()
	t.Cleanup(func() {
		l.Close()
		<-done
		closeKept(s)
	})
	return l.Addr().String()
}

// closeKept closes the files s keeps open for writes (keptFiles, v4Kept),
// as the server's process exiting would: Windows cannot remove the test's
// directory while a file in it is open.
func closeKept(s *Server) {
	k := &s.v3.kept
	k.mu.Lock()
	var files []billy.File
	for f := k.evictLocked(); f != nil; f = k.evictLocked() {
		files = append(files, f)
	}
	k.mu.Unlock()
	for _, f := range files {
		f.Close()
	}

	st := &s.v4
	st.mu.Lock()
	for _, f := range st.files {
		if f.kept != nil {
			st.closing = append(st.closing, f.kept.f)
			f.kept = nil
		}
	}
	st.unlock()
}

// hostPerm is the permission a host file, or with dir a directory, created
// or chmodded with perm reads back with. Windows keeps only a read-only
// attribute, which Go reports as 0444 or 0666, adding 0111 for directories.
func hostPerm(perm os.FileMode, dir bool) os.FileMode {
	if runtime.GOOS != "windows" {
		return perm
	}
	p := os.FileMode(0o666)
	if perm&0o200 == 0 {
		p = 0o444
	}
	if dir {
		p |= 0o111
	}
	return p
}

// rpcClient sends hand-encoded calls, one at a time, over one connection.
type rpcClient struct {
	t   testing.TB
	nc  net.Conn
	r   *bufio.Reader
	xid uint32
}

// dial connects a client to a test server.
func dial(t testing.TB, addr string) *rpcClient {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return &rpcClient{t: t, nc: nc, r: bufio.NewReader(nc)}
}

// call sends one call with AUTH_SYS credentials, args encoding the
// procedure's arguments (nil for none), and returns the reply's accept_stat
// and a decoder over its body. A denied or mismatched reply fails the test.
func (c *rpcClient) call(prog, vers, proc uint32, args func(*encoder)) (acceptStat, *decoder) {
	c.t.Helper()
	c.xid++
	e := encoder{}
	e.u32(0)
	e.u32(c.xid)
	e.u32(msgCall)
	e.u32(2)
	e.u32(prog)
	e.u32(vers)
	e.u32(proc)
	cred := encoder{}
	cred.u32(0) // stamp
	cred.str("nfstest")
	cred.u32(uint32(os.Getuid()))
	cred.u32(uint32(os.Getgid()))
	cred.u32(0) // gids
	e.u32(authSys)
	e.opaque(cred.buf)
	e.u32(authNone)
	e.u32(0)
	if args != nil {
		args(&e)
	}
	e.put32(0, lastFragment|uint32(e.len()-4))
	if _, err := c.nc.Write(e.buf); err != nil {
		c.t.Fatal(err)
	}
	rec, err := readRecord(c.r, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	d := &decoder{buf: rec}
	if xid := d.u32(); xid != c.xid {
		c.t.Fatalf("reply xid %d, want %d", xid, c.xid)
	}
	if d.u32() != msgReply || d.u32() != replyAccepted {
		c.t.Fatal("call not accepted")
	}
	d.u32()
	d.opaque(maxAuthBytes)
	stat := acceptStat(d.u32())
	if d.err != nil {
		c.t.Fatal("truncated reply header")
	}
	return stat, d
}
