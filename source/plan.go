package source

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/store"
)

// maxPaths bounds the example paths a plan lists per set.
const maxPaths = 20

// Explain classifies p the way a capture would and reports whether it exists in view.
// Paths at or below a .git entry belong to the git adapter, and the root's .jj
// to the jj adapter, not to the rules.
func Explain(view View, d *Domains, set rules.Set, p string) (rules.Decision, bool) {
	p = strings.Trim(path.Clean("/"+p), "/")
	_, err := view.Lstat(p)
	exists := err == nil
	if IsJJPath(p) {
		return rules.Decision{Class: rules.Hidden, At: ".jj", Rule: rules.Rule{Class: rules.Hidden, Text: "jj workspace state: each checkout gets a workspace of its own", Origin: "jj adapter"}}, exists
	}
	if IsGitPath(p) {
		at := p
		for !strings.HasSuffix(at, ".git") || (at != ".git" && !strings.HasSuffix(at, "/.git")) {
			at = parent(at)
		}
		class, text := rules.Local, "a nested repository's .git stays a local copy"
		if (at == ".git" && d.RootIsRepo()) || d.Module(parent(at)) != "" {
			class, text = rules.Hidden, "replaced by the checkout's private git directory"
		}
		return rules.Decision{Class: class, At: at, Rule: rules.Rule{Class: class, Text: text, Origin: "git adapter"}}, exists
	}
	return set.Classify(p, func(prefix string) rules.Facts { return d.Facts(view, prefix, nil) }), exists
}

// PlanInput is a classified source: a capture of a folder or a checkout's current state.
type PlanInput struct {
	Root    string // folder that cache key files are read from
	On      string
	Files   map[string]store.File
	Info    func(p string) Info // captured or current stat data; zero when unknown
	Roots   map[string]rules.Class
	Domains *Domains
	Lock    manifest.Lock
}

// Plan splits a source by how a launch elsewhere would get each part: blobs a git
// origin already has, bytes sent from this machine, caches rebuilt there, and paths that stay.
func Plan(ctx context.Context, in PlanInput) (api.PlanReply, error) {
	reply := api.PlanReply{Root: in.Root, On: in.On, Lock: in.Lock.Hash(), Repos: []api.PlanRepo{}, Cache: []api.PlanCache{}, Warnings: []string{}}
	upstream := map[string]map[string]string{} // repo path → repo-relative path → blob id at the upstream commit
	for _, r := range in.Domains.Repos() {
		repo := in.Domains.Repo(r.Path)
		if repo == nil {
			continue
		}
		pr := api.PlanRepo{Path: r.Path, Head: r.Head}
		ref, commit, err := repo.Upstream(ctx)
		if err != nil {
			return api.PlanReply{}, err
		}
		if commit == "" {
			reply.Warnings = append(reply.Warnings, fmt.Sprintf("%s has no upstream; all of its files are sent from this machine", display(r.Path)))
		} else {
			pr.Upstream = ref
			if pr.Origin, err = repo.Remote(ctx, ref); err != nil {
				return api.PlanReply{}, err
			}
			if r.Head != "" {
				if pr.Pushed, err = repo.Contains(ctx, commit, r.Head); err != nil {
					return api.PlanReply{}, err
				}
			}
			// The commit attach names for the remote to fetch: HEAD when pushed,
			// else its merge base with the upstream.
			fetched := commit
			if o, ok, err := repo.Origin(ctx, r.Head); err != nil {
				return api.PlanReply{}, err
			} else if ok {
				fetched = o.Commit
			}
			entries, err := repo.LsTree(ctx, fetched)
			if err != nil {
				return api.PlanReply{}, err
			}
			ids := make(map[string]string, len(entries))
			for _, e := range entries {
				if e.Type == "blob" {
					ids[e.Path] = e.OID
				}
			}
			upstream[r.Path] = ids
		}
		reply.Repos = append(reply.Repos, pr)
	}
	for _, p := range SortedPaths(in.Files) {
		f := in.Files[p]
		if f.Mode == store.ModeDir {
			continue
		}
		size := in.Info(p).Size
		add(&reply.Source, p, size)
		dom := in.Domains.For(p)
		if ids, ok := upstream[dom.path]; ok && dom.repo != nil && ids[dom.rel(p)] == string(f.ID) {
			add(&reply.Origin, p, size)
		} else {
			add(&reply.Here, p, size)
		}
		if rules.IsSecret(p, false) {
			reply.Warnings = append(reply.Warnings, fmt.Sprintf("%s looks like a secret but is source and would be sent; add `local %s` to keep it here", p, p))
		}
	}
	for _, p := range SortedPaths(in.Roots) {
		switch in.Roots[p] {
		case rules.Cache:
			c := api.PlanCache{Root: p}
			if entry, ok := in.Lock.CacheFor(p); ok {
				c.Rebuild, c.Key = entry.Rebuild, entry.Key
				// A remote checkout has no folder here to read key files from.
				if len(entry.Key) > 0 && in.Root != "" {
					hash, err := manifest.KeyHash(in.Root, p, entry.Key)
					if err != nil {
						return api.PlanReply{}, err
					}
					c.KeyHash = hash
				}
			}
			reply.Cache = append(reply.Cache, c)
		case rules.Local:
			add(&reply.Local, p, in.Info(p).Size)
		case rules.Hidden:
			add(&reply.Hidden, p, in.Info(p).Size)
		}
	}
	return reply, nil
}

// add counts one path into a plan set, listing the first few.
func add(s *api.PlanSet, p string, size int64) {
	s.Files++
	s.Bytes += size
	if len(s.Paths) < maxPaths {
		s.Paths = append(s.Paths, p)
	}
}

// display names a repository path for people: "." for the root.
func display(p string) string {
	if p == "" {
		return "."
	}
	return p
}
