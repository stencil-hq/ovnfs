//go:build linux

package fsys

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux directory reads:
//
//   - Children open with openat(parent, name): one-component lookups instead
//     of resolving the full path from the root for every directory.
//   - Directory and ignore-file opens request O_NOATIME so walking does not
//     dirty inode atimes. The kernel only allows it for the file owner (or
//     CAP_FOWNER); the first EPERM disables it for the rest of the process.
//   - Listings use raw getdents64 into a large reusable buffer, parsed in
//     place; d_type supplies entry types without per-entry stat calls.
//   - Full detail and DT_UNKNOWN entries use statx with only the needed
//     fields (type, size, mtime) and AT_STATX_DONT_SYNC, so network
//     filesystems answer from cached attributes. Kernels or seccomp policies
//     without statx fall back to fstatat.
//
// getdents64 has no end-of-directory flag, and a short read is not proof of
// EOF (the kernel also stops filling early when a signal is pending). The
// end is recognised from the position cookie instead (see list); on
// filesystems without such a cookie (XFS, older tmpfs) a listing ends with
// one empty call.

const (
	listBufSize = 128 << 10

	// defaultWorkers caps the default worker count: on a 16-vCPU arm64 VM,
	// 8 workers walked faster and steadier than 16 (kernel contention on
	// fd-table and dcache locks grows with thread count).
	defaultWorkers = 8

	// struct linux_dirent64 field offsets.
	direntIno    = 0
	direntOff    = 8
	direntReclen = 16
	direntType   = 18
	direntName   = 19

	statxMask  = unix.STATX_TYPE | unix.STATX_SIZE | unix.STATX_MTIME
	statxFlags = unix.AT_SYMLINK_NOFOLLOW | unix.AT_NO_AUTOMOUNT | unix.AT_STATX_DONT_SYNC
)

// noatime holds O_NOATIME until an open fails with EPERM.
var noatime atomic.Int32

// noStatx is set once statx reports ENOSYS or EPERM (seccomp).
var noStatx atomic.Bool

func init() { noatime.Store(unix.O_NOATIME) }

func openat(dir int, cname []byte, flags int) (int, error) {
	for {
		extra := int(noatime.Load())
		r, _, e := unix.Syscall6(unix.SYS_OPENAT, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), uintptr(flags|extra), 0, 0, 0)
		switch e {
		case 0:
			return int(r), nil
		case unix.EINTR:
			continue
		case unix.EPERM:
			if extra != 0 {
				noatime.Store(0)
				continue
			}
		}
		return -1, e
	}
}

// list reads every entry of h into s.ls.
func (s *session) list(h handle, detail Detail, _ uint32) error {
	if len(s.buf) < listBufSize {
		s.buf = make([]byte, listBufSize)
	}
	buf := s.buf
	for {
		r, _, e := unix.Syscall(unix.SYS_GETDENTS64, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
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
		var names strings.Builder
		var slab []nativeInfo
		if detail == metadataDetail {
			nameBytes, entries, err := metadataBatchSize(buf[:n])
			if err != nil {
				return err
			}
			names.Grow(nameBytes)
			slab = make([]nativeInfo, 0, entries)
			s.entries = slices.Grow(s.entries, entries)
		}
		var next int64
		for off := 0; off+direntName <= n; {
			rec := buf[off:n]
			reclen := int(*(*uint16)(unsafe.Pointer(&rec[direntReclen])))
			if reclen < direntName || reclen > len(rec) {
				return unix.EIO
			}
			off += reclen
			next = *(*int64)(unsafe.Pointer(&rec[direntOff]))
			if *(*uint64)(unsafe.Pointer(&rec[direntIno])) == 0 {
				continue
			}
			// The name must be NUL-terminated inside its record: the
			// terminator lets statx/fstatat read it in place.
			i := bytes.IndexByte(rec[direntName:reclen], 0)
			if i < 0 {
				return unix.EIO
			}
			cname := rec[direntName : direntName+i+1 : direntName+i+1]
			name := cname[:i]
			if i == 0 || isDotOrDotDot(name) {
				continue
			}
			dt := rec[direntType]
			if detail == metadataDetail {
				start := names.Len()
				names.Write(name)
				s.entries = append(s.entries, s.metadataEntry(h, cname, dt, names.String()[start:], &slab))
				continue
			}
			typ := typeFromDT(dt)
			if typ == 0 && dt != unix.DT_UNKNOWN {
				continue
			}
			size, mtime := int64(-1), int64(0)
			if detail == Full || typ == 0 {
				var ok bool
				if typ, size, mtime, ok = s.stat(h, cname); !ok {
					continue
				}
			}
			s.ls.add(name, typ, size, mtime, 0)
		}
		// The kernel stores the directory position after the batch in the
		// last entry's d_off. ext4 (htree), Btrfs, tmpfs, and overlayfs
		// move it to their end-of-directory cookie (INT32_MAX/INT64_MAX)
		// once everything was returned, so the confirming empty call can be
		// skipped. A batch cut short (buffer full, signal pending) carries
		// the next real position instead.
		if next == math.MaxInt32 || next == math.MaxInt64 {
			return nil
		}
	}
}

const stampMask = unix.STATX_TYPE | unix.STATX_INO | unix.STATX_SIZE | unix.STATX_MTIME | unix.STATX_CTIME

// emptyPath is the "" path statx needs with AT_EMPTY_PATH.
var emptyPath = []byte{0}

func (s *session) Stat(path string, follow bool) (FileInfo, error) {
	flags := unix.AT_SYMLINK_NOFOLLOW
	if follow {
		flags = 0
	}
	cpath, err := s.cstr(path)
	if err != nil {
		return FileInfo{}, err
	}
	st, typ, size, err := statStamp(unix.AT_FDCWD, cpath, flags)
	return FileInfo{Type: typ, Size: size, Stamp: st}, err
}

// statHandle stamps an open descriptor and reports its type and size.
func statHandle(h handle) (Stamp, Type, int64, error) {
	return statStamp(h, emptyPath, unix.AT_EMPTY_PATH)
}

func statStamp(dir int, cname []byte, flags int) (Stamp, Type, int64, error) {
	if !noStatx.Load() {
		var stx unix.Statx_t
		for {
			_, _, e := unix.Syscall6(unix.SYS_STATX, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), uintptr(flags|unix.AT_NO_AUTOMOUNT), stampMask, uintptr(unsafe.Pointer(&stx)), 0)
			switch e {
			case 0:
				return Stamp{
					Dev:   unix.Mkdev(stx.Dev_major, stx.Dev_minor),
					Ino:   stx.Ino,
					Mtime: nanos(stx.Mtime.Sec, int64(stx.Mtime.Nsec)),
					Ctime: nanos(stx.Ctime.Sec, int64(stx.Ctime.Nsec)),
				}, typeFromMode(uint32(stx.Mode)), int64(stx.Size), nil
			case unix.EINTR:
				continue
			case unix.ENOSYS, unix.EPERM:
				// Pre-4.11 kernels lack statx; older container seccomp
				// profiles reject it with EPERM. fstatat answers the same.
				noStatx.Store(true)
			default:
				return Stamp{}, 0, 0, e
			}
			break
		}
	}
	var st syscall.Stat_t
	var err error
	if flags&unix.AT_EMPTY_PATH != 0 {
		for {
			if err = syscall.Fstat(dir, &st); err != syscall.EINTR {
				break
			}
		}
	} else {
		err = fstatat(dir, cname, &st, flags)
	}
	return Stamp{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Mtime: nanos(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)), Ctime: nanos(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))}, typeFromMode(uint32(st.Mode)), int64(st.Size), err
}

// statChange is st's status change time in Unix nanoseconds.
func statChange(st *syscall.Stat_t) int64 { return nanos(int64(st.Ctim.Sec), int64(st.Ctim.Nsec)) }

// fstatat is unix.Fstatat on a NUL-terminated name without converting it to
// a string, retrying EINTR.
func fstatat(dir int, cname []byte, st *syscall.Stat_t, flags int) error {
	for {
		_, _, e := unix.Syscall6(sysFstatat, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), uintptr(unsafe.Pointer(st)), uintptr(flags), 0, 0)
		if e != unix.EINTR {
			if e != 0 {
				return e
			}
			return nil
		}
	}
}

// fineTimestamps admits local filesystems that keep sub-second timestamps
// from the local clock. NFS, CIFS, FUSE, and overlayfs (whose merged
// directories need not change when a lower layer does) are listed every
// time.
func fineTimestamps(h handle) bool {
	var fs unix.Statfs_t
	if unix.Fstatfs(h, &fs) != nil {
		return false
	}
	switch uint32(fs.Type) {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.TMPFS_MAGIC,
		unix.F2FS_SUPER_MAGIC, unix.BCACHEFS_SUPER_MAGIC, zfsSuperMagic:
		return true
	}
	return false
}

const zfsSuperMagic = 0x2fc12fc1

// stat resolves the NUL-terminated cname relative to dir without following
// symlinks.
func (s *session) stat(dir handle, cname []byte) (typ Type, size, mtime int64, ok bool) {
	size = -1
	if !noStatx.Load() {
		var stx unix.Statx_t
		for {
			_, _, e := unix.Syscall6(unix.SYS_STATX, uintptr(dir), uintptr(unsafe.Pointer(&cname[0])), statxFlags, statxMask, uintptr(unsafe.Pointer(&stx)), 0)
			switch e {
			case 0:
				typ = typeFromMode(uint32(stx.Mode))
				if typ == File {
					size = int64(stx.Size)
				}
				return typ, size, nanos(stx.Mtime.Sec, int64(stx.Mtime.Nsec)), typ != 0
			case unix.EINTR:
				continue
			case unix.ENOSYS, unix.EPERM:
				// Pre-4.11 kernels lack statx; older container seccomp
				// profiles reject it with EPERM. fstatat answers the same.
				noStatx.Store(true)
			default:
				return 0, -1, 0, false
			}
			break
		}
	}
	var st syscall.Stat_t
	if fstatat(dir, cname, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return 0, -1, 0, false
	}
	typ = typeFromMode(uint32(st.Mode))
	if typ == File {
		size = int64(st.Size)
	}
	return typ, size, nanos(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)), typ != 0
}

// readDir lists the directory at path with complete lstat-compatible
// metadata for every entry, special files included, through the same open,
// getdents64 loop, and buffer as search listings. Entries whose lstat fails
// are reported as failedEntry values carrying an lstat *os.PathError (path built only on failure) instead of failing the listing.
func (s *session) readDir(path string) ([]os.DirEntry, error) {
	s.dir = path
	h, err := s.openDir(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer closeHandle(h)
	s.entries = nil
	if err := s.list(h, metadataDetail, 0); err != nil {
		s.entries = nil
		return nil, &os.PathError{Op: "readdirent", Path: path, Err: err}
	}
	return s.entries, nil
}

// metadataBatchSize sizes compact, independently owned name and stat slabs.
// Names are validated with the same rules as the getdents loop.
func metadataBatchSize(buf []byte) (nameBytes, entries int, err error) {
	for off := 0; off+direntName <= len(buf); {
		rec := buf[off:]
		reclen := int(*(*uint16)(unsafe.Pointer(&rec[direntReclen])))
		if reclen < direntName || reclen > len(rec) {
			return 0, 0, unix.EIO
		}
		off += reclen
		if *(*uint64)(unsafe.Pointer(&rec[direntIno])) == 0 {
			continue
		}
		i := bytes.IndexByte(rec[direntName:reclen], 0)
		if i < 0 {
			return 0, 0, unix.EIO
		}
		if i != 0 && !isDotOrDotDot(rec[direntName:direntName+i]) {
			nameBytes += i
			entries++
		}
	}
	return nameBytes, entries, nil
}

// metadataEntry lstats the NUL-terminated cname relative to dir into a
// complete nativeInfo, or reports the failure with the type d_type gave.
func (s *session) metadataEntry(dir handle, cname []byte, dt byte, name string, slab *[]nativeInfo) os.DirEntry {
	info := newInfo(slab)
	info.name = name
	if err := fstatat(dir, cname, &info.st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		dropInfo(slab)
		return failedEntry{name: name, typ: dtMode(dt), err: &os.PathError{Op: "lstat", Path: filepath.Join(s.dir, name), Err: err}}
	}
	info.mode = modeFromStat(uint32(info.st.Mode))
	info.mod = time.Unix(int64(info.st.Mtim.Sec), int64(info.st.Mtim.Nsec))
	return info
}

// dtMode maps a d_type to the Go file type bits; DT_UNKNOWN maps to 0.
func dtMode(dt byte) os.FileMode {
	switch dt {
	case unix.DT_DIR:
		return os.ModeDir
	case unix.DT_LNK:
		return os.ModeSymlink
	case unix.DT_FIFO:
		return os.ModeNamedPipe
	case unix.DT_SOCK:
		return os.ModeSocket
	case unix.DT_CHR:
		return os.ModeDevice | os.ModeCharDevice
	case unix.DT_BLK:
		return os.ModeDevice
	}
	return 0
}
