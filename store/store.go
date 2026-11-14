// Package store keeps git-format objects (blobs and trees) content-addressed
// under one directory, without git's "<type> <len>\x00" header. Objects up to
// cloneFile bytes are appended, zstd-compressed where that shrinks them, to
// pack files indexed by sorted, memory-mapped index files (see index.go). Larger blobs stay loose when reflinked, or
// use content-defined packed chunks when copying; loose blobs contain exactly
// the file's bytes and can be cloned into a checkout. Ids are Git SHA-1 ids,
// so a snapshot tree built
// here is byte-identical to the one git would write.
//
// On-disk layout:
//
//	<dir>/packs/<n>.pack            appended object frames (see pack.go)
//	<dir>/packs/<n>.idx             index of a sealed (full) pack
//	<dir>/packs/<g>.midx            index merged from the indexes of many sealed packs
//	<dir>/objects/<2 hex>/<38 hex>  loose object body, read-only (0444)
//	<dir>/tmp/<unique>              private staging files, renamed into objects
//	<dir>/epoch                     token Sweep replaces before it removes objects
//
// Small blobs are also kept in memory, least recently used evicted first
// within a byte budget, so hot small files are read without opening objects.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// OID is a git object id: 40 lowercase hex SHA-1 over "<type> <len>\x00<body>".
type OID string

// EmptyTree is the id of the tree with no entries; it is valid even if never stored.
const EmptyTree OID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Git tree entry modes used by snapshots.
const (
	ModeFile    uint32 = 0o100644
	ModeExec    uint32 = 0o100755
	ModeSymlink uint32 = 0o120000
	ModeDir     uint32 = 0o040000
)

// objectPerm protects loose object files from writes; contents are immutable by id.
const objectPerm os.FileMode = 0o444

// ModeOf maps os.FileMode to a git mode (any user-exec bit → ModeExec; symlink; dir); ok=false for other types.
func ModeOf(m os.FileMode) (uint32, bool) {
	switch {
	case m.IsDir():
		return ModeDir, true
	case m&os.ModeSymlink != 0:
		return ModeSymlink, true
	case m.IsRegular() && m&0o100 != 0:
		return ModeExec, true
	case m.IsRegular():
		return ModeFile, true
	}
	return 0, false
}

// Valid reports whether id is a well-formed 40-character lowercase hex SHA-1.
func (id OID) Valid() bool {
	if len(id) != 2*sha1.Size {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Store keeps Git-format objects under one directory: small objects packed,
// large cloned blobs loose, and large copied blobs in deduplicated chunks. One
// process owns a store: Sweep coordinates with the store's other users through
// memory, not the filesystem.
type Store struct {
	packs   *packSet
	objects string
	tmp     string
	epochAt string // the file persisting epoch across processes
	// mu orders publication (rename or freshness touch) and root registration
	// against Sweep's deletions: publishers hold it shared, Sweep exclusively.
	mu sync.RWMutex
	// publishMu prevents a concurrent loose clone and manifest from both
	// publishing alternate representations of the same large blob.
	publishMu sync.Mutex
	// epoch names the period since objects were last removed; guarded by mu.
	epoch string
	// holds excludes Sweep while held shared by Hold.
	holds sync.RWMutex
	// ingest excludes Sweep's removals from a chunked ingest, held shared
	// for the whole ingest, since its chunks are stored before the manifest
	// that reaches them. Not mu: a Sweep waiting for mu would block every
	// reader and publisher until a long ingest ends. Not holds: callers may
	// already Hold, and a goroutine must not hold it shared twice. Sweep
	// takes holds, then ingest, then mu.
	ingest sync.RWMutex
	// roots maps trees Build returned to when it did; Sweep keeps what they reach
	// while they are newer than its cutoff, covering work not yet in a caller's roots.
	roots   map[OID]time.Time
	rootsMu sync.Mutex
	// trees holds trees known stored in the current epoch, so Build need not
	// touch them again; Sweep empties it when it starts removing.
	trees treeSet
	// noReflink holds source devices whose files cannot be cloned into the store.
	noReflink sync.Map
	// reflink remembers successful clones, allowing dictionary deltas on that device.
	reflink sync.Map
	// staging holds this process's in-flight staging names so Sweep never
	// removes a file another goroutine is still filling.
	staging sync.Map
	seq     atomic.Uint64
	nonce   string
	cache   *blobCache // nil when off
}

// Open prepares (creating if needed) the object store rooted at dir; cache bounds
// the bytes of small blobs kept in memory (0 disables the cache).
func Open(dir string, cache int64) (*Store, error) {
	s := &Store{objects: filepath.Join(dir, "objects"), tmp: filepath.Join(dir, "tmp"), epochAt: filepath.Join(dir, "epoch"), roots: map[OID]time.Time{}, cache: newBlobCache(cache)}
	if err := os.MkdirAll(s.tmp, 0o755); err != nil {
		return nil, fmt.Errorf("open store %s: %w", dir, err)
	}
	packs, err := openPacks(filepath.Join(dir, "packs"))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", dir, err)
	}
	s.packs = packs
	for i := range 256 {
		if err := os.MkdirAll(filepath.Join(s.objects, fmt.Sprintf("%02x", i)), 0o755); err != nil {
			return nil, fmt.Errorf("open store %s: %w", dir, err)
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("open store %s: %w", dir, err)
	}
	s.nonce = strconv.Itoa(os.Getpid()) + "-" + hex.EncodeToString(b[:])
	if data, err := os.ReadFile(s.epochAt); err == nil && len(data) == 32 {
		s.epoch = string(data)
	} else if err := s.renewEpoch(); err != nil {
		return nil, fmt.Errorf("open store %s: %w", dir, err)
	}
	return s, nil
}

// Epoch names the period since objects were last removed: an object known stored
// while Epoch returned a value is still stored while it returns the same value.
// A store whose epoch file is lost starts a new epoch, invalidating that knowledge.
func (s *Store) Epoch() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.epoch
}

// Hold keeps Sweep from removing objects until release is called, so objects
// found stored meanwhile stay stored, and the epoch stays the same; it waits for
// a running or waiting Sweep first. A goroutine must not Hold twice at once.
func (s *Store) Hold() (release func()) {
	s.holds.RLock()
	return sync.OnceFunc(s.holds.RUnlock)
}

// renewEpoch starts a new epoch, durably before any removal it announces; s.mu
// must be held exclusively (or the store not yet shared).
func (s *Store) renewEpoch() error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	epoch := hex.EncodeToString(b[:])
	tmp := s.epochAt + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(epoch)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, s.epochAt)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(s.epochAt))
	s.epoch = epoch
	return nil
}

// HashBytes returns the git id of data as an object of kind ("blob" or "tree").
func HashBytes(kind string, data []byte) OID {
	h := newHash(kind, int64(len(data)))
	h.Write(data)
	return sumOID(h)
}

// HashFile computes the blob id of a regular file's content by streaming it (no store write).
// It fails rather than return a torn id if the file's length changes while it is read.
func HashFile(path string) (OID, int64, error) {
	f, size, _, err := openRegular(path)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	defer f.Close()
	id, size, err := hashOpen(f, size)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", path, err)
	}
	return id, size, nil
}

// newHash starts a git object hash by writing its "<kind> <size>\x00" header.
func newHash(kind string, size int64) hash.Hash {
	h := sha1.New()
	h.Write([]byte(kind + " " + strconv.FormatInt(size, 10) + "\x00"))
	return h
}

// sumOID finishes a hash as an object id.
func sumOID(h hash.Hash) OID { return OID(hex.EncodeToString(h.Sum(nil))) }

// hashOpen streams an open regular file into a blob id, requiring exactly the size fstat reported.
func hashOpen(f *os.File, size int64) (OID, int64, error) {
	h := newHash("blob", size)
	n, err := io.Copy(h, io.NewSectionReader(f, 0, size))
	if err != nil {
		return "", 0, err
	}
	var extra [1]byte
	if k, _ := f.ReadAt(extra[:], size); n != size || k != 0 {
		return "", 0, errors.New("file changed size while hashing")
	}
	return sumOID(h), size, nil
}

// objectPath is the loose file holding id; callers validate id first.
func (s *Store) objectPath(id OID) string {
	return filepath.Join(s.objects, string(id[:2]), string(id[2:]))
}

// Reader is an open stored object.
type Reader interface {
	io.ReadSeekCloser
	io.ReaderAt
}

// memReader serves in-memory object content as a Reader.
type memReader struct{ *bytes.Reader }

// Close releases nothing: the bytes are the reader's own or the cache's.
func (memReader) Close() error { return nil }

// Open returns the content of stored object id: an *os.File for a loose
// object, which callers may clone, an in-memory reader for a packed or
// resident one. Views and peers call it to serve object content.
func (s *Store) Open(id OID) (Reader, error) {
	if !id.Valid() {
		return nil, fmt.Errorf("open object %q: malformed id", id)
	}
	if data, ok := s.cache.get(id); ok {
		return memReader{bytes.NewReader(data)}, nil
	}
	if e, ok := s.packs.lookup(rawOf(id)); ok && e.flags&frameManifest != 0 {
		return s.openChunks(rawOf(id))
	}
	data, ok, err := s.readPacked(rawOf(id))
	if err != nil {
		return nil, fmt.Errorf("open object %s: %w", id, err)
	}
	if ok {
		return memReader{bytes.NewReader(data)}, nil
	}
	f, err := hostfile.Open(s.objectPath(id))
	if err != nil {
		return nil, fmt.Errorf("open object %s: %w", id, err)
	}
	return f, nil
}

// Size returns the content length of stored object id without reading it, for
// callers that announce sizes (peer offers) before serving content.
func (s *Store) Size(id OID) (int64, error) {
	if !id.Valid() {
		return 0, fmt.Errorf("size of object %q: malformed id", id)
	}
	if e, ok := s.packs.lookup(rawOf(id)); ok {
		if e.flags&frameManifest != 0 {
			current, frame, found, err := s.packs.frame(rawOf(id))
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, fs.ErrNotExist
			}
			if current.flags&frameManifest == 0 {
				return int64(current.raw), nil
			}
			_, size, err := parseManifest(frame[frameHeader:])
			return size, err
		}
		return int64(e.raw), nil
	}
	info, err := os.Lstat(s.objectPath(id))
	if err != nil {
		return 0, fmt.Errorf("size of object %s: %w", id, err)
	}
	return info.Size(), nil
}

// Has reports whether the object id is present in the store.
func (s *Store) Has(id OID) bool {
	if !id.Valid() {
		return false
	}
	if _, ok := s.packs.lookup(rawOf(id)); ok {
		return true
	}
	_, err := os.Lstat(s.objectPath(id))
	return err == nil
}

// ReadBlob returns the content of the stored object id in a slice the caller owns.
func (s *Store) ReadBlob(id OID) ([]byte, error) {
	if !id.Valid() {
		return nil, fmt.Errorf("read object %q: malformed id", id)
	}
	if data, ok := s.cache.get(id); ok {
		return slices.Clone(data), nil
	}
	data, err := s.load(id)
	if err != nil {
		return nil, fmt.Errorf("read object %s: %w", id, err)
	}
	return data, nil
}

// CloneBlob materializes a stored blob at the new path dst with mode: a loose
// object is cloned (else copied), a packed one written out.
func (s *Store) CloneBlob(id OID, dst string, mode os.FileMode) error {
	if !id.Valid() {
		return fmt.Errorf("clone object %q: malformed id", id)
	}
	if e, ok := s.packs.lookup(rawOf(id)); ok && e.flags&frameManifest != 0 {
		return s.cloneChunks(id, dst, mode)
	}
	data, ok, err := s.readPacked(rawOf(id))
	if err != nil {
		return fmt.Errorf("clone object %s: %w", id, err)
	}
	if ok {
		if err := writeNew(dst, data, mode); err != nil {
			return fmt.Errorf("clone object %s: %w", id, err)
		}
		return nil
	}
	in, err := os.Open(s.objectPath(id))
	if err != nil {
		return fmt.Errorf("clone object %s: %w", id, err)
	}
	defer in.Close()
	if err := clone.FD(in, dst, mode); err != nil {
		return fmt.Errorf("clone object %s: %w", id, err)
	}
	return nil
}

// writeNew creates the new file dst holding data with exactly mode (the umask
// may narrow it at creation), removing it again on failure.
func writeNew(dst string, data []byte, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	if err == nil {
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

// cloneFile is the largest object kept in a pack, and so the largest file
// PutFile reads into memory, writing it only if the object is new. Above it
// PutFile stages a block-sharing clone where it can: the clone saves disk
// space, and from about this size it costs no more time than a copy (measured
// on APFS and btrfs; below it a copy is up to 2x faster).
const cloneFile = 1 << 20

// PutFile stores a regular file as a blob and returns its id and size. It opens
// path once and reads a file of up to cloneFile bytes into memory, packing it
// only if the object is new. Larger files use a block-sharing loose clone,
// hashed afterwards, where supported; otherwise a private staged copy is
// hashed and streamed into content-defined chunks. The stored
// bytes are exactly the hashed bytes even if path changes concurrently; the size
// returned is theirs, so it differs from the file's when the file changed while
// it was read.
// A symlink at path is refused rather than followed.
func (s *Store) PutFile(path string) (OID, int64, error) {
	return s.putFile(path, false)
}

// putFile preserves chunked history when chunks is true, even if a new source
// happens to support reflinks; ordinary PutFile keeps its clone fast path.
func (s *Store) putFile(path string, chunks bool) (OID, int64, error) {
	in, size, dev, err := openRegular(path)
	if err != nil {
		return "", 0, fmt.Errorf("store %s: %w", path, err)
	}
	defer in.Close()
	if size <= cloneFile {
		bp := smallBuffers.Get().(*[]byte)
		defer smallBuffers.Put(bp)
		data, whole, err := readSmall(in, *bp)
		if err != nil {
			return "", 0, fmt.Errorf("store %s: %w", path, err)
		}
		if whole {
			id := HashBytes("blob", data)
			if err := s.putObject(id, data, nil, false); err != nil {
				return "", 0, fmt.Errorf("store %s: %w", path, err)
			}
			return id, int64(len(data)), nil
		}
		// The file grew past the buffer while it was read: stage it instead.
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			return "", 0, fmt.Errorf("store %s: %w", path, err)
		}
	}
	stage := s.stageName()
	defer s.staging.Delete(stage)
	var id OID
	err = errors.ErrUnsupported
	if _, no := s.noReflink.Load(dev); !chunks && !no && size > cloneFile {
		id, size, err = stageClone(in, stage)
		if err == nil {
			s.reflink.Store(dev, struct{}{})
		}
		if errors.Is(err, errors.ErrUnsupported) {
			s.noReflink.Store(dev, struct{}{})
		}
	}
	copied := errors.Is(err, errors.ErrUnsupported)
	if copied {
		id, size, err = stageCopy(in, size, stage)
	}
	if err != nil {
		removeFile(stage)
		return "", 0, fmt.Errorf("store %s: %w", path, err)
	}
	if copied && size > cloneFile {
		err = s.chunkStage(id, stage)
	} else {
		err = s.publish(id, stage, nil, false)
	}
	if err != nil {
		return "", 0, fmt.Errorf("store %s: %w", path, err)
	}
	return id, size, nil
}

// stageClone clones in to the new staging file stage, dated now, and hashes the
// clone; errors.ErrUnsupported means stage was not created.
func stageClone(in *os.File, stage string) (OID, int64, error) {
	if err := clone.Reflink(in, stage, objectPerm); err != nil {
		return "", 0, err
	}
	// Clones keep the source's mtime; the object's age must reflect when it was stored.
	now := time.Now()
	if err := os.Chtimes(stage, now, now); err != nil {
		return "", 0, err
	}
	staged, err := os.Open(stage)
	if err != nil {
		return "", 0, err
	}
	defer staged.Close()
	info, err := staged.Stat()
	if err != nil {
		return "", 0, err
	}
	return hashOpen(staged, info.Size())
}

// readSmall reads in to its end into buf, reporting whether the whole file fit.
func readSmall(in *os.File, buf []byte) ([]byte, bool, error) {
	n := 0
	for n < len(buf) {
		k, err := in.Read(buf[n:])
		n += k
		if err == io.EOF {
			return buf[:n], true, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

// smallBuffers recycles PutFile's whole-file buffers, one byte longer than
// cloneFile so a file that grew past it is noticed.
var smallBuffers = sync.Pool{New: func() any { b := make([]byte, cloneFile+1); return &b }}

// copyBuffers recycles stageCopy's read buffers.
var copyBuffers = sync.Pool{New: func() any { b := make([]byte, 128<<10); return &b }}

// stageCopy reads in to its end once, writing the new staging file stage and
// hashing as a blob of size bytes. When the read count differs (the file changed
// while it was read), the staged bytes are hashed again under their own size.
func stageCopy(in *os.File, size int64, stage string) (OID, int64, error) {
	out, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, objectPerm)
	if err != nil {
		return "", 0, err
	}
	h := newHash("blob", size)
	bp := copyBuffers.Get().(*[]byte)
	defer copyBuffers.Put(bp)
	buf := *bp
	var n int64
	for {
		k, readErr := in.Read(buf)
		if k > 0 {
			if _, err = out.Write(buf[:k]); err != nil {
				break
			}
			h.Write(buf[:k])
			n += int64(k)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			err = readErr
			break
		}
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	if n == size {
		return sumOID(h), n, nil
	}
	staged, err := os.Open(stage)
	if err != nil {
		return "", 0, err
	}
	defer staged.Close()
	return hashOpen(staged, n)
}

// PutBytes stores data as a blob and returns its id; a small blob also becomes
// resident in the cache (copied, so the caller may reuse data).
func (s *Store) PutBytes(data []byte) (OID, error) {
	id, err := s.putData("blob", HashBytes("blob", data), data)
	if err != nil {
		return "", fmt.Errorf("store blob: %w", err)
	}
	return id, nil
}

// putData stores an in-memory object body of kind under its id; blobs are
// admitted to the cache, trees remembered as stored for Build.
func (s *Store) putData(kind string, id OID, data []byte) (OID, error) {
	var keep []byte
	if kind == "blob" && s.cache.fits(int64(len(data))) {
		keep = slices.Clone(data)
	}
	if err := s.putObject(id, data, keep, kind == "tree"); err != nil {
		return "", err
	}
	return id, nil
}

// putObject stores data as object id unless it exists, which is then refreshed;
// keep and tree are recorded as publish does. Data of up to cloneFile bytes is
// packed, compressed before any lock is taken.
func (s *Store) putObject(id OID, data, keep []byte, tree bool) error {
	if s.touch(id, keep, tree) {
		return nil
	}
	if len(data) <= cloneFile {
		return s.putFrame(id, encodeFrame(rawOf(id), data, time.Now().UnixNano(), 0), keep, tree)
	}
	if !tree {
		return s.putChunks(id, bytes.NewReader(data))
	}
	stage := s.stageNear(id)
	defer s.staging.Delete(stage)
	if err := writeExclusive(stage, data); err != nil {
		removeFile(stage)
		return err
	}
	return s.publish(id, stage, keep, tree)
}

// putFrame packs frame, encoded for object id, unless the object is packed
// already, which is then refreshed; keep and tree are recorded as publish does.
func (s *Store) putFrame(id OID, frame, keep []byte, tree bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.packs.add(frame); err != nil {
		return err
	}
	s.stored(id, keep, tree)
	return nil
}

// writeExclusive creates the new read-only object file p holding data; the
// creating handle may still write it.
func writeExclusive(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, objectPerm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// stageName returns a fresh staging path unique across goroutines and processes, registered against Sweep.
func (s *Store) stageName() string {
	p := filepath.Join(s.tmp, s.nonce+"-"+strconv.FormatUint(s.seq.Add(1), 10))
	s.staging.Store(p, struct{}{})
	return p
}

// stagePrefix starts the names of staging files kept beside objects; no object name has it.
const stagePrefix = "tmp-"

// stageNear returns a fresh staging path, registered against Sweep, in the
// directory that will hold object id. Creating files spread over the fan-out
// directories and renaming within one is several times faster on NTFS than
// funnelling every write through one staging directory.
func (s *Store) stageNear(id OID) string {
	p := filepath.Join(s.objects, string(id[:2]), stagePrefix+s.nonce+"-"+strconv.FormatUint(s.seq.Add(1), 10))
	s.staging.Store(p, struct{}{})
	return p
}

// publish moves a complete read-only staging file, dated now, into place as
// object id, or discards it and refreshes the object's mtime when the object
// already exists, so a concurrent Sweep treats it as fresh either way. A non-nil
// keep, which the cache then owns, becomes the resident body once the object is
// in place; a tree is remembered as stored.
func (s *Store) publish(id OID, stage string, keep []byte, tree bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if s.packs.touch(rawOf(id), time.Now().UnixNano()) {
		removeFile(stage)
		s.stored(id, keep, tree)
		return nil
	}
	dst := s.objectPath(id)
	exists, err := renameNoReplace(stage, dst)
	if err != nil {
		removeFile(stage)
		return err
	}
	if exists {
		removeFile(stage)
		return s.refresh(id, dst, keep, tree)
	}
	s.stored(id, keep, tree)
	return nil
}

// renameChecked renames old to new unless new exists, reporting whether it did;
// for filesystems without an atomic no-replace rename. Two writers may both
// rename the same object: its content is identical either way.
func renameChecked(old, new string) (bool, error) {
	if _, err := os.Lstat(new); err == nil {
		return true, nil
	}
	if err := os.Rename(old, new); err != nil {
		// Windows refuses to replace a read-only file another writer just published.
		if _, statErr := os.Lstat(new); statErr == nil {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

// refresh marks the existing object dst fresh and, on success, records it as
// stored; s.mu must be held shared.
func (s *Store) refresh(id OID, dst string, keep []byte, tree bool) error {
	now := time.Now()
	if err := os.Chtimes(dst, now, now); err != nil {
		return err
	}
	s.stored(id, keep, tree)
	return nil
}

// stored makes keep resident and remembers a tree as stored in the current
// epoch; s.mu must be held shared since the object's existence was checked.
func (s *Store) stored(id OID, keep []byte, tree bool) {
	if keep != nil {
		s.cache.add(id, keep)
	}
	if tree {
		s.trees.add(rawOf(id))
	}
}

// touch refreshes an existing object's freshness (in memory for a packed one,
// its mtime for a loose one) so Sweep keeps it, reporting whether it exists;
// when it does, it is recorded as stored.
func (s *Store) touch(id OID, keep []byte, tree bool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.packs.touch(rawOf(id), time.Now().UnixNano()) {
		s.stored(id, keep, tree)
		return true
	}
	return s.refresh(id, s.objectPath(id), keep, tree) == nil
}

// knownTree reports a tree stored during the current epoch, so it is still stored
// unless the epoch has changed since.
func (s *Store) knownTree(id OID) bool { return s.trees.has(rawOf(id)) }

// treeGeneration is the number of trees a treeSet generation holds; the set
// remembers between one and two generations, the most recently stored.
const treeGeneration = 1 << 15

// treeSet remembers recently stored trees within a fixed bound: a full
// generation becomes the old one, dropping the generation before. Forgetting
// a tree only costs Build a freshness touch.
type treeSet struct {
	mu       sync.Mutex
	cur, old map[rawID]struct{}
}

// add remembers id.
func (t *treeSet) add(id rawID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cur == nil {
		t.cur = map[rawID]struct{}{}
	}
	t.cur[id] = struct{}{}
	if len(t.cur) >= treeGeneration {
		t.old, t.cur = t.cur, nil
	}
}

// has reports whether id is remembered.
func (t *treeSet) has(id rawID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, cur := t.cur[id]
	_, old := t.old[id]
	return cur || old
}

// clear forgets every tree.
func (t *treeSet) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cur, t.old = nil, nil
}

// register records root as just built and so kept by Sweep, unless the epoch is
// no longer epoch: objects found stored during it may since have been removed.
func (s *Store) register(root OID, epoch string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.epoch != epoch {
		return false
	}
	s.rootsMu.Lock()
	s.roots[root] = time.Now()
	s.rootsMu.Unlock()
	return true
}

// removeFile deletes p, clearing a read-only attribute (which blocks deletion on Windows) if needed.
func removeFile(p string) error {
	err := os.Remove(p)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if os.Chmod(p, 0o644) == nil {
		err = os.Remove(p)
	}
	return err
}
