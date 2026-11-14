package export

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLateColdReadFault rejects the id selected before a trim discovers that
// an existing authoritative index cannot be read for its replacement.
func TestLateColdReadFault(t *testing.T) {
	ns := newNamespace("co/file", "co/new", "pressure/entry")
	nt := newNodeTable(ns.list)
	nt.id("co/file")
	forceCold(t, nt, ns, "co")
	nt.cacheLimit = 1 << 20
	nt.mu.Lock()
	nt.addLeaf(nt.find("co"), "new")
	nt.find("co").dir.coldDirty = true
	nt.mu.Unlock()
	breakOwners(t, nt.pack.path, ownerHead(t, nt, nt.find("co").key))
	nt.cacheLimit = 0
	if id := nt.id("pressure/entry"); id != 0 || nt.cacheErr == nil {
		t.Fatalf("late fault returned %x, %v", id, nt.cacheErr)
	}
	// forceCold's cleanup expects success; the fault is asserted above.
	nt.cacheErr = nil
	nt.fault.Store(nil)
}

func TestSpillRetryPreservesResidentOwners(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	old := nt.id("co/file")
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	nt.coldRoot = blocked
	nt.cacheLimit = 0
	fresh := nt.id("pressure/entry")
	if nt.spillErr == nil || nt.cacheErr != nil || fresh == 0 {
		t.Fatalf("failed spill: %v %v %x", nt.spillErr, nt.cacheErr, fresh)
	}
	used, next := nt.cacheUsed, nt.next
	for i := range 1000 {
		p := fmt.Sprintf("new%d/file", i)
		ns.add(p)
		if nt.id(p) != 0 {
			t.Fatal("degraded cache admitted a new identity")
		}
	}
	if nt.cacheUsed != used || nt.next != next {
		t.Fatal("degraded admissions grew ownership")
	}
	if release, err := nt.guard(false, "new0/file"); err == nil {
		release()
		t.Fatal("degraded mutation admitted new ownership")
	}
	if err := nt.admitListing("new0"); err == nil {
		t.Fatal("degraded listing admitted a snapshot")
	}
	retry := nt.spillRetry
	for range 10 {
		if pathOf(t, nt, old) != "co/file" {
			t.Fatal("lost resident owner")
		}
	}
	if nt.spillRetry != retry || !retry.After(time.Now()) {
		t.Fatal("retry did not back off")
	}
	nt.coldRoot = ""
	nt.spillRetry = time.Time{}
	nt.id("pressure/entry")
	if nt.spillErr != nil || nt.cacheUsed > nt.cacheLimit+512 {
		t.Fatalf("retry: %v, usage %d", nt.spillErr, nt.cacheUsed)
	}
	if pathOf(t, nt, old) != "co/file" {
		t.Fatal("retry lost owner")
	}
}

func TestColdStoreLifecycle(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "nodes.log")
	abandoned := filepath.Join(name+".cache", "instance-abandoned")
	if err := os.MkdirAll(abandoned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "old"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ns := newNamespace("co/file", "pressure/entry")
	nt := openLogged(t, name, ns)
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned store survived: %v", err)
	}
	nt.id("co/file")
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	if filepath.Dir(nt.coldRoot) != name+".cache" {
		t.Fatal(nt.coldRoot)
	}
	if nt.pack == nil || nt.pack.end == 0 || nt.pack.end != nt.coldBytes {
		t.Fatalf("index size %v != %d", nt.pack, nt.coldBytes)
	}
	other := openLogged(t, filepath.Join(t.TempDir(), "nodes.log"), ns)
	defer closeLogged(t, other)
	if _, err := os.Stat(nt.coldRoot); err != nil {
		t.Fatal("independent export removed a live store", err)
	}
	cold := nt.coldRoot
	closeLogged(t, nt)
	if _, err := os.Stat(cold); err != nil {
		t.Fatal("planned close removed store", err)
	}
	nt = openLogged(t, name, ns)
	if nt.coldRoot != cold {
		t.Fatalf("reopen after a clean close used %q, not the completed store", nt.coldRoot)
	}
	closeLogged(t, nt)
	// The store belongs to the log it was completed for.
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	nt = openLogged(t, name, ns)
	defer closeLogged(t, nt)
	if _, err := os.Stat(cold); !os.IsNotExist(err) {
		t.Fatal("reopen with another log retained the store", err)
	}
}
