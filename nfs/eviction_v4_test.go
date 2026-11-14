//go:build !windows

package nfs_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/nodestest"
	"github.com/stencil-hq/ovnfs/nfs"
)

type xdr4 []byte

func (b *xdr4) u(n uint32) { *b = binary.BigEndian.AppendUint32(*b, n) }
func (b *xdr4) q(n uint64) { *b = binary.BigEndian.AppendUint64(*b, n) }
func (b *xdr4) opaque(data []byte) {
	b.u(uint32(len(data)))
	*b = append(*b, data...)
	for len(*b)%4 != 0 {
		*b = append(*b, 0)
	}
}
func (b *xdr4) str(s string) { b.opaque([]byte(s)) }
func (b *xdr4) take(n int) []byte {
	if n > len(*b) {
		panic("short XDR reply")
	}
	v := (*b)[:n]
	*b = (*b)[n:]
	return v
}
func (b *xdr4) word() uint32 { return binary.BigEndian.Uint32(b.take(4)) }
func (b *xdr4) blob() []byte { n := b.word(); v := b.take(int(n)); b.take(int(-n & 3)); return v }
func op4(n uint32, body func(*xdr4)) xdr4 {
	b := xdr4{}
	b.u(n)
	if body != nil {
		body(&b)
	}
	return b
}
func put4(id uint64) xdr4 {
	return op4(22, func(b *xdr4) { h := binary.BigEndian.AppendUint64(nil, id); b.opaque(h) })
}
func stateOp4(n uint32, id []byte) xdr4 {
	return op4(n, func(b *xdr4) {
		if n == 4 {
			b.u(1)
		}
		*b = append(*b, id...)
	})
}

func record4(c net.Conn) ([]byte, error) {
	var out []byte
	for {
		var word [4]byte
		if _, err := io.ReadFull(c, word[:]); err != nil {
			return nil, err
		}
		mark := binary.BigEndian.Uint32(word[:])
		if mark&0x7fffffff > 4<<20 {
			return nil, fmt.Errorf("oversize RPC record")
		}
		data := make([]byte, mark&0x7fffffff)
		if _, err := io.ReadFull(c, data); err != nil {
			return nil, err
		}
		out = append(out, data...)
		if mark>>31 != 0 {
			return out, nil
		}
	}
}
func send4(c net.Conn, b []byte) error {
	_, err := c.Write(append(binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(b))), b...))
	return err
}

type wire4 struct {
	t   *testing.T
	c   net.Conn
	xid uint32
}

func (w *wire4) call(ops ...xdr4) xdr4 {
	w.t.Helper()
	w.xid++
	b := xdr4{}
	for _, n := range []uint32{w.xid, 0, 2, 100003, 4, 1, 0, 0, 0, 0} {
		b.u(n)
	}
	b.str("evict")
	b.u(0)
	b.u(uint32(len(ops)))
	for _, op := range ops {
		b = append(b, op...)
	}
	w.c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := send4(w.c, b); err != nil {
		w.t.Fatal(err)
	}
	raw, err := record4(w.c)
	if err != nil {
		w.t.Fatal(err)
	}
	r := xdr4(raw)
	if r.word() != w.xid || r.word() != 1 || r.word() != 0 {
		w.t.Fatal("bad RPC reply")
	}
	r.word()
	r.blob()
	if r.word() != 0 {
		w.t.Fatal("RPC rejected")
	}
	if st := r.word(); st != 0 {
		w.t.Fatalf("COMPOUND status %d", st)
	}
	r.blob()
	if n := r.word(); n != uint32(len(ops)) {
		w.t.Fatal("short compound")
	}
	for i, op := range ops {
		if r.word() != binary.BigEndian.Uint32(op[:4]) || r.word() != 0 {
			w.t.Fatal("failed operation")
		}
		if i == len(ops)-1 {
			return r
		}
	}
	return r
}

type countedV4 struct {
	nfs.Backend
	holds atomic.Int64
}

func (f *countedV4) Cacheable(string) bool { return true }
func (f *countedV4) HoldFileID(id uint64) func() {
	release := f.Backend.(interface{ HoldFileID(uint64) func() }).HoldFileID(id)
	f.holds.Add(1)
	return func() { release(); f.holds.Add(-1) }
}

// TestV4EvictingExport runs actual OPEN/LOCK/delegation wire state against the
// evicting backend. The callback, all RPCs and the worker have bounded deadlines.
func TestV4EvictingExport(t *testing.T) {
	files := map[string]string{"d/file": "state content", "d/expire": "expiry content"}
	for i := range 8 {
		files[fmt.Sprintf("pressure/d%d/file", i)] = "pressure"
	}
	e, state := nodestest.New(t, files)
	e.SetNodeCache(0)
	backend := &countedV4{Backend: e.NFS()}
	var offset atomic.Int64
	srv := nfs.EvictionServer(backend, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
	e.OnRecall(srv.Recall)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	go func() { srv.Serve(l); close(serverDone) }()
	t.Cleanup(func() {
		l.Close()
		select {
		case <-serverDone:
		case <-time.After(10 * time.Second):
			t.Error("server failed to stop")
		}
	})
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	w := &wire4{t: t, c: conn}
	cb, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	probe := make(chan struct{}, 1)
	recall := make(chan []byte, 4)
	go func() {
		nc, err := cb.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		for {
			nc.SetDeadline(time.Now().Add(15 * time.Second))
			data, err := record4(nc)
			if err != nil {
				return
			}
			d := xdr4(data)
			xid := d.word()
			d.take(16)
			proc := d.word()
			d.word()
			d.blob()
			d.word()
			d.blob()
			out := xdr4{}
			for _, n := range []uint32{xid, 1, 0, 0, 0, 0} {
				out.u(n)
			}
			if proc == 1 {
				tag := d.blob()
				d.take(16)
				sid := append([]byte(nil), d.take(16)...)
				d.word()
				d.blob()
				recall <- sid
				out.u(0)
				out.opaque(tag)
				out.u(1)
				out.u(4)
				out.u(0)
			} else {
				select {
				case probe <- struct{}{}:
				default:
				}
			}
			if send4(nc, out) != nil {
				return
			}
		}
	}()
	addr := cb.Addr().(*net.TCPAddr)
	uaddr := fmt.Sprintf("127.0.0.1.%d.%d", addr.Port>>8, addr.Port&255)
	registered := w.call(op4(35, func(b *xdr4) {
		*b = append(*b, []byte("bootverf")...)
		b.str("cold-state")
		b.u(0x40000000)
		b.str("tcp")
		b.str(uaddr)
		b.u(7)
	}))
	client := binary.BigEndian.Uint64(registered.take(8))
	confirm := registered.take(8)
	w.call(op4(36, func(b *xdr4) { b.q(client); *b = append(*b, confirm...) }))
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("callback probe timeout")
	}
	// Allow the acknowledged callback probe to publish its result.
	time.Sleep(20 * time.Millisecond)
	parent := backend.FileID("co/d")
	open := func(name, owner string) ([]byte, []byte, uint64) {
		r := w.call(put4(parent), op4(18, func(b *xdr4) {
			b.u(1)
			b.u(1)
			b.u(0)
			b.q(client)
			b.str(owner)
			b.u(0)
			b.u(0)
			b.str(name)
		}))
		sid := append([]byte(nil), r.take(16)...)
		r.take(20)
		r.word()
		words := r.word()
		r.take(int(words) * 4)
		if typ := r.word(); typ != 1 {
			t.Fatalf("no read delegation: %d", typ)
		}
		deleg := append([]byte(nil), r.take(16)...)
		r.word()
		r.take(12)
		r.blob()
		return sid, deleg, backend.FileID("co/d/" + name)
	}
	opened, deleg, id := open("file", "reader")
	locked := w.call(put4(id), op4(12, func(b *xdr4) {
		b.u(1)
		b.u(0)
		b.q(0)
		b.q(100)
		b.u(1)
		b.u(1)
		*b = append(*b, opened...)
		b.u(1)
		b.q(client)
		b.str("locker")
	}))
	lock := append([]byte(nil), locked.take(16)...)
	pressure := func() {
		for i := range 8 {
			backend.FileID(fmt.Sprintf("co/pressure/d%d/file", i))
		}
	}
	pressure()
	if backend.holds.Load() != 3 {
		t.Fatalf("state holds %d", backend.holds.Load())
	}
	e.Invalidate("co", nil, []string{""})
	select {
	case got := <-recall:
		if string(got) != string(deleg) {
			t.Fatalf("wrong recall stateid %x want %x", got, deleg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retained identity missed recall")
	}
	if err := e.Nodes().Rename("co/d/file", "co/d/moved"); err != nil {
		t.Fatal(err)
	}
	read := w.call(put4(id), op4(25, func(b *xdr4) { *b = append(*b, opened...); b.q(0); b.u(100) }))
	read.word()
	if got := string(read.blob()); got != "state content" {
		t.Fatal(got)
	}
	w.call(put4(id), op4(14, func(b *xdr4) { b.u(1); b.u(1); *b = append(*b, lock...); b.q(0); b.q(100) }))
	w.call(put4(id), stateOp4(8, deleg))
	w.call(put4(id), stateOp4(4, opened))
	// An empty lock owner survives UNLOCK until RELEASE_LOCKOWNER.
	w.call(op4(39, func(b *xdr4) { b.q(client); b.str("locker") }))
	if backend.holds.Load() != 0 {
		t.Fatalf("released state holds %d", backend.holds.Load())
	}
	expired, expireDeleg, expireID := open("expire", "expires")
	_ = expired
	_ = expireDeleg
	pressure()
	if backend.holds.Load() != 2 {
		t.Fatal("expiry setup lost state")
	}
	offset.Add(int64(4 * time.Second))
	// A new client registration drives the server's lease sweep.
	w.call(op4(35, func(b *xdr4) {
		*b = append(*b, []byte("newboot!")...)
		b.str("sweeper")
		b.u(0)
		b.str("")
		b.str("")
		b.u(0)
	}))
	if backend.holds.Load() != 0 {
		t.Fatalf("lease expiry retained %d holds", backend.holds.Load())
	}
	pressure()
	nodestest.Cold(t, state, expireID, "expire")
	// Released unlogged state must really leave memory. Corrupt the cold
	// store only now; Path would succeed if the node were still pinned.
	nodestest.BreakCold(t, state)
	if _, ok := e.Nodes().Path(expireID); ok || e.Nodes().IdentityError() == nil {
		t.Fatal("released identity was not evictable")
	}
}
