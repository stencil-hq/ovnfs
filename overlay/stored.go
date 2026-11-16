package overlay

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/durable"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// Blobs serves the content of stored layer entries by blob id; checkout adapts
// the snapshot store to it.
type Blobs interface {
	// Open returns blob id's bytes: a file's content or a symlink's target.
	Open(id string) (Content, error)
	// CopyTo writes blob id to the new regular file dst with permissions perm.
	CopyTo(id, dst string, perm os.FileMode) error
}

// manifestName holds a stored layer's manifest in its directory; the reserved
// prefix keeps it out of any index of the directory.
const manifestName = metaPrefix + "manifest"

// manifestVersion is the only manifest layout this package reads.
const manifestVersion = 1

// manifest describes a stored layer completely: every entry in [Layer.Walk]
// order with the metadata the host reported when the layer was frozen.
type manifest struct {
	Version int             `json:"v"`
	Tree    string          `json:"r,omitempty"` // store tree reaching every blob, the layer's GC root
	Entries []manifestEntry `json:"e"`
}

// manifestEntry restores one indexed path; whiteouts carry only Path and Kind.
type manifestEntry struct {
	Path   string `json:"p"`
	Name   string `json:"n,omitempty"` // the root's name; others are named by their last component
	Kind   Kind   `json:"k"`
	Opaque bool   `json:"o,omitempty"`
	Mode   uint32 `json:"m,omitempty"` // os.FileMode, type bits included
	Size   int64  `json:"s,omitempty"`
	Sec    int64  `json:"t,omitempty"` // modification time
	Nsec   int64  `json:"u,omitempty"`
	Sys    []byte `json:"x,omitempty"` // native stat record, see encodeSys
	Link   int    `json:"l,omitempty"` // 1 + index of the first entry naming the same host file
	Blob   string `json:"b,omitempty"` // stored content; absent: the raw file stays in the directory
	Origin string `json:"q,omitempty"` // rename's source path in the parent layer view
}

// storedInfo is metadata a manifest restores: exactly what the host reported
// for the entry when the layer was frozen.
type storedInfo struct {
	name  string
	size  int64
	mode  os.FileMode
	mtime time.Time
	sys   any
	link  uint64 // shared by the hard links of one host file; 0 when alone
}

// Name returns the base name.
func (i storedInfo) Name() string { return i.name }

// Size returns the recorded length.
func (i storedInfo) Size() int64 { return i.size }

// Mode returns the recorded mode, type bits included.
func (i storedInfo) Mode() os.FileMode { return i.mode }

// ModTime returns the recorded modification time.
func (i storedInfo) ModTime() time.Time { return i.mtime }

// IsDir reports whether the entry is a directory.
func (i storedInfo) IsDir() bool { return i.mode.IsDir() }

// Sys returns the recorded native stat record.
func (i storedInfo) Sys() any { return i.sys }

// storedLinks numbers the hard-link groups of stored entries in this process.
var storedLinks atomic.Uint64

// sameStored reports whether a and b are restored hard links of one host file,
// which [os.SameFile] cannot tell without the file.
func sameStored(a, b os.FileInfo) bool {
	x, ok := a.(storedInfo)
	y, ok2 := b.(storedInfo)
	return ok && ok2 && x.link != 0 && x.link == y.link
}

// Stored reports whether the layer is described by its manifest, some of its
// content served from blobs.
func (l *Layer) Stored() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.blobs != nil
}

// Tree returns the store tree that reaches every blob the layer serves, which
// garbage collection keeps while the layer exists; "" when none.
func (l *Layer) Tree() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.tree
}

// Blob returns the store id of the content of stored file or symlink p.
func (l *Layer) Blob(p string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	id, ok := l.ids[p]
	return id, ok
}

// Content runs fn with the backing of file or symlink p: blob is the store id
// of a stored entry's content (a symlink's target), else host is the raw file.
// [Layer.Store] cannot remove the raw file while fn runs, so a raw file fn
// opens stays readable after it returns.
func (l *Layer) Content(p string, fn func(host, blob string) error) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if id, ok := l.ids[p]; ok {
		return fn("", id)
	}
	return fn(filepath.Join(l.dir, filepath.FromSlash(p)), "")
}

// readBlob returns all of blob id; the caller holds l.mu shared.
func (l *Layer) readBlob(id string) ([]byte, error) {
	c, err := l.blobs.Open(id)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(c)
	return data, errors.Join(err, c.Close())
}

// open opens regular file p of a frozen layer read-only under the overlay name
// name. A stored layer's directories no longer exist on the host, so they fail
// like the bottom's.
func (l *Layer) open(p, name string, flag int) (billy.File, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if id, ok := l.ids[p]; ok {
		c, err := l.blobs.Open(id)
		if err != nil {
			return nil, err
		}
		return wrapContent(c, name), nil
	}
	if l.blobs != nil && l.entries[p].Kind == KindDir {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
	}
	h, err := open(filepath.Join(l.dir, filepath.FromSlash(p)), name, flag, 0, nil)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Readlink returns symlink p's target with slashes; diffs and the overlay read it.
func (l *Layer) Readlink(p string) (string, error) {
	var target string
	err := l.Content(p, func(host, blob string) error {
		if blob != "" {
			data, err := l.readBlob(blob)
			target = string(data)
			return err
		}
		// Windows stores and reports targets with backslashes; the overlay uses slashes.
		t, err := os.Readlink(host)
		target = filepath.ToSlash(t)
		return err
	})
	return target, err
}

// ReadFile returns regular file p's bytes; diffs and in-memory copy-up read it.
func (l *Layer) ReadFile(p string) ([]byte, error) {
	var data []byte
	err := l.Content(p, func(host, blob string) error {
		var err error
		if blob != "" {
			data, err = l.readBlob(blob)
		} else {
			data, err = os.ReadFile(host)
		}
		return err
	})
	return data, err
}

// copyTo materializes file or symlink p, described by info, at the new path
// dst with its permissions and modification (and, where kept, creation) time.
func (l *Layer) copyTo(p, dst string, info os.FileInfo) error {
	return l.Content(p, func(host, blob string) error {
		if blob == "" {
			return clone.Tree(host, dst)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := l.readBlob(blob)
			if err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(string(target)), dst); err != nil {
				return err
			}
			return hostfile.Lchtimes(dst, info.ModTime(), info.ModTime())
		}
		if err := l.blobs.CopyTo(blob, dst, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		if birth, ok := birthTime(info); ok {
			return setCreationTime(dst, birth)
		}
		return nil
	})
}

// Store makes the layer serve the files and symlinks named in ids (path → id
// of the blob blobs serves for its content or target) from blobs, and removes
// their raw files: it durably writes the manifest, holding every entry's
// metadata, before switching readers and removing anything. tree is the store
// tree that keeps those blobs alive ([Layer.Tree]). Entries not in ids stay raw.
// Removal is best effort: a file still open on Windows, or anything left by an
// error or crash, is removed when [OpenLayer] next loads the layer. Once
// nothing raw remains, the manifest replaces the directory at [Layer.Dir].
// Storing a stored layer does nothing; ids naming anything but a file or
// symlink fail before any change. The layer keeps ids.
func (l *Layer) Store(ids map[string]string, tree string, blobs Blobs) error {
	l.storing.Lock()
	defer l.storing.Unlock()
	if l.Stored() {
		return nil
	}
	for p, id := range ids {
		e, ok := l.entries[p]
		if !ok || id == "" || (e.Kind != KindSymlink && (e.Kind != KindFile || !e.Info.Mode().IsRegular())) {
			return &fs.PathError{Op: "store", Path: p, Err: fs.ErrInvalid}
		}
	}
	m, err := l.manifest(ids, tree)
	if err != nil {
		return err
	}
	data, err := encodeManifest(m)
	if err != nil {
		return err
	}
	if err := writeManifest(l.dir, data); err != nil {
		return err
	}
	l.mu.Lock()
	l.blobs, l.ids, l.tree, l.metadata = blobs, ids, tree, data
	l.mu.Unlock()
	l.prune()
	collapse(l.dir)
	return nil
}

// manifest describes every entry, linking hard links of one host file.
func (l *Layer) manifest(ids map[string]string, tree string) (manifest, error) {
	m := manifest{Version: manifestVersion, Tree: tree}
	first := make(map[inodeKey]int)
	err := l.Walk(func(p string, e Entry) error {
		me := manifestEntry{Path: p, Kind: e.Kind, Opaque: e.Opaque, Blob: ids[p], Origin: e.Origin}
		if e.Kind != KindWhiteout {
			if p == "" {
				me.Name = e.Info.Name()
			}
			me.Mode, me.Size = uint32(e.Info.Mode()), e.Info.Size()
			me.Sec, me.Nsec = e.Info.ModTime().Unix(), int64(e.Info.ModTime().Nanosecond())
			sys, err := encodeSys(e.Info)
			if err != nil {
				return fmt.Errorf("overlay: record %q: %w", p, err)
			}
			me.Sys = sys
		}
		if e.Kind == KindFile || e.Kind == KindSymlink {
			// Windows reads a raw entry's file id from its path on first use;
			// load it before Store removes the file, or Rename stops telling
			// this layer's hard links apart from other files.
			_ = primeInfo(e.Info)
			if key, err := identity(l.dir, filepath.FromSlash(p), e.Info); err == nil {
				if i, ok := first[key]; ok {
					me.Link = i + 1
					m.Entries[i].Link = i + 1
				} else {
					first[key] = len(m.Entries)
				}
			}
		}
		m.Entries = append(m.Entries, me)
		return nil
	})
	return m, err
}

// encodeManifest compresses repeating native stat records with the layer metadata.
func encodeManifest(m manifest) ([]byte, error) {
	var data bytes.Buffer
	w, err := flate.NewWriter(&data, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(w).Encode(m); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

// writeManifest atomically and durably publishes compressed metadata in dir.
func writeManifest(dir string, data []byte) error {
	tmp := filepath.Join(dir, manifestName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = durable.Sync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, manifestName))
	}
	if err == nil {
		err = durable.Dir(dir)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// collapsing is the suffix of a stored layer's manifest while it moves out of
// its emptied directory to take the directory's place (see [collapse]).
const collapsing = ".manifest"

// collapse replaces the layer directory dir, once it holds nothing but its
// manifest, with the manifest itself, so a stored layer costs one small file.
// The manifest moves aside, the directory goes, the manifest takes its name;
// [OpenLayer] finishes or undoes a crashed collapse from whichever of these
// steps persisted, so none needs a sync. It is best effort: a directory still
// holding anything stays as it is.
func collapse(dir string) {
	items, err := os.ReadDir(dir)
	if err != nil || len(items) != 1 || items[0].Name() != manifestName {
		return
	}
	staged := dir + collapsing
	if os.Rename(filepath.Join(dir, manifestName), staged) != nil {
		return
	}
	if os.Remove(dir) != nil {
		_ = os.Rename(staged, filepath.Join(dir, manifestName))
		return
	}
	_ = os.Rename(staged, dir)
}

// recoverCollapse finishes a collapse of the layer at dir that a crash
// interrupted after the manifest moved aside: it takes the place of the
// emptied (or already removed) directory, or returns into one still holding files.
func recoverCollapse(dir string) error {
	staged := dir + collapsing
	if _, err := os.Lstat(staged); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if items, err := os.ReadDir(dir); err == nil && len(items) != 0 {
		return os.Rename(staged, filepath.Join(dir, manifestName))
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(staged, dir); err != nil {
		return err
	}
	return durable.Dir(filepath.Dir(dir))
}

// OpenLayer loads the frozen layer at dir: from its manifest once stored (the
// manifest file itself, or one inside the directory of a layer that keeps raw
// files), with blobs serving the stored content, else by indexing its raw
// files. Loading a stored layer removes what [Layer.Store] left behind (a crash
// or an open file between writing the manifest and removing raw files). It
// fails on an unreadable, malformed or unknown-version manifest.
func OpenLayer(dir string, blobs Blobs) (*Layer, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := recoverCollapse(root); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	at := root
	if info.IsDir() {
		at = filepath.Join(root, manifestName)
	}
	f, err := os.Open(at)
	if errors.Is(err, fs.ErrNotExist) && info.IsDir() {
		// A manifest never published leaves at most its staging file.
		if err := os.Remove(at + ".tmp"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return indexLayer(root, false)
	}
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("overlay: layer manifest %s: %w", at, err)
	}
	l, err := RestoreLayer(root, data, blobs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		l.prune()
		collapse(root)
	}
	return l, nil
}

// RestoreLayer loads compressed metadata persisted by the caller; raw entries
// stay under dir, which need not exist when every entry is stored. It retains data.
func RestoreLayer(dir string, data []byte, blobs Blobs) (*Layer, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	reader := flate.NewReader(bytes.NewReader(data))
	defer reader.Close()
	var m manifest
	if err := json.NewDecoder(reader).Decode(&m); err != nil {
		return nil, fmt.Errorf("overlay: layer manifest %s: %w", root, err)
	}
	if m.Version != manifestVersion {
		return nil, fmt.Errorf("overlay: layer manifest in %s has version %d, want %d", root, m.Version, manifestVersion)
	}
	l := &Layer{dir: root, entries: make(map[string]Entry), children: make(map[string]map[string]string), blobs: blobs, ids: make(map[string]string), tree: m.Tree, metadata: data}
	links := make(map[int]uint64)
	for i, me := range m.Entries {
		if err := l.load(i, me, links); err != nil {
			return nil, fmt.Errorf("overlay: layer manifest in %s: %w", root, err)
		}
	}
	if e, ok := l.entries[""]; !ok || e.Kind != KindDir {
		return nil, fmt.Errorf("overlay: layer manifest in %s has no root directory", root)
	}
	return l, nil
}

// Metadata returns immutable compressed layer metadata, or nil before Store;
// registries embed it in their shared durable state before removing its file.
func (l *Layer) Metadata() []byte {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.metadata
}

// RemoveMetadata removes the standalone manifest after its caller durably
// persisted Metadata elsewhere; raw entries and their directory remain.
// A layer not yet stored is rejected without changing its files.
func (l *Layer) RemoveMetadata() error {
	if !l.Stored() {
		return &fs.PathError{Op: "remove metadata", Path: l.dir, Err: fs.ErrInvalid}
	}
	info, err := os.Lstat(l.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	at, raw := l.dir, false
	if info.IsDir() {
		raw = l.prune()
		at = filepath.Join(l.dir, manifestName)
	}
	if err := os.Remove(at); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if info.IsDir() && !raw {
		// Stored-only layers can have an empty directory after interrupted cleanup.
		_ = os.Remove(l.dir)
	}
	return nil
}

// load indexes manifest entry i after its parent directory.
func (l *Layer) load(i int, me manifestEntry, links map[int]uint64) error {
	p := me.Path
	if p != Clean(p) || reserved(p) {
		return fmt.Errorf("invalid path %q", p)
	}
	if _, dup := l.entries[p]; dup {
		return fmt.Errorf("duplicate path %q", p)
	}
	if p != "" {
		if e, ok := l.entries[parent(p)]; !ok || e.Kind != KindDir {
			return fmt.Errorf("%q precedes its parent directory", p)
		}
	}
	if me.Kind == KindWhiteout {
		if p == "" {
			return errors.New("whiteout root")
		}
		l.set(p, Entry{Kind: KindWhiteout})
		return nil
	}
	sys, err := decodeSys(me.Sys)
	if err != nil {
		return fmt.Errorf("%q: %w", p, err)
	}
	info := storedInfo{name: me.Name, size: me.Size, mode: os.FileMode(me.Mode), mtime: time.Unix(me.Sec, me.Nsec), sys: sys}
	if p != "" {
		info.name = path.Base(p)
	}
	if me.Link != 0 {
		if me.Link < 1 || me.Link > i+1 {
			return fmt.Errorf("%q links to entry %d", p, me.Link)
		}
		if links[me.Link] == 0 {
			links[me.Link] = storedLinks.Add(1)
		}
		info.link = links[me.Link]
	}
	e := entry(info)
	if e.Kind != me.Kind {
		return fmt.Errorf("%q has kind %d but mode %v", p, me.Kind, info.mode)
	}
	if me.Blob != "" {
		if e.Kind != KindSymlink && !info.mode.IsRegular() {
			return fmt.Errorf("%q has a blob but mode %v", p, info.mode)
		}
		l.ids[p] = me.Blob
	}
	e.Opaque = me.Opaque
	if validOrigin(e.Kind, me.Origin) {
		e.Origin = me.Origin
	}
	l.set(p, e)
	return nil
}

// prune removes from the layer directory every raw file the manifest makes
// redundant (stored content, whiteout and opaque markers) and the directories
// left empty, keeping raw entries. It is best effort: what cannot be removed now
// (a file open on Windows) is retried when the layer is next opened. It reports
// whether the layer still holds raw entries.
func (l *Layer) prune() bool {
	l.mu.RLock()
	ids := l.ids
	l.mu.RUnlock()
	_ = os.Remove(filepath.Join(l.dir, originsName))
	_ = os.Remove(filepath.Join(l.dir, originsName+".tmp"))
	var dirs []string
	held := make(map[string]bool) // directories holding a raw entry, the root included
	_ = l.Walk(func(p string, e Entry) error {
		host := filepath.Join(l.dir, filepath.FromSlash(p))
		switch e.Kind {
		case KindDir:
			// Removing children needs a writable directory; its recorded mode is in the manifest.
			if perm := e.Info.Mode().Perm(); perm&0o300 != 0o300 {
				_ = os.Chmod(host, perm|0o700)
			}
			if e.Opaque {
				_ = os.Remove(filepath.Join(host, opaqueName))
			}
			if p != "" {
				dirs = append(dirs, p)
			}
		case KindWhiteout:
			_ = os.Remove(filepath.Join(l.dir, filepath.FromSlash(parent(p)), whPrefix+path.Base(p)))
		default:
			if _, ok := ids[p]; ok {
				_ = os.Remove(host)
				return nil
			}
			for d := parent(p); !held[d]; d = parent(d) {
				held[d] = true
				if d == "" {
					break
				}
			}
		}
		return nil
	})
	// Children follow their parents in walk order. A directory holding a raw
	// entry is not even tried: macOS rmdir(2) also removes a directory that
	// holds only AppleDouble "._" files, deleting them with it.
	for i := len(dirs) - 1; i >= 0; i-- {
		if !held[dirs[i]] {
			_ = os.Remove(filepath.Join(l.dir, filepath.FromSlash(dirs[i])))
		}
	}
	return held[""]
}
