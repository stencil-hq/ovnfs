package base

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/overlay"
)

// A checkout reads the folder's cache roots (build output, dependency trees)
// where they are. Nothing of them is copied, indexed or deleted again,
// however many files they hold and whatever the filesystem: a root costs a
// checkout what the checkout touches of it. What a checkout sees is fenced at
// the instant its cache snapshot began (see [Caches]):
//
//   - a file or symlink is there while the folder has not changed it since:
//     its change time, which content, metadata and renames move and no tool
//     sets back, is before the fence;
//   - a directory is there unless the folder made it since, where the
//     filesystem records when: entries coming and going change a directory,
//     so its change time says nothing;
//   - an entry the folder rewrites, replaces or removes afterwards leaves
//     the checkout, as an evicted cache entry would. It never changes there,
//     and nothing the folder makes later appears.
//
// So a host build never hands a checkout output newer than the checkout's own
// sources, which build tools that trust mtimes, such as cargo, would take for
// their own and link (#58); what a host build takes away, the checkout's
// build makes again, in its upper. Nothing invalidates a checkout either:
// every lookup asks the folder.
//
// A folder file serves its entry as it is, so the entry's identity is the
// file's inode ([ident.State]). That space numbers the state volume's inodes,
// which is why the view exists only where the folder is on that volume;
// elsewhere cache roots start empty, as on another machine. The fence keeps a
// reused inode number from naming two entries: a file made after it is not
// shown. Two names of one file (cargo links an artifact into deps/ and
// beside it) share its identity, as hard links do; the overlay decides what
// a copy of one keeps (overlay.FS.inherit).

// fence is where a checkout's view of the folder's cache roots ends.
type fence struct {
	// at is the fence in Unix nanoseconds of the state volume's clock (see
	// [Caches.stamp]); 0 shows cache roots as empty directories.
	at int64
	// volume is the folder's volume, which is the state's: an entry on
	// another, such as a mount point below a cache root, is not shown.
	volume uint64
}

// shows reports whether the view shows the folder's entry at host, which
// info, its lstat data, describes.
func (f fence) shows(host string, info os.FileInfo) bool {
	if !onVolume(host, info, f.volume) {
		return false
	}
	if info.IsDir() {
		born, known := birthTime(host, info)
		return !known || born < f.at
	}
	changed, err := fsys.ChangeTime(host, info)
	return err == nil && changed < f.at
}

// proofTTL is how long a folder directory stays proven real, as long as the
// one-second kernel attribute caches of the frontends: a directory swapped for
// a symlink is refused within that time.
const proofTTL = time.Second

// dirProofs remembers folder directories proven real together with all their
// ancestors. The lstat chain it saves dominated lookups below cache roots (on
// Windows over half of the daemon's time in a no-op cargo build).
type dirProofs struct {
	mu sync.Mutex
	at map[string]time.Time // proof time; a proof covers the directory's ancestors
}

// lowerAncestorsAreDirs reports whether every ancestor of p in the folder is a
// real directory, reusing proofs younger than [proofTTL]. Concurrent changes
// can outlive a check; this is not a containment boundary.
func (b *Base) lowerAncestorsAreDirs(p string) bool {
	now := time.Now()
	ps := &b.proofs
	ps.mu.Lock()
	// A new proof is only as fresh as the ancestor proof it extends.
	stamp := now
	var unproven []string
	for dir := path.Dir(p); dir != "." && dir != ""; dir = path.Dir(dir) {
		if at, ok := ps.at[dir]; ok && now.Sub(at) < proofTTL {
			stamp = at
			break
		}
		unproven = append(unproven, dir)
	}
	ps.mu.Unlock()
	for _, dir := range unproven {
		info, err := os.Lstat(b.lower(dir))
		if err != nil || !info.IsDir() {
			return false
		}
	}
	if len(unproven) == 0 {
		return true
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.at == nil || len(ps.at) >= 1<<16 {
		ps.at = make(map[string]time.Time)
	}
	for _, dir := range unproven {
		ps.at[dir] = stamp
	}
	return true
}

// fenced returns what serves p, at or below cache root root (index node n):
// the folder's entry with its own metadata, where the view's fence shows it.
// The root carries its Capture identity; an entry below it, the State
// identity of its inode. Without a fence a root that is a directory is
// there, empty.
func (v bottom) fenced(p, root string, n uint32) (entry, error) {
	b, f := v.b, v.c.fence
	if f.at == 0 && p != root || !b.lowerAncestorsAreDirs(p) {
		return entry{}, absent(p)
	}
	host := b.lower(p)
	info, err := hostfile.Lstat(host)
	if err != nil {
		return entry{}, inView("lstat", p, err)
	}
	if f.at == 0 && !info.IsDir() || f.at != 0 && !f.shows(host, info) {
		return entry{}, absent(p)
	}
	e := entry{host: host, folder: info, n: n, fenced: true}
	if p == root {
		e.info = hostInfo{FileInfo: info, id: captureID(n)}
	} else {
		e.info = v.c.locator.lookupInfo(info, host, p)
		if !info.IsDir() {
			e.info = v.c.rememberFenced(p, e.info)
		}
	}
	return e, nil
}

// fencedDir lists directory p, at or below a cache root: the folder's
// entries the view's fence shows, sorted by name.
func (v bottom) fencedDir(p string) ([]os.FileInfo, error) {
	f := v.c.fence
	if f.at == 0 {
		return []os.FileInfo{}, nil
	}
	dir := v.b.lower(p)
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, inView("readdir", p, err)
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed during the listing
			}
			return nil, inView("readdir", p, err)
		}
		// Darwin's stat carries everything the fence and State identity
		// need. Linux needs a path only for a directory's statx birth time;
		// Windows needs it for junctions and deferred file identities.
		var host string
		if runtime.GOOS == "windows" || runtime.GOOS == "linux" && info.IsDir() {
			host = filepath.Join(dir, e.Name())
		}
		if info = hostfile.Linked(info, host); f.shows(host, info) {
			li := v.c.locator.listedInfo(info, host, p)
			if !info.IsDir() {
				li = v.c.rememberFenced(p+"/"+info.Name(), li)
			}
			out = append(out, li)
		}
	}
	return out, nil
}

// live serves p, at or below virtual local root root (index node n), straight
// from the folder without copying or fencing. The root keeps its Capture
// identity (valid on any volume); deeper entries use the folder's inode and
// show only where the folder is on the state's volume.
func (v bottom) live(p, root string, n uint32) (entry, error) {
	b, vol := v.b, v.c.fence.volume
	if p != root && vol == 0 || !b.lowerAncestorsAreDirs(p) {
		return entry{}, absent(p)
	}
	host := b.lower(p)
	info, err := hostfile.Lstat(host)
	if err != nil {
		return entry{}, inView("lstat", p, err)
	}
	if p != root && !onVolume(info, vol) {
		return entry{}, absent(p)
	}
	e := entry{host: host, n: n, live: true}
	if p == root {
		e.info = hostInfo{FileInfo: info, id: captureID(n)}
	} else {
		e.info = v.c.locator.lookupInfo(info, host, p)
	}
	return e, nil
}

// liveDir lists directory p, at or below a virtual local root, straight from
// the folder without fencing, skipping entries on other volumes.
func (v bottom) liveDir(p string) ([]os.FileInfo, error) {
	vol := v.c.fence.volume
	if vol == 0 {
		return []os.FileInfo{}, nil
	}
	dir := v.b.lower(p)
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, inView("readdir", p, err)
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed during the listing
			}
			return nil, inView("readdir", p, err)
		}
		var host string
		if runtime.GOOS == "windows" {
			host = filepath.Join(dir, e.Name())
		}
		if info = hostfile.Linked(info, host); onVolume(info, vol) {
			out = append(out, v.c.locator.listedInfo(info, host, p))
		}
	}
	return out, nil
}

// openFenced opens the regular folder file that e, from [bottom.fenced] of
// p, describes. The folder may have changed it since that lookup: the open
// file must be the one looked up and still shown.
func (v bottom) openFenced(p string, e entry) (overlay.Content, error) {
	f := v.c.fence
	file, err := hostfile.Open(e.host)
	if err != nil {
		return nil, inView("open", p, err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(e.folder, opened) || !f.shows(e.host, opened) {
		file.Close()
		return nil, absent(p)
	}
	return &fencedFile{file: file, path: p, before: f.at}, nil
}

// fencedFile is an open folder file below a cache root. Its entry leaves the
// view when the folder changes it. Once its inode crosses the fence a read
// fails with ESTALE, even if a writer restored the size and mtime.
type fencedFile struct {
	// file is no embedded field: none of its methods may reach the content
	// past the checks below, as its WriteTo would for an io.Copy.
	file   *os.File
	path   string // in the view, for errors
	before int64  // exclusive change-time fence
}

// Read reads at the file's offset; see [fencedFile.intact].
func (f *fencedFile) Read(p []byte) (int, error) {
	n, err := f.file.Read(p)
	if stale := f.intact(); stale != nil {
		return 0, stale
	}
	return n, err
}

// ReadAt reads at off; see [fencedFile.intact].
func (f *fencedFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.file.ReadAt(p, off)
	if stale := f.intact(); stale != nil {
		return 0, stale
	}
	return n, err
}

// Seek moves the offset of Read.
func (f *fencedFile) Seek(offset int64, whence int) (int64, error) {
	return f.file.Seek(offset, whence)
}

// Close releases the folder's file.
func (f *fencedFile) Close() error { return f.file.Close() }

// intact checks the open inode after reading or cloning, before publishing
// any bytes. An unlink that advances ctime invalidates the handle too.
func (f *fencedFile) intact() error {
	if changed, err := fsys.FileChangeTime(f.file); err != nil || changed >= f.before {
		return &fs.PathError{Op: "read", Path: f.path, Err: syscall.ESTALE}
	}
	return nil
}

// inView returns err, from an operation op on the folder's entry at view path
// p, naming p instead of the host path: a missing entry reads as [absent]
// does, anything else keeps its cause.
func inView(op, p string, err error) error {
	if os.IsNotExist(err) || isNotDir(err) {
		return absent(p)
	}
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		err = pe.Err
	}
	return &fs.PathError{Op: op, Path: p, Err: err}
}
