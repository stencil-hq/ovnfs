package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/peer"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// RemoteChanges snapshots an attached checkout and returns its changed source leaves with verified content.
func (r *Registry) RemoteChanges(ctx context.Context, name string, from store.OID) (peer.Pulled, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return peer.Pulled{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(name)
	if err != nil {
		return peer.Pulled{}, err
	}
	if h.base.view.Meta().Remote == nil || h.base.view.Meta().Tree != from {
		return peer.Pulled{}, failure("invalid", "checkout %s is no longer on the attached snapshot", name)
	}
	snap, err := r.Snap(ctx, api.SnapArgs{Checkout: name, Label: "pull", Wait: true})
	if err != nil {
		return peer.Pulled{}, err
	}
	st := h.base.view.Store()
	before, err := st.Flatten(from)
	if err != nil {
		return peer.Pulled{}, coded(err)
	}
	after, err := st.Flatten(store.OID(snap.Snapshot.Tree))
	if err != nil {
		return peer.Pulled{}, coded(err)
	}
	result := peer.Pulled{Snapshot: snap.Snapshot, Base: map[string]store.File{}, Files: map[string]store.File{}, Data: map[store.OID][]byte{}}
	for _, c := range compare(before, after) {
		for _, side := range []struct{ source, dest map[string]store.File }{{before, result.Base}, {after, result.Files}} {
			f, ok := side.source[c.Path]
			if !ok || f.Mode == store.ModeDir {
				continue
			}
			side.dest[c.Path] = f
			if _, ok := result.Data[f.ID]; ok {
				continue
			}
			data, err := h.base.view.Blob(c.Path, f.ID)
			if err != nil {
				return peer.Pulled{}, coded(err)
			}
			result.Data[f.ID] = data
		}
	}
	return result, nil
}

// MergePull merges a remote checkout's source changes since from, the tree the
// local checkout was attached as, into the local checkout, never its source
// folder. Overlapping text changes are written with conflict markers; a path
// whose markers the user resolved merges later changes against the version it
// was marked with (see checkoutMeta.MergeBases).
func (r *Registry) MergePull(ctx context.Context, args api.PullArgs, from store.OID, pulled peer.Pulled) (api.PullReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.PullReply{}, err
	}
	defer resume()
	if err := checkContext(ctx); err != nil {
		return api.PullReply{}, err
	}
	for p := range union(pulled.Base, pulled.Files) {
		if p == "" || p != overlay.Clean(p) || source.IsGitPath(p) || source.IsJJPath(p) || strings.Contains(p, "\\") {
			return api.PullReply{}, failure("invalid", "remote pull contains invalid source path %q", p)
		}
	}
	for _, files := range []map[string]store.File{pulled.Base, pulled.Files} {
		for p, f := range files {
			if f.Mode != store.ModeFile && f.Mode != store.ModeExec && f.Mode != store.ModeSymlink {
				return api.PullReply{}, failure("invalid", "remote pull contains invalid mode for %s", p)
			}
			data, ok := pulled.Data[f.ID]
			if !ok || store.HashBytes("blob", data) != f.ID {
				return api.PullReply{}, failure("invalid", "remote pull contains missing or corrupt content for %s", p)
			}
		}
	}
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.PullReply{}, err
	}
	h.merge.Lock()
	defer h.merge.Unlock()
	source := pullSource(from)
	bases, err := r.mergeBases(h, source)
	if err != nil {
		return api.PullReply{}, coded(err)
	}
	reply, placed, err := r.mergePull(ctx, h, args, pulled, bases)
	if len(placed) != 0 {
		r.invalidate(args.Checkout, placed, nil)
	}
	if err != nil || args.DryRun {
		return reply, err
	}
	read := func(p string, f store.File) ([]byte, error) {
		data, ok := pulled.Data[f.ID]
		if !ok {
			return nil, fmt.Errorf("content of %s (%s) is not in the pull", p, f.ID)
		}
		return data, nil
	}
	if err := r.setMergeBases(h, source, bases, read); err != nil {
		return api.PullReply{}, coded(err)
	}
	return reply, nil
}

// mergePull is MergePull under the checkout's freeze lock, advancing bases as
// it goes; it also returns the paths it wrote, which the caller invalidates
// once the locks are released.
func (r *Registry) mergePull(ctx context.Context, h *checkoutRecord, args api.PullArgs, pulled peer.Pulled, bases map[string]store.File) (reply api.PullReply, placed []string, err error) {
	h.freeze.Lock()
	defer h.freeze.Unlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.checkouts[args.Checkout] != h || h.retired.Load() {
		return api.PullReply{}, nil, failure("not_found", "no checkout %s", args.Checkout)
	}
	if r.closed {
		return api.PullReply{}, nil, failure("io", "registry is closed")
	}
	if h.base.view.Meta().Remote != nil {
		return api.PullReply{}, nil, failure("invalid", "pull needs the local checkout that was attached to another machine")
	}
	reply = api.PullReply{Snapshot: pulled.Snapshot, ApplyReply: api.ApplyReply{Source: filepath.Join(r.config.Mount, args.Checkout), Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}}}
	s := sides{ours: "in the local checkout", theirs: "remotely",
		labels: git.Labels{Ours: "local", Base: "base", Theirs: strings.TrimSpace("remote " + pulled.Snapshot.Checkout)}}
	// The remote reports only paths it changed since the attached tree; of
	// those, a based path merges against its merge base instead.
	against := union(pulled.Base, nil)
	for p, f := range bases {
		_, before := pulled.Base[p]
		if _, now := pulled.Files[p]; before || now {
			against[p] = f
		}
	}
	blob := func(f store.File) ([]byte, error) {
		if data, ok := pulled.Data[f.ID]; ok {
			return data, nil
		}
		return r.store.ReadBlob(f.ID)
	}
	seen := map[string]bool{}
	for _, c := range compare(against, pulled.Files) {
		if err := checkContext(ctx); err != nil {
			return api.PullReply{}, placed, err
		}
		seen[c.Path] = true
		base, theirs := against[c.Path], pulled.Files[c.Path]
		// A parent file or symlink must not redirect a remote path outside the checkout.
		reason := ""
		d := ""
		parts := strings.Split(c.Path, "/")
		for _, component := range parts[:len(parts)-1] {
			d = path.Join(d, component)
			info, err := h.fs.Lstat(d)
			if err == nil && !info.IsDir() {
				reason = "parent is not a directory in the local checkout"
				break
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return api.PullReply{}, placed, coded(err)
			}
		}
		if reason != "" {
			reply.Conflicts = append(reply.Conflicts, api.Conflict{Path: c.Path, Reason: reason})
			continue
		}
		ours, current, isDir, err := overlayLeaf(h.fs, c.Path)
		if err != nil {
			return api.PullReply{}, placed, coded(err)
		}
		if isDir {
			reply.Conflicts = append(reply.Conflicts, api.Conflict{Path: c.Path, Reason: "is a directory in the local checkout"})
			continue
		}
		o, data := skipped, pulled.Data[theirs.ID]
		switch {
		case ours == theirs:
		case ours == base:
			o = applied
		default:
			why, text := s.conflict(base, ours, theirs)
			if !text {
				o, reason = conflict, why
				break
			}
			var baseData []byte
			if base.ID != "" {
				if baseData, err = blob(base); err != nil {
					return api.PullReply{}, placed, coded(fmt.Errorf("%s: %w", c.Path, err))
				}
			}
			data, o, reason = mergeText(s.labels, current, baseData, data, why)
		}
		if !args.DryRun && (o == applied || o == merged || o == marked) {
			placed = append(placed, c.Path)
			if err := placeOverlay(h.fs, c.Path, theirs, data); err != nil {
				return api.PullReply{}, placed, coded(fmt.Errorf("%s: %w", c.Path, err))
			}
		}
		report(&reply.ApplyReply, c, o, reason)
		advance(bases, c.Path, theirs, o)
	}
	err = reportPending(&reply.ApplyReply, bases, seen, func(p string) (bool, error) {
		ours, current, _, err := overlayLeaf(h.fs, p)
		return regular(ours) && hasMarkers(current), err
	})
	if err != nil {
		return api.PullReply{}, placed, coded(err)
	}
	return reply, placed, nil
}

// overlayLeaf reads only the local paths a pull needs to compare.
func overlayLeaf(fs *overlay.FS, p string) (store.File, []byte, bool, error) {
	info, err := fs.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return store.File{}, nil, false, nil
	}
	if err != nil {
		return store.File{}, nil, false, err
	}
	if info.IsDir() {
		return store.File{}, nil, true, nil
	}
	mode, ok := store.ModeOf(info.Mode())
	if !ok {
		return store.File{}, nil, false, fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
	var data []byte
	if mode == store.ModeSymlink {
		target, err := fs.Readlink(p)
		if err != nil {
			return store.File{}, nil, false, err
		}
		data = []byte(target)
	} else {
		file, err := fs.Open(p)
		if err != nil {
			return store.File{}, nil, false, err
		}
		data, err = io.ReadAll(file)
		err = errors.Join(err, file.Close())
		if err != nil {
			return store.File{}, nil, false, err
		}
	}
	return store.File{Mode: mode, ID: store.HashBytes("blob", data)}, data, false, nil
}

// placeOverlay writes through the overlay so its index and deletion markers stay current.
func placeOverlay(fs *overlay.FS, p string, f store.File, data []byte) error {
	if f.ID == "" {
		err := fs.Remove(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := fs.MkdirAll(path.Dir(p), 0o755); err != nil {
		return err
	}
	if old, err := fs.Lstat(p); err == nil && (old.Mode()&os.ModeSymlink != 0 || f.Mode == store.ModeSymlink) {
		if err := fs.Remove(p); err != nil {
			return err
		}
	}
	if f.Mode == store.ModeSymlink {
		return fs.Symlink(string(data), p)
	}
	file, err := fs.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, modeOf(f))
	if err != nil {
		return err
	}
	_, err = io.Copy(file, bytes.NewReader(data))
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return fs.Chmod(p, modeOf(f))
}
