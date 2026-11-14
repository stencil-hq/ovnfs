package fuse

import (
	"context"
	"github.com/stencil-hq/ovnfs/internal/nodestest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// rawNodes uses the real export without a kernel mount or its attribute cache.
func rawNodes(t *testing.T) (*export.Export, *exportFS, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	state := filepath.Join(root, "state")
	for _, d := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(source, d), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, d, "file"), []byte(d), 0644); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := checkout.Open(checkout.Config{State: state, Mount: filepath.Join(root, "mount")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	e, err := export.New(export.NewRouter(reg), filepath.Join(state, "nodes.log"), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if _, err = reg.New(context.Background(), api.NewArgs{Dir: source, Name: "co"}); err != nil {
		t.Fatal(err)
	}
	return e, newExportFS(e.Nodes()), state
}

// TestSpillWriteFailureKeepsOpenFile injects the first failed spill inside
// LOOKUP, after its backend stat. Existing descriptors must still read/write/sync.
func TestSpillWriteFailureKeepsOpenFile(t *testing.T) {
	e, x, state := rawNodes(t)
	var old gofuse.EntryOut
	if st := x.entry("co/a/file", &old); st != gofuse.OK {
		t.Fatal(st)
	}
	var opened gofuse.OpenOut
	if st := x.Open(nil, &gofuse.OpenIn{InHeader: gofuse.InHeader{NodeId: old.NodeId}, Flags: syscall.O_RDWR}, &opened); st != gofuse.OK {
		t.Fatal(st)
	}
	defer x.Release(nil, &gofuse.ReleaseIn{Fh: opened.Fh})
	// The first spill creates the owner index pack, so replace its store
	// before any, then fill the budget exactly: the next LOOKUP's trim spills.
	parent := filepath.Join(state, "nodes.log.cache")
	if err := os.RemoveAll(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	e.SetNodeCache(e.NodeCache().Used)
	var out gofuse.EntryOut
	if st := x.entry("co/b/file", &out); st != gofuse.OK || out.NodeId == 0 {
		t.Fatalf("LOOKUP: %v %x", st, out.NodeId)
	}
	var refused gofuse.EntryOut
	if st := x.entry("co/c/file", &refused); st != gofuse.EIO || refused.NodeId != 0 {
		t.Fatalf("new allocation during failed spill: %v %x", st, refused.NodeId)
	}
	if e.NodeCacheError() == "" || x.nodes.IdentityError() != nil {
		t.Fatal("write failure did not remain degraded")
	}
	if _, ok := x.nodes.Path(old.NodeId); !ok {
		t.Fatal("resident handle stopped resolving")
	}
	if _, st := x.Write(nil, &gofuse.WriteIn{Fh: opened.Fh}, []byte("updated")); st != gofuse.OK {
		t.Fatal(st)
	}
	if st := x.Fsync(nil, &gofuse.FsyncIn{Fh: opened.Fh}); st != gofuse.OK {
		t.Fatal(st)
	}
	buf := make([]byte, 64)
	result, st := x.Read(nil, &gofuse.ReadIn{Fh: opened.Fh, Size: 64}, buf)
	if st != gofuse.OK {
		t.Fatal(st)
	}
	data, st := result.Bytes(buf)
	if st != gofuse.OK || string(data) != "updated" {
		t.Fatalf("READ %q: %v", data, st)
	}
}

// TestLookupColdFaultInsideOperation removes authoritative ownership after the
// parent was resolved; entry's ID call must return EIO with no published inode.
func TestLookupColdFaultInsideOperation(t *testing.T) {
	e, x, state := rawNodes(t)
	e.SetNodeCache(0)
	x.nodes.ID("co/a/file")
	x.nodes.ID("co/b/file")
	nodestest.BreakCold(t, state)
	if x.nodes.IdentityError() != nil {
		t.Fatal("fault latched before operation")
	}
	var out gofuse.EntryOut
	if st := x.entry("co/a/file", &out); st != gofuse.EIO || out.NodeId != 0 {
		t.Fatalf("LOOKUP %v inode %x", st, out.NodeId)
	}
}

// TestNonRootDaemonOwnsEveryNode checks the owners LOOKUP reports. A daemon
// not running as root reports every node, source files and new entries alike,
// as its own user's with the source's mode bits, so default_permissions lets
// it write a checkout of another user's folder (#57); a root daemon reports
// the real owners. The daemon's ids differ from the test's, which own the
// source, to stand for a source owned by another user.
func TestNonRootDaemonOwnsEveryNode(t *testing.T) {
	_, x, _ := rawNodes(t)
	x.uid, x.gid = uint32(os.Geteuid())+1, uint32(os.Getegid())+1
	var created gofuse.CreateOut
	var dir gofuse.EntryOut
	if st := x.entry("co/a", &dir); st != gofuse.OK {
		t.Fatal(st)
	}
	if st := x.Create(nil, &gofuse.CreateIn{InHeader: gofuse.InHeader{NodeId: dir.NodeId}, Flags: syscall.O_RDWR, Mode: 0o640}, "new", &created); st != gofuse.OK {
		t.Fatal(st)
	}
	x.Release(nil, &gofuse.ReleaseIn{Fh: created.Fh})
	for _, root := range []bool{false, true} {
		x.chown = root
		for p, mode := range map[string]uint32{"co/a": syscall.S_IFDIR | 0o755, "co/a/file": syscall.S_IFREG | 0o644, "co/a/new": syscall.S_IFREG | 0o640} {
			info, err := x.nodes.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			owner, _ := unixstat.Of(info)
			uid, gid := x.uid, x.gid
			if root {
				uid, gid = owner.UID, owner.GID
			}
			var out gofuse.EntryOut
			if st := x.entry(p, &out); st != gofuse.OK {
				t.Fatal(st)
			}
			if out.Uid != uid || out.Gid != gid || out.Mode != mode {
				t.Errorf("root daemon %v: %s is %d:%d %o, want %d:%d %o", root, p, out.Uid, out.Gid, out.Mode, uid, gid, mode)
			}
		}
	}
}

type evictionClient struct {
	t *testing.T
	x *exportFS
}

func (c evictionClient) ID(p string) uint64 {
	var out gofuse.EntryOut
	if st := c.x.entry(p, &out); st != gofuse.OK {
		c.t.Fatalf("LOOKUP %s: %v", p, st)
	}
	return out.NodeId
}
func (c evictionClient) Read(id uint64) (string, bool) {
	var out gofuse.OpenOut
	st := c.x.Open(nil, &gofuse.OpenIn{InHeader: gofuse.InHeader{NodeId: id}}, &out)
	if st != gofuse.OK {
		return "", false
	}
	defer c.x.Release(nil, &gofuse.ReleaseIn{Fh: out.Fh})
	buf := make([]byte, 4096)
	r, st := c.x.Read(nil, &gofuse.ReadIn{Fh: out.Fh, Size: 4096}, buf)
	if st != gofuse.OK {
		return "", false
	}
	data, st := r.Bytes(buf)
	return string(data), st == gofuse.OK
}
func (c evictionClient) List(id uint64) ([]string, bool) {
	var out gofuse.OpenOut
	if st := c.x.OpenDir(nil, &gofuse.OpenIn{InHeader: gofuse.InHeader{NodeId: id}}, &out); st != gofuse.OK {
		return nil, false
	}
	defer c.x.ReleaseDir(&gofuse.ReleaseIn{Fh: out.Fh})
	d := c.x.dir(out.Fh)
	var names []string
	for _, e := range d.entries {
		if e.name != "." && e.name != ".." {
			names = append(names, e.name)
		}
	}
	return names, true
}
func (c evictionClient) Rename(from, to string) {
	a, an := path.Split(from)
	b, bn := path.Split(to)
	if st := c.x.Rename(nil, &gofuse.RenameIn{InHeader: gofuse.InHeader{NodeId: c.ID(strings.TrimSuffix(a, "/"))}, Newdir: c.ID(strings.TrimSuffix(b, "/"))}, an, bn); st != gofuse.OK {
		c.t.Fatal(st)
	}
}
func (c evictionClient) Remove(p string) {
	d, n := path.Split(p)
	if st := c.x.Unlink(nil, &gofuse.InHeader{NodeId: c.ID(strings.TrimSuffix(d, "/"))}, n); st != gofuse.OK {
		c.t.Fatal(st)
	}
}
func (c evictionClient) Create(p, data string) {
	d, n := path.Split(p)
	var out gofuse.CreateOut
	if st := c.x.Create(nil, &gofuse.CreateIn{InHeader: gofuse.InHeader{NodeId: c.ID(strings.TrimSuffix(d, "/"))}, Flags: syscall.O_RDWR, Mode: 0644}, n, &out); st != gofuse.OK {
		c.t.Fatal(st)
	}
	defer c.x.Release(nil, &gofuse.ReleaseIn{Fh: out.Fh})
	if _, st := c.x.Write(nil, &gofuse.WriteIn{Fh: out.Fh}, []byte(data)); st != gofuse.OK {
		c.t.Fatal(st)
	}
}
func (c evictionClient) Path(id uint64) (string, bool) {
	var out gofuse.AttrOut
	if st := c.x.GetAttr(nil, &gofuse.GetAttrIn{InHeader: gofuse.InHeader{NodeId: id}}, &out); st != gofuse.OK {
		return "", false
	}
	return c.x.nodes.Path(id)
}
func TestConcurrentColdMutations(t *testing.T) {
	e, state := nodestest.New(t, nodestest.Files())
	x := newExportFS(e.Nodes())
	nodestest.Pressure(t, e, state, func() nodestest.Client { return evictionClient{t, x} })
}
