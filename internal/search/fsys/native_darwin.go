//go:build darwin

package fsys

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin directory reads:
//
//   - Children open with openat(parent, name): one-component lookups instead
//     of re-resolving the whole path from the root on every directory.
//   - Minimal listings use __getdirentries64 with a buffer of at least
//     GETDIRENTRIES64_EXTENDED_BUFSIZE; the kernel then stores an EOF flag in
//     the buffer's last four bytes, so a directory that fits in the buffer
//     takes exactly one listing call instead of a second call returning 0.
//     On APFS this also beats getattrlistbulk(NAME|OBJTYPE) by ~25%.
//   - Full listings use getattrlistbulk, which returns names, types, mtimes,
//     and sizes without per-entry stat calls. The parent listing also returns
//     each subdirectory's entry count, so the child listing stops once it has
//     seen that many entries rather than issuing a final empty call.
//   - ReadDir (metadataDetail) uses the same getattrlistbulk loop and buffer
//     with the attributes of a complete stat record, so a regular file's
//     lstat data arrives with its name; every other entry is described by
//     fstatat relative to the directory descriptor.

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var (
	libc_openat_trampoline_addr          uintptr
	libc_getdirentries64_trampoline_addr uintptr
	libc_getattrlistbulk_trampoline_addr uintptr
	libc_fstatat_trampoline_addr         uintptr
)

//go:cgo_import_dynamic libc_openat openat "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_getdirentries64 __getdirentries64 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_getattrlistbulk getattrlistbulk "/usr/lib/libSystem.B.dylib"

const (
	listBufSize = 128 << 10

	// defaultWorkers caps the default worker count. Directory opens and
	// listings contend in the kernel: on APFS throughput peaks at 4–6
	// threads and collapses beyond 8 (16 threads run ~1.7× slower than 4).
	defaultWorkers = 4

	// struct dirent (64-bit inode layout) field offsets.
	direntIno    = 0
	direntReclen = 16
	direntNamlen = 18
	direntType   = 20
	direntName   = 21

	getdirentries64EOF = 1

	attrBitMapCount     = 5
	attrCmnName         = 0x00000001
	attrCmnDevID        = 0x00000002
	attrCmnObjType      = 0x00000008
	attrCmnCrTime       = 0x00000200
	attrCmnModTime      = 0x00000400
	attrCmnChgTime      = 0x00000800
	attrCmnAccTime      = 0x00001000
	attrCmnOwnerID      = 0x00008000
	attrCmnGrpID        = 0x00010000
	attrCmnAccessMask   = 0x00020000
	attrCmnFlags        = 0x00040000
	attrCmnFileID       = 0x02000000
	attrCmnReturnedAttr = 0x80000000
	attrDirEntryCount   = 0x00000002
	attrFileLinkCount   = 0x00000001
	attrFileAllocSize   = 0x00000004
	attrFileIOBlkSize   = 0x00000008
	attrFileDataLength  = 0x00000200
	fsoptNoFollow       = 0x00000001

	// getattrlistbulk record layout: u32 length, the 20-byte returned
	// attribute_set_t (common, vol, dir, file, fork), then each returned
	// attribute in bitmap order, 4-byte aligned. getattrlistbulk rejects
	// requests without ATTR_CMN_RETURNED_ATTRS (EINVAL) and omits attributes
	// that do not apply to an entry, so later offsets depend on the set.
	bulkReturnedCommon = 4
	bulkReturnedDir    = 12
	bulkReturnedFile   = 16
	bulkNameRef        = 24
	bulkObjType        = 32
	bulkModTime        = 36
	bulkMinLen         = 52

	// <sys/vnode.h> vtype values of ATTR_CMN_OBJTYPE.
	vreg  = 1
	vdir  = 2
	vblk  = 3
	vchr  = 4
	vlnk  = 5
	vsock = 6
	vfifo = 7

	// st_flags bits of files whose lstat size the filesystem or decmpfs may
	// report differently to getattrlist: transparently compressed files
	// (UF_COMPRESSED) and dataless placeholders (SF_DATALESS).
	ufCompressed = 0x00000020
	sfDataless   = 0x40000000

	// st_mode bits getattrlist reports: the permissions and the setuid,
	// setgid and sticky bits (the file type comes from ATTR_CMN_OBJTYPE).
	modeBits = 0o7777
)

// The attributes of a complete stat record. In a record they are packed in
// ascending bit order, common attributes before file attributes, after the
// returned attribute set and the name reference, with 4-byte alignment.
const (
	metaCommon = attrCmnName | attrCmnDevID | attrCmnObjType | attrCmnCrTime | attrCmnModTime |
		attrCmnChgTime | attrCmnAccTime | attrCmnOwnerID | attrCmnGrpID | attrCmnAccessMask |
		attrCmnFlags | attrCmnFileID
	metaFile = attrFileLinkCount | attrFileAllocSize | attrFileIOBlkSize | attrFileDataLength
)

// metaLayout is where a complete stat record holds each attribute.
type metaLayout struct {
	devID, objType, crTime, modTime, chgTime, accTime int
	owner, group, access, flags, fileID               int
	linkCount, allocSize, ioBlkSize, dataLen          int
	end                                               int // first byte after the fixed attributes
}

var metaOff = func() (l metaLayout) {
	cursor := bulkNameRef + 8 // after the name reference, the first common attribute
	take := func(size int) int {
		at := cursor
		cursor += size
		return at
	}
	l.devID, l.objType = take(4), take(4)
	l.crTime, l.modTime, l.chgTime, l.accTime = take(16), take(16), take(16), take(16)
	l.owner, l.group, l.access, l.flags = take(4), take(4), take(4), take(4)
	l.fileID = take(8)
	l.linkCount, l.allocSize, l.ioBlkSize, l.dataLen = take(4), take(8), take(4), take(8)
	l.end = cursor
	return l
}()

// isRoot reports whether the process is the superuser, to whom lstat also
// reports st_gen, which getattrlist cannot.
var isRoot = os.Geteuid() == 0

type attrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

var fullAttrs = attrList{
	bitmapCount: attrBitMapCount,
	commonAttr:  attrCmnReturnedAttr | attrCmnName | attrCmnObjType | attrCmnModTime,
	dirAttr:     attrDirEntryCount,
	fileAttr:    attrFileDataLength,
}

// metadataAttrs requests a complete stat record: every field of a regular
// file's lstat data. ATTR_CMN_GEN_COUNT is a different counter from st_gen
// and is not requested.
var metadataAttrs = attrList{
	bitmapCount: attrBitMapCount,
	commonAttr:  attrCmnReturnedAttr | metaCommon,
	fileAttr:    metaFile,
}

func openat(dir int, cname []byte, flags int) (int, error) {
	for {
		r, _, e := syscall_syscall6(libc_openat_trampoline_addr, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), uintptr(flags), 0, 0, 0)
		switch e {
		case 0:
			return int(r), nil
		case unix.EINTR:
			continue
		}
		return -1, e
	}
}

// list reads every entry of h into s.ls. count is the directory's entry
// count from its parent's Full listing (0 when unknown).
func (s *session) list(h handle, detail Detail, count uint32) error {
	if len(s.buf) < listBufSize {
		s.buf = make([]byte, listBufSize)
	}
	if detail == Full || detail == metadataDetail {
		return s.listBulk(h, detail, count)
	}
	return s.listDirents(h)
}

func (s *session) listDirents(h handle) error {
	buf := s.buf
	var base int64
	for {
		r, _, e := syscall_syscall6(libc_getdirentries64_trampoline_addr, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&base)), 0, 0)
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		n := int(r)
		if n <= 0 {
			return nil
		}
		for off := 0; off+direntName <= n; {
			rec := buf[off:n]
			reclen := int(binary.LittleEndian.Uint16(rec[direntReclen:]))
			if reclen < direntName || reclen > len(rec) {
				return unix.EIO
			}
			off += reclen
			namlen := int(binary.LittleEndian.Uint16(rec[direntNamlen:]))
			if direntName+namlen > reclen || binary.LittleEndian.Uint64(rec[direntIno:]) == 0 {
				continue
			}
			name := rec[direntName : direntName+namlen]
			if isDotOrDotDot(name) {
				continue
			}
			typ := typeFromDT(rec[direntType])
			if typ == 0 {
				if rec[direntType] != unix.DT_UNKNOWN {
					continue
				}
				if typ = statType(h, name); typ == 0 {
					continue
				}
			}
			s.ls.add(name, typ, -1, 0, 0)
		}
		if binary.LittleEndian.Uint32(buf[len(buf)-4:])&getdirentries64EOF != 0 {
			return nil
		}
	}
}

// listBulk reads h with getattrlistbulk: compact Full records into s.ls, or,
// for metadataDetail, complete stat entries into s.entries.
func (s *session) listBulk(h handle, detail Detail, count uint32) error {
	buf := s.buf
	attrs := &fullAttrs
	var slab []nativeInfo
	if detail == metadataDetail {
		attrs = &metadataAttrs
	}
	seen := uint32(0)
	for {
		r, _, e := syscall_syscall6(libc_getattrlistbulk_trampoline_addr, uintptr(h), uintptr(unsafe.Pointer(attrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fsoptNoFollow, 0)
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		n := int(r)
		if n <= 0 {
			return nil
		}
		if n > len(buf)/bulkMinLen {
			return unix.EIO
		}
		var names strings.Builder
		if detail == metadataDetail {
			nameBytes, entries, err := metadataBatchSize(buf, n)
			if err != nil {
				return err
			}
			// Own only the names, not the much larger stat records in buf.
			// Exact growth keeps even the last short batch compact.
			names.Grow(nameBytes)
			slab = make([]nativeInfo, 0, entries)
			s.entries = slices.Grow(s.entries, entries)
		}
		off := 0
		for range n {
			if off+bulkMinLen > len(buf) {
				return unix.EIO
			}
			rec := buf[off:]
			reclen := int(binary.LittleEndian.Uint32(rec))
			if reclen < bulkMinLen || reclen > len(rec) {
				return unix.EIO
			}
			rec = rec[:reclen]
			off += reclen
			if detail == metadataDetail {
				entry, err := s.metadataEntry(h, rec, &slab, &names)
				if err != nil {
					return err
				}
				if entry != nil {
					s.entries = append(s.entries, entry)
				}
				continue
			}
			const wantCommon = attrCmnName | attrCmnObjType | attrCmnModTime
			if binary.LittleEndian.Uint32(rec[bulkReturnedCommon:])&wantCommon != wantCommon {
				continue
			}
			name, nameOff, ok := recordName(rec, bulkMinLen)
			if !ok {
				return unix.EIO
			}
			if len(name) == 0 || isDotOrDotDot(name) {
				continue
			}
			var typ Type
			switch binary.LittleEndian.Uint32(rec[bulkObjType:]) {
			case vreg:
				typ = File
			case vdir:
				typ = Dir
			case vlnk:
				typ = Symlink
			default:
				continue
			}
			sec := int64(binary.LittleEndian.Uint64(rec[bulkModTime:]))
			nsec := int64(binary.LittleEndian.Uint64(rec[bulkModTime+8:]))
			size := int64(-1)
			var children uint32
			cursor := bulkMinLen
			if binary.LittleEndian.Uint32(rec[bulkReturnedDir:])&attrDirEntryCount != 0 && cursor+4 <= nameOff {
				children = binary.LittleEndian.Uint32(rec[cursor:])
				cursor += 4
			}
			if binary.LittleEndian.Uint32(rec[bulkReturnedFile:])&attrFileDataLength != 0 && cursor+8 <= nameOff && typ == File {
				size = int64(binary.LittleEndian.Uint64(rec[cursor:]))
			}
			s.ls.add(name, typ, size, nanos(sec, nsec), children)
		}
		seen += uint32(n)
		if count > 0 && seen >= count {
			return nil
		}
	}
}

// Stat allocates nothing, unlike unix.Fstatat.
func (s *session) Stat(path string, follow bool) (FileInfo, error) {
	flags := uintptr(unix.AT_SYMLINK_NOFOLLOW)
	if follow {
		flags = 0
	}
	cname, err := s.cstr(path)
	if err != nil {
		return FileInfo{}, err
	}
	cwd := unix.AT_FDCWD
	var st unix.Stat_t
	for {
		_, _, e := syscall_syscall6(libc_fstatat_trampoline_addr, uintptr(cwd), uintptr(unsafe.Pointer(&cname[0])), uintptr(unsafe.Pointer(&st)), flags, 0, 0)
		switch e {
		case 0:
			return FileInfo{Type: typeFromMode(uint32(st.Mode)), Size: st.Size, Stamp: stampOf(&st)}, nil
		case unix.EINTR:
			continue
		}
		return FileInfo{}, e
	}
}

// statHandle stamps an open descriptor and reports its type and size.
func statHandle(h handle) (Stamp, Type, int64, error) {
	var st unix.Stat_t
	err := unix.Fstat(h, &st)
	return stampOf(&st), typeFromMode(uint32(st.Mode)), st.Size, err
}

// statChange is st's status change time in Unix nanoseconds.
func statChange(st *syscall.Stat_t) int64 { return nanos(st.Ctimespec.Sec, st.Ctimespec.Nsec) }

func stampOf(st *unix.Stat_t) Stamp {
	return Stamp{Dev: uint64(uint32(st.Dev)), Ino: st.Ino, Mtime: nanos(st.Mtim.Sec, st.Mtim.Nsec), Ctime: nanos(st.Ctim.Sec, st.Ctim.Nsec)}
}

// fineTimestamps admits APFS. HFS+ keeps 1-second timestamps; network and
// FUSE filesystems may not update directory times promptly.
func fineTimestamps(h handle) bool {
	var fs unix.Statfs_t
	return unix.Fstatfs(h, &fs) == nil && unix.ByteSliceToString(fs.Fstypename[:]) == "apfs"
}

// statType resolves an entry whose listing reported DT_UNKNOWN.
func statType(dir handle, name []byte) Type {
	var st unix.Stat_t
	if unix.Fstatat(dir, string(name), &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return 0
	}
	return typeFromMode(uint32(st.Mode))
}

// recordName returns the name of bulk record rec, without its terminator, and
// where the name's bytes start in rec, which must be at least minOff, past the
// fixed attributes. ok is false for a name outside the record.
func recordName(rec []byte, minOff int) (name []byte, nameOff int, ok bool) {
	nameOff = bulkNameRef + int(int32(binary.LittleEndian.Uint32(rec[bulkNameRef:])))
	nameLen := int(binary.LittleEndian.Uint32(rec[bulkNameRef+4:]))
	if nameOff < minOff || nameLen == 0 || nameOff+nameLen > len(rec) {
		return nil, 0, false
	}
	name = rec[nameOff : nameOff+nameLen]
	if name[len(name)-1] == 0 {
		name = name[:len(name)-1]
	}
	return name, nameOff, true
}

// metadataBatchSize validates the names before sizing their immutable arena.
// Only real entries contribute storage; "." and ".." are never returned.
func metadataBatchSize(buf []byte, n int) (nameBytes, entries int, err error) {
	for off := 0; n > 0; n-- {
		if off+bulkMinLen > len(buf) {
			return 0, 0, unix.EIO
		}
		rec := buf[off:]
		reclen := int(binary.LittleEndian.Uint32(rec))
		if reclen < bulkMinLen || reclen > len(rec) {
			return 0, 0, unix.EIO
		}
		rec = rec[:reclen]
		off += reclen
		if binary.LittleEndian.Uint32(rec[bulkReturnedCommon:])&attrCmnName == 0 {
			return 0, 0, unix.EIO
		}
		name, _, ok := recordName(rec, bulkNameRef+8)
		if !ok {
			return 0, 0, unix.EIO
		}
		if len(name) != 0 && !isDotOrDotDot(name) {
			nameBytes += len(name)
			entries++
		}
	}
	return nameBytes, entries, nil
}

// metadataEntry describes the entry of bulk record rec in directory dir. A
// regular file whose record holds a complete stat record is described by it;
// anything else is lstat'ed relative to dir, and one that fails is a
// failedEntry. A nil entry is "." or "..".
func (s *session) metadataEntry(dir handle, rec []byte, slab *[]nativeInfo, names *strings.Builder) (os.DirEntry, error) {
	if binary.LittleEndian.Uint32(rec[bulkReturnedCommon:])&attrCmnName == 0 {
		return nil, unix.EIO
	}
	raw, nameOff, ok := recordName(rec, bulkNameRef+8)
	if !ok {
		return nil, unix.EIO
	}
	if len(raw) == 0 || isDotOrDotDot(raw) {
		return nil, nil
	}
	info := newInfo(slab)
	vtype, complete := bulkStat(rec, nameOff, &info.st)
	start := names.Len()
	names.Write(raw)
	name := names.String()[start:]
	if !complete {
		cname := append(append(s.cname[:0], raw...), 0)
		s.cname = cname
		if err := fstatat(dir, cname, &info.st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			dropInfo(slab)
			return failedEntry{name: name, typ: vtypeMode(vtype), err: &os.PathError{Op: "lstat", Path: filepath.Join(s.dir, name), Err: err}}, nil
		}
	}
	info.name = name
	info.mode = modeFromStat(uint32(info.st.Mode))
	info.mod = time.Unix(info.st.Mtimespec.Sec, info.st.Mtimespec.Nsec)
	return info, nil
}

// bulkStat decodes bulk record rec, whose name's bytes start at nameOff, into
// st and reports whether it is complete: a regular file carrying every
// attribute of metadataAttrs, which lstat describes identically. It returns
// the entry's vnode type, 0 if the record lacks it. An incomplete record
// leaves st untouched; the entry must be lstat'ed.
func bulkStat(rec []byte, nameOff int, st *syscall.Stat_t) (vtype uint32, complete bool) {
	le := binary.LittleEndian
	common := le.Uint32(rec[bulkReturnedCommon:])
	if common&attrCmnObjType != 0 {
		at := bulkNameRef + 8
		if common&attrCmnDevID != 0 {
			at += 4
		}
		vtype = le.Uint32(rec[at:])
	}
	// The returned set names ATTR_CMN_RETURNED_ATTRS itself too; every other
	// attribute must be exactly the requested ones.
	vol, dirs := le.Uint32(rec[bulkReturnedCommon+4:]), le.Uint32(rec[bulkReturnedDir:])
	file, fork := le.Uint32(rec[bulkReturnedFile:]), le.Uint32(rec[bulkReturnedFile+4:])
	if isRoot || vtype != vreg || common&^attrCmnReturnedAttr != metaCommon || file != metaFile || vol != 0 || dirs != 0 || fork != 0 || nameOff < metaOff.end {
		return vtype, false
	}
	u32 := func(at int) uint32 { return le.Uint32(rec[at:]) }
	ts := func(at int) syscall.Timespec {
		return syscall.Timespec{Sec: int64(le.Uint64(rec[at:])), Nsec: int64(le.Uint64(rec[at+8:]))}
	}
	size := int64(le.Uint64(rec[metaOff.dataLen:]))
	alloc := int64(le.Uint64(rec[metaOff.allocSize:]))
	flags := u32(metaOff.flags)
	if size < 0 || alloc < 0 || flags&(ufCompressed|sfDataless) != 0 {
		return vtype, false
	}
	*st = syscall.Stat_t{
		Dev:           int32(u32(metaOff.devID)),
		Mode:          syscall.S_IFREG | uint16(u32(metaOff.access)&modeBits),
		Nlink:         uint16(u32(metaOff.linkCount)),
		Ino:           le.Uint64(rec[metaOff.fileID:]),
		Uid:           u32(metaOff.owner),
		Gid:           u32(metaOff.group),
		Atimespec:     ts(metaOff.accTime),
		Mtimespec:     ts(metaOff.modTime),
		Ctimespec:     ts(metaOff.chgTime),
		Birthtimespec: ts(metaOff.crTime),
		Size:          size,
		Blocks:        (alloc + 511) / 512,
		Blksize:       int32(u32(metaOff.ioBlkSize)),
		Flags:         flags,
	}
	return vtype, true
}

// vtypeMode maps a vnode type to the Go file type bits; 0 for a regular file or an unknown type.
func vtypeMode(vtype uint32) os.FileMode {
	switch vtype {
	case vdir:
		return os.ModeDir
	case vblk:
		return os.ModeDevice
	case vchr:
		return os.ModeDevice | os.ModeCharDevice
	case vlnk:
		return os.ModeSymlink
	case vsock:
		return os.ModeSocket
	case vfifo:
		return os.ModeNamedPipe
	}
	return 0
}

// fstatat is lstat (with flags AT_SYMLINK_NOFOLLOW) of cname, NUL-terminated,
// relative to directory dir, into st.
func fstatat(dir handle, cname []byte, st *syscall.Stat_t, flags uintptr) error {
	for {
		_, _, e := syscall_syscall6(libc_fstatat_trampoline_addr, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), uintptr(unsafe.Pointer(st)), flags, 0, 0)
		switch e {
		case 0:
			return nil
		case unix.EINTR:
			continue
		}
		return e
	}
}

// readDir lists path with complete lstat metadata; see [ReadDir]. A directory
// the process cannot search is listed by [os.ReadDir]: getattrlistbulk does
// not need search permission, but lstat of its entries does, and their
// failures must show. A listing getattrlistbulk does not support (some
// network and FUSE filesystems) also falls back to it, which reports all
// directory errors.
func (s *session) readDir(path string) ([]os.DirEntry, error) {
	if entries, err := s.readDirNative(path); err == nil {
		return entries, nil
	}
	s.entries = nil
	return os.ReadDir(path)
}

// readDirNative lists path with the native bulk reader alone.
func (s *session) readDirNative(path string) ([]os.DirEntry, error) {
	s.dir = path
	h, err := s.openDir(path)
	if err != nil {
		return nil, err
	}
	defer closeHandle(h)
	// Check the opened directory, not its absolute path again: this avoids
	// a second root-to-leaf lookup and checks the identity we actually list.
	if err := unix.Faccessat(h, ".", unix.X_OK, unix.AT_EACCESS); err != nil {
		return nil, err
	}
	s.entries = nil
	if err := s.list(h, metadataDetail, 0); err != nil {
		s.entries = nil
		return nil, err
	}
	return s.entries, nil
}
