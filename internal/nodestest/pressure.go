// Package nodestest shares real-export eviction fixtures between protocol tests.
package nodestest

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
)

func New(t testing.TB, files map[string]string) (*export.Export, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	state := filepath.Join(root, "state")
	for name, data := range files {
		p := filepath.Join(source, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(p, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
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
	t.Cleanup(func() { e.Close() })
	if _, err := reg.New(context.Background(), api.NewArgs{Dir: source, Name: "co"}); err != nil {
		t.Fatal(err)
	}
	return e, state
}

// Cold checks the exact derived owner in the disposable index, not just that
// some directory spilled.
func Cold(t testing.TB, state string, id uint64, name string) {
	t.Helper()
	if id&(1<<52) == 0 {
		t.Fatalf("expected derived identity %x", id)
	}
	key := id >> 22 & ((1 << 30) - 1)
	hash := uint32(id & ((1 << 22) - 1))
	files, _ := filepath.Glob(filepath.Join(state, "nodes.log.cache", "instance-*", fmt.Sprintf("%02x", key&255), fmt.Sprint(key)))
	if len(files) != 1 {
		t.Fatalf("cold index for %x: %v", id, files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range binary.LittleEndian.Uint32(data[:4]) {
		row := data[4+16*i:]
		if binary.LittleEndian.Uint32(row) == hash {
			size := binary.LittleEndian.Uint32(row[4:])
			off := binary.LittleEndian.Uint64(row[8:])
			if string(data[off:off+uint64(size)]) == name {
				return
			}
		}
	}
	t.Fatalf("index has no owner %s for %x", name, id)
}

// Client performs protocol operations without pinning the stateless handles
// retained by this test. Each worker owns its own connection.
type Client interface {
	ID(string) uint64
	Read(uint64) (string, bool)
	List(uint64) ([]string, bool)
	Rename(string, string)
	Remove(string)
	Create(string, string)
	Path(uint64) (string, bool)
}

// Pressure overlaps listings and namespace changes. The oracle lock protects
// generation snapshots only; no filesystem call runs under it.
func Pressure(t *testing.T, e *export.Export, state string, newClient func() Client) {
	t.Helper()
	c := newClient()
	e.SetNodeCache(0)
	overlapListings(t, e, state, c, newClient)
	type identity struct {
		id         uint64
		name, data string
		dir, dead  bool
	}
	const rounds = 24
	var ids [][]identity
	var dirs []uint64
	var generations [rounds]uint64
	for i := range rounds {
		d := fmt.Sprintf("co/m%02d", i)
		dirs = append(dirs, c.ID(d))
		ids = append(ids, []identity{{c.ID(d + "/file"), d + "/file", "original", false, false}, {c.ID(d + "/dead"), d + "/dead", "old", false, false}, {c.ID(d + "/f998"), d + "/f998", "collision owner", false, false}, {c.ID(d + "/empty"), d + "/empty", "", true, false}})
	}
	pressure := func() {
		for i := range 8 {
			c.ID(fmt.Sprintf("co/pressure/d%d/file", i))
		}
	}
	pressure()
	for _, set := range ids {
		for _, v := range set {
			Cold(t, state, v.id, path.Base(v.name))
		}
	}
	var oracle sync.RWMutex
	var wg sync.WaitGroup
	stop := make(chan struct{})
	done := make(chan struct{})
	for range 3 {
		worker := newClient()
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := range 8 {
					worker.ID(fmt.Sprintf("co/pressure/d%d/file", i))
				}
				for i := range rounds {
					oracle.RLock()
					gen := generations[i]
					oracle.RUnlock()
					if gen%2 != 0 {
						continue
					}
					names, ok := worker.List(dirs[i])
					oracle.RLock()
					stable := generations[i] == gen
					oracle.RUnlock()
					if !stable {
						continue
					}
					want := []string{"dead", "empty", "f998", "file"}
					if gen != 0 {
						want = []string{"dead", "f4776", "f998", "file", "moved", "renamed"}
					}
					slices.Sort(names)
					slices.Sort(want)
					if !ok || !slices.Equal(names, want) {
						t.Errorf("stable listing %d generation %d = %v (%v), want %v", i, gen, names, ok, want)
					}
				}
			}
		})
	}
	go func() { wg.Wait(); close(done) }()
	defer func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("pressure workers did not stop")
		}
	}()
	for i, set := range ids {
		oracle.Lock()
		generations[i]++
		oracle.Unlock()
		d := fmt.Sprintf("co/m%02d", i)
		moved := d + "-moved"
		c.Rename(d+"/file", d+"/moved")
		c.Create(d+"/file", "replacement")
		c.Rename(d+"/empty", d+"/renamed")
		c.Remove(d + "/dead")
		c.Create(d+"/dead", "replacement")
		c.Create(d+"/f4776", "colliding replacement")
		c.Rename(d, moved)
		for j := range set {
			set[j].name = moved + strings.TrimPrefix(set[j].name, d)
		}
		set[0].name = moved + "/moved"
		set[1].dead = true
		set[3].name = moved + "/renamed"
		ids[i] = set
		oracle.Lock()
		generations[i]++
		oracle.Unlock()
		for _, v := range set {
			if v.dir {
				if got, ok := c.Path(v.id); !ok || got != v.name {
					t.Errorf("directory %x: %q %v, want %q", v.id, got, ok, v.name)
				}
				continue
			}
			got, ok := c.Read(v.id)
			if v.dead {
				if ok {
					t.Errorf("removed handle %x read %q", v.id, got)
				}
			} else if !ok || got != v.data {
				t.Errorf("retained handle %x read %q %v, want %q", v.id, got, ok, v.data)
			}
		}
	}
	t.Logf("mutated %d cold fixtures; verified moved files, empty directories, ancestors, removal/recreation, colliding owners and complete generation-consistent listings", rounds)
}

func Files() map[string]string {
	files := map[string]string{}
	for i := range 2 {
		d := fmt.Sprintf("overlap%d/", i)
		for name, data := range map[string]string{"file": "original", "dead": "old", "f998": "collision owner"} {
			files[d+name] = data
		}
	}
	for i := range 24 {
		d := fmt.Sprintf("m%02d/", i)
		for name, data := range map[string]string{"file": "original", "dead": "old", "f998": "collision owner", "empty/": ""} {
			files[d+name] = data
		}
	}
	for i := range 8 {
		files[fmt.Sprintf("pressure/d%d/file", i)] = "pressure"
	}
	return files
}

// listingBarrier pauses a backend snapshot before nodeFS observes it. It is
// installed before clients start, and consumes one arm so mutation's own reads
// cannot deadlock behind the paused request.
type listingBarrier struct {
	billy.Filesystem
	mu               sync.Mutex
	target           string
	entered, release chan struct{}
}

func (b *listingBarrier) ReadDir(p string) ([]os.FileInfo, error) {
	infos, err := b.Filesystem.ReadDir(p)
	b.mu.Lock()
	entered, release := b.entered, b.release
	pause := p == b.target && entered != nil
	if pause {
		b.entered = nil
	}
	b.mu.Unlock()
	if pause {
		close(entered)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("listing barrier timed out")
		}
	}
	return infos, err
}
func overlapListings(t *testing.T, e *export.Export, state string, c Client, newClient func() Client) {
	t.Helper()
	nodes := e.Nodes()
	b := &listingBarrier{Filesystem: nodes.Filesystem}
	nodes.Filesystem = b
	for i := range 2 {
		d := fmt.Sprintf("co/overlap%d", i)
		old, dead, collision := c.ID(d+"/file"), c.ID(d+"/dead"), c.ID(d+"/f998")
		dir := c.ID(d)
		c.ID("co/pressure/d0/file")
		Cold(t, state, old, "file")
		entered, release := make(chan struct{}), make(chan struct{})
		b.mu.Lock()
		b.target, b.entered, b.release = d, entered, release
		b.mu.Unlock()
		worker := newClient()
		type result struct {
			names []string
			ok    bool
		}
		done := make(chan result, 1)
		go func() { names, ok := worker.List(dir); done <- result{names, ok} }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("listing did not reach backend barrier")
		}
		c.Rename(d+"/file", d+"/moved")
		c.Create(d+"/file", "replacement")
		c.Remove(d + "/dead")
		c.Create(d+"/dead", "replacement")
		c.Create(d+"/f4776", "colliding replacement")
		if i == 1 {
			c.Rename(d, d+"-moved")
			d += "-moved"
		}
		close(release)
		select {
		case got := <-done:
			// A request spanning mutation may be stale or return its old
			// snapshot; it may also retry and return the committed namespace.
			slices.Sort(got.names)
			if got.ok && !slices.Equal(got.names, []string{"dead", "f998", "file"}) && !slices.Equal(got.names, []string{"dead", "f4776", "f998", "file", "moved"}) {
				t.Fatalf("overlapping listing: %v", got.names)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("overlapping listing did not finish")
		}
		for _, v := range []struct {
			id         uint64
			name, data string
		}{{old, "moved", "original"}, {collision, "f998", "collision owner"}} {
			if got, ok := c.Read(v.id); !ok || got != v.data {
				t.Fatalf("retained %x read %q (%v)", v.id, got, ok)
			}
			if got := c.ID(d + "/" + v.name); got != v.id {
				t.Fatalf("owner changed %x -> %x", v.id, got)
			}
		}
		if _, ok := c.Read(dead); ok {
			t.Fatal("removed handle named replacement")
		}
		if c.ID(d+"/file") == old || c.ID(d+"/dead") == dead || c.ID(d+"/f4776") == collision {
			t.Fatal("replacement inherited old handle")
		}
		names, ok := c.List(dir)
		slices.Sort(names)
		if !ok || !slices.Equal(names, []string{"dead", "f4776", "f998", "file", "moved"}) {
			t.Fatalf("committed listing incomplete: %v %v", names, ok)
		}
	}
	t.Log("overlapped two backend listings with move/recreation, including an ancestor move; verified exact retained and replacement ownership")
}
