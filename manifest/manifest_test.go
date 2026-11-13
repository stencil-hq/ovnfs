package manifest

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/rules"
)

// writeManifest writes dir/.o2/ovnfs.yml.
func writeManifest(t *testing.T, dir, body string) {
	t.Helper()
	p := filepath.Join(dir, ".o2", "ovnfs.yml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// file is a Facts for a plain untracked file.
type file struct{}

func (file) Dir() bool      { return false }
func (file) Size() int64    { return 1 }
func (file) Tracked() bool  { return false }
func (file) Ignored() bool  { return false }
func (file) CacheDir() bool { return false }
func (file) VenvDir() bool  { return false }

const projectYAML = `# project manifest
rules: |   # rule lines
  source  .env.local  # keep it

  local   data/**
  hidden  shared
cache:
  node_modules: { rebuild: bun install --frozen-lockfile, key: [bun.lock] }
  ./web/dist: { rebuild: make }
env:
  setup: [rustup show]
`

const userYAML = `rules: |
  hidden .env.local
  hidden shared
  local  user-only
cache:
  node_modules: { rebuild: npm ci }
  target: { rebuild: cargo build }
env:
  image: linux-dev
  setup: [old]
`

func TestLoadLayers(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	writeManifest(t, root, projectYAML)
	writeManifest(t, home, userYAML)
	m, err := Load(root, home, []string{"cache shared # flag wins"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Files, []string{".o2/ovnfs.yml", "~/.o2/ovnfs.yml"}) {
		t.Fatalf("Files = %v", m.Files)
	}
	var got []string
	for _, r := range m.Rules.Rules {
		got = append(got, r.Origin+" "+r.String())
	}
	want := []string{
		"flag cache shared",
		".o2/ovnfs.yml:3 source .env.local",
		".o2/ovnfs.yml:5 local data/**",
		".o2/ovnfs.yml:6 hidden shared",
		"~/.o2/ovnfs.yml:2 hidden .env.local",
		"~/.o2/ovnfs.yml:3 hidden shared",
		"~/.o2/ovnfs.yml:4 local user-only",
	}
	for _, b := range rules.Builtins() {
		want = append(want, b.Origin+" "+b.String())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// First match wins: flag > project > user > builtin.
	for _, tt := range []struct {
		path   string
		origin string
	}{
		{"shared", "flag"},
		{"a/.env.local", ".o2/ovnfs.yml:3"},
		{"user-only", "~/.o2/ovnfs.yml:4"},
		{".env", "builtin:2"},
	} {
		if r, _ := m.Rules.Match(tt.path, file{}); r.Origin != tt.origin {
			t.Errorf("%s matched %q, want %q", tt.path, r.Origin, tt.origin)
		}
	}

	if c := m.Cache["node_modules"]; c.Rebuild != "bun install --frozen-lockfile" || !slices.Equal(c.Key, []string{"bun.lock"}) {
		t.Errorf("project cache entry should win: %+v", c)
	}
	if m.Cache["target"].Rebuild != "cargo build" || m.Cache["web/dist"].Rebuild != "make" {
		t.Errorf("cache = %+v", m.Cache)
	}
	if m.Env.Image != "linux-dev" || !slices.Equal(m.Env.Setup, []string{"rustup show"}) {
		t.Errorf("env = %+v", m.Env)
	}
}

func TestLoadMissingFiles(t *testing.T) {
	m, err := Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 0 || len(m.Rules.Rules) != len(rules.Builtins()) || m.Cache != nil {
		t.Fatalf("got %+v", m)
	}
	root := t.TempDir()
	writeManifest(t, root, "env:\n  image: x\n")
	m, err = Load(root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Files, []string{".o2/ovnfs.yml"}) || m.Env.Image != "x" {
		t.Fatalf("got %+v", m)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"unknown top key", "rules: |\n  local a\nrulez: x\n", `.o2/ovnfs.yml:3: unknown key "rulez"`},
		{"unknown cache key", "cache:\n  node_modules:\n    rebuld: x\n", `.o2/ovnfs.yml:3: unknown key "cache.node_modules.rebuld"`},
		{"unknown env key", "env:\n  img: x\n", `.o2/ovnfs.yml:2: unknown key "env.img"`},
		{"bad class", "# c\nrules: |\n  local a\n  cahce b\n", `.o2/ovnfs.yml:4: unknown class "cahce"`},
		{"bad predicate", "rules: |\n\n  local a &\n", `.o2/ovnfs.yml:3: expected predicate after "&"`},
		{"rules not text", "rules:\n  - local a\n", `.o2/ovnfs.yml:2: rules must be a block of rule lines`},
		{"wrong type", "env:\n  setup: 3\n", `.o2/ovnfs.yml:2: cannot unmarshal`},
		{"syntax", "env: [\n", `.o2/ovnfs.yml:`},
		{"not a mapping", "- a\n", `.o2/ovnfs.yml:1: expected a mapping`},
		{"daemon setting in a folder", "rules: |\n  local a\nstate: /s\n", `.o2/ovnfs.yml:3: "state" is read only from ~/.o2/ovnfs.yml`},
	}
	for _, tt := range tests {
		root := t.TempDir()
		writeManifest(t, root, tt.body)
		_, err := Load(root, "", nil)
		if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want prefix %q", tt.name, err, tt.want)
		}
	}
	home := t.TempDir()
	writeManifest(t, home, "rules: |\n  locl a\n")
	if _, err := Load(t.TempDir(), home, nil); err == nil || !strings.HasPrefix(err.Error(), `~/.o2/ovnfs.yml:2: unknown class "locl"`) {
		t.Errorf("user file err = %v", err)
	}
	if _, err := Load(t.TempDir(), "", []string{"local a &"}); err == nil || err.Error() != `flag: expected predicate after "&"` {
		t.Errorf("flag err = %v", err)
	}
}

// TestUserDaemon reads state and mount only from the user file, expanding ~/,
// also when a folder's root is the home whose file it is.
func TestUserDaemon(t *testing.T) {
	home := t.TempDir()
	if d, err := UserDaemon(home); err != nil || d != (Daemon{}) {
		t.Fatalf("no file: %+v, %v", d, err)
	}
	mount := filepath.Join(t.TempDir(), "m")
	writeManifest(t, home, "state: ~/s\nmount: "+mount+"\nproto: nfs\nmemory: 0\nfsync: false\nblob_cache: 0\natime: false\nrules: |\n  local a\n")
	d, err := UserDaemon(home)
	want := Daemon{State: filepath.Join(home, "s"), Mount: mount, Proto: "nfs", Memory: "0", Fsync: "false", BlobCache: "0", Atime: "false"}
	if err != nil || d != want {
		t.Fatalf("UserDaemon = %+v, %v", d, err)
	}
	for _, root := range []string{t.TempDir(), home} {
		if _, err := Load(root, home, nil); err != nil {
			t.Fatalf("Load(%s) with a user file setting the daemon: %v", root, err)
		}
	}
	writeManifest(t, home, "mount: checkouts\n")
	if _, err := UserDaemon(home); err == nil || err.Error() != "~/.o2/ovnfs.yml:1: mount must be an absolute path or start with ~/" {
		t.Fatalf("relative mount err = %v", err)
	}
	// A bare drive letter names a Windows drive's root; elsewhere it is relative.
	// YAML needs it quoted: an unquoted trailing colon starts a mapping.
	writeManifest(t, home, "mount: 'O:'\n")
	d, err = UserDaemon(home)
	if runtime.GOOS == "windows" && (err != nil || d.Mount != `O:\`) || runtime.GOOS != "windows" && err == nil {
		t.Fatalf("drive mount = %+v, %v", d, err)
	}
}

func TestDaemonSettingsUserOnlyAndTypes(t *testing.T) {
	for _, key := range []string{"proto", "memory", "fsync", "blob_cache", "node_cache", "atime"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			writeManifest(t, root, key+": value\n")
			_, err := Load(root, "", nil)
			want := `.o2/ovnfs.yml:1: "` + key + `" is read only from ~/.o2/ovnfs.yml`
			if err == nil || err.Error() != want {
				t.Fatalf("project setting: %v, want %s", err, want)
			}
		})
	}
	for _, body := range []string{
		"proto: false\n", "memory: false\n", "blob_cache: []\n",
		"fsync: 0\n", "atime: {}\n",
	} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			writeManifest(t, home, body)
			if _, err := UserDaemon(home); err == nil {
				t.Fatalf("accepted invalid value: %s", body)
			}
			if _, err := Load(t.TempDir(), home, nil); err == nil {
				t.Fatalf("Load accepted invalid value: %s", body)
			}
		})
	}
	home := t.TempDir()
	writeManifest(t, home, "proto: fuse\nmemory: 1G\nfsync: true\nblob_cache: 512M\natime: true\n")
	want := Daemon{Proto: "fuse", Memory: "1G", Fsync: "true", BlobCache: "512M", Atime: "true"}
	if got, err := UserDaemon(home); err != nil || got != want {
		t.Fatalf("UserDaemon = %+v, %v; want %+v", got, err, want)
	}
}

func TestLockRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, projectYAML)
	m, err := Load(root, "", []string{"cache x"})
	if err != nil {
		t.Fatal(err)
	}
	l := m.Lock()
	s, err := l.Set()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Rules) != len(m.Rules.Rules) {
		t.Fatalf("len %d != %d", len(s.Rules), len(m.Rules.Rules))
	}
	for i, r := range s.Rules {
		if r.String() != m.Rules.Rules[i].String() || r.Origin != m.Rules.Rules[i].Origin {
			t.Errorf("rule %d: %v (%s) != %v", i, r, r.Origin, m.Rules.Rules[i])
		}
	}
	if r, _ := s.Match("data/f", file{}); r.Origin != ".o2/ovnfs.yml:5" {
		t.Errorf("re-parsed set matched %q", r.Origin)
	}

	h := l.Hash()
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 || m.Lock().Hash() != h {
		t.Fatalf("hash %q not stable", h)
	}
	l2 := m.Lock()
	l2.Env.Image = "other"
	if l2.Hash() == h {
		t.Fatal("hash ignores env")
	}
	l.Rules[0].Text = "local &"
	if _, err := l.Set(); err == nil || !strings.Contains(err.Error(), "flag:") {
		t.Fatalf("bad lock rule err = %v", err)
	}
}

func TestCacheFor(t *testing.T) {
	l := Lock{Cache: map[string]Cache{
		"node_modules":     {Rebuild: "generic"},
		"web/node_modules": {Rebuild: "web"},
	}}
	for _, tt := range []struct {
		root, want string
		ok         bool
	}{
		{"web/node_modules", "web", true},
		{"api/node_modules", "generic", true},
		{"node_modules", "generic", true},
		{"target", "", false},
	} {
		c, ok := l.CacheFor(tt.root)
		if ok != tt.ok || c.Rebuild != tt.want {
			t.Errorf("CacheFor(%q) = %+v, %v", tt.root, c, ok)
		}
	}
}

func TestKeyHash(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("web/bun.lock", "v1")
	write("root.lock", "r")

	h1, err := KeyHash(dir, "web/node_modules", []string{"bun.lock", "missing.lock"})
	if err != nil || !strings.HasPrefix(h1, "sha256:") {
		t.Fatalf("%q, %v", h1, err)
	}
	if h, _ := KeyHash(dir, "web/node_modules", []string{"missing.lock", "bun.lock"}); h != h1 {
		t.Error("key order changes hash")
	}
	write("web/bun.lock", "v2")
	h2, _ := KeyHash(dir, "web/node_modules", []string{"bun.lock", "missing.lock"})
	if h2 == h1 {
		t.Error("content change not reflected")
	}
	write("web/missing.lock", "")
	if h3, _ := KeyHash(dir, "web/node_modules", []string{"bun.lock", "missing.lock"}); h3 == h2 {
		t.Error("missing vs empty file not distinguished")
	}
	if _, err := KeyHash(dir, "web/node_modules", []string{"../root.lock"}); err != nil {
		t.Errorf("parent key: %v", err)
	}
	if _, err := KeyHash(dir, "node_modules", []string{"../x"}); err == nil {
		t.Error("key escaping the source accepted")
	}
	if _, err := KeyHash(dir, "web/node_modules", []string{"."}); err == nil {
		t.Error("directory key accepted")
	}
}
