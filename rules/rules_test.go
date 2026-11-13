package rules

import (
	"strings"
	"testing"
)

// facts is a static Facts for tests; asked records which questions were evaluated.
type facts struct {
	dir, tracked, ignored, cachedir, venvdir bool
	size                                     int64
	asked                                    map[string]bool
}

func (f *facts) note(q string) {
	if f.asked != nil {
		f.asked[q] = true
	}
}
func (f *facts) Dir() bool      { f.note("dir"); return f.dir }
func (f *facts) Size() int64    { f.note("size"); return f.size }
func (f *facts) Tracked() bool  { f.note("tracked"); return f.tracked }
func (f *facts) Ignored() bool  { f.note("ignored"); return f.ignored }
func (f *facts) CacheDir() bool { f.note("cachedir"); return f.cachedir }
func (f *facts) VenvDir() bool  { f.note("venvdir"); return f.venvdir }

func mustRule(t *testing.T, line string) Rule {
	t.Helper()
	r, err := ParseLine(line, "test")
	if err != nil {
		t.Fatalf("ParseLine(%q): %v", line, err)
	}
	return r
}

func TestPredicates(t *testing.T) {
	file := &facts{}
	dir := &facts{dir: true}
	tests := []struct {
		pred string
		path string
		f    *facts
		want bool
	}{
		// precedence: ! > & > |
		{"tracked | ignored & dir", "a", &facts{tracked: true}, true},
		{"(tracked | ignored) & dir", "a", &facts{tracked: true}, false},
		{"!tracked & ignored", "a", &facts{ignored: true}, true},
		{"!tracked & ignored", "a", &facts{tracked: true, ignored: true}, false},
		{"!(tracked & ignored)", "a", &facts{tracked: true}, true},
		{"!!dir", "a", dir, true},
		{"!dir|dir&!dir", "a", dir, false},
		{"untracked", "a", file, true},
		{"untracked", "a", &facts{ignored: true}, false},
		{"untracked", "a", &facts{tracked: true}, false},
		{"cachedir", "a", &facts{dir: true, cachedir: true}, true},
		{"venvdir", "a", &facts{dir: true, venvdir: true}, true},
		{"secret", "x/.env", file, true},
		{"secret", "x/.env", dir, false},
		{"secret", "x/.ssh", dir, true},
		{"secret", "x/.ssh", file, false},
		// size units and boundaries
		{"size > 100MB", "a", &facts{size: 100_000_000}, false},
		{"size > 100MB", "a", &facts{size: 100_000_001}, true},
		{"size>=100MB", "a", &facts{size: 100_000_000}, true},
		{"size < 1KiB", "a", &facts{size: 1023}, true},
		{"size < 1KiB", "a", &facts{size: 1024}, false},
		{"size <= 1KiB", "a", &facts{size: 1024}, true},
		{"size>1kb", "a", &facts{size: 1001}, true},
		{"size > 1 GiB", "a", &facts{size: 1 << 30}, false},
		{"size >= 1 GiB", "a", &facts{size: 1 << 30}, true},
		{"size > 2TB", "a", &facts{size: 2e12 + 1}, true},
		{"size > 1TiB", "a", &facts{size: 1 << 40}, false},
		{"size >= 10", "a", &facts{size: 10}, true},
		{"size >= 10B", "a", &facts{size: 9}, false},
		{"size < 10", "a", dir, false}, // false for directories
		{"size > 1 & tracked", "a", &facts{size: 2, tracked: true}, true},
		// globs without slash match the basename at any depth
		{"node_modules", "node_modules", dir, true},
		{"node_modules", "a/b/node_modules", dir, true},
		{"node_modules", "node_modules/x", file, false},
		{"*.log", "a/b.log", file, true},
		{"*.log", "a/b.log/c", file, false},
		{".env.local", "deep/.env.local", file, true},
		{"*", "a/b/c", file, true},
		{"[!a]*", "b", file, true},
		{"[!a]*", "a", file, false},
		// leading or middle slash anchors
		{"/dist", "dist", dir, true},
		{"/dist", "a/dist", dir, false},
		{"a/dist", "a/dist", dir, true},
		{"a/dist", "b/a/dist", dir, false},
		// trailing slash: directories only
		{"build/", "x/build", dir, true},
		{"build/", "x/build", file, false},
		{"/out/", "out", dir, true},
		{"/out/", "out", file, false},
		// ** per gitignore
		{"data/**", "data", dir, false},
		{"data/**", "data/x", file, true},
		{"data/**", "data/x/y", file, true},
		{"**/foo", "foo", file, true},
		{"**/foo", "a/b/foo", file, true},
		{"a/**/b", "a/b", file, true},
		{"a/**/b", "a/x/y/b", file, true},
		{"a/**/b", "a/x/y/c", file, false},
		{"/**", "x", file, true},
		// keyword combined with glob
		{"dir & (node_modules | .venv)", "a/.venv", dir, true},
		{"dir & (node_modules | .venv)", "a/.venv", file, false},
	}
	for _, tt := range tests {
		r := mustRule(t, "local "+tt.pred)
		if got := r.Matches(tt.path, tt.f); got != tt.want {
			t.Errorf("%q on %q = %v, want %v", tt.pred, tt.path, got, tt.want)
		}
	}
}

func TestLazyEvaluation(t *testing.T) {
	tests := []struct {
		pred      string
		path      string
		f         facts
		forbidden []string
	}{
		{"tracked | ignored", "a", facts{tracked: true}, []string{"ignored"}},
		{"ignored & dir", "a", facts{}, []string{"dir"}},
		{"*.log", "a.txt", facts{}, []string{"dir", "size", "tracked", "ignored", "cachedir"}},
		{"build/", "src", facts{}, []string{"dir"}},
		{"secret", "main.go", facts{}, []string{"dir"}},
		{"size > 10", "a", facts{size: 1}, []string{"dir"}},
		{"untracked", "a", facts{tracked: true}, []string{"ignored"}},
	}
	for _, tt := range tests {
		f := tt.f
		f.asked = map[string]bool{}
		mustRule(t, "local "+tt.pred).Matches(tt.path, &f)
		for _, q := range tt.forbidden {
			if f.asked[q] {
				t.Errorf("%q on %q asked %s", tt.pred, tt.path, q)
			}
		}
	}
}

func TestParseLine(t *testing.T) {
	r := mustRule(t, "  cache\t  dir & (a | b)   # why")
	if r.Class != Cache || r.Text != "dir & (a | b)" || r.Origin != "test" {
		t.Fatalf("got %+v", r)
	}
	if r.String() != "cache dir & (a | b)" {
		t.Fatalf("String = %q", r.String())
	}
	if r := mustRule(t, "source a#b"); r.Text != "a#b" {
		t.Fatalf("glob # stripped: %q", r.Text)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		line, want string
	}{
		{"cahce foo", `flag: unknown class "cahce"`},
		{"local", `flag: missing predicate after "local"`},
		{"local a &", `flag: expected predicate after "&"`},
		{"local a | | b", `flag: expected predicate after "|"`},
		{"local !", `flag: expected predicate after "!"`},
		{"local ) a", `flag: expected predicate, found ")"`},
		{"local (a | b", `flag: missing ")"`},
		{"local (a b)", `flag: unexpected "b"`},
		{"local a b", `flag: unexpected "b"`},
		{"local a)", `flag: unexpected ")"`},
		{"local size > 1 tracked", `flag: unexpected "tracked"`},
		{"local size", `flag: expected one of > >= < <= after "size"`},
		{"local size = 3", `flag: expected one of > >= < <= after "size"`},
		{"local size > MB", `flag: expected number after "size >"`},
		{"local size > 3XB", `flag: unknown size unit "XB"`},
		{"local size > 99999999999TiB", `flag: size 99999999999TiB is too large`},
		{"local a[", `flag: invalid glob "a["`},
		{"local a//b", `flag: invalid glob "a//b": empty path component`},
		{"local /", `flag: invalid glob "/"`},
		{"# only comment", `flag: empty rule`},
	}
	for _, tt := range tests {
		_, err := ParseLine(tt.line, "flag")
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("ParseLine(%q) error = %v, want prefix %q", tt.line, err, tt.want)
		}
	}
}

func TestParseOrigins(t *testing.T) {
	rs, err := Parse("\n# c\nsource a\n\n  local b # x\n", ".o2/ovnfs.yml", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Origin != ".o2/ovnfs.yml:6" || rs[1].Origin != ".o2/ovnfs.yml:8" {
		t.Fatalf("got %v", rs)
	}
	_, err = Parse("source a\nlocal a &\n", ".o2/ovnfs.yml", 7)
	if err == nil || err.Error() != `.o2/ovnfs.yml:8: expected predicate after "&"` {
		t.Fatalf("err = %v", err)
	}
}

func TestBuiltins(t *testing.T) {
	bs := Builtins()
	if len(bs) != 8 || bs[0].Origin != "builtin:1" || bs[7].Origin != "builtin:8" || bs[7].String() != "source *" {
		t.Fatalf("builtins = %v", bs)
	}
	set := Set{Rules: bs}
	tests := []struct {
		path   string
		f      facts
		class  Class
		origin string
	}{
		{".DS_Store", facts{}, Hidden, "builtin:1"},
		{"a/._foo", facts{}, Hidden, "builtin:1"},
		{".env", facts{tracked: true}, Local, "builtin:2"},
		{".env.example", facts{tracked: true}, Source, "builtin:8"},
		// Python environments: any name when it holds pyvenv.cfg, even ignored;
		// tool directories of environments by name; never tracked content.
		{".venv", facts{dir: true, venvdir: true}, Hidden, "builtin:3"},
		{"env", facts{dir: true, ignored: true, venvdir: true}, Hidden, "builtin:3"},
		{".tox", facts{dir: true, ignored: true}, Hidden, "builtin:3"},
		{"env", facts{dir: true, tracked: true, venvdir: true}, Source, "builtin:8"},
		{"node_modules", facts{dir: true}, Cache, "builtin:4"},
		{"node_modules", facts{}, Source, "builtin:8"},
		{"x", facts{dir: true, cachedir: true}, Cache, "builtin:4"},
		{"build", facts{dir: true, ignored: true}, Cache, "builtin:5"},
		{"app.log", facts{ignored: true}, Local, "builtin:6"},
		{"big.bin", facts{size: 200_000_000}, Local, "builtin:7"},
		{"src/a.go", facts{tracked: true}, Source, "builtin:8"},
	}
	for _, tt := range tests {
		f := tt.f
		d := set.Classify(tt.path, func(string) Facts { return &f })
		if d.Class != tt.class || d.Rule.Origin != tt.origin {
			t.Errorf("%s: got %s by %q, want %s by %q", tt.path, d.Class, d.Rule.Origin, tt.class, tt.origin)
		}
	}
}

func TestFirstMatchWins(t *testing.T) {
	var rs []Rule
	rs = append(rs, mustRule(t, "source .env.local"))
	rs = append(rs, mustRule(t, "hidden .env*"))
	rs = append(rs, Builtins()...)
	set := Set{Rules: rs}
	f := &facts{}
	if r, _ := set.Match("a/.env.local", f); r.Class != Source {
		t.Fatalf(".env.local -> %v", r)
	}
	if r, _ := set.Match(".env", f); r.Class != Hidden {
		t.Fatalf(".env -> %v", r)
	}
	if _, ok := (Set{}).Match("a", f); ok {
		t.Fatal("empty set matched")
	}
}

func TestClassify(t *testing.T) {
	set := Set{Rules: []Rule{
		mustRule(t, "local data/**"),
		mustRule(t, "hidden secret"),
		mustRule(t, "cache dir & node_modules"),
		mustRule(t, "source *"),
	}}
	dirs := map[string]bool{"a": true, "a/node_modules": true, "a/node_modules/x": true, "data": true, "data/raw": true, "k": true, "k/.ssh": true}
	var asked []string
	factsOf := func(p string) Facts {
		asked = append(asked, p)
		return &facts{dir: dirs[p]}
	}
	tests := []struct {
		path    string
		class   Class
		at      string
		prefixs int
	}{
		{"a/node_modules/x/.env", Cache, "a/node_modules", 2},
		{"data", Source, "data", 1},
		{"data/raw/f.csv", Local, "data/raw", 2},
		{"k/.ssh/id_rsa.pub", Hidden, "k/.ssh", 2},
		{"a/b.go", Source, "a/b.go", 2},
	}
	for _, tt := range tests {
		asked = nil
		d := set.Classify(tt.path, factsOf)
		if d.Class != tt.class || d.At != tt.at || len(asked) != tt.prefixs {
			t.Errorf("%s: got %s at %q asking %v, want %s at %q", tt.path, d.Class, d.At, asked, tt.class, tt.at)
		}
	}
	d := (Set{Rules: []Rule{mustRule(t, "local x")}}).Classify("a/b", factsOf)
	if d.Class != Source || d.Rule.Origin != "" || d.At != "a/b" {
		t.Fatalf("no match: %+v", d)
	}
}

func TestIsSecret(t *testing.T) {
	tests := []struct {
		path string
		dir  bool
		want bool
	}{
		{".env", false, true},
		{"a/.env.production", false, true},
		{"a/.env.example", false, false},
		{".env.sample", false, false},
		{".env.template", false, false},
		{".env.dist", false, false},
		{".netrc", false, true},
		{"x/credentials.json", false, true},
		{"tls/server.pem", false, true},
		{"server.key", false, true},
		{"cert.p12", false, true},
		{"id_ed25519", false, true},
		{"id_ed25519.pub", false, false},
		{"id_rsa.pub", false, false},
		{".ENV", false, false},
		{"home/.ssh", true, true},
		{".aws", true, true},
		{".aws", false, false},
		{".env", true, false},
		{"main.go", false, false},
	}
	for _, tt := range tests {
		if got := IsSecret(tt.path, tt.dir); got != tt.want {
			t.Errorf("IsSecret(%q, %v) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}

func TestParseClass(t *testing.T) {
	for _, s := range []string{"source", "cache", "local", "hidden"} {
		if c, err := ParseClass(s); err != nil || string(c) != s {
			t.Errorf("ParseClass(%q) = %q, %v", s, c, err)
		}
	}
	if _, err := ParseClass("Source"); err == nil {
		t.Error("ParseClass is case-sensitive")
	}
}
