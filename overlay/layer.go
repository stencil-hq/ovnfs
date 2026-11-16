package overlay

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Bottom is the read-only filesystem beneath an overlay's immutable layers.
type Bottom interface {
	Lstat(p string) (os.FileInfo, error)
	// ReadDir returns a caller-owned slice sorted by name. It rejects missing
	// paths and non-directories, including final symlinks.
	ReadDir(p string) ([]os.FileInfo, error)
	Open(p string) (Content, error)
	Readlink(p string) (string, error)
	CopyTo(p, dst string) error
}

// Kind identifies a frozen layer entry, including deletion markers.
type Kind uint8

// Layer entry kinds distinguish data from deletion markers.
const (
	KindFile Kind = iota + 1
	KindDir
	KindSymlink
	KindWhiteout
)

// Entry describes one indexed path; whiteouts have no file information.
type Entry struct {
	Kind   Kind
	Opaque bool
	Info   os.FileInfo
	Origin string // path before this upper's renames; a delta-base hint, not file identity
}

// Layer is a frozen directory indexed once and safe for concurrent readers.
// Its files start raw in the directory; [Layer.Store] moves those snapshot
// hashing kept in the store out of it, and both backings serve the same view.
type Layer struct {
	dir      string
	entries  map[string]Entry
	children map[string]map[string]Entry
	gits     map[string]bool   // paths of entries named .git, see [Layer.GitEntries]
	storing  sync.Mutex        // serializes [Layer.Store]
	mu       sync.RWMutex      // guards blobs, ids and tree; held shared while raw files are read, see [Layer.Content]
	blobs    Blobs             // nil while every entry is raw
	ids      map[string]string // path → blob id of a stored file's content or symlink's target
	tree     string            // store tree reaching every stored blob; "" when none
	metadata []byte            // immutable compressed metadata once stored
}

// IndexLayer indexes a frozen upper after its staging work directory is removed.
func IndexLayer(dir string) (*Layer, error) { return indexLayer(dir, false) }

// IndexUpper indexes a live upper for a read-only pass (diff, plan); the result is a
// point-in-time view that later writes to the upper do not update.
func IndexUpper(dir string) (*Layer, error) { return indexLayer(dir, true) }

// indexLayer also supports a live upper whose staging directory is private.
func indexLayer(dir string, live bool) (*Layer, error) {
	return walkLayer(dir, live, true, "")
}

// walkLayer can visit only a listing's ancestors and direct children.
func walkLayer(dir string, live, recursive bool, listing string) (*Layer, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	l := &Layer{dir: root, entries: make(map[string]Entry), children: make(map[string]map[string]Entry)}
	err = filepath.WalkDir(root, func(real string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, real)
		if err != nil {
			return err
		}
		p := Clean(rel)
		// A shallow listing needs its ancestors, their markers and its own children.
		if !recursive && p != "" && parent(p) != listing && p != listing && !strings.HasPrefix(listing, p+"/") &&
			!strings.HasPrefix(path.Base(p), whPrefix) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if p == "" {
			if !info.IsDir() {
				return &fs.PathError{Op: "index", Path: root, Err: fs.ErrInvalid}
			}
			if info, err = withMode(real, info); err != nil {
				return err
			}
			l.set(p, entry(info))
			return nil
		}
		n := info.Name()
		switch {
		case n == workName:
			if !live || parent(p) != "" {
				return fmt.Errorf("overlay: staging directory remains in layer: %s", real)
			}
			if info.IsDir() {
				return filepath.SkipDir
			}
			return fmt.Errorf("overlay: staging path is not a directory: %s", real)
		case n == opaqueName:
			e := l.entries[parent(p)]
			e.Opaque = true
			l.set(parent(p), e)
		case strings.HasPrefix(n, metaPrefix):
		case strings.HasPrefix(n, whPrefix):
			q := path.Join(parent(p), strings.TrimPrefix(n, whPrefix))
			if _, ok := l.entries[q]; !ok {
				l.set(q, Entry{Kind: KindWhiteout})
			}
		default:
			if info, err = withMode(real, info); err != nil {
				return err
			}
			l.set(p, entry(info))
			if info.IsDir() && !recursive && p != listing && !strings.HasPrefix(listing, p+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err == nil {
		err = l.loadOrigins()
	}
	return l, err
}

// Dir returns the host path of the immutable layer: the directory of its raw
// files and, once stored, its manifest; or the manifest file alone once
// nothing raw remains. Garbage collection removes it with the layer.
func (l *Layer) Dir() string { return l.dir }

// Get looks up a canonical overlay path without touching the filesystem.
func (l *Layer) Get(p string) (Entry, bool) { e, ok := l.entries[Clean(p)]; return e, ok }

// Children returns an independent map of direct entries, including whiteouts.
func (l *Layer) Children(p string) map[string]Entry { return maps.Clone(l.children[Clean(p)]) }

// GitEntries lists the paths of the layer's entries named .git, in no order. The
// index keeps them as it changes, so the git adapter finds a checkout's gitfiles
// without walking every entry.
func (l *Layer) GitEntries() []string { return slices.Collect(maps.Keys(l.gits)) }

// Extract moves entry p, with everything below it, out of a layer that
// [FS.Freeze] just returned and nothing reads yet, to the new host path dst.
// The directory that held p keeps its indexed modification time.
func (l *Layer) Extract(p, dst string) error {
	p = Clean(p)
	if e, ok := l.entries[p]; p == "" || !ok || e.Kind == KindWhiteout {
		return &fs.PathError{Op: "extract", Path: p, Err: fs.ErrNotExist}
	}
	if err := os.Rename(filepath.Join(l.dir, filepath.FromSlash(p)), dst); err != nil {
		return err
	}
	l.drop(p)
	dir := l.entries[parent(p)].Info
	return os.Chtimes(filepath.Join(l.dir, filepath.FromSlash(parent(p))), dir.ModTime(), dir.ModTime())
}

// Walk visits entries in deterministic pre-order, including the root directory.
func (l *Layer) Walk(fn func(p string, e Entry) error) error {
	var visit func(string) error
	visit = func(p string) error {
		if err := fn(p, l.entries[p]); err != nil {
			return err
		}
		for _, n := range slices.Sorted(maps.Keys(l.children[p])) {
			if err := visit(path.Join(p, n)); err != nil {
				return err
			}
		}
		return nil
	}
	return visit("")
}

// entry translates host metadata into an indexed data entry.
func entry(info os.FileInfo) Entry {
	kind := KindFile
	if info.IsDir() {
		kind = KindDir
	} else if info.Mode()&os.ModeSymlink != 0 {
		kind = KindSymlink
	}
	return Entry{Kind: kind, Info: info}
}

// set updates a mutable upper's path and direct-child indexes together.
func (l *Layer) set(p string, e Entry) {
	if (e.Kind == KindFile || e.Kind == KindSymlink) && e.Origin == "" {
		e.Origin = l.entries[p].Origin
	}
	l.entries[p] = e
	if path.Base(p) == ".git" {
		if l.gits == nil {
			l.gits = make(map[string]bool)
		}
		l.gits[p] = true
	}
	if p != "" {
		d := parent(p)
		if l.children[d] == nil {
			l.children[d] = make(map[string]Entry)
		}
		l.children[d][path.Base(p)] = e
	}
}

// drop removes a mutable upper subtree from both indexes.
func (l *Layer) drop(p string) {
	for n := range l.children[p] {
		l.drop(path.Join(p, n))
	}
	delete(l.children, p)
	delete(l.entries, p)
	delete(l.gits, p)
	if p != "" {
		delete(l.children[parent(p)], path.Base(p))
	}
}

// barrier reports whether this layer prevents lookup in all deeper layers.
func (l *Layer) barrier(p string) bool {
	for q := parent(p); ; q = parent(q) {
		if e, ok := l.entries[q]; ok && (e.Kind != KindDir || e.Opaque) {
			return true
		}
		if q == "" {
			return false
		}
	}
}
