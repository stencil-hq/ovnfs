package nfs

import (
	"bytes"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/overlay"
)

// TestKeptEvictionAfterOwnerClose covers cached writers whose checkout was
// removed before cache pressure evicts them. keep reports eviction close errors
// to the new WRITE, so a second-close error would fail unrelated compiler output.
func TestKeptEvictionAfterOwnerClose(t *testing.T) {
	for _, storage := range []string{"disk", "spilled"} {
		t.Run(storage, func(t *testing.T) {
			var memory *overlay.Memory
			if storage == "spilled" {
				memory = overlay.NewMemory(1<<20, 1<<20, 0)
			}
			fs, err := overlay.New(t.TempDir(), nil, nil, memory)
			if err != nil {
				t.Fatal(err)
			}
			file, err := fs.OpenFile("output", os.O_CREATE|os.O_WRONLY, 0o755)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write([]byte("compiler output")); err != nil {
				t.Fatal(err)
			}
			if err := fs.Flush(); err != nil {
				t.Fatal(err)
			}
			cache := keptFiles{idle: time.Hour}
			if err := cache.keep(keptKey{id: 1, path: "removed/output"}, file); err != nil {
				t.Fatal(err)
			}
			fs.CloseFiles()
			cache.mu.Lock()
			evicted := cache.evictLocked()
			cache.mu.Unlock()
			if evicted == nil {
				t.Fatal("idle writer was not evicted")
			}
			if err := evicted.Close(); err != nil {
				t.Fatalf("evicting a retired writer would fail the next WRITE: %v", err)
			}
		})
	}
}

// seekWriteFS exposes only billy's sequential file API to the NFS writer.
type seekWriteFS struct{ *testFS }

// OpenFile hides WriteAt so cached writes must serialize Seek with Write.
func (f *seekWriteFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	return seekWriteFile{File: file}, nil
}

type seekWriteFile struct{ billy.File }

// Seek yields between positioning and writing to expose shared-offset races.
func (f seekWriteFile) Seek(off int64, whence int) (int64, error) {
	pos, err := f.File.Seek(off, whence)
	runtime.Gosched()
	return pos, err
}

func TestV3CachedWritesPreserveOffsets(t *testing.T) {
	fs := &seekWriteFS{newTestFS(t)}
	fs.write(t, "file", "")
	s := &Server{FS: fs}
	t.Cleanup(func() { closeKept(s) })
	id := fs.FileID("file")
	const workers, block = 16, 64
	want := make([]byte, workers*block)
	if _, err := s.writeAt3(id, "file", want, 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range workers {
		data := bytes.Repeat([]byte{byte(i + 1)}, block)
		copy(want[i*block:], data)
		wg.Go(func() {
			for range 32 {
				n, err := s.writeAt3(id, "file", data, int64(i*block))
				if err != nil || n != len(data) {
					t.Errorf("write at %d: %d bytes, %v", i*block, n, err)
					return
				}
			}
		})
	}
	wg.Wait()
	got, err := os.ReadFile(fs.host("file"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("concurrent writes corrupted file offsets: got %x, want %x", got, want)
	}
}
