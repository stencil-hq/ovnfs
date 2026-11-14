package checkout

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/peer"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// Node returns this daemon's node id (Config.Node).
func (r *Registry) Node() string { return r.node }

// LinkKey names the links a remote base reads through: those of the checkouts
// attached from one folder of one source daemon.
func LinkKey(node, source string) string { return node + " " + source }

// Store returns the object store remote reads are cached in.
func (r *Registry) Store() *store.Store { return r.store }

// remoteConfig is baseConfig for a base attached from another machine: its reads
// go to the git mirror, then to whatever link currently connects its source.
func (r *Registry) remoteConfig(id string, remote *base.Remote) base.Config {
	key := LinkKey(remote.Node, remote.Source)
	config := r.baseConfig(id)
	config.Mirror = r.mirror
	config.Fetch = func(ctx context.Context, wants []base.Want) error {
		if r.config.Fetch == nil {
			return errors.New("this daemon has no links")
		}
		return r.config.Fetch(ctx, key, wants)
	}
	return config
}

// Missing lists, by path and each id once, the blobs that bases attached
// through the link key still read through it (see base.Base.Missing): every
// checkout and snapshot attached from that source, whichever lane holds it.
// A push fetches them so this machine can read all of them without the link.
func (r *Registry) Missing(key string) []base.Want {
	r.mu.RLock()
	var views []*base.Base
	for _, b := range r.bases {
		if m := b.view.Meta().Remote; m != nil && LinkKey(m.Node, m.Source) == key {
			views = append(views, b.view)
		}
	}
	r.mu.RUnlock()
	var wants []base.Want
	for _, v := range views {
		wants = append(wants, v.Missing()...)
	}
	slices.SortFunc(wants, func(x, y base.Want) int { return strings.Compare(x.Path, y.Path) })
	seen := make(map[store.OID]bool, len(wants))
	return slices.DeleteFunc(wants, func(w base.Want) bool {
		dup := seen[w.ID]
		seen[w.ID] = true
		return dup
	})
}

// offerOrigins names, for each repository of a capture with an upstream, a
// commit there the remote machine can fetch committed content from instead of
// asking this one. Repositories git cannot answer for are left out: their
// content still crosses the link.
func offerOrigins(ctx context.Context, d *source.Domains) []git.Origin {
	if d == nil {
		return nil
	}
	var origins []git.Origin
	for _, info := range d.Repos() {
		repo := d.Repo(info.Path)
		if repo == nil {
			continue
		}
		if o, ok, err := repo.Origin(ctx, info.Head); err == nil && ok {
			origins = append(origins, o)
		}
	}
	return origins
}

// Offer snapshots a checkout and describes it for attaching on another machine;
// the returned Blobs serves the snapshot's content to that machine.
func (r *Registry) Offer(ctx context.Context, name, remoteName string) (peer.Offer, peer.Blobs, api.Snapshot, error) {
	if r.node == "" {
		return peer.Offer{}, nil, api.Snapshot{}, failure("unavailable", "this daemon has no node identity to offer checkouts under")
	}
	h, err := r.lookupCheckout(name)
	if err != nil {
		return peer.Offer{}, nil, api.Snapshot{}, err
	}
	meta := h.base.view.Meta()
	if meta.Remote != nil {
		return peer.Offer{}, nil, api.Snapshot{}, failure("invalid", "checkout %s is attached from %s; attach it from that machine", name, meta.Remote.Source)
	}
	snap, err := r.Snap(ctx, api.SnapArgs{Checkout: name, Label: "attach", Wait: true})
	if err != nil {
		return peer.Offer{}, nil, api.Snapshot{}, err
	}
	st := h.base.view.Store()
	files, err := st.Flatten(store.OID(snap.Snapshot.Tree))
	if err != nil {
		return peer.Offer{}, nil, api.Snapshot{}, coded(err)
	}
	view := h.base.view
	entries := make([]peer.Entry, 0, len(files))
	for _, p := range source.SortedPaths(files) {
		f := files[p]
		e := peer.Entry{Path: p, Mode: f.Mode, ID: f.ID, Mtime: snap.Snapshot.Created.UnixNano()}
		if f.Mode != store.ModeDir {
			if info, ok := view.Info(p); ok && leafIs(view, p, f) {
				e.Size, e.Mtime = info.Size, info.Mtime.UnixNano()
			} else if size, err := st.Size(f.ID); err == nil {
				e.Size = size
			}
		}
		entries = append(entries, e)
	}
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	if remoteName == "" {
		remoteName = name
	}
	offer := peer.Offer{Node: r.node, Source: host + ":" + meta.Dir, Name: remoteName, Tree: store.OID(snap.Snapshot.Tree), Lock: h.base.lock, Files: entries, Roots: view.Roots(), Created: snap.Snapshot.Created,
		Origins: offerOrigins(ctx, h.base.view.Domains())}
	return offer, baseBlobs{h.base.view}, snap.Snapshot, nil
}

// leafIs reports whether f is the leaf the base captured at p.
func leafIs(view *base.Base, p string, f store.File) bool {
	leaf, ok := view.Leaf(p)
	return ok && leaf == f
}

// baseBlobs serves an offered snapshot's content: from the store, else from the source folder's git objects.
type baseBlobs struct{ view *base.Base }

// Open streams a stored blob, or reads a repository object.
func (b baseBlobs) Open(id store.OID, p string) (io.ReadCloser, int64, error) {
	st := b.view.Store()
	if st.Has(id) {
		r, err := st.Open(id)
		if err != nil {
			return nil, 0, err
		}
		size, err := r.Seek(0, io.SeekEnd)
		if err == nil {
			_, err = r.Seek(0, io.SeekStart)
		}
		if err != nil {
			r.Close()
			return nil, 0, err
		}
		return r, size, nil
	}
	data, err := b.view.Blob(p, id)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// AttachRemote opens a checkout on a snapshot offered by another machine. Content
// is fetched through the link to that machine on first read. Re-offering the
// same snapshot returns the existing checkout; a new snapshot of the same source
// updates a cache/local-only checkout in place, and otherwise opens next to it.
func (r *Registry) AttachRemote(ctx context.Context, o peer.Offer) (api.Checkout, error) {
	files := make(map[string]store.File, len(o.Files))
	info := make(map[string]source.Info, len(o.Files))
	for _, e := range o.Files {
		files[e.Path] = store.File{Mode: e.Mode, ID: e.ID}
		if e.Mode != store.ModeDir {
			info[e.Path] = source.Info{Mtime: time.Unix(0, e.Mtime), Size: e.Size}
		}
	}
	tree, err := r.store.Build(files)
	if err != nil {
		return api.Checkout{}, failure("invalid", "offer from %s: %v", o.Source, err)
	}
	if tree != o.Tree {
		return api.Checkout{}, failure("invalid", "offer from %s is inconsistent: its files hash to %s, not %s", o.Source, tree, o.Tree)
	}
	set, err := o.Lock.Set()
	if err != nil {
		return api.Checkout{}, failure("invalid", "offer from %s: %v", o.Source, err)
	}
	name := o.Name
	if name == "" {
		name = sanitizeName(path.Base(o.Source))
	}
	// A checkout of that name attached from the same source takes the offer in
	// place while its upper holds only cache paths.
	r.mu.RLock()
	h := r.checkouts[name]
	if h != nil {
		switch m := h.base.view.Meta(); {
		case m.Remote == nil || m.Remote.Node != o.Node || m.Remote.Source != o.Source:
			h = nil
		case m.Tree == o.Tree:
			reply := r.checkoutLocked(h)
			r.mu.RUnlock()
			return reply, nil
		}
	}
	r.mu.RUnlock()
	if h == nil || r.freezeAndLock(h) != nil {
		r.mu.Lock()
	} else {
		if h.base.view.Meta().Tree == o.Tree {
			reply := r.checkoutLocked(h)
			h.freeze.Unlock()
			r.mu.Unlock()
			return reply, nil
		}
		clean, err := r.cacheOnlyLocked(h)
		if err != nil {
			h.freeze.Unlock()
			r.mu.Unlock()
			return api.Checkout{}, coded(err)
		}
		if clean {
			reply, err := r.reattachLocked(ctx, h, o, source.Result{Tree: tree, Files: files, Info: info, Roots: o.Roots})
			h.freeze.Unlock()
			r.mu.Unlock()
			if err == nil {
				// The new base changes whatever the upper does not shadow.
				r.invalidate(name, nil, []string{""})
				r.mirror.want(o.Origins)
			}
			return reply, err
		}
		h.freeze.Unlock()
	}
	if r.closed {
		r.mu.Unlock()
		return api.Checkout{}, failure("io", "registry is closed")
	}
	if r.checkouts[name] != nil || r.reserved[name] {
		name = ""
	}
	if name, err = r.pickNameLocked(name, path.Base(o.Source)); err != nil {
		r.mu.Unlock()
		return api.Checkout{}, err
	}
	id, err := r.newBaseIDLocked()
	if err != nil {
		r.mu.Unlock()
		return api.Checkout{}, coded(err)
	}
	r.operations.Add(1)
	defer r.operations.Done()
	r.reserved[name] = true
	r.intents[name] = diskIntent{Op: "creating", Checkout: name, Layer: id}
	if err = r.persistIntentLocked(); err != nil {
		delete(r.reserved, name)
		delete(r.intents, name)
		r.mu.Unlock()
		return api.Checkout{}, coded(err)
	}
	r.mu.Unlock()
	remote := base.Remote{Node: o.Node, Source: o.Source, Origins: o.Origins}
	config := r.remoteConfig(id, &remote)
	view, err := base.CreateRemote(config, source.Result{Tree: tree, Files: files, Info: info, Roots: o.Roots}, remote)
	if err == nil {
		if err = writeLock(filepath.Join(config.StateDir, lockFile), o.Lock); err != nil {
			view.Close()
		}
	}
	if err != nil {
		return api.Checkout{}, r.abortCreate(name, config.StateDir, err)
	}
	reply, err := r.publishBase(ctx, name, id, &baseRecord{view: view, lock: o.Lock, rules: set}, gitSource{})
	if err != nil {
		return api.Checkout{}, err
	}
	r.mirror.want(o.Origins)
	return reply.Checkout, nil
}

// cacheOnlyLocked checks every retained overlay, including snapshots' cache layers.
func (r *Registry) cacheOnlyLocked(h *checkoutRecord) (bool, error) {
	layers := h.fs.Stack()
	if err := h.fs.Flush(); err != nil {
		return false, err
	}
	upper, err := overlay.IndexUpper(h.upper)
	if err != nil {
		return false, err
	}
	layers = append(layers, upper)
	t, err := newTree(h.base, nil, h.base.view.Meta().Tree, overlayView{h.fs})
	if err != nil {
		return false, err
	}
	clean := true
	for _, layer := range layers {
		t.class = map[string]rules.Class{"": rules.Source}
		err := layer.Walk(func(p string, e overlay.Entry) error {
			if p == "" {
				if e.Opaque {
					clean = false
				}
				return nil
			}
			class := parentClass(t.class, p)
			if class == rules.Source {
				var err error
				if class, err = t.classify(p, e.Info); err != nil {
					return err
				}
			}
			if e.Kind == overlay.KindDir {
				t.class[p] = class
			}
			if class == rules.Cache || class == rules.Local {
				return nil
			}
			// Source directories are only scaffolding for nested cache roots.
			if class == rules.Source && e.Kind == overlay.KindDir && !e.Opaque && len(layer.Children(p)) != 0 {
				return nil
			}
			clean = false
			return nil
		})
		if err != nil {
			return false, err
		}
	}
	return clean, nil
}

// reattachLocked publishes a new base without replacing the live filesystem or its upper.
func (r *Registry) reattachLocked(ctx context.Context, h *checkoutRecord, o peer.Offer, captured source.Result) (api.Checkout, error) {
	if err := checkContext(ctx); err != nil {
		return api.Checkout{}, err
	}
	id, err := r.newBaseIDLocked()
	if err != nil {
		return api.Checkout{}, coded(err)
	}
	set, err := o.Lock.Set()
	if err != nil {
		return api.Checkout{}, coded(err)
	}
	name := h.meta.Name
	r.intents[name] = diskIntent{Op: "creating", Checkout: name, Layer: id}
	if err := r.persistIntentLocked(); err != nil {
		delete(r.intents, name)
		return api.Checkout{}, coded(err)
	}
	remote := base.Remote{Node: o.Node, Source: o.Source, Origins: o.Origins}
	config := r.remoteConfig(id, &remote)
	view, err := base.CreateRemote(config, captured, remote)
	if err == nil {
		err = writeLock(filepath.Join(config.StateDir, lockFile), o.Lock)
	}
	if err != nil {
		if view != nil {
			view.Close()
		}
		delete(r.intents, name)
		return api.Checkout{}, coded(errors.Join(err, os.RemoveAll(config.StateDir), r.persistLocked()))
	}
	oldBase, oldMeta := h.base, cloneCheckoutMeta(h.meta)
	b := &baseRecord{view: view, lock: o.Lock, rules: set}
	r.bases[id] = b
	portable := make(map[string]bool, len(h.stack))
	for _, id := range h.stack {
		portable[id] = r.layers[id].portable
		r.layers[id].portable = true
	}
	h.base, h.meta.Base, h.meta.Head, h.meta.Parent = b, id, nil, nil
	h.fs.Rebase(view.Bottom(h.meta.Created))
	r.retainLocked(b, h.stack)
	r.releaseLocked(oldBase, h.stack)
	delete(r.intents, name)
	if err := r.persistLocked(); err != nil {
		r.retainLocked(oldBase, h.stack)
		r.releaseLocked(b, h.stack)
		h.base, h.meta = oldBase, oldMeta
		h.fs.Rebase(oldBase.view.Bottom(h.meta.Created))
		for id, was := range portable {
			r.layers[id].portable = was
		}
		delete(r.bases, id)
		view.Close()
		return api.Checkout{}, coded(errors.Join(err, os.RemoveAll(config.StateDir), r.persistLocked()))
	}
	// Generation deliberately stays unchanged: export node handles name this lane.
	return r.checkoutLocked(h), nil
}

// newBaseIDLocked allocates an unused base id.
func (r *Registry) newBaseIDLocked() (string, error) {
	for {
		id, err := randomID("b_")
		if err != nil {
			return "", err
		}
		if r.bases[id] == nil && !exists(filepath.Join(r.config.State, "bases", id)) {
			return id, nil
		}
	}
}
