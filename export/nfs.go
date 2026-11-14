package export

import (
	"time"

	"github.com/stencil-hq/ovnfs/nfs"
	"github.com/stencil-hq/ovnfs/overlay"
)

// nfsFS is the export as the NFS server sees it.
type nfsFS struct{ protocolFS }

// NFS exposes the routed filesystem to the NFS server. Its requests skip the
// export's recall hook: the server recalls the delegations an NFS request
// conflicts with itself, and recalling the requesting client's own delegation
// races with the reply that granted it.
func (e *Export) NFS() nfs.Backend { return &nfsFS{e.protocolFS()} }

// ChildID identifies a listed child without constructing its full path.
func (n *nfsFS) ChildID(dir, name string) uint64 {
	return n.nodes.childID(dir, name)
}

// Path resolves a live node id, which NFS file handles carry, to its path.
func (n *nfsFS) Path(id uint64) (string, bool) {
	return n.nodes.path(id)
}

// RetireHandle makes an existing NFS handle stale without removing its path.
func (n *nfsFS) RetireHandle(id uint64) bool {
	return n.nodes.retireHandle(id)
}

// Rename advances a regular file's mtime to stand in for the ctime change
// rename(2) implies: the NFS server reports ctime as mtime. macOS NFS needs
// that change to invalidate cached linker output, even when executable bits
// are restored only after renaming the temporary file.
//
// Other protocols must preserve mtime on rename; SMB clients may still have
// dirty output pages cached when they issue it.
func (n *nfsFS) Rename(from, to string) error {
	if err := n.nodeFS.Rename(from, to); err != nil {
		return err
	}
	if overlay.Clean(from) == overlay.Clean(to) {
		return nil
	}
	info, err := n.Filesystem.Lstat(to)
	if err != nil || !info.Mode().IsRegular() {
		return err
	}
	mtime := info.ModTime().Add(time.Nanosecond)
	return n.Change.Chtimes(to, mtime, mtime)
}

// The adapter is a complete NFS backend.
var _ nfs.Backend = (*nfsFS)(nil)
