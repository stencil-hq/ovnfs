package fuse

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
	"golang.org/x/sys/unix"
)

// rootID is the kernel's fixed root inode, which is also the export root's node id.
const rootID = gofuse.FUSE_ROOT_ID

// openMask keeps the open(2) flags the export honors on an existing file.
const openMask = os.O_RDONLY | os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_TRUNC

// exportFS answers raw FUSE requests from the export, using stable node ids as
// kernel node ids. Ids persist, so lookups need no reference counts and FORGET
// is a no-op, as with NFS file handles.
type exportFS struct {
	gofuse.RawFileSystem
	nodes  *export.Nodes
	uid    uint32
	gid    uint32
	chown  bool
	kernel *gofuse.Server

	mu      sync.Mutex
	next    uint64
	handles map[uint64]any
}

// fileHandle is an open regular file and the node it was opened through.
type fileHandle struct {
	release func()
	ino     uint64
	file    billy.File
	append  bool
}

// dirHandle is an open directory's listing, snapshotted for stable offsets.
type dirHandle struct {
	release func()
	mu      sync.Mutex
	ino     uint64
	path    string
	read    bool
	entries []dirEntry
}

// dirEntry is one listed name; READDIRPLUS stats it again when answering.
type dirEntry struct {
	name string
	ino  uint64
	mode uint32
}

// newExportFS serves nodes with the daemon's credentials. A root daemon
// reports real owners and gives new entries to the calling user, as a local
// filesystem would. Any other daemon reports every node as its own user's,
// mode bits kept: only that user reaches the mount (no allow_other), and a
// write copies up into a layer it owns whoever owns the source, so the
// source's owner must not make default_permissions refuse it.
func newExportFS(nodes *export.Nodes) *exportFS {
	return &exportFS{
		RawFileSystem: gofuse.NewDefaultRawFileSystem(),
		nodes:         nodes,
		uid:           uint32(os.Geteuid()),
		gid:           uint32(os.Getegid()),
		chown:         os.Geteuid() == 0,
		handles:       make(map[uint64]any),
	}
}

// Init keeps the server for kernel cache invalidations.
func (x *exportFS) Init(s *gofuse.Server) { x.kernel = s }

// String names the filesystem in go-fuse debug output.
func (x *exportFS) String() string { return "ovnfs" }

// path resolves a kernel node id to its current export path.
func (x *exportFS) path(ino uint64) (string, gofuse.Status) {
	if x.nodes.IdentityError() != nil {
		return "", gofuse.EIO
	}
	p, ok := x.nodes.Path(ino)
	if !ok {
		if x.nodes.IdentityError() != nil {
			return "", gofuse.EIO
		}
		return "", gofuse.Status(syscall.ESTALE)
	}
	return p, gofuse.OK
}

// child resolves name inside the directory node parent.
func (x *exportFS) child(parent uint64, name string) (string, gofuse.Status) {
	dir, st := x.path(parent)
	if !st.Ok() {
		return "", st
	}
	return path.Join(dir, name), gofuse.OK
}

// entry describes p as a kernel dentry with short cache lifetimes.
func (x *exportFS) entry(p string, out *gofuse.EntryOut) gofuse.Status {
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return errno(err)
	}
	ino := x.nodes.ID(p)
	if ino == 0 || x.nodes.IdentityError() != nil {
		return gofuse.EIO
	}
	out.NodeId, out.Generation = ino, 0
	x.fill(&out.Attr, ino, info)
	out.SetEntryTimeout(TTL)
	out.SetAttrTimeout(attrTTL(&out.Attr))
	return gofuse.OK
}

// attrTTL is how long the kernel may cache a. Every hard link of a file has
// its own node id, so the kernel cannot see a write through another link;
// multiply linked files are therefore revalidated on every use.
func attrTTL(a *gofuse.Attr) time.Duration {
	if a.Mode&syscall.S_IFMT == syscall.S_IFREG && a.Nlink > 1 {
		return 0
	}
	return TTL
}

// fill converts export attributes to kernel attributes for node ino, with
// the owners newExportFS describes. ctime and atime report mtime, as the NFS
// frontend does.
func (x *exportFS) fill(a *gofuse.Attr, ino uint64, info os.FileInfo) {
	*a = gofuse.Attr{Ino: ino, Mode: unixMode(info.Mode()), Nlink: 1, Blksize: 4096}
	if size := info.Size(); size > 0 {
		a.Size = uint64(size)
		a.Blocks = (a.Size + 511) / 512
	}
	mtime := info.ModTime()
	a.SetTimes(&mtime, &mtime, &mtime)
	if st, ok := unixstat.Of(info); ok {
		if st.Nlink > 0 {
			a.Nlink = st.Nlink
		}
		a.Uid, a.Gid = st.UID, st.GID
		a.Rdev = uint32(unix.Mkdev(st.Major, st.Minor))
	}
	if ino == rootID || !x.chown {
		a.Uid, a.Gid = x.uid, x.gid
	}
}

// unixMode converts a Go file mode to st_mode type and permission bits.
func unixMode(m os.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		mode |= syscall.S_ISUID
	}
	if m&os.ModeSetgid != 0 {
		mode |= syscall.S_ISGID
	}
	if m&os.ModeSticky != 0 {
		mode |= syscall.S_ISVTX
	}
	switch {
	case m.IsDir():
		return mode | syscall.S_IFDIR
	case m&os.ModeSymlink != 0:
		return mode | syscall.S_IFLNK
	case m&os.ModeNamedPipe != 0:
		return mode | syscall.S_IFIFO
	case m&os.ModeSocket != 0:
		return mode | syscall.S_IFSOCK
	case m&os.ModeCharDevice != 0:
		return mode | syscall.S_IFCHR
	case m&os.ModeDevice != 0:
		return mode | syscall.S_IFBLK
	}
	return mode | syscall.S_IFREG
}

// fileMode converts st_mode permission and special bits to a Go file mode.
func fileMode(mode uint32) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&syscall.S_ISUID != 0 {
		m |= os.ModeSetuid
	}
	if mode&syscall.S_ISGID != 0 {
		m |= os.ModeSetgid
	}
	if mode&syscall.S_ISVTX != 0 {
		m |= os.ModeSticky
	}
	return m
}

// errno maps export errors to kernel status codes. The router rejects
// mutations of the export root (its checkout entries) with a bare permission
// error, which FUSE reports as a read-only filesystem; a file whose checkout was
// removed reports closed, which is stale as with NFS handles.
func errno(err error) gofuse.Status {
	if err == nil {
		return gofuse.OK
	}
	var e syscall.Errno
	switch {
	case errors.As(err, &e):
		return gofuse.Status(e)
	case errors.Is(err, fs.ErrNotExist):
		return gofuse.ENOENT
	case errors.Is(err, fs.ErrExist):
		return gofuse.Status(syscall.EEXIST)
	case errors.Is(err, fs.ErrPermission):
		return gofuse.EROFS
	case errors.Is(err, fs.ErrClosed):
		return gofuse.Status(syscall.ESTALE)
	case errors.Is(err, fs.ErrInvalid):
		return gofuse.EINVAL
	case errors.Is(err, billy.ErrNotSupported):
		return gofuse.ENOTSUP
	}
	return gofuse.EIO
}

// add registers an open handle and returns its kernel file handle.
func (x *exportFS) add(h any) uint64 {
	switch h := h.(type) {
	case *fileHandle:
		h.release = x.nodes.HoldFileID(h.ino)
	case *dirHandle:
		h.release = x.nodes.HoldFileID(h.ino)
	}
	if x.nodes.IdentityError() != nil {
		switch h := h.(type) {
		case *fileHandle:
			h.release()
			h.file.Close()
		case *dirHandle:
			h.release()
		}
		return 0
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.next++
	x.handles[x.next] = h
	return x.next
}

// file returns the open regular file behind fh.
func (x *exportFS) file(fh uint64) *fileHandle {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, _ := x.handles[fh].(*fileHandle)
	return f
}

// dir returns the open directory behind fh.
func (x *exportFS) dir(fh uint64) *dirHandle {
	x.mu.Lock()
	defer x.mu.Unlock()
	d, _ := x.handles[fh].(*dirHandle)
	return d
}

// drop forgets fh and returns what it referred to.
func (x *exportFS) drop(fh uint64) any {
	x.mu.Lock()
	h := x.handles[fh]
	delete(x.handles, fh)
	x.mu.Unlock()
	switch h := h.(type) {
	case *fileHandle:
		if h.release != nil {
			h.release()
		}
	case *dirHandle:
		if h.release != nil {
			h.release()
		}
	}
	return h
}

// created applies the caller's exact mode, which the daemon's umask may have
// reduced, and, for a root daemon, the caller's ownership; a setgid parent
// keeps its group, as on a local filesystem.
func (x *exportFS) created(p string, mode uint32, caller *gofuse.Caller, out *gofuse.EntryOut) gofuse.Status {
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return errno(err)
	}
	want := fileMode(mode)
	const bits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if info.Mode()&os.ModeSymlink == 0 && info.Mode()&bits != want {
		if err := x.nodes.Chmod(p, want); err != nil {
			return errno(err)
		}
	}
	if x.chown {
		gid := int(caller.Gid)
		if dir, err := x.nodes.Lstat(path.Dir(p)); err == nil && dir.Mode()&os.ModeSetgid != 0 {
			gid = -1
		}
		if err := x.nodes.Lchown(p, int(caller.Uid), gid); err != nil {
			return errno(err)
		}
	}
	return x.entry(p, out)
}

// Lookup resolves one name and returns its stable node id.
func (x *exportFS) Lookup(_ <-chan struct{}, in *gofuse.InHeader, name string, out *gofuse.EntryOut) gofuse.Status {
	p, st := x.child(in.NodeId, name)
	if !st.Ok() {
		return st
	}
	return x.entry(p, out)
}

// GetAttr reports a node's attributes, falling back to an open file of the
// node once its name is gone (unlinked or replaced while open). The kernel
// sends no file handle for fstat(2), so any open handle of the node serves.
func (x *exportFS) GetAttr(_ <-chan struct{}, in *gofuse.GetAttrIn, out *gofuse.AttrOut) gofuse.Status {
	return x.attr(in.NodeId, in.Fh(), in.Flags()&gofuse.FUSE_GETATTR_FH != 0, out)
}

// attr reports node ino's attributes by name, else through an open file.
func (x *exportFS) attr(ino, fh uint64, hasFh bool, out *gofuse.AttrOut) gofuse.Status {
	if x.nodes.IdentityError() != nil {
		return gofuse.EIO
	}
	info, st := x.stat(ino)
	if !st.Ok() {
		if x.nodes.IdentityError() != nil {
			return gofuse.EIO
		}
		var f *fileHandle
		if hasFh {
			f = x.file(fh)
		}
		if f == nil {
			f = x.openFile(ino)
		}
		if f == nil {
			return st
		}
		s, ok := f.file.(interface{ Stat() (os.FileInfo, error) })
		if !ok {
			return st
		}
		var err error
		if info, err = s.Stat(); err != nil {
			return errno(err)
		}
	}
	x.fill(&out.Attr, ino, info)
	out.SetTimeout(attrTTL(&out.Attr))
	return gofuse.OK
}

// openFile returns any open file of node ino.
func (x *exportFS) openFile(ino uint64) *fileHandle {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, h := range x.handles {
		if f, ok := h.(*fileHandle); ok && f.ino == ino {
			return f
		}
	}
	return nil
}

// stat describes the node's current name.
func (x *exportFS) stat(ino uint64) (os.FileInfo, gofuse.Status) {
	p, st := x.path(ino)
	if !st.Ok() {
		return nil, st
	}
	info, err := x.nodes.Lstat(p)
	return info, errno(err)
}

// SetAttr applies truncate, chmod, chown, and utimens in that order. Only
// truncation works through an open file once the node's name is gone.
func (x *exportFS) SetAttr(_ <-chan struct{}, in *gofuse.SetAttrIn, out *gofuse.AttrOut) gofuse.Status {
	fh, hasFh := in.GetFh()
	p, st := x.path(in.NodeId)
	if size, ok := in.GetSize(); ok {
		if err := x.truncate(fh, hasFh, p, st, int64(size)); err != nil {
			return errno(err)
		}
	}
	if !st.Ok() && in.Valid&^(gofuse.FATTR_SIZE|gofuse.FATTR_FH|gofuse.FATTR_LOCKOWNER) != 0 {
		return st
	}
	if mode, ok := in.GetMode(); ok {
		if err := x.nodes.Chmod(p, fileMode(mode)); err != nil {
			return errno(err)
		}
	}
	uid, uok := in.GetUID()
	gid, gok := in.GetGID()
	if uok || gok {
		u, g := -1, -1
		if uok {
			u = int(uid)
		}
		if gok {
			g = int(gid)
		}
		if err := x.nodes.Lchown(p, u, g); err != nil {
			return errno(err)
		}
	}
	atime, aok := in.GetATime()
	mtime, mok := in.GetMTime()
	if aok || mok {
		if err := x.utimens(p, atime, aok, mtime, mok); err != nil {
			return errno(err)
		}
	}
	return x.attr(in.NodeId, fh, hasFh, out)
}

// truncate resizes through the caller's open file when given, else by path.
func (x *exportFS) truncate(fh uint64, hasFh bool, p string, st gofuse.Status, size int64) error {
	if err := x.nodes.IdentityError(); err != nil {
		return err
	}
	if hasFh {
		if f := x.file(fh); f != nil {
			return f.file.Truncate(size)
		}
	}
	if !st.Ok() {
		return syscall.Errno(st)
	}
	f, err := x.nodes.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	err = f.Truncate(size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// utimens sets the given times, keeping mtime for an unspecified one. The
// export only changes times through symlinks, so a symlink's own times are
// left alone rather than altering its target.
func (x *exportFS) utimens(p string, atime time.Time, aok bool, mtime time.Time, mok bool) error {
	info, err := x.nodes.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return err
	}
	if !aok {
		atime = info.ModTime()
	}
	if !mok {
		mtime = info.ModTime()
	}
	return x.nodes.Chtimes(p, atime, mtime)
}

// Mknod creates regular files, sockets and FIFOs.
func (x *exportFS) Mknod(_ <-chan struct{}, in *gofuse.MknodIn, name string, out *gofuse.EntryOut) gofuse.Status {
	p, st := x.child(in.NodeId, name)
	if !st.Ok() {
		return st
	}
	var err error
	switch in.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		var f billy.File
		f, err = x.nodes.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode(in.Mode))
		if err == nil {
			err = f.Close()
		}
	case syscall.S_IFSOCK:
		err = x.nodes.Mknod(p, fileMode(in.Mode)|os.ModeSocket)
	case syscall.S_IFIFO:
		err = x.nodes.Mknod(p, fileMode(in.Mode)|os.ModeNamedPipe)
	default:
		return gofuse.EPERM
	}
	if err != nil {
		return errno(err)
	}
	return x.created(p, in.Mode, &in.Caller, out)
}

// Mkdir creates one directory, failing if the name exists.
func (x *exportFS) Mkdir(_ <-chan struct{}, in *gofuse.MkdirIn, name string, out *gofuse.EntryOut) gofuse.Status {
	p, st := x.child(in.NodeId, name)
	if !st.Ok() {
		return st
	}
	if _, err := x.nodes.Lstat(p); err == nil {
		return gofuse.Status(syscall.EEXIST)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errno(err)
	}
	if err := x.nodes.MkdirAll(p, fileMode(in.Mode)); err != nil {
		return errno(err)
	}
	return x.created(p, in.Mode, &in.Caller, out)
}

// Unlink removes a non-directory name.
func (x *exportFS) Unlink(_ <-chan struct{}, in *gofuse.InHeader, name string) gofuse.Status {
	return x.remove(in.NodeId, name, false)
}

// Rmdir removes an empty directory.
func (x *exportFS) Rmdir(_ <-chan struct{}, in *gofuse.InHeader, name string) gofuse.Status {
	return x.remove(in.NodeId, name, true)
}

// remove checks the entry kind that unlink(2) or rmdir(2) requires, then removes it.
func (x *exportFS) remove(parent uint64, name string, dir bool) gofuse.Status {
	p, st := x.child(parent, name)
	if !st.Ok() {
		return st
	}
	info, err := x.nodes.Lstat(p)
	if err != nil {
		return errno(err)
	}
	if info.IsDir() != dir {
		if dir {
			return gofuse.ENOTDIR
		}
		return gofuse.Status(syscall.EISDIR)
	}
	return errno(x.nodes.Remove(p))
}

// Rename moves a name within one checkout; RENAME_NOREPLACE is honored and
// RENAME_EXCHANGE is unsupported.
func (x *exportFS) Rename(_ <-chan struct{}, in *gofuse.RenameIn, oldName, newName string) gofuse.Status {
	if in.Flags&^unix.RENAME_NOREPLACE != 0 {
		return gofuse.EINVAL
	}
	from, st := x.child(in.NodeId, oldName)
	if !st.Ok() {
		return st
	}
	to, st := x.child(in.Newdir, newName)
	if !st.Ok() {
		return st
	}
	if in.Flags&unix.RENAME_NOREPLACE != 0 {
		if _, err := x.nodes.Lstat(to); err == nil {
			return gofuse.Status(syscall.EEXIST)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return errno(err)
		}
	}
	return errno(x.nodes.Rename(from, to))
}

// Link creates a hard link within one checkout; the new name gets its own node id.
func (x *exportFS) Link(_ <-chan struct{}, in *gofuse.LinkIn, name string, out *gofuse.EntryOut) gofuse.Status {
	from, st := x.path(in.Oldnodeid)
	if !st.Ok() {
		return st
	}
	to, st := x.child(in.NodeId, name)
	if !st.Ok() {
		return st
	}
	if err := x.nodes.Link(from, to); err != nil {
		return errno(err)
	}
	// The source's cached attributes were given a TTL while it had one link;
	// drop them so writes through the new name show (see attrTTL).
	x.kernel.InodeNotify(in.Oldnodeid, -1, 0)
	return x.entry(to, out)
}

// Symlink stores target verbatim.
func (x *exportFS) Symlink(_ <-chan struct{}, in *gofuse.InHeader, target, name string, out *gofuse.EntryOut) gofuse.Status {
	p, st := x.child(in.NodeId, name)
	if !st.Ok() {
		return st
	}
	if err := x.nodes.Symlink(target, p); err != nil {
		return errno(err)
	}
	return x.created(p, 0, &in.Caller, out)
}

// Readlink returns a symlink's stored target.
func (x *exportFS) Readlink(_ <-chan struct{}, in *gofuse.InHeader) ([]byte, gofuse.Status) {
	p, st := x.path(in.NodeId)
	if !st.Ok() {
		return nil, st
	}
	target, err := x.nodes.Readlink(p)
	if err != nil {
		return nil, errno(err)
	}
	return []byte(target), gofuse.OK
}

// Access allows everything: the kernel checks permissions (default_permissions).
func (x *exportFS) Access(<-chan struct{}, *gofuse.AccessIn) gofuse.Status { return gofuse.OK }

// Create creates and opens a regular file, or opens it if it already exists
// and O_EXCL is absent, keeping its mode and owner.
func (x *exportFS) Create(_ <-chan struct{}, in *gofuse.CreateIn, name string, out *gofuse.CreateOut) gofuse.Status {
	p, st := x.child(in.NodeId, name)
	if !st.Ok() {
		return st
	}
	flags := int(in.Flags)&(openMask|os.O_EXCL) | os.O_CREATE
	_, err := x.nodes.Lstat(p)
	existed := err == nil
	f, err := x.nodes.OpenFile(p, flags, fileMode(in.Mode))
	if err != nil {
		return errno(err)
	}
	if existed {
		st = x.entry(p, &out.EntryOut)
	} else {
		st = x.created(p, in.Mode, &in.Caller, &out.EntryOut)
	}
	if !st.Ok() {
		f.Close()
		return st
	}
	out.Fh = x.add(&fileHandle{ino: out.NodeId, file: f, append: flags&os.O_APPEND != 0})
	if out.Fh == 0 {
		return gofuse.EIO
	}
	return gofuse.OK
}

// Open opens an existing file. The kernel drops its page cache on every open
// (no FOPEN_KEEP_CACHE), since snapshots and forks change files underneath.
func (x *exportFS) Open(_ <-chan struct{}, in *gofuse.OpenIn, out *gofuse.OpenOut) gofuse.Status {
	p, st := x.path(in.NodeId)
	if !st.Ok() {
		return st
	}
	flags := int(in.Flags) & openMask
	f, err := x.nodes.OpenFile(p, flags, 0)
	if err != nil {
		return errno(err)
	}
	out.Fh = x.add(&fileHandle{ino: in.NodeId, file: f, append: flags&os.O_APPEND != 0})
	if out.Fh == 0 {
		return gofuse.EIO
	}
	return gofuse.OK
}

// Read reads at the kernel's offset.
func (x *exportFS) Read(_ <-chan struct{}, in *gofuse.ReadIn, buf []byte) (gofuse.ReadResult, gofuse.Status) {
	if x.nodes.IdentityError() != nil {
		return nil, gofuse.EIO
	}
	f := x.file(in.Fh)
	if f == nil {
		return nil, gofuse.EBADF
	}
	n, err := f.file.ReadAt(buf[:min(int(in.Size), len(buf))], int64(in.Offset))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errno(err)
	}
	return gofuse.ReadResultData(buf[:n]), gofuse.OK
}

// Write writes one buffer; see Writev.
func (x *exportFS) Write(cancel <-chan struct{}, in *gofuse.WriteIn, data []byte) (uint32, gofuse.Status) {
	return x.Writev(cancel, in, [][]byte{data})
}

// Writev writes at the kernel's offset, or at the current end of file for
// O_APPEND handles, where the kernel's cached size may be stale.
func (x *exportFS) Writev(_ <-chan struct{}, in *gofuse.WriteIn, data [][]byte) (uint32, gofuse.Status) {
	if x.nodes.IdentityError() != nil {
		return 0, gofuse.EIO
	}
	f := x.file(in.Fh)
	if f == nil {
		return 0, gofuse.EBADF
	}
	at, positioned := f.file.(io.WriterAt)
	if !f.append && !positioned {
		return 0, gofuse.EBADF
	}
	off := int64(in.Offset)
	var written uint32
	for _, b := range data {
		var n int
		var err error
		if f.append {
			n, err = f.file.Write(b)
		} else {
			n, err = at.WriteAt(b, off)
		}
		written += uint32(n)
		off += int64(n)
		if err != nil {
			if written > 0 {
				return written, gofuse.OK
			}
			return 0, errno(err)
		}
	}
	return written, gofuse.OK
}

// Flush is a no-op: writes go straight to the export.
func (x *exportFS) Flush(<-chan struct{}, *gofuse.FlushIn) gofuse.Status { return gofuse.OK }

// Fsync makes an open file's data durable.
func (x *exportFS) Fsync(_ <-chan struct{}, in *gofuse.FsyncIn) gofuse.Status {
	if x.nodes.IdentityError() != nil {
		return gofuse.EIO
	}
	f := x.file(in.Fh)
	if f == nil {
		return gofuse.EBADF
	}
	if s, ok := f.file.(interface{ Sync() error }); ok {
		return errno(s.Sync())
	}
	return gofuse.OK
}

// Release closes an open file.
func (x *exportFS) Release(_ <-chan struct{}, in *gofuse.ReleaseIn) {
	if f, ok := x.drop(in.Fh).(*fileHandle); ok {
		f.file.Close()
	}
}

// OpenDir snapshots a directory listing for offset-stable reads.
func (x *exportFS) OpenDir(_ <-chan struct{}, in *gofuse.OpenIn, out *gofuse.OpenOut) gofuse.Status {
	p, st := x.path(in.NodeId)
	if !st.Ok() {
		return st
	}
	d := &dirHandle{ino: in.NodeId, path: p}
	if err := x.list(d); err != nil {
		return errno(err)
	}
	out.Fh = x.add(d)
	if out.Fh == 0 {
		return gofuse.EIO
	}
	return gofuse.OK
}

// list loads d's entries, starting with "." and "..".
func (x *exportFS) list(d *dirHandle) error {
	infos, err := x.nodes.ReadDir(d.path)
	if err != nil {
		return err
	}
	parent := uint64(rootID)
	if d.path != "" {
		parent = x.nodes.ID(parentPath(d.path))
	}
	if parent == 0 {
		return syscall.EIO
	}
	entries := make([]dirEntry, 0, len(infos)+2)
	entries = append(entries,
		dirEntry{name: ".", ino: d.ino, mode: syscall.S_IFDIR},
		dirEntry{name: "..", ino: parent, mode: syscall.S_IFDIR})
	for _, info := range infos {
		p := path.Join(d.path, info.Name())
		ino := x.nodes.ID(p)
		if ino == 0 || x.nodes.IdentityError() != nil {
			return syscall.EIO
		}
		entries = append(entries, dirEntry{name: info.Name(), ino: ino, mode: unixMode(info.Mode())})
	}
	d.entries = entries
	return nil
}

// parentPath returns the export path of p's parent directory.
func parentPath(p string) string {
	if dir := path.Dir(p); dir != "." {
		return dir
	}
	return ""
}

// ReadDir lists names from the snapshot.
func (x *exportFS) ReadDir(_ <-chan struct{}, in *gofuse.ReadIn, out *gofuse.DirEntryList) gofuse.Status {
	return x.readDir(in, out, false)
}

// ReadDirPlus lists names from the snapshot with their current attributes.
func (x *exportFS) ReadDirPlus(_ <-chan struct{}, in *gofuse.ReadIn, out *gofuse.DirEntryList) gofuse.Status {
	return x.readDir(in, out, true)
}

// readDir resumes the listing at the kernel's offset, refreshing it on rewind.
func (x *exportFS) readDir(in *gofuse.ReadIn, out *gofuse.DirEntryList, plus bool) gofuse.Status {
	d := x.dir(in.Fh)
	if d == nil {
		return gofuse.EBADF
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if in.Offset == 0 && d.read {
		if err := x.list(d); err != nil {
			return errno(err)
		}
	}
	d.read = true
	for i := in.Offset; i < uint64(len(d.entries)); i++ {
		e := d.entries[i]
		de := gofuse.DirEntry{Name: e.name, Mode: e.mode, Ino: e.ino, Off: i + 1}
		if !plus {
			if !out.AddDirEntry(de) {
				break
			}
			continue
		}
		eo := out.AddDirLookupEntry(de)
		if eo == nil {
			break
		}
		if i >= 2 { // "." and ".." carry no attributes
			x.lookupEntry(d.path, e, eo)
		}
	}
	return gofuse.OK
}

// lookupEntry fills eo with the current attributes of e in directory dir. The
// kernel applies READDIRPLUS attributes over every write it saw before sending
// the request, so they come from a fresh stat, never the OPENDIR snapshot. A
// name that is gone or now names another node is listed without attributes;
// the kernel then looks it up on use.
func (x *exportFS) lookupEntry(dir string, e dirEntry, eo *gofuse.EntryOut) {
	p := path.Join(dir, e.name)
	info, err := x.nodes.Lstat(p)
	if err != nil || x.nodes.ID(p) != e.ino {
		return
	}
	eo.NodeId = e.ino
	x.fill(&eo.Attr, e.ino, info)
	eo.SetEntryTimeout(TTL)
	eo.SetAttrTimeout(attrTTL(&eo.Attr))
}

// ReleaseDir drops a directory snapshot.
func (x *exportFS) ReleaseDir(in *gofuse.ReleaseIn) { x.drop(in.Fh) }

// FsyncDir is a no-op: directory changes are applied synchronously.
func (x *exportFS) FsyncDir(<-chan struct{}, *gofuse.FsyncIn) gofuse.Status { return gofuse.OK }

// StatFs reports the upper volume, as NFS FSSTAT and SMB do.
func (x *exportFS) StatFs(_ <-chan struct{}, _ *gofuse.InHeader, out *gofuse.StatfsOut) gofuse.Status {
	st, err := x.nodes.Statfs()
	if err != nil {
		return errno(err)
	}
	*out = gofuse.StatfsOut{
		Blocks: st.Blocks, Bfree: st.Free, Bavail: st.Avail,
		Files: st.Files, Ffree: st.FilesFree,
		Bsize: st.BlockSize, Frsize: st.BlockSize, NameLen: 255,
	}
	return gofuse.OK
}
