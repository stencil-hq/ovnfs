//go:build !windows

package nfs_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/stencil-hq/ovnfs/internal/nodestest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/nfstest"
	"github.com/stencil-hq/ovnfs/nfs"
)

// pressuredFS switches the identity cache's current directory after each id.
// A paginated listing's handles must work after this separate cache evicts them.
type pressuredFS struct{ nfs.Backend }

func (f pressuredFS) FileID(p string) uint64 {
	id := f.Backend.FileID(p)
	f.Backend.FileID("co/pressure/file")
	return id
}

func (f pressuredFS) ChildID(dir, name string) uint64 {
	id := f.Backend.ChildID(dir, name)
	f.Backend.FileID("co/pressure/file")
	return id
}

// TestEvictedHandlesAndReaddir uses a real overlay and wire NFSv3 requests.
func TestEvictedHandlesAndReaddir(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	for _, dir := range []string{"files", "pressure"} {
		if err := os.MkdirAll(filepath.Join(source, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 300 {
		name := fmt.Sprintf("f%04d", i)
		if err := os.WriteFile(filepath.Join(source, "files", name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "pressure", "file"), []byte("pressure"), 0644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	reg, err := checkout.Open(checkout.Config{State: state, Mount: filepath.Join(root, "mount")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reg.Close(); err != nil {
			t.Error(err)
		}
	})
	e, err := export.New(export.NewRouter(reg), filepath.Join(state, "nodes.log"), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	e.SetNodeCache(0)
	if _, err := reg.New(context.Background(), api.NewArgs{Dir: source, Name: "co"}); err != nil {
		t.Fatal(err)
	}
	c := nfstest.Start(t, pressuredFS{e.NFS()})
	status := func(st uint32, err error) {
		t.Helper()
		if err != nil || st != nfstest.OK {
			t.Fatalf("status %d: %v", st, err)
		}
	}
	co, _, st, err := c.Lookup(c.Root, "co")
	status(st, err)
	dir, _, st, err := c.Lookup(co, "files")
	status(st, err)
	old, _, st, err := c.Lookup(dir, "f0000")
	status(st, err)
	entries, st, err := c.ReadDirPlus(dir)
	status(st, err)
	count := 0
	for _, entry := range entries {
		if entry.FileName == "." || entry.FileName == ".." {
			continue
		}
		count++
		attr, st, err := c.GetAttr(entry.Handle.FH)
		status(st, err)
		if attr.Fileid != entry.FileId {
			t.Fatal("READDIRPLUS and GETATTR identities differ")
		}
		data, _, st, err := c.Read(entry.Handle.FH, 0, 64)
		status(st, err)
		if string(data) != entry.FileName {
			t.Fatalf("wrong-file read: %s -> %q", entry.FileName, data)
		}
	}
	if count != 300 {
		t.Fatalf("READDIRPLUS returned %d entries", count)
	}
	st, err = c.Rename(dir, "f0000", dir, "moved")
	status(st, err)
	replacement, st, err := c.Create(dir, "f0000", 0644)
	status(st, err)
	data, _, st, err := c.Read(old, 0, 64)
	status(st, err)
	if string(data) != "f0000" {
		t.Fatalf("old handle lost its moved file: %q", data)
	}
	if string(replacement) == string(old) {
		t.Fatal("new file reused moved identity")
	}
	st, err = c.Remove(dir, "moved")
	status(st, err)
	if _, st, err := c.GetAttr(old); err != nil || st != nfstest.Stale {
		t.Fatalf("removed handle status %d: %v", st, err)
	}
}

// evictionClient gives each pressure walker a separate TCP connection.
type evictionClient struct {
	t     *testing.T
	c     *nfstest.Client
	nodes *export.Nodes
}

func (c evictionClient) ID(p string) uint64 {
	handle := c.c.Root
	for _, name := range strings.Split(p, "/") {
		h, _, st, err := c.c.Lookup(handle, name)
		if err != nil || st != 0 {
			c.t.Fatalf("LOOKUP %s: %d %v", p, st, err)
		}
		handle = h
	}
	return binary.BigEndian.Uint64(handle)
}
func (c evictionClient) Read(id uint64) (string, bool) {
	data, _, st, err := c.c.Read(binary.BigEndian.AppendUint64(nil, id), 0, 4096)
	if err != nil {
		c.t.Error(err)
	}
	return string(data), err == nil && st == 0
}
func (c evictionClient) List(id uint64) ([]string, bool) {
	entries, st, err := c.c.ReadDirPlus(binary.BigEndian.AppendUint64(nil, id))
	if err != nil || st != 0 {
		return nil, false
	}
	var names []string
	for _, e := range entries {
		if e.FileName != "." && e.FileName != ".." {
			names = append(names, e.FileName)
		}
	}
	return names, true
}
func (c evictionClient) Rename(from, to string) {
	a, an := path.Split(from)
	b, bn := path.Split(to)
	st, err := c.c.Rename(binary.BigEndian.AppendUint64(nil, c.ID(strings.TrimSuffix(a, "/"))), an, binary.BigEndian.AppendUint64(nil, c.ID(strings.TrimSuffix(b, "/"))), bn)
	if err != nil || st != 0 {
		c.t.Fatalf("RENAME: %d %v", st, err)
	}
}
func (c evictionClient) Remove(p string) {
	d, n := path.Split(p)
	st, err := c.c.Remove(binary.BigEndian.AppendUint64(nil, c.ID(strings.TrimSuffix(d, "/"))), n)
	if err != nil || st != 0 {
		c.t.Fatalf("REMOVE: %d %v", st, err)
	}
}
func (c evictionClient) Create(p, data string) {
	d, n := path.Split(p)
	h, st, err := c.c.Create(binary.BigEndian.AppendUint64(nil, c.ID(strings.TrimSuffix(d, "/"))), n, 0644)
	if err != nil || st != 0 {
		c.t.Fatalf("CREATE: %d %v", st, err)
	}
	_, st, err = c.c.Write(h, 0, []byte(data))
	if err != nil || st != 0 {
		c.t.Fatalf("WRITE: %d %v", st, err)
	}
}
func (c evictionClient) Path(id uint64) (string, bool) {
	_, st, err := c.c.GetAttr(binary.BigEndian.AppendUint64(nil, id))
	if err != nil || st != 0 {
		return "", false
	}
	return c.nodes.Path(id)
}
func TestConcurrentColdMutations(t *testing.T) {
	e, state := nodestest.New(t, nodestest.Files())
	nodestest.Pressure(t, e, state, func() nodestest.Client { return evictionClient{t, nfstest.Start(t, e.NFS()), e.Nodes()} })
}

// TestEvictionWireLatency measures the production paginated READDIRPLUS path,
// with no artificial directory pin, alongside unrelated wire LOOKUP requests.
func TestEvictionWireLatency(t *testing.T) {
	if os.Getenv("OVNFS_LATENCY") == "" {
		t.Skip("set OVNFS_LATENCY=1")
	}
	files := map[string]string{"other/file": "other", "pressure/file": "pressure"}
	for i := range 20000 {
		files[fmt.Sprintf("wide/f%05d", i)] = "content"
	}
	e, _ := nodestest.New(t, files)
	e.SetNodeCache(0)
	c := nfstest.Start(t, e.NFS())
	other := nfstest.Start(t, e.NFS())
	lookup := func(c *nfstest.Client, dir []byte, name string) []byte {
		t.Helper()
		h, _, st, err := c.Lookup(dir, name)
		if st != 0 || err != nil {
			t.Fatalf("LOOKUP %d %v", st, err)
		}
		return h
	}
	co := lookup(c, c.Root, "co")
	wide := lookup(c, co, "wide")
	od := lookup(other, co, "other")
	pd := lookup(c, co, "pressure")
	entries, st, err := c.ReadDirPlus(wide)
	if st != 0 || err != nil || len(entries) != 20002 {
		t.Fatalf("first listing %d %d %v", len(entries), st, err)
	}
	lookup(c, pd, "file")
	stop := make(chan struct{})
	done := make(chan []time.Duration, 1)
	go func() {
		var lat []time.Duration
		for {
			select {
			case <-stop:
				done <- lat
				return
			default:
			}
			start := time.Now()
			lookup(other, od, "file")
			lat = append(lat, time.Since(start))
			time.Sleep(100 * time.Microsecond)
		}
	}()
	start := time.Now()
	entries, st, err = c.ReadDirPlus(wide)
	elapsed := time.Since(start)
	close(stop)
	lat := <-done
	if st != 0 || err != nil || len(entries) != 20002 {
		t.Fatalf("rewalk %d %d %v", len(entries), st, err)
	}
	slices.Sort(lat)
	if len(lat) == 0 {
		t.Fatal("no concurrent lookup samples")
	}
	t.Logf("wire_names=20000 rewalk=%s unrelated_samples=%d p50=%s p95=%s max=%s", elapsed, len(lat), lat[len(lat)/2], lat[len(lat)*95/100], lat[len(lat)-1])
}
