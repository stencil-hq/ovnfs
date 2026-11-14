package smb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

type writeCacheBackend struct {
	Backend
	allowed bool
}

func (b writeCacheBackend) WriteCacheable(string) bool { return b.allowed }

func enableWriteCache(s *Server) {
	s.WriteCache = true
	s.FS = writeCacheBackend{Backend: s.FS, allowed: true}
}

type quiesceResult struct {
	release func()
	err     error
}

func startQuiesce(s *Server, ctx context.Context) <-chan quiesceResult {
	result := make(chan quiesceResult, 1)
	go func() {
		release, err := s.Quiesce(ctx)
		result <- quiesceResult{release, err}
	}()
	return result
}

func awaitQuiesce(t *testing.T, result <-chan quiesceResult) quiesceResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("Quiesce did not complete")
		return quiesceResult{}
	}
}

func TestSMBWriteCacheGrantPolicyAndConcurrentOpens(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		optIn, hook, allowed bool
		want                 uint32
	}{
		{"disabled", false, true, true, 3},
		{"missing-hook", true, false, false, 3},
		{"backend-denied", true, true, false, 3},
		{"enabled", true, true, true, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLeaseStateFixture(t)
			f.c.srv.WriteCache = tc.optIn
			if tc.hook {
				f.c.srv.FS = writeCacheBackend{Backend: f.c.srv.FS, allowed: tc.allowed}
			}
			o, _, _ := f.open("file", leaseStateArgs(52, 1, 7, 0))
			assertLeaseCache(t, o, tc.want)
		})
	}
	t.Run("unpublished-concurrent-grants", func(t *testing.T) {
		f := newLeaseStateFixture(t)
		enableWriteCache(f.c.srv)
		g := &f.c.srv.st.opens
		var wg sync.WaitGroup
		for i := range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o := &open{path: "same", tree: f.tree}
				g.grantCaching(f.c, o, leaseStateArgs(52, byte(i+1), 7, 0))
			}()
		}
		wg.Wait()
		g.mu.Lock()
		writers := 0
		for _, grant := range g.grants {
			if grant.state&4 != 0 {
				writers++
			}
		}
		g.mu.Unlock()
		if writers != 1 {
			t.Fatalf("concurrent isolated WRITE grants = %d", writers)
		}
	})
}

type changedWritePolicyBackend struct {
	Backend
	server *Server
	once   sync.Once
}

func (b *changedWritePolicyBackend) WriteCacheable(string) bool {
	b.once.Do(func() {
		release, err := b.server.Quiesce(context.Background())
		if err == nil {
			release()
		}
	})
	// Simulate an eligibility result sampled before a protected hardlink.
	return true
}

func TestSMBWriteCacheEligibilityCannotOutliveBarrier(t *testing.T) {
	f := newLeaseStateFixture(t)
	f.c.srv.WriteCache = true
	f.c.srv.FS = &changedWritePolicyBackend{Backend: f.c.srv.FS, server: f.c.srv}
	o, _, _ := f.open("file", leaseStateArgs(52, 1, 7, 0))
	assertLeaseCache(t, o, 3)
	// A fresh policy check, after the mutation, can grant WRITE again.
	next, _, _ := f.open("file", leaseStateArgs(52, 1, 7, 0))
	assertLeaseCache(t, next, 7)
}

func TestSMBWriteCacheQuiesceFlushAndNestedGates(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) { putDiskFile(t, upper, "cached", "before") })
	owner := guestClient(t, fixture)
	key := [16]byte{41}
	id, initial := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	if initial.state != 7 {
		t.Fatalf("initial WRITE lease = %#x", initial.state)
	}
	pending := startQuiesce(fixture.server, context.Background())
	leaseBreak(t, owner, key)
	reopened, breaking := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	if breaking.state != 7 || breaking.flags&2 == 0 {
		t.Fatalf("pending reopen = %+v", breaking)
	}
	owner.expectStatus(owner.write(reopened, 0, []byte("flushed")), wireSuccess)
	select {
	case got := <-pending:
		if got.release != nil {
			got.release()
		}
		t.Fatal("Quiesce passed before ACK")
	default:
	}
	leaseAcknowledge(owner, key)
	first := awaitQuiesce(t, pending)
	if first.err != nil {
		t.Fatal(first.err)
	}
	defer first.release()
	file, err := fixture.fs.Open("cached")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "flushed" {
		t.Fatalf("protected backend read = %q, %v", data, err)
	}
	second, err := fixture.server.Quiesce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	first.release()
	first.release()
	gated, lease := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	if lease.state&4 != 0 {
		t.Fatal("first release opened nested gate")
	}
	second()
	promoted, lease := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	if lease.state != 7 {
		t.Fatalf("released gate did not permit promotion: %+v", lease)
	}
	for _, opened := range []wireID{promoted, gated, reopened, id} {
		owner.expectStatus(owner.close(opened), wireSuccess)
	}
	clean, err := fixture.server.Quiesce(context.Background())
	if err != nil {
		t.Fatalf("successful final CLOSE fenced share: %v", err)
	}
	clean()
}

func TestSMBWriteCacheQuiesceFailureFences(t *testing.T) {
	for _, reason := range []string{"timeout", "disconnect", "transport"} {
		t.Run(reason, func(t *testing.T) {
			f := newLeaseStateFixture(t)
			enableWriteCache(f.c.srv)
			o, _, _ := f.open("file", leaseStateArgs(52, 1, 7, 0))
			result := startQuiesce(f.c.srv, context.Background())
			if reason == "transport" {
				_ = f.wire.conn.Close()
			} else {
				leaseBreak(t, f.wire, [16]byte{1})
				if reason == "disconnect" {
					f.c.shutdown()
				} else {
					g := &f.c.srv.st.opens
					g.mu.Lock()
					g.grants[o].timer.Reset(0)
					g.mu.Unlock()
				}
			}
			got := awaitQuiesce(t, result)
			if !errors.Is(got.err, ErrWriteCacheLost) || got.release != nil {
				t.Fatalf("unsafe quiescence: %+v", got)
			}
			if release, err := f.c.srv.Quiesce(context.Background()); !errors.Is(err, ErrWriteCacheLost) || release != nil {
				t.Fatalf("lost dirty owner fault was cleared: %v", err)
			}
		})
	}
}

func TestSMBWriteCacheQuiesceCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		f := newLeaseStateFixture(t)
		enableWriteCache(f.c.srv)
		f.open("file", leaseStateArgs(52, 1, 7, 0))
		ctx, cancel := context.WithCancel(context.Background())
		want := context.Canceled
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			want = context.DeadlineExceeded
		}
		result := startQuiesce(f.c.srv, ctx)
		leaseBreak(t, f.wire, [16]byte{1})
		if !deadline {
			cancel()
		}
		got := awaitQuiesce(t, result)
		cancel()
		if !errors.Is(got.err, want) || got.release != nil {
			t.Fatalf("quiescence cancellation = %v, want %v", got.err, want)
		}
		if status := f.ack(f.c, f.sess, [16]byte{1}, 0); status != statusSuccess {
			t.Fatalf("late successful drain status %#x", status)
		}
		release, err := f.c.srv.Quiesce(context.Background())
		if err != nil {
			t.Fatalf("caller cancellation poisoned successfully drained cache: %v", err)
		}
		release()
	}
	// Even a backend holding registry bookkeeping cannot defeat a deadline.
	f := newLeaseStateFixture(t)
	enableWriteCache(f.c.srv)
	g := &f.c.srv.st.opens
	g.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	result := startQuiesce(f.c.srv, ctx)
	got := awaitQuiesce(t, result)
	g.mu.Unlock()
	cancel()
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("locked registry deadline = %v", got.err)
	}
}

type cacheTestFile struct {
	billy.File
	entered, resume             chan struct{}
	writeErr, syncErr, closeErr error
}

func (f *cacheTestFile) Write(b []byte) (int, error) {
	if f.entered != nil {
		close(f.entered)
		<-f.resume
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(b)
}
func (f *cacheTestFile) Sync() error { return f.syncErr }
func (f *cacheTestFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func cacheWriteRequest(f *leaseStateFixture, o *open, off uint64, data []byte) *request {
	raw := make([]byte, 112+len(data))
	b := raw[64:]
	binary.LittleEndian.PutUint16(b, 49)
	binary.LittleEndian.PutUint16(b[2:], 112)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	binary.LittleEndian.PutUint64(b[8:], off)
	putOpenID(b[16:], o.id)
	copy(b[48:], data)
	return &request{c: f.c, sess: f.sess, tree: f.tree, raw: raw, body: b}
}

func cacheFixtureFile(t *testing.T, f *leaseStateFixture) (*open, *cacheTestFile) {
	t.Helper()
	o, _, _ := f.open("file", leaseStateArgs(52, 1, 7, 0))
	file, err := osfs.New(t.TempDir()).OpenFile("file", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &cacheTestFile{File: file}
	o.file, o.access = wrapped, 3
	f.c.maxWrite = 1 << 20
	return o, wrapped
}

func TestSMBWriteCacheACKWaitsForBackendIO(t *testing.T) {
	f := newLeaseStateFixture(t)
	enableWriteCache(f.c.srv)
	o, file := cacheFixtureFile(t, f)
	result := startQuiesce(f.c.srv, context.Background())
	leaseBreak(t, f.wire, [16]byte{1})
	file.entered, file.resume = make(chan struct{}), make(chan struct{})
	written := make(chan uint32, 1)
	go func() { _, st := handleWrite(cacheWriteRequest(f, o, 0, []byte("dirty"))); written <- st }()
	<-file.entered
	// ACK does not need the backend's open lock. It defers revocation until
	// this WRITE completes instead of letting the barrier see stale bytes.
	if st := f.ack(f.c, f.sess, [16]byte{1}, 0); st != statusSuccess {
		t.Fatalf("ACK = %#x", st)
	}
	select {
	case <-result:
		t.Fatal("ACK let Quiesce overtake backend WRITE")
	default:
	}
	close(file.resume)
	if st := <-written; st != statusSuccess {
		t.Fatalf("WRITE = %#x", st)
	}
	got := awaitQuiesce(t, result)
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.release()
	data := make([]byte, 5)
	if _, err := file.ReadAt(data, 0); err != nil || string(data) != "dirty" {
		t.Fatalf("drained backend data = %q, %v", data, err)
	}
}

func TestSMBWriteCacheBackendFailuresStayFenced(t *testing.T) {
	for _, operation := range []string{"WRITE", "FLUSH", "CLOSE"} {
		t.Run(operation, func(t *testing.T) {
			f := newLeaseStateFixture(t)
			enableWriteCache(f.c.srv)
			o, file := cacheFixtureFile(t, f)
			failure := errors.New("injected backend failure")
			result := startQuiesce(f.c.srv, context.Background())
			leaseBreak(t, f.wire, [16]byte{1})
			var status uint32
			switch operation {
			case "WRITE":
				file.writeErr = failure
				_, status = handleWrite(cacheWriteRequest(f, o, 0, []byte("dirty")))
			case "FLUSH":
				file.syncErr = failure
				body := make([]byte, 24)
				binary.LittleEndian.PutUint16(body, 24)
				putOpenID(body[8:], o.id)
				_, status = handleFlush(&request{c: f.c, tree: f.tree, body: body})
			case "CLOSE":
				file.closeErr = failure
				status = closeOpen(f.c, o)
			}
			if status == statusSuccess {
				t.Fatal("backend failure was hidden")
			}
			got := awaitQuiesce(t, result)
			if !errors.Is(got.err, ErrWriteCacheLost) {
				t.Fatalf("failed %s admitted protected operation: %v", operation, got.err)
			}
			if release, err := f.c.srv.Quiesce(context.Background()); !errors.Is(err, ErrWriteCacheLost) || release != nil {
				t.Fatalf("failed %s fault not sticky: %v", operation, err)
			}
		})
	}
}

func TestSMBWriteCacheSharedLeaseConcurrentSeekWrites(t *testing.T) {
	f := newLeaseStateFixture(t)
	enableWriteCache(f.c.srv)
	fs := osfs.New(t.TempDir())
	var opens []*open
	for range 2 {
		o, _, _ := f.open("file", leaseStateArgs(52, 1, 7, 0))
		file, err := fs.OpenFile("file", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately hide WriterAt to exercise the Seek+Write fallback.
		o.file = &cacheTestFile{File: file}
		o.access = 3
		opens = append(opens, o)
	}
	f.c.maxWrite = 1 << 20
	var wg sync.WaitGroup
	for index, o := range opens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 128 {
				data := bytes.Repeat([]byte{byte(index + 1)}, 64)
				if _, st := handleWrite(cacheWriteRequest(f, o, uint64((i*2+index)*64), data)); st != statusSuccess {
					t.Errorf("WRITE = %#x", st)
					return
				}
			}
		}()
	}
	wg.Wait()
	data := make([]byte, 128*128)
	if _, err := opens[0].file.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	for i := range 256 {
		if !bytes.Equal(data[i*64:(i+1)*64], bytes.Repeat([]byte{byte(i%2 + 1)}, 64)) {
			t.Fatalf("concurrent shared-lease cursor corruption at extent %d", i)
		}
	}
}

func TestSMBWriteCacheOwnerTruncateAndRenamePreserveLease(t *testing.T) {
	for _, rename := range []bool{false, true} {
		fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) { putDiskFile(t, upper, "cached", "before") })
		owner := guestClient(t, fixture)
		key := [16]byte{61}
		id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", accessAll, 7, 7, key, [16]byte{}, 0)))
		g := &fixture.server.st.opens
		g.mu.Lock()
		var opened *open
		for o := range g.entries {
			opened = o
		}
		g.mu.Unlock()
		class := byte(20)
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, 3)
		if rename {
			class = 10
			name := wireUTF16("renamed")
			data = make([]byte, 20)
			binary.LittleEndian.PutUint32(data[16:], uint32(len(name)))
			data = append(data, name...)
		}
		raw := make([]byte, 96+len(data))
		b := raw[64:]
		binary.LittleEndian.PutUint16(b, 33)
		b[2], b[3] = 1, class
		binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
		binary.LittleEndian.PutUint16(b[8:], 96)
		copy(b[16:], id[:])
		copy(b[32:], data)
		owner.expectStatus(owner.write(id, 0, []byte("flushed")), wireSuccess)
		done := make(chan uint32, 1)
		go func() {
			_, st := handleSetInfo(&request{c: opened.tree.sess.conn, tree: opened.tree, raw: raw, body: b, compound: &compoundState{}})
			done <- st
		}()
		select {
		case st := <-done:
			if st != statusSuccess {
				t.Fatalf("mutation status %#x", st)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("mutation blocked its own cache flush")
		}
		assertLeaseCache(t, opened, 7)
		path, want := "cached", "flu"
		if rename {
			path, want = "renamed", "flushed"
		}
		file, err := fixture.fs.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || string(got) != want {
			t.Fatalf("mutation after flush = %q, %v; want %q", got, err, want)
		}
		readEquals(t, owner, id, want)
		if rename {
			owner.expectStatus(owner.write(id, uint64(len(want)), []byte(" later")), wireSuccess)
			readEquals(t, owner, id, want+" later")
			reopened, state := leaseResponse(t, owner.exchange(5, leaseCreateBody("renamed", accessAll, 7, 7, key, [16]byte{}, 0)))
			if state.state != 7 {
				t.Fatalf("renamed lease key lost ownership: %+v", state)
			}
			readEquals(t, owner, reopened, want+" later")
			owner.expectStatus(owner.close(reopened), wireSuccess)
		}
		owner.expectStatus(owner.close(id), wireSuccess)
	}
}

func TestSMBWriteCacheOwnerRenameKeepsExternalBreakPending(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
		putDiskFile(t, upper, "source", "before")
	})
	owner := guestClient(t, fixture)
	key := [16]byte{81}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("source", accessAll, 7, 7, key, [16]byte{}, 0)))
	pending := startQuiesce(fixture.server, context.Background())
	leaseBreak(t, owner, key)
	// The owner must finish its rename while an external break is pending,
	// otherwise the vnode lock can prevent the flush that Quiesce needs.
	owner.expectStatus(owner.rename(id, "renamed", false), wireSuccess)
	reopened, state := leaseResponse(t, owner.exchange(5, leaseCreateBody("renamed", accessAll, 7, 7, key, [16]byte{}, 0)))
	if state.state != 7 || state.flags&2 == 0 {
		t.Fatalf("rename bypassed outstanding break: %+v", state)
	}
	owner.expectStatus(owner.write(id, 0, []byte("dirty after rename")), wireSuccess)
	readEquals(t, owner, reopened, "dirty after rename")
	select {
	case <-pending:
		t.Fatal("rename released external barrier without a flush ACK")
	default:
	}
	leaseAcknowledge(owner, key)
	got := awaitQuiesce(t, pending)
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.release()
	file, err := fixture.fs.Open("renamed")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || string(data) != "dirty after rename" {
		t.Fatalf("protected renamed data = %q, %v", data, err)
	}
	if _, err := fixture.fs.Stat("source"); !os.IsNotExist(err) {
		t.Fatalf("old rename path survived: %v", err)
	}
	owner.expectStatus(owner.close(id), wireSuccess)
	owner.expectStatus(owner.close(reopened), wireSuccess)
}

func TestSMBWriteCacheOwnerUnlinkRetainsHandlesUntilLastClose(t *testing.T) {
	for _, style := range []string{"disposition", "disposition-ex", "delete-on-close-create"} {
		t.Run(style, func(t *testing.T) {
			fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
				putDiskFile(t, upper, "cached", "before")
			})
			owner := guestClient(t, fixture)
			key := [16]byte{82}
			id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", accessAll, 7, 7, key, [16]byte{}, 0)))
			body := leaseCreateBody("cached", accessAll, 7, 7, key, [16]byte{}, 0)
			if style == "delete-on-close-create" {
				binary.LittleEndian.PutUint32(body[40:], createNonDirectory|createDeleteOnClose)
			}
			other, lease := leaseResponse(t, owner.exchange(5, body))
			if lease.state != 7 {
				t.Fatalf("delete reopen lost own lease: %+v", lease)
			}
			switch style {
			case "disposition":
				owner.expectStatus(owner.disposition(id, true), wireSuccess)
			case "disposition-ex":
				owner.expectStatus(owner.setInfo(id, 64, []byte{1, 0, 0, 0}), wireSuccess)
			}
			owner.expectStatus(owner.write(id, 0, []byte("after unlink")), wireSuccess)
			readEquals(t, owner, other, "after unlink")
			_, denied := owner.create("cached", 1, createNonDirectory)
			owner.expectStatus(denied, statusDeletePending)
			owner.expectStatus(owner.close(id), wireSuccess)
			pending := startQuiesce(fixture.server, context.Background())
			leaseBreak(t, owner, key)
			owner.expectStatus(owner.write(other, 0, []byte("final drain!")), wireSuccess)
			readEquals(t, owner, other, "final drain!")
			// Explicit last CLOSE proves the client has drained the deleted
			// inode; neither disconnect nor disposition alone can substitute.
			owner.expectStatus(owner.close(other), wireSuccess)
			got := awaitQuiesce(t, pending)
			if got.err != nil {
				t.Fatal(got.err)
			}
			got.release()
			if _, err := fixture.fs.Stat("cached"); !os.IsNotExist(err) {
				t.Fatalf("last CLOSE did not remove pending file: %v", err)
			}
		})
	}
}

func TestSMBWriteCacheOwnerOverwriteCreateKeepsLease(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
		putDiskFile(t, upper, "cached", "before")
	})
	owner := guestClient(t, fixture)
	key := [16]byte{83}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", accessAll, 7, 7, key, [16]byte{}, 0)))
	body := leaseCreateBody("cached", accessAll, 7, 7, key, [16]byte{}, 0)
	binary.LittleEndian.PutUint32(body[36:], 4) // FILE_OVERWRITE
	other, lease := leaseResponse(t, owner.exchange(5, body))
	if lease.state != 7 {
		t.Fatalf("same-owner overwrite revoked its lease: %+v", lease)
	}
	owner.expectStatus(owner.write(id, 0, []byte("new")), wireSuccess)
	readEquals(t, owner, other, "new")
	owner.expectStatus(owner.close(id), wireSuccess)
	owner.expectStatus(owner.close(other), wireSuccess)
}

func TestSMBWriteCacheDirectoryRenamePreservesOwnAndDrainsForeign(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
		putDiskFile(t, upper, "parent/own", "own")
		putDiskFile(t, upper, "parent/foreign", "old")
	})
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key, foreignKey := [16]byte{84}, [16]byte{85}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("parent/own", accessAll, 7, 7, key, [16]byte{}, 0)))
	foreign, _ := leaseResponse(t, other.exchange(5, leaseCreateBody("parent/foreign", accessAll, 7, 7, foreignKey, [16]byte{}, 0)))
	directory := mustOpen(t, owner, "parent", 1, createDirectory)
	owner.expectStatus(owner.rename(directory, "renamed", false), statusPending)
	leaseBreak(t, other, foreignKey)
	other.expectStatus(other.write(foreign, 0, []byte("foreign flush")), wireSuccess)
	leaseAcknowledge(other, foreignKey)
	owner.expectStatus(leaseReceive(t, owner), wireSuccess)
	owner.expectStatus(owner.write(id, 0, []byte("own after rename")), wireSuccess)
	readEquals(t, owner, id, "own after rename")
	readEquals(t, other, foreign, "foreign flush")
	reopened, lease := leaseResponse(t, owner.exchange(5, leaseCreateBody("renamed/own", accessAll, 7, 7, key, [16]byte{}, 0)))
	if lease.state != 7 {
		t.Fatalf("directory rename revoked own descendant: %+v", lease)
	}
	readEquals(t, owner, reopened, "own after rename")
	owner.expectStatus(owner.close(reopened), wireSuccess)
	owner.expectStatus(owner.close(id), wireSuccess)
	other.expectStatus(other.close(foreign), wireSuccess)
	owner.expectStatus(owner.close(directory), wireSuccess)
}

func TestSMBWriteCacheRenameStillRecallsReplacementTarget(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
		putDiskFile(t, upper, "source", "source data")
		putDiskFile(t, upper, "target", "target data")
	})
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key, targetKey := [16]byte{86}, [16]byte{87}
	source, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("source", accessAll, 7, 7, key, [16]byte{}, 0)))
	target, _ := leaseResponse(t, other.exchange(5, leaseCreateBody("target", accessAll, 7, 7, targetKey, [16]byte{}, 0)))
	owner.expectStatus(owner.rename(source, "target", true), statusPending)
	leaseBreak(t, other, targetKey)
	other.expectStatus(other.write(target, 0, []byte("target flush")), wireSuccess)
	other.expectStatus(other.close(target), wireSuccess)
	owner.expectStatus(leaseReceive(t, owner), wireSuccess)
	readEquals(t, owner, source, "source data")
	owner.expectStatus(owner.write(source, 0, []byte("source kept")), wireSuccess)
	reopened, lease := leaseResponse(t, owner.exchange(5, leaseCreateBody("target", accessAll, 7, 7, key, [16]byte{}, 0)))
	if lease.state != 7 {
		t.Fatalf("target replacement lost source's lease: %+v", lease)
	}
	readEquals(t, owner, reopened, "source kept")
	owner.expectStatus(owner.close(source), wireSuccess)
	owner.expectStatus(owner.close(reopened), wireSuccess)
}

// macOS sends these setup/flush operations after CREATE. None is an external
// reader of the owner's dirty data, nor may it evict an unrelated file's lease.
func TestSMBWriteCacheOwnerSetupAndProbesKeepCaching(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
		putDiskFile(t, upper, "cached", "before")
		putDiskFile(t, upper, "unrelated", "other")
	})
	owner := guestClient(t, fixture)
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", accessAll, 7, 7, [16]byte{71}, [16]byte{}, 0)))
	other, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("unrelated", accessAll, 7, 7, [16]byte{72}, [16]byte{}, 0)))
	g := &fixture.server.st.opens
	epoch := g.writeEpoch.Load()
	size := binary.LittleEndian.AppendUint64(nil, 4096)
	for _, class := range []byte{19, 20, 39} {
		owner.expectStatus(owner.setInfo(id, class, size), wireSuccess)
	}
	owner.expectStatus(owner.setInfo(id, 4, make([]byte, 40)), wireSuccess)
	for _, code := range []uint32{ioctlResumeKey, ioctlCreateObjectID, ioctlGetObjectID} {
		_, reply := owner.ioctl(id, code, nil, 64)
		owner.expectStatus(reply, wireSuccess)
	}
	for _, code := range []uint32{ioctlSetSparse, ioctlGetIntegrity} {
		_, reply := owner.ioctl(id, code, nil, 64)
		owner.expectStatus(reply, statusInvalidDeviceRequest)
	}
	_, reply := owner.ioctl(id, 0xffffffff, nil, 64)
	owner.expectStatus(reply, statusNotSupported)
	owner.expectStatus(owner.setInfo(id, 11, nil), statusNotSupported)
	zero := make([]byte, 16)
	binary.LittleEndian.PutUint64(zero[8:], 16)
	_, reply = owner.ioctl(id, ioctlZeroData, zero, 64)
	owner.expectStatus(reply, wireSuccess)
	_, reply = owner.ioctl(id, ioctlAllocatedRanges, zero, 64)
	owner.expectStatus(reply, wireSuccess)
	if got := g.writeEpoch.Load(); got != epoch {
		t.Fatalf("owner-local metadata/probes unnecessarily gated unrelated CREATEs: epoch %d -> %d", epoch, got)
	}
	g.mu.Lock()
	for _, grant := range g.grants {
		if grant.state != 7 {
			t.Errorf("owner setup revoked lease %x to %#x", grant.key, grant.state)
		}
	}
	g.mu.Unlock()
	owner.expectStatus(owner.close(id), wireSuccess)
	owner.expectStatus(owner.close(other), wireSuccess)
}

func TestSMBWriteCacheDirectoryPreservesOwnerButRecallsOtherClient(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) {
		putDiskFile(t, upper, "cached", "old")
	})
	owner, other := guestClient(t, fixture), guestClient(t, fixture)
	key := [16]byte{73}
	id, _ := leaseResponse(t, owner.exchange(5, leaseCreateBody("cached", accessAll, 7, 7, key, [16]byte{}, 0)))
	ownDir := mustOpen(t, owner, "", 1, createDirectory)
	otherDir := mustOpen(t, other, "", 1, createDirectory)
	query := func(id wireID) []byte {
		body := make([]byte, 32)
		binary.LittleEndian.PutUint16(body, 33)
		body[2], body[3] = 1, 1 // FILE_DIRECTORY_INFORMATION, restart
		copy(body[8:], id[:])
		binary.LittleEndian.PutUint32(body[28:], 1<<16)
		return body
	}
	owner.expectStatus(owner.exchange(cmdQueryDirectory, query(ownDir)), wireSuccess)
	g := &fixture.server.st.opens
	g.mu.Lock()
	for _, grant := range g.grants {
		if grant.key == key && grant.state != 7 {
			t.Errorf("owner directory query revoked its file lease to %#x", grant.state)
		}
	}
	g.mu.Unlock()
	other.expectStatus(other.exchange(cmdQueryDirectory, query(otherDir)), statusPending)
	leaseBreak(t, owner, key)
	owner.expectStatus(owner.write(id, 0, []byte("new contents")), wireSuccess)
	leaseAcknowledge(owner, key)
	reply := leaseReceive(t, other)
	other.expectStatus(reply, wireSuccess)
	data := reply.body[8:]
	found := false
	for len(data) >= 64 {
		nameLen := int(binary.LittleEndian.Uint32(data[60:]))
		if nameLen > len(data)-64 {
			t.Fatal("invalid directory record")
		}
		if utf16Decode(data[64:64+nameLen]) == "cached" {
			found = true
			if size := binary.LittleEndian.Uint64(data[40:]); size != uint64(len("new contents")) {
				t.Fatalf("external directory read stale EOF %d", size)
			}
		}
		next := int(binary.LittleEndian.Uint32(data))
		if next == 0 {
			break
		}
		if next > len(data) {
			t.Fatal("invalid directory offset")
		}
		data = data[next:]
	}
	if !found {
		t.Fatal("directory response omitted cached file")
	}
	owner.expectStatus(owner.close(id), wireSuccess)
	owner.expectStatus(owner.close(ownDir), wireSuccess)
	other.expectStatus(other.close(otherDir), wireSuccess)
}

type cacheEOFBackend struct {
	Backend
	files           billy.Filesystem
	entered, resume chan struct{}
	failure         error
}

func (b *cacheEOFBackend) WriteCacheable(string) bool          { return true }
func (b *cacheEOFBackend) Lstat(p string) (os.FileInfo, error) { return b.files.Lstat(p) }
func (b *cacheEOFBackend) OpenFile(p string, flags int, mode os.FileMode) (billy.File, error) {
	file, err := b.files.OpenFile(p, flags, mode)
	if err != nil {
		return nil, err
	}
	return &cacheEOFFile{File: file, backend: b}, nil
}

type cacheEOFFile struct {
	billy.File
	backend *cacheEOFBackend
}

func (f *cacheEOFFile) Truncate(size int64) error {
	close(f.backend.entered)
	<-f.backend.resume
	if f.backend.failure != nil {
		return f.backend.failure
	}
	return f.File.Truncate(size)
}

func TestSMBWriteCacheOwnerEOFDuringQuiesceDrainsBeforeACK(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := newLeaseStateFixture(t)
		enableWriteCache(f.c.srv)
		o, _ := cacheFixtureFile(t, f)
		backend := &cacheEOFBackend{
			Backend: f.c.srv.FS,
			files:   osfs.New(t.TempDir()),
			entered: make(chan struct{}),
			resume:  make(chan struct{}),
		}
		if fail {
			backend.failure = errors.New("injected backend EOF failure")
		}
		file, err := backend.files.Create("file")
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		f.c.srv.FS = backend
		result := startQuiesce(f.c.srv, context.Background())
		leaseBreak(t, f.wire, [16]byte{1})
		raw := make([]byte, 104)
		body := raw[64:]
		binary.LittleEndian.PutUint16(body, 33)
		body[2], body[3] = 1, 20
		binary.LittleEndian.PutUint32(body[4:], 8)
		binary.LittleEndian.PutUint16(body[8:], 96)
		putOpenID(body[16:], o.id)
		binary.LittleEndian.PutUint64(body[32:], 4096)
		finished := make(chan uint32, 1)
		go func() {
			_, status := handleSetInfo(&request{c: f.c, tree: f.tree, raw: raw, body: body, compound: &compoundState{}})
			finished <- status
		}()
		select {
		case <-backend.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("owner EOF blocked behind the break it must flush")
		}
		if status := f.ack(f.c, f.sess, [16]byte{1}, 0); status != statusSuccess {
			t.Fatalf("ACK status %#x", status)
		}
		select {
		case <-result:
			t.Fatal("Quiesce overtook owner EOF after ACK")
		default:
		}
		close(backend.resume)
		status := <-finished
		got := awaitQuiesce(t, result)
		if fail {
			if status == statusSuccess || !errors.Is(got.err, ErrWriteCacheLost) {
				t.Fatalf("failed EOF admitted barrier: status %#x, %v", status, got.err)
			}
		} else {
			if status != statusSuccess || got.err != nil {
				t.Fatalf("owner EOF failed: status %#x, %v", status, got.err)
			}
			info, err := backend.files.Stat("file")
			got.release()
			if err != nil || info.Size() != 4096 {
				t.Fatalf("protected EOF = %v, %v", info, err)
			}
		}
	}
}

func TestSMBWriteCacheSharedLeaseOffsets(t *testing.T) {
	fixture := startSMBWith(t, enableWriteCache, func(lower, upper string) { putDiskFile(t, upper, "cached", "") })
	first, second := guestClient(t, fixture), guestClient(t, fixture)
	// These are handles on one client/lease, but independent backend cursors.
	key := [16]byte{51}
	one, _ := leaseResponse(t, first.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	two, _ := leaseResponse(t, first.exchange(5, leaseCreateBody("cached", 3, 7, 7, key, [16]byte{}, 0)))
	for i := range 32 {
		first.expectStatus(first.write(one, uint64(i*128), bytes.Repeat([]byte{byte(i)}, 64)), wireSuccess)
		first.expectStatus(first.write(two, uint64(i*128+64), bytes.Repeat([]byte{byte(i + 64)}, 64)), wireSuccess)
	}
	_, pending := second.create("cached", 1, 0x40)
	second.expectStatus(pending, statusPending)
	leaseBreak(t, first, key)
	leaseAcknowledge(first, key)
	reader := leaseOpenID(t, second, leaseReceive(t, second))
	got, reply := second.read(reader, 0, 4096)
	second.expectStatus(reply, wireSuccess)
	want := make([]byte, 0, 4096)
	for i := range 32 {
		want = append(want, bytes.Repeat([]byte{byte(i)}, 64)...)
		want = append(want, bytes.Repeat([]byte{byte(i + 64)}, 64)...)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("shared lease handles corrupted explicit write offsets")
	}
	first.expectStatus(first.close(one), wireSuccess)
	first.expectStatus(first.close(two), wireSuccess)
	second.expectStatus(second.close(reader), wireSuccess)
}
