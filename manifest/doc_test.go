package manifest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// readDoc reads d, failing the test on an unreadable file.
func readDoc(t *testing.T, d *Doc) *Doc {
	t.Helper()
	if err := d.Read(); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestResolveMergesLikeLoad checks that a key's merged values are the ones
// Load gives a checkout: rules concatenate, a folder cache entry replaces the
// user's whole, and an empty image or an empty setup list decide differently.
func TestResolveMergesLikeLoad(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	writeManifest(t, root, `rules: |
  source .env.local

  local  data/**
cache:
  ./web/.next: { rebuild: bun run build }
env:
  image: ""
  setup: []
`)
	writeManifest(t, home, `rules: |
  local user-only
cache:
  web/.next: { rebuild: npm run build, key: [package.json] }
  target: { rebuild: cargo build }
env:
  image: linux-dev
  setup: [old]
proto: nfs
`)
	docs := Docs(root, home)
	for _, d := range docs {
		readDoc(t, d)
	}
	for _, tt := range []struct {
		key  string
		want []string // file and value
	}{
		{"rules", []string{".o2/ovnfs.yml source .env.local", ".o2/ovnfs.yml local  data/**", "~/.o2/ovnfs.yml local user-only"}},
		{"cache.web/.next.rebuild", []string{".o2/ovnfs.yml bun run build"}},
		{"cache.web/.next.key", nil}, // the folder's entry has no key and replaces the user's
		{"cache.target.rebuild", []string{"~/.o2/ovnfs.yml cargo build"}},
		{"env.image", []string{"~/.o2/ovnfs.yml linux-dev"}}, // an empty image is unset
		{"env.setup", nil}, // an empty list is set
		{"proto", []string{"~/.o2/ovnfs.yml nfs"}},
	} {
		settings, err := Resolve(tt.key, docs...)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", tt.key, err)
		}
		var got []string
		for _, s := range settings {
			got = append(got, s.File+" "+s.Value)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("Resolve(%s) = %q, want %q", tt.key, got, tt.want)
		}
	}
	m, err := Load(root, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Env.Image != "linux-dev" || m.Env.Setup == nil || len(m.Env.Setup) != 0 || m.Cache["web/.next"].Key != nil {
		t.Fatalf("Load disagrees: env %+v, cache %+v", m.Env, m.Cache)
	}
	if _, err := Resolve("env.imge", docs...); err == nil || err.Error() != `unknown key "env.imge"` {
		t.Fatalf("unknown key: %v", err)
	}
}

// TestDocEditKeepsComments edits a commented file with flow mappings and a
// cache key spelled ./web/dist, then loads the result.
func TestDocEditKeepsComments(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, projectYAML)
	d := readDoc(t, FolderDoc(root, ""))
	for _, err := range []error{
		d.Append("rules", "hidden *.tmp"),
		d.Append("cache.node_modules.key", "package.json"),
		d.Set("cache.web/dist.key", "Makefile"),
		d.Append("env.setup", "make deps"),
		d.Set("env.image", "true"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	m, err := Load(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var rs []string
	for _, r := range m.Rules.Rules[:4] {
		rs = append(rs, r.String())
	}
	if !slices.Equal(rs, []string{"source .env.local", "local data/**", "hidden shared", "hidden *.tmp"}) {
		t.Errorf("rules = %q", rs)
	}
	if c := m.Cache["node_modules"]; !slices.Equal(c.Key, []string{"bun.lock", "package.json"}) || c.Rebuild == "" {
		t.Errorf("node_modules = %+v", c)
	}
	if c := m.Cache["web/dist"]; c.Rebuild != "make" || !slices.Equal(c.Key, []string{"Makefile"}) || len(m.Cache) != 2 {
		t.Errorf("cache = %+v", m.Cache)
	}
	if m.Env.Image != "true" || !slices.Equal(m.Env.Setup, []string{"rustup show", "make deps"}) {
		t.Errorf("env = %+v", m.Env)
	}
	data, err := os.ReadFile(d.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, comment := range []string{"# project manifest", "# rule lines", "# keep it"} {
		if !strings.Contains(string(data), comment) {
			t.Errorf("lost %q:\n%s", comment, data)
		}
	}
}

// TestDocSaveRefuses checks that no edit writes a file that would not load,
// and that values YAML would read as another type still save as strings.
func TestDocSaveRefuses(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	writeManifest(t, root, "rules: |\n  local a\n")
	for _, set := range []struct{ key, value, err string }{
		{"rules", "bogus x", `.o2/ovnfs.yml:2: unknown class "bogus"`},
		{"proto", "smb", `.o2/ovnfs.yml:3: "proto" is read only from ~/.o2/ovnfs.yml`},
		{"nfs_io_size", "64KiB", `.o2/ovnfs.yml:3: "nfs_io_size" is read only from ~/.o2/ovnfs.yml`},
	} {
		d := readDoc(t, FolderDoc(root, home))
		if err := d.Set(set.key, set.value); err != nil {
			t.Fatal(err)
		}
		if err := d.Save(); err == nil || err.Error() != set.err {
			t.Errorf("Save after %s = %v, want %s", set.key, err, set.err)
		}
	}
	if data, _ := os.ReadFile(FolderDoc(root, home).Path); string(data) != "rules: |\n  local a\n" {
		t.Fatalf("refused save wrote %q", data)
	}

	d := readDoc(t, UserDoc(home))
	for _, err := range []error{d.Set("memory", "1e3"), d.Set("fsync", "true"), d.Set("proto", "false"), d.Set("nfs_io_size", "64KiB")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	if got, err := UserDaemon(home); err != nil || got != (Daemon{Memory: "1e3", Fsync: "true", Proto: "false", NFSIOSize: "64KiB"}) {
		t.Fatalf("UserDaemon = %+v, %v", got, err)
	}
	if err := d.Set("env.image", "a", "b"); err == nil {
		t.Error("Set took two values for a scalar")
	}
	if err := d.Append("proto", "x"); err == nil {
		t.Error("Append extended a scalar")
	}
	if err := d.Set("cache.target.build", "x"); err == nil {
		t.Error("Set took an unknown cache field")
	}
}

// TestDocUnset checks that unsetting removes the sections it empties and
// reports a key the file does not have.
func TestDocUnset(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "cache:\n  target: { rebuild: cargo build }\n  ./web/dist: { rebuild: make }\nenv:\n  image: dev\n")
	d := readDoc(t, FolderDoc(root, ""))
	for _, key := range []string{"cache.target.rebuild", "cache.web/dist.rebuild", "env.image"} {
		if found, err := d.Unset(key); err != nil || !found {
			t.Fatalf("Unset(%s) = %v, %v", key, found, err)
		}
	}
	if found, err := d.Unset("env.image"); err != nil || found {
		t.Fatalf("Unset of a missing key = %v, %v", found, err)
	}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(d.Path); err != nil || len(data) != 0 {
		t.Fatalf("emptied file = %q, %v", data, err)
	}
}

// TestLoadHomeThroughSymlink reads a home folder's manifest once, as the
// user's, when $HOME reaches it through a symlink.
func TestLoadHomeThroughSymlink(t *testing.T) {
	home := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Skip(err)
	}
	writeManifest(t, home, "proto: nfs\nrules: |\n  local a\n")
	m, err := Load(home, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Files, []string{File}) {
		t.Fatalf("Files = %q", m.Files)
	}
}
