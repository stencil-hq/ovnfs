package base

import (
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// revisionView evaluates ignore files from the revision, while testing the type
// of existing ignored paths against the folder. Dirty ignore files must not
// decide what survives a revision checkout.
type revisionView struct {
	source.Dir
	files   map[string]store.File
	objects *git.Objects
}

func (v revisionView) ReadFile(p string) ([]byte, error) {
	f, ok := v.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return v.objects.Blob(string(f.ID))
}

// revisionIndex names the private index of a revision base in its state directory.
const revisionIndex = "revision-index"

// reviseCapture substitutes tree metadata, not a materialized checkout. The
// capture's stat identities remain usable only for content identical to REV.
// Its index has no stat data yet: seedRevisionIndex adds it once takes are done.
func reviseCapture(ctx context.Context, c Config, res *source.Result, revision *git.Revision) error {
	index := filepath.Join(c.StateDir, revisionIndex)
	if err := revision.WriteIndex(ctx, index, nil); err != nil {
		return err
	}
	repos := []source.Repo{{Path: "", Head: revision.Commit, Index: index}}
	domains, err := source.NewDomains(res.Root, repos)
	if err != nil {
		return err
	}
	repo := domains.Repo("")
	objects, err := repo.Objects()
	if err != nil {
		return err
	}
	defer objects.Close()
	files, info := treeFiles(revision.Entries, *res)
	// Gitlinks start empty in revision checkouts. Their contents belong to the
	// submodule and must not be judged by the superproject's ignore rules.
	gitlinks := map[string]bool{}
	for _, entry := range revision.Entries {
		if entry.Type == "commit" {
			gitlinks[entry.Path] = true
		}
	}
	inSubmodule := func(p string) bool {
		for ; p != "." && p != ""; p = path.Dir(p) {
			if gitlinks[p] {
				return true
			}
		}
		return false
	}
	tracked := map[string]bool{}
	for file := range files {
		for p := file; p != "."; p = path.Dir(p) {
			tracked[p] = true
		}
	}
	view := revisionView{Dir: source.Dir(res.Root), files: files, objects: objects}
	roots := map[string]rules.Class{}
	// Ignored roots remain live and copy-on-write, and hidden ones absent, just
	// as in an ordinary checkout. Nonignored roots can contain ignored
	// descendants; descend only those roots, never a retained ignored build
	// directory.
	keep := func(p string, class rules.Class) error {
		return filepath.WalkDir(filepath.Join(res.Root, filepath.FromSlash(p)), func(host string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			rel, err := filepath.Rel(res.Root, host)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if inSubmodule(rel) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !tracked[rel] && domains.Facts(view, rel, nil).Ignored() {
				roots[rel] = class
				if entry.IsDir() {
					return filepath.SkipDir
				}
			}
			// A tracked file replaces anything formerly beneath it.
			if f, ok := files[rel]; ok && f.Mode != store.ModeDir && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		})
	}
	for p, class := range res.Roots {
		if path.Base(p) == ".git" || inSubmodule(p) {
			continue // discarded nested repositories need no object-directory walk; gitlinks start empty
		}
		if class == rules.Hidden && !tracked[p] {
			// REV tracks nothing here, so however large the root, it stays
			// absent whole, unwalked. Below a hidden root REV does track, REV's
			// files appear and only the ignored remainder stays hidden.
			roots[p] = rules.Hidden
			continue
		}
		if err := keep(p, class); err != nil {
			return err
		}
	}
	for p := range res.Files {
		if !inSubmodule(p) && !tracked[p] && domains.Facts(view, p, nil).Ignored() {
			roots[p] = rules.Local
		}
	}
	// An ignored descendant can survive a removed untracked directory. Keep
	// its ancestor directories visible without retaining their other files.
	dirs, parents := map[string]bool{}, map[string]bool{}
	for p, class := range roots {
		if inSubmodule(p) || class == rules.Hidden {
			continue
		}
		dir := path.Dir(p)
		if dir == "." {
			continue
		}
		dirs[dir] = true
		for p := path.Dir(dir); p != "."; p = path.Dir(p) {
			parents[p] = true
		}
	}
	for dir := range dirs {
		if !tracked[dir] && !parents[dir] {
			files[dir] = store.File{Mode: store.ModeDir, ID: store.EmptyTree}
		}
	}
	// A hidden root keeps no directory visible. As in an ordinary capture, it
	// is recorded only inside a directory the view keeps: below one the
	// revision dropped it is absent anyway, and takes retain no directory for it.
	for p, class := range roots {
		dir := path.Dir(p)
		if class != rules.Hidden || dir == "." || dirs[dir] || parents[dir] {
			continue
		}
		if f, ok := files[dir]; !tracked[dir] || ok && f.Mode != store.ModeDir {
			delete(roots, p)
		}
	}
	res.Files, res.Info, res.Roots, res.Repos, res.Domains = files, info, roots, repos, domains
	res.Tree, err = c.Store.Build(files)
	return err
}

// seedRevisionIndex rewrites the revision index with the stat data the view
// presents for every file it serves with exactly REV's blob and mode, so git's
// first refresh trusts those files instead of reading each one. It runs on the
// final capture: a path taken with other content, a gitlink, and any other path
// the view does not serve as REV's keep zero stat data, which git reads.
func seedRevisionIndex(ctx context.Context, c Config, res source.Result, revision *git.Revision, fork time.Time) error {
	stat := make(map[string]git.IndexStat, len(revision.Entries))
	for _, e := range revision.Entries {
		if res.Files[e.Path] != (store.File{Mode: e.Mode, ID: store.OID(e.OID)}) {
			continue
		}
		info := res.Info[e.Path]
		stat[e.Path] = git.IndexStat{Size: info.Size, Mtime: presentedMtime(info, fork)}
	}
	return revision.WriteIndex(ctx, filepath.Join(c.StateDir, revisionIndex), stat)
}
