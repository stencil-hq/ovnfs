package export

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

type faultFile struct {
	billy.File
	closes int
}

func (f *faultFile) Close() error { f.closes++; return f.File.Close() }

type faultCreateFS struct {
	billy.Filesystem
	after    func()
	file     *faultFile
	tempName string
}

func (f *faultCreateFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	f.file = &faultFile{File: file}
	f.after()
	return f.file, nil
}
func (f *faultCreateFS) TempFile(dir, prefix string) (billy.File, error) {
	var file billy.File
	var err error
	if f.tempName != "" {
		file, err = f.Filesystem.OpenFile(path.Join(dir, f.tempName), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	} else {
		file, err = f.Filesystem.TempFile(dir, prefix)
	}
	if err != nil {
		return nil, err
	}
	f.file = &faultFile{File: file}
	f.after()
	return f.file, nil
}

// TestLateCreateFaultClosesResult faults inside backend creation, after
// preflight and before mutation bookkeeping/release choose their final result.
func TestLateCreateFaultClosesResult(t *testing.T) {
	for _, temp := range []bool{false, true} {
		t.Run(map[bool]string{false: "OpenFile", true: "TempFile"}[temp], func(t *testing.T) {
			n := newMetadataTestFS(t)
			fs := &faultCreateFS{Filesystem: n.Filesystem, after: func() { n.nodes.mu.Lock(); defer n.nodes.mu.Unlock(); n.nodes.cacheFailure(os.ErrPermission) }}
			n.Filesystem = fs
			var file billy.File
			var err error
			if temp {
				file, err = n.TempFile("", "temp-")
			} else {
				file, err = n.OpenFile("file", os.O_CREATE|os.O_RDWR, 0600)
			}
			if file != nil || !errors.Is(err, syscall.EIO) {
				t.Fatalf("returned file %v, error %v", file, err)
			}
			if fs.file == nil || fs.file.closes != 1 {
				t.Fatal("late fault did not close the unpublished descriptor exactly once")
			}
		})
	}
}

// TestTempFileCollisionAfterLateFault forces a generated name to collide with
// a cold owner or retirement, then faults before creation bookkeeping. The
// committed entry must be logged even though the caller gets no descriptor.
func TestTempFileCollisionAfterLateFault(t *testing.T) {
	for _, dir := range []string{"", "co/deep"} {
		for _, retired := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retired=%t", dir, retired), func(t *testing.T) {
				n := newMetadataTestFS(t)
				backend := n.Filesystem
				owner, generated := path.Join(dir, collideA), path.Join(dir, collideB)
				for _, p := range []string{owner, "pressure/entry"} {
					parent, _ := split(p)
					if err := backend.MkdirAll(parent, 0755); err != nil {
						t.Fatal(err)
					}
					f, err := backend.Create(p)
					if err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
				log := filepath.Join(t.TempDir(), "nodes.log")
				nt, err := openNodeTable(log, backend.ReadDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { nt.close() })
				n.nodes = nt
				old := nt.id(owner)
				if old == 0 || old&derivedBit == 0 {
					t.Fatal("fixture did not issue a derived owner")
				}
				key := nt.find(dir).key
				if retired {
					if err := n.Remove(owner); err != nil {
						t.Fatal(err)
					}
				}
				nt.cacheLimit = 0
				nt.id("pressure/entry")
				ownerHead(t, nt, key)
				if d := nt.find(dir); d != nil && nt.takenResident(d, nameHash(collideB)) {
					t.Fatal("fixture kept collision ownership resident")
				}
				nt.cacheLimit = 1 << 20
				fs := &faultCreateFS{Filesystem: backend, tempName: collideB, after: func() {
					// Pressure during backend I/O must not evict the guarded owners.
					nt.cacheLimit = 0
					nt.id("pressure/entry")
					nt.mu.Lock()
					nt.cacheFailure(os.ErrPermission)
					nt.mu.Unlock()
				}}
				n.Filesystem = fs
				file, err := n.TempFile(dir, collideB)
				if file != nil || !errors.Is(err, syscall.EIO) {
					t.Fatalf("returned file %v, error %v", file, err)
				}
				if fs.file == nil || fs.file.closes != 1 {
					t.Fatal("late fault did not close the created descriptor exactly once")
				}
				if _, err := backend.Lstat(generated); err != nil {
					t.Fatalf("backend did not commit generated name: %v", err)
				}
				if nt.close() == nil {
					t.Fatal("fault disappeared")
				}
				if !retired {
					// With only the generated name left, an unlogged collision
					// would let the old handle resolve to the new file on replay.
					if err := backend.Remove(owner); err != nil {
						t.Fatal(err)
					}
				}
				reopened, err := openNodeTable(log, backend.ReadDir)
				if err != nil {
					t.Fatal(err)
				}
				nt = reopened
				if got := pathOf(t, nt, old); got != "<stale>" {
					t.Fatalf("old handle aliased generated file after reopen: %s", got)
				}
				if fresh := nt.id(generated); fresh == 0 || fresh == old || fresh&derivedBit != 0 {
					t.Fatalf("committed collision was not logged: %x", fresh)
				}
			})
		}
	}
}

// TestDegradedMutationsDoNotRestoreColdOwners covers already issued names,
// whose repeated O_CREATE used to bypass the fresh-ID admission gate.
func TestDegradedMutationsDoNotRestoreColdOwners(t *testing.T) {
	n := newMetadataTestFS(t)
	for i := range 50 {
		p := fmt.Sprintf("co/d%d/file", i)
		if err := n.MkdirAll(fmt.Sprintf("co/d%d", i), 0755); err != nil {
			t.Fatal(err)
		}
		f, err := n.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		n.nodes.id(p)
	}
	if err := n.MkdirAll("pressure", 0755); err != nil {
		t.Fatal(err)
	}
	f, err := n.OpenFile("pressure/file", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	n.nodes.cacheLimit = 0
	n.nodes.id("pressure/file")
	// Make one dirty resident directory impossible to spill. Backoff must
	// freeze additional ownership, including materialization of cold owners.
	n.nodes.mu.Lock()
	d := n.nodes.find("pressure")
	n.nodes.addLeaf(d, "pending")
	d.dir.coldDirty = true
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	originalRoot := n.nodes.coldRoot
	t.Cleanup(func() { n.nodes.coldRoot = originalRoot; n.nodes.spillErr = nil; n.nodes.close() })
	n.nodes.coldRoot = blocked
	n.nodes.spillErr = os.ErrPermission
	n.nodes.spillRetry = time.Now().Add(time.Hour)
	used, next := n.nodes.cacheUsed, n.nodes.next
	n.nodes.mu.Unlock()
	for i := range 50 {
		p := fmt.Sprintf("co/d%d/file", i)
		f, err := n.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
		if f != nil {
			f.Close()
			t.Fatal("degraded cache admitted cold O_CREATE")
		}
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("cold O_CREATE: %v", err)
		}
	}
	before, err := n.Filesystem.ReadDir("pressure")
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		f, err := n.TempFile("pressure", "fresh-")
		if f != nil {
			f.Close()
			t.Fatal("degraded cache created a TempFile")
		}
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatal(err)
		}
	}
	after, err := n.Filesystem.ReadDir("pressure")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) || n.nodes.cacheUsed != used || n.nodes.next != next {
		t.Fatal("refused mutations grew ownership or changed namespace")
	}
	// The ordinary non-creating writable descriptor path remains usable.
	open, err := n.OpenFile("pressure/file", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if _, err := open.Write([]byte("still writable")); err != nil {
		t.Fatal(err)
	}
	// Restore the index path only for private-test cleanup.
	n.nodes.coldRoot = ""
	n.nodes.spillErr = nil
}
