// Package source captures a folder into a snapshot: it walks the folder once,
// classifies every path with the checkout's rules, and stores the content of
// source-class files. Any folder works; git repositories found under it, at
// the root or nested, only contribute facts to the rules.
package source

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/rules"
)

// Repo is a git repository found under a source root.
type Repo struct {
	Path  string `json:"path"`            // relative to the source root; "" is the root
	Head  string `json:"head,omitempty"`  // HEAD commit at capture; "" when unborn
	Index string `json:"index,omitempty"` // index copy taken at capture; "" reads the live index
	// Module places an absorbed submodule's private git directory below the root's
	// ("modules/<name>", nested as git nests them, see git.Repo.ModuleDir); "" for
	// the root and for nested repositories whose .git stays a local copy.
	Module string `json:"module,omitempty"`
}

// View is the read-only tree that facts are read from: the source folder at
// capture, a checkout's merged view when snapshots are hashed.
type View interface {
	Lstat(p string) (os.FileInfo, error)
	ReadFile(p string) ([]byte, error)
}

// Dir is a View over a host directory.
type Dir string

// Lstat returns metadata of a root-relative path without following a final symlink.
func (d Dir) Lstat(p string) (os.FileInfo, error) { return os.Lstat(d.path(p)) }

// ReadFile reads a root-relative regular file.
func (d Dir) ReadFile(p string) ([]byte, error) { return os.ReadFile(d.path(p)) }

// path joins a root-relative slash path to the host root.
func (d Dir) path(p string) string { return filepath.Join(string(d), filepath.FromSlash(p)) }

// domain holds the facts of one git repository (or of the plain root) for its subtree.
type domain struct {
	path    string          // root-relative; "" for the root
	repo    *git.Repo       // nil outside any repository
	module  string          // Repo.Module
	tracked map[string]bool // domain-relative tracked paths and their ancestor directories
	index   map[string]git.IndexEntry
	indexAt git.Index
	ignore  *git.Ignore
}

// rel converts a root-relative path to this domain's relative path.
func (d *domain) rel(p string) string {
	if d.path == "" {
		return p
	}
	return strings.TrimPrefix(p, d.path+"/")
}

// Domains answers rule facts for every path of one source root, switching to a
// nested repository's facts below that repository's root.
type Domains struct {
	root  string
	mu    sync.RWMutex
	list  []*domain // longest path first, so the innermost domain matches first
	repos []Repo
}

// NewDomains opens the facts of root and of the given repositories. A root
// without a Repo entry for "" is a plain folder whose .gitignore files still apply.
func NewDomains(root string, repos []Repo) (*Domains, error) {
	d := &Domains{root: root}
	rootRepo := false
	for _, r := range repos {
		if r.Path == "" {
			rootRepo = true
		}
		if err := d.Add(r); err != nil {
			return nil, err
		}
	}
	if !rootRepo {
		ignore, err := git.NewIgnore(nil)
		if err != nil {
			return nil, err
		}
		d.insert(&domain{path: "", tracked: map[string]bool{}, index: map[string]git.IndexEntry{}, ignore: ignore})
	}
	return d, nil
}

// Add opens a repository whose work tree is r.Path below the root and reads its index.
func (d *Domains) Add(r Repo) error {
	repo, err := git.Open(filepath.Join(d.root, filepath.FromSlash(r.Path)))
	if err != nil {
		return err
	}
	return d.add(r, repo)
}

// add reads the index of repo, opened at r.Path, and adds its facts.
func (d *Domains) add(r Repo, repo *git.Repo) error {
	name := r.Index
	if name == "" {
		name = repo.IndexPath()
	}
	index, err := repo.ReadIndex(name)
	if err != nil {
		return err
	}
	ignore, err := git.NewIgnore(repo)
	if err != nil {
		return err
	}
	dom := &domain{path: r.Path, repo: repo, module: r.Module, tracked: make(map[string]bool, len(index.Entries)), index: make(map[string]git.IndexEntry, len(index.Entries)), indexAt: index, ignore: ignore}
	for _, e := range index.Entries {
		dom.index[e.Path] = e
		for p := e.Path; p != ""; p = parent(p) {
			if dom.tracked[p] {
				break
			}
			dom.tracked[p] = true
		}
	}
	d.mu.Lock()
	d.repos = append(d.repos, r)
	d.mu.Unlock()
	d.insert(dom)
	return nil
}

// insert keeps domains ordered innermost first.
func (d *Domains) insert(dom *domain) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list = append(d.list, dom)
	sort.SliceStable(d.list, func(i, j int) bool { return len(d.list[i].path) > len(d.list[j].path) })
}

// Repos lists the repositories opened so far, root first.
func (d *Domains) Repos() []Repo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := append([]Repo(nil), d.repos...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Repo returns the opened repository whose work tree is root-relative path p.
func (d *Domains) Repo(p string) *git.Repo {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, dom := range d.list {
		if dom.path == p {
			return dom.repo
		}
	}
	return nil
}

// Module returns Repo.Module of the opened repository whose work tree is p, "" for
// any path that is not an absorbed submodule.
func (d *Domains) Module(p string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, dom := range d.list {
		if dom.path == p {
			return dom.module
		}
	}
	return ""
}

// moduleOf returns Repo.Module for repo, opened at root-relative path p: where its
// private git directory goes below the root's when it is a submodule absorbed into
// the root or into a submodule that is itself absorbed, else "".
func (d *Domains) moduleOf(p string, repo *git.Repo) string {
	parent := d.For(p)
	if parent.repo == nil || (parent.path != "" && parent.module == "") {
		return ""
	}
	if m := parent.repo.ModuleDir(repo); m != "" {
		return path.Join(parent.module, m)
	}
	return ""
}

// RepoOf returns the repository whose facts apply to p, nil outside any repository.
func (d *Domains) RepoOf(p string) *git.Repo { return d.For(p).repo }

// IsRepoRoot reports whether p is the work tree root of a known repository.
func (d *Domains) IsRepoRoot(p string) bool { return d.Repo(p) != nil }

// RootIsRepo reports whether the source root itself is a repository.
func (d *Domains) RootIsRepo() bool { return d.IsRepoRoot("") }

// For returns the domain whose facts apply to p: the innermost repository strictly
// containing p. A repository's own root directory is classified by its parent domain.
func (d *Domains) For(p string) *domain {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, dom := range d.list {
		if dom.path == "" || strings.HasPrefix(p, dom.path+"/") {
			return dom
		}
	}
	return nil
}

// Discover opens repositories rooted at any directory on the way to p that the
// view shows a .git entry in, for explaining a path no capture has walked.
func (d *Domains) Discover(view View, p string) {
	parts := strings.Split(p, "/")
	for n := 1; n < len(parts); n++ {
		prefix := strings.Join(parts[:n], "/")
		if d.IsRepoRoot(prefix) {
			continue
		}
		if _, err := view.Lstat(prefix + "/.git"); err == nil {
			if repo, err := git.Open(filepath.Join(d.root, filepath.FromSlash(prefix))); err == nil {
				_ = d.add(Repo{Path: prefix, Module: d.moduleOf(prefix, repo)}, repo)
			}
		}
	}
}

// Facts returns lazily evaluated rule facts for root-relative path p; info may be
// nil, in which case it is read from view on first use (and a missing path has no facts).
func (d *Domains) Facts(view View, p string, info os.FileInfo) rules.Facts {
	return &facts{dom: d.For(p), view: view, p: p, info: info, statted: info != nil}
}

// facts implements rules.Facts for one path.
type facts struct {
	dom     *domain
	view    View
	p       string
	info    os.FileInfo
	statted bool
}

// stat reads metadata once; a missing path reports no facts rather than failing.
func (f *facts) stat() os.FileInfo {
	if !f.statted {
		f.statted = true
		f.info, _ = f.view.Lstat(f.p)
	}
	return f.info
}

// Dir reports a real directory (a symlink to one is not).
func (f *facts) Dir() bool { info := f.stat(); return info != nil && info.IsDir() }

// Size is a regular file's length.
func (f *facts) Size() int64 {
	if info := f.stat(); info != nil && info.Mode().IsRegular() {
		return info.Size()
	}
	return 0
}

// Tracked reports a path in its repository's index, or a directory holding one.
func (f *facts) Tracked() bool { return f.dom.tracked[f.dom.rel(f.p)] }

// Ignored reports git's own notion: untracked and excluded by ignore rules.
func (f *facts) Ignored() bool {
	if f.Tracked() {
		return false
	}
	rel := f.dom.rel(f.p)
	prefix := f.dom.path
	read := func(name string) ([]byte, error) {
		if prefix != "" {
			name = prefix + "/" + name
		}
		return f.view.ReadFile(name)
	}
	if err := f.dom.ignore.Ensure(parent(rel), read); err != nil {
		return false
	}
	return f.dom.ignore.Ignored(rel, f.Dir())
}

// CacheDir reports a directory tagged as a cache by CACHEDIR.TAG.
func (f *facts) CacheDir() bool { return f.holds("CACHEDIR.TAG") }

// VenvDir reports a Python virtual environment: a directory holding pyvenv.cfg.
func (f *facts) VenvDir() bool { return f.holds("pyvenv.cfg") }

// holds reports a directory with a regular file named name directly inside it.
func (f *facts) holds(name string) bool {
	if !f.Dir() {
		return false
	}
	info, err := f.view.Lstat(f.p + "/" + name)
	return err == nil && info.Mode().IsRegular()
}

// parent returns a root-relative path's parent, "" for top-level names.
func parent(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// IsJJPath reports a path that the jj adapter owns: the root's .jj, a jj
// workspace's state, or anything below it.
func IsJJPath(p string) bool { return p == ".jj" || strings.HasPrefix(p, ".jj/") }

// IsGitPath reports a path that the git adapter owns: a .git entry or anything below one.
func IsGitPath(p string) bool {
	for part := range strings.SplitSeq(p, "/") {
		if part == ".git" {
			return true
		}
	}
	return false
}
