// Package rules classifies every path of a source folder into source, cache,
// local or hidden with an ordered list of `<class> <predicate>` rules.
package rules

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Class is what a checkout does with a path: freeze and share it, keep it here, or hide it.
type Class string

// Classes, from "leaves the machine" to "never visible".
const (
	// Source paths are frozen at launch, hashed into snapshots and synced elsewhere.
	Source Class = "source"
	// Cache paths are copy-on-write here and rebuilt or empty elsewhere.
	Cache Class = "cache"
	// Local paths are copy-on-write here and absent elsewhere.
	Local Class = "local"
	// Hidden paths are absent everywhere.
	Hidden Class = "hidden"
)

// ParseClass converts a class word into a Class, rejecting unknown words.
func ParseClass(s string) (Class, error) {
	switch c := Class(s); c {
	case Source, Cache, Local, Hidden:
		return c, nil
	}
	return "", fmt.Errorf("unknown class %q", s)
}

// Facts answers predicate questions about one path; implementations compute
// lazily, so evaluate only what an expression needs.
type Facts interface {
	// Dir reports whether the path is a directory.
	Dir() bool
	// Size is the regular file size; 0 for dirs.
	Size() int64
	// Tracked reports whether the path is in a git index (false outside repos).
	Tracked() bool
	// Ignored reports whether .gitignore rules exclude the path.
	Ignored() bool
	// CacheDir reports whether the path is a directory containing CACHEDIR.TAG.
	CacheDir() bool
	// VenvDir reports whether the path is a directory containing pyvenv.cfg:
	// a Python virtual environment.
	VenvDir() bool
}

// Rule assigns Class to every path its predicate matches.
type Rule struct {
	Class  Class
	Text   string // predicate as written, trimmed, without the class word or comment
	Origin string // "builtin:3", ".o2/ovnfs.yml:6", "flag"
	expr   node
}

// String renders the rule as a parseable `<class> <text>` line.
func (r Rule) String() string {
	return string(r.Class) + " " + r.Text
}

// Matches reports whether the rule's predicate holds for path p; a zero Rule matches nothing.
func (r Rule) Matches(p string, f Facts) bool {
	return r.expr != nil && r.expr.eval(p, f)
}

// ParseLine parses one `<class> <predicate>` line (an optional trailing
// `# comment` is stripped); errors are prefixed with origin.
func ParseLine(line, origin string) (Rule, error) {
	line = strings.TrimSpace(stripComment(line))
	if line == "" {
		return Rule{}, fmt.Errorf("%s: empty rule", origin)
	}
	word, rest := line, ""
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		word, rest = line[:i], line[i+1:]
	}
	class, err := ParseClass(word)
	if err != nil {
		return Rule{}, fmt.Errorf("%s: %w", origin, err)
	}
	text := strings.TrimSpace(rest)
	if text == "" {
		return Rule{}, fmt.Errorf("%s: missing predicate after %q", origin, word)
	}
	expr, err := parseExpr(text)
	if err != nil {
		return Rule{}, fmt.Errorf("%s: %w", origin, err)
	}
	return Rule{Class: class, Text: text, Origin: origin, expr: expr}, nil
}

// stripComment drops a `#` comment that starts the line or follows whitespace,
// so globs like `a#b` keep their `#`.
func stripComment(line string) string {
	for i := range len(line) {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

// Parse parses a block of lines; blank and `#` lines are skipped; each rule's
// Origin is origin:firstLine+i where i is the 0-based line index.
func Parse(text, origin string, firstLine int) ([]Rule, error) {
	var out []Rule
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		r, err := ParseLine(line, fmt.Sprintf("%s:%d", origin, firstLine+i))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// builtinText is the default rule list every manifest ends with; line n is Origin "builtin:n".
// Python environments are hidden rather than cache: their scripts and editable
// installs name the folder by absolute path, so a live one would run the
// folder's interpreter and code from inside a checkout.
const builtinText = `hidden   .DS_Store | ._* | Thumbs.db | desktop.ini
local    secret
hidden   !tracked & (venvdir | dir & (.venv | .tox | .nox))
cache    cachedir | dir & (node_modules | __pycache__ | .next | .nuxt | .turbo | .gradle | .pytest_cache | .mypy_cache | .ruff_cache)
cache    ignored & dir
local    ignored
local    size > 100MB
source   *`

// builtins parses builtinText once; a failure is a programming error caught by tests.
var builtins = sync.OnceValue(func() []Rule {
	rs, err := Parse(builtinText, "builtin", 1)
	if err != nil {
		panic(fmt.Sprintf("rules: invalid builtins: %v", err))
	}
	return rs
})

// Builtins returns a fresh copy of the default rules, Origin "builtin:<n>" with n 1-based.
func Builtins() []Rule {
	return slices.Clone(builtins())
}

// Set is an ordered rule list; the first matching rule wins.
type Set struct{ Rules []Rule }

// Match returns the first rule matching p.
func (s Set) Match(p string, f Facts) (Rule, bool) {
	for _, r := range s.Rules {
		if r.Matches(p, f) {
			return r, true
		}
	}
	return Rule{}, false
}

// Decision is a path's class and the rule that decided it; At is the prefix
// the rule matched (== path unless an ancestor decided).
type Decision struct {
	Class Class
	Rule  Rule
	At    string
}

// Classify walks p's prefixes top-down ("a", "a/b", "a/b/c"); the first prefix
// whose class is not Source decides p and its whole subtree. If every prefix is
// Source, the decision for p itself is returned. facts(prefix) supplies Facts
// for each prefix. A Set must end with a catch-all; if nothing matches,
// Classify returns Source with a zero Rule.
func (s Set) Classify(p string, facts func(prefix string) Facts) Decision {
	for i := 0; ; {
		j := strings.IndexByte(p[i:], '/')
		prefix := p
		if j >= 0 {
			prefix = p[:i+j]
		}
		d := Decision{Class: Source, At: prefix}
		if r, ok := s.Match(prefix, facts(prefix)); ok {
			d.Class, d.Rule = r.Class, r
		}
		if d.Class != Source || j < 0 {
			return d
		}
		i += j + 1
	}
}
