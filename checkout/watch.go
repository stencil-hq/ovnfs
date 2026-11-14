package checkout

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/unixstat"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/watch"
)

// Tracked reports whether every change to path p of checkout name reaches the
// daemon, so a file protocol may let clients cache it until recalled: false
// only for paths served live from a source folder the watcher does not cover
// right now.
func (r *Registry) Tracked(name, p string) bool {
	r.mu.RLock()
	h := r.checkouts[name]
	r.mu.RUnlock()
	if h == nil || h.retired.Load() {
		return false
	}
	if !h.base.view.Live(p) {
		return true
	}
	return r.watcher != nil && r.watcher.Covers(h.base.view.Meta().Dir)
}

// WriteCacheable reports whether p is isolated data owned by the checkout.
// Only singly linked regular files in its writable upper qualify, whether held
// in memory or on disk. Classification does not affect ownership: an upper
// shadows even a live cache/local lower or a hidden path. Lower files, frozen
// layers and git/jj metadata never qualify. The client gate protects uppers
// through direct operations, including flush, snapshot and reattachment.
// This is a read-only lease-policy check, not an operation barrier. In particular
// it must not Quiesce or take the freeze lock: the file server may already hold a
// Session while deciding a lease, and pending drains must still make progress.
func (r *Registry) WriteCacheable(name, p string) bool {
	if p == "" || p != overlay.Clean(p) || strings.ContainsRune(p, '\\') || source.IsGitPath(p) || source.IsJJPath(p) {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	h := r.checkouts[name]
	if r.closed || h == nil || h.retired.Load() {
		return false
	}
	for at := path.Dir(p); at != "."; at = path.Dir(at) {
		info, err := h.fs.Lstat(at)
		if err != nil || !info.IsDir() {
			return false
		}
	}
	info, ok := h.fs.UpperFile(p)
	if !ok {
		return false
	}
	if stat, ok := unixstat.Of(info); !ok || stat.Nlink != 1 {
		return false
	}
	return true
}

// invalidate reports daemon-side changes to checkout paths, and anywhere in
// trees ("" for the whole checkout), so the export can recall clients' cached
// copies. Callers hold no registry locks: the export reads the checkout to
// retire vanished paths.
func (r *Registry) invalidate(name string, paths, trees []string) {
	if r.config.Invalidate != nil {
		r.config.Invalidate(name, paths, trees)
	}
}

// wakeWatch asks the watch loop to resynchronize the watched folders after the
// set of bases changed.
func (r *Registry) wakeWatch() {
	select {
	case r.watchWake <- struct{}{}:
	default:
	}
}

// watchFolders keeps the folder watcher on exactly the source folders of local
// bases and of folders whose captures are reused (see reuseFolder), off the registry
// lock: adding or removing a folder may wait for an in-flight event batch, which
// itself reads the registry.
func (r *Registry) watchFolders() {
	defer r.workers.Done()
	for {
		select {
		case <-r.stop:
			return
		case <-r.watchWake:
		}
		r.watchMu.Lock()
		r.mu.RLock()
		want := make(map[string]bool, len(r.bases)+len(r.folders))
		for _, b := range r.bases {
			if m := b.view.Meta(); m.Remote == nil && m.Dir != "" {
				want[m.Dir] = true
			}
		}
		for root := range r.folders {
			want[root] = true
		}
		r.mu.RUnlock()
		for dir := range r.watched {
			if !want[dir] {
				r.watcher.Remove(dir)
				delete(r.watched, dir)
			}
		}
		for dir := range want {
			if !r.watched[dir] {
				r.addWatchLocked(dir)
			}
		}
		r.watchMu.Unlock()
	}
}

// watchFolder starts watching folder dir now, rather than when the watch loop
// next runs, and reports whether every change below it reaches folderChanged.
// The caller holds no registry lock and has registered dir in r.folders, which
// keeps the loop from dropping it.
func (r *Registry) watchFolder(dir string) bool {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	if !r.watched[dir] {
		r.addWatchLocked(dir)
	}
	return r.watcher.Covers(dir)
}

// addWatchLocked gives the watcher dir; r.watchMu is held.
func (r *Registry) addWatchLocked(dir string) {
	if err := r.watcher.Add(dir); err != nil {
		r.enqueueNotice(baseNotice{fault: "watch " + dir + ": " + err.Error()})
		return
	}
	r.watched[dir] = true
}

// folderChanged records folder edits that can change a capture (see reuseFolder), and
// maps them to the checkouts whose live paths show them and invalidates those
// paths. A tree change invalidates that subtree of every checkout on the folder
// (it may hold live paths below captured ones); a reported folder root means
// the folder's events were lost, so those checkouts are invalidated whole.
func (r *Registry) folderChanged(changes []watch.Change) {
	changed := make(map[string][]string) // checkout -> changed paths
	trees := make(map[string][]string)   // checkout -> changed subtrees
	whole := make(map[string]bool)
	paths := make([]string, len(changes))
	for i, c := range changes {
		paths[i] = c.Path
	}
	r.mu.RLock()
	r.foldersChangedLocked(paths)
	users := make(map[*baseRecord][]string)
	for name, h := range r.checkouts {
		if !h.retired.Load() {
			users[h.base] = append(users[h.base], name)
		}
	}
	for b, names := range users {
		m := b.view.Meta()
		if m.Remote != nil || m.Dir == "" {
			continue
		}
		for _, c := range changes {
			rel, ok := below(m.Dir, c.Path)
			if !ok {
				continue
			}
			for _, name := range names {
				switch {
				case rel == "":
					whole[name] = true
				case c.Tree:
					trees[name] = append(trees[name], rel)
				case b.view.Live(rel):
					changed[name] = append(changed[name], rel)
				}
			}
		}
	}
	r.mu.RUnlock()
	for name := range whole {
		delete(changed, name)
		delete(trees, name)
		r.invalidate(name, nil, []string{""})
	}
	for name, rels := range changed {
		r.invalidate(name, rels, trees[name])
		delete(trees, name)
	}
	for name, rels := range trees {
		r.invalidate(name, nil, rels)
	}
}

// below returns p relative to dir in slash form ("" for dir itself), or false
// when p lies outside dir.
func below(dir, p string) (string, bool) {
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}
