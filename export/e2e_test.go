//go:build !windows

package export_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	exportfs "github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
)

// stackFixture owns a real captured base, persisted registry, router, and protocol export.
type stackFixture struct {
	reg    *checkout.Registry
	export *exportfs.Export
	lower  string
	state  string
}

// newStack prepares all lower files before capture and uses no kernel mounts or external services.
func newStack(t *testing.T, files map[string]string) *stackFixture {
	t.Helper()
	return newStackWithMemory(t, files, overlay.NewMemory(64<<20, 1<<20, 0))
}

// newStackWithMemory also allows protocol tests to exercise disk-only uppers.
func newStackWithMemory(t *testing.T, files map[string]string, memory *overlay.Memory) *stackFixture {
	t.Helper()
	lower := t.TempDir()
	for name, data := range files {
		p := filepath.Join(lower, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	state, mount := t.TempDir(), t.TempDir()
	var e *exportfs.Export
	reg, err := checkout.Open(checkout.Config{State: state, Mount: mount, Memory: memory, BlobCache: 64 << 20, Retire: func(name string) {
		if e != nil {
			e.Retire(name)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reg.Close(); err != nil {
			t.Error(err)
		}
	})
	e, err = exportfs.New(exportfs.NewRouter(reg), filepath.Join(state, "nodes.log"), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	return &stackFixture{reg: reg, export: e, lower: lower, state: state}
}

// create adds a checkout through the same registry API used by the admin server.
func (f *stackFixture) create(t *testing.T, name string) {
	t.Helper()
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: f.lower, Name: name}); err != nil {
		t.Fatal(err)
	}
}

// nfsStatus distinguishes server statuses from transport and decoder failures.
func nfsStatus(t *testing.T, got, want uint32, err error) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("NFS status %d, want %d; transport/decode error: %v", got, want, err)
	}
}

// lookup traverses names one component at a time so every access goes through LOOKUP RPCs.
func lookup(t *testing.T, c *nfstest.Client, path string) []byte {
	t.Helper()
	fh := c.Root
	for name := range strings.SplitSeq(path, "/") {
		if name == "" {
			continue
		}
		next, _, status, err := c.Lookup(fh, name)
		nfsStatus(t, status, nfstest.OK, err)
		fh = next
	}
	return fh
}

// nfsRead asserts small-file contents over READ rather than inspecting the backing upper.
func nfsRead(t *testing.T, c *nfstest.Client, fh []byte, want string) {
	t.Helper()
	data, eof, status, err := c.Read(fh, 0, 1<<20)
	nfsStatus(t, status, nfstest.OK, err)
	if string(data) != want || !eof {
		t.Fatalf("READ = %q, EOF %t; want %q, EOF true", data, eof, want)
	}
}

// nfsWrite requires a full stable write.
func nfsWrite(t *testing.T, c *nfstest.Client, fh []byte, data string) {
	t.Helper()
	n, status, err := c.Write(fh, 0, []byte(data))
	nfsStatus(t, status, nfstest.OK, err)
	if n != uint32(len(data)) {
		t.Fatalf("WRITE accepted %d of %d bytes", n, len(data))
	}
}

// nfsCreate creates an upper entry and fills it through protocol writes.
func nfsCreate(t *testing.T, c *nfstest.Client, dir []byte, name, data string) []byte {
	t.Helper()
	fh, status, err := c.Create(dir, name, 0644)
	nfsStatus(t, status, nfstest.OK, err)
	nfsWrite(t, c, fh, data)
	return fh
}

// nfsNames validates READDIRPLUS attributes and handles as well as the merged name set.
func nfsNames(t *testing.T, c *nfstest.Client, dir []byte, want ...string) {
	t.Helper()
	entries, status, err := c.ReadDirPlus(dir)
	nfsStatus(t, status, nfstest.OK, err)
	var names []string
	for _, entry := range entries {
		if entry.FileName == "." || entry.FileName == ".." {
			continue
		}
		if !entry.Attr.IsSet || !entry.Handle.IsSet {
			t.Fatalf("READDIRPLUS omitted attributes or handle for %q", entry.FileName)
		}
		attr, status, err := c.GetAttr(entry.Handle.FH)
		nfsStatus(t, status, nfstest.OK, err)
		if entry.FileId != attr.Fileid || entry.Attr.Attr.Fileid != attr.Fileid {
			t.Fatalf("READDIRPLUS file ids for %q disagree with GETATTR", entry.FileName)
		}
		names = append(names, entry.FileName)
	}
	sort.Strings(names)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) && !(len(names) == 0 && len(want) == 0) {
		t.Fatalf("READDIRPLUS = %v, want %v", names, want)
	}
}

// TestNFSCheckoutRouting exercises virtual-root protection, isolation, EXDEV, and handle retirement.
func TestNFSCheckoutRouting(t *testing.T) {
	f := newStack(t, map[string]string{"source": "lower"})
	c := nfstest.Start(t, f.export.NFS())
	rootAttr, rootStatus, rootErr := c.GetAttr(c.Root)
	nfsStatus(t, rootStatus, nfstest.OK, rootErr)
	if rootAttr.FileMode&0777 != 0555 {
		t.Fatalf("export root mode = %#o, want 0555", rootAttr.FileMode)
	}
	nfsNames(t, c, c.Root)
	f.create(t, "alpha")
	f.create(t, "beta")
	nfsNames(t, c, c.Root, "alpha", "beta")
	a, b := lookup(t, c, "alpha"), lookup(t, c, "beta")
	_, status, err := c.Mkdir(c.Root, "forbidden", 0755)
	nfsStatus(t, status, nfstest.Access, err)
	af, bf := lookup(t, c, "alpha/source"), lookup(t, c, "beta/source")
	nfsWrite(t, c, af, "alpha")
	nfsRead(t, c, af, "alpha")
	nfsRead(t, c, bf, "lower")
	status, err = c.Rename(a, "source", b, "moved")
	nfsStatus(t, status, nfstest.XDev, err)
	nfsRead(t, c, af, "alpha")
	nfsRead(t, c, bf, "lower")
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "alpha"}); err != nil {
		t.Fatal(err)
	}
	nfsNames(t, c, c.Root, "beta")
	for _, fh := range [][]byte{a, af} {
		_, status, err := c.GetAttr(fh)
		nfsStatus(t, status, nfstest.Stale, err)
	}
	_, _, status, err = c.Read(af, 0, 10)
	nfsStatus(t, status, nfstest.Stale, err)
}

// TestNFSOverlayRPCs covers copy-up, whiteout/recreation, symlinks, merged listings, and renames across layers.
func TestNFSOverlayRPCs(t *testing.T) {
	f := newStack(t, map[string]string{"source": "lower", "dir/low": "low", "dir/deep/base": "base", "opaque/old": "old"})
	f.create(t, "agent")
	c := nfstest.Start(t, f.export.NFS())
	root, dir := lookup(t, c, "agent"), lookup(t, c, "agent/dir")
	original := lookup(t, c, "agent/source")
	before, status, err := c.GetAttr(original)
	nfsStatus(t, status, nfstest.OK, err)
	nfsWrite(t, c, original, "upper")
	after, status, err := c.GetAttr(original)
	nfsStatus(t, status, nfstest.OK, err)
	if before.Fileid != after.Fileid || before.FileMode != after.FileMode {
		t.Fatal("copy-up changed stable identity or mode")
	}
	data, err := os.ReadFile(filepath.Join(f.lower, "source"))
	if err != nil || string(data) != "lower" {
		t.Fatalf("copy-up changed lower: %q, %v", data, err)
	}
	status, err = c.Remove(root, "source")
	nfsStatus(t, status, nfstest.OK, err)
	_, _, status, err = c.Lookup(root, "source")
	nfsStatus(t, status, nfstest.NoEnt, err)
	nfsCreate(t, c, root, "source", "fresh")
	nfsRead(t, c, lookup(t, c, "agent/source"), "fresh")
	link, status, err := c.Symlink(root, "link", "dir/deep/base")
	nfsStatus(t, status, nfstest.OK, err)
	target, status, err := c.Readlink(link)
	nfsStatus(t, status, nfstest.OK, err)
	if target != "dir/deep/base" {
		t.Fatalf("READLINK = %q", target)
	}
	nfsCreate(t, c, dir, "one", "one")
	deep := lookup(t, c, "agent/dir/deep/base")
	deepBefore, status, err := c.GetAttr(deep)
	nfsStatus(t, status, nfstest.OK, err)
	if _, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "agent", Label: "first layer", Wait: true}); err != nil {
		t.Fatal(err)
	}
	nfsCreate(t, c, dir, "two", "two")
	if _, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "agent", Label: "second layer", Wait: true}); err != nil {
		t.Fatal(err)
	}
	nfsCreate(t, c, dir, "three", "three")
	status, err = c.Remove(dir, "low")
	nfsStatus(t, status, nfstest.OK, err)
	nfsNames(t, c, dir, "deep", "one", "two", "three")
	deepAfter, status, err := c.GetAttr(deep)
	nfsStatus(t, status, nfstest.OK, err)
	if deepAfter.Fileid != deepBefore.Fileid {
		t.Fatal("snapshots changed the NFS file id")
	}
	status, err = c.Rename(root, "dir", root, "moved")
	nfsStatus(t, status, nfstest.OK, err)
	_, _, status, err = c.Lookup(root, "dir")
	nfsStatus(t, status, nfstest.NoEnt, err)
	nfsNames(t, c, lookup(t, c, "agent/moved"), "deep", "one", "two", "three")
	nfsRead(t, c, deep, "base")
	for _, name := range []string{"one", "two", "three"} {
		nfsRead(t, c, lookup(t, c, "agent/moved/"+name), name)
	}
	opaque := lookup(t, c, "agent/opaque")
	status, err = c.Remove(opaque, "old")
	nfsStatus(t, status, nfstest.OK, err)
	status, err = c.Rmdir(root, "opaque")
	nfsStatus(t, status, nfstest.OK, err)
	opaque, status, err = c.Mkdir(root, "opaque", 0755)
	nfsStatus(t, status, nfstest.OK, err)
	nfsNames(t, c, opaque)
	nfsCreate(t, c, opaque, "new", "new")
	nfsNames(t, c, opaque, "new")
}

// TestNFSForkIsolation ensures parent/child writes and child whiteouts cannot mutate shared frozen layers.
func TestNFSForkIsolation(t *testing.T) {
	f := newStack(t, map[string]string{"base": "before"})
	f.create(t, "parent")
	c := nfstest.Start(t, f.export.NFS())
	parent := lookup(t, c, "parent")
	shared := nfsCreate(t, c, parent, "shared", "shared")
	if _, err := f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "parent", Name: "child", Label: "fork"}); err != nil {
		t.Fatal(err)
	}
	child := lookup(t, c, "child")
	nfsNames(t, c, c.Root, "child", "parent")
	nfsWrite(t, c, lookup(t, c, "parent/base"), "parent")
	nfsRead(t, c, lookup(t, c, "child/base"), "before")
	nfsWrite(t, c, lookup(t, c, "child/base"), "child!")
	nfsRead(t, c, lookup(t, c, "parent/base"), "parent")
	nfsRead(t, c, lookup(t, c, "child/base"), "child!")
	status, err := c.Remove(child, "shared")
	nfsStatus(t, status, nfstest.OK, err)
	_, _, status, err = c.Lookup(child, "shared")
	nfsStatus(t, status, nfstest.NoEnt, err)
	nfsRead(t, c, shared, "shared")
	nfsCreate(t, c, parent, "only-parent", "parent")
	_, _, status, err = c.Lookup(child, "only-parent")
	nfsStatus(t, status, nfstest.NoEnt, err)
	nfsCreate(t, c, child, "only-child", "child")
	_, _, status, err = c.Lookup(parent, "only-child")
	nfsStatus(t, status, nfstest.NoEnt, err)
}

// SMB wire statuses keep assertions independent of the server's private constants.
const (
	smbOK     uint32 = 0
	smbDenied uint32 = 0xc0000022
	smbXDev   uint32 = 0xc00000d4
	smbClosed uint32 = 0xc0000128
)

// smbID is the opaque persistent/volatile pair returned by SMB CREATE.
type smbID [16]byte

// smbReply exposes the status and payload of one independently decoded SMB2 response.
type smbReply struct {
	raw, body []byte
	status    uint32
}

// smbWire is a small anonymous SMB2.1 client; no server-side codecs are reused.
type smbWire struct {
	t                *testing.T
	conn             net.Conn
	message, session uint64
	tree             uint32
}

// utf16le encodes names exactly as SMB2 requires.
func utf16le(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// smbExpect reports the protocol status at the calling test site.
func smbExpect(t *testing.T, reply smbReply, status uint32) {
	t.Helper()
	if reply.status != status {
		t.Fatalf("SMB status %#08x, want %#08x", reply.status, status)
	}
}

// startSMB serves the same Export backend used by NFS and completes real NTLM anonymous authentication.
func startSMB(t *testing.T, f *stackFixture) *smbWire {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smb.Server{FS: f.export.SMB(), Share: "agents"}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	t.Cleanup(func() { _ = l.Close(); <-done })
	nc, err := net.DialTimeout("tcp", l.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	c := &smbWire{t: t, conn: nc}
	b := make([]byte, 38)
	binary.LittleEndian.PutUint16(b, 36)
	binary.LittleEndian.PutUint16(b[2:], 1)
	binary.LittleEndian.PutUint16(b[4:], 1)
	binary.LittleEndian.PutUint32(b[8:], 4)
	binary.LittleEndian.PutUint16(b[36:], 0x0210)
	smbExpect(t, c.exchange(0, b), smbOK)
	first := make([]byte, 32)
	copy(first, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(first[8:], 1)
	binary.LittleEndian.PutUint32(first[12:], 0xa0888215)
	r := c.setup(first)
	smbExpect(t, r, 0xc0000016)
	c.session = binary.LittleEndian.Uint64(r.raw[40:])
	last := make([]byte, 64)
	copy(last, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(last[8:], 3)
	binary.LittleEndian.PutUint32(last[60:], 0xa0888a15)
	binary.LittleEndian.PutUint16(last[12:], 1)
	binary.LittleEndian.PutUint16(last[14:], 1)
	binary.LittleEndian.PutUint32(last[16:], 64)
	for _, off := range []int{20, 28, 36, 44, 52} {
		binary.LittleEndian.PutUint32(last[off+4:], 65)
	}
	r = c.setup(append(last, 0))
	smbExpect(t, r, smbOK)
	c.session = binary.LittleEndian.Uint64(r.raw[40:])
	name := utf16le(`\\127.0.0.1\agents`)
	b = make([]byte, 8)
	binary.LittleEndian.PutUint16(b, 9)
	binary.LittleEndian.PutUint16(b[4:], 72)
	binary.LittleEndian.PutUint16(b[6:], uint16(len(name)))
	r = c.exchange(3, append(b, name...))
	smbExpect(t, r, smbOK)
	c.tree = binary.LittleEndian.Uint32(r.raw[36:])
	return c
}

// exchange sends and validates a bounded standalone Direct TCP SMB2 packet.
func (c *smbWire) exchange(command uint16, body []byte) smbReply {
	c.t.Helper()
	if err := c.conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	charge := uint64(1)
	if command == 8 || command == 9 {
		if len(body) < 8 {
			c.t.Fatal("short SMB READ/WRITE request")
		}
		size := uint64(binary.LittleEndian.Uint32(body[4:]))
		if size != 0 {
			charge = (size + 65535) / 65536
		}
		if charge > 65535 {
			c.t.Fatalf("SMB request requires unrepresentable credit charge %d", charge)
		}
	}
	msg := make([]byte, 64, 64+len(body))
	copy(msg, "\xfeSMB")
	binary.LittleEndian.PutUint16(msg[4:], 64)
	binary.LittleEndian.PutUint16(msg[6:], uint16(charge))
	binary.LittleEndian.PutUint16(msg[12:], command)
	binary.LittleEndian.PutUint16(msg[14:], 32)
	binary.LittleEndian.PutUint64(msg[24:], c.message)
	binary.LittleEndian.PutUint32(msg[32:], 0x1234)
	binary.LittleEndian.PutUint32(msg[36:], c.tree)
	binary.LittleEndian.PutUint64(msg[40:], c.session)
	msg = append(msg, body...)
	frame := append([]byte{0, byte(len(msg) >> 16), byte(len(msg) >> 8), byte(len(msg))}, msg...)
	if _, err := io.Copy(c.conn, bytes.NewReader(frame)); err != nil {
		c.t.Fatal(err)
	}
	var h [4]byte
	if _, err := io.ReadFull(c.conn, h[:]); err != nil {
		c.t.Fatal(err)
	}
	n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if h[0] != 0 || n < 64 || n > 16<<20 {
		c.t.Fatalf("invalid SMB frame header %x", h)
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(c.conn, raw); err != nil {
		c.t.Fatal(err)
	}
	if string(raw[:4]) != "\xfeSMB" || binary.LittleEndian.Uint16(raw[4:]) != 64 || binary.LittleEndian.Uint16(raw[12:]) != command || binary.LittleEndian.Uint64(raw[24:]) != c.message || binary.LittleEndian.Uint32(raw[16:])&1 == 0 {
		c.t.Fatal("SMB response envelope does not match request")
	}
	c.message += charge
	return smbReply{raw: raw, body: raw[64:], status: binary.LittleEndian.Uint32(raw[8:])}
}

// setup wraps an NTLM token in SESSION_SETUP without SPNEGO, as permitted by SMB2.
func (c *smbWire) setup(token []byte) smbReply {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b, 25)
	b[3] = 1
	binary.LittleEndian.PutUint16(b[12:], 88)
	binary.LittleEndian.PutUint16(b[14:], uint16(len(token)))
	return c.exchange(1, append(b, token...))
}

// open issues CREATE for a file or directory and retains error statuses.
func (c *smbWire) open(name string, disposition, options uint32) (smbID, smbReply) {
	c.t.Helper()
	b := make([]byte, 56)
	binary.LittleEndian.PutUint16(b, 57)
	binary.LittleEndian.PutUint32(b[4:], 2)
	binary.LittleEndian.PutUint32(b[24:], 0xc0010080)
	binary.LittleEndian.PutUint32(b[28:], 0x80)
	binary.LittleEndian.PutUint32(b[32:], 7)
	binary.LittleEndian.PutUint32(b[36:], disposition)
	binary.LittleEndian.PutUint32(b[40:], options)
	nameBytes := utf16le(name)
	binary.LittleEndian.PutUint16(b[44:], 120)
	binary.LittleEndian.PutUint16(b[46:], uint16(len(nameBytes)))
	r := c.exchange(5, append(b, nameBytes...))
	var id smbID
	if r.status == smbOK {
		if len(r.body) < 88 {
			c.t.Fatal("short SMB CREATE response")
		}
		copy(id[:], r.body[64:80])
	}
	return id, r
}

// mustOpen requires a successful file open with broad sharing enabled.
func (c *smbWire) mustOpen(name string, disposition uint32) smbID {
	c.t.Helper()
	id, r := c.open(name, disposition, 0)
	smbExpect(c.t, r, smbOK)
	return id
}

// close releases an SMB handle before subsequent filesystem mutations.
func (c *smbWire) close(id smbID) {
	c.t.Helper()
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b, 24)
	copy(b[8:], id[:])
	smbExpect(c.t, c.exchange(6, b), smbOK)
}

// write transfers a complete small file through a retained SMB open.
func (c *smbWire) write(id smbID, data string) {
	c.t.Helper()
	b := make([]byte, 48)
	binary.LittleEndian.PutUint16(b, 49)
	binary.LittleEndian.PutUint16(b[2:], 112)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	copy(b[16:], id[:])
	r := c.exchange(9, append(b, data...))
	smbExpect(c.t, r, smbOK)
	if len(r.body) < 8 || binary.LittleEndian.Uint32(r.body[4:]) != uint32(len(data)) {
		c.t.Fatal("short SMB WRITE")
	}
}

// read retains status so reads from retired handles can assert STATUS_FILE_CLOSED.
func (c *smbWire) read(id smbID) ([]byte, smbReply) {
	c.t.Helper()
	b := make([]byte, 49)
	binary.LittleEndian.PutUint16(b, 49)
	binary.LittleEndian.PutUint32(b[4:], 1<<20)
	copy(b[16:], id[:])
	r := c.exchange(8, b)
	if r.status != smbOK {
		return nil, r
	}
	if len(r.body) < 16 {
		c.t.Fatal("short SMB READ")
	}
	return c.payload(r, int(r.body[2]), int(binary.LittleEndian.Uint32(r.body[4:]))), r
}

// payload bounds-checks an absolute-offset SMB response buffer.
func (c *smbWire) payload(r smbReply, offset, size int) []byte {
	c.t.Helper()
	if offset < 64 || size < 0 || offset > len(r.raw) || size > len(r.raw)-offset {
		c.t.Fatal("invalid SMB response buffer")
	}
	return r.raw[offset : offset+size]
}

// contents checks bytes on an existing open without closing it.
func (c *smbWire) contents(id smbID, want string) {
	c.t.Helper()
	data, r := c.read(id)
	smbExpect(c.t, r, smbOK)
	if string(data) != want {
		c.t.Fatalf("SMB READ = %q, want %q", data, want)
	}
}

// queryInfo decodes a file or security information response independently of the server.
func (c *smbWire) queryInfo(id smbID, kind, class byte, additional uint32) []byte {
	c.t.Helper()
	b := make([]byte, 40)
	binary.LittleEndian.PutUint16(b, 41)
	b[2], b[3] = kind, class
	binary.LittleEndian.PutUint32(b[4:], 4096)
	binary.LittleEndian.PutUint32(b[16:], additional)
	copy(b[24:], id[:])
	r := c.exchange(16, b)
	smbExpect(c.t, r, smbOK)
	if len(r.body) < 8 {
		c.t.Fatal("short SMB QUERY_INFO response")
	}
	return c.payload(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
}

// rename submits FILE_RENAME_INFORMATION against an open file.
func (c *smbWire) rename(id smbID, target string) smbReply {
	name := utf16le(target)
	data := make([]byte, 20)
	binary.LittleEndian.PutUint32(data[16:], uint32(len(name)))
	data = append(data, name...)
	b := make([]byte, 32)
	binary.LittleEndian.PutUint16(b, 33)
	b[2], b[3] = 1, 10
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	binary.LittleEndian.PutUint16(b[8:], 96)
	copy(b[16:], id[:])
	return c.exchange(17, append(b, data...))
}

// names reads a complete small directory using FILE_NAMES_INFORMATION.
func (c *smbWire) names(path string, want ...string) {
	c.t.Helper()
	id, r := c.open(path, 1, 1)
	smbExpect(c.t, r, smbOK)
	defer c.close(id)
	b := make([]byte, 32)
	binary.LittleEndian.PutUint16(b, 33)
	b[2], b[3] = 12, 1
	copy(b[8:], id[:])
	binary.LittleEndian.PutUint16(b[24:], 96)
	pattern := utf16le("*")
	binary.LittleEndian.PutUint16(b[26:], uint16(len(pattern)))
	binary.LittleEndian.PutUint32(b[28:], 65536)
	r = c.exchange(14, append(b, pattern...))
	smbExpect(c.t, r, smbOK)
	if len(r.body) < 8 {
		c.t.Fatal("short SMB QUERY_DIRECTORY")
	}
	data := c.payload(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
	var names []string
	for len(data) != 0 {
		if len(data) < 12 {
			c.t.Fatal("short SMB directory entry")
		}
		next, length := int(binary.LittleEndian.Uint32(data)), int(binary.LittleEndian.Uint32(data[8:]))
		if length%2 != 0 || length > len(data)-12 {
			c.t.Fatal("invalid SMB directory name")
		}
		u := make([]uint16, length/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(data[12+2*i:])
		}
		name := string(utf16.Decode(u))
		if name != "." && name != ".." {
			names = append(names, name)
		}
		if next == 0 {
			break
		}
		if next < 12+length || next > len(data) || next%8 != 0 {
			c.t.Fatal("invalid SMB directory continuation")
		}
		data = data[next:]
	}
	sort.Strings(names)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		c.t.Fatalf("SMB listing %q = %v, want %v", path, names, want)
	}
}

// TestSMBCheckoutRouting checks the same isolation and error boundaries using actual SMB2 packets.
func TestSMBCheckoutRouting(t *testing.T) {
	f := newStack(t, map[string]string{"source": "lower"})
	f.create(t, "alpha")
	f.create(t, "beta")
	c := startSMB(t, f)
	c.names("", "alpha", "beta")
	_, r := c.open("forbidden", 2, 1)
	smbExpect(t, r, smbDenied)
	a, b := c.mustOpen(`alpha\source`, 1), c.mustOpen(`beta\source`, 1)
	c.write(a, "alpha")
	c.contents(a, "alpha")
	c.contents(b, "lower")
	smbExpect(t, c.rename(a, `beta\moved`), smbXDev)
	c.contents(a, "alpha")
	c.contents(b, "lower")
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "alpha"}); err != nil {
		t.Fatal(err)
	}
	c.names("", "beta")
	_, r = c.read(a)
	smbExpect(t, r, smbClosed)
	c.contents(b, "lower")
}

// TestSMBOpenAcrossCheckpoint catches writes into an immutable layer through an old persistent SMB open.
func TestSMBOpenAcrossCheckpoint(t *testing.T) {
	f := newStack(t, map[string]string{"base": "before"})
	f.create(t, "parent")
	c := startSMB(t, f)
	old := c.mustOpen(`parent\shared`, 2)
	c.write(old, "before")
	if _, err := f.reg.Snap(context.Background(), api.SnapArgs{Checkout: "parent", Label: "freeze open file", Wait: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.Fork(context.Background(), api.ForkArgs{Checkout: "parent", Name: "child", Label: "fork frozen file"}); err != nil {
		t.Fatal(err)
	}
	child := c.mustOpen(`child\shared`, 1)
	c.contents(child, "before")
	c.write(old, "parent")
	c.contents(old, "parent")
	c.contents(child, "before")
	fresh := c.mustOpen(`parent\shared`, 1)
	c.contents(fresh, "parent")
	c.write(child, "child!")
	c.contents(child, "child!")
	c.contents(old, "parent")
	c.contents(fresh, "parent")
	c.close(fresh)
	c.close(child)
	c.close(old)
}

// TestSMBDirectoryLinks describes a symlink to a directory as NTFS does: its
// attributes carry FILE_ATTRIBUTE_DIRECTORY, and RemoveDirectory's open of the
// link itself (FILE_DIRECTORY_FILE, FILE_OPEN_REPARSE_POINT) removes the link,
// not the directory it names.
func TestSMBDirectoryLinks(t *testing.T) {
	f := newStack(t, map[string]string{"dir/in": "deep", "file": "lower"})
	f.create(t, "alpha")
	nodes := f.export.Nodes()
	for link, target := range map[string]string{"alpha/dirlink": "dir", "alpha/filelink": "file"} {
		if err := nodes.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	c := startSMB(t, f)
	for name, attrs := range map[string]uint32{`alpha\dirlink`: 0x410, `alpha\filelink`: 0x420} {
		id, r := c.open(name, 1, 0x200000)
		smbExpect(t, r, smbOK)
		if got := binary.LittleEndian.Uint32(c.queryInfo(id, 1, 35, 0)); got != attrs {
			t.Errorf("%s attributes = %#x, want %#x", name, got, attrs)
		}
		c.close(id)
	}
	id, r := c.open(`alpha\dirlink`, 1, 0x201001) // FILE_DIRECTORY_FILE|FILE_DELETE_ON_CLOSE|FILE_OPEN_REPARSE_POINT
	smbExpect(t, r, smbOK)
	c.close(id)
	if _, err := nodes.Lstat("alpha/dirlink"); !os.IsNotExist(err) {
		t.Fatalf("link survived removal: %v", err)
	}
	if _, err := nodes.Stat("alpha/dir/in"); err != nil {
		t.Fatalf("removing the link touched its target: %v", err)
	}
}
