package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/peer"
	"github.com/stencil-hq/ovnfs/store"
)

// dirtyClient models a counted export grant gate and a client whose pending
// writes arrive through Enter only after the export requests a flush.
type dirtyClient struct {
	drain  func() error
	once   sync.Once
	done   chan struct{}
	err    error
	active atomic.Int32
	peak   atomic.Int32
}

func pendingClient(r *Registry, drain func() error) *dirtyClient {
	c := &dirtyClient{drain: drain, done: make(chan struct{})}
	r.config.Quiesce = c.quiesce
	return c
}

func (c *dirtyClient) quiesce(ctx context.Context) (func(), error) {
	n := c.active.Add(1)
	for old := c.peak.Load(); old < n && !c.peak.CompareAndSwap(old, n); old = c.peak.Load() {
	}
	release := sync.OnceFunc(func() { c.active.Add(-1) })
	c.once.Do(func() {
		go func() {
			c.err = c.drain()
			close(c.done)
		}()
	})
	select {
	case <-ctx.Done():
		return release, ctx.Err()
	case <-c.done:
		return release, c.err
	}
}

func flushClientWrite(r *Registry, name, p, data string) error {
	var s Session
	if err := r.Enter(name, &s); err != nil {
		return err
	}
	defer s.Done()
	if err := s.FS.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := s.FS.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte(data))
	err = errors.Join(err, f.Close())
	if err == nil {
		s.Touch()
		err = s.FS.Flush()
	}
	return err
}

func TestQuiesceSnapshotIncludesPendingSourceAndPrivateGit(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "snap", true: "fork"}[fork], func(t *testing.T) {
			f := newFixture(t)
			f.newCheckout(revisionRepo(t), "app")
			gitdir := f.gitdir("app")
			client := pendingClient(f.reg, func() error {
				if err := flushClientWrite(f.reg, "app", "tracked.txt", "pending source\n"); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(gitdir, "pending-marker"), []byte("pending git\n"), 0o644)
			})
			f.reg.config.Emit = func(e Event) {
				if (e.Name == "snapshotted" || e.Name == "created") && client.active.Load() == 0 {
					t.Error("published snapshot/checkout after releasing the write-cache gate")
				}
			}
			if fork {
				if _, err := f.reg.Fork(t.Context(), api.ForkArgs{Checkout: "app", Name: "child"}); err != nil {
					t.Fatal(err)
				}
			} else {
				snap, err := f.reg.Snap(t.Context(), api.SnapArgs{Checkout: "app", Wait: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.reg.New(t.Context(), api.NewArgs{Snapshot: snap.Snapshot.ID, Name: "child"}); err != nil {
					t.Fatal(err)
				}
			}
			if got := f.read("child", "tracked.txt"); got != "pending source\n" {
				t.Fatalf("snapshot source = %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(f.gitdir("child"), "pending-marker")); err != nil || string(got) != "pending git\n" {
				t.Fatalf("snapshot private git = %q, %v", got, err)
			}
			if client.active.Load() != 0 {
				t.Fatal("snapshot leaked grant gate")
			}
		})
	}
}

func TestQuiesceObserversSeePendingWrites(t *testing.T) {
	checks := map[string]func(*testing.T, *fixture){
		"diff": func(t *testing.T, f *fixture) {
			r, err := f.reg.Diff(t.Context(), api.DiffArgs{Checkout: "app", Patch: true})
			if err != nil || len(r.Changes) != 1 || !strings.Contains(r.Patch, "pending needle") {
				t.Fatalf("diff = %+v, %v", r, err)
			}
		},
		"grep": func(t *testing.T, f *fixture) {
			r, err := f.reg.Grep(t.Context(), api.GrepArgs{Checkout: "app", Pattern: "pending needle"})
			if err != nil || len(r.Matches) != 1 || r.Matches[0].Path != "pending.txt" {
				t.Fatalf("grep = %+v, %v", r, err)
			}
		},
		"glob": func(t *testing.T, f *fixture) {
			r, err := f.reg.Glob(t.Context(), api.GlobArgs{Checkout: "app", Pattern: "pending.txt"})
			if err != nil || len(r.Matches) != 1 {
				t.Fatalf("glob = %+v, %v", r, err)
			}
		},
		"explain": func(t *testing.T, f *fixture) {
			r, err := f.reg.Explain(t.Context(), api.ExplainArgs{Checkout: "app", Path: "pending.txt"})
			if err != nil || !r.Exists || r.Class != "source" {
				t.Fatalf("explain = %+v, %v", r, err)
			}
		},
		"ls": func(t *testing.T, f *fixture) {
			r, err := f.reg.Ls(t.Context(), api.LsArgs{})
			if err != nil || len(r.Checkouts) != 1 || !r.Checkouts[0].Dirty {
				t.Fatalf("ls = %+v, %v", r, err)
			}
		},
		"prune": func(t *testing.T, f *fixture) {
			r, err := f.reg.Prune(t.Context(), api.PruneArgs{Select: api.Select{NoDiff: true}})
			if err != nil || len(r.Removed) != 0 {
				t.Fatalf("prune discarded dirty pending data: %+v, %v", r, err)
			}
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.newCheckout(folder(t, map[string]string{"base.txt": "base\n"}), "app")
			client := pendingClient(f.reg, func() error {
				return flushClientWrite(f.reg, "app", "pending.txt", "pending needle\n")
			})
			check(t, f)
			if client.active.Load() != 0 || client.peak.Load() == 0 {
				t.Fatal("observer did not acquire and release the gate")
			}
		})
	}
}

func TestQuiesceApplyKeepsNestedGate(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "before\n"})
	f.newCheckout(dir, "app")
	client := pendingClient(f.reg, func() error {
		return flushClientWrite(f.reg, "app", "file", "pending\n")
	})
	if _, err := f.reg.Apply(t.Context(), api.ApplyArgs{Checkout: "app"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(got) != "pending\n" {
		t.Fatalf("apply = %q, %v", got, err)
	}
	if client.peak.Load() < 2 || client.active.Load() != 0 {
		t.Fatalf("nested grant gate: peak %d, active %d", client.peak.Load(), client.active.Load())
	}
}

func TestQuiesceRemoteMutationsPreservePendingWrites(t *testing.T) {
	isolateGit(t)
	src, dst := newFixture(t), newFixture(t)
	src.newCheckout(folder(t, map[string]string{"file": "base\n"}), "app")
	pendingClient(src.reg, func() error {
		return flushClientWrite(src.reg, "app", "offered", "pending offer\n")
	})
	offer, remote := offerFixture(t, src, dst, "app")
	if got := dst.read(remote.Name, "offered"); got != "pending offer\n" {
		t.Fatalf("offer missed pending write: %q", got)
	}
	pendingClient(dst.reg, func() error {
		return flushClientWrite(dst.reg, remote.Name, "file", "remote\n")
	})
	pulled, err := dst.reg.RemoteChanges(t.Context(), remote.Name, offer.Tree)
	if err != nil {
		t.Fatal(err)
	}
	pendingClient(src.reg, func() error {
		return flushClientWrite(src.reg, "app", "file", "local\n")
	})
	merged, err := src.reg.MergePull(t.Context(), api.PullArgs{Checkout: "app"}, offer.Tree, pulled)
	if err != nil || len(merged.Conflicts) != 1 {
		t.Fatalf("pull overwrote pending local write: %+v, %v", merged, err)
	}

	// A pending source edit must prevent reattach from replacing a clean lane.
	src.put("app", "file", "next offer\n")
	offer, _, _, err = src.reg.Offer(t.Context(), "app", "app")
	if err != nil {
		t.Fatal(err)
	}
	clean, err := dst.reg.AttachRemote(t.Context(), peer.Offer{
		Node: offer.Node, Source: offer.Source, Name: "clean", Tree: offer.Tree,
		Lock: offer.Lock, Files: offer.Files, Roots: offer.Roots,
	})
	if err != nil {
		t.Fatal(err)
	}
	src.put("app", "file", "newer offer\n")
	offer, _, _, err = src.reg.Offer(t.Context(), "app", "clean")
	if err != nil {
		t.Fatal(err)
	}
	pendingClient(dst.reg, func() error {
		return flushClientWrite(dst.reg, clean.Name, "pending", "keep\n")
	})
	next, err := dst.reg.AttachRemote(t.Context(), offer)
	if err != nil || next.Name == clean.Name || dst.read(clean.Name, "pending") != "keep\n" {
		t.Fatalf("reattach replaced dirty lane: %+v, %v", next, err)
	}
}

func TestQuiesceFailurePrecedesEveryOperation(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "base\n"})
	f.newCheckout(dir, "app")
	snap, err := f.reg.Snap(t.Context(), api.SnapArgs{Checkout: "app", Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	operations := map[string]func(context.Context) error{
		"new": func(ctx context.Context) error {
			_, e := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "child"})
			return e
		},
		"snap": func(ctx context.Context) error { _, e := f.reg.Snap(ctx, api.SnapArgs{Checkout: "app"}); return e },
		"fork": func(ctx context.Context) error {
			_, e := f.reg.Fork(ctx, api.ForkArgs{Checkout: "app", Name: "child"})
			return e
		},
		"diff":  func(ctx context.Context) error { _, e := f.reg.Diff(ctx, api.DiffArgs{Checkout: "app"}); return e },
		"apply": func(ctx context.Context) error { _, e := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "app"}); return e },
		"grep": func(ctx context.Context) error {
			_, e := f.reg.Grep(ctx, api.GrepArgs{Checkout: "app", Pattern: "base"})
			return e
		},
		"glob": func(ctx context.Context) error {
			_, e := f.reg.Glob(ctx, api.GlobArgs{Checkout: "app", Pattern: "*"})
			return e
		},
		"explain": func(ctx context.Context) error {
			_, e := f.reg.Explain(ctx, api.ExplainArgs{Checkout: "app", Path: "file"})
			return e
		},
		"plan": func(ctx context.Context) error { _, e := f.reg.Plan(ctx, api.PlanArgs{Checkout: "app"}); return e },
		"ls":   func(ctx context.Context) error { _, e := f.reg.Ls(ctx, api.LsArgs{}); return e },
		"prune": func(ctx context.Context) error {
			_, e := f.reg.Prune(ctx, api.PruneArgs{Select: api.Select{NoDiff: true}})
			return e
		},
		"rm":     func(ctx context.Context) error { return f.reg.Rm(ctx, api.RmArgs{Checkout: "app"}) },
		"drop":   func(ctx context.Context) error { return f.reg.Drop(ctx, api.DropArgs{Snapshot: snap.Snapshot.ID}) },
		"offer":  func(ctx context.Context) error { _, _, _, e := f.reg.Offer(ctx, "app", "app"); return e },
		"attach": func(ctx context.Context) error { _, e := f.reg.AttachRemote(ctx, peer.Offer{}); return e },
		"remote_changes": func(ctx context.Context) error {
			_, e := f.reg.RemoteChanges(ctx, "app", store.OID(snap.Snapshot.Tree))
			return e
		},
		"merge_pull": func(ctx context.Context) error {
			_, e := f.reg.MergePull(ctx, api.PullArgs{Checkout: "app"}, "", peer.Pulled{})
			return e
		},
	}
	failure := errors.New("client failed to flush")
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			called, released := 0, 0
			f.reg.config.Quiesce = func(context.Context) (func(), error) {
				called++
				return func() { released++ }, failure
			}
			if err := operation(t.Context()); err == nil || err.Error() != failure.Error() {
				t.Fatalf("failed flush accepted: %v", err)
			}
			if called != 1 || released != 1 {
				t.Fatalf("failed acquisition calls/releases = %d/%d", called, released)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := operation(ctx); err == nil || !strings.Contains(err.Error(), "canceled") || called != 1 {
				t.Fatalf("canceled operation reached gate: %v, calls %d", err, called)
			}
			if len(f.reg.checkouts) != 1 || len(f.reg.snapshots) != 1 || len(f.reg.reserved) != 0 || f.reg.nextSnapshot != 1 {
				t.Fatal("failed barrier changed registry state")
			}
			if got := f.read("app", "file"); got != "base\n" {
				t.Fatalf("failed barrier changed bytes: %q", got)
			}
		})
	}
}

func TestQuiesceDeadlineAndConcurrentHolds(t *testing.T) {
	r := &Registry{}
	if release, err := r.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	var received time.Time
	r.config.Quiesce = func(ctx context.Context) (func(), error) {
		received, _ = ctx.Deadline()
		return func() {}, nil
	}
	start := time.Now()
	release, err := r.Quiesce(t.Context())
	if err != nil || received.Before(start) || received.After(time.Now().Add(5*time.Second)) {
		t.Fatalf("unbounded default deadline: %v, %v", received, err)
	}
	release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	release, err = r.Quiesce(ctx)
	if err != nil || !received.Equal(deadline) {
		t.Fatalf("caller deadline lost: %v, %v", received, err)
	}
	release()
	client := pendingClient(r, func() error { return nil })
	first, err := r.Quiesce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		release func()
		err     error
	}
	acquired := make(chan result, 1)
	go func() {
		second, err := r.Quiesce(ctx)
		acquired <- result{second, err}
	}()
	second := <-acquired
	if second.err != nil {
		first()
		t.Fatal(second.err)
	}
	first()
	if client.active.Load() != 1 {
		t.Fatal("first release lifted another operation's gate")
	}
	second.release()
	if client.active.Load() != 0 {
		t.Fatal("concurrent gate leaked")
	}
	ctx, cancel = context.WithCancel(t.Context())
	r.config.Quiesce = func(context.Context) (func(), error) { cancel(); return func() {}, nil }
	if release, err := r.Quiesce(ctx); err == nil || release != nil {
		t.Fatal("cancellation during a successful callback was accepted")
	}
	wait, stop := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stop()
	r.config.Quiesce = func(ctx context.Context) (func(), error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if release, err := r.Quiesce(wait); err == nil || release != nil {
		t.Fatal("blocked acquisition ignored caller deadline")
	}
	r.config.Quiesce = func(context.Context) (func(), error) { return nil, nil }
	if release, err := r.Quiesce(t.Context()); err == nil || release != nil {
		t.Fatal("missing release accepted")
	}
}

func TestQuiesceCloseDoesNotRecallDisconnectedClients(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "flush", true: "persist_error"}[fail], func(t *testing.T) {
			f := newFixture(t)
			f.newCheckout(folder(t, map[string]string{"file": "base"}), "app")
			f.put("app", "file", "host pending")
			upper := f.reg.checkouts["app"].upper
			f.reg.config.Quiesce = func(context.Context) (func(), error) {
				t.Error("Close tried to recall clients after transport shutdown")
				return nil, errors.New("transport closed")
			}
			if fail {
				state := filepath.Join(f.state, "state.json")
				if err := os.Remove(state); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(state, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.reg.Close(); (err != nil) != fail {
				t.Fatalf("Close error = %v, failure expected %v", err, fail)
			}
			if got, err := os.ReadFile(filepath.Join(upper, "file")); err != nil || string(got) != "host pending" {
				t.Fatalf("Close lost host writes: %q, %v", got, err)
			}
		})
	}
}

func TestWriteCacheableOnlyIsolatedOwnedUpper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX link metadata is required for write caching")
	}
	f := newFixture(t)
	dir := folder(t, map[string]string{
		"base": "base", "cache/out": "cache", "local/secret": "local", "hidden/out": "hidden",
	})
	f.newCheckout(dir, "app",
		"cache dir & cache", "local dir & local", "hidden dir & hidden",
		"cache dir & new-cache", "local dir & new-local", "hidden dir & new-hidden")
	for _, p := range []string{"base", "cache/out", "local/secret", "hidden/out"} {
		if f.reg.WriteCacheable("app", p) {
			t.Errorf("unowned lower path %q is cacheable", p)
		}
	}
	if got := f.read("app", "hidden/out"); got != "<absent>" {
		t.Fatalf("hidden lower was visible before upper creation: %q", got)
	}
	owned := []string{
		"upper", "cache/out", "local/secret", "hidden/out",
		"cache/new", "local/new", "hidden/new",
		"new-cache/out", "new-local/secret", "new-hidden/out",
	}
	for _, p := range owned {
		f.put("app", p, "written")
		if !f.reg.WriteCacheable("app", p) {
			t.Errorf("owned memory upper %q is not cacheable", p)
		}
	}
	for _, p := range []string{"cache/out", "local/secret", "hidden/out"} {
		write(t, filepath.Join(dir, p), "source changed")
		if got := f.read("app", p); got != "written" || !f.reg.WriteCacheable("app", p) {
			t.Errorf("source edit affected owned upper %q: %q", p, got)
		}
	}
	f.put("app", ".git/config", "metadata")
	f.put("app", ".jj/repo", "metadata")
	f.put("app", "spill", "small")
	if !f.reg.WriteCacheable("app", "spill") {
		t.Fatal("new memory file is not cacheable")
	}
	f.put("app", "spill", strings.Repeat("x", 2<<20))
	if !f.reg.WriteCacheable("app", "spill") {
		t.Fatal("spilling a memory file lost its isolated upper ownership")
	}
	var s Session
	if err := f.reg.Enter("app", &s); err != nil {
		t.Fatal(err)
	}
	defer s.Done()
	if err := s.FS.Symlink("upper", "symlink"); err != nil {
		t.Fatal(err)
	}
	if err := s.FS.Symlink("cache", "redirect"); err != nil {
		t.Fatal(err)
	}
	// Linking a memory file spills it; neither alias may keep a write cache.
	if err := s.FS.Link("upper", "linked"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "base", "cache", "symlink", "redirect/out", "upper", "linked", "../upper", ".git/config", ".jj/repo"} {
		if f.reg.WriteCacheable("app", p) {
			t.Errorf("unsafe path %q is cacheable", p)
		}
	}
	if err := s.FS.Remove("linked"); err != nil {
		t.Fatal(err)
	}
	if !f.reg.WriteCacheable("app", "upper") {
		t.Fatal("isolated disk upper is not cacheable")
	}
	if f.reg.WriteCacheable("missing", "upper") {
		t.Fatal("missing checkout is cacheable")
	}
	s.Done()
	if _, err := f.reg.Snap(t.Context(), api.SnapArgs{Checkout: "app", Wait: true}); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(owned, "spill") {
		if f.reg.WriteCacheable("app", p) {
			t.Fatalf("frozen layer path %q is cacheable", p)
		}
	}
}
