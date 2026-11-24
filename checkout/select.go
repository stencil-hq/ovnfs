package checkout

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

// Prune removes the checkouts args picks, each as Rm does, except one touched
// since it was picked.
func (r *Registry) Prune(ctx context.Context, args api.PruneArgs) (api.PruneReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.PruneReply{}, err
	}
	defer resume()
	if args.Select == (api.Select{}) {
		return api.PruneReply{}, failure("invalid", "prune needs a criterion (dir, idle_s or no_diff); it never removes every checkout")
	}
	picked, err := r.pick(ctx, args.Select)
	if err != nil {
		return api.PruneReply{}, err
	}
	reply := api.PruneReply{Removed: []api.Checkout{}}
	for _, c := range picked {
		if !args.DryRun {
			removed, err := r.remove(ctx, api.RmArgs{Checkout: c.Name, StickySnapshots: args.StickySnapshots}, c.Touched)
			if e, ok := errors.AsType[*Error](err); ok && e.Code == "not_found" {
				continue // removed meanwhile
			}
			if err != nil {
				e, _ := errors.AsType[*Error](coded(err))
				return api.PruneReply{}, failure(e.Code, "removing %s after %d others: %s", c.Name, len(reply.Removed), e.Message)
			}
			if !removed {
				continue
			}
		}
		reply.Removed = append(reply.Removed, c.Checkout)
	}
	return reply, nil
}

type pickedCheckout struct {
	api.Checkout
	h    *checkoutRecord
	life uint32
}

// pick returns the live checkouts sel picks (see api.Select) in name order.
func (r *Registry) pick(ctx context.Context, sel api.Select) ([]pickedCheckout, error) {
	// dir is a folder outside the mount; family the checkout a folder inside it
	// (spelled so, or through symlinks) is in, never resolved through the daemon's
	// own mount. The mount itself picks all.
	var dir, family string
	if sel.Dir != "" {
		if !filepath.IsAbs(sel.Dir) {
			return nil, failure("invalid", "dir %s is not absolute", sel.Dir)
		}
		rel, inMount := r.underMount(sel.Dir)
		if !inMount {
			real, err := filepath.EvalSymlinks(sel.Dir)
			if err != nil {
				real = filepath.Clean(sel.Dir) // a folder since deleted still names its checkouts
			}
			if rel, inMount = r.underMount(real); !inMount {
				dir = real
			}
		}
		if inMount && rel != "." {
			family, _, _ = strings.Cut(filepath.ToSlash(rel), "/")
		}
	}
	var found []pickedCheckout
	now := time.Now()
	idle := time.Duration(sel.Idle * float64(time.Second))
	r.mu.RLock()
	kin := "" // family's source
	if family != "" {
		h := r.checkouts[family]
		if h == nil || h.retired.Load() {
			r.mu.RUnlock()
			return nil, failure("not_found", "%s is in the checkout mount, but no checkout %s exists", sel.Dir, family)
		}
		kin = h.source()
	}
	for _, h := range r.checkouts {
		if h.retired.Load() {
			continue
		}
		switch src := h.source(); {
		case kin != "" && src != kin:
			continue
		case dir != "" && !inside(dir, src) && !inside(src, dir):
			continue
		}
		c := r.describeLocked(h)
		if sel.Idle == 0 || now.Sub(c.Touched) >= idle {
			found = append(found, pickedCheckout{c, h, h.meta.Life})
		}
	}
	r.mu.RUnlock()
	for i := range found {
		found[i].Dirty = r.dirty(found[i].h)
	}
	picked := found
	if sel.NoDiff {
		picked = found[:0]
		bases := map[*baseRecord]map[string]store.File{}
		for _, f := range found {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			same, err := r.unchanged(f.h, bases)
			if err != nil {
				return nil, coded(fmt.Errorf("checkout %s: %w", f.Name, err))
			}
			if same {
				picked = append(picked, f)
			}
		}
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].Name < picked[j].Name })
	return picked, nil
}

// unchanged reports whether a checkout has no source changes since its base, as
// an empty diff shows. bases caches flattened capture trees across checkouts
// that pulled nothing from their folder since.
func (r *Registry) unchanged(h *checkoutRecord, bases map[*baseRecord]map[string]store.File) (bool, error) {
	r.mu.RLock()
	pulled := h.meta.MergeBases[pulledSource] != "" || h.meta.MergeBases[folderSource] != ""
	r.mu.RUnlock()
	from, ok := bases[h.base]
	switch {
	case pulled:
		var err error
		if from, err = r.checkoutBase(h); err != nil {
			return false, err
		}
	case !ok:
		var err error
		if from, err = h.base.view.Store().Flatten(h.base.view.Meta().Tree); err != nil {
			return false, err
		}
		bases[h.base] = from
	}
	now, err := r.liveTree(h)
	if err != nil {
		return false, err
	}
	return len(compare(from, now.files)) == 0, nil
}
