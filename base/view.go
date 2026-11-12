package base

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// bottom serves a local base to the overlay without copying anything. A source
// file is the folder's file while its size, mtime and inode still match the
// capture, and the captured blob once they do not; a source directory lists
// the captured names (plus live roots). Cache and local roots are the folder as
// it is now. A checkout's writes copy up into its own upper.
type bottom struct{ b *Base }

// Lstat returns current metadata for unchanged and live paths, captured metadata otherwise.
func (v bottom) Lstat(p string) (os.FileInfo, error) {
	_, info, err := v.b.resolve(overlay.Clean(p))
	return info, err
}

// ReadDir lists a directory: captured source names, live roots and, for live
// directories, whatever the folder holds now.
func (v bottom) ReadDir(p string) ([]os.FileInfo, error) {
	b := v.b
	p = overlay.Clean(p)
	_, info, err := b.resolve(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
	}
	if b.Live(p) {
		return b.liveDir(p)
	}
	// Both lists are sorted. Native metadata serves every present child;
	// captured names missing from the host still retain their fork-time info.
	children := b.children[p]
	lower, listErr := fsys.ReadDir(b.lower(p))
	out := make([]os.FileInfo, 0, len(children))
	for i, j := 0, 0; i < len(children) || j < len(lower); {
		var child string
		var entry os.DirEntry
		switch {
		case j == len(lower) || i < len(children) && path.Base(children[i]) < lower[j].Name():
			child = children[i]
			i++
		case i == len(children) || lower[j].Name() < path.Base(children[i]):
			entry = lower[j]
			child = path.Join(p, entry.Name())
			j++
		default:
			entry = lower[j]
			child = children[i]
			i++
			j++
		}
		if child == ".git" || source.IsJJPath(child) {
			continue
		}
		e, captured := b.sources[child]
		if !captured && b.classes[child] != rules.Cache && b.classes[child] != rules.Local {
			continue
		}
		var current os.FileInfo
		var err error
		if entry != nil {
			current, err = entry.Info()
			if err == nil {
				current = hostfile.Linked(current, b.meta.Dir, child)
			}
		}
		if listErr != nil || err != nil {
			_, current, err = b.resolve(child)
		} else if captured {
			current, _ = b.sourceInfo(child, e, current)
		} else if !b.lowerAncestorsAreDirs(child) {
			continue
		}
		if err != nil {
			if os.IsNotExist(err) || isNotDir(err) {
				continue
			}
			return nil, err
		}
		if current != nil {
			out = append(out, current)
		}
	}
	return out, nil
}

// Open opens the folder's file while it still matches the capture, else serves
// the captured blob (from memory when the store caches it).
func (v bottom) Open(p string) (overlay.Content, error) {
	b := v.b
	p = overlay.Clean(p)
	host, info, err := b.resolve(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EISDIR}
	}
	if host != "" {
		f, err := hostfile.Open(host)
		if err == nil {
			// Between lstat and open the folder may have changed; the open file must
			// be the inode that matched (or, for live paths, the one just listed).
			if opened, statErr := f.Stat(); statErr == nil && os.SameFile(info, opened) && opened.Size() == info.Size() && opened.ModTime().Equal(info.ModTime()) {
				return f, nil
			}
			f.Close()
		}
		if _, live := b.derivedRoot(p); live {
			return nil, absent(p)
		}
	}
	return b.blobFile(p, b.sources[p].oid)
}

// Readlink returns a symlink's target from the folder or the captured blob. A
// live link whose absolute target lies inside the folder becomes relative to
// the link, so in a checkout it names the checkout's copy instead of the
// folder: Bun on Windows, for one, links workspace packages by absolute path.
func (v bottom) Readlink(p string) (string, error) {
	b := v.b
	p = overlay.Clean(p)
	host, info, err := b.resolve(p)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", &fs.PathError{Op: "readlink", Path: p, Err: syscall.EINVAL}
	}
	if host != "" {
		target, err := os.Readlink(host)
		if err != nil {
			return "", err
		}
		if _, live := b.derivedRoot(p); live {
			target = b.relocate(host, target)
		}
		// Windows stores and reports targets with backslashes; views use slashes.
		return filepath.ToSlash(target), nil
	}
	data, err := b.Blob(p, b.sources[p].oid)
	return string(data), err
}

// relocate returns target relative to the link at host when it is an absolute
// path inside the folder, else unchanged.
func (b *Base) relocate(host, target string) string {
	if !filepath.IsAbs(target) {
		return target
	}
	if in, err := filepath.Rel(b.meta.Dir, target); err != nil || in == ".." || strings.HasPrefix(in, ".."+string(filepath.Separator)) {
		return target
	}
	if rel, err := filepath.Rel(filepath.Dir(host), target); err == nil {
		return rel
	}
	return target
}

// CopyTo materializes p (a whole directory tree, for renames) at the new path dst for copy-up.
func (v bottom) CopyTo(p, dst string) error {
	p = overlay.Clean(p)
	_, info, err := v.b.resolve(p)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := os.Mkdir(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		children, err := v.ReadDir(p)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := v.CopyTo(path.Join(p, child.Name()), filepath.Join(dst, child.Name())); err != nil && !os.IsNotExist(err) {
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
	case info.Mode().IsRegular():
		c, err := v.Open(p)
		if err != nil {
			return err
		}
		err = copyContent(c, dst, info.Mode().Perm())
		c.Close()
		if err != nil {
			return err
		}
	default:
		// Sockets, FIFOs and devices in live roots have no content to copy.
		return nil
	}
	return setTimes(dst, info.ModTime(), info.Mode()&os.ModeSymlink != 0)
}

// resolve finds what serves p: a host path (the folder's own entry) with its
// current metadata, or "" with captured metadata when the captured blob serves it.
func (b *Base) resolve(p string) (string, os.FileInfo, error) {
	if p == ".git" || strings.HasPrefix(p, ".git/") || source.IsJJPath(p) {
		return "", nil, absent(p)
	}
	if e, ok := b.sources[p]; ok {
		host := b.lower(p)
		current, _ := hostfile.Lstat(host)
		info, live := b.sourceInfo(p, e, current)
		if live {
			return host, info, nil
		}
		return "", info, nil
	}
	if root, live := b.derivedRoot(p); root != "" && live {
		return b.live(p)
	}
	return "", nil, absent(p)
}

// sourceInfo selects current or captured metadata after a stat or bulk listing.
func (b *Base) sourceInfo(p string, e entry, current os.FileInfo) (os.FileInfo, bool) {
	if e.mode.IsDir() {
		if current != nil && current.IsDir() {
			return current, true
		}
		name := path.Base(p)
		if p == "" {
			name = "/"
		}
		return capturedInfo{name: name, mode: e.mode, mtime: b.meta.ForkTime}, false
	}
	captured := b.capture.Info[p]
	if current != nil && b.unchanged(p, current, e, captured) {
		return current, true
	}
	return capturedInfo{name: path.Base(p), mode: e.mode, size: captured.Size, mtime: presentedMtime(captured, b.meta.ForkTime)}, false
}

// presentedMtime is the mtime of a source file served from its capture: the
// captured one, or the fork time for content the folder never held.
func presentedMtime(captured source.Info, fork time.Time) time.Time {
	if captured.Mtime.IsZero() {
		return fork
	}
	return captured.Mtime
}

// unchanged reports a folder entry that still is the captured file: same type,
// size, mtime and inode. The inode check keeps a swapped-in lookalike (an
// ancestor directory replaced by a copy) from being served as captured content.
func (b *Base) unchanged(p string, current os.FileInfo, e entry, captured source.Info) bool {
	if current.Mode().Type() != e.mode.Type() || current.Size() != captured.Size || !current.ModTime().Equal(captured.Mtime) {
		return false
	}
	if captured.Ino == 0 {
		return false
	}
	_, ino, err := fileIdentity(current, b.meta.Dir, p)
	return err == nil && ino == captured.Ino
}

// blobFile serves a source file's captured content: cached bytes, the stored
// object, or a repository object written into the store first.
func (b *Base) blobFile(p string, id store.OID) (overlay.Content, error) {
	st := b.config.Store
	if data, ok := st.Cached(id); ok {
		return overlay.ByteContent(data), nil
	}
	if !st.Has(id) {
		data, err := b.Blob(p, id)
		if err != nil {
			return nil, err
		}
		if _, err := st.PutBytes(data); err != nil {
			return nil, err
		}
		// PutBytes made a small blob resident; serve it without reopening.
		if data, ok := st.Cached(id); ok {
			return overlay.ByteContent(data), nil
		}
	}
	return st.Open(id)
}

// copyContent materializes c at the new path dst with mode: host files are
// cloned, in-memory content is written out.
func copyContent(c overlay.Content, dst string, mode os.FileMode) error {
	if f, ok := c.(*os.File); ok {
		return clone.FD(f, dst, mode)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.NewSectionReader(c, 0, math.MaxInt64))
	if err == nil {
		// The umask may have narrowed mode at creation.
		err = out.Chmod(mode)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// live serves a cache or local path straight from the folder, as it is now.
// Ancestors must be real directories, so a symlink inside the folder cannot
// lead the view outside it.
func (b *Base) live(p string) (string, os.FileInfo, error) {
	if !b.lowerAncestorsAreDirs(p) {
		return "", nil, absent(p)
	}
	host := b.lower(p)
	info, err := hostfile.Lstat(host)
	if err != nil {
		return "", nil, err
	}
	return host, info, nil
}

// Live reports whether p is served from the source folder as it is now (a
// cache or local path), so an edit made in the folder changes it without
// passing through ovnfs. Remote bases have no live paths.
func (b *Base) Live(p string) bool {
	if b.remote != nil {
		return false
	}
	_, live := b.derivedRoot(overlay.Clean(p))
	return live
}

// Affects reports whether an edit at folder path p can make the folder differ
// from the capture: false only below a cache, local or hidden root, which the
// capture never descended, and in the root's .jj, which captures leave out. A
// name the capture never saw counts, since it may be source. Remote bases have
// no folder to edit.
func (b *Base) Affects(p string) bool {
	p = overlay.Clean(p)
	if b.remote != nil || source.IsJJPath(p) {
		return false
	}
	for end := 0; end < len(p); {
		end = nextPrefixEnd(p, end)
		prefix := p[:end]
		if _, ok := b.sources[prefix]; ok {
			continue
		}
		_, known := b.classes[prefix]
		return !known
	}
	return true
}

// nextPrefixEnd returns the end of the root-relative clean path p's component
// that starts after offset from (the next slash or len(p)), so p[:end] is the
// next prefix without building it.
func nextPrefixEnd(p string, from int) int {
	if from > 0 {
		from++ // skip the slash ending the previous prefix
	}
	if i := strings.IndexByte(p[from:], '/'); i >= 0 {
		return from + i
	}
	return len(p)
}

// derivedRoot finds the first path prefix absent from the captured tree; live
// reports a cache or local root, which the view serves from the folder. Any
// other absent prefix (a hidden root, or a name that appeared after the
// capture) does not exist in the view.
func (b *Base) derivedRoot(p string) (root string, live bool) {
	if p == "" {
		return "", false
	}
	for end := 0; end < len(p); {
		end = nextPrefixEnd(p, end)
		prefix := p[:end]
		if _, ok := b.sources[prefix]; ok {
			continue
		}
		class := b.classes[prefix]
		return prefix, class == rules.Cache || class == rules.Local
	}
	return "", false
}

// liveDir lists live directory p from the folder's own listing, whose entries
// need no further proof: p's ancestors were just proven and p was lstat'ed.
func (b *Base) liveDir(p string) ([]os.FileInfo, error) {
	dir := b.lower(p)
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed during the listing
			}
			return nil, err
		}
		out = append(out, hostfile.Linked(info, dir, e.Name()))
	}
	return out, nil
}

// proofTTL is how long a folder directory stays proven real, as long as the
// one-second kernel attribute caches of the frontends: a directory swapped for
// a symlink is refused within that time.
const proofTTL = time.Second

// dirProofs remembers folder directories proven real together with all their
// ancestors. The lstat chain it saves dominated live lookups (on Windows over
// half of the daemon's time in a no-op cargo build).
type dirProofs struct {
	mu sync.Mutex
	at map[string]time.Time // proof time; a proof covers the directory's ancestors
}

// lowerAncestorsAreDirs reports whether every ancestor of p in the folder is a
// real directory, reusing proofs younger than [proofTTL].
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

// lower names an entry in the source folder.
func (b *Base) lower(p string) string { return filepath.Join(b.meta.Dir, filepath.FromSlash(p)) }

// isNotDir reports a lookup through a non-directory, which listings skip like a missing name.
func isNotDir(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && pe.Err == syscall.ENOTDIR
}

// absent returns the filesystem-standard missing-entry error.
func absent(p string) error { return &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist} }
