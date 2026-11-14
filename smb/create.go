package smb

import (
	"encoding/binary"
	"hash/maphash"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/ntinfo"
)

// Create option and access masks identify filesystem operations rather than metadata-only opens.
const (
	createDirectory     = 0x00000001
	createWriteThrough  = 0x00000002
	createNonDirectory  = 0x00000040
	createDeleteOnClose = 0x00001000
	createOpenReparse   = 0x00200000
	accessReadData      = 0x00000001
	accessWriteData     = 0x00000002
	accessAppendData    = 0x00000004
	accessExecute       = 0x00000020
	accessDelete        = 0x00010000
	accessAll           = 0x001f01ff
	posixCreateTag      = "\x93\xad\x25\x50\x9c\xb4\x11\xe7\xb4\x23\x83\xde\x96\x8b\xcd\x7c"
)

// openRegistry tracks server-wide share modes, delete disposition and cache grants.
type openRegistry struct {
	// ns orders namespace transactions. Transactions on files share it and
	// hold their names' stripes of names (CREATE and CLOSE one name, a rename
	// two), so transactions on different names run in parallel: a host open
	// can take milliseconds (Windows Defender scans), and a global lock made
	// parallel builds queue behind each other. Transactions that reach a
	// directory's subtree (its rename or deletion, delete-on-close and
	// link-following creates) hold ns exclusively.
	ns         sync.RWMutex
	names      [64]sync.Mutex
	mu         sync.Mutex
	entries    map[*open]bool
	byPath     map[string]map[*open]struct{} // entries by path; open paths change only under mu
	pending    map[string]bool
	grants     map[*open]*oplockGrant
	writeGates atomic.Int64
	writeEpoch atomic.Uint64 // invalidates eligibility sampled before a protected mutation
	writeFault error         // sticky: a lost dirty owner cannot be recovered by a later ACK
}

// nameSeed spreads names over openRegistry.names.
var nameSeed = maphash.MakeSeed()

// lockName starts a transaction on name p alone; unlock ends it.
func (g *openRegistry) lockName(p string) (unlock func()) { return g.lockNames(p, p) }

// lockNames starts a transaction on names p and q alone; unlock ends it.
// Stripes lock in index order, so concurrent pairs cannot deadlock.
func (g *openRegistry) lockNames(p, q string) (unlock func()) {
	g.ns.RLock()
	i := maphash.String(nameSeed, p) % uint64(len(g.names))
	j := maphash.String(nameSeed, q) % uint64(len(g.names))
	if i > j {
		i, j = j, i
	}
	g.names[i].Lock()
	if j != i {
		g.names[j].Lock()
	}
	return func() {
		if j != i {
			g.names[j].Unlock()
		}
		g.names[i].Unlock()
		g.ns.RUnlock()
	}
}

// lockAll starts a transaction that may touch any name; unlock ends it.
func (g *openRegistry) lockAll() (unlock func()) {
	g.ns.Lock()
	return g.ns.Unlock
}

// add publishes an open for share-mode checks.
func (g *openRegistry) add(o *open) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries == nil {
		g.entries = make(map[*open]bool)
		g.byPath = make(map[string]map[*open]struct{})
	}
	g.entries[o] = true
	g.index(o.path, o)
}

// remove withdraws an open from share-mode checks.
func (g *openRegistry) remove(o *open) {
	g.mu.Lock()
	g.dropLocked(o)
	g.mu.Unlock()
}

// dropLocked withdraws a live open; the caller holds g.mu.
func (g *openRegistry) dropLocked(o *open) {
	if !g.entries[o] {
		return
	}
	delete(g.entries, o)
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	g.unindex(p, o)
}

// index files o under path p; the caller holds g.mu.
func (g *openRegistry) index(p string, o *open) {
	set := g.byPath[p]
	if set == nil {
		set = make(map[*open]struct{})
		g.byPath[p] = set
	}
	set[o] = struct{}{}
}

// unindex removes o from path p's opens; the caller holds g.mu.
func (g *openRegistry) unindex(p string, o *open) {
	if set := g.byPath[p]; set != nil {
		delete(set, o)
		if len(set) == 0 {
			delete(g.byPath, p)
		}
	}
}

// under lists the open paths at or below p; the caller holds g.mu.
func (g *openRegistry) under(p string) []string {
	var out []string
	for q := range g.byPath {
		if withinPath(q, p) {
			out = append(out, q)
		}
	}
	return out
}

// at snapshots every live open on a path.
func (g *openRegistry) at(p string) []*open {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*open, 0, len(g.byPath[p]))
	for o := range g.byPath[p] {
		out = append(out, o)
	}
	return out
}

// byID resolves a live server-wide open for copy-chunk resume keys.
func (g *openRegistry) byID(id fileID) *open {
	g.mu.Lock()
	defer g.mu.Unlock()
	for o := range g.entries {
		if o.id == id {
			return o
		}
	}
	return nil
}

// rename keeps open paths and pending deletion attached to renamed objects.
func (g *openRegistry) rename(from, to string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	moved := make(map[string]map[*open]struct{})
	for _, q := range g.under(from) {
		moved[q] = g.byPath[q]
		delete(g.byPath, q)
	}
	for q, set := range moved {
		next := to + strings.TrimPrefix(q, from)
		for o := range set {
			o.mu.Lock()
			o.path = next
			o.mu.Unlock()
			g.index(next, o)
		}
	}
	for p, v := range g.pending {
		if p == from || strings.HasPrefix(p, from+"/") {
			delete(g.pending, p)
			g.pending[to+strings.TrimPrefix(p, from)] = v
		}
	}
}

// setDeletePending changes the shared delete disposition of a path.
func (g *openRegistry) setDeletePending(p string, v bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == nil {
		g.pending = make(map[string]bool)
	}
	if v {
		g.pending[p] = true
	} else {
		delete(g.pending, p)
	}
}

// deletePending reports whether further opens must be refused.
func (g *openRegistry) deletePending(p string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		if g.pending[p] {
			return true
		}
		if p == "" {
			return false
		}
		p = path.Dir(p)
		if p == "." {
			p = ""
		}
	}
}

// createContext is a bounded, decoded SMB2 CREATE context.
type createContext struct {
	name string
	data []byte
}

// createArgs holds validated CREATE request fields across asynchronous lease breaks.
type createArgs struct {
	name                                string
	access, share, disposition, options uint32
	oplock                              uint8
	contexts                            []createContext
	leaseKey                            [16]byte
	leaseState                          uint32
	leaseVersion                        int
	leaseEpoch                          uint16
	leaseFlags                          uint32
	leaseParentKey                      [16]byte
	mode                                os.FileMode
	posix                               bool
	// lastLink: the name's last component is a link the server followed.
	lastLink bool
}

// init registers the file lifecycle commands.
func init() {
	handlers[cmdCreate] = handleCreate
	handlers[cmdClose] = handleClose
}

// decodeCreateContexts rejects overlapping or out-of-bounds context chains.
func decodeCreateContexts(raw []byte, off, length uint32) ([]createContext, uint32) {
	if length == 0 {
		return nil, statusSuccess
	}
	if off < 64 || off%8 != 0 || uint64(off)+uint64(length) > uint64(len(raw)) {
		return nil, statusInvalidParameter
	}
	b := raw[int(off):int(uint64(off)+uint64(length))]
	var out []createContext
	for {
		if len(b) < 16 {
			return nil, statusInvalidParameter
		}
		next := binary.LittleEndian.Uint32(b)
		span := len(b)
		if next != 0 {
			if next < 16 || next%8 != 0 || uint64(next) > uint64(len(b)) {
				return nil, statusInvalidParameter
			}
			span = int(next)
		}
		no := int(binary.LittleEndian.Uint16(b[4:]))
		nl := int(binary.LittleEndian.Uint16(b[6:]))
		do := int(binary.LittleEndian.Uint16(b[10:]))
		dl := uint64(binary.LittleEndian.Uint32(b[12:]))
		if no < 16 || nl == 0 || no+nl > span || (dl != 0 && (do < 16 || uint64(do)+dl > uint64(span))) {
			return nil, statusInvalidParameter
		}
		name := string(b[no : no+nl])
		for _, c := range out {
			if c.name == name {
				return nil, statusInvalidParameter
			}
		}
		var data []byte
		if dl != 0 {
			data = b[do:int(uint64(do)+dl)]
		}
		out = append(out, createContext{name, data})
		if next == 0 {
			return out, statusSuccess
		}
		b = b[span:]
	}
}

// expandAccess converts generic access flags into file-specific rights.
func expandAccess(v uint32) uint32 {
	if v&0x10000000 != 0 || v&0x02000000 != 0 {
		v |= accessAll
	}
	if v&0x80000000 != 0 {
		v |= 0x00120089
	}
	if v&0x40000000 != 0 {
		v |= 0x00120116
	}
	if v&0x20000000 != 0 {
		v |= 0x001200a0
	}
	return v & 0x01ffffff
}

// parseCreate validates the fixed CREATE request and supported context payloads.
func parseCreate(r *request) (createArgs, uint32) {
	var a createArgs
	b := r.body
	if len(b) < 56 || binary.LittleEndian.Uint16(b) != 57 {
		return a, statusInvalidParameter
	}
	a.oplock = b[3]
	a.access = expandAccess(binary.LittleEndian.Uint32(b[24:]))
	a.share = binary.LittleEndian.Uint32(b[32:])
	a.disposition = binary.LittleEndian.Uint32(b[36:])
	a.options = binary.LittleEndian.Uint32(b[40:])
	if a.disposition > 5 || a.share&^uint32(7) != 0 || a.options&(createDirectory|createNonDirectory) == createDirectory|createNonDirectory {
		return a, statusInvalidParameter
	}
	if a.options&(0x00000080|0x00002000|0x00100000) != 0 {
		return a, statusNotSupported
	}
	if a.options&createDeleteOnClose != 0 && a.access&accessDelete == 0 {
		return a, statusAccessDenied
	}
	if a.oplock != 0 && a.oplock != 1 && a.oplock != 8 && a.oplock != 9 && a.oplock != 255 {
		return a, statusInvalidParameter
	}
	no, nl := int(binary.LittleEndian.Uint16(b[44:])), int(binary.LittleEndian.Uint16(b[46:]))
	if nl%2 != 0 || (nl != 0 && (no < 120 || no+nl > len(r.raw))) {
		return a, statusInvalidParameter
	}
	var name string
	if nl != 0 {
		name = utf16Decode(r.raw[no : no+nl])
	}
	var st uint32
	a.name, st = smbPath(name)
	if st != statusSuccess {
		return a, st
	}
	if a.name == "" && a.options&createDeleteOnClose != 0 {
		return a, statusCannotDelete
	}
	a.contexts, st = decodeCreateContexts(r.raw, binary.LittleEndian.Uint32(b[48:]), binary.LittleEndian.Uint32(b[52:]))
	if st != statusSuccess {
		return a, st
	}
	a.mode = 0o666
	if a.options&createDirectory != 0 {
		a.mode = 0o777
	}
	for _, c := range a.contexts {
		switch c.name {
		case "RqLs":
			if len(c.data) != 32 && len(c.data) != 52 {
				return a, statusInvalidParameter
			}
			if a.oplock != 255 || (len(c.data) == 52 && r.c.dialect < 0x300) || (len(c.data) == 32 && r.c.dialect < 0x210) {
				continue
			}
			copy(a.leaseKey[:], c.data[:16])
			a.leaseState = binary.LittleEndian.Uint32(c.data[16:])
			if a.leaseState&^uint32(7) != 0 {
				return a, statusInvalidParameter
			}
			a.leaseVersion = len(c.data)
			if len(c.data) == 52 {
				a.leaseFlags = binary.LittleEndian.Uint32(c.data[20:]) & 4
				if a.leaseFlags&4 != 0 {
					copy(a.leaseParentKey[:], c.data[32:48])
				}
				a.leaseEpoch = binary.LittleEndian.Uint16(c.data[48:])
			}
		case posixCreateTag:
			if !r.c.posix {
				continue
			}
			if len(c.data) != 4 {
				return a, statusInvalidParameter
			}
			a.posix = true
			m := binary.LittleEndian.Uint32(c.data)
			a.mode = os.FileMode(m & 0o777)
			if m&0o4000 != 0 {
				a.mode |= os.ModeSetuid
			}
			if m&0o2000 != 0 {
				a.mode |= os.ModeSetgid
			}
			if m&0o1000 != 0 {
				a.mode |= os.ModeSticky
			}
		case "AAPL":
			if len(c.data) != 24 || binary.LittleEndian.Uint32(c.data) != 1 {
				return a, statusInvalidParameter
			}
		case "DHnC", "DH2C":
			return a, statusObjectNameNotFound
		case "TWrp":
			return a, statusNotSupported
		case "ExtA", "SecD":
			return a, statusNotSupported
		}
	}
	if a.oplock == 255 && a.leaseVersion == 0 {
		a.oplock = 0
	}
	return a, statusSuccess
}

// shareCompatible performs the symmetric Windows read/write/delete sharing
// check. As on NTFS, an open without read, write, execute or delete access
// takes no part in it: Windows opens the share root for a free-space query
// with no sharing, which must not lock other clients out of listing it.
func shareCompatible(access, share uint32, o *open) bool {
	mask := func(a uint32) uint32 {
		var v uint32
		if a&(accessReadData|0x20) != 0 {
			v |= 1
		}
		if a&(accessWriteData|accessAppendData) != 0 {
			v |= 2
		}
		if a&accessDelete != 0 {
			v |= 4
		}
		return v
	}
	want, held := mask(access), mask(o.access)
	return want == 0 || held == 0 || want&^o.shareAccess == 0 && held&^share == 0
}

// handleCreate opens overlay objects after flushing incompatible client caches.
func handleCreate(r *request) ([]byte, uint32) {
	a, st := parseCreate(r)
	if st != statusSuccess {
		return nil, st
	}
	if r.tree.ipc {
		return createPipe(r, a)
	}
	if a.posix {
		a.options |= createOpenReparse
	}
	body, st, breaks := createAttempt(r, a, nil)
	if len(breaks) == 0 {
		return body, st
	}
	g := &r.c.srv.st.opens
	if r.compound != nil {
		for len(breaks) != 0 {
			if !g.waitBreaks(breaks, nil) {
				return nil, statusCancelled
			}
			body, st, breaks = createAttempt(r, a, nil)
		}
		return body, st
	}
	async := r.async()
	go func() {
		for len(breaks) != 0 {
			if !g.waitBreaks(breaks, async.cancelled) {
				r.complete(async, nil, statusCancelled)
				return
			}
			body, st, breaks = createAttempt(r, a, async.cancelled)
		}
		r.complete(async, body, st)
	}()
	return nil, statusPending
}

// createAttempt validates and commits one CREATE transaction, but never waits with a transaction lock held.
// Re-resolving the original name after every break also handles renames and reparse changes.
func createAttempt(r *request, a createArgs, cancelled <-chan struct{}) ([]byte, uint32, []*oplockGrant) {
	g := &r.c.srv.st.opens
	var unlock func()
	exclusive := a.options&createDeleteOnClose != 0 && a.options&createNonDirectory == 0
	if exclusive {
		// Marking a directory delete-pending must exclude creates below it.
		unlock = g.lockAll()
	} else {
		unlock = g.lockName(a.name)
	}
	defer func() { unlock() }()
	select {
	case <-cancelled:
		return nil, statusCancelled, nil
	default:
	}
	if g.cacheError() != nil {
		return nil, statusUnsuccessful, nil
	}
	epoch := r.c.srv.st.epoch.Load()
	name, info, lastLink, st := resolveCreatePath(r.c.srv.FS, a.name, a.options&createOpenReparse != 0)
	if st == statusSuccess && name != a.name && !exclusive {
		// Links led to a name this transaction does not hold.
		unlock()
		unlock = g.lockAll()
		epoch = r.c.srv.st.epoch.Load()
		name, info, lastLink, st = resolveCreatePath(r.c.srv.FS, a.name, a.options&createOpenReparse != 0)
	}
	if st != statusSuccess {
		return nil, st, nil
	}
	a.name, a.lastLink = name, lastLink
	if g.deletePending(a.name) {
		return nil, statusDeletePending, nil
	}
	if a.oplock == 255 {
		g.mu.Lock()
		invalidKey := false
		for o, grant := range g.grants {
			if grant.lease && grant.key == a.leaseKey && grant.c.clientGUID == r.c.clientGUID {
				o.mu.Lock()
				invalidKey = o.path != a.name
				o.mu.Unlock()
				if invalidKey {
					break
				}
			}
		}
		g.mu.Unlock()
		if invalidKey {
			return nil, statusInvalidParameter, nil
		}
	}
	compatible := true
	for _, o := range g.at(a.name) {
		if !shareCompatible(a.access, a.share, o) {
			compatible = false
		}
	}
	var breaks []*oplockGrant
	if !compatible {
		// HANDLE caching can retain a locally closed handle with restrictive sharing.
		// Revoke it before deciding whether a live sharing violation remains.
		breaks = g.breakDescendants(a.name)
	} else if a.options&createDeleteOnClose != 0 || r.c.srv.WriteCache && (a.disposition == 0 || a.disposition == 4 || a.disposition == 5) {
		breaks = g.breakNamespaceSource(r.c, a.name, a.leaseKey, a.oplock == 255 && a.leaseVersion != 0, info != nil && info.IsDir())
	} else if r.c.srv.WriteCache || a.access&(accessReadData|accessWriteData|accessAppendData|accessExecute|accessDelete) != 0 || a.disposition != 1 {
		breaks = g.breakConflicting(r.c, a.name, a.leaseKey, a.oplock == 255)
	}
	if len(breaks) != 0 {
		return nil, statusPending, breaks
	}
	if !compatible {
		return nil, statusSharingViolation, nil
	}
	if g.cacheError() != nil {
		return nil, statusUnsuccessful, nil
	}
	body, st := finishCreate(r, a, info, epoch)
	return body, st, nil
}

// resolveCreatePath follows relative links in the merged namespace, leaving a
// final reparse-point open intact. It also returns the resolved name's
// metadata as CREATE would read it: nil when the name is missing or ends in an
// absolute link the backend follows itself; and whether the name's last
// component is a link it followed. A backend lookup succeeds only through
// directories, so an existing name, or a missing one in an existing
// directory, costs one or two lookups instead of one per component.
func resolveCreatePath(fs Backend, name string, openReparse bool) (string, os.FileInfo, bool, uint32) {
	info, err := fs.Lstat(name)
	if err == nil && (openReparse || info.Mode()&os.ModeSymlink == 0) {
		return name, info, false, statusSuccess
	}
	if os.IsNotExist(err) && name != "" {
		parent := path.Dir(name)
		if parent == "." {
			parent = ""
		}
		if pi, pe := fs.Lstat(parent); pe == nil && pi.IsDir() {
			return name, nil, false, statusSuccess
		}
	}
	links := 0
	// Links before the last component keep it in place, so the first link
	// followed at the end is the requested name's own.
	lastLink := false
	for start := 0; start < len(name); {
		end := len(name)
		if i := strings.IndexByte(name[start:], '/'); i >= 0 {
			end = start + i
		}
		if end == len(name) && openReparse {
			break
		}
		info, err := fs.Lstat(name[:end])
		if err != nil {
			if os.IsNotExist(err) {
				return name, nil, lastLink, statusSuccess
			}
			return "", nil, false, ntinfo.Status(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if end < len(name) && !info.IsDir() {
				return "", nil, false, statusNotADirectory
			}
			if end == len(name) {
				return name, info, lastLink, statusSuccess
			}
			start = end + 1
			continue
		}
		if links == 40 {
			return "", nil, false, statusTooManyLinks
		}
		links++
		lastLink = lastLink || end == len(name)
		target, err := fs.Readlink(name[:end])
		if err != nil {
			return "", nil, false, ntinfo.Status(err)
		}
		if ntinfo.IsAbs(target) {
			// Absolute targets retain the backend's host-path semantics.
			return name, nil, lastLink, statusSuccess
		}
		tail := ""
		if end < len(name) {
			tail = name[end+1:]
		}
		name = strings.TrimPrefix(path.Clean("/"+path.Join(path.Dir(name[:end]), target, tail)), "/")
		start = 0
	}
	return name, nil, lastLink, statusSuccess
}

// createPipe opens a supported IPC named pipe without touching the overlay.
func createPipe(r *request, a createArgs) ([]byte, uint32) {
	p, st := openPipe(a.name)
	if st != statusSuccess {
		return nil, st
	}
	p.srv = r.c.srv
	o := &open{tree: r.tree, path: "\x00IPC/" + a.name, pipe: p, access: a.access, shareAccess: a.share, options: a.options}
	if !publishOpen(r, o) {
		return nil, statusNetworkNameDeleted
	}
	out := make([]byte, 88)
	binary.LittleEndian.PutUint16(out, 89)
	binary.LittleEndian.PutUint32(out[4:], 1)
	binary.LittleEndian.PutUint32(out[56:], 0x80)
	putOpenID(out[64:], o.id)
	return out, statusSuccess
}

// finishCreate applies the requested disposition inside createAttempt's
// transaction. info is the name's metadata from resolveCreatePath, read at
// epoch, or nil to look it up.
func finishCreate(r *request, a createArgs, info os.FileInfo, epoch uint64) ([]byte, uint32) {
	fs := r.c.srv.FS
	var err error
	resolved := info != nil
	if info == nil {
		if a.options&createOpenReparse != 0 {
			info, err = fs.Lstat(a.name)
		} else {
			info, err = fs.Stat(a.name)
		}
	}
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, ntinfo.Status(err)
	}
	// QUERY_INFO reads the name itself; only an existing name that resolved
	// without following an absolute link has the same metadata here.
	cacheable := resolved || !exists
	if !exists {
		parent := path.Dir(a.name)
		if parent == "." {
			parent = ""
		}
		pi, pe := fs.Stat(parent)
		if pe != nil {
			if os.IsNotExist(pe) {
				return nil, statusObjectPathNotFound
			}
			return nil, ntinfo.Status(pe)
		}
		if !pi.IsDir() {
			return nil, statusNotADirectory
		}
	}
	if exists && a.disposition == 2 {
		return nil, statusObjectNameCollision
	}
	if !exists && (a.disposition == 1 || a.disposition == 4) {
		return nil, statusObjectNameNotFound
	}
	// A directory symlink opened as itself counts as a directory, as on NTFS:
	// RemoveDirectory opens it with FILE_DIRECTORY_FILE.
	if exists && ntinfo.IsDir(info) && a.options&createNonDirectory != 0 {
		return nil, statusFileIsADirectory
	}
	if exists && !ntinfo.IsDir(info) && a.options&createDirectory != 0 {
		return nil, statusNotADirectory
	}
	if exists && info.IsDir() && (a.disposition == 0 || a.disposition == 4 || a.disposition == 5) {
		return nil, statusFileIsADirectory
	}
	truncate := exists && (a.disposition == 0 || a.disposition == 4 || a.disposition == 5)
	if truncate && a.access&(accessWriteData|accessAppendData) == 0 {
		return nil, statusAccessDenied
	}
	if exists && a.disposition == 0 && a.access&accessDelete == 0 {
		return nil, statusAccessDenied
	}
	o := &open{tree: r.tree, path: a.name, access: a.access, shareAccess: a.share, options: a.options, deleteOnClose: a.options&createDeleteOnClose != 0}
	created := !exists
	changed := created || truncate
	if !exists && a.options&createDirectory != 0 {
		if err = fs.MkdirAll(a.name, a.mode); err != nil {
			return nil, ntinfo.Status(err)
		}
		o.isDir = true
	} else if exists && info.IsDir() {
		o.isDir = true
	} else if exists && info.Mode()&os.ModeSymlink != 0 && a.options&createOpenReparse != 0 {
		if truncate {
			return nil, statusAccessDenied
		}
	} else if !changed && a.access&(accessReadData|accessWriteData|accessAppendData|accessExecute) == 0 {
		// An attribute or delete open never touches data: no backend descriptor,
		// which on Windows hosts also spares a CreateFile per client stat.
	} else {
		flags := os.O_RDONLY
		if a.access&(accessWriteData|accessAppendData) != 0 || created || truncate {
			flags = os.O_RDWR
		}
		if created {
			flags |= os.O_CREATE | os.O_EXCL
		}
		if truncate {
			flags |= os.O_TRUNC
		}
		if a.options&createWriteThrough != 0 {
			flags |= os.O_SYNC
		}
		o.file, err = fs.OpenFile(a.name, flags, a.mode)
		if err != nil {
			return nil, ntinfo.Status(err)
		}
		// Copy-up changes the backing inode; existing readers must follow the merged object.
		if flags&os.O_RDWR != 0 {
			changed = true
			for _, existing := range r.c.srv.st.opens.at(a.name) {
				existing.mu.Lock()
				if existing.file != nil && existing.access&(accessWriteData|accessAppendData) == 0 {
					replacement, e := fs.OpenFile(a.name, os.O_RDONLY, 0)
					if e != nil {
						existing.mu.Unlock()
						o.file.Close()
						return nil, ntinfo.Status(e)
					}
					previous := existing.file
					existing.file = replacement
					previous.Close()
				}
				existing.mu.Unlock()
			}
		}
	}
	if created && a.posix {
		if err = fs.Chmod(a.name, a.mode); err != nil {
			if o.file != nil {
				o.file.Close()
			}
			return nil, ntinfo.Status(err)
		}
	}
	if changed {
		r.c.srv.st.changed()
		epoch = r.c.srv.st.epoch.Load()
		if a.options&createOpenReparse != 0 {
			info, err = fs.Lstat(a.name)
		} else {
			info, err = fs.Stat(a.name)
		}
		if err != nil {
			if o.file != nil {
				o.file.Close()
			}
			return nil, ntinfo.Status(err)
		}
	}
	if cacheable {
		o.info, o.infoEpoch, o.infoAt = info, epoch, time.Now()
	}
	if o.deleteOnClose && o.isDir {
		children, e := fs.ReadDir(a.name)
		if e != nil {
			return nil, ntinfo.Status(e)
		}
		if len(children) != 0 {
			return nil, statusDirectoryNotEmpty
		}
	}
	nodeID := fs.FileID(a.name)
	if nodeID == 0 {
		closeImplicit(o)
		return nil, statusInternalError
	}
	g := &r.c.srv.st.opens
	var leaseFlags uint32
	a.leaseEpoch, leaseFlags = g.grantCaching(r.c, o, a)
	a.leaseFlags = leaseFlags
	a.leaseParentKey = [16]byte{}
	// ACKs may race response construction; take one coherent lease-state snapshot.
	g.mu.Lock()
	leaseState, oplock := o.leaseState, o.oplock
	if grant := g.grants[o]; grant != nil && grant.lease {
		leaseState, a.leaseEpoch = grant.state, grant.epoch
		a.leaseParentKey = grant.parentKey
		a.leaseFlags &^= 6
		if a.leaseVersion == 52 && grant.parentKey != [16]byte{} {
			a.leaseFlags |= 4
		}
		if grant.done != nil {
			a.leaseFlags |= 2
		}
	}
	g.mu.Unlock()
	if !publishOpen(r, o) {
		return nil, statusNetworkNameDeleted
	}
	if o.deleteOnClose {
		r.c.srv.st.opens.setDeletePending(a.name, true)
	}
	if created {
		filter := uint32(1)
		if o.isDir {
			filter = 2
		}
		r.c.srv.st.notify.changedFiltered(a.name, notifyAdded, filter)
	}
	if truncate {
		r.c.srv.st.notify.changedFiltered(a.name, notifyModified, 0x18)
	}
	action := uint32(1)
	if created {
		action = 2
	} else if truncate {
		action = 3
		if a.disposition == 0 {
			action = 0
		}
	}
	out := make([]byte, 88)
	binary.LittleEndian.PutUint16(out, 89)
	out[2] = oplock
	if a.lastLink && r.c.dialect >= 0x0300 {
		// SMB2_CREATE_FLAG_REPARSEPOINT: the open went through a link. Without
		// it, Windows files the target's metadata under the link's name for
		// its cache lifetime, so the link stops reading as a link (lstat,
		// readlink, and so realpath in tools such as tsc and bun).
		out[3] = 1
	}
	binary.LittleEndian.PutUint32(out[4:], action)
	appendCreateAttrs(r.c, out[8:], info)
	putOpenID(out[64:], o.id)
	var contexts []createContext
	for _, c := range a.contexts {
		switch c.name {
		case "MxAc":
			d := make([]byte, 8)
			binary.LittleEndian.PutUint32(d[4:], accessAll)
			contexts = append(contexts, createContext{"MxAc", d})
		case "QFid":
			d := make([]byte, 32)
			binary.LittleEndian.PutUint64(d, nodeID)
			copy(d[8:16], r.c.srv.guid[:8])
			contexts = append(contexts, createContext{"QFid", d})
		case "RqLs":
			if a.oplock == 255 {
				d := make([]byte, a.leaseVersion)
				copy(d, a.leaseKey[:])
				binary.LittleEndian.PutUint32(d[16:], leaseState)
				binary.LittleEndian.PutUint32(d[20:], a.leaseFlags&2)
				if len(d) == 52 {
					binary.LittleEndian.PutUint32(d[20:], a.leaseFlags)
					copy(d[32:48], a.leaseParentKey[:])
					binary.LittleEndian.PutUint16(d[48:], a.leaseEpoch)
				}
				contexts = append(contexts, createContext{"RqLs", d})
			}
		case posixCreateTag:
			if a.posix {
				d := appendPosixInfo(nil, a.name, info, nodeID)
				contexts = append(contexts, createContext{posixCreateTag, d[68:]})
			}
		case "AAPL":
			contexts = append(contexts, createContext{"AAPL", appleCreateResponse(r.c, c.data)})
		}
	}
	if len(contexts) != 0 {
		data := appendCreateContexts(contexts)
		binary.LittleEndian.PutUint32(out[80:], 152)
		binary.LittleEndian.PutUint32(out[84:], uint32(len(data)))
		out = append(out, data...)
	}
	return out, statusSuccess
}

// appendCreateAttrs fills CREATE/CLOSE time, allocation, size and attribute fields.
func appendCreateAttrs(c *conn, out []byte, info os.FileInfo) {
	binary.LittleEndian.PutUint64(out, ntinfo.Filetime(ntinfo.BirthTime(info)))
	binary.LittleEndian.PutUint64(out[8:], ntinfo.Filetime(ntinfo.AccessTime(info)))
	binary.LittleEndian.PutUint64(out[16:], ntinfo.Filetime(info.ModTime()))
	binary.LittleEndian.PutUint64(out[24:], ntinfo.Filetime(ntinfo.ChangeTime(info)))
	binary.LittleEndian.PutUint64(out[32:], ntinfo.AllocationSize(info))
	if !info.IsDir() {
		binary.LittleEndian.PutUint64(out[40:], uint64(info.Size()))
	}
	binary.LittleEndian.PutUint32(out[48:], c.fileAttributes(info))
}

// appendCreateContexts encodes aligned contexts and forward offsets.
func appendCreateContexts(contexts []createContext) []byte {
	var out []byte
	for i, c := range contexts {
		start := len(out)
		doff := (16 + len(c.name) + 7) &^ 7
		size := (doff + len(c.data) + 7) &^ 7
		out = append(out, make([]byte, size)...)
		b := out[start:]
		if i+1 < len(contexts) {
			binary.LittleEndian.PutUint32(b, uint32(size))
		}
		binary.LittleEndian.PutUint16(b[4:], 16)
		binary.LittleEndian.PutUint16(b[6:], uint16(len(c.name)))
		binary.LittleEndian.PutUint16(b[10:], uint16(doff))
		binary.LittleEndian.PutUint32(b[12:], uint32(len(c.data)))
		copy(b[16:], c.name)
		copy(b[doff:], c.data)
	}
	return out
}

// appleCreateResponse advertises implemented Unix metadata and case-sensitive naming only.
func appleCreateResponse(c *conn, in []byte) []byte {
	bitmap := binary.LittleEndian.Uint64(in[8:]) & 7
	out := binary.LittleEndian.AppendUint32(nil, 1)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint64(out, bitmap)
	if bitmap&1 != 0 {
		caps := uint64(4 | 8)
		client := binary.LittleEndian.Uint64(in[16:])
		if client&0x10 != 0 {
			caps |= 0x10
			c.appleReadDir.Store(true)
		} else if client&1 != 0 {
			caps |= 1
			c.appleReadDir.Store(true)
		}
		out = binary.LittleEndian.AppendUint64(out, caps)
	}
	if bitmap&2 != 0 {
		out = binary.LittleEndian.AppendUint64(out, 2)
		c.appleCaseSensitive.Store(true)
	}
	if bitmap&4 != 0 {
		out = binary.LittleEndian.AppendUint32(out, 0)
		out = binary.LittleEndian.AppendUint32(out, uint32(utf16Len("ovnfs")))
		out = utf16Append(out, "ovnfs")
	}
	return out
}

// putOpenID serializes the persistent and volatile handle identifiers.
func putOpenID(dst []byte, id fileID) {
	binary.LittleEndian.PutUint64(dst, id.Persistent)
	binary.LittleEndian.PutUint64(dst[8:], id.Volatile)
}

// getOpenID decodes a validated sixteen-byte identifier.
func getOpenID(src []byte) fileID {
	return fileID{binary.LittleEndian.Uint64(src), binary.LittleEndian.Uint64(src[8:])}
}

// publishOpen installs idempotent teardown before making a handle visible.
func publishOpen(r *request, o *open) bool {
	var once sync.Once
	c := r.c
	var release func()
	if fs, ok := c.srv.FS.(interface{ HoldFileID(uint64) func() }); ok && o.pipe == nil {
		id := c.srv.FS.FileID(o.path)
		if id == 0 {
			c.srv.st.opens.dropCaching(o)
			closeImplicit(o)
			return false
		}
		release = fs.HoldFileID(id)
		if fs, ok := c.srv.FS.(interface{ IdentityError() error }); ok && fs.IdentityError() != nil {
			release()
			c.srv.st.opens.dropCaching(o)
			closeImplicit(o)
			return false
		}
	}
	o.closeFn = func() {
		once.Do(func() {
			closeOpen(c, o)
			if release != nil {
				release()
			}
		})
	}
	t := r.tree
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		c.srv.st.opens.dropCaching(o)
		closeImplicit(o)
		return false
	}
	n := resourceSequence.Add(1)
	o.id = fileID{n, n}
	o.tree = t
	if t.files == nil {
		t.files = make(map[fileID]*open)
	}
	t.files[o.id] = o
	c.srv.st.opens.add(o)
	t.mu.Unlock()
	r.setRelated(o.id)
	return true
}

// closeOpen releases all server state and removes a final delete-pending object.
func closeOpen(c *conn, o *open) uint32 {
	g := &c.srv.st.opens
	g.mu.Lock()
	if !g.entries[o] {
		g.mu.Unlock()
		return statusSuccess
	}
	g.mu.Unlock()
	// Namespace removal and CREATE validation share the name's transaction. Do
	// not take it for an unpublished open disposed during a concurrent tree
	// teardown. Renames hold the stripes of both names or everything, so once
	// the open's current name's stripe is held, the name is stable.
	var unlock func()
	for {
		o.mu.Lock()
		p := o.path
		o.mu.Unlock()
		unlock = g.lockName(p)
		o.mu.Lock()
		same := o.path == p
		o.mu.Unlock()
		if same {
			break
		}
		unlock()
	}
	defer unlock()
	g.mu.Lock()
	if !g.entries[o] {
		g.mu.Unlock()
		return statusSuccess
	}
	g.dropLocked(o)
	g.mu.Unlock()
	c.srv.st.locks.releaseAll(o)
	c.srv.st.notify.cancelOpen(o)
	o.mu.Lock()
	p, dir, f := o.path, o.isDir, o.file
	o.file = nil
	o.mu.Unlock()
	st := uint32(statusSuccess)
	if f != nil {
		if err := f.Close(); err != nil {
			g.loseCaching(o, err)
			st = ntinfo.Status(err)
		}
		if o.access&(accessWriteData|accessAppendData) != 0 {
			c.srv.st.changed() // a host can still move the mtime at close
		}
	}
	g.dropCaching(o)
	g.mu.Lock()
	last := len(g.byPath[p]) == 0
	if last && g.pending[p] {
		err := c.srv.FS.Remove(p)
		c.srv.st.changed()
		if err != nil {
			st = ntinfo.Status(err)
		} else {
			filter := uint32(1)
			if dir {
				filter = 2
			}
			c.srv.st.notify.changedFiltered(p, notifyRemoved, filter)
		}
		delete(g.pending, p)
	}
	g.mu.Unlock()
	return st
}

// handleClose returns optional postquery attributes and tears down the open once.
func handleClose(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 24 || binary.LittleEndian.Uint16(b) != 24 {
		return nil, statusInvalidParameter
	}
	o, st := r.openFile(getOpenID(b[8:]))
	if st != statusSuccess {
		return nil, st
	}
	out := make([]byte, 60)
	binary.LittleEndian.PutUint16(out, 60)
	if binary.LittleEndian.Uint16(b[2:])&1 != 0 && o.pipe == nil {
		o.mu.Lock()
		p := o.path
		o.mu.Unlock()
		var info os.FileInfo
		var err error
		if o.options&createOpenReparse != 0 {
			info, err = r.c.srv.FS.Lstat(p)
		} else {
			info, err = r.c.srv.FS.Stat(p)
		}
		if err == nil {
			binary.LittleEndian.PutUint16(out[2:], 1)
			appendCreateAttrs(r.c, out[8:], info)
		}
	}
	r.tree.removeOpen(o.id)
	st = closeOpen(r.c, o)
	if o.closeFn != nil {
		o.closeFn()
	}
	if st != statusSuccess {
		return nil, st
	}
	return out, statusSuccess
}
