package nfs

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// v3call calls NFSv3 procedure proc and returns the result's status and a
// decoder over the rest of the result.
func (c *rpcClient) v3call(proc uint32, args func(*encoder)) (status, *decoder) {
	c.t.Helper()
	stat, d := c.call(progNFS, 3, proc, args)
	if stat != rpcSuccess {
		c.t.Fatalf("procedure %d: accept_stat %d", proc, stat)
	}
	st := status(d.u32())
	if d.err != nil {
		c.t.Fatalf("procedure %d: no status", proc)
	}
	return st, d
}

// wireAttr is a decoded fattr3.
type wireAttr struct {
	typ, mode, nlink, uid, gid uint32
	size, used                 uint64
	rdev                       [2]uint32
	fsid, fileid               uint64
	atime, mtime, ctime        [2]uint32
}

// fattr3Of decodes an fattr3.
func fattr3Of(d *decoder) wireAttr {
	var a wireAttr
	a.typ, a.mode, a.nlink, a.uid, a.gid = d.u32(), d.u32(), d.u32(), d.u32(), d.u32()
	a.size, a.used = d.u64(), d.u64()
	a.rdev = [2]uint32{d.u32(), d.u32()}
	a.fsid, a.fileid = d.u64(), d.u64()
	a.atime = [2]uint32{d.u32(), d.u32()}
	a.mtime = [2]uint32{d.u32(), d.u32()}
	a.ctime = [2]uint32{d.u32(), d.u32()}
	return a
}

// postOpOf decodes a post_op_attr; ok is false when absent.
func postOpOf(t testing.TB, d *decoder) (wireAttr, bool) {
	t.Helper()
	if !d.bool() {
		return wireAttr{}, false
	}
	return fattr3Of(d), true
}

// dirop encodes diropargs3.
func dirop(e *encoder, dir uint64, name string) {
	e.fh(dir)
	e.str(name)
}

// noSattr encodes an sattr3 that sets nothing.
func noSattr(e *encoder) {
	for range 6 {
		e.u32(0)
	}
}

// modeSattr encodes an sattr3 that sets only the mode.
func modeSattr(e *encoder, mode uint32) {
	e.u32(1)
	e.u32(mode)
	for range 5 {
		e.u32(0)
	}
}

// write writes a host file for a test.
func (f *testFS) write(t *testing.T, p, data string) {
	t.Helper()
	if err := os.WriteFile(f.host(p), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// v3server serves fs and returns a client of it.
func v3server(t *testing.T, fs Backend) *rpcClient {
	return dial(t, startServer(t, &Server{FS: fs}))
}

// TestV3HandlesAndNames checks handle resolution and name vetting.
func TestV3HandlesAndNames(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "f", "x")
	c := v3server(t, fs)

	if st, _ := c.v3call(v3GetAttr, func(e *encoder) { e.opaque(make([]byte, 7)) }); st != stBadHandle {
		t.Errorf("7-byte handle: %d, want BADHANDLE", st)
	}
	if st, _ := c.v3call(v3GetAttr, func(e *encoder) { e.fh(999) }); st != stStale {
		t.Errorf("unknown id: %d, want STALE", st)
	}
	if stat, _ := c.call(progNFS, 3, v3GetAttr, func(e *encoder) { e.opaque(make([]byte, 65)) }); stat != rpcGarbageArgs {
		t.Errorf("65-byte handle: accept_stat %d, want GARBAGE_ARGS", stat)
	}
	if stat, _ := c.call(progNFS, 3, 22, nil); stat != rpcProcUnavail {
		t.Errorf("procedure 22: accept_stat %d, want PROC_UNAVAIL", stat)
	}
	if stat, d := c.call(progNFS, 3, v3Null, nil); stat != rpcSuccess || len(d.buf) != 0 {
		t.Errorf("NULL: accept_stat %d, %d body bytes", stat, len(d.buf))
	}
	for name, want := range map[string]status{
		"":                       stAccess,
		"a/b":                    stAccess,
		"a\x00":                  stAccess,
		strings.Repeat("n", 256): stNameTooLong,
		"missing":                stNoEnt,
	} {
		st, d := c.v3call(v3Lookup, func(e *encoder) { dirop(e, 1, name) })
		if st != want || len(d.buf) != 4 {
			t.Errorf("LOOKUP %q: %d with %d body bytes, want %d with 4", name, st, len(d.buf), want)
		}
	}
	if st, d := c.v3call(v3Create, func(e *encoder) {
		dirop(e, 1, strings.Repeat("n", 256))
		e.u32(createUnchecked)
		noSattr(e)
	}); st != stNameTooLong || len(d.buf) != 8 {
		t.Errorf("CREATE of a long name: %d with %d body bytes", st, len(d.buf))
	}
}

// TestV3Lookup checks LOOKUP of names, "." and "..", and the reply layout.
func TestV3Lookup(t *testing.T) {
	fs := newTestFS(t)
	if err := os.Mkdir(fs.host("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs.write(t, "d/f", "hello")
	c := v3server(t, fs)
	dir := fs.FileID("d")

	lookup := func(dirID uint64, name string) (status, uint64, wireAttr, wireAttr) {
		t.Helper()
		st, d := c.v3call(v3Lookup, func(e *encoder) { dirop(e, dirID, name) })
		if st != stOK {
			return st, 0, wireAttr{}, wireAttr{}
		}
		id, ok := d.fh(maxFH3)
		obj, ok1 := postOpOf(t, d)
		da, ok2 := postOpOf(t, d)
		if !ok || !ok1 || !ok2 || d.err != nil || len(d.buf) != 0 {
			t.Fatalf("LOOKUP %q: malformed reply", name)
		}
		return st, id, obj, da
	}
	st, id, obj, da := lookup(dir, "f")
	if st != stOK || id != fs.FileID("d/f") || obj.fileid != id || obj.size != 5 || obj.typ != uint32(typeReg) || da.fileid != dir {
		t.Errorf("LOOKUP f: %d id %d attrs %+v dir %+v", st, id, obj, da)
	}
	if st, id, _, _ := lookup(dir, "."); st != stOK || id != dir {
		t.Errorf("LOOKUP .: %d id %d", st, id)
	}
	if st, id, obj, _ := lookup(dir, ".."); st != stOK || id != 1 || obj.typ != uint32(typeDir) {
		t.Errorf("LOOKUP ..: %d id %d", st, id)
	}
	if st, _, _, _ := lookup(1, ".."); st != stAccess {
		t.Errorf("LOOKUP .. at the root: %d, want ACCES", st)
	}
	if st, _, _, _ := lookup(fs.FileID("d/f"), "x"); st != stNotDir {
		t.Errorf("LOOKUP in a file: %d, want NOTDIR", st)
	}
}

// TestV3SizeRegressionRetiresHandle checks that an unexpected shrink makes
// the old handle stale, while a client-requested truncate preserves it.
func TestV3SizeRegressionRetiresHandle(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "f", "12345678")
	addr := startServer(t, &Server{FS: fs, Concurrency: 4})
	first, second := dial(t, addr), dial(t, addr)
	id := fs.FileID("f")

	getattr := func(c *rpcClient, id uint64) (status, wireAttr) {
		t.Helper()
		st, d := c.v3call(v3GetAttr, func(e *encoder) { e.fh(id) })
		if st != stOK {
			if len(d.buf) != 0 {
				t.Fatalf("GETATTR failure %d has %d body bytes", st, len(d.buf))
			}
			return st, wireAttr{}
		}
		a := fattr3Of(d)
		if d.err != nil || len(d.buf) != 0 {
			t.Fatal("GETATTR: malformed reply")
		}
		return st, a
	}
	lookup := func(c *rpcClient) (uint64, wireAttr) {
		t.Helper()
		st, d := c.v3call(v3Lookup, func(e *encoder) { dirop(e, 1, "f") })
		if st != stOK {
			t.Fatalf("LOOKUP: %d", st)
		}
		id, ok := d.fh(maxFH3)
		a, attrs := postOpOf(t, d)
		if _, dirAttrs := postOpOf(t, d); !ok || !attrs || !dirAttrs || d.err != nil || len(d.buf) != 0 {
			t.Fatal("LOOKUP: malformed reply")
		}
		return id, a
	}

	if st, a := getattr(first, id); st != stOK || a.size != 8 {
		t.Fatalf("initial GETATTR: %d, size %d", st, a.size)
	}
	if err := os.Truncate(fs.host("f"), 3); err != nil {
		t.Fatal(err)
	}
	if st, _ := getattr(first, id); st != stStale {
		t.Fatalf("GETATTR after shrink: %d, want STALE", st)
	}
	if st, _ := getattr(second, id); st != stStale {
		t.Fatalf("old handle on another connection: %d, want STALE", st)
	}
	next, a := lookup(first)
	if next == id || a.size != 3 {
		t.Fatalf("fresh lookup: id %d size %d, old id %d", next, a.size, id)
	}
	id = next

	st, d := first.v3call(v3SetAttr, func(e *encoder) {
		e.fh(id)
		e.u32(0) // mode
		e.u32(0) // uid
		e.u32(0) // gid
		e.bool(true)
		e.u64(2)
		e.u32(0) // atime
		e.u32(0) // mtime
		e.bool(false)
	})
	if st != stOK || !d.bool() {
		t.Fatalf("SETATTR size: %d with absent pre-op attributes", st)
	}
	if size := d.u64(); size != 3 {
		t.Fatalf("SETATTR pre-op size %d, want 3", size)
	}
	d.fixed(16) // pre-op mtime and ctime
	post, ok := postOpOf(t, d)
	if !ok || post.size != 2 || d.err != nil || len(d.buf) != 0 {
		t.Fatalf("SETATTR post-op attributes %+v, present %v", post, ok)
	}
	if got, a := getattr(first, id); got != stOK || a.size != 2 {
		t.Fatalf("requested shrink: %d, size %d", got, a.size)
	}

	if err := os.Truncate(fs.host("f"), 12); err != nil {
		t.Fatal(err)
	}
	if got, a := getattr(first, id); got != stOK || a.size != 12 {
		t.Fatalf("growth: %d, size %d", got, a.size)
	}
	if err := os.Truncate(fs.host("f"), 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := getattr(first, id); got != stStale {
		t.Fatalf("second shrink: %d, want STALE", got)
	}
	fresh, a := lookup(first)
	if fresh == id || a.size != 1 {
		t.Fatalf("second fresh lookup: id %d size %d, old id %d", fresh, a.size, id)
	}
}

// TestV3FailureBodies checks every procedure's failure arm: the status and
// the discriminants of its absent attributes.
func TestV3FailureBodies(t *testing.T) {
	c := v3server(t, newTestFS(t))
	const stale = 999
	fh := func(e *encoder) { e.fh(stale) }
	for _, tc := range []struct {
		proc uint32
		args func(*encoder)
		body int
	}{
		{v3GetAttr, fh, 0},
		{v3SetAttr, func(e *encoder) { e.fh(stale); noSattr(e); e.u32(0) }, 8},
		{v3Lookup, func(e *encoder) { dirop(e, stale, "a") }, 4},
		{v3Access, func(e *encoder) { e.fh(stale); e.u32(0x3f) }, 4},
		{v3Readlink, fh, 4},
		{v3Read, func(e *encoder) { e.fh(stale); e.u64(0); e.u32(10) }, 4},
		{v3Write, func(e *encoder) { e.fh(stale); e.u64(0); e.u32(1); e.u32(2); e.opaque([]byte{1}) }, 8},
		{v3Create, func(e *encoder) { dirop(e, stale, "a"); e.u32(createGuarded); noSattr(e) }, 8},
		{v3Mkdir, func(e *encoder) { dirop(e, stale, "a"); noSattr(e) }, 8},
		{v3Symlink, func(e *encoder) { dirop(e, stale, "a"); noSattr(e); e.str("t") }, 8},
		{v3Mknod, func(e *encoder) { dirop(e, stale, "a"); e.u32(7); noSattr(e) }, 8},
		{v3Remove, func(e *encoder) { dirop(e, stale, "a") }, 8},
		{v3Rmdir, func(e *encoder) { dirop(e, stale, "a") }, 8},
		{v3Rename, func(e *encoder) { dirop(e, stale, "a"); dirop(e, stale, "b") }, 16},
		{v3Link, func(e *encoder) { e.fh(stale); dirop(e, stale, "a") }, 12},
		{v3ReadDir, func(e *encoder) { e.fh(stale); e.u64(0); e.u64(0); e.u32(4096) }, 4},
		{v3ReadDirPlus, func(e *encoder) { e.fh(stale); e.u64(0); e.u64(0); e.u32(4096); e.u32(4096) }, 4},
		{v3FSStat, fh, 4},
		{v3FSInfo, fh, 4},
		{v3PathConf, fh, 4},
		{v3Commit, func(e *encoder) { e.fh(stale); e.u64(0); e.u32(0) }, 8},
	} {
		st, d := c.v3call(tc.proc, tc.args)
		if st != stStale || !bytes.Equal(d.buf, make([]byte, tc.body)) {
			t.Errorf("procedure %d: status %d body %x, want STALE and %d zero bytes", tc.proc, st, d.buf, tc.body)
		}
	}
}

// TestV3MknodRefused checks a backend without special-node support.
func TestV3MknodRefused(t *testing.T) {
	fs := newTestFS(t)
	c := v3server(t, fs)
	st, d := c.v3call(v3Mknod, func(e *encoder) { dirop(e, 1, "fifo"); e.u32(uint32(typeFifo)); noSattr(e) })
	if st != stNotSupp || len(d.buf) != 8 {
		t.Fatalf("MKNOD: %d with %d body bytes", st, len(d.buf))
	}
	if _, err := os.Lstat(fs.host("fifo")); !os.IsNotExist(err) {
		t.Fatalf("MKNOD made a file: %v", err)
	}
}

// read3 reads count bytes at off from file id.
func (c *rpcClient) read3(id, off uint64, count uint32) (status, []byte, bool) {
	c.t.Helper()
	st, d := c.v3call(v3Read, func(e *encoder) { e.fh(id); e.u64(off); e.u32(count) })
	if st != stOK {
		return st, nil, false
	}
	a, ok := postOpOf(c.t, d)
	n := d.u32()
	eof := d.bool()
	data := d.opaque(maxIO)
	if !ok || a.fileid != id || d.err != nil || len(d.buf) != 0 || int(n) != len(data) {
		c.t.Fatalf("READ: malformed reply (count %d, %d bytes)", n, len(data))
	}
	return st, data, eof
}

// TestV3ReadEOF checks that eof is set exactly when the data reaches the end
// of the file.
func TestV3ReadEOF(t *testing.T) {
	fs := newTestFS(t)
	data := make([]byte, 64<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fs.host("f"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := v3server(t, fs)
	id := fs.FileID("f")
	const k = 1024
	for _, tc := range []struct {
		off       uint64
		count     uint32
		want, end int
		eof       bool
	}{
		{0, 16 * k, 0, 16 * k, false},
		{48 * k, 16 * k, 48 * k, 64 * k, true},
		{0, 40 * k, 0, 40 * k, false},
		{24 * k, 40 * k, 24 * k, 64 * k, true},
		{60 * k, 40 * k, 60 * k, 64 * k, true},
		{64 * k, 40 * k, 64 * k, 64 * k, true},
		{1 << 63, 10, 64 * k, 64 * k, true},
	} {
		st, got, eof := c.read3(id, tc.off, tc.count)
		if st != stOK || eof != tc.eof || !bytes.Equal(got, data[tc.want:tc.end]) {
			t.Errorf("READ %d+%d: %d, %d bytes, eof %v; want %d bytes, eof %v", tc.off, tc.count, st, len(got), eof, tc.end-tc.want, tc.eof)
		}
	}
	if st, _, _ := c.read3(1, 0, 10); st != stIsDir {
		t.Errorf("READ of a directory: %d, want ISDIR", st)
	}
}

// TestV3ReadClampsCount checks READ returns at most maxIO bytes, and no eof
// when that stops short of the end.
func TestV3ReadClampsCount(t *testing.T) {
	fs := newTestFS(t)
	if err := os.WriteFile(fs.host("big"), make([]byte, maxIO+10), 0o644); err != nil {
		t.Fatal(err)
	}
	c := v3server(t, fs)
	st, got, eof := c.read3(fs.FileID("big"), 0, maxIO+100)
	if st != stOK || len(got) != maxIO || eof {
		t.Fatalf("READ past maxIO: %d, %d bytes, eof %v", st, len(got), eof)
	}
}

// write3 writes data at off to file id with stable_how how and returns the
// status, count, committed and verifier.
func (c *rpcClient) write3(id, off uint64, how uint32, data []byte) (status, uint32, uint32, []byte) {
	c.t.Helper()
	st, d := c.v3call(v3Write, func(e *encoder) {
		e.fh(id)
		e.u64(off)
		e.u32(uint32(len(data)))
		e.u32(how)
		e.opaque(data)
	})
	if st != stOK {
		return st, 0, 0, nil
	}
	if !d.bool() { // pre-op attributes
		c.t.Fatal("WRITE: no pre-op attributes")
	}
	d.fixed(24)
	if _, ok := postOpOf(c.t, d); !ok {
		c.t.Fatal("WRITE: no post-op attributes")
	}
	n, committed, verf := d.u32(), d.u32(), d.fixed(8)
	if d.err != nil || len(d.buf) != 0 {
		c.t.Fatal("WRITE: malformed reply")
	}
	return st, n, committed, verf
}

// commit3 commits file id and returns the status and verifier.
func (c *rpcClient) commit3(id uint64) (status, []byte) {
	c.t.Helper()
	st, d := c.v3call(v3Commit, func(e *encoder) { e.fh(id); e.u64(0); e.u32(0) })
	if st != stOK {
		return st, nil
	}
	if d.bool() {
		c.t.Fatal("COMMIT: unexpected pre-op attributes")
	}
	if _, ok := postOpOf(c.t, d); !ok {
		c.t.Fatal("COMMIT: no post-op attributes")
	}
	verf := d.fixed(8)
	if d.err != nil || len(d.buf) != 0 {
		c.t.Fatal("COMMIT: malformed reply")
	}
	return st, verf
}

// TestV3WriteCommitVerifier checks WRITE reaches the file FILE_SYNC at any
// stability asked for, and WRITE and COMMIT share a verifier that changes
// with the server instance.
func TestV3WriteCommitVerifier(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "f", "hello world")
	c := v3server(t, fs)
	id := fs.FileID("f")

	st, n, committed, verf := c.write3(id, 6, 0, []byte("WORLD!"))
	if st != stOK || n != 6 || committed != fileSync3 {
		t.Fatalf("WRITE: %d count %d committed %d", st, n, committed)
	}
	if got, _ := os.ReadFile(fs.host("f")); string(got) != "hello WORLD!" {
		t.Fatalf("file holds %q", got)
	}
	_, _, _, verf2 := c.write3(id, 0, 1, []byte("H"))
	st, cverf := c.commit3(id)
	if st != stOK || !bytes.Equal(verf, verf2) || !bytes.Equal(verf, cverf) {
		t.Fatalf("verifiers differ: %x %x %x", verf, verf2, cverf)
	}
	other := v3server(t, fs)
	if _, overf := other.commit3(id); bytes.Equal(overf, verf) {
		t.Fatal("another server instance has the same verifier")
	}
	if st, _, _, _ := c.write3(id, 0, 3, []byte("x")); st != stInval {
		t.Errorf("WRITE with stable_how 3: %d, want INVAL", st)
	}
	if st, _, _, _ := c.write3(1, 0, 2, []byte("x")); st != stInval {
		t.Errorf("WRITE to a directory: %d, want INVAL", st)
	}
	if stat, _ := c.call(progNFS, 3, v3Write, func(e *encoder) { e.fh(id); e.u64(0); e.u32(5); e.u32(2) }); stat != rpcGarbageArgs {
		t.Errorf("WRITE without data: accept_stat %d", stat)
	}
}

// TestV3SetattrGuard checks the SETATTR guard: a ctime other than the one
// GETATTR reports fails NOT_SYNC and changes nothing; the current one
// applies the change, whose truncate must not trip over the write
// permission the same call removes.
func TestV3SetattrGuard(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "f", "0123456789")
	c := v3server(t, fs)
	id := fs.FileID("f")
	getattr := func() wireAttr {
		t.Helper()
		st, d := c.v3call(v3GetAttr, func(e *encoder) { e.fh(id) })
		if st != stOK {
			t.Fatalf("GETATTR: %d", st)
		}
		a := fattr3Of(d)
		if d.err != nil || len(d.buf) != 0 {
			t.Fatal("GETATTR: malformed reply")
		}
		return a
	}
	setattr := func(ctime [2]uint32) (status, *decoder) {
		return c.v3call(v3SetAttr, func(e *encoder) {
			e.fh(id)
			e.u32(1)
			e.u32(0o400) // mode
			e.u32(0)
			e.u32(0)
			e.u32(1)
			e.u64(4) // size
			e.u32(0)
			e.u32(0)
			e.u32(1) // guard
			e.u32(ctime[0])
			e.u32(ctime[1])
		})
	}
	a := getattr()
	if a.ctime != a.mtime || a.atime != a.mtime {
		t.Fatalf("times differ: %+v", a)
	}
	if st, d := setattr([2]uint32{a.ctime[0] - 1, a.ctime[1]}); st != stNotSync || len(d.buf) != 8 {
		t.Fatalf("stale guard: %d with %d body bytes", st, len(d.buf))
	}
	if b := getattr(); b.mode != a.mode || b.size != 10 {
		t.Fatalf("failed SETATTR changed the file: %+v", b)
	}
	st, d := setattr(a.ctime)
	if st != stOK || !d.bool() || d.u64() != 10 {
		t.Fatalf("SETATTR: %d, pre-op size wrong", st)
	}
	d.fixed(16)
	post, ok := postOpOf(t, d)
	if !ok || post.mode != uint32(hostPerm(0o400, false)) || post.size != 4 || len(d.buf) != 0 {
		t.Fatalf("SETATTR post-op: %+v", post)
	}
}

// TestV3SetattrTimes checks SETATTR sets client and server times.
func TestV3SetattrTimes(t *testing.T) {
	fs := newTestFS(t)
	fs.write(t, "f", "")
	c := v3server(t, fs)
	st, _ := c.v3call(v3SetAttr, func(e *encoder) {
		e.fh(fs.FileID("f"))
		for range 4 {
			e.u32(0)
		}
		e.u32(1) // atime: server time
		e.u32(2) // mtime: client time
		e.u32(1_000_000)
		e.u32(500) // NTFS keeps 100ns units
		e.u32(0)
	})
	if st != stOK {
		t.Fatalf("SETATTR: %d", st)
	}
	info, err := os.Stat(fs.host("f"))
	if err != nil || !info.ModTime().Equal(time.Unix(1_000_000, 500)) {
		t.Fatalf("mtime %v, %v", info.ModTime(), err)
	}
	if stat, _ := c.call(progNFS, 3, v3SetAttr, func(e *encoder) {
		e.fh(fs.FileID("f"))
		for range 4 {
			e.u32(0)
		}
		e.u32(3) // no such time_how
		e.u32(0)
		e.u32(0)
	}); stat != rpcGarbageArgs {
		t.Fatalf("bad time_how: accept_stat %d", stat)
	}
}

// create3 creates name in the root with mode how, sattr args, and returns
// the status and the new handle.
func (c *rpcClient) create3(name string, how uint32, args func(*encoder)) (status, uint64) {
	c.t.Helper()
	st, d := c.v3call(v3Create, func(e *encoder) {
		dirop(e, 1, name)
		e.u32(how)
		args(e)
	})
	if st != stOK {
		if len(d.buf) != 8 {
			c.t.Fatalf("CREATE failure body of %d bytes", len(d.buf))
		}
		return st, 0
	}
	if !d.bool() {
		c.t.Fatal("CREATE: no handle")
	}
	id, ok := d.fh(maxFH3)
	if a, aok := postOpOf(c.t, d); !ok || !aok || a.fileid != id {
		c.t.Fatal("CREATE: handle and attributes disagree")
	}
	if d.bool() { // no pre-op attributes for the directory
		c.t.Fatal("CREATE: unexpected pre-op attributes")
	}
	if _, ok := postOpOf(c.t, d); !ok || d.err != nil || len(d.buf) != 0 {
		c.t.Fatal("CREATE: malformed directory wcc_data")
	}
	return st, id
}

// TestV3CreateModes checks UNCHECKED, GUARDED and EXCLUSIVE CREATE.
func TestV3CreateModes(t *testing.T) {
	fs := newTestFS(t)
	c := v3server(t, fs)

	st, id := c.create3("new", createGuarded, func(e *encoder) { modeSattr(e, 0o640) })
	if st != stOK || id != fs.FileID("new") {
		t.Fatalf("GUARDED create: %d id %d", st, id)
	}
	if info, err := os.Stat(fs.host("new")); err != nil || info.Mode().Perm() != hostPerm(0o640, false) {
		t.Fatalf("created file: %v %v", info, err)
	}
	fs.write(t, "new", "keep")
	if st, _ := c.create3("new", createGuarded, noSattr); st != stExist {
		t.Fatalf("GUARDED create of an existing file: %d, want EXIST", st)
	}
	if st, id2 := c.create3("new", createUnchecked, func(e *encoder) { modeSattr(e, 0o600) }); st != stOK || id2 != id {
		t.Fatalf("UNCHECKED create of an existing file: %d id %d", st, id2)
	}
	if got, _ := os.ReadFile(fs.host("new")); string(got) != "keep" {
		t.Fatalf("UNCHECKED create truncated the file: %q", got)
	}
	if info, err := os.Stat(fs.host("new")); err != nil || info.Mode().Perm() != hostPerm(0o640, false) {
		t.Fatalf("UNCHECKED create of an existing file changed its mode: %v %v", info, err)
	}
	if err := os.Symlink("new", fs.host("link")); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.create3("link", createUnchecked, noSattr); st != stExist {
		t.Fatalf("UNCHECKED create over a symlink: %d, want EXIST", st)
	}
	if st, _ := c.create3("new", createUnchecked, func(e *encoder) {
		for _, v := range []uint32{0, 0, 0, 1, 0, 0, 0, 0} { // size 0
			e.u32(v)
		}
	}); st != stOK {
		t.Fatalf("UNCHECKED create with size 0: %d", st)
	}
	if got, _ := os.ReadFile(fs.host("new")); len(got) != 0 {
		t.Fatalf("size 0 kept %q", got)
	}
	verf := func(v uint64) func(*encoder) { return func(e *encoder) { e.u64(v) } }
	st, excl := c.create3("excl", createExclusive, verf(42))
	if st != stOK || excl != fs.FileID("excl") {
		t.Fatalf("EXCLUSIVE create: %d id %d", st, excl)
	}
	if st, again := c.create3("excl", createExclusive, verf(42)); st != stOK || again != excl {
		t.Fatalf("retransmitted EXCLUSIVE create: %d id %d", st, again)
	}
	if st, _ := c.create3("excl", createExclusive, verf(43)); st != stExist {
		t.Fatalf("EXCLUSIVE create with another verifier: %d, want EXIST", st)
	}
	if st, _ := c.create3("new", createExclusive, verf(42)); st != stExist {
		t.Fatalf("EXCLUSIVE create of a file it did not make: %d, want EXIST", st)
	}
	if err := os.Mkdir(fs.host("dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.create3("dir", createUnchecked, noSattr); st != stExist {
		t.Fatalf("UNCHECKED create over a directory: %d, want EXIST", st)
	}
	if st, _ := c.create3(".", createUnchecked, noSattr); st != stExist {
		t.Fatalf("create of \".\": %d, want EXIST", st)
	}
	if stat, _ := c.call(progNFS, 3, v3Create, func(e *encoder) { dirop(e, 1, "x"); e.u32(3) }); stat != rpcGarbageArgs {
		t.Fatalf("createhow 3: accept_stat %d", stat)
	}
}

// TestV3WritesAfterOwnerWriteGoes checks that a file created without its
// owner's write permission, or chmodded so, still takes the WRITEs and
// truncates its creator sends after the CREATE or SETATTR (git writes
// objects it created 0444), until its kept file has been idle a while.
func TestV3WritesAfterOwnerWriteGoes(t *testing.T) {
	fs := newTestFS(t)
	srv := &Server{FS: fs}
	srv.v3.kept.idle = 200 * time.Millisecond
	c := dial(t, startServer(t, srv))
	setattr := func(id uint64, sattr func(*encoder)) status {
		t.Helper()
		st, _ := c.v3call(v3SetAttr, func(e *encoder) {
			e.fh(id)
			sattr(e)
			e.u32(0) // no guard
		})
		return st
	}

	st, created := c.create3("created", createGuarded, func(e *encoder) { modeSattr(e, 0o444) })
	if st != stOK {
		t.Fatalf("CREATE 0444: %d", st)
	}
	if st, n, _, _ := c.write3(created, 0, fileSync3, []byte("object")); st != stOK || n != 6 {
		t.Fatalf("WRITE to a file created 0444: %d, %d bytes", st, n)
	}

	fs.write(t, "chmodded", "")
	chmodded := fs.FileID("chmodded")
	if st := setattr(chmodded, func(e *encoder) { modeSattr(e, 0o444) }); st != stOK {
		t.Fatalf("SETATTR 0444: %d", st)
	}
	if st, _, _, _ := c.write3(chmodded, 0, fileSync3, []byte("late data")); st != stOK {
		t.Fatalf("WRITE after SETATTR 0444: %d", st)
	}
	if st := setattr(chmodded, func(e *encoder) {
		for _, v := range []uint32{0, 0, 0, 1, 0, 4, 0, 0} { // size 4
			e.u32(v)
		}
	}); st != stOK {
		t.Fatalf("truncate after SETATTR 0444: %d", st)
	}
	for name, want := range map[string]string{"created": "object", "chmodded": "late"} {
		info, err := os.Stat(fs.host(name))
		got, _ := os.ReadFile(fs.host(name))
		if err != nil || info.Mode().Perm() != 0o444 || string(got) != want {
			t.Fatalf("%s: %v %q, want 0444 %q", name, info, got, want)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for srv.v3.kept.has(keptKey{id: created}) || srv.v3.kept.has(keptKey{id: chmodded}) {
		if time.Now().After(deadline) {
			t.Fatal("kept files never expired")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, _, _, _ := c.write3(created, 0, fileSync3, []byte("x")); st != stAccess {
		t.Fatalf("WRITE once the kept file expired: %d, want ACCES", st)
	}
}

// TestV3MkdirSymlinkReadlink checks MKDIR, SYMLINK and READLINK.
func TestV3MkdirSymlinkReadlink(t *testing.T) {
	fs := newTestFS(t)
	c := v3server(t, fs)
	st, d := c.v3call(v3Mkdir, func(e *encoder) { dirop(e, 1, "d"); modeSattr(e, 0o700) })
	if st != stOK || !d.bool() {
		t.Fatalf("MKDIR: %d", st)
	}
	if id, _ := d.fh(maxFH3); id != fs.FileID("d") {
		t.Fatalf("MKDIR handle %d", id)
	}
	if info, err := os.Stat(fs.host("d")); err != nil || !info.IsDir() || info.Mode().Perm() != hostPerm(0o700, true) {
		t.Fatalf("MKDIR made %v %v", info, err)
	}
	if st, _ := c.v3call(v3Mkdir, func(e *encoder) { dirop(e, 1, "d"); noSattr(e) }); st != stExist {
		t.Fatalf("MKDIR of an existing name: %d", st)
	}
	if st, _ := c.v3call(v3Symlink, func(e *encoder) { dirop(e, 1, "l"); noSattr(e); e.str("d") }); st != stOK {
		t.Fatalf("SYMLINK: %d", st)
	}
	st, d = c.v3call(v3Readlink, func(e *encoder) { e.fh(fs.FileID("l")) })
	if st != stOK {
		t.Fatalf("READLINK: %d", st)
	}
	if a, ok := postOpOf(t, d); !ok || a.typ != uint32(typeLnk) {
		t.Fatalf("READLINK attributes %+v", a)
	}
	if target := d.str(maxPath3); target != "d" || len(d.buf) != 0 {
		t.Fatalf("READLINK target %q", target)
	}
	if st, _ := c.v3call(v3Readlink, func(e *encoder) { e.fh(fs.FileID("d")) }); st != stInval {
		t.Fatalf("READLINK of a directory: %d, want INVAL", st)
	}
	if st, _ := c.v3call(v3Symlink, func(e *encoder) { dirop(e, 1, "l"); noSattr(e); e.str("x") }); st != stExist {
		t.Fatalf("SYMLINK over a link: %d, want EXIST", st)
	}
}

// TestV3RemoveRmdir checks REMOVE and RMDIR statuses and their effect.
func TestV3RemoveRmdir(t *testing.T) {
	fs := newTestFS(t)
	for _, d := range []string{"empty", "full"} {
		if err := os.Mkdir(fs.host(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fs.write(t, "full/f", "")
	fs.write(t, "f", "")
	if err := os.Symlink("full", fs.host("link")); err != nil {
		t.Fatal(err)
	}
	c := v3server(t, fs)
	fid := fs.FileID("f")
	for _, tc := range []struct {
		proc uint32
		name string
		want status
	}{
		{v3Remove, "full", stIsDir},
		{v3Rmdir, "f", stNotDir},
		{v3Rmdir, "full", stNotEmpty},
		{v3Rmdir, ".", stInval},
		{v3Remove, "..", stInval},
		{v3Remove, "missing", stNoEnt},
		{v3Remove, "link", stOK},
		{v3Remove, "f", stOK},
		{v3Rmdir, "empty", stOK},
	} {
		st, d := c.v3call(tc.proc, func(e *encoder) { dirop(e, 1, tc.name) })
		if st != tc.want {
			t.Errorf("procedure %d of %q: %d, want %d", tc.proc, tc.name, st, tc.want)
			continue
		}
		if st == stOK && (!d.bool() || len(d.fixed(24)) != 24 || !d.bool()) {
			t.Errorf("procedure %d of %q: wcc_data incomplete", tc.proc, tc.name)
		}
	}
	for _, p := range []string{"f", "link", "empty"} {
		if _, err := os.Lstat(fs.host(p)); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", p, err)
		}
	}
	if _, err := os.Stat(fs.host("full/f")); err != nil {
		t.Errorf("REMOVE of a link followed it: %v", err)
	}
	if st, _ := c.v3call(v3GetAttr, func(e *encoder) { e.fh(fid) }); st != stStale {
		t.Errorf("handle of a removed file: %d, want STALE", st)
	}
}

// TestV3FSInfoStatPathconf checks the file system procedures' layouts.
func TestV3FSInfoStatPathconf(t *testing.T) {
	c := v3server(t, newTestFS(t))
	root := func(e *encoder) { e.fh(1) }

	st, d := c.v3call(v3FSStat, root)
	if _, ok := postOpOf(t, d); st != stOK || !ok {
		t.Fatalf("FSSTAT: %d", st)
	}
	got := []uint64{d.u64(), d.u64(), d.u64(), d.u64(), d.u64(), d.u64(), uint64(d.u32())}
	want := []uint64{4096 << 20, 4096 << 19, 4096 << 18, 1 << 20, 1 << 19, 1 << 19, 0}
	if !slices.Equal(got, want) || len(d.buf) != 0 {
		t.Errorf("FSSTAT %v, want %v", got, want)
	}

	st, d = c.v3call(v3FSInfo, root)
	if _, ok := postOpOf(t, d); st != stOK || !ok {
		t.Fatalf("FSINFO: %d", st)
	}
	var w [7]uint32
	for i := range w {
		w[i] = d.u32()
	}
	maxSize, delta, props := d.u64(), [2]uint32{d.u32(), d.u32()}, d.u32()
	if w != [7]uint32{maxIO, maxIO, 4096, maxIO, maxIO, 4096, 8192} || maxSize != 1<<62 || delta != [2]uint32{0, 1} || props != 0x1b || len(d.buf) != 0 {
		t.Errorf("FSINFO %v %d %v %#x", w, maxSize, delta, props)
	}

	st, d = c.v3call(v3PathConf, root)
	if _, ok := postOpOf(t, d); st != stOK || !ok {
		t.Fatalf("PATHCONF: %d", st)
	}
	if pc := [6]uint32{d.u32(), d.u32(), d.u32(), d.u32(), d.u32(), d.u32()}; pc != [6]uint32{maxLink, 255, 1, 0, 0, 1} || len(d.buf) != 0 {
		t.Errorf("PATHCONF %v", pc)
	}

	st, d = c.v3call(v3Access, func(e *encoder) { e.fh(1); e.u32(0x3f) })
	if _, ok := postOpOf(t, d); st != stOK || !ok || d.u32() != 0x3f || len(d.buf) != 0 {
		t.Errorf("ACCESS: %d", st)
	}
}

// slowFS blocks Lstat of "slow" until released.
type slowFS struct {
	*testFS
	release chan struct{}
	entered chan struct{} // if set, receives one value per blocked Lstat
}

// Lstat waits for the release of "slow".
func (f *slowFS) Lstat(p string) (os.FileInfo, error) {
	if p == "slow" {
		if f.entered != nil {
			f.entered <- struct{}{}
		}
		<-f.release
	}
	return f.testFS.Lstat(p)
}

// send writes an NFSv3 call without waiting for its reply.
func (c *rpcClient) send(xid, proc uint32, args func(*encoder)) {
	c.t.Helper()
	e := encoder{}
	for _, v := range []uint32{0, xid, msgCall, 2, progNFS, 3, proc, authNone, 0, authNone, 0} {
		e.u32(v)
	}
	if args != nil {
		args(&e)
	}
	e.put32(0, lastFragment|uint32(e.len()-4))
	if _, err := c.nc.Write(e.buf); err != nil {
		c.t.Fatal(err)
	}
}

// replyXID reads one reply and returns its xid; false if none arrives in
// time.
func (c *rpcClient) replyXID(timeout time.Duration) (uint32, bool) {
	c.t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = c.nc.SetReadDeadline(time.Time{}) }()
	rec, err := readRecord(c.r, nil)
	if err != nil {
		if os.IsTimeout(err) {
			return 0, false
		}
		c.t.Fatal(err)
	}
	return binary.BigEndian.Uint32(rec), true
}

// TestV3ConcurrencyLimit checks that with Concurrency 2 and
// two blocked calls, a third call is held back (the bound) and answered once
// one finishes.
func TestV3ConcurrencyLimit(t *testing.T) {
	fs := &slowFS{testFS: newTestFS(t), release: make(chan struct{})}
	fs.write(t, "slow", "")
	var once sync.Once
	release := func() { once.Do(func() { close(fs.release) }) }
	t.Cleanup(release)
	addr := startServer(t, &Server{FS: fs, Concurrency: 2})
	slow := fs.FileID("slow")

	c := dial(t, addr)
	c.send(1, v3GetAttr, func(e *encoder) { e.fh(slow) })
	c.send(2, v3GetAttr, func(e *encoder) { e.fh(slow) })
	c.send(3, v3GetAttr, func(e *encoder) { e.fh(1) })
	if xid, ok := c.replyXID(200 * time.Millisecond); ok {
		t.Fatalf("reply %d while both workers block; want the third call held back", xid)
	}
	release()
	seen := map[uint32]bool{}
	for range 3 {
		xid, ok := c.replyXID(5 * time.Second)
		if !ok {
			t.Fatalf("missing replies after release; got %v", seen)
		}
		seen[xid] = true
	}
	if !seen[1] || !seen[2] || !seen[3] {
		t.Fatalf("replies %v, want xids 1, 2, 3", seen)
	}

}

// TestV3ServeDrainsAcceptedCallsOnDisconnect runs conn.serve directly: after
// the client disconnects with calls blocked in the backend, serve must not
// return until those calls finish, and must return once they do.
func TestV3ServeDrainsAcceptedCallsOnDisconnect(t *testing.T) {
	fs := &slowFS{testFS: newTestFS(t), release: make(chan struct{}), entered: make(chan struct{}, 2)}
	fs.write(t, "slow", "")
	var once sync.Once
	release := func() { once.Do(func() { close(fs.release) }) }
	t.Cleanup(release)
	srv := &Server{FS: fs, Concurrency: 2}
	t.Cleanup(func() { closeKept(srv) })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := dial(t, l.Addr().String())
	sc, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() {
		(&conn{srv: srv, nc: sc}).serve()
		close(returned)
	}()

	slow := fs.FileID("slow")
	c.send(1, v3GetAttr, func(e *encoder) { e.fh(slow) })
	c.send(2, v3GetAttr, func(e *encoder) { e.fh(slow) })
	for range 2 {
		select {
		case <-fs.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("calls never reached the backend")
		}
	}
	c.nc.Close() // serve sees EOF with both calls accepted and blocked
	select {
	case <-returned:
		t.Fatal("serve returned while accepted calls were still blocked")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after blocked calls were released")
	}
}

// TestV3ConcurrentCalls checks that with Concurrency above one, a slow call
// does not hold up the next on its connection; without, calls are answered
// in order.
func TestV3ConcurrentCalls(t *testing.T) {
	for _, conc := range []int{0, 1, 4} {
		fs := &slowFS{testFS: newTestFS(t), release: make(chan struct{})}
		fs.write(t, "slow", "")
		var once sync.Once
		release := func() { once.Do(func() { close(fs.release) }) }
		t.Cleanup(release)
		c := dial(t, startServer(t, &Server{FS: fs, Concurrency: conc}))
		slow := fs.FileID("slow")

		c.send(1, v3GetAttr, func(e *encoder) { e.fh(slow) })
		c.send(2, v3GetAttr, func(e *encoder) { e.fh(1) })
		if conc > 1 {
			if xid, ok := c.replyXID(5 * time.Second); !ok || xid != 2 {
				t.Fatalf("Concurrency %d: first reply %d (%v), want 2 while 1 blocks", conc, xid, ok)
			}
			release()
			if xid, ok := c.replyXID(5 * time.Second); !ok || xid != 1 {
				t.Fatalf("Concurrency %d: second reply %d (%v), want 1", conc, xid, ok)
			}
			continue
		}
		if xid, ok := c.replyXID(200 * time.Millisecond); ok {
			t.Fatalf("Concurrency %d: reply %d while call 1 blocks", conc, xid)
		}
		release()
		for _, want := range []uint32{1, 2} {
			if xid, ok := c.replyXID(5 * time.Second); !ok || xid != want {
				t.Fatalf("Concurrency %d: reply %d (%v), want %d", conc, xid, ok, want)
			}
		}
	}
}
