package checkout

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/internal/search/glob"
	"github.com/stencil-hq/ovnfs/internal/search/grep"
	"github.com/stencil-hq/ovnfs/internal/search/walk"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/store"
)

// searchTarget is a resolved search root.
type searchTarget struct {
	root   string
	walk   walk.Options
	remote grep.Remote // set for a checkout attached from another machine
}

// searchRoot resolves mounted checkout paths without accessing the network mount.
// The caller owns s and releases its shared freeze lock after all search workers
// have stopped. A checkout attached from another machine first fetches the
// ignore files the walk reads, in one batch, and grep scans the files it has
// not fetched on that machine.
func (r *Registry) searchRoot(ctx context.Context, args api.SearchArgs, s *Session) (searchTarget, error) {
	wo := walk.Options{
		Hidden: args.Hidden, Gitignore: !args.NoIgnore, SkipGit: true,
		SkipNodeModules: !args.NodeModules, Workers: args.Workers, MaxDepth: args.MaxDepth,
	}
	if (args.Dir == "") == (args.Checkout == "") {
		return searchTarget{}, failure("invalid", "search needs exactly one of a folder or a checkout")
	}
	if args.Workers < 0 || args.MaxDepth < 0 {
		return searchTarget{}, failure("invalid", "workers and max_depth must not be negative")
	}
	if args.Path != "" && (strings.ContainsRune(args.Path, '\\') || !filepath.IsLocal(args.Path)) {
		return searchTarget{}, failure("invalid", "search path must stay relative to its root")
	}
	name, sub := args.Checkout, args.Path
	if args.Dir != "" {
		if !filepath.IsAbs(args.Dir) {
			return searchTarget{}, failure("invalid", "folder path must be absolute")
		}
		dir := filepath.Clean(args.Dir)
		if rel, mounted := r.underMount(dir); mounted {
			if rel == "." {
				return searchTarget{}, failure("invalid", "%s is the checkout mount; search a checkout", dir)
			}
			var rest string
			name, rest, _ = strings.Cut(filepath.ToSlash(rel), "/")
			sub = path.Join(rest, sub)
		} else {
			return searchTarget{root: filepath.Join(dir, filepath.FromSlash(sub)), walk: wo}, nil
		}
	}
	if err := r.Enter(name, s); err != nil {
		return searchTarget{}, err
	}
	wo.FS, wo.Config = fsys.FromOverlay(s.FS), fsys.Native
	t := searchTarget{root: path.Join("/", sub), walk: wo}
	view := s.record.base.view
	if remote := view.Meta().Remote; remote != nil {
		if wo.Gitignore {
			// A failed fetch leaves the walk to read them one by one, as it would anyway.
			_ = view.FetchMatching(ctx, ignoreFiles(t.root[1:]))
		}
		if r.config.Search != nil {
			t.remote = remoteSearch{fs: s.FS, base: view, key: LinkKey(remote.Node, remote.Source), search: r.config.Search}
		}
	}
	return t, nil
}

// ignoreFiles selects the ignore files a walk of the slash-relative dir reads:
// those below it and in its ancestors.
func ignoreFiles(dir string) func(p string) bool {
	within := func(p, dir string) bool { return dir == "" || p == dir || strings.HasPrefix(p, dir+"/") }
	names := walk.IgnoreFiles()
	return func(p string) bool {
		for _, name := range names {
			if at, ok := strings.CutSuffix(p, name); ok && (at == "" || strings.HasSuffix(at, "/")) {
				at = strings.TrimSuffix(at, "/")
				return within(at, dir) || within(dir, at)
			}
		}
		return false
	}
}

// remoteSearch scans a remote checkout's unfetched files on the machine it is
// attached from: files no upper or layer of the checkout shadows, whose blobs
// are not in the store.
type remoteSearch struct {
	fs     *overlay.FS
	base   *base.Base
	key    string // the link to that machine (see LinkKey)
	search func(ctx context.Context, key string, q grep.Query, wants []base.Want) ([]grep.Outcome, error)
}

// Locate reports p's blob when only the source machine holds it.
func (g remoteSearch) Locate(p string) (grep.Blob, bool) {
	if !g.fs.InBottom(p) {
		return grep.Blob{}, false
	}
	w, ok := g.base.Uncached(p)
	return grep.Blob{ID: string(w.ID), Path: w.Path, Size: w.Size}, ok
}

// Scan asks the link to the source machine to scan blobs with q.
func (g remoteSearch) Scan(ctx context.Context, q grep.Query, blobs []grep.Blob) ([]grep.Outcome, error) {
	wants := make([]base.Want, len(blobs))
	for i, b := range blobs {
		wants[i] = base.Want{ID: store.OID(b.ID), Path: b.Path, Size: b.Size}
	}
	return g.search(ctx, g.key, q, wants)
}

// Grep searches a folder or a checkout's merged contents without its file protocol.
func (r *Registry) Grep(ctx context.Context, args api.GrepArgs) (api.GrepReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.GrepReply{}, err
	}
	defer resume()
	if err := checkContext(ctx); err != nil {
		return api.GrepReply{}, err
	}
	mode := grep.Content
	switch args.Mode {
	case "", "content":
	case "filesWithMatches":
		mode = grep.FilesWithMatches
	case "count":
		mode = grep.Count
	default:
		return api.GrepReply{}, failure("invalid", "grep mode must be content, filesWithMatches, or count")
	}
	if args.Before < 0 || args.After < 0 || args.MaxColumns < 0 || args.Offset < 0 || args.MaxCount < 0 || args.MaxCountPerFile < 0 || args.MaxFileSize < 0 {
		return api.GrepReply{}, failure("invalid", "grep limits and context must not be negative")
	}
	var s Session
	t, err := r.searchRoot(ctx, args.SearchArgs, &s)
	if err != nil {
		return api.GrepReply{}, err
	}
	defer s.Done()
	if err := checkContext(ctx); err != nil {
		return api.GrepReply{}, err
	}
	res, err := grep.Grep(ctx, t.root, grep.Options{
		Pattern: args.Pattern, Literal: args.Literal, IgnoreCase: args.IgnoreCase,
		Multiline: args.Multiline, Glob: args.Glob, Anchored: args.Anchored, Exclude: args.Exclude, Type: args.Type,
		Mode: mode, Before: args.Before, After: args.After, MaxColumns: args.MaxColumns,
		Offset: args.Offset, MaxCount: args.MaxCount, MaxCountPerFile: args.MaxCountPerFile,
		MaxFileSize: args.MaxFileSize, Walk: t.walk, Remote: t.remote,
	})
	if err != nil {
		return api.GrepReply{}, coded(err)
	}
	if err := checkContext(ctx); err != nil {
		return api.GrepReply{}, err
	}
	reply := api.GrepReply{
		Matches: make([]api.GrepMatch, len(res.Matches)), TotalMatches: res.TotalMatches,
		FilesWithMatches: res.FilesWithMatches, FilesSearched: res.FilesSearched,
		LimitReached: res.LimitReached, SkippedOversized: res.SkippedOversized,
	}
	for i, m := range res.Matches {
		if i%256 == 0 {
			if err := checkContext(ctx); err != nil {
				return api.GrepReply{}, err
			}
		}
		reply.Matches[i] = api.GrepMatch{Path: m.Path, Line: m.Line, Text: m.Text,
			Before: searchContext(m.Before), After: searchContext(m.After), Truncated: m.Truncated, Column: m.Column, Count: m.Count}
	}
	if err := checkContext(ctx); err != nil {
		return api.GrepReply{}, err
	}
	return reply, nil
}

// Glob lists matching paths in a folder or directly in a checkout's merged tree.
func (r *Registry) Glob(ctx context.Context, args api.GlobArgs) (api.GlobReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.GlobReply{}, err
	}
	defer resume()
	if err := checkContext(ctx); err != nil {
		return api.GlobReply{}, err
	}
	var typ fsys.Type
	switch args.Type {
	case "":
	case "file":
		typ = fsys.File
	case "dir":
		typ = fsys.Dir
	case "symlink":
		typ = fsys.Symlink
	default:
		return api.GlobReply{}, failure("invalid", "glob type must be file, dir, or symlink")
	}
	if args.Limit < 0 {
		return api.GlobReply{}, failure("invalid", "glob limit must not be negative")
	}
	var s Session
	t, err := r.searchRoot(ctx, args.SearchArgs, &s)
	if err != nil {
		return api.GlobReply{}, err
	}
	defer s.Done()
	if err := checkContext(ctx); err != nil {
		return api.GlobReply{}, err
	}
	res, err := glob.Glob(ctx, t.root, glob.Options{
		Pattern: args.Pattern, Anchored: args.Anchored, Type: typ, Limit: args.Limit,
		SortByMtime: args.SortByMtime, Walk: t.walk,
	})
	if err != nil {
		return api.GlobReply{}, coded(err)
	}
	if err := checkContext(ctx); err != nil {
		return api.GlobReply{}, err
	}
	reply := api.GlobReply{Matches: make([]api.GlobMatch, len(res.Matches)), Total: res.Total, Truncated: res.Truncated}
	for i, m := range res.Matches {
		if i%256 == 0 {
			if err := checkContext(ctx); err != nil {
				return api.GlobReply{}, err
			}
		}
		reply.Matches[i] = api.GlobMatch{Path: m.Path, Type: m.Type.String(), Size: m.Size, Mtime: m.Mtime}
	}
	if err := checkContext(ctx); err != nil {
		return api.GlobReply{}, err
	}
	return reply, nil
}

func searchContext(lines []grep.ContextLine) []api.ContextLine {
	if len(lines) == 0 {
		return nil
	}
	out := make([]api.ContextLine, len(lines))
	for i, line := range lines {
		out[i] = api.ContextLine{Line: line.Line, Text: line.Text}
	}
	return out
}
