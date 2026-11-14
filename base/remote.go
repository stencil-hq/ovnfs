package base

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// Remote bases serve the attached snapshot from metadata alone: listing and
// stat never move content. The first open of a file copies its blob into the
// store, where it stays cached and verified by id: from the mirror of the
// source's git origins when that holds it, else from the source machine. An
// open that crosses the link also prefetches the directory's other small files
// the mirror lacks, in one batch.

// fetchTimeout bounds how long a read waits for the source machine.
const fetchTimeout = 2 * time.Minute

// Prefetch limits for the batch an open sends for its siblings.
const (
	prefetchFileLimit  = 256 << 10
	prefetchBatchFiles = 500
	prefetchBatchBytes = 16 << 20
)

// remoteState is the lazily fetched side of a remote base. Cache roots, which
// the source machine never sends, are present as empty directories.
type remoteState struct {
	b          *Base
	mu         sync.Mutex
	prefetched map[string]bool // directories whose siblings were already requested
}

// CreateRemote records a snapshot attached from another machine. res carries
// the offered leaves and their sizes and mtimes; its tree must already be in the store.
func CreateRemote(c Config, res source.Result, r Remote) (*Base, error) {
	if c.Store == nil {
		return nil, fmt.Errorf("base: missing store")
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, err
	}
	m := Meta{ID: c.ID, ForkTime: time.Now().UTC(), Tree: res.Tree, Remote: &r}
	return create(c, m, &res)
}

// ensure fetches one blob unless the store already holds it.
func (r *remoteState) ensure(p string, id store.OID) error {
	if r.b.config.Store.Has(id) || r.fromMirror(p, id) {
		return nil
	}
	if r.b.config.Fetch == nil {
		return &fs.PathError{Op: "fetch", Path: p, Err: syscall.EIO}
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	info, _ := r.b.Info(p)
	if err := r.b.config.Fetch(ctx, []Want{{ID: id, Path: p, Size: info.Size}}); err != nil {
		if r.b.config.OnFault != nil {
			r.b.config.OnFault(fmt.Sprintf("fetch %s from %s: %v", p, r.b.meta.Remote.Source, err))
		}
		return &fs.PathError{Op: "fetch", Path: p, Err: syscall.EIO}
	}
	return nil
}

// fromMirror copies id's blob from the mirror into the store; false when the
// mirror lacks it or returned bytes that do not hash to id.
func (r *remoteState) fromMirror(p string, id store.OID) bool {
	if !r.b.mirrored(id) {
		return false
	}
	data, err := r.b.config.Mirror.Blob(id)
	if err == nil {
		var got store.OID
		if got, err = r.b.config.Store.PutBytes(data); err == nil && got != id {
			err = fmt.Errorf("content hashes to %s", got)
		}
	}
	if err != nil {
		if r.b.config.OnFault != nil {
			r.b.config.OnFault(fmt.Sprintf("read %s from the git mirror: %v; fetching it from %s", p, err, r.b.meta.Remote.Source))
		}
		return false
	}
	return true
}

// mirrored reports whether the mirror of this base's git origins holds id.
func (b *Base) mirrored(id store.OID) bool { return b.config.Mirror != nil && b.config.Mirror.Has(id) }

// prefetch requests, in one batch, the small uncached files of the directory
// holding p and then of its subdirectories, breadth first, within a budget.
// Every directory the batch covered completely is not prefetched again: a
// recursive reader (grep, a build) then pays one round trip per subtree, not
// one per directory.
func (r *remoteState) prefetch(p string) {
	dir := path.Dir(p)
	if dir == "." {
		dir = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prefetched[dir] || r.b.config.Fetch == nil {
		return
	}
	var wants []Want
	var total int64
	full := false
	x := r.b.index
	for queue := []string{dir}; len(queue) > 0 && !full; queue = queue[1:] {
		d := queue[0]
		if r.prefetched[d] {
			continue
		}
		var subdirs []string
		dn, _ := x.lookup(d)
		first, end := x.children(dn)
		for n := first; n < end; n++ {
			kind := x.kind(n)
			if kind != kindDir && kind != kindFile {
				continue
			}
			q := x.nameOf(n)
			if d != "" {
				q = d + "/" + q
			}
			if kind == kindDir {
				subdirs = append(subdirs, q)
				continue
			}
			info, _ := x.info(n)
			if q == p || info.Size > prefetchFileLimit {
				continue
			}
			id := x.oid(n)
			if r.b.config.Store.Has(id) || r.b.mirrored(id) {
				continue
			}
			if len(wants) == prefetchBatchFiles || total+info.Size > prefetchBatchBytes {
				full = true
				break
			}
			wants = append(wants, Want{ID: id, Path: q, Size: info.Size})
			total += info.Size
		}
		if !full {
			r.prefetched[d] = true
			queue = append(queue, subdirs...)
		}
	}
	if len(wants) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		_ = r.b.config.Fetch(ctx, wants)
	}()
}

// Uncached returns the blob a remote base would fetch over the link to read the
// regular file p; false for local bases, other entries, and content already
// stored or held by the git mirror.
func (b *Base) Uncached(p string) (Want, bool) {
	if b.remote == nil {
		return Want{}, false
	}
	n, ok := b.index.lookup(overlay.Clean(p))
	if !ok || b.index.kind(n) != kindFile {
		return Want{}, false
	}
	return b.want(overlay.Clean(p), n)
}

// Missing lists, in no particular order, the blobs a remote base would still
// fetch over the link: those of its regular files and symlinks that neither the
// store nor the git mirror holds. Once they are fetched, the base reads without
// the link. Local bases miss nothing.
func (b *Base) Missing() []Want {
	if b.remote == nil {
		return nil
	}
	var wants []Want
	b.index.walk(func(p string, n uint32) bool {
		if kind := b.index.kind(n); kind == kindFile || kind == kindSymlink {
			if w, ok := b.want(p, n); ok {
				wants = append(wants, w)
			}
		}
		return true
	})
	return wants
}

// want is the fetch the blob of p, index node n, needs, unless the store or
// the git mirror already holds it.
func (b *Base) want(p string, n uint32) (Want, bool) {
	id := b.index.oid(n)
	if b.config.Store.Has(id) || b.mirrored(id) {
		return Want{}, false
	}
	info, _ := b.index.info(n)
	return Want{ID: id, Path: p, Size: info.Size}, true
}

// FetchMatching fetches, in one batch, every uncached regular file of a
// remote base that keep selects, so readers that need a known set of files
// pay one round trip instead of one per file. Local bases fetch nothing.
func (b *Base) FetchMatching(ctx context.Context, keep func(p string) bool) error {
	if b.remote == nil || b.config.Fetch == nil {
		return nil
	}
	var wants []Want
	b.index.walk(func(p string, n uint32) bool {
		if b.index.kind(n) == kindFile && keep(p) {
			if w, ok := b.want(p, n); ok {
				wants = append(wants, w)
			}
		}
		return true
	})
	if len(wants) == 0 {
		return nil
	}
	return b.config.Fetch(ctx, wants)
}

// remoteBottom serves a remote base to the overlay.
type remoteBottom struct{ b *Base }

// info describes p from the offer, without fetching anything.
func (v remoteBottom) info(p string) (os.FileInfo, error) {
	p = overlay.Clean(p)
	n, ok := v.b.index.lookup(p)
	if !ok {
		return nil, absent(p)
	}
	return v.nodeInfo(p, n)
}

// nodeInfo describes p, index node n: an offered path, or a cache root
// (one that is no file) as an empty directory.
func (v remoteBottom) nodeInfo(p string, n uint32) (os.FileInfo, error) {
	x := v.b.index
	meta, hasInfo := x.info(n)
	if x.kind(n) != kindNone {
		mtime := meta.Mtime
		if mtime.IsZero() {
			mtime = v.b.meta.ForkTime
		}
		return capturedInfo{name: path.Base("/" + p), mode: x.fileMode(n), size: meta.Size, mtime: mtime}, nil
	}
	if x.class(n) == rules.Cache && !hasInfo {
		return capturedInfo{name: path.Base(p), mode: os.ModeDir | 0o755, mtime: v.b.meta.ForkTime}, nil
	}
	return nil, absent(p)
}

// Lstat returns the offered metadata; it never fetches content.
func (v remoteBottom) Lstat(p string) (os.FileInfo, error) { return v.info(p) }

// ReadDir lists offered children (and cache roots as empty directories).
func (v remoteBottom) ReadDir(p string) ([]os.FileInfo, error) {
	p = overlay.Clean(p)
	info, err := v.info(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	x := v.b.index
	dir, _ := x.lookup(p)
	first, end := x.children(dir)
	out := make([]os.FileInfo, 0, end-first)
	for n := first; n < end; n++ {
		if child, err := v.nodeInfo(path.Join(p, x.nameOf(n)), n); err == nil {
			out = append(out, child)
		}
	}
	return out, nil
}

// Open fetches a regular file's blob on first use and serves the stored copy,
// from memory when the store caches it.
func (v remoteBottom) Open(p string) (overlay.Content, error) {
	p = overlay.Clean(p)
	n, ok := v.b.index.lookup(p)
	if !ok || v.b.index.kind(n) == kindNone {
		return nil, absent(p)
	}
	if v.b.index.kind(n) != kindFile {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EISDIR}
	}
	id := v.b.index.oid(n)
	if !v.b.config.Store.Has(id) {
		// Siblings ride along on a link miss's round trip; a hit, or a blob the
		// mirror holds, has none to share.
		if !v.b.mirrored(id) {
			v.b.remote.prefetch(p)
		}
		if err := v.b.remote.ensure(p, id); err != nil {
			return nil, err
		}
	}
	if data, ok := v.b.config.Store.Cached(id); ok {
		return overlay.ByteContent(data), nil
	}
	return v.b.config.Store.Open(id)
}

// Readlink returns a symlink's target, fetching it if needed.
func (v remoteBottom) Readlink(p string) (string, error) {
	p = overlay.Clean(p)
	n, ok := v.b.index.lookup(p)
	if !ok || v.b.index.kind(n) != kindSymlink {
		return "", &fs.PathError{Op: "readlink", Path: p, Err: syscall.EINVAL}
	}
	data, err := v.b.Blob(p, v.b.index.oid(n))
	return string(data), err
}

// CopyTo materializes p (a whole directory tree, for renames) at the new path dst.
func (v remoteBottom) CopyTo(p, dst string) error {
	p = overlay.Clean(p)
	info, err := v.info(p)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		x := v.b.index
		dir, _ := x.lookup(p)
		first, end := x.children(dir)
		for n := first; n < end; n++ {
			name := x.nameOf(n)
			if err := v.CopyTo(path.Join(p, name), filepath.Join(dst, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
	case info.Mode()&os.ModeSymlink != 0:
		target, err := v.Readlink(p)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	default:
		n, _ := v.b.index.lookup(p)
		id := v.b.index.oid(n)
		if err := v.b.remote.ensure(p, id); err != nil {
			return err
		}
		if err := v.b.config.Store.CloneBlob(id, dst, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return setTimes(dst, info.ModTime(), info.Mode()&os.ModeSymlink != 0)
}

// capturedInfo is captured or offered metadata presented as a FileInfo owned by this process's user.
type capturedInfo struct {
	name  string
	mode  os.FileMode
	size  int64
	mtime time.Time
}

// Name returns the base name.
func (i capturedInfo) Name() string { return i.name }

// Size returns the offered size (0 for directories).
func (i capturedInfo) Size() int64 { return i.size }

// Mode returns the offered type and permissions.
func (i capturedInfo) Mode() os.FileMode { return i.mode }

// ModTime returns the offered modification time.
func (i capturedInfo) ModTime() time.Time { return i.mtime }

// IsDir reports a directory.
func (i capturedInfo) IsDir() bool { return i.mode.IsDir() }

// Sys supplies host-style stat data so frontends report this user as owner.
func (i capturedInfo) Sys() any { return syntheticSys(i.mode, i.size, i.mtime) }
