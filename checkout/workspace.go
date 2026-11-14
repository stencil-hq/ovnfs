package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/jj"
	"github.com/stencil-hq/ovnfs/overlay"
)

// A checkout of a folder that is the root of a jj workspace is a jj workspace
// of that repository too, named after the checkout: jj creates it in
// State/jj/NAME, which jj records as the workspace's path, and its .jj moves
// to the root of the checkout's upper. That .jj belongs to the checkout alone:
// snapshots, diffs and apply leave it out, a freeze keeps it in the upper
// instead of the layer, and every fork or checkout from a snapshot gets a
// workspace of its own. Removing the checkout forgets the workspace. jj only
// runs on host paths (see package jj).

// jjWorkspace is a jj workspace made for a new checkout, its .jj still in a
// directory of its own below State/jj; the zero value is none.
type jjWorkspace struct {
	name string // in the repository
	dir  string // holding the .jj until installWorkspace moves it
}

// workspacesDir holds workspaces made for checkouts not yet installed.
func (r *Registry) workspacesDir() string { return filepath.Join(r.config.State, "jj") }

// addWorkspace makes a jj workspace for checkout name when b's folder is the
// root of a jj workspace. A create goes on without one, with a warning, when
// jj is not installed or fails. The caller discards it (discardWorkspace)
// once the create is done, which is a no-op after installWorkspace.
func (r *Registry) addWorkspace(ctx context.Context, b *baseRecord, name string) (jjWorkspace, []string) {
	root := b.view.Meta().Dir
	if root == "" {
		return jjWorkspace{}, nil
	}
	repo, err := jj.Repo(root)
	if err == nil && repo == "" {
		return jjWorkspace{}, nil
	}
	dir := ""
	if err == nil {
		// Only a create that failed to discard its workspace leaves one here.
		dir = filepath.Join(r.workspacesDir(), name)
		r.discardWorkspace(jjWorkspace{dir: dir})
		err = os.Mkdir(dir, 0o700)
	}
	ws := jjWorkspace{dir: dir}
	if err == nil {
		ws.name, err = jj.Add(ctx, root, name, dir)
	}
	if err == nil {
		return ws, nil
	}
	if dir != "" {
		err = errors.Join(err, removeTree(dir))
	}
	if errors.Is(err, jj.ErrNotInstalled) {
		return jjWorkspace{}, []string{fmt.Sprintf("%s is a jj repository, but jj is not installed: checkout %s is no jj workspace", root, name)}
	}
	return jjWorkspace{}, []string{fmt.Sprintf("checkout %s is no jj workspace: %v", name, err)}
}

// installWorkspace makes ws the jj workspace of h, a checkout not yet
// registered: its .jj moves to the root of h's upper, and h's overlay opens
// again to index it.
func (r *Registry) installWorkspace(h *checkoutRecord, ws jjWorkspace) error {
	if ws.dir == "" {
		return nil
	}
	if err := h.fs.Flush(); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(ws.dir, jj.Dir), filepath.Join(h.upper, jj.Dir)); err != nil {
		return err
	}
	h.meta.JJ = ws.name
	if err := removeTree(ws.dir); err != nil {
		return err
	}
	fs, err := overlay.New(h.upper, h.fs.Stack(), h.base.view.Bottom(h.meta.Created), r.config.Memory)
	if err != nil {
		return err
	}
	h.fs = fs
	return nil
}

// discardWorkspace forgets ws in its repository and removes its directory,
// unless installWorkspace moved it into a checkout.
func (r *Registry) discardWorkspace(ws jjWorkspace) {
	if ws.dir != "" {
		r.forgetWorkspace(ws.dir)
		if err := removeTree(ws.dir); err != nil {
			r.enqueueNotice(baseNotice{fault: "remove jj workspace: " + err.Error()})
		}
	}
}

// forgetWorkspace forgets the jj workspace whose .jj is at the root of host
// directory dir, if any, in its repository before dir goes. A failure is a
// notice: it never keeps a checkout from going.
func (r *Registry) forgetWorkspace(dir string) {
	if err := jj.Forget(context.Background(), dir); err != nil {
		r.enqueueNotice(baseNotice{fault: "forget jj workspace in " + dir + ": " + err.Error()})
	}
}

// removeCheckoutFiles forgets the jj workspace of the checkout whose
// directories are named dir, then removes its upper and private git directory.
func (r *Registry) removeCheckoutFiles(dir string) error {
	r.forgetWorkspace(r.upperPath(dir))
	return errors.Join(removeTree(r.upperPath(dir)), removeTree(r.gitdirPath(dir)))
}
