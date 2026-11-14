package export

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestColdStoreOpenedBeforeReclamation(t *testing.T) {
	ns := newNamespace("co/file", "pressure/entry")
	nt := newNodeTable(ns.list)
	nt.coldParent = filepath.Join(t.TempDir(), "nodes.log.cache")
	old := filepath.Join(nt.coldParent, "instance-abandoned", "2")
	if err := os.MkdirAll(filepath.Dir(old), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("invalid owners"), 0600); err != nil {
		t.Fatal(err)
	}
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var release, barrier sync.Once
	nt.reclaimIO = func() { barrier.Do(func() { close(entered); <-resume; close(finished) }) }
	t.Cleanup(func() { release.Do(func() { close(resume) }); nt.close(); <-finished })
	if _, err := nt.openColdStore(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reclamation did not start")
	}
	id := nt.id("co/file")
	forceCold(t, nt, ns, "co")
	if got := pathOf(t, nt, id); got != "co/file" || nt.cacheErr != nil {
		t.Fatalf("serving before reclamation: %s, %v", got, nt.cacheErr)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("abandoned owners still reachable", err)
	}
	done := make(chan error, 1)
	go func() { done <- nt.close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close waited for reclamation")
	}
	// A stopped worker may return from I/O after a new table starts. It must
	// neither hold the node mutex nor reach the new table's authoritative files.
	nt.mu.Lock()
	nt.mu.Unlock()
	next := newNodeTable(ns.list)
	next.coldParent = nt.coldParent
	if _, err := next.openColdStore(nil); err != nil {
		t.Fatal(err)
	}
	defer next.close()
	next.id("co/file")
	forceCold(t, next, ns, "co")
	file := next.pack.path
	release.Do(func() { close(resume) })
	<-finished
	if err := reclaimCold(nt.coldParent+".discard", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("reclamation touched live index", err)
	}
}

func TestColdReclamationResumed(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "store-interrupted", "index", "instance-old", "00", "256")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	close(stop)
	if err := reclaimCold(root, stop, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("canceled cleanup deleted file", err)
	}
	if err := reclaimCold(root, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("resumed cleanup left files", entries, err)
	}
}

// TestColdIndexIsOnePack keeps the owners of many evicted directories in
// one file of the instance, rather than a file per directory.
func TestColdIndexIsOnePack(t *testing.T) {
	ns := newNamespace("pressure/entry")
	nt := newNodeTable(ns.list)
	defer nt.close()
	var ids []uint64
	for i := range 512 {
		p := fmt.Sprintf("co/d%d/file", i)
		ns.add(p)
		ids = append(ids, nt.id(p))
	}
	nt.cacheLimit = 0
	nt.id("pressure/entry")
	entries, err := os.ReadDir(nt.coldRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(nt.pack.path) {
		t.Fatalf("instance holds %v, %v", entries, err)
	}
	for i, id := range ids {
		if got := pathOf(t, nt, id); got != fmt.Sprintf("co/d%d/file", i) {
			t.Fatal(got)
		}
	}
}

// Batch deletion must restart its stream after directory offsets reshuffle.
func TestColdReclamationMultipleBatches(t *testing.T) {
	root := t.TempDir()
	for i := range 513 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	visits := 0
	if err := reclaimCold(root, stop, func() {
		visits++
		if visits == 140 {
			close(stop)
		}
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 513-139 {
		t.Fatalf("interrupted batch: %d entries, %v", len(entries), err)
	}
	if err := reclaimCold(root, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("completed batches: %d entries, %v", len(entries), err)
	}
}
