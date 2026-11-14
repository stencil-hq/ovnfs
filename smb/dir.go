package smb

import (
	"encoding/binary"
	"errors"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// dirCursor retains stable enumeration names and cookies, not file metadata.
type dirCursor struct {
	pattern string
	entries []dirEntry
	next    int
	queried bool
}

// dirEntry keeps the name and path separate for synthetic dot entries.
type dirEntry struct {
	name string
	path string
}

// init registers directory enumeration independently of transport setup.
func init() { handlers[cmdQueryDirectory] = handleQueryDirectory }

// handleQueryDirectory enumerates a stable, per-open snapshot using FSCC records.
func handleQueryDirectory(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 32 || binary.LittleEndian.Uint16(b) != 33 {
		return nil, statusInvalidParameter
	}
	class, flags := b[2], b[3]
	switch class {
	case 1, 2, 3, 12, 37, 38, 60, 80:
	case 100:
		if !r.c.posix {
			return nil, statusInvalidInfoClass
		}
	default:
		return nil, statusInvalidInfoClass
	}
	if flags & ^byte(0x17) != 0 {
		return nil, statusInvalidParameter
	}
	limit := binary.LittleEndian.Uint32(b[28:])
	max := r.c.maxTrans
	if max == 0 {
		max = 1 << 20
	}
	if limit > max {
		return nil, statusInvalidParameter
	}
	if limit == 0 {
		return nil, statusBufferTooSmall
	}
	nameLen := int(binary.LittleEndian.Uint16(b[26:]))
	nameOff := int(binary.LittleEndian.Uint16(b[24:])) - 64
	pattern := ""
	if nameLen != 0 {
		if nameLen&1 != 0 || nameOff < 32 || nameOff > len(b) || nameLen > len(b)-nameOff {
			return nil, statusInvalidParameter
		}
		pattern = utf16Decode(b[nameOff : nameOff+nameLen])
		if strings.ContainsAny(pattern, "/\\\x00:") {
			return nil, statusObjectNameInvalid
		}
	}
	o, st := r.openFile(fileID{binary.LittleEndian.Uint64(b[8:]), binary.LittleEndian.Uint64(b[16:])})
	if st != statusSuccess {
		return nil, st
	}
	if !o.isDir {
		return nil, statusNotADirectory
	}
	if o.access&1 == 0 {
		return nil, statusAccessDenied
	}
	// Other clients' cached child metadata must drain before enumeration.
	// The requesting client reconciles its own dirty EOF/metadata locally;
	// recalling those leases here would defeat write caching on every readdir.
	// FILE_NAMES_INFORMATION consumes names only, not cached file contents.
	if r.c.srv.WriteCache && class != 12 && !r.cacheProtected {
		return r.withWriteBarrier(o, true, true, func() ([]byte, uint32) { return handleQueryDirectory(r) })
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	first := o.dir == nil || flags&0x10 != 0
	if first || flags&1 != 0 {
		if !first {
			pattern = o.dir.pattern
		}
		if pattern == "" {
			pattern = "*"
		}
		entries, st := snapshotDirectory(r.c.srv.FS, o.path, pattern, r.c.posix || r.c.appleCaseSensitive.Load())
		if st != statusSuccess {
			return nil, st
		}
		o.dir = &dirCursor{pattern: pattern, entries: entries}
	}
	d := o.dir
	if flags&4 != 0 {
		index := uint64(binary.LittleEndian.Uint32(b[4:]))
		if index > 0 {
			index--
		}
		if index > uint64(len(d.entries)) {
			index = uint64(len(d.entries))
		}
		d.next = int(index)
	}
	if d.next == len(d.entries) {
		st := uint32(statusNoMoreFiles)
		if !d.queried && len(d.entries) == 0 {
			st = statusNoSuchFile
		}
		d.queried = true
		return nil, st
	}
	var data []byte
	previous := -1
	next := d.next
	for next < len(d.entries) {
		e := d.entries[next]
		// Metadata must belong to this request, not to an older enumeration
		// page: clients can otherwise accept an obsolete EOF and shrink caches.
		info, err := r.c.srv.FS.Lstat(e.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				next++
				continue
			}
			return nil, ntinfo.Status(err)
		}
		oldLength := len(data)
		start := (oldLength + 7) &^ 7
		data = append(data, make([]byte, start-oldLength)...)
		id := r.c.srv.FS.FileID(e.path)
		if id == 0 {
			return nil, statusInternalError
		}
		data = appendDirectoryEntry(r.c, data, class, e, info, uint32(next+1), id)
		if class == 37 && r.c.appleReadDir.Load() {
			if info.Mode()&os.ModeSymlink == 0 {
				binary.LittleEndian.PutUint32(data[start+64:], accessAll)
			}
			binary.LittleEndian.PutUint16(data[start+94:], uint16(unixMode(info)))
		}
		if len(data) > int(limit) {
			if oldLength == 0 {
				base := len(data) - utf16Len(e.name)
				if int(limit) < base {
					return nil, statusInfoLengthMismatch
				}
				d.next = next
				d.queried = true
				return directoryResponse(data[:int(limit)]), statusBufferOverflow
			}
			data = data[:oldLength]
			break
		}
		if previous >= 0 {
			binary.LittleEndian.PutUint32(data[previous:], uint32(start-previous))
		}
		previous = start
		next++
		if flags&2 != 0 {
			break
		}
	}
	d.next = next
	d.queried = true
	if len(data) == 0 {
		return nil, statusNoMoreFiles
	}
	return directoryResponse(data), statusSuccess
}

// snapshotDirectory uses direct lookup for exact names. Clients that negotiated
// case-sensitive naming must not turn an absent name into a full directory scan.
func snapshotDirectory(fs Backend, dir, pattern string, caseSensitive bool) ([]dirEntry, uint32) {
	if !strings.ContainsAny(pattern, "*?<>\"") {
		p := dir
		switch pattern {
		case ".":
		case "..":
			p = directoryParent(dir)
		default:
			p = path.Join(dir, pattern)
		}
		info, err := fs.Lstat(p)
		if err == nil {
			name := pattern
			if name != "." && name != ".." {
				name = info.Name()
				p = path.Join(dir, name)
			}
			return []dirEntry{{name: name, path: p}}, statusSuccess
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, ntinfo.Status(err)
		}
		if caseSensitive {
			return nil, statusSuccess
		}
	}
	infos, err := fs.ReadDir(dir)
	if err != nil {
		return nil, ntinfo.Status(err)
	}
	entries := make([]dirEntry, 0, len(infos)+2)
	for _, name := range []string{".", ".."} {
		if !dosWildcardMatch(pattern, name) {
			continue
		}
		p := dir
		if name == ".." {
			p = directoryParent(dir)
		}
		if _, err := fs.Lstat(p); err != nil {
			return nil, ntinfo.Status(err)
		}
		entries = append(entries, dirEntry{name: name, path: p})
	}
	for _, info := range infos {
		if dosWildcardMatch(pattern, info.Name()) {
			entries = append(entries, dirEntry{name: info.Name(), path: path.Join(dir, info.Name())})
		}
	}
	return entries, statusSuccess
}

// directoryParent clamps the parent of the share root to the share root.
func directoryParent(p string) string {
	p = path.Dir(p)
	if p == "." || p == "/" {
		return ""
	}
	return p
}

// appendDirectoryEntry encodes one unpadded MS-FSCC directory information record.
func appendDirectoryEntry(c *conn, dst []byte, class byte, e dirEntry, info os.FileInfo, index uint32, id uint64) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, 0)
	dst = binary.LittleEndian.AppendUint32(dst, index)
	if class == 100 {
		dst = appendPosixInfo(dst, e.path, info, id)
		dst = binary.LittleEndian.AppendUint32(dst, uint32(utf16Len(e.name)))
		return utf16Append(dst, e.name)
	}
	if class == 12 {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(utf16Len(e.name)))
		return utf16Append(dst, e.name)
	}
	dst = binary.LittleEndian.AppendUint64(dst, ntinfo.Filetime(ntinfo.BirthTime(info)))
	dst = binary.LittleEndian.AppendUint64(dst, ntinfo.Filetime(ntinfo.AccessTime(info)))
	dst = binary.LittleEndian.AppendUint64(dst, ntinfo.Filetime(info.ModTime()))
	dst = binary.LittleEndian.AppendUint64(dst, ntinfo.Filetime(ntinfo.ChangeTime(info)))
	size := uint64(info.Size())
	allocation := ntinfo.AllocationSize(info)
	if info.IsDir() {
		size, allocation = 0, 0
	}
	dst = binary.LittleEndian.AppendUint64(dst, size)
	dst = binary.LittleEndian.AppendUint64(dst, allocation)
	dst = binary.LittleEndian.AppendUint32(dst, c.fileAttributes(info))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(utf16Len(e.name)))
	var tag uint32
	if info.Mode()&os.ModeSymlink != 0 {
		tag = 0xa000000c
	}
	if class != 1 {
		ea := tag
		if class == 60 || class == 80 {
			ea = 0
		}
		dst = binary.LittleEndian.AppendUint32(dst, ea)
	}
	switch class {
	case 3:
		dst = append(dst, make([]byte, 26)...)
	case 37:
		dst = append(dst, make([]byte, 28)...)
		dst = binary.LittleEndian.AppendUint64(dst, id)
	case 38:
		dst = binary.LittleEndian.AppendUint32(dst, 0)
		dst = binary.LittleEndian.AppendUint64(dst, id)
	case 60, 80:
		dst = binary.LittleEndian.AppendUint32(dst, tag)
		if class == 80 {
			dst = binary.LittleEndian.AppendUint64(dst, id)
		}
		dst = binary.LittleEndian.AppendUint64(dst, id)
		dst = binary.LittleEndian.AppendUint64(dst, 0)
	}
	return utf16Append(dst, e.name)
}

// directoryResponse wraps directory and notification records in their common SMB2 buffer response.
func directoryResponse(data []byte) []byte {
	b := make([]byte, 8, 8+len(data))
	binary.LittleEndian.PutUint16(b, 9)
	binary.LittleEndian.PutUint16(b[2:], 72)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(data)))
	return append(b, data...)
}

// dosWildcardMatch implements Windows case-insensitive ordinary and DOS wildcard matching.
func dosWildcardMatch(pattern, name string) bool {
	if name == "" {
		return pattern == ""
	}
	if pattern == "*" || pattern == "*.*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?<>\"") {
		return strings.EqualFold(pattern, name)
	}
	p := utf16.Encode([]rune(strings.ToUpper(pattern)))
	n := utf16.Encode([]rune(strings.ToUpper(name)))
	lastDot := -1
	for i, ch := range n {
		if ch == '.' {
			lastDot = i
		}
	}
	next, current := make([]bool, len(n)+1), make([]bool, len(n)+1)
	next[len(n)] = true
	for _, v := range slices.Backward(p) {
		clear(current)
		for i := len(n); i >= 0; i-- {
			switch v {
			case '*':
				current[i] = next[i] || i < len(n) && current[i+1]
			case '?':
				current[i] = i < len(n) && next[i+1]
			case '<':
				current[i] = next[i] || i < len(n) && i != lastDot && current[i+1]
			case '>':
				if i == len(n) || n[i] == '.' {
					current[i] = next[i]
				} else {
					current[i] = next[i+1]
				}
			case '"':
				current[i] = i == len(n) && next[i] || i < len(n) && n[i] == '.' && next[i+1]
			default:
				current[i] = i < len(n) && v == n[i] && next[i+1]
			}
		}
		next, current = current, next
	}
	return next[0]
}
