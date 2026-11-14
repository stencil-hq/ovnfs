package export

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/overlay"
)

// recall tells the protocol layer that clients' cached copies of the named
// export paths are stale. Paths no client was ever handed are skipped.
func (n *nodeFS) recall(paths ...string) {
	fn := n.onRecall.Load()
	if fn == nil {
		return
	}
	for _, p := range paths {
		if id, ok := n.nodes.lookup(overlay.Clean(p)); ok {
			(*fn)(id)
		}
	}
}

// recallTree recalls every id issued at or below export path p, without
// checking that the paths still exist.
func (n *nodeFS) recallTree(p string) {
	if fn := n.onRecall.Load(); fn != nil {
		for _, id := range n.nodes.subtree(p) {
			(*fn)(id)
		}
	}
}

// OpenFile recalls cached copies before any open that may change the file.
func (n *nodeFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&routerWriteFlags != 0 {
		n.recall(name)
	}
	return n.openFile(name, flag, perm)
}

// openFile opens name and notes an entry O_CREATE may have made for node ids.
func (n *nodeFS) openFile(name string, flag int, perm os.FileMode) (f billy.File, err error) {
	if err := n.IdentityError(); err != nil {
		return nil, err
	}
	if flag&os.O_CREATE != 0 {
		var release func()
		release, err = n.nodes.guard(false, overlay.Clean(name))
		if err != nil {
			return nil, err
		}
		defer func() {
			release()
			if fault := n.IdentityError(); fault != nil {
				if f != nil {
					f.Close()
					f = nil
				}
				err = fault
			}
		}()
	}

	f, err = n.Filesystem.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	if flag&os.O_CREATE != 0 {
		n.nodes.created(overlay.Clean(name))
	}
	return n.bindFile(name, f)
}

// bindFile pins the identity used to reopen a retained protocol descriptor
// after a freeze, following renames without reviving a removed/replaced file.
func (n *nodeFS) bindFile(name string, f billy.File) (billy.File, error) {
	if file, ok := f.(*routerFile); ok {
		id := n.nodes.id(overlay.Clean(name))
		if id == 0 {
			f.Close()
			return nil, syscall.EIO
		}
		file.identity, file.id, file.unpin = n, id, n.nodes.retain(id)
		if err := n.IdentityError(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// Create truncates through OpenFile so the recall is not bypassed.
func (n *nodeFS) Create(name string) (billy.File, error) {
	return n.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Chmod changes permissions after recalling cached attributes.
func (n *nodeFS) Chmod(name string, mode os.FileMode) error {
	n.recall(name)
	return n.Change.Chmod(name, mode)
}

// Lchown changes a link's ownership after recalling cached attributes.
func (n *nodeFS) Lchown(name string, uid, gid int) error {
	n.recall(name)
	return n.Change.Lchown(name, uid, gid)
}

// Chown changes ownership after recalling cached attributes.
func (n *nodeFS) Chown(name string, uid, gid int) error {
	n.recall(name)
	return n.Change.Chown(name, uid, gid)
}

// Chtimes changes times after recalling cached attributes.
func (n *nodeFS) Chtimes(name string, atime, mtime time.Time) error {
	n.recall(name)
	return n.Change.Chtimes(name, atime, mtime)
}

// OnRecall registers the frontend's cache invalidation hook for stable node ids;
// daemon changes and other filesystem consumers invoke fn, which must not block.
func (e *Export) OnRecall(fn func(id uint64)) { e.fs.onRecall.Store(&fn) }

// Invalidate recalls clients' cached copies of checkout paths the daemon
// changed behind the file protocols (pull, reattach, folder edits), and of
// every handle issued at or below trees ("" covers the whole checkout).
//
// One of paths that no longer exists retires its node, so old handles report
// stale instead of reaching a later file of that name. Handles below trees are
// recalled unchecked, since checking costs a stat for every handle ever issued
// there: one whose path is gone keeps its id, and fails with the backend's
// error when a client next uses it.
func (e *Export) Invalidate(checkout string, paths, trees []string) {
	for _, t := range trees {
		full := path.Join(checkout, t)
		e.fs.nodes.unlist(full, true)
		if t != "" {
			dir, _ := split(full)
			e.fs.nodes.unlist(dir, false)
		}
		e.fs.recallTree(full)
	}
	fn := e.fs.onRecall.Load()
	var issued []string
	for _, p := range paths {
		full := path.Join(checkout, p)
		dir, _ := split(full)
		e.fs.nodes.unlist(full, false)
		e.fs.nodes.unlist(dir, false)
		if id, ok := e.fs.nodes.lookup(full); ok {
			issued = append(issued, full)
			if fn != nil {
				(*fn)(id)
			}
		}
	}
	gone := map[string]bool{}
	for _, p := range issued {
		if under(p, gone) {
			continue
		}
		if _, err := e.fs.Filesystem.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			e.fs.nodes.remove(p)
			gone[p] = true
		}
	}
}

// under reports whether p is one of roots or lies below one, looking up p and
// each of its parents rather than every root.
func under(p string, roots map[string]bool) bool {
	for {
		if roots[p] {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}
