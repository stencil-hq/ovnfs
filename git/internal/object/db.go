package object

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// maxAlternateDepth bounds how deep alternates may chain.
	maxAlternateDepth = 5
	// maxPeel bounds tag chains, which only corruption could make cyclic.
	maxPeel = 1000
)

var (
	errClosed = errors.New("object database is closed")
	// errMiss is lookup's internal "not stored here".
	errMiss = errors.New("object not in this store")
)

// notFoundError reports a missing object; it matches os.ErrNotExist.
type notFoundError struct{ id ID }

func (e *notFoundError) Error() string { return "object " + e.id.String() + " not found" }
func (e *notFoundError) Unwrap() error { return fs.ErrNotExist }

// DB reads a repository's objects, loose and packed, from its objects
// directory and alternates, and writes new ones. It is safe for concurrent use.
type DB struct {
	format Format
	dirs   []*objectDir // the repository's own store first, then alternates
	closed atomic.Bool
	// generation counts pack list changes, so a lookup that missed while
	// another goroutine's rescan swapped packs knows to look again.
	generation atomic.Uint64
}

// objectDir is one object store: the repository's or an alternate.
type objectDir struct {
	path   string
	mu     sync.Mutex // serializes rescans and Close
	closed bool       // guarded by mu
	packs  atomic.Pointer[[]*pack]
}

// Open opens the object database rooted at objectsDir (a repository's
// "objects" directory) holding objects of format f, following
// objects/info/alternates. The environment (GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES) is deliberately ignored.
func Open(objectsDir string, f Format) (*DB, error) {
	if f != SHA1 && f != SHA256 {
		return nil, fmt.Errorf("unknown object format %v", f)
	}
	abs, err := filepath.Abs(objectsDir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("object directory %s is not a directory", abs)
	}
	db := &DB{format: f}
	db.addDir(abs, 0, map[string]bool{})
	db.rescan()
	return db, nil
}

// Format returns the hash format of the objects in the database.
func (db *DB) Format() Format { return db.format }

// Dirs returns the absolute object directories the database searches, in
// lookup order: its own store first, then the resolved alternates. Readers
// of per-store files (such as info/commit-graph) use it to cover alternates.
func (db *DB) Dirs() []string {
	dirs := make([]string, len(db.dirs))
	for i, d := range db.dirs {
		dirs[i] = d.path
	}
	return dirs
}

// Close releases the database's packs. Reads already in progress finish
// first; later calls fail.
func (db *DB) Close() error {
	if db.closed.Swap(true) {
		return nil
	}
	for _, d := range db.dirs {
		d.mu.Lock()
		old := *d.packs.Swap(new([]*pack))
		d.closed = true
		d.mu.Unlock()
		for _, p := range old {
			deltaBases.dropPack(p)
			p.release()
		}
	}
	return nil
}

// addDir adds an object store and, recursively, its alternates. Directories
// already added (alternates may form cycles) and missing ones are skipped.
func (db *DB) addDir(path string, depth int, seen map[string]bool) {
	key := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		key = real
	}
	if seen[key] {
		return
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return
	}
	seen[key] = true
	d := &objectDir{path: path}
	d.packs.Store(new([]*pack))
	db.dirs = append(db.dirs, d)
	if depth >= maxAlternateDepth {
		return
	}
	for _, alt := range readAlternates(path) {
		db.addDir(alt, depth+1, seen)
	}
}

// readAlternates lists the alternate object stores of objects directory dir:
// one path per line of info/alternates, taken verbatim (git trims nothing),
// '#' starting a comment line, a leading '"' marking a C-quoted path, and
// relative paths resolved against dir.
func readAlternates(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, "info", "alternates"))
	if err != nil {
		return nil
	}
	var paths []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '"' {
			if s, err := strconv.Unquote(line); err == nil {
				line = s
			}
		}
		if !filepath.IsAbs(line) {
			line = filepath.Join(dir, line)
		}
		paths = append(paths, filepath.Clean(line))
	}
	return paths
}

// rescan brings every store's pack list up to date: another process may have
// repacked or fetched since the last scan.
func (db *DB) rescan() {
	for _, d := range db.dirs {
		db.rescanDir(d)
	}
}

// Rescan brings the database's view of every store's packs up to date with
// the pack directories, picking up packs another process has written and
// dropping deleted ones. Lookups that miss do this themselves; Contains does not.
func (db *DB) Rescan() error {
	if db.closed.Load() {
		return errClosed
	}
	db.rescan()
	return nil
}

// rescanSince rescans and reports whether any pack list changed since
// generation gen was observed, by this rescan or a concurrent one.
func (db *DB) rescanSince(gen uint64) bool {
	db.rescan()
	return db.generation.Load() != gen
}

func (db *DB) rescanDir(d *objectDir) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	packDir := filepath.Join(d.path, "pack")
	ents, _ := os.ReadDir(packDir)
	present := map[string]bool{}
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".idx") {
			present[filepath.Join(packDir, n)] = true
		}
	}
	old := *d.packs.Load()
	var next, gone []*pack
	for _, p := range old {
		idx := strings.TrimSuffix(p.path, ".pack") + ".idx"
		if present[idx] {
			next = append(next, p)
			delete(present, idx)
		} else {
			gone = append(gone, p)
		}
	}
	opened := false
	for _, idx := range slices.Sorted(maps.Keys(present)) {
		// A pack that fails to open (half-deleted, corrupt) is skipped and
		// retried on the next scan.
		if p, err := openPack(idx, db.format); err == nil {
			next = append(next, p)
			opened = true
		}
	}
	if !opened && len(gone) == 0 {
		return
	}
	d.packs.Store(&next)
	db.generation.Add(1)
	for _, p := range gone {
		deltaBases.dropPack(p)
		p.release()
	}
}

// lookup finds where id is stored and calls inPack or inLoose on it, trying
// further copies if one fails. When no store has it, packs are rescanned and,
// if they changed, the search is repeated once.
func (db *DB) lookup(id ID, inPack func(*pack, int64) error, inLoose func(string) error) error {
	if db.closed.Load() {
		return errClosed
	}
	if id.format != db.format {
		return fmt.Errorf("object name %s is not a %s name", id, db.format)
	}
	gen := db.generation.Load()
	err := db.lookupOnce(id, inPack, inLoose)
	if err == errMiss && db.rescanSince(gen) {
		err = db.lookupOnce(id, inPack, inLoose)
	}
	if err == errMiss {
		return &notFoundError{id}
	}
	return err
}

// lookupOnce is one pass of lookup over the current pack lists; it returns
// errMiss when no store has id, or the first failure if every copy failed.
func (db *DB) lookupOnce(id ID, inPack func(*pack, int64) error, inLoose func(string) error) error {
	var firstErr error
	done := func(err error) bool {
		if err != nil && err != errMiss && firstErr == nil {
			firstErr = err
		}
		return err == nil
	}
	for _, d := range db.dirs {
		for _, p := range *d.packs.Load() {
			if done(tryPack(p, id, inPack)) {
				return nil
			}
		}
		err := inLoose(loosePath(d.path, id))
		if errors.Is(err, fs.ErrNotExist) {
			err = errMiss
		}
		if done(err) {
			return nil
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return errMiss
}

func tryPack(p *pack, id ID, fn func(*pack, int64) error) error {
	if !p.acquire() {
		return errMiss
	}
	defer p.release()
	i, ok, err := p.idx.find(id)
	if err != nil {
		return err
	}
	if !ok {
		return errMiss
	}
	off, err := p.idx.offset(i)
	if err != nil {
		return err
	}
	return fn(p, off)
}

func statLoose(path string) error {
	_, err := os.Lstat(path)
	return err
}

// Has reports whether the database stores id.
func (db *DB) Has(id ID) bool {
	return db.lookup(id, func(*pack, int64) error { return nil }, statLoose) == nil
}

// Contains is Has without the rescan of pack directories on a miss, for bulk
// checks where misses are expected; call Rescan (or Has) once first if packs
// may have changed since the last scan.
func (db *DB) Contains(id ID) bool {
	if db.closed.Load() || id.format != db.format {
		return false
	}
	return db.lookupOnce(id, func(*pack, int64) error { return nil }, statLoose) == nil
}

// Header returns the kind and size of object id without reconstructing it.
// A missing object is an error matching os.ErrNotExist.
func (db *DB) Header(id ID) (Kind, int64, error) {
	return db.header(id, 0)
}

func (db *DB) header(id ID, depth int) (kind Kind, size int64, err error) {
	if depth > maxDeltaChain {
		return 0, 0, fmt.Errorf("object %s: delta bases nest too deeply", id)
	}
	err = db.lookup(id,
		func(p *pack, off int64) (err error) {
			kind, size, err = p.header(db, off, depth)
			return err
		},
		func(path string) (err error) {
			kind, size, err = readLooseHeader(path)
			return err
		})
	return kind, size, err
}

// Read returns the kind and content of object id. The caller owns the
// returned slice. A missing object is an error matching os.ErrNotExist.
func (db *DB) Read(id ID) (Kind, []byte, error) {
	return db.read(id, 0)
}

func (db *DB) read(id ID, depth int) (kind Kind, data []byte, err error) {
	if depth > maxDeltaChain {
		return 0, nil, fmt.Errorf("object %s: delta bases nest too deeply", id)
	}
	err = db.lookup(id,
		func(p *pack, off int64) (err error) {
			kind, data, err = p.read(db, off, depth)
			return err
		},
		func(path string) (err error) {
			kind, data, err = readLoose(path)
			return err
		})
	return kind, data, err
}

// Peel follows annotated tags from id until it reaches an object of kind
// want; for want Tree a commit yields its tree. Reaching an object that
// cannot peel to want is an error that does not match os.ErrNotExist.
func (db *DB) Peel(id ID, want Kind) (ID, error) {
	for range maxPeel {
		kind, _, err := db.Header(id)
		if err != nil {
			return ID{}, err
		}
		if kind == want {
			return id, nil
		}
		switch {
		case kind == Tag:
			_, data, err := db.Read(id)
			if err != nil {
				return ID{}, err
			}
			t, err := ParseTag(db.format, data)
			if err != nil {
				return ID{}, fmt.Errorf("tag %s: %w", id, err)
			}
			id = t.Object
		case kind == Commit && want == Tree:
			_, data, err := db.Read(id)
			if err != nil {
				return ID{}, err
			}
			c, err := ParseCommit(db.format, data)
			if err != nil {
				return ID{}, fmt.Errorf("commit %s: %w", id, err)
			}
			return c.Tree, nil
		default:
			return ID{}, fmt.Errorf("object %s is a %s, not a %s", id, kind, want)
		}
	}
	return ID{}, fmt.Errorf("object %s: tag chain too long", id)
}

// FindPrefix returns, sorted, the distinct objects whose hex name starts with
// hexPrefix (at least 4 hex digits, any case) in every store, stopping once
// limit are found when limit > 0. Abbreviated names resolve when it returns
// exactly one, and are ambiguous when it returns more.
func (db *DB) FindPrefix(hexPrefix string, limit int) ([]ID, error) {
	if db.closed.Load() {
		return nil, errClosed
	}
	p, err := parsePrefix(db.format, hexPrefix)
	if err != nil {
		return nil, err
	}
	gen := db.generation.Load()
	ids, err := db.findPrefixOnce(p, limit)
	if err == nil && len(ids) == 0 && db.rescanSince(gen) {
		ids, err = db.findPrefixOnce(p, limit)
	}
	return ids, err
}

func (db *DB) findPrefixOnce(p prefix, limit int) ([]ID, error) {
	set := map[ID]struct{}{}
	full := func() bool { return limit > 0 && len(set) >= limit }
	add := func(id ID) bool {
		set[id] = struct{}{}
		return !full()
	}
	for _, d := range db.dirs {
		for _, pk := range *d.packs.Load() {
			if full() || !pk.acquire() {
				continue
			}
			err := pk.idx.findPrefix(p, add)
			pk.release()
			if err != nil {
				return nil, err
			}
		}
		if full() {
			break
		}
		ents, err := os.ReadDir(filepath.Join(d.path, p.full[:2]))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range ents {
			name := e.Name()
			if len(name) != db.format.HexSize()-2 || !strings.HasPrefix(name, p.full[2:]) {
				continue
			}
			if id, err := ParseIDFormat(db.format, p.full[:2]+name); err == nil && !add(id) {
				break
			}
		}
	}
	return slices.SortedFunc(maps.Keys(set), ID.Compare), nil
}
