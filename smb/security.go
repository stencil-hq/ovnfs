package smb

import (
	"encoding/binary"
	"os"
	"reflect"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// unixMetadata extracts native stat fields without depending on an OS-specific stat type.
// ok reports whether the stat carries Unix IDs; on Windows it does not, and uid and gid are 0.
func unixMetadata(info os.FileInfo) (uid, gid uint32, ino, dev, links uint64, ok bool) {
	links = 1
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	get := func(name string) uint64 {
		f := v.FieldByName(name)
		if !f.IsValid() {
			return 0
		}
		switch f.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return f.Uint()
		case reflect.Int, reflect.Int32, reflect.Int64:
			return uint64(f.Int())
		}
		return 0
	}
	uid, gid = uint32(get("Uid")), uint32(get("Gid"))
	ok = v.FieldByName("Uid").IsValid() && v.FieldByName("Gid").IsValid()
	ino, dev = get("Ino"), get("Dev")
	if n := get("Nlink"); n != 0 {
		links = n
	}
	return
}

// unixMode converts Go's mode flags to the POSIX st_mode representation.
func unixMode(info os.FileInfo) uint32 {
	m := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		m |= 04000
	}
	if info.Mode()&os.ModeSetgid != 0 {
		m |= 02000
	}
	if info.Mode()&os.ModeSticky != 0 {
		m |= 01000
	}
	switch {
	case info.IsDir():
		m |= 0040000
	case info.Mode()&os.ModeSymlink != 0:
		m |= 0120000
	case info.Mode()&os.ModeNamedPipe != 0:
		m |= 0010000
	case info.Mode()&os.ModeSocket != 0:
		m |= 0140000
	case info.Mode()&os.ModeDevice != 0:
		if info.Mode()&os.ModeCharDevice != 0 {
			m |= 0020000
		} else {
			m |= 0060000
		}
	default:
		m |= 0100000
	}
	return m
}

// appendSID writes a revision-one SID with its identifier authority and subauthorities.
func appendSID(dst []byte, authority byte, sub ...uint32) []byte {
	dst = append(dst, 1, byte(len(sub)), 0, 0, 0, 0, 0, authority)
	for _, n := range sub {
		dst = binary.LittleEndian.AppendUint32(dst, n)
	}
	return dst
}

// securityDescriptor describes Unix ownership and mode using Samba's NFS interoperability ACEs.
// A file without Unix IDs is owned by owner and group, binary SIDs, when they are
// set, and otherwise by the Unix-ID SIDs of uid and gid 0.
func securityDescriptor(info os.FileInfo, want uint32, owner, group []byte) []byte {
	uid, gid, _, _, _, unix := unixMetadata(info)
	sd := make([]byte, 20)
	sd[0] = 1
	binary.LittleEndian.PutUint16(sd[2:], 0x8000)
	if want&1 != 0 {
		binary.LittleEndian.PutUint32(sd[4:], uint32(len(sd)))
		if unix || owner == nil {
			sd = appendSID(sd, 22, 1, uid)
		} else {
			sd = append(sd, owner...)
		}
	}
	if want&2 != 0 {
		binary.LittleEndian.PutUint32(sd[8:], uint32(len(sd)))
		if unix || group == nil {
			sd = appendSID(sd, 22, 2, gid)
		} else {
			sd = append(sd, group...)
		}
	}
	if want&4 != 0 {
		binary.LittleEndian.PutUint16(sd[2:], 0x8004)
		binary.LittleEndian.PutUint32(sd[16:], uint32(len(sd)))
		acl := []byte{2, 0, 0, 0, 4, 0, 0, 0}
		rights := uint32(0x00120080)
		mode := info.Mode().Perm()
		if mode&0444 != 0 {
			rights |= 1 | 8
		}
		if mode&0222 != 0 {
			rights |= 2 | 4 | 0x10 | 0x100 | 0x10000
		}
		if mode&0111 != 0 {
			rights |= 0x20
		}
		ace := func(kind byte, mask uint32, sid []byte) {
			acl = append(acl, kind, 0)
			acl = binary.LittleEndian.AppendUint16(acl, uint16(8+len(sid)))
			acl = binary.LittleEndian.AppendUint32(acl, mask)
			acl = append(acl, sid...)
		}
		ace(0, rights, appendSID(nil, 1, 0))
		ace(1, 0, appendSID(nil, 5, 88, 1, uid))
		ace(1, 0, appendSID(nil, 5, 88, 2, gid))
		ace(1, 0, appendSID(nil, 5, 88, 3, unixMode(info)))
		binary.LittleEndian.PutUint16(acl[2:], uint16(len(acl)))
		sd = append(sd, acl...)
	}
	return sd
}

// applySecurity validates a self-relative descriptor and applies its Samba Unix-mode ACE.
func applySecurity(fs Backend, p string, sd []byte) uint32 {
	if len(sd) < 20 || sd[0] != 1 || binary.LittleEndian.Uint16(sd[2:])&0x8000 == 0 {
		return statusInvalidParameter
	}
	for _, field := range []int{4, 8} {
		off := uint64(binary.LittleEndian.Uint32(sd[field:]))
		if off != 0 {
			if off+8 > uint64(len(sd)) || sd[off] != 1 || off+8+uint64(sd[off+1])*4 > uint64(len(sd)) {
				return statusInvalidParameter
			}
		}
	}
	off := uint64(binary.LittleEndian.Uint32(sd[16:]))
	if off == 0 {
		return statusSuccess
	}
	if off+8 > uint64(len(sd)) {
		return statusInvalidParameter
	}
	size := uint64(binary.LittleEndian.Uint16(sd[off+2:]))
	if size < 8 || off+size > uint64(len(sd)) {
		return statusInvalidParameter
	}
	end := off + size
	count := binary.LittleEndian.Uint16(sd[off+4:])
	off += 8
	var mode uint32
	found := false
	for range count {
		if off+4 > end {
			return statusInvalidParameter
		}
		n := uint64(binary.LittleEndian.Uint16(sd[off+2:]))
		if n < 4 || off+n > end {
			return statusInvalidParameter
		}
		if sd[off] <= 1 && n >= 28 && binary.LittleEndian.Uint32(sd[off+4:]) == 0 {
			sid := sd[off+8 : off+n]
			if sid[0] == 1 && sid[1] == 3 && sid[2] == 0 && sid[3] == 0 && sid[4] == 0 && sid[5] == 0 && sid[6] == 0 && sid[7] == 5 && binary.LittleEndian.Uint32(sid[8:]) == 88 && binary.LittleEndian.Uint32(sid[12:]) == 3 {
				mode = binary.LittleEndian.Uint32(sid[16:])
				found = true
			}
		}
		off += n
	}
	if found {
		m := os.FileMode(mode & 0777)
		if mode&04000 != 0 {
			m |= os.ModeSetuid
		}
		if mode&02000 != 0 {
			m |= os.ModeSetgid
		}
		if mode&01000 != 0 {
			m |= os.ModeSticky
		}
		info, err := fs.Lstat(p)
		if err != nil {
			return ntinfo.Status(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == m {
				return statusSuccess
			}
			return statusNotSupported
		}
		return ntinfo.Status(fs.Chmod(p, m))
	}
	return statusSuccess
}
