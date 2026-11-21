package git

import (
	"crypto/sha1"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/internal/hostfile"
)

// A checkout of a repository gets a private git directory instead of a worktree of
// the user's repository: HEAD, refs, config and the index are copies taken at
// capture, and "objects" links to an object directory shared by every checkout on
// same base, which borrows the user's objects through alternates. Commits
// made in a checkout land there; ovnfs never writes the user's repository,
// except for checkout remotes (see SetCheckoutRemotes). A checkout whose "hooks"
// links to the repository's own (see LinkHooks) writes there through it.

// PrepareObjects creates a base's shared object directory that reads the repository's objects through alternates.
func (r *Repo) PrepareObjects(dir string) error {
	for _, sub := range []string{"info", "pack"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "info", "alternates"), []byte(filepath.Join(r.CommonDir, "objects")+"\n"), 0o644)
}

// ModuleDir returns where sub, a repository nested in r's work tree, keeps its git
// directory relative to r's: "modules/<name>" for a submodule absorbed into r (its
// .git a gitfile into r's modules directory, as git submodule lays it out), or ""
// for any other nested repository. A checkout gives an absorbed submodule a private
// git directory at that path below the root's, where git looks for it.
func (r *Repo) ModuleDir(sub *Repo) string {
	gitdir := realPath(sub.GitDir)
	if gitdir != realPath(sub.CommonDir) {
		return "" // a linked work tree of the submodule
	}
	for _, own := range []string{r.GitDir, r.CommonDir} {
		rel, err := filepath.Rel(filepath.Join(realPath(own), "modules"), gitdir)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "modules/" + filepath.ToSlash(rel)
		}
	}
	return ""
}

// realPath resolves p's symlinks so paths git reports through different spellings
// compare equal; a path that cannot be resolved is compared as given.
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// MakeGitdir writes a checkout's private git directory from the repository's current
// HEAD, refs and config, the captured index, and a link to the shared objects. It
// runs no git process: the config is edited as it is copied.
func (r *Repo) MakeGitdir(gitdir, objects, index string) error {
	if err := os.Mkdir(gitdir, 0o755); err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(r.CommonDir, "config"))
	if err != nil {
		return err
	}
	// The checkout is its own work tree (the directory holding the .git file), and its
	// files keep their mtimes but not their inodes, so stat checks stay minimal.
	edited := editConfig(string(config), "core", []configEntry{
		{"bare", "false"},
		{"checkStat", "minimal"},
		{"trustctime", "false"},
		{"untrackedCache", "false"},
		{"fsmonitor", "false"},
	}, "worktree")
	if err := os.WriteFile(filepath.Join(gitdir, "config"), []byte(edited), 0o644); err != nil {
		return err
	}
	copies := []struct {
		src, dst string
		required bool
	}{
		{filepath.Join(r.GitDir, "HEAD"), "HEAD", true},
		{filepath.Join(r.GitDir, "config.worktree"), "config.worktree", false},
		{filepath.Join(r.CommonDir, "packed-refs"), "packed-refs", false},
		{filepath.Join(r.CommonDir, "shallow"), "shallow", false},
		{filepath.Join(r.CommonDir, "info", "exclude"), filepath.Join("info", "exclude"), false},
	}
	for _, c := range copies {
		dst := filepath.Join(gitdir, c.dst)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		err := clone.Tree(c.src, dst)
		if err != nil && (c.required || !errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	// Empty directories hold no refs, and ref deletions leave many behind.
	if err := copyFiles(filepath.Join(r.CommonDir, "refs"), filepath.Join(gitdir, "refs")); err != nil {
		return err
	}
	if err := clone.File(index, filepath.Join(gitdir, "index")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := emptyIndex(filepath.Join(gitdir, "index")); err != nil {
			return err
		}
	}
	return hostfile.Symlink(objects, filepath.Join(gitdir, "objects"))
}

// LinkHooks links gitdir's hooks to the repository's, so git finds the hooks a
// linked work tree of the repository would. It leaves gitdir alone when gitdir
// already has a hooks entry, such as the real directory of a submodule cloned
// inside a checkout. When the repository's hooks are gone, are not a directory
// or can never resolve (a symlink loop, a path through a file), it links nothing
// and removes gitdir's hooks entry if that is exactly the link LinkHooks made to
// it before; any other entry stays. When the hooks cannot be examined (a
// permission or I/O error) it changes nothing: a link must not go on a failure
// that may pass.
func (r *Repo) LinkHooks(gitdir string) error {
	hooks := filepath.Join(r.CommonDir, "hooks")
	link := filepath.Join(gitdir, "hooks")
	info, err := os.Stat(hooks)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !hostfile.Unresolvable(err) {
		return nil
	}
	if err != nil || !info.IsDir() {
		if target, err := os.Readlink(link); err == nil && target == hooks {
			return os.Remove(link)
		}
		return nil
	}
	if _, err := os.Lstat(link); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return hostfile.Symlink(hooks, link)
}

// UnsetModuleWorktrees removes core.worktree from every submodule git directory
// below a private git directory restored for another checkout, as MakeGitdir does
// at creation: git records it when it (re)connects a submodule, as a path into the
// checkout it ran in, and each restored directory must serve the checkout whose
// .git file points at it.
func UnsetModuleWorktrees(gitdir string) error {
	modules, err := moduleGitdirs(gitdir)
	if err != nil {
		return err
	}
	for _, m := range modules {
		err := rewriteConfig(filepath.Join(gitdir, filepath.FromSlash(m)), func(config string) string {
			return editConfig(config, "core", nil, "worktree")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// moduleGitdirs lists the submodule git directories below gitdir where git keeps
// them, modules/<name> and recursively <module>/modules/<name>, as slash paths
// relative to gitdir. A name may hold slashes, so any directory below modules/
// holding a HEAD file is a git directory and any other is walked through.
func moduleGitdirs(gitdir string) ([]string, error) {
	var found []string
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub, subRel := filepath.Join(dir, e.Name()), rel+"/"+e.Name()
			if info, err := os.Lstat(filepath.Join(sub, "HEAD")); err == nil && info.Mode().IsRegular() {
				found = append(found, subRel)
				err = walk(filepath.Join(sub, "modules"), subRel+"/modules")
			} else {
				err = walk(sub, subRel)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	err := walk(filepath.Join(gitdir, "modules"), "modules")
	return found, err
}

// CopyGitdir copies a private git directory to the new path dst, linking the copy's
// objects to the shared directory instead of copying them. The registry uses it for
// checkouts that reuse a capture, from a directory MakeGitdir made for them, and for
// snapshots taken by builds that saved a whole directory copy per snapshot.
func CopyGitdir(src, dst, objects string) error {
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "objects" {
			continue
		}
		if err := clone.Tree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return hostfile.Symlink(objects, filepath.Join(dst, "objects"))
}

// MarkCheckout sets ovnfs.checkout to name in a private git directory's own config, so
// only this directory identifies as an ovnfs checkout (inherited user config and the
// source repo stay unmarked). The registry calls it whenever it creates or reopens a
// checkout; it keeps every other line and leaves the file untouched when the marker
// already names the checkout.
func MarkCheckout(gitdir, name string) error {
	return rewriteConfig(gitdir, func(config string) string { return markConfig(config, name) })
}

// MarkNew writes config, with ovnfs.checkout set to name as MarkCheckout sets it,
// as the config of git directory gitdir, which nothing reads yet: in one write,
// not atomically.
func MarkNew(gitdir, config, name string) error {
	return os.WriteFile(filepath.Join(gitdir, "config"), []byte(markConfig(config, name)), 0o644)
}

// markConfig rewrites git config text so its [ovnfs] section holds exactly one
// checkout = name (see editConfig).
func markConfig(config, name string) string {
	return editConfig(config, "ovnfs", []configEntry{{"checkout", name}})
}

// rewriteConfig replaces gitdir's config file with edit's result, atomically and
// keeping its mode, and leaves it untouched when edit changes nothing. It edits the
// file in place of exec'ing git config.
func rewriteConfig(gitdir string, edit func(string) string) error {
	path := filepath.Join(gitdir, "config")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	edited := edit(string(data))
	if edited == string(data) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return replaceFile(path, edited, info.Mode().Perm())
}

// replaceFile atomically replaces file name with data and mode perm. The
// temporary file's name ends in .lock, which the folder watcher ignores in a
// source repository's git directory.
func replaceFile(name, data string, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+"-*.lock")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, err = f.WriteString(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// hostfile.Rename replaces a read-only file, which Windows' plain rename refuses.
	return hostfile.Rename(tmp, name)
}

// configEntry is one variable a config edit sets.
type configEntry struct{ key, value string }

// editConfig rewrites git config text as git config --file would: every
// occurrence of set's and unset's variables in section (no subsection, names
// compared case-insensitively) is removed, then set's entries are placed right
// after the section's first header, or in a new section at the end. Every other
// line, comments and subsections included, is kept; "[section] key = old" keeps
// its header and loses the old value.
func editConfig(config, section string, set []configEntry, unset ...string) string {
	drop := func(line string) bool {
		key := configKey(line)
		for _, e := range set {
			if strings.EqualFold(key, e.key) {
				return true
			}
		}
		for _, k := range unset {
			if strings.EqualFold(key, k) {
				return true
			}
		}
		return false
	}
	var out []string
	insert, inSection, continued := -1, false, false
	for _, line := range strings.SplitAfter(config, "\n") {
		if line == "" {
			continue
		}
		if continued {
			continued = continuesLine(line)
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			end := strings.IndexByte(trimmed, ']')
			if end < 0 {
				inSection = false
				out = append(out, line)
				continue
			}
			inSection = strings.EqualFold(strings.TrimSpace(trimmed[1:end]), section)
			if inSection && drop(trimmed[end+1:]) {
				continued = continuesLine(line)
				line = trimmed[:end+1] + "\n"
			}
			out = append(out, line)
			if inSection && insert < 0 {
				insert = len(out)
			}
			continue
		}
		if inSection && drop(trimmed) {
			continued = continuesLine(line)
			continue
		}
		out = append(out, line)
	}
	if len(set) == 0 {
		return strings.Join(out, "")
	}
	entries := make([]string, 0, len(set))
	for _, e := range set {
		entries = append(entries, "\t"+e.key+" = "+configValue(e.value)+"\n")
	}
	if insert < 0 {
		if n := len(out); n != 0 && !strings.HasSuffix(out[n-1], "\n") {
			out[n-1] += "\n"
		}
		out = append(append(out, "["+section+"]\n"), entries...)
	} else {
		out = append(out[:insert], append(entries, out[insert:]...)...)
	}
	return strings.Join(out, "")
}

// configKey returns the variable name a config line inside a section sets.
func configKey(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, "= \t"); i >= 0 {
		return line[:i]
	}
	return line
}

// continuesLine reports whether a config line ends in an unescaped backslash, which
// continues its value on the next line.
func continuesLine(line string) bool {
	line = strings.TrimRight(line, "\r\n")
	n := len(line) - len(strings.TrimRight(line, `\`))
	return n%2 == 1
}

// configValue spells a git config value, quoting and escaping it when git would
// otherwise trim, split or misread it.
func configValue(v string) string {
	if v != "" && v == strings.TrimSpace(v) && !strings.ContainsAny(v, "\"\\#;\n\t") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(v) + `"`
}

// WriteGitfile points a checkout's root at its private git directory.
func WriteGitfile(root, gitdir string) error {
	content, err := Gitfile(gitdir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, ".git"), content, 0o644)
}

// ParseGitfile returns the git directory a .git file's content points at, as
// written (relative paths are relative to the file's directory), and false for
// content that is not a gitfile.
func ParseGitfile(content []byte) (string, bool) {
	line, _, _ := strings.Cut(string(content), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "gitdir: ")
	return gitdir, ok && gitdir != ""
}

// Gitfile is the content of a .git file that points at the absolute git directory gitdir.
func Gitfile(gitdir string) ([]byte, error) {
	if strings.ContainsAny(gitdir, "\r\n") {
		return nil, errors.New("git directory path contains a newline")
	}
	return []byte("gitdir: " + gitdir + "\n"), nil
}

// emptyIndex writes a valid empty version-two index for an unborn repository.
func emptyIndex(name string) error {
	const header = "DIRC\x00\x00\x00\x02\x00\x00\x00\x00"
	checksum := sha1.Sum([]byte(header))
	return os.WriteFile(name, append([]byte(header), checksum[:]...), 0o600)
}

// copyFiles clones the files and symlinks below directory src to the new directory
// dst, creating only the directories that lead to them.
func copyFiles(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	made := map[string]bool{dst: true}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if dir := filepath.Dir(target); !made[dir] {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			made[dir] = true
		}
		return clone.Tree(p, target)
	})
}
