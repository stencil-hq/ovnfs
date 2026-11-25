package git

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Ignore matches root-relative paths against git's ignore rules. Each
// directory's .gitignore is added when a walk or a lookup first reaches it.
type Ignore struct {
	mu       sync.RWMutex
	patterns []ignoreRule
	fold     bool
	loaded   map[string]bool
}

// ignoreRule matches Git's exact endpoint, recursive-glob and excluded-parent
// semantics. Its parts are path.Match globs, one per path component: unlike
// filepath.Match on Windows, path.Match keeps backslash escapes.
type ignoreRule struct {
	domain                     []string
	parts                      []string
	recursive                  []bool
	include, dirOnly, basename bool
}

// ignoreClasses translates Git's ASCII POSIX bracket classes to Go matcher ranges.
var ignoreClasses = map[string]string{
	"alnum": "A-Za-z0-9", "alpha": "A-Za-z", "blank": " \t",
	"cntrl": "\x00-\x1f\x7f", "digit": "0-9", "graph": "!-~",
	"lower": "a-z", "print": " -~", "punct": "!-." + ":-@" + "[-`" + "{-~",
	"space": " \t\r\n\v\f", "upper": "A-Z", "xdigit": "0-9A-Fa-f",
}

// expandIgnoreClasses rewrites Git's POSIX character classes, which path.Match
// does not recognize, as explicit ranges.
func expandIgnoreClasses(component string) string {
	if !strings.Contains(component, "[:") {
		return component
	}
	var out strings.Builder
	bracket := false
	for offset := 0; offset < len(component); {
		ch := component[offset]
		if ch == '\\' && offset+1 < len(component) {
			out.WriteString(component[offset : offset+2])
			offset += 2
			continue
		}
		if bracket && strings.HasPrefix(component[offset:], "[:") {
			if end := strings.Index(component[offset+2:], ":]"); end >= 0 {
				end += offset + 2
				if expanded, ok := ignoreClasses[component[offset+2:end]]; ok {
					out.WriteString(expanded)
					offset = end + 2
					continue
				}
			}
		}
		if ch == '[' {
			bracket = true
		} else if ch == ']' {
			bracket = false
		}
		out.WriteByte(ch)
		offset++
	}
	return out.String()
}

// newIgnoreRule parses a .gitignore line's negation and trailing spaces before
// compiling its pattern.
func newIgnoreRule(line string, domain []string) ignoreRule {
	include := strings.HasPrefix(line, "!")
	if include {
		line = line[1:]
	}
	line = trimTrailingSpaces(line)
	rule := newPatternRule(line, domain)
	rule.include = include
	return rule
}

// trimTrailingSpaces drops a line's unescaped trailing spaces, keeping the one
// a backslash quotes, as git's trim_trailing_spaces does: every space after the
// escaped one is unescaped, so only that one stays.
func trimTrailingSpaces(line string) string {
	last := -1
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			if last < 0 {
				last = i
			}
		case '\\':
			// Skip the quoted byte; a line ending in a backslash keeps it.
			if i++; i == len(line) {
				return line
			}
			last = -1
		default:
			last = -1
		}
	}
	if last >= 0 {
		return line[:last]
	}
	return line
}

// newPatternRule compiles a wildcard pattern with gitignore's scope rules
// (anchoring, trailing-slash directories, **), which gitattributes shares.
func newPatternRule(line string, domain []string) ignoreRule {
	rule := ignoreRule{domain: append([]string(nil), domain...)}
	rule.dirOnly = strings.HasSuffix(line, "/")
	line = strings.TrimSuffix(line, "/")
	rule.basename = !strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	for component := range strings.SplitSeq(line, "/") {
		rule.recursive = append(rule.recursive, component == "**" && !rule.basename)
		// Only a whole component has recursive semantics; other repeated stars
		// are ordinary stars.
		var normalized strings.Builder
		star := false
		escaped := false
		for _, ch := range component {
			if escaped {
				normalized.WriteRune(ch)
				escaped, star = false, false
				continue
			}
			if ch == '\\' {
				normalized.WriteRune(ch)
				escaped, star = true, false
				continue
			}
			if ch == '*' && star {
				continue
			}
			normalized.WriteRune(ch)
			star = ch == '*'
		}
		component = normalized.String()
		// Git uses ! as well as ^ to negate a bracket class; Go's matcher
		// accepts only ^. Escaped brackets remain literal.
		var bracketNormalized strings.Builder
		escaped = false
		for offset, ch := range component {
			if ch == '!' && offset > 0 && component[offset-1] == '[' && !escaped {
				bracketNormalized.WriteByte('^')
			} else {
				bracketNormalized.WriteRune(ch)
			}
			if ch == '\\' {
				escaped = !escaped
			} else if ch != '[' {
				escaped = false
			}
		}
		rule.parts = append(rule.parts, expandIgnoreClasses(bracketNormalized.String()))
	}
	return rule
}

// matchParts implements slash-separated ** backtracking without letting a pattern
// for a parent directory accidentally negate an unrelated rule for its child.
func (r *ignoreRule) matchParts(pattern, name int, parts []string, dir bool) bool {
	if pattern == len(r.parts) {
		return name == len(parts)
	}
	if r.recursive[pattern] {
		if pattern == len(r.parts)-1 {
			return name < len(parts)
		}
		for offset := name; offset <= len(parts); offset++ {
			if r.matchParts(pattern+1, offset, parts, dir) {
				return true
			}
		}
		return false
	}
	if name == len(parts) {
		return false
	}
	// A malformed glob matches nothing.
	if ok, err := path.Match(r.parts[pattern], parts[name]); err != nil || !ok {
		return false
	}
	return r.matchParts(pattern+1, name+1, parts, dir)
}

// matches applies a rule only to the candidate endpoint within its directory domain.
func (r *ignoreRule) matches(parts []string, dir bool) bool {
	if len(parts) <= len(r.domain) || r.dirOnly && !dir {
		return false
	}
	for n, component := range r.domain {
		if parts[n] != component {
			return false
		}
	}
	parts = parts[len(r.domain):]
	if r.basename {
		parts = parts[len(parts)-1:]
	}
	return r.matchParts(0, 0, parts, dir)
}

// parseIgnore reads Git's line-based format while retaining significant escaped characters.
func parseIgnore(reader io.Reader, domain []string, fold bool) ([]ignoreRule, error) {
	var patterns []ignoreRule
	if fold && len(domain) > 0 {
		domain = append([]string(nil), domain...)
		for n := range domain {
			domain[n] = strings.ToLower(domain[n])
		}
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fold {
			line = strings.ToLower(line)
		}
		patterns = append(patterns, newIgnoreRule(line, domain))
	}
	return patterns, scanner.Err()
}

// hostIgnore reads shared exclude files, ignoring only absent files.
func hostIgnore(name string, fold bool) ([]ignoreRule, error) {
	file, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return parseIgnore(file, nil, fold)
}

// NewIgnore starts a matcher for repository r with core.excludesFile and info/exclude.
// A nil r starts a matcher for a folder outside any repository: only its
// .gitignore files apply, the way `rg --no-require-git` treats them.
func NewIgnore(r *Repo) (*Ignore, error) {
	i := &Ignore{loaded: make(map[string]bool)}
	if r == nil {
		return i, nil
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return nil, err
	}
	if i.fold, _, err = cfg.Bool("core.ignorecase"); err != nil {
		return nil, err
	}
	global, _, err := cfg.Path("core.excludesFile")
	if err != nil {
		return nil, err
	}
	if global == "" {
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			configHome = filepath.Join(home, ".config")
		}
		global = filepath.Join(configHome, "git", "ignore")
	}
	if global != "" {
		if !filepath.IsAbs(global) {
			global = filepath.Join(r.Top, global)
		}
		patterns, err := hostIgnore(global, i.fold)
		if err != nil {
			return nil, err
		}
		i.patterns = append(i.patterns, patterns...)
	}
	patterns, err := hostIgnore(filepath.Join(r.CommonDir, "info", "exclude"), i.fold)
	if err != nil {
		return nil, err
	}
	i.patterns = append(i.patterns, patterns...)
	return i, nil
}

// Add appends the rules of dir's .gitignore. A walk adds parents before
// children, so deeper rules come later and win, as in git.
func (i *Ignore) Add(dir string, data []byte) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.addLocked(dir, data)
}

// addLocked parses data in dir's domain and marks dir loaded.
func (i *Ignore) addLocked(dir string, data []byte) error {
	i.loaded[dir] = true
	if len(data) == 0 {
		return nil
	}
	var domain []string
	if dir != "" {
		domain = strings.Split(dir, "/")
	}
	patterns, err := parseIgnore(bytes.NewReader(data), domain, i.fold)
	if err != nil {
		return err
	}
	i.patterns = append(i.patterns, patterns...)
	return nil
}

// Ensure loads the .gitignore of dir and of each ancestor not seen yet, root first.
// read returns a file's bytes; os.ErrNotExist means the directory has none.
func (i *Ignore) Ensure(dir string, read func(name string) ([]byte, error)) error {
	var chain []string
	for d := dir; ; d = parentDir(d) {
		chain = append(chain, d)
		if d == "" {
			break
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, d := range slices.Backward(chain) {

		if i.loaded[d] {
			continue
		}
		data, err := read(path.Join(d, ".gitignore"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := i.addLocked(d, data); err != nil {
			return err
		}
	}
	return nil
}

// parentDir returns a root-relative path's parent, "" for top-level names.
func parentDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// match applies the last matching pattern, whose directory domain enforces scope.
func (i *Ignore) match(parts []string, dir bool) bool {
	for n := len(i.patterns) - 1; n >= 0; n-- {
		if i.patterns[n].matches(parts, dir) {
			return !i.patterns[n].include
		}
	}
	return false
}

// Ignored reports Git-style exclusion, including the rule that excluded parents
// prevent a descendant's negation from making that descendant visible.
func (i *Ignore) Ignored(p string, dir bool) bool {
	p = strings.TrimPrefix(path.Clean(filepath.ToSlash(p)), "./")
	if i.fold {
		p = strings.ToLower(p)
	}
	if p == "." || p == "" {
		return false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	for n := 1; n < len(parts); n++ {
		if i.match(parts[:n], true) {
			return true
		}
	}
	return i.match(parts, dir)
}
