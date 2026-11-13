package rules

import (
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
)

// node is a compiled predicate; eval short-circuits so Facts are computed only when needed.
type node interface {
	eval(p string, f Facts) bool
}

// orNode holds when either side holds.
type orNode struct{ l, r node }

// eval evaluates r only when l is false.
func (n orNode) eval(p string, f Facts) bool { return n.l.eval(p, f) || n.r.eval(p, f) }

// andNode holds when both sides hold.
type andNode struct{ l, r node }

// eval evaluates r only when l is true.
func (n andNode) eval(p string, f Facts) bool { return n.l.eval(p, f) && n.r.eval(p, f) }

// notNode negates its operand.
type notNode struct{ x node }

// eval inverts the operand.
func (n notNode) eval(p string, f Facts) bool { return !n.x.eval(p, f) }

// factNode is a keyword answered directly by Facts.
type factNode string

// Keywords answered by Facts.
const (
	kwIgnored   factNode = "ignored"
	kwTracked   factNode = "tracked"
	kwUntracked factNode = "untracked"
	kwCacheDir  factNode = "cachedir"
	kwVenvDir   factNode = "venvdir"
	kwDir       factNode = "dir"
)

// eval asks Facts the keyword's question; untracked asks Ignored only for untracked paths.
func (n factNode) eval(_ string, f Facts) bool {
	switch n {
	case kwIgnored:
		return f.Ignored()
	case kwTracked:
		return f.Tracked()
	case kwUntracked:
		return !f.Tracked() && !f.Ignored()
	case kwCacheDir:
		return f.CacheDir()
	case kwVenvDir:
		return f.VenvDir()
	case kwDir:
		return f.Dir()
	}
	return false
}

// keywords maps keyword words to their nodes; any other word is a glob.
var keywords = map[string]node{
	"ignored":   kwIgnored,
	"tracked":   kwTracked,
	"untracked": kwUntracked,
	"cachedir":  kwCacheDir,
	"venvdir":   kwVenvDir,
	"dir":       kwDir,
	"secret":    secretNode{},
}

// sizeNode compares a regular file's size with a threshold; it is false for directories.
type sizeNode struct {
	op string
	n  int64
}

// eval compares the size first and asks Dir only when the comparison holds.
func (n sizeNode) eval(_ string, f Facts) bool {
	s := f.Size()
	var ok bool
	switch n.op {
	case ">":
		ok = s > n.n
	case ">=":
		ok = s >= n.n
	case "<":
		ok = s < n.n
	case "<=":
		ok = s <= n.n
	}
	return ok && !f.Dir()
}

// sizeUnits maps lower-cased unit names to byte multipliers.
var sizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"kb":  1e3,
	"mb":  1e6,
	"gb":  1e9,
	"tb":  1e12,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
}

// globNode matches a path with gitignore glob syntax relative to the source root.
type globNode struct {
	segs     []string // path.Match patterns; "**" spans zero or more components
	anchored bool     // pattern had a leading or middle slash
	dirOnly  bool     // pattern had a trailing slash
}

// eval matches the path itself (not its descendants) and asks Dir only for dir-only patterns.
func (g globNode) eval(p string, f Facts) bool {
	if p == "" {
		return false
	}
	var ok bool
	if g.anchored {
		ok = matchSegs(g.segs, strings.Split(p, "/"))
	} else {
		ok, _ = path.Match(g.segs[0], path.Base(p))
	}
	return ok && (!g.dirOnly || f.Dir())
}

// matchSegs matches path components against pattern segments; a trailing "**"
// needs at least one component, any other "**" matches zero or more.
func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if len(pat) == 1 {
			return len(name) > 0
		}
		for k := range len(name) + 1 {
			if matchSegs(pat[1:], name[k:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, _ := path.Match(pat[0], name[0])
	return ok && matchSegs(pat[1:], name[1:])
}

// compileGlob turns a gitignore-style pattern into a globNode, rejecting malformed patterns.
func compileGlob(w string) (node, error) {
	g := globNode{}
	p := w
	if strings.HasSuffix(p, "/") {
		g.dirOnly = true
		p = p[:len(p)-1]
	}
	if strings.Contains(p, "/") {
		g.anchored = true
		p = strings.TrimPrefix(p, "/")
	}
	if p == "" {
		return nil, fmt.Errorf("invalid glob %q", w)
	}
	for s := range strings.SplitSeq(p, "/") {
		if s == "" {
			return nil, fmt.Errorf("invalid glob %q: empty path component", w)
		}
		if s != "**" {
			s = bangClass(s)
			if _, err := path.Match(s, ""); err != nil {
				return nil, fmt.Errorf("invalid glob %q: %w", w, err)
			}
		}
		g.segs = append(g.segs, s)
	}
	return g, nil
}

// bangClass rewrites gitignore's `[!...]` class negation into path.Match's `[^...]`.
func bangClass(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\':
			i++
		case b[i] == '[' && i+1 < len(b) && b[i+1] == '!':
			b[i+1] = '^'
		}
	}
	return string(b)
}

// tokKind distinguishes operators, words and size comparisons.
type tokKind uint8

// Token kinds produced by lex.
const (
	tokOp tokKind = iota + 1
	tokWord
	tokSize
)

// token is one lexeme of a predicate; size tokens carry their compiled node.
type token struct {
	kind tokKind
	text string
	size sizeNode
}

// isSpace reports whether c separates tokens.
func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// isOp reports whether c is a single-character operator.
func isOp(c byte) bool { return strings.IndexByte("|&()!", c) >= 0 }

// isWordEnd reports whether c ends a word; `!` inside a word is literal.
func isWordEnd(c byte) bool { return isSpace(c) || strings.IndexByte("|&()", c) >= 0 }

// lex splits a predicate into tokens; `!` is an operator only at a token's start.
func lex(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case isSpace(c):
			i++
		case isOp(c):
			toks = append(toks, token{kind: tokOp, text: s[i : i+1]})
			i++
		default:
			j := i
			for j < len(s) && !isWordEnd(s[j]) {
				j++
			}
			if s[i:j] == "size" || strings.HasPrefix(s[i:j], "size<") || strings.HasPrefix(s[i:j], "size>") || strings.HasPrefix(s[i:j], "size=") {
				t, n, err := lexSize(s[i:])
				if err != nil {
					return nil, err
				}
				toks = append(toks, t)
				i += n
				continue
			}
			toks = append(toks, token{kind: tokWord, text: s[i:j]})
			i = j
		}
	}
	return toks, nil
}

// lexSize lexes `size OP N[unit]` at the start of s with optional spaces and
// returns the token and the number of bytes consumed.
func lexSize(s string) (token, int, error) {
	i := len("size")
	skip := func() {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
	}
	skip()
	var op string
	switch {
	case strings.HasPrefix(s[i:], ">="), strings.HasPrefix(s[i:], "<="):
		op = s[i : i+2]
	case strings.HasPrefix(s[i:], ">"), strings.HasPrefix(s[i:], "<"):
		op = s[i : i+1]
	default:
		return token{}, 0, fmt.Errorf(`expected one of > >= < <= after "size"`)
	}
	i += len(op)
	skip()
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if start == i {
		return token{}, 0, fmt.Errorf("expected number after %q", "size "+op)
	}
	num, err := strconv.ParseInt(s[start:i], 10, 64)
	if err != nil {
		return token{}, 0, fmt.Errorf("invalid size %q: %w", s[start:i], err)
	}
	// The unit may follow the number directly or after spaces; a spaced word
	// that is not a unit belongs to the rest of the expression.
	unitAt := i
	skip()
	j := i
	for j < len(s) && !isWordEnd(s[j]) {
		j++
	}
	unit := s[i:j]
	mult, known := sizeUnits[strings.ToLower(unit)]
	switch {
	case unitAt == i && !known:
		return token{}, 0, fmt.Errorf("unknown size unit %q", unit)
	case !known:
		i, unit, mult = unitAt, "", 1
	default:
		i = j
	}
	if num > math.MaxInt64/mult {
		return token{}, 0, fmt.Errorf("size %d%s is too large", num, unit)
	}
	text := strings.TrimSpace(s[:i])
	return token{kind: tokSize, text: text, size: sizeNode{op: op, n: num * mult}}, i, nil
}

// parser is a recursive-descent parser over lexed tokens.
type parser struct {
	toks []token
	pos  int
}

// parseExpr compiles predicate text into a node.
func parseExpr(text string) (node, error) {
	toks, err := lex(text)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].text)
	}
	return n, nil
}

// peekOp reports whether the next token is operator op.
func (p *parser) peekOp(op string) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == tokOp && p.toks[p.pos].text == op
}

// or parses `and ('|' and)*`.
func (p *parser) or() (node, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peekOp("|") {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = orNode{l, r}
	}
	return l, nil
}

// and parses `not ('&' not)*`.
func (p *parser) and() (node, error) {
	l, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.peekOp("&") {
		p.pos++
		r, err := p.not()
		if err != nil {
			return nil, err
		}
		l = andNode{l, r}
	}
	return l, nil
}

// not parses `'!' not | atom`.
func (p *parser) not() (node, error) {
	if p.peekOp("!") {
		p.pos++
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return notNode{x}, nil
	}
	return p.atom()
}

// atom parses a parenthesized expression, keyword, size comparison or glob.
func (p *parser) atom() (node, error) {
	if p.pos >= len(p.toks) {
		return nil, p.expected()
	}
	t := p.toks[p.pos]
	switch {
	case t.kind == tokSize:
		p.pos++
		return t.size, nil
	case t.kind == tokWord:
		p.pos++
		if n, ok := keywords[t.text]; ok {
			return n, nil
		}
		return compileGlob(t.text)
	case t.text == "(":
		p.pos++
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.toks) {
			return nil, fmt.Errorf(`missing ")"`)
		}
		if !p.peekOp(")") {
			return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].text)
		}
		p.pos++
		return n, nil
	}
	return nil, p.expected()
}

// expected reports a missing predicate, naming the token before it.
func (p *parser) expected() error {
	switch {
	case len(p.toks) == 0:
		return fmt.Errorf("missing predicate")
	case p.pos == 0:
		return fmt.Errorf("expected predicate, found %q", p.toks[0].text)
	}
	return fmt.Errorf("expected predicate after %q", p.toks[p.pos-1].text)
}
