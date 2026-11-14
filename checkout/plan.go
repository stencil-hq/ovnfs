package checkout

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
)

// folder resolves a folder argument for explain and plan: its source root, its
// manifest with extra rule lines, and facts for its repositories.
func (r *Registry) folder(dir string, extra []string) (string, manifest.Manifest, *source.Domains, error) {
	if !filepath.IsAbs(dir) {
		return "", manifest.Manifest{}, nil, failure("invalid", "folder path must be absolute")
	}
	root, err := source.Resolve(dir)
	if err != nil {
		return "", manifest.Manifest{}, nil, coded(err)
	}
	m, err := manifest.Load(root, r.config.UserHome, extra)
	if err != nil {
		return "", manifest.Manifest{}, nil, failure("invalid", "%v", err)
	}
	var repos []source.Repo
	if repo, err := git.Open(root); err == nil && repo.Top == root {
		repos = append(repos, source.Repo{Path: ""})
	}
	domains, err := source.NewDomains(root, repos, nil)
	if err != nil {
		return "", manifest.Manifest{}, nil, coded(err)
	}
	return root, m, domains, nil
}

// Explain reports which class a path gets and which rule decided it, for a folder
// (with its manifest as it is now) or a checkout (with the rules it was created with).
func (r *Registry) Explain(ctx context.Context, args api.ExplainArgs) (api.ExplainReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.ExplainReply{}, err
	}
	defer resume()
	if (args.Dir == "") == (args.Checkout == "") {
		return api.ExplainReply{}, failure("invalid", "explain needs exactly one of a folder or a checkout")
	}
	var (
		dec    rules.Decision
		exists bool
		p      string
	)
	if args.Checkout != "" {
		p = cleanRel(args.Path)
		h, err := r.lookupCheckout(args.Checkout)
		if err != nil {
			return api.ExplainReply{}, err
		}
		set := h.base.rules
		if len(args.Rules) != 0 {
			extra, err := rules.Parse(strings.Join(args.Rules, "\n"), "flag", 1)
			if err != nil {
				return api.ExplainReply{}, failure("invalid", "%v", err)
			}
			set = rules.Set{Rules: append(extra, set.Rules...)}
		}
		var s Session
		if err := r.Enter(args.Checkout, &s); err != nil {
			return api.ExplainReply{}, err
		}
		dec, exists = source.Explain(overlayView{s.FS}, h.base.view.Domains(), set, p)
		s.Done()
	} else {
		root, m, domains, err := r.folder(args.Dir, args.Rules)
		if err != nil {
			return api.ExplainReply{}, err
		}
		// The folder may sit inside a repository whose top level becomes the root,
		// and root has its symlinks resolved, so the folder needs them resolved too.
		dir, err := filepath.EvalSymlinks(args.Dir)
		if err != nil {
			return api.ExplainReply{}, coded(err)
		}
		rel, err := filepath.Rel(root, filepath.Join(dir, filepath.FromSlash(args.Path)))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return api.ExplainReply{}, failure("invalid", "%s is outside %s", args.Path, root)
		}
		p = cleanRel(filepath.ToSlash(rel))
		view := source.Dir(root)
		domains.Discover(view, p)
		dec, exists = source.Explain(view, domains, m.Rules, p)
	}
	return api.ExplainReply{Path: p, Class: string(dec.Class), Rule: api.Rule{Class: string(dec.Rule.Class), Text: dec.Rule.Text, Origin: dec.Rule.Origin}, At: dec.At, Exists: exists}, nil
}

// cleanRel normalizes a root-relative slash path.
func cleanRel(p string) string { return strings.Trim(path.Clean("/"+p), "/") }

// Plan computes what launching a folder or a checkout elsewhere would transfer, rebuild and leave behind.
func (r *Registry) Plan(ctx context.Context, args api.PlanArgs) (api.PlanReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.PlanReply{}, err
	}
	defer resume()
	if (args.Dir == "") == (args.Checkout == "") {
		return api.PlanReply{}, failure("invalid", "plan needs exactly one of a folder or a checkout")
	}
	on := args.On
	if on == "" {
		on = "cloud"
	}
	if on != "cloud" && on != "peer" {
		return api.PlanReply{}, failure("invalid", "unknown target %q: use cloud or peer", on)
	}
	in := source.PlanInput{On: on}
	var warnings []string
	if args.Checkout != "" {
		if len(args.Rules) != 0 {
			return api.PlanReply{}, failure("invalid", "a checkout's rules are fixed when it is created; plan its folder to try other rules")
		}
		h, err := r.lookupCheckout(args.Checkout)
		if err != nil {
			return api.PlanReply{}, err
		}
		now, err := r.liveTree(h)
		if err != nil {
			return api.PlanReply{}, coded(err)
		}
		view := h.base.view
		info := func(p string) source.Info {
			if l, ok := now.layers[p]; ok {
				if e, ok := l.Get(p); ok && e.Info != nil {
					return source.Info{Mtime: e.Info.ModTime(), Size: e.Info.Size()}
				}
			}
			info, _ := view.Info(p)
			return info
		}
		in.Root, in.Files, in.Info, in.Roots, in.Domains, in.Lock = view.Meta().Dir, now.files, info, view.Roots(), view.Domains(), h.base.lock
	} else {
		root, m, _, err := r.folder(args.Dir, args.Rules)
		if err != nil {
			return api.PlanReply{}, err
		}
		res, err := source.Capture(ctx, source.Options{Root: root, Rules: m.Rules, Cache: r.statCachePath(root), CacheTrees: r.store})
		if err != nil {
			return api.PlanReply{}, coded(err)
		}
		in.Root, in.Files, in.Info, in.Roots, in.Domains, in.Lock = root, res.Files, func(p string) source.Info { return res.Info[p] }, res.Roots, res.Domains, m.Lock()
		warnings = res.Warnings
	}
	reply, err := source.Plan(ctx, in)
	if err != nil {
		return api.PlanReply{}, coded(err)
	}
	reply.Warnings = append(warnings, reply.Warnings...)
	return reply, nil
}
