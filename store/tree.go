package store

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Entry is one named child of a git tree.
type Entry struct {
	Name string
	Mode uint32
	ID   OID
}

// File is a leaf of a flattened tree: a blob (file/exec/symlink) or an empty directory (ModeDir, EmptyTree).
type File struct {
	Mode uint32
	ID   OID
}

// validMode reports whether mode is one of the snapshot tree entry modes.
func validMode(mode uint32) bool {
	return mode == ModeFile || mode == ModeExec || mode == ModeSymlink || mode == ModeDir
}

// checkName rejects names git cannot store or that would escape or alias a directory.
func checkName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("empty entry name")
	case name == "." || name == "..":
		return fmt.Errorf("entry name %q", name)
	case strings.ContainsAny(name, "/\x00"):
		return fmt.Errorf("entry name %q contains '/' or NUL", name)
	}
	return nil
}

// sortKey is the name git compares when ordering tree entries: directories sort as name+"/".
func sortKey(e Entry) string {
	if e.Mode == ModeDir {
		return e.Name + "/"
	}
	return e.Name
}

// PutTree sorts entries in git order (directories compare as name+"/"), encodes "<octal mode without leading zeros> <name>\x00<20 raw bytes>", stores and returns the id. Rejects empty/duplicate names and names containing '/' or NUL.
// It also rejects "." and "..", unknown modes and malformed ids. Referenced objects need not be stored.
func (s *Store) PutTree(entries []Entry) (OID, error) {
	data, err := encodeTree(entries)
	if err != nil {
		return "", err
	}
	id, err := s.putData("tree", HashBytes("tree", data), data)
	if err != nil {
		return "", fmt.Errorf("put tree: %w", err)
	}
	return id, nil
}

// encodeTree validates entries and encodes them as a git tree body.
func encodeTree(entries []Entry) ([]byte, error) {
	sorted := slices.Clone(entries)
	seen := make(map[string]struct{}, len(sorted))
	for _, e := range sorted {
		if err := checkName(e.Name); err != nil {
			return nil, fmt.Errorf("put tree: %w", err)
		}
		if !validMode(e.Mode) {
			return nil, fmt.Errorf("put tree: entry %q has mode %o", e.Name, e.Mode)
		}
		if !e.ID.Valid() {
			return nil, fmt.Errorf("put tree: entry %q has malformed id %q", e.Name, e.ID)
		}
		if _, dup := seen[e.Name]; dup {
			return nil, fmt.Errorf("put tree: duplicate entry %q", e.Name)
		}
		seen[e.Name] = struct{}{}
	}
	slices.SortFunc(sorted, func(a, b Entry) int { return strings.Compare(sortKey(a), sortKey(b)) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(strconv.FormatUint(uint64(e.Mode), 8))
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(string(e.ID))
		buf.Write(raw)
	}
	return buf.Bytes(), nil
}

// ReadTree returns the entries of the stored tree id in git order; EmptyTree reads as no entries even if never stored.
func (s *Store) ReadTree(id OID) ([]Entry, error) {
	if id == EmptyTree {
		return nil, nil
	}
	data, err := s.treeBody(id)
	if err != nil {
		return nil, err
	}
	entries, err := parseTree(data)
	if err != nil {
		return nil, fmt.Errorf("read tree %s: %w", id, err)
	}
	return entries, nil
}

// treeBody returns the verified body of stored tree id, possibly the cache's
// copy, which must not be modified.
func (s *Store) treeBody(id OID) ([]byte, error) {
	if !id.Valid() {
		return nil, fmt.Errorf("read tree: read object %q: malformed id", id)
	}
	if e, ok := s.packs.lookup(rawOf(id)); ok && e.flags&(frameChunk|frameManifest) != 0 {
		return nil, fmt.Errorf("read tree %s: %w", id, errCorrupt)
	}
	data, ok := s.cache.get(id)
	if !ok {
		var err error
		if data, err = s.load(id); err != nil {
			return nil, fmt.Errorf("read tree: read object %s: %w", id, err)
		}
	}
	if HashBytes("tree", data) != id {
		return nil, fmt.Errorf("read tree %s: %w", id, errCorrupt)
	}
	return data, nil
}

// parseTree decodes a git tree body, validating every entry.
func parseTree(data []byte) ([]Entry, error) {
	var entries []Entry
	err := walkTree(data, func(mode uint32, name []byte, id rawID) error {
		if err := checkName(string(name)); err != nil {
			return err
		}
		entries = append(entries, Entry{Name: string(name), Mode: mode, ID: id.oid()})
		return nil
	})
	return entries, err
}

// walkTree calls fn with each entry of a git tree body in order, its name a
// view of data; it validates modes and framing, not names.
func walkTree(data []byte, fn func(mode uint32, name []byte, id rawID) error) error {
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return fmt.Errorf("truncated entry mode")
		}
		var mode uint32
		for _, c := range data[:sp] {
			if c < '0' || c > '7' || mode > 0o7777777 {
				return fmt.Errorf("bad entry mode %q", data[:sp])
			}
			mode = mode<<3 | uint32(c-'0')
		}
		if !validMode(mode) {
			return fmt.Errorf("bad entry mode %q", data[:sp])
		}
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+1+20 {
			return fmt.Errorf("truncated entry")
		}
		if err := fn(mode, data[:nul], rawID(data[nul+1:nul+21])); err != nil {
			return err
		}
		data = data[nul+21:]
	}
	return nil
}

// Flatten lists every leaf below root by slash path; non-empty directories are implied by their leaves.
func (s *Store) Flatten(root OID) (map[string]File, error) {
	files := make(map[string]File)
	if err := s.flatten(root, "", files); err != nil {
		return nil, err
	}
	return files, nil
}

// flatten adds the leaves of tree id to files, prefixing their names with dir.
func (s *Store) flatten(id OID, dir string, files map[string]File) error {
	entries, err := s.ReadTree(id)
	if err != nil {
		return fmt.Errorf("flatten %q: %w", dir, err)
	}
	for _, e := range entries {
		p := e.Name
		if dir != "" {
			p = dir + "/" + e.Name
		}
		if e.Mode == ModeDir && e.ID != EmptyTree {
			if err := s.flatten(e.ID, p, files); err != nil {
				return err
			}
			continue
		}
		files[p] = File{Mode: e.Mode, ID: e.ID}
	}
	return nil
}

// buildNode is one directory while Build assembles the implied tree.
type buildNode struct {
	files map[string]File
	dirs  map[string]*buildNode
}

// newBuildNode returns an empty directory node.
func newBuildNode() *buildNode {
	return &buildNode{files: map[string]File{}, dirs: map[string]*buildNode{}}
}

// Build writes every implied directory tree bottom-up and returns the root id; a leaf that is also a parent of another leaf is an error.
// Paths must be clean, relative and slash-separated; a ModeDir leaf must be EmptyTree.
// Sweep keeps everything the root reaches for as long as it keeps objects
// stored at the moment Build returned, so the caller may record the root later.
func (s *Store) Build(files map[string]File) (OID, error) {
	root := newBuildNode()
	for p, f := range files {
		if !validMode(f.Mode) || !f.ID.Valid() {
			return "", fmt.Errorf("build: %q has mode %o id %q", p, f.Mode, f.ID)
		}
		if f.Mode == ModeDir && f.ID != EmptyTree {
			return "", fmt.Errorf("build: directory leaf %q is not the empty tree", p)
		}
		if p == "" {
			return "", fmt.Errorf("build: empty path")
		}
		parts := strings.Split(p, "/")
		n := root
		for i, name := range parts {
			if err := checkName(name); err != nil {
				return "", fmt.Errorf("build: path %q: %w", p, err)
			}
			if i == len(parts)-1 {
				if _, ok := n.dirs[name]; ok {
					return "", fmt.Errorf("build: %q is both a leaf and a directory", p)
				}
				n.files[name] = f
				break
			}
			if _, ok := n.files[name]; ok {
				return "", fmt.Errorf("build: %q is both a leaf and a directory", strings.Join(parts[:i+1], "/"))
			}
			child := n.dirs[name]
			if child == nil {
				child = newBuildNode()
				n.dirs[name] = child
			}
			n = child
		}
	}
	// A tree known stored when its build started may have been removed before the
	// root was registered only if the epoch changed in between; build again then.
	for {
		epoch := s.Epoch()
		id, err := s.buildTree(root)
		if err != nil {
			return "", err
		}
		if s.register(id, epoch) {
			return id, nil
		}
	}
}

// TreeWriter stores the changed directories of a tree derived from stored
// trees, reusing the ids of unchanged subtrees, and registers its root the way
// Build does. Snapshot hashing creates one per attempt, before reading the
// trees it derives from.
type TreeWriter struct {
	s     *Store
	epoch string
}

// NewTreeWriter starts a derivation in the current epoch.
func (s *Store) NewTreeWriter() *TreeWriter { return &TreeWriter{s: s, epoch: s.Epoch()} }

// Put stores the directory holding entries (validated as by PutTree) and
// returns its id; a tree already known stored in this epoch is not written again.
func (w *TreeWriter) Put(entries []Entry) (OID, error) {
	return w.PutLike(entries, "")
}

// PutLike stores entries, using the same-path parent tree like as a delta base when beneficial.
func (w *TreeWriter) PutLike(entries []Entry, like OID) (OID, error) {
	data, err := encodeTree(entries)
	if err != nil {
		return "", err
	}
	id := HashBytes("tree", data)
	if w.s.knownTree(id) {
		return id, nil
	}
	if _, err := w.s.putLikeKind("tree", data, like); err != nil {
		return "", fmt.Errorf("put tree: %w", err)
	}
	return id, nil
}

// Commit registers root so Sweep keeps everything it reaches until the caller
// records it, as Build does. It reports false when objects may have been
// removed since the writer was created: trees read or reused meanwhile may be
// gone, so the caller derives the tree again with a new writer.
func (w *TreeWriter) Commit(root OID) bool { return w.s.register(root, w.epoch) }

// buildTree stores the subtrees of n, then n itself, returning its id; trees
// stored earlier in the current epoch are not touched again.
func (s *Store) buildTree(n *buildNode) (OID, error) {
	entries := make([]Entry, 0, len(n.files)+len(n.dirs))
	for name, f := range n.files {
		entries = append(entries, Entry{Name: name, Mode: f.Mode, ID: f.ID})
	}
	for name, child := range n.dirs {
		id, err := s.buildTree(child)
		if err != nil {
			return "", err
		}
		entries = append(entries, Entry{Name: name, Mode: ModeDir, ID: id})
	}
	data, err := encodeTree(entries)
	if err != nil {
		return "", err
	}
	id := HashBytes("tree", data)
	if s.knownTree(id) {
		return id, nil
	}
	if _, err := s.putData("tree", id, data); err != nil {
		return "", fmt.Errorf("put tree: %w", err)
	}
	return id, nil
}
