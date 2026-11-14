package nfs

import (
	"github.com/go-git/go-billy/v5"

	"github.com/stencil-hq/ovnfs/smb"
)

// Backend is the filesystem a Server exports. Paths are overlay-clean and
// relative, "" being the root; every method must be safe for concurrent use.
type Backend interface {
	billy.Filesystem
	billy.Change
	// Link makes newname another name of the file oldname. Errors keep their
	// errno (EXDEV, EMLINK, ENOTSUP, ...), which clients see as statuses.
	Link(oldname, newname string) error
	// FileID returns p's stable id, allocating one on first use. File handles
	// and fileids carry it, so it must follow p across renames.
	FileID(p string) uint64
	// ChildID identifies one basename under clean directory dir without
	// requiring callers to construct a complete child path.
	ChildID(dir, name string) uint64
	// Path returns the current path of a live id; false makes the id's
	// handles stale.
	Path(id uint64) (string, bool)
	// RetireHandle makes id stale while preserving its path for a new identity.
	// It returns false when id is already stale or names the export root.
	RetireHandle(id uint64) bool
	// Cacheable permits NFSv4 read delegations on p. It must return true only
	// when every change to p made outside this server reaches Server.Recall.
	Cacheable(p string) bool
	// Statfs reports the capacity FSSTAT and the NFSv4 space attributes show.
	Statfs() (smb.Statfs, error)
}

// identityError lets a backend fail closed when stable identities cannot be
// read. Backends without a separate identity store need no hook.
func identityError(fs Backend) error {
	if fs, ok := fs.(interface{ IdentityError() error }); ok {
		return fs.IdentityError()
	}
	return nil
}
