package checkout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// liveTree returns a checkout's current source files without freezing it: its nearest
// hashed ancestor plus every newer layer and the live upper, hashed without storing.
func (r *Registry) liveTree(h *checkoutRecord) (*tree, error) {
	r.mu.RLock()
	parent, layers, ready, err := r.ancestorTreeLocked(h.base, h.stack, h.meta.Head)
	if err == nil && !ready {
		// An ancestor is still hashing; start from the base and replay every layer.
		parent, layers, _, err = r.ancestorTreeLocked(h.base, h.stack, nil)
	}
	r.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	h.freeze.RLock()
	defer h.freeze.RUnlock()
	if err := h.fs.Flush(); err != nil {
		return nil, err
	}
	upper, err := overlay.IndexUpper(h.upper)
	if err != nil {
		return nil, err
	}
	t, err := newTree(h.base, nil, parent, overlayView{h.fs})
	if err != nil {
		return nil, err
	}
	for _, l := range append(layers, upper) {
		if err := t.apply(l); err != nil {
			return nil, err
		}
	}
	t.finish()
	return t, nil
}

// lookupCheckout returns a live checkout.
func (r *Registry) lookupCheckout(name string) (*checkoutRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h := r.checkouts[name]
	if h == nil || h.retired.Load() {
		return nil, failure("not_found", "no checkout %s", name)
	}
	return h, nil
}

// fromTree resolves the tree a diff compares against: the checkout's base capture, or a hashed snapshot on the same base.
func (r *Registry) fromTree(ctx context.Context, h *checkoutRecord, id string) (store.OID, error) {
	if id == "" {
		return h.base.view.Meta().Tree, nil
	}
	r.mu.RLock()
	s := r.snapshots[id]
	r.mu.RUnlock()
	if s == nil {
		return "", failure("not_found", "no snapshot %s", id)
	}
	if s.base != h.base {
		return "", failure("invalid", "snapshot %s is not on the same capture as checkout %s", id, h.meta.Name)
	}
	reply, err := r.waitHashed(ctx, s)
	if err != nil {
		return "", err
	}
	return store.OID(reply.Snapshot.Tree), nil
}

// Diff lists source paths that differ between a snapshot (default: the base) and the checkout now.
func (r *Registry) Diff(ctx context.Context, args api.DiffArgs) (api.DiffReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.DiffReply{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.DiffReply{}, err
	}
	fromID, err := r.fromTree(ctx, h, args.From)
	if err != nil {
		return api.DiffReply{}, err
	}
	from, err := h.base.view.Store().Flatten(fromID)
	if err != nil {
		return api.DiffReply{}, coded(err)
	}
	now, err := r.liveTree(h)
	if err != nil {
		return api.DiffReply{}, coded(err)
	}
	reply := api.DiffReply{From: args.From, Changes: compare(from, now.files)}
	if reply.From == "" {
		reply.From = "base"
	}
	if args.Patch || args.Binary {
		var eol *eolConverter
		if args.Binary {
			paths := make([]string, len(reply.Changes))
			for i, c := range reply.Changes {
				paths[i] = c.Path
			}
			if eol, err = newEOLConverter(ctx, h.base.view, paths); err != nil {
				return api.DiffReply{}, coded(err)
			}
			defer eol.Close()
		}
		var patch strings.Builder
		kept := reply.Changes[:0:0]
		for _, c := range reply.Changes {
			old, err := r.content(h, c.Path, from[c.Path], nil)
			if err != nil {
				return api.DiffReply{}, coded(err)
			}
			cur, err := r.content(h, c.Path, now.files[c.Path], now.layers[c.Path])
			if err != nil {
				return api.DiffReply{}, coded(err)
			}
			if !args.Binary {
				unified(&patch, c.Path, old, cur)
				kept = append(kept, c)
				continue
			}
			// A git patch shows what git stores: a change of line endings that git
			// normalizes away is no change.
			a, old, err := eol.blob(c.Path, from[c.Path], old)
			if err != nil {
				return api.DiffReply{}, coded(err)
			}
			b, cur, err := eol.blob(c.Path, now.files[c.Path], cur)
			if err != nil {
				return api.DiffReply{}, coded(err)
			}
			if a == b {
				continue
			}
			gitPatch(&patch, c.Path, a, b, old, cur)
			kept = append(kept, c)
		}
		reply.Changes, reply.Patch, reply.Binary = kept, patch.String(), args.Binary
	}
	return reply, nil
}

// compare returns changed blob paths in order; empty-directory leaves are not changes.
func compare(from, to map[string]store.File) []api.Change {
	changes := []api.Change{}
	for _, p := range source.SortedPaths(union(from, to)) {
		a, inFrom := from[p]
		b, inTo := to[p]
		if inFrom && a.Mode == store.ModeDir {
			inFrom = false
		}
		if inTo && b.Mode == store.ModeDir {
			inTo = false
		}
		switch {
		case !inFrom && !inTo:
		case !inFrom:
			changes = append(changes, api.Change{Path: p, Status: "A"})
		case !inTo:
			changes = append(changes, api.Change{Path: p, Status: "D"})
		case (a.Mode == store.ModeSymlink) != (b.Mode == store.ModeSymlink):
			changes = append(changes, api.Change{Path: p, Status: "T"})
		case a != b:
			changes = append(changes, api.Change{Path: p, Status: "M"})
		}
	}
	return changes
}

// union merges two leaf maps' keys.
func union(a, b map[string]store.File) map[string]store.File {
	out := make(map[string]store.File, len(a)+len(b))
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

// content reads a leaf's bytes: from the layer that supplied it, else the store
// or the source's git objects.
func (r *Registry) content(h *checkoutRecord, p string, f store.File, layer *overlay.Layer) ([]byte, error) {
	if f.ID == "" || f.Mode == store.ModeDir {
		return nil, nil
	}
	if layer != nil {
		if f.Mode == store.ModeSymlink {
			target, err := layer.Readlink(p)
			return []byte(target), err
		}
		return layer.ReadFile(p)
	}
	return h.base.view.Blob(p, f.ID)
}

// unified appends one path's `diff -u` output, nothing when the contents are
// equal, or a one-line note for binary content. A nil side is absent (/dev/null).
func unified(w *strings.Builder, p string, old, cur []byte) {
	if bytes.IndexByte(head(old), 0) >= 0 || bytes.IndexByte(head(cur), 0) >= 0 {
		fmt.Fprintf(w, "Binary files a/%s and b/%s differ\n", p, p)
		return
	}
	labelA, labelB := "a/"+p, "b/"+p
	if old == nil {
		labelA = "/dev/null"
	}
	if cur == nil {
		labelB = "/dev/null"
	}
	unifiedHunks(w, labelA, labelB, old, cur)
}

// diffContext is the number of unchanged lines around each change, as diff -u.
const diffContext = 3

// Edit operations of a line diff script.
const (
	opEqual  byte = ' '
	opDelete byte = '-'
	opInsert byte = '+'
)

// writeHunk prints script[start:end] as one hunk: line positions are recovered
// by counting the deletes/equals and inserts/equals that precede start.
func writeHunk(w *strings.Builder, script []byte, start, end int, a, b []string) {
	ai, bi := 0, 0
	for _, op := range script[:start] {
		if op != opInsert {
			ai++
		}
		if op != opDelete {
			bi++
		}
	}
	an, bn := 0, 0
	for _, op := range script[start:end] {
		if op != opInsert {
			an++
		}
		if op != opDelete {
			bn++
		}
	}
	fmt.Fprintf(w, "@@ -%s +%s @@\n", hunkRange(ai, an), hunkRange(bi, bn))
	for _, op := range script[start:end] {
		var line string
		switch op {
		case opInsert:
			line = b[bi]
			bi++
		case opDelete:
			line = a[ai]
			ai++
		default:
			line = a[ai]
			ai++
			bi++
		}
		w.WriteByte(op)
		w.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			w.WriteString("\n\\ No newline at end of file\n")
		}
	}
}

// hunkRange formats a hunk side as diff -u does: an empty side names the line
// before it, and a count of one is left out.
func hunkRange(before, n int) string {
	switch n {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return fmt.Sprint(before + 1)
	}
	return fmt.Sprintf("%d,%d", before+1, n)
}

// splitLines cuts b after every newline; a final line without one keeps no
// terminator, so it differs from the same text with one, as in diff.
func splitLines(b []byte) []string {
	var lines []string
	for len(b) > 0 {
		n := bytes.IndexByte(b, '\n') + 1
		if n == 0 {
			n = len(b)
		}
		lines = append(lines, string(b[:n]))
		b = b[n:]
	}
	return lines
}

// maxEdits and maxSteps bound the Myers search: past this many inserted plus
// deleted lines, or this many diagonal and match steps, the middle left after
// trimming the common prefix and suffix is reported as wholly replaced, still
// a correct patch, instead of spending quadratic time and memory on a minimal one.
const (
	maxEdits = 2000
	maxSteps = 1 << 26
)

// lineDiff returns an edit script turning a into b, deletions before insertions
// within each change.
func lineDiff(a, b []string) []byte {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ids := map[string]int32{}
	intern := func(lines []string) []int32 {
		out := make([]int32, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = int32(len(ids))
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	x, y := intern(a[pre:len(a)-suf]), intern(b[pre:len(b)-suf])
	script := make([]byte, 0, len(a)+len(b))
	for range pre {
		script = append(script, opEqual)
	}
	middle, ok := myers(x, y)
	if !ok {
		middle = middle[:0]
		for range x {
			middle = append(middle, opDelete)
		}
		for range y {
			middle = append(middle, opInsert)
		}
	}
	script = append(script, middle...)
	for range suf {
		script = append(script, opEqual)
	}
	return script
}

// myers finds a shortest edit script from x to y (Myers 1986), keeping each
// round's furthest-reaching diagonals to backtrack. It gives up (false) past
// maxEdits or maxSteps.
func myers(x, y []int32) ([]byte, bool) {
	n, m := len(x), len(y)
	off := maxEdits + 1
	v := make([]int32, 2*maxEdits+3)
	var trace [][]int32
	steps := 0
	for d := 0; d <= maxEdits; d++ {
		for k := -d; k <= d; k += 2 {
			var i int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				i = int(v[off+k+1])
			} else {
				i = int(v[off+k-1]) + 1
			}
			j := i - k
			steps++
			for i < n && j < m && x[i] == y[j] {
				i++
				j++
				steps++
			}
			if steps > maxSteps {
				return nil, false
			}
			v[off+k] = int32(i)
			if i >= n && j >= m {
				trace = append(trace, slices.Clone(v[off-d:off+d+1]))
				return backtrack(trace, n, m), true
			}
		}
		trace = append(trace, slices.Clone(v[off-d:off+d+1]))
	}
	return nil, false
}

// backtrack walks myers' rounds from (n, m) back to the origin, then orders
// each change's deletions before its insertions as diff -u prints them.
func backtrack(trace [][]int32, n, m int) []byte {
	var rev []byte
	i, j := n, m
	for d := len(trace) - 1; d > 0; d-- {
		prev := trace[d-1] // diagonal k is at prev[k+d-1]
		k := i - j
		pk := k - 1
		if k == -d || (k != d && prev[k-1+d-1] < prev[k+1+d-1]) {
			pk = k + 1
		}
		pi := int(prev[pk+d-1])
		pj := pi - pk
		mi := pi + 1 // the snake starts after this round's edit
		if pk == k+1 {
			mi = pi
		}
		for i > mi {
			rev = append(rev, opEqual)
			i--
			j--
		}
		if pk == k+1 {
			rev = append(rev, opInsert)
		} else {
			rev = append(rev, opDelete)
		}
		i, j = pi, pj
	}
	for range i {
		rev = append(rev, opEqual)
	}
	slices.Reverse(rev)
	for s := 0; s < len(rev); {
		if rev[s] == opEqual {
			s++
			continue
		}
		e := s
		dels := 0
		for e < len(rev) && rev[e] != opEqual {
			if rev[e] == opDelete {
				dels++
			}
			e++
		}
		for t := s; t < e; t++ {
			rev[t] = opInsert
			if t-s < dels {
				rev[t] = opDelete
			}
		}
		s = e
	}
	return rev
}

// head is the prefix binary detection inspects, as git does.
func head(b []byte) []byte { return b[:min(len(b), 8000)] }

// Apply merges a checkout's source changes back into the folder it came from, three-way
// against the base capture: paths unchanged in the folder take the checkout's version,
// text files changed on both sides are merged, with conflict markers where both changed
// the same lines, and everything else is a conflict left untouched. A path whose markers
// the user resolved merges later changes against the version it was marked with
// (see checkoutMeta.MergeBases). Applies into one folder take turns, each merging
// against what the last one wrote. With Branch it instead copies the checkout's commits.
func (r *Registry) Apply(ctx context.Context, args api.ApplyArgs) (reply api.ApplyReply, err error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.ApplyReply{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.ApplyReply{}, err
	}
	if remote := h.base.view.Meta().Remote; remote != nil {
		return api.ApplyReply{}, failure("invalid", "checkout %s is attached from %s; its folder is not on this machine", h.meta.Name, remote.Source)
	}
	root := h.base.view.Meta().Dir
	reply = api.ApplyReply{Source: root, Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}}
	if args.Branch != "" {
		repo := h.base.view.Domains().Repo("")
		if !h.meta.Git || repo == nil {
			return api.ApplyReply{}, failure("invalid", "checkout %s has no git directory; --branch needs a repository source", h.meta.Name)
		}
		commit, updated, err := repo.FetchBranch(ctx, r.gitdirPath(h.meta.dir()), args.Branch, args.DryRun)
		if errors.Is(err, git.ErrBranchConflict) {
			return api.ApplyReply{}, failure("conflict", "%s", err)
		}
		if errors.Is(err, git.ErrBranchDryRun) {
			return api.ApplyReply{}, failure("invalid", "%s", err)
		}
		if err != nil {
			return api.ApplyReply{}, coded(err)
		}
		for _, change := range updated {
			reply.Applied = append(reply.Applied, api.Change{Path: change.Path, Status: change.Status})
		}
		reply.Branch, reply.Commit = args.Branch, commit
		return reply, nil
	}
	h.merge.Lock()
	defer h.merge.Unlock()
	snap, err := r.Snap(ctx, api.SnapArgs{Checkout: args.Checkout, Label: "apply", Wait: !args.DryRun})
	if err != nil {
		return api.ApplyReply{}, err
	}
	// Dry runs release even on failure; normal applies retain the snapshot on
	// failure or conflict, but release it after a successful merge.
	id := snap.Snapshot.ID
	defer func() {
		if args.DryRun || err == nil && len(reply.Conflicts) == 0 {
			if dropErr := r.Drop(context.WithoutCancel(ctx), api.DropArgs{Snapshot: id}); dropErr != nil && err == nil {
				reply, err = api.ApplyReply{}, dropErr
			}
		}
	}()
	if args.DryRun {
		r.mu.RLock()
		s := r.snapshots[id]
		r.mu.RUnlock()
		if s == nil {
			return api.ApplyReply{}, failure("not_found", "no snapshot %s", id)
		}
		if snap, err = r.waitHashed(ctx, s); err != nil {
			return api.ApplyReply{}, err
		}
	}
	// Unix would fail each path with ENOTDIR, but Windows reports them absent,
	// which would turn every path into a deletion conflict.
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		return api.ApplyReply{}, failure("invalid", "source %s is not a directory", root)
	}
	st := h.base.view.Store()
	from, err := st.Flatten(h.base.view.Meta().Tree)
	if err != nil {
		return api.ApplyReply{}, coded(err)
	}
	to, err := st.Flatten(store.OID(snap.Snapshot.Tree))
	if err != nil {
		return api.ApplyReply{}, coded(err)
	}
	bases, err := r.mergeBases(h, applySource)
	if err != nil {
		return api.ApplyReply{}, coded(err)
	}
	maps.Copy(from, bases)
	release, err := r.applying.lock(ctx, root)
	if err != nil {
		return api.ApplyReply{}, err
	}
	defer release()
	s := sides{ours: "in the source folder", theirs: "in the checkout",
		labels: git.Labels{Ours: "folder", Base: "base", Theirs: "checkout " + h.meta.Name}}
	seen := map[string]bool{}
	for _, c := range compare(from, to) {
		seen[c.Path] = true
		o, reason, err := r.applyOne(ctx, h, root, c.Path, from[c.Path], to[c.Path], s, args.DryRun)
		if err != nil {
			return api.ApplyReply{}, coded(fmt.Errorf("%s: %w", c.Path, err))
		}
		report(&reply, c, o, reason)
		advance(bases, c.Path, to[c.Path], o)
	}
	err = reportPending(&reply, bases, seen, func(p string) (bool, error) {
		return hasMarkersAt(filepath.Join(root, filepath.FromSlash(p)))
	})
	if err != nil {
		return api.ApplyReply{}, coded(err)
	}
	if args.DryRun {
		return reply, nil
	}
	read := func(p string, f store.File) ([]byte, error) { return h.base.view.Blob(p, f.ID) }
	if err := r.setMergeBases(h, applySource, bases, read); err != nil {
		return api.ApplyReply{}, coded(err)
	}
	return reply, nil
}

// applyOne brings one path of the folder to the checkout's version when that is safe,
// writing conflict markers where both sides changed the same lines of a text file.
func (r *Registry) applyOne(ctx context.Context, h *checkoutRecord, root, p string, base, theirs store.File, s sides, dry bool) (outcome, string, error) {
	dst := filepath.Join(root, filepath.FromSlash(p))
	ours, isDir, err := leafAt(dst)
	if err != nil {
		return 0, "", err
	}
	if isDir {
		return conflict, "is a directory in the source folder", nil
	}
	if ours == theirs {
		return skipped, "", nil
	}
	if ours == base {
		if dry {
			return applied, "", nil
		}
		return applied, "", r.place(h, dst, p, theirs)
	}
	why, text := s.conflict(base, ours, theirs)
	if !text {
		return conflict, why, nil
	}
	current, err := os.ReadFile(dst)
	if err != nil {
		return 0, "", err
	}
	var baseData []byte
	if base.ID != "" {
		if baseData, err = h.base.view.Blob(p, base.ID); err != nil {
			return 0, "", err
		}
	}
	theirData, err := h.base.view.Blob(p, theirs.ID)
	if err != nil {
		return 0, "", err
	}
	result, o, reason := mergeText(s.labels, current, baseData, theirData, why)
	if dry || (o != merged && o != marked) {
		return o, reason, nil
	}
	return o, reason, writeAtomic(dst, func(tmp string) error { return os.WriteFile(tmp, result, modeOf(theirs)) })
}

// hasMarkersAt reports whether the folder file name holds conflict markers; anything but a regular file holds none.
func hasMarkersAt(name string) (bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(name)
	return hasMarkers(data), err
}

// leafAt reads the folder's current state of a path as a tree leaf (zero when absent).
func leafAt(name string) (store.File, bool, error) {
	info, err := hostfile.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return store.File{}, false, nil
	}
	if err != nil {
		return store.File{}, false, err
	}
	if info.IsDir() {
		return store.File{}, true, nil
	}
	mode, ok := store.ModeOf(info.Mode())
	if !ok {
		return store.File{}, false, fmt.Errorf("unsupported file type %s", info.Mode().Type())
	}
	if mode == store.ModeSymlink {
		target, err := os.Readlink(name)
		return store.File{Mode: mode, ID: store.HashBytes("blob", []byte(filepath.ToSlash(target)))}, false, err
	}
	id, _, err := store.HashFile(name)
	return store.File{Mode: mode, ID: id}, false, err
}

// place writes the checkout's version of a path into the folder, or deletes it.
func (r *Registry) place(h *checkoutRecord, dst, p string, f store.File) error {
	if f.ID == "" {
		err := os.Remove(dst)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	st := h.base.view.Store()
	return writeAtomic(dst, func(tmp string) error {
		switch {
		case f.Mode == store.ModeSymlink:
			target, err := h.base.view.Blob(p, f.ID)
			if err != nil {
				return err
			}
			return os.Symlink(string(target), tmp)
		case st.Has(f.ID):
			return st.CloneBlob(f.ID, tmp, modeOf(f))
		default:
			data, err := h.base.view.Blob(p, f.ID)
			if err != nil {
				return err
			}
			return os.WriteFile(tmp, data, modeOf(f))
		}
	})
}

// modeOf maps a blob leaf to its permission bits.
func modeOf(f store.File) os.FileMode {
	if f.Mode == store.ModeExec {
		return 0o755
	}
	return 0o644
}

// writeAtomic creates content at a temporary name beside dst and renames it over dst
// with [hostfile.Rename], which replaces dst even when it is read-only or, on
// Windows, held open by a process that shares delete access.
func writeAtomic(dst string, write func(tmp string) error) error {
	tmp := filepath.Join(filepath.Dir(dst), ".ovnfs-apply-"+filepath.Base(dst))
	_ = os.Remove(tmp)
	if err := write(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := hostfile.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
