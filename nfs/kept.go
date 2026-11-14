package nfs

import (
	"container/list"
	"io"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Kept-file limits.
const (
	// keptIdle is how long a kept file outlives its last use. NFSv3 has no
	// close, but clients send a file's cached writes when it is closed, so
	// the writes of one open come in a burst.
	keptIdle = 10 * time.Second
	// keptMax bounds the files kept at once; past it the least recently
	// used idle one is closed.
	keptMax = 4096
)

// keptKey separates cached writers, keyed by id and current path, from
// permission fallbacks, whose empty path lets them follow renames like an
// already-open file. Cached writers reopen after a rename so a backend
// that reopens descriptors across snapshots remembers the current path.
type keptKey struct {
	id   uint64
	path string
}

// keptFiles retains NFSv3 writers across bursts of WRITE requests instead
// of opening and closing a host descriptor for each small write. Files
// opened before CREATE or SETATTR removes write permission also stay here:
// clients may send their buffered writes afterwards. NFSv4 keeps those
// permission fallbacks per open instead (keepWritable).
type keptFiles struct {
	mu    sync.Mutex
	files map[keptKey]*list.Element // of *keptFile
	lru   list.List                 // most recently used first
	idle  time.Duration             // zero: keptIdle
}

// keptFile is one kept file. It closes once idle with no write under way.
type keptFile struct {
	mu    sync.Mutex // serializes seek/write for files without WriteAt
	key   keptKey
	f     billy.File
	users int
	timer *time.Timer
}

// has reports whether key has a kept file.
func (k *keptFiles) has(key keptKey) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.files[key]
	return ok
}

// keep takes ownership of f, closing it if key is already cached or every
// slot is busy. Any descriptor closed here reports its error to the caller.
func (k *keptFiles) keep(key keptKey, f billy.File) error {
	k.mu.Lock()
	if _, ok := k.files[key]; ok {
		k.mu.Unlock()
		return f.Close()
	}
	if k.files == nil {
		k.files = make(map[keptKey]*list.Element)
	}
	var evicted billy.File
	if len(k.files) >= keptMax {
		evicted = k.evictLocked()
		if evicted == nil {
			k.mu.Unlock()
			return f.Close()
		}
	}
	e := &keptFile{key: key, f: f}
	k.files[key] = k.lru.PushFront(e)
	e.timer = time.AfterFunc(k.idleTime(), func() { k.expire(e) })
	k.mu.Unlock()
	if evicted != nil {
		return evicted.Close()
	}
	return nil
}

// with serializes fn on key's kept file; ok is false when there is none.
func (k *keptFiles) with(key keptKey, fn func(billy.File) error) (ok bool, err error) {
	k.mu.Lock()
	el := k.files[key]
	if el == nil {
		k.mu.Unlock()
		return false, nil
	}
	e := el.Value.(*keptFile)
	e.users++
	k.lru.MoveToFront(el)
	k.mu.Unlock()

	e.mu.Lock()
	err = fn(e.f)
	e.mu.Unlock()

	k.mu.Lock()
	e.users--
	e.timer.Reset(k.idleTime())
	k.mu.Unlock()
	return true, err
}

// expire closes e once it is idle.
func (k *keptFiles) expire(e *keptFile) {
	k.mu.Lock()
	el := k.files[e.key]
	if el == nil || el.Value != e {
		k.mu.Unlock()
		return
	}
	if e.users > 0 {
		e.timer.Reset(k.idleTime())
		k.mu.Unlock()
		return
	}
	k.lru.Remove(el)
	delete(k.files, e.key)
	k.mu.Unlock()
	e.f.Close()
}

// evictLocked drops the least recently used idle file and returns it for
// the caller to close outside the lock; nil when every file is in use.
func (k *keptFiles) evictLocked() billy.File {
	for el := k.lru.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*keptFile); e.users == 0 {
			e.timer.Stop()
			k.lru.Remove(el)
			delete(k.files, e.key)
			return e.f
		}
	}
	return nil
}

// idleTime is the configured idle lifetime.
func (k *keptFiles) idleTime() time.Duration {
	if k.idle > 0 {
		return k.idle
	}
	return keptIdle
}

// writeAt writes data at offset of f, without moving an offset others
// share where the file can.
func writeAt(f billy.File, data []byte, offset int64) (int, error) {
	if w, ok := f.(io.WriterAt); ok {
		return w.WriteAt(data, offset)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return f.Write(data)
}
