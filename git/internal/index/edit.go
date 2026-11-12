package index

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
)

// Add inserts e, replacing any entry at the same path and stage, with the
// semantics of a "<mode> <oid> [<stage>]\t<path>" line fed to
// `git update-index -z --index-info`:
//   - the mode is canonicalized as git does: symlinks and gitlinks keep their
//     type, anything else becomes 100755 if the owner-execute bit is set and
//     100644 otherwise; tree modes are rejected;
//   - adding a stage-0 entry resolves a conflict: the path's higher stages
//     are removed and saved as resolve-undo information;
//   - entries of the same stage that conflict as file and directory with the
//     path ("a" when adding "a/b", or "a/..." when adding "a") are removed;
//   - the cache tree is invalidated along the paths touched.
//
// update-index records no stat data or flags for such lines; pass only Path,
// Mode, ID and Stage to mirror it. Paths inside a sparse-directory entry
// return ErrSparseDirectory.
func (idx *Index) Add(e Entry) error {
	e, err := idx.prepare(e)
	if err != nil {
		return err
	}

	if e.Stage == 0 {
		lo, hi := idx.pathRange(e.Path)
		idx.removeIn(lo, hi, func(x *Entry) bool { return x.Stage > 0 })
	}
	// A file at a parent directory of the path.
	for i := strings.IndexByte(e.Path, '/'); i >= 0; i = nextSlash(e.Path, i) {
		if j, ok := idx.Find(e.Path[:i], e.Stage); ok {
			idx.removeIn(j, j+1, func(*Entry) bool { return true })
		}
	}
	// Entries below the path, which is becoming a file.
	lo, hi := idx.prefixRange(e.Path + "/")
	idx.removeIn(lo, hi, func(x *Entry) bool { return x.Stage == e.Stage })

	if idx.CacheTree != nil {
		idx.CacheTree.invalidatePath(e.Path)
	}
	if i, ok := idx.Find(e.Path, e.Stage); ok {
		idx.Entries[i] = e
	} else {
		idx.Entries = slices.Insert(idx.Entries, i, e)
	}
	return nil
}

// prepare validates e for Add and canonicalizes its mode.
func (idx *Index) prepare(e Entry) (Entry, error) {
	if err := verifyPath(e.Path); err != nil {
		return e, err
	}
	if e.Stage > 3 {
		return e, fmt.Errorf("%s: invalid stage %d", e.Path, e.Stage)
	}
	if e.ID.Format() != idx.Format {
		return e, fmt.Errorf("%s: object name %q is not a %s name", e.Path, e.ID, idx.Format)
	}
	mode, err := canonicalMode(e.Mode)
	if err != nil {
		return e, fmt.Errorf("%s: %w", e.Path, err)
	}
	e.Mode = mode
	return e, idx.checkNotSparse(e.Path)
}

// Remove deletes every stage of path, saving removed conflict stages as
// resolve-undo information, and invalidates the cache tree along path even
// when nothing was removed, as git does. It reports whether an entry was
// removed. This is a mode-0 line of `git update-index --index-info`. Paths
// inside a sparse-directory entry return ErrSparseDirectory.
func (idx *Index) Remove(path string) (bool, error) {
	if err := idx.checkNotSparse(path); err != nil {
		return false, err
	}
	if idx.CacheTree != nil {
		idx.CacheTree.invalidatePath(path)
	}
	lo, hi := idx.pathRange(path)
	return idx.removeIn(lo, hi, func(*Entry) bool { return true }), nil
}

// Replace has the effect of Remove for each path in remove followed by Add
// for each entry of add, but costs one pass over Entries instead of shifting
// them once per edit. On error the index may be partly updated, as by that
// sequence.
func (idx *Index) Replace(remove []string, add []Entry) error {
	if len(remove) > 0 {
		dead := make([]bool, len(idx.Entries))
		for _, p := range remove {
			if err := idx.checkNotSparseLive(p, dead); err != nil {
				return err
			}
			lo, hi := idx.pathRange(p)
			for i := lo; i < hi; i++ {
				dead[i] = true
			}
		}
		if idx.CacheTree != nil {
			for _, p := range remove {
				idx.CacheTree.invalidatePath(p)
			}
		}
		kept := 0
		for i := range idx.Entries {
			e := &idx.Entries[i]
			if dead[i] {
				if e.Stage > 0 {
					idx.recordResolveUndo(e)
				}
				continue
			}
			idx.Entries[kept] = *e
			kept++
		}
		clear(idx.Entries[kept:])
		idx.Entries = idx.Entries[:kept]
	}
	adds, ok := idx.insertable(add)
	switch {
	case ok && len(adds) == 0:
		return nil
	case !ok:
		// Some entry fails or displaces another: Add decides each in turn.
		for _, e := range add {
			if err := idx.Add(e); err != nil {
				return err
			}
		}
		return nil
	}
	if idx.CacheTree != nil {
		for i := range adds {
			idx.CacheTree.invalidatePath(adds[i].Path)
		}
	}
	// Merge from the back, so each entry moves once and in place.
	n := len(idx.Entries)
	idx.Entries = slices.Grow(idx.Entries, len(adds))[:n+len(adds)]
	for i, j, k := n-1, len(adds)-1, len(idx.Entries)-1; j >= 0; k-- {
		if i >= 0 && compareEntry(&idx.Entries[i], &adds[j]) > 0 {
			idx.Entries[k], i = idx.Entries[i], i-1
		} else {
			idx.Entries[k], j = adds[j], j-1
		}
	}
	return nil
}

// insertable returns add canonicalized and sorted if Add would accept every
// entry and insert it without replacing or removing anything, the entries
// included: no path is already present at any stage, has a present path as a
// parent or any below it, or is added twice.
func (idx *Index) insertable(add []Entry) ([]Entry, bool) {
	out := make([]Entry, len(add))
	for i, e := range add {
		e, err := idx.prepare(e)
		if err != nil {
			return nil, false
		}
		if lo, hi := idx.pathRange(e.Path); lo < hi {
			return nil, false
		}
		for j := strings.IndexByte(e.Path, '/'); j >= 0; j = nextSlash(e.Path, j) {
			if lo, hi := idx.pathRange(e.Path[:j]); lo < hi {
				return nil, false
			}
		}
		if lo, hi := idx.prefixRange(e.Path + "/"); lo < hi {
			return nil, false
		}
		out[i] = e
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	for i := range out {
		if i > 0 && out[i].Path == out[i-1].Path {
			return nil, false
		}
		// Paths below out[i] sort after it, though not necessarily next to it.
		prefix := out[i].Path + "/"
		j, _ := slices.BinarySearchFunc(out[i+1:], prefix, func(e Entry, p string) int { return strings.Compare(e.Path, p) })
		if i+1+j < len(out) && strings.HasPrefix(out[i+1+j].Path, prefix) {
			return nil, false
		}
	}
	return out, true
}

// checkNotSparseLive is checkNotSparse ignoring the entries marked dead.
func (idx *Index) checkNotSparseLive(path string, dead []bool) error {
	for i := strings.IndexByte(path, '/'); i >= 0; i = nextSlash(path, i) {
		if j, ok := idx.Find(path[:i+1], 0); ok && !dead[j] && idx.Entries[j].IsSparseDir() {
			return fmt.Errorf("%s: %w %s", path, ErrSparseDirectory, path[:i+1])
		}
	}
	return nil
}

// pathRange returns the span of Entries holding any stage of path.
func (idx *Index) pathRange(path string) (int, int) {
	lo, _ := idx.Find(path, 0)
	hi := lo
	for hi < len(idx.Entries) && idx.Entries[hi].Path == path {
		hi++
	}
	return lo, hi
}

// prefixRange returns the span of Entries whose paths start with prefix.
func (idx *Index) prefixRange(prefix string) (int, int) {
	lo, _ := idx.Find(prefix, 0)
	hi := lo
	for hi < len(idx.Entries) && strings.HasPrefix(idx.Entries[hi].Path, prefix) {
		hi++
	}
	return lo, hi
}

func nextSlash(path string, i int) int {
	j := strings.IndexByte(path[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// removeIn deletes the entries in Entries[lo:hi] that match. Conflict stages
// removed are saved as resolve-undo records, and the cache tree is
// invalidated for each removed path.
func (idx *Index) removeIn(lo, hi int, match func(*Entry) bool) bool {
	kept := lo
	for i := lo; i < hi; i++ {
		e := &idx.Entries[i]
		if !match(e) {
			idx.Entries[kept] = *e
			kept++
			continue
		}
		if e.Stage > 0 {
			idx.recordResolveUndo(e)
		}
		if idx.CacheTree != nil {
			idx.CacheTree.invalidatePath(e.Path)
		}
	}
	idx.Entries = slices.Delete(idx.Entries, kept, hi)
	return kept < hi
}

// recordResolveUndo saves conflict stage e in the path's resolve-undo record,
// overwriting only that stage of an existing record.
func (idx *Index) recordResolveUndo(e *Entry) {
	i, ok := slices.BinarySearchFunc(idx.ResolveUndo, e.Path, func(r ResolveUndo, path string) int {
		return strings.Compare(r.Path, path)
	})
	if !ok {
		idx.ResolveUndo = slices.Insert(idx.ResolveUndo, i, ResolveUndo{Path: e.Path})
	}
	idx.ResolveUndo[i].Mode[e.Stage-1] = e.Mode
	idx.ResolveUndo[i].ID[e.Stage-1] = e.ID
}

// checkNotSparse rejects paths below a sparse-directory entry.
func (idx *Index) checkNotSparse(path string) error {
	for i := strings.IndexByte(path, '/'); i >= 0; i = nextSlash(path, i) {
		if j, ok := idx.Find(path[:i+1], 0); ok && idx.Entries[j].IsSparseDir() {
			return fmt.Errorf("%s: %w %s", path, ErrSparseDirectory, path[:i+1])
		}
	}
	return nil
}

// canonicalMode maps a mode to one git stores, as update-index does with
// --index-info input.
func canonicalMode(m object.Mode) (object.Mode, error) {
	switch m & 0o170000 {
	case object.ModeSymlink:
		return object.ModeSymlink, nil
	case object.ModeGitlink:
		return object.ModeGitlink, nil
	case object.ModeTree:
		return 0, fmt.Errorf("mode %s is a directory", m)
	}
	if m&0o100 != 0 {
		return object.ModeExec, nil
	}
	return object.ModeFile, nil
}

var errInvalidPath = errors.New("invalid path")

// verifyPath rejects paths git refuses to put in the index: empty, absolute,
// with a trailing or doubled slash, or with a ".", ".." or ".git" component
// (".git" in any case).
func verifyPath(path string) error {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%w %q", errInvalidPath, path)
	}
	for c := range strings.SplitSeq(path, "/") {
		if c == "" || c == "." || c == ".." || strings.EqualFold(c, ".git") {
			return fmt.Errorf("%w %q", errInvalidPath, path)
		}
	}
	return nil
}
