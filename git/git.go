// Package git reads facts from git repositories found under a source folder.
//
// Git is never required: a folder without a repository is captured the same
// way, and repositories are read and written in process, without a git
// installation. Only fetches run the git binary, when it is installed, falling
// back to package fetch without it, and so does deleting the tracking refs git
// fetched for checkout remotes. Where a repository exists, it contributes
// facts (which paths are tracked or ignored, which commit is pushed), an object
// database to read unchanged tracked content from, and a private git directory
// that makes `git` work inside a checkout without writing to the user's
// repository, except for checkout remotes (see SetCheckoutRemotes) and hooks
// a checkout links to the repository's own (see LinkHooks).
//
// The engines under git/internal implement git's formats and protocols from
// git's documentation: object (objects and packs), config, refs, index, merge
// (three-way file merge), repo (discovery), rev (revision syntax and history)
// and fetch (transports).
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/index"
	"github.com/stencil-hq/ovnfs/git/internal/merge"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// ErrNotRepo reports a directory outside a git work tree.
var ErrNotRepo = errors.New("not a git work tree")

// Repo identifies a work tree and its shared object database.
type Repo struct {
	Top, CommonDir, GitDir string
	repo                   *repo.Repository
}

// newRepo wraps a repository found or created by package repo.
func newRepo(h *repo.Repository) *Repo {
	return &Repo{Top: h.Top, CommonDir: h.CommonDir, GitDir: h.GitDir, repo: h}
}

// Open resolves the work tree containing dir, its common directory and its own
// git directory, as `git rev-parse --show-toplevel --git-common-dir --git-dir`
// reports them (physical paths). Any directory git would not treat as inside a
// work tree, including an unsafe or unsupported repository, is ErrNotRepo.
func Open(dir string) (*Repo, error) {
	h, err := repo.Discover(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotRepo, err)
	}
	return newRepo(h), nil
}

// IsRoot reports whether dir holds a .git entry, the cheap check made before Open while walking.
func IsRoot(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// session runs fn with r's revision source ready, its objects open until fn returns.
func (r *Repo) session(fn func(s *rev.Source) error) error {
	return r.sessionObjects(func(s *rev.Source, _ *repo.Objects) error { return fn(s) })
}

// sessionObjects is session for operations that also walk trees.
func (r *Repo) sessionObjects(fn func(s *rev.Source, objects *repo.Objects) error) error {
	objects, err := r.repo.OpenObjects()
	if err != nil {
		return err
	}
	defer objects.Close()
	s, err := r.repo.RevSource(objects)
	if err != nil {
		return err
	}
	return fn(&s, objects)
}

// resolveCommit resolves spec to a commit as `git rev-parse --verify --quiet
// <spec>^{commit}` does: "" when spec names no commit.
func resolveCommit(s *rev.Source, spec string) (string, error) {
	id, err := rev.Resolve(s, spec+"^{commit}", rev.Options{})
	for _, quiet := range []error{rev.ErrUnknown, rev.ErrAmbiguous, rev.ErrPeel, rev.ErrNoUpstream, rev.ErrNotBranch} {
		if errors.Is(err, quiet) {
			return "", nil
		}
	}
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// IndexEntry is one tracked path with the stat data git cached for it.
type IndexEntry struct {
	Path         string
	OID          string
	Mode         uint32
	Size         uint32
	Mtime, Ctime time.Time
	Inode        uint32
}

// Index is the decoded index of a repository at the moment it was read.
type Index struct {
	Entries  []IndexEntry
	Modified time.Time // the index file's mtime; entries modified at or after it are racily clean

	// attrFiles lists every stage-0 regular .gitattributes blob, including
	// entries Entries omits (skip-worktree, intent-to-add): the work tree
	// fallback of Attributes.
	attrFiles map[string]object.ID
}

// IndexPath is the work tree's live index file.
func (r *Repo) IndexPath() string { return filepath.Join(r.GitDir, "index") }

// ReadIndex decodes index file name, written for r, keeping the stage-zero
// entries that are checked out (not skip-worktree or intent-to-add). A missing
// index is empty.
func (r *Repo) ReadIndex(name string) (Index, error) {
	idx, err := index.Read(name, r.repo.Format, index.ReadOptions{})
	if errors.Is(err, os.ErrNotExist) {
		return Index{}, nil
	}
	if err != nil {
		return Index{}, fmt.Errorf("read %s: %w", name, err)
	}
	out := Index{Entries: make([]IndexEntry, 0, len(idx.Entries)), Modified: idx.ModTime, attrFiles: map[string]object.ID{}}
	for _, e := range idx.Entries {
		if isIndexedAttrFile(e) {
			out.attrFiles[e.Path] = e.ID
		}
		if e.Stage != 0 || e.SkipWorktree || e.IntentToAdd {
			continue
		}
		out.Entries = append(out.Entries, IndexEntry{
			Path:  e.Path,
			OID:   e.ID.String(),
			Mode:  uint32(e.Mode),
			Size:  e.Stat.Size,
			Mtime: time.Unix(int64(e.Stat.MTime.Sec), int64(e.Stat.MTime.Nsec)),
			Ctime: time.Unix(int64(e.Stat.CTime.Sec), int64(e.Stat.CTime.Nsec)),
			Inode: e.Stat.Ino,
		})
	}
	return out, nil
}

// isIndexedAttrFile reports a stage-0 .gitattributes entry that holds a regular file's blob.
func isIndexedAttrFile(e index.Entry) bool {
	return e.Stage == 0 && path.Base(e.Path) == ".gitattributes" && e.Mode != object.ModeSymlink && e.Mode.Kind() == object.Blob
}

// Head returns the commit HEAD points at, or "" on an unborn branch.
func (r *Repo) Head(ctx context.Context) (head string, err error) {
	err = r.session(func(s *rev.Source) error {
		head, err = resolveCommit(s, "HEAD")
		return err
	})
	return head, err
}

// Busy names an unfinished merge, rebase, cherry-pick or revert, whose index cannot be trusted as a base.
func (r *Repo) Busy() string {
	for _, state := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(r.GitDir, state)); err == nil {
			return state
		}
	}
	return ""
}

// Upstream picks the remote-tracking branch a launch elsewhere would fetch from, as
// "<remote>/<branch>": the current branch's upstream, else origin's default branch.
// Both results are "" when the repository has neither.
func (r *Repo) Upstream(ctx context.Context) (ref, tip string, err error) {
	err = r.session(func(s *rev.Source) error {
		for _, candidate := range []string{"@{upstream}", "origin/HEAD"} {
			// A missing upstream is a lookup failure, not an error to report.
			name, lookupErr := rev.AbbrevRef(s, candidate)
			if lookupErr != nil || name == "" || name == candidate {
				continue
			}
			c, err := resolveCommit(s, name)
			if err != nil || c != "" {
				ref, tip = name, c
				return err
			}
		}
		return nil
	})
	return ref, tip, err
}

// Remote returns the URL of the remote an Upstream name such as "origin/main" belongs to.
func (r *Repo) Remote(ctx context.Context, upstream string) (string, error) {
	name, _, _ := strings.Cut(upstream, "/")
	if name == "" {
		name = "origin"
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return "", err
	}
	url, _ := cfg.Get("remote." + name + ".url")
	return url, nil
}

// Contains reports whether commit is reachable from tip.
func (r *Repo) Contains(ctx context.Context, tip, commit string) (contained bool, err error) {
	err = r.session(func(s *rev.Source) error {
		a, b, err := commitIDs(s, commit, tip)
		if err != nil {
			return err
		}
		contained, err = rev.IsAncestor(s, a, b)
		return err
	})
	return contained, err
}

// commitIDs resolves two commit names.
func commitIDs(s *rev.Source, a, b string) (object.ID, object.ID, error) {
	var ids [2]object.ID
	for i, name := range []string{a, b} {
		id, err := rev.Resolve(s, name+"^{commit}", rev.Options{})
		if err != nil {
			return object.ID{}, object.ID{}, err
		}
		ids[i] = id
	}
	return ids[0], ids[1], nil
}

// TreeEntry is a recursive ls-tree record; Mode uses git's octal mode encoding.
type TreeEntry struct {
	Mode            uint32
	Type, OID, Path string
	Size            int64
}

// LsTree returns the recursive tree of a commit or tree, files and gitlinks only.
func (r *Repo) LsTree(ctx context.Context, tree string) ([]TreeEntry, error) {
	return r.lsTree(ctx, tree, false)
}

// lsTree is `git ls-tree -r --full-tree [-l] <tree>`; sizes adds blob sizes.
func (r *Repo) lsTree(ctx context.Context, tree string, sizes bool) (entries []TreeEntry, err error) {
	err = r.sessionObjects(func(s *rev.Source, objects *repo.Objects) error {
		root, err := rev.Resolve(s, tree+"^{tree}", rev.Options{})
		if err != nil {
			return err
		}
		for e, err := range objects.Walk(root, object.WalkOptions{}) {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := TreeEntry{Mode: uint32(e.Mode), Type: e.Mode.Kind().String(), OID: e.ID.String(), Path: e.Name}
			if sizes && e.Mode != object.ModeGitlink {
				if _, entry.Size, err = objects.Header(e.ID); err != nil {
					return err
				}
			}
			entries = append(entries, entry)
		}
		return nil
	})
	return entries, err
}

// Labels name the three sides of a merge in the conflict markers it writes.
type Labels = merge.Labels

// MergeFile merges theirs into ours against base as `git merge-file -p` does,
// in the conflict style of the user's merge.conflictStyle; clean reports whether
// the result has no conflict markers, which carry labels. Content git refuses to
// merge (binary) is an error.
func MergeFile(ours, base, theirs []byte, labels Labels) (merged []byte, clean bool, err error) {
	cfg, err := config.Load(config.Options{})
	if err != nil {
		return nil, false, err
	}
	opts := merge.Options{Labels: labels}
	if style, ok := cfg.Get("merge.conflictStyle"); ok {
		switch style {
		case "merge":
		case "diff3":
			opts.Style = merge.StyleDiff3
		case "zdiff3":
			opts.Style = merge.StyleZDiff3
		default:
			return nil, false, fmt.Errorf("unknown style %q given for merge.conflictstyle", style)
		}
	}
	merged, conflicts, err := merge.File(ours, base, theirs, opts)
	if err != nil {
		return nil, false, err
	}
	return merged, conflicts == 0, nil
}
