//go:build windows

package fsys

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows directory reads:
//
//   - Children open with NtCreateFile relative to the parent's handle
//     (OBJECT_ATTRIBUTES.RootDirectory) and the raw entry name: no Win32
//     path parsing, no DOS-to-NT path conversion, no per-component
//     re-traversal from the drive root, and no MAX_PATH limit. Files read
//     through a directory handle open the same way.
//   - Listings use NtQueryDirectoryFile with FileDirectoryInformation, the
//     smallest class that carries attributes, size, and mtime, so Full detail
//     costs nothing extra and each call returns as many entries as possible.
//   - Names convert from UTF-16 straight into the listing arena as WTF-8,
//     which round-trips unpaired surrogates back to the exact on-disk name.
//   - Following stats of absolute paths come from NtQueryFullAttributesFile,
//     which answers a path query without the caller opening a handle. The NT
//     layer takes names as spelled, so Win32 rewriting (separators, `.`/`..`,
//     repeated separators, trailing dots and spaces) runs first through
//     GetFullPathName; relative, nonfollowing, device, and reparse-point
//     queries keep Win32 semantics through CreateFile.
//
// Reparse points (symlinks, junctions, mount points, cloud placeholders) are
// reported as Symlink and never descended.

type handle = windows.Handle

var (
	ntdll                         = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryDirectoryFile      = ntdll.NewProc("NtQueryDirectoryFile")
	procNtQueryFullAttributesFile = ntdll.NewProc("NtQueryFullAttributesFile")
)

const (
	listBufSize = 64 << 10

	// defaultWorkers caps the default worker count (unmeasured on Windows;
	// the reference walker uses the same).
	defaultWorkers = 4

	// maxReadChunk is the most one positional ReadFile requests.
	maxReadChunk = 1 << 30

	// maxNameUnits is the longest UNICODE_STRING, in UTF-16 units.
	maxNameUnits = 0x7fff

	// longPathUnits is the length from which a Win32 path gets the `\\?\`
	// prefix so it escapes MAX_PATH.
	longPathUnits = 248

	fileDirectoryInformation = 1

	// FILE_DIRECTORY_INFORMATION field offsets.
	fdiNextEntryOffset = 0
	fdiCreationTime    = 8
	fdiLastAccessTime  = 16
	fdiLastWriteTime   = 24
	fdiChangeTime      = 32
	fdiEndOfFile       = 40
	fdiFileAttributes  = 56
	fdiFileNameLength  = 60
	fdiFileName        = 64

	statusNoSuchFile = windows.NTStatus(0xC000000F)

	// 100ns ticks between 1601-01-01 and 1970-01-01.
	unixEpochTicks = 116_444_736_000_000_000

	shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

	pathSep = '\\'
	// memoStableByDev: path queries expose no volume identity (Stamp.Dev is
	// always 0), so the filesystem is checked per stored directory.
	memoStableByDev = false

	fileNetworkOpenInformation = 34

	// dirAccess lists entries and reads attributes (for stamps).
	dirAccess  = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
	fileAccess = windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
)

// networkOpenInfo is FILE_NETWORK_OPEN_INFORMATION, shared by
// NtQueryFullAttributesFile (by path) and NtQueryInformationFile (by handle).
type networkOpenInfo struct {
	creationTime, lastAccessTime, lastWriteTime, changeTime int64
	allocationSize, endOfFile                               int64
	fileAttributes                                          uint32
	_                                                       uint32
}

func (i *networkOpenInfo) stamp() Stamp {
	return Stamp{
		Ino:   uint64(i.creationTime),
		Mtime: filetimeNanos(i.lastWriteTime),
		Ctime: filetimeNanos(i.changeTime),
	}
}

// fileInfo describes what the attributes name: a directory (size -1) or a
// file. With link set a reparse point is reported as a Symlink (size -1)
// instead of by the target it was opened through.
func (i *networkOpenInfo) fileInfo(link bool) FileInfo {
	switch {
	case link && i.fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		return FileInfo{Type: Symlink, Size: -1, Stamp: i.stamp()}
	case i.fileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		return FileInfo{Type: Dir, Size: -1, Stamp: i.stamp()}
	}
	return FileInfo{Type: File, Size: i.endOfFile, Stamp: i.stamp()}
}

// filetimeNanos converts a FILETIME tick count (100ns since 1601) to Unix
// nanoseconds, saturating at the int64 range instead of wrapping.
func filetimeNanos(ticks int64) int64 {
	if ticks < math.MinInt64+unixEpochTicks {
		return math.MinInt64
	}
	d := ticks - unixEpochTicks
	switch {
	case d > math.MaxInt64/100:
		return math.MaxInt64
	case d < math.MinInt64/100:
		return math.MinInt64
	}
	return d * 100
}

// pathKind classifies a Win32 path by its root.
type pathKind uint8

const (
	// pathOther covers relative, drive-relative, rooted, and device paths:
	// anything the NT layer cannot take after plain normalization.
	pathOther pathKind = iota
	pathVerbatim
	pathDrive
	pathUNC
)

func isSep(c byte) bool { return c == '\\' || c == '/' }

// classify reports how path is rooted. Only exact `\\?\` is verbatim; other
// `?` and `.` device spellings, and extra leading separators, stay with Win32.
func classify(path string) pathKind {
	switch {
	case strings.HasPrefix(path, `\\?\`):
		return pathVerbatim
	case len(path) >= 3 && isSep(path[0]) && isSep(path[1]):
		if isSep(path[2]) || ((path[2] == '?' || path[2] == '.') && (len(path) == 3 || isSep(path[3]))) {
			return pathOther
		}
		return pathUNC
	case len(path) >= 3 && (path[0]|0x20 >= 'a' && path[0]|0x20 <= 'z') && path[1] == ':' && isSep(path[2]):
		return pathDrive
	}
	return pathOther
}

// needsNormalizing reports whether GetFullPathName would rewrite a
// non-verbatim path: `/` separators, repeated separators, `.`/`..`
// components, or a component ending in a dot or space.
func needsNormalizing(path string) bool {
	body := strings.TrimPrefix(path, `\\`)
	if strings.IndexByte(body, '/') >= 0 || strings.Contains(body, `\\`) {
		return true
	}
	for i := 0; i < len(body); i++ {
		if c := body[i]; (c == '.' || c == ' ') && (i+1 == len(body) || body[i+1] == '\\') {
			return true
		}
	}
	return false
}

// fullPath resolves the NUL-terminated UTF-16 path in to its absolute Win32
// form with GetFullPathName, using buf while it is large enough.
func fullPath(in, buf []uint16) ([]uint16, error) {
	for {
		n, err := windows.GetFullPathName(&in[0], uint32(len(buf)), &buf[0], nil)
		if err != nil {
			return nil, err
		}
		if int(n) < len(buf) {
			return buf[:n], nil
		}
		buf = make([]uint16, n)
	}
}

// ntName leaves in s.wname the NT object name (`\??\...`) of the absolute
// path after the Win32 normalization the NT layer lacks; verbatim paths pass
// as spelled. ok is false when path is not an absolute drive, UNC, or
// verbatim path (or normalizes to a device path), which only Win32 resolves.
func (s *session) ntName(path string) (ok bool, err error) {
	kind := classify(path)
	switch kind {
	case pathVerbatim:
		s.wname = appendUTF16Raw(appendUTF16Raw(s.wname[:0], `\??\`), path[4:])
	case pathDrive, pathUNC:
		if !needsNormalizing(path) {
			if kind == pathUNC {
				s.wname = appendUTF16Raw(appendUTF16Raw(s.wname[:0], `\??\UNC\`), path[2:])
			} else {
				s.wname = appendUTF16Raw(appendUTF16Raw(s.wname[:0], `\??\`), path)
			}
			break
		}
		s.wname = append(appendUTF16Raw(s.wname[:0], path), 0)
		var stack [windows.MAX_PATH]uint16
		full, err := fullPath(s.wname, stack[:])
		if err != nil {
			return false, err
		}
		switch {
		case len(full) >= 3 && full[0] == '\\' && full[1] == '\\':
			if (full[2] == '?' || full[2] == '.') && (len(full) == 3 || full[3] == '\\') {
				return false, nil
			}
			s.wname = append(appendUTF16Raw(s.wname[:0], `\??\UNC\`), full[2:]...)
		case len(full) >= 3 && full[1] == ':' && full[2] == '\\':
			s.wname = append(appendUTF16Raw(s.wname[:0], `\??\`), full...)
		default:
			return false, nil
		}
	default:
		return false, nil
	}
	if len(s.wname) > maxNameUnits {
		return false, windows.ERROR_FILENAME_EXCED_RANGE
	}
	return true, nil
}

// Stat describes the file or directory at path. A following stat of an
// absolute path queries its normalized NT name without opening a handle;
// relative and nonfollowing stats, and reparse points met on the direct path,
// go through the Win32 layer.
func (s *session) Stat(path string, follow bool) (FileInfo, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return FileInfo{}, windows.ERROR_INVALID_NAME
	}
	if !follow {
		return s.statWin32(path, false)
	}
	ok, err := s.ntName(path)
	if err != nil {
		return FileInfo{}, err
	}
	if !ok {
		return s.statWin32(path, true)
	}
	us := windows.NTUnicodeString{Length: uint16(2 * len(s.wname)), MaximumLength: uint16(2 * len(s.wname)), Buffer: &s.wname[0]}
	oa := windows.OBJECT_ATTRIBUTES{ObjectName: &us, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var info networkOpenInfo
	r, _, _ := syscall.SyscallN(procNtQueryFullAttributesFile.Addr(), uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&info)))
	if status := windows.NTStatus(r); status != windows.STATUS_SUCCESS {
		return FileInfo{}, ntErr(status)
	}
	// The attribute query describes a reparse point itself; opening it
	// resolves the target.
	if info.fileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return s.statWin32(path, true)
	}
	return info.fileInfo(false), nil
}

// statWin32 opens path through Win32 and describes it. With follow its
// reparse points resolve and a dangling link fails as not-exist; without, a
// final reparse point is reported as a Symlink.
func (s *session) statWin32(path string, follow bool) (FileInfo, error) {
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if !follow {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	h, err := s.createFile(path, windows.FILE_READ_ATTRIBUTES, flags)
	if err != nil {
		return FileInfo{}, err
	}
	defer windows.CloseHandle(h)
	info, err := queryHandle(h)
	if err != nil {
		return FileInfo{}, err
	}
	return info.fileInfo(!follow), nil
}

// Identify opens the path because path queries carry no file index.
func (s *session) Identify(path string) (ID, error) {
	h, err := s.createFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return ID{}, err
	}
	defer windows.CloseHandle(h)
	return identifyHandle(h)
}

// IdentifyDir identifies the open directory h from its handle, without
// resolving a path.
func (s *session) IdentifyDir(h Handle) (ID, error) { return identifyHandle(h.native()) }

// identifyHandle reads the volume serial number and file index of h.
func identifyHandle(h handle) (ID, error) {
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return ID{}, err
	}
	return ID{Dev: uint64(fi.VolumeSerialNumber), Ino: uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)}, nil
}

// queryHandle reads the network-open attributes of an open handle.
func queryHandle(h handle) (networkOpenInfo, error) {
	var info networkOpenInfo
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtQueryInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), fileNetworkOpenInformation)
	return info, ntErr(err)
}

// statHandle stamps an open handle and reports its type and size (reparse
// points opened without FILE_OPEN_REPARSE_POINT report their target).
func statHandle(h handle) (Stamp, Type, int64, error) {
	info, err := queryHandle(h)
	if err != nil {
		return Stamp{}, 0, 0, err
	}
	typ := File
	if info.fileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		typ = Dir
	}
	return info.stamp(), typ, info.endOfFile, nil
}

// fineTimestamps admits NTFS and ReFS, which keep 100ns timestamps and
// update a directory's write time when its entries change.
func fineTimestamps(h handle) bool {
	var name [windows.MAX_PATH + 1]uint16
	if windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, nil, &name[0], uint32(len(name))) != nil {
		return false
	}
	fs := windows.UTF16ToString(name[:])
	return fs == "NTFS" || fs == "ReFS"
}

// win32Name leaves path NUL-terminated in s.wname for the Win32 layer, which
// resolves drive-relative forms, UNC paths, and `\\?\` prefixes itself. A path
// long enough to hit MAX_PATH is made absolute first and given the `\\?\`
// prefix, which verbatim and device paths already carry.
func (s *session) win32Name(path string) (*uint16, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return nil, windows.ERROR_INVALID_NAME
	}
	s.wname = append(appendUTF16Raw(s.wname[:0], path), 0)
	if len(s.wname) > longPathUnits && !strings.HasPrefix(path, `\\?\`) && !strings.HasPrefix(path, `\\.\`) && !strings.HasPrefix(path, `\??\`) {
		var stack [windows.MAX_PATH]uint16
		full, err := fullPath(s.wname, stack[:])
		if err != nil {
			return nil, err
		}
		switch {
		case len(full) >= 4 && full[0] == '\\' && full[1] == '\\' && (full[2] == '?' || full[2] == '.') && full[3] == '\\':
			s.wname = append(s.wname[:0], full...)
		case len(full) >= 3 && full[0] == '\\' && full[1] == '\\':
			s.wname = append(appendUTF16Raw(s.wname[:0], `\\?\UNC\`), full[2:]...)
		case len(full) >= 3 && full[1] == ':' && full[2] == '\\':
			s.wname = append(appendUTF16Raw(s.wname[:0], `\\?\`), full...)
		default:
			s.wname = append(s.wname[:0], full...)
		}
		s.wname = append(s.wname, 0)
	}
	return &s.wname[0], nil
}

// createFile opens path through the Win32 layer.
func (s *session) createFile(path string, access, flags uint32) (handle, error) {
	name, err := s.win32Name(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(name, access, shareAll, nil, windows.OPEN_EXISTING, flags, 0)
}

func (s *session) openDir(path string) (handle, error) {
	h, err := s.createFile(path, dirAccess, windows.FILE_FLAG_BACKUP_SEMANTICS)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// FILE_FLAG_BACKUP_SEMANTICS also opens regular files.
	if _, typ, _, err := statHandle(h); err != nil || typ != Dir {
		windows.CloseHandle(h)
		if err == nil {
			err = windows.ERROR_DIRECTORY
		}
		return windows.InvalidHandle, err
	}
	return h, nil
}

// openChild opens the subdirectory name of parent without following a
// reparse point that replaced it since the listing.
func (s *session) openChild(parent handle, name string) (handle, error) {
	return s.ntOpen(parent, name, dirAccess, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT)
}

func (s *session) ntOpen(parent handle, name string, access, options uint32) (handle, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return windows.InvalidHandle, windows.ERROR_INVALID_NAME
	}
	s.wname = appendUTF16(s.wname[:0], name)
	if len(s.wname) == 0 || len(s.wname) > maxNameUnits {
		return windows.InvalidHandle, windows.ERROR_INVALID_NAME
	}
	us := windows.NTUnicodeString{
		Length:        uint16(2 * len(s.wname)),
		MaximumLength: uint16(2 * len(s.wname)),
		Buffer:        &s.wname[0],
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: &us}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h handle
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0, shareAll, windows.FILE_OPEN,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		return windows.InvalidHandle, ntErr(err)
	}
	return h, nil
}

// ntErr turns an NTSTATUS into the Win32 error the os package would report, so
// callers can match fs.ErrNotExist and the like; a FILE_NON_DIRECTORY_FILE open
// that found a directory is ErrNotRegular, as on Unix.
func ntErr(err error) error {
	status, ok := err.(windows.NTStatus)
	switch {
	case !ok:
		return err
	case status == windows.STATUS_FILE_IS_A_DIRECTORY:
		return ErrNotRegular
	}
	return status.Errno()
}

func closeHandle(h handle) { windows.CloseHandle(h) }

// list reads every entry of h into s.ls. Sizes and mtimes are always filled
// because the listing returns them for free. With metadataDetail it instead
// appends complete os.DirEntry values to s.entries (see readDir).
func (s *session) list(h handle, detail Detail, _ uint32) error {
	if len(s.buf) < listBufSize {
		s.buf = make([]byte, listBufSize)
	}
	buf := s.buf
	restart := uintptr(1)
	for {
		var iosb windows.IO_STATUS_BLOCK
		r, _, _ := syscall.SyscallN(procNtQueryDirectoryFile.Addr(),
			uintptr(h), 0, 0, 0, uintptr(unsafe.Pointer(&iosb)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			fileDirectoryInformation, 0, 0, restart)
		restart = 0
		switch status := windows.NTStatus(r); status {
		case windows.STATUS_SUCCESS:
		case windows.STATUS_NO_MORE_FILES, statusNoSuchFile:
			return nil
		default:
			if int32(status) < 0 {
				return ntErr(status)
			}
		}
		n := int(iosb.Information)
		if n == 0 {
			return nil
		}
		if n > len(buf) {
			return windows.ERROR_INVALID_DATA
		}
		var names string
		var slab []winInfo
		if detail == metadataDetail {
			var err error
			names, slab, err = s.metadataBatch(buf[:n])
			if err != nil {
				return err
			}
		}
		for off := 0; ; {
			if off+fdiFileName > n {
				return windows.ERROR_INVALID_DATA
			}
			rec := buf[off:n]
			next := int(binary.LittleEndian.Uint32(rec[fdiNextEntryOffset:]))
			nameLen := int(binary.LittleEndian.Uint32(rec[fdiFileNameLength:]))
			if nameLen%2 != 0 || fdiFileName+nameLen > len(rec) {
				return windows.ERROR_INVALID_DATA
			}
			wname := unsafe.Slice((*uint16)(unsafe.Pointer(&rec[fdiFileName])), nameLen/2)
			if !isDotOrDotDotUTF16(wname) && nameLen > 0 {
				attrs := binary.LittleEndian.Uint32(rec[fdiFileAttributes:])
				if detail == metadataDetail {
					var name string
					name, names, _ = strings.Cut(names, "\x00")
					s.addMetadata(rec, name, attrs, &slab)
				} else {
					typ, size := File, int64(binary.LittleEndian.Uint64(rec[fdiEndOfFile:]))
					switch {
					case attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
						typ, size = Symlink, -1
					case attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
						typ, size = Dir, -1
					}
					mtime := filetimeNanos(int64(binary.LittleEndian.Uint64(rec[fdiLastWriteTime:])))
					ls := &s.ls
					ls.Entries = append(ls.Entries, Record{off: uint32(len(ls.names)), Type: typ, Size: size, Mtime: mtime})
					ls.names = appendWTF8(ls.names, wname)
					r := &ls.Entries[len(ls.Entries)-1]
					r.nlen = uint32(len(ls.names)) - r.off
				}
			}
			if next == 0 {
				break
			}
			if next < fdiFileName || off+next > n {
				return windows.ERROR_INVALID_DATA
			}
			off += next
		}
	}
}

// metadataBatch converts names once into reusable scratch, then freezes only
// those bytes, not the native records. NUL separators locate each WTF-8 name
// without an offset allocation or a second UTF-16 conversion.
func (s *session) metadataBatch(buf []byte) (string, []winInfo, error) {
	scratch := s.ls.names[:0]
	entries, ordinary := 0, 0
	for off := 0; ; {
		if off+fdiFileName > len(buf) {
			return "", nil, windows.ERROR_INVALID_DATA
		}
		rec := buf[off:]
		next := int(binary.LittleEndian.Uint32(rec[fdiNextEntryOffset:]))
		nameLen := int(binary.LittleEndian.Uint32(rec[fdiFileNameLength:]))
		if nameLen%2 != 0 || fdiFileName+nameLen > len(rec) {
			return "", nil, windows.ERROR_INVALID_DATA
		}
		wname := unsafe.Slice((*uint16)(unsafe.Pointer(&rec[fdiFileName])), nameLen/2)
		if nameLen > 0 && !isDotOrDotDotUTF16(wname) {
			scratch = appendWTF8(scratch, wname)
			scratch = append(scratch, 0)
			entries++
			if binary.LittleEndian.Uint32(rec[fdiFileAttributes:])&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
				ordinary++
			}
		}
		if next == 0 {
			break
		}
		if next < fdiFileName || off+next > len(buf) {
			return "", nil, windows.ERROR_INVALID_DATA
		}
		off += next
	}
	s.ls.names = scratch
	s.entries = slices.Grow(s.entries, entries)
	return string(scratch), make([]winInfo, 0, ordinary), nil
}

func isDotOrDotDotUTF16(name []uint16) bool {
	return len(name) > 0 && len(name) <= 2 && name[0] == '.' && (len(name) == 1 || name[1] == '.')
}

func (s *session) OpenFile(dir Handle, name string) (Handle, FileInfo, error) {
	var h handle
	var err error
	if dir.ok {
		h, err = s.ntOpen(dir.native(), name, fileAccess, windows.FILE_NON_DIRECTORY_FILE)
	} else {
		h, err = s.createFile(name, fileAccess, 0)
	}
	if err != nil {
		return Handle{}, FileInfo{}, err
	}
	st, typ, size, err := statHandle(h)
	if err == nil && typ != File {
		err = ErrNotRegular
	}
	if err != nil {
		windows.CloseHandle(h)
		return Handle{}, FileInfo{}, err
	}
	return Handle{h: uintptr(h), size: size, ok: true}, FileInfo{Type: File, Size: size, Stamp: st}, nil
}

// Read passes each read's offset in an OVERLAPPED, which on a synchronous
// handle makes it positional; at end of file such a read fails with
// ERROR_HANDLE_EOF instead of returning 0 bytes. No request exceeds
// maxReadChunk; the read loop continues past a clamped read.
func (s *session) Read(h Handle, buf []byte, limit int64) ([]byte, error) {
	return readInto(buf, h.size, limit, func(p []byte, off int64) (int, error) {
		p = p[:min(len(p), maxReadChunk)]
		var n uint32
		o := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
		err := windows.ReadFile(h.native(), p, &n, &o)
		if err == windows.ERROR_HANDLE_EOF {
			err = nil
		}
		return int(n), err
	})
}

// Map is unsupported: mapping was only measured to beat reading on darwin,
// so Windows always reads.
func (*session) Map(Handle, int64) ([]byte, error) { return nil, errors.ErrUnsupported }

func (*session) Unmap([]byte) {}
