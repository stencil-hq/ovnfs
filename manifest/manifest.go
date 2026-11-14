// Package manifest resolves the rules, cache recipes and environment of a checkout
// from command-line overrides, the folder's `.o2/ovnfs.yml`, the user's
// `~/.o2/ovnfs.yml` and the builtin rules, and freezes the result as a Lock.
// The user's file alone may also set the daemon's folders (see [UserDaemon]).
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/stencil-hq/ovnfs/rules"
	"gopkg.in/yaml.v3"
)

// File is the optional per-folder manifest `.o2/ovnfs.yml`; the same schema
// applies to the user-wide `~/.o2/ovnfs.yml`, which may also set [Daemon].
const File = ".o2/ovnfs.yml"

// userFile is how the user-wide manifest is named in origins and Files.
const userFile = "~/" + File

// Cache describes how to rebuild a cache root elsewhere and which files key its contents.
type Cache struct {
	Rebuild string   `json:"rebuild,omitempty" yaml:"rebuild"`
	Key     []string `json:"key,omitempty" yaml:"key"`
}

// Env names the environment a checkout runs in and the commands that prepare it.
type Env struct {
	Image string   `json:"image,omitempty" yaml:"image"`
	Setup []string `json:"setup,omitempty" yaml:"setup"`
}

// Daemon contains daemon settings settable only in the user-wide manifest;
// an empty field is unset.
type Daemon struct {
	State      string
	Mount      string
	Proto      string
	Memory     string
	Fsync      string
	NodeCache  string
	BlobCache  string
	Atime      string
	NFSIOSize  string
	GitRemotes string
}

// Manifest is the resolved description of a checkout for one source root.
type Manifest struct {
	Rules rules.Set        // overrides, then project, then user, then builtins
	Cache map[string]Cache // project entries override user entries by key
	Env   Env              // project fields override user fields
	Files []string         // manifest files read, highest precedence first (display form)
}

// daemonKeys are the settings only the user-wide manifest may set, each with
// the YAML type it accepts besides a string or null ("" for none).
var daemonKeys = map[string]string{
	"state": "", "mount": "", "proto": "",
	"memory": "!!int", "fsync": "!!bool", "node_cache": "!!int", "blob_cache": "!!int", "atime": "!!bool",
	"git_remotes": "!!bool",
	"nfs_io_size": "!!int",
}

// UserOnly reports whether key is a daemon setting, which only ~/.o2/ovnfs.yml
// may set; `ovnfs config` writes such keys there unless told otherwise.
func UserOnly(key string) bool {
	_, ok := daemonKeys[key]
	return ok
}

// cacheRoot cleans a cache key as written ("./web/dist") into the root path it
// names ("web/dist"); ok is false for a key naming no path.
func cacheRoot(k string) (string, bool) {
	root := strings.TrimPrefix(path.Clean(strings.TrimPrefix(k, "./")), "/")
	return root, root != "" && root != "."
}

// layer is one parsed manifest file.
type layer struct {
	rules  []rules.Rule
	cache  map[string]Cache
	env    Env
	daemon Daemon
}

// Load resolves: overrides (rule lines, Origin "flag") → <root>/.o2/ovnfs.yml →
// <userHome>/.o2/ovnfs.yml → Builtins(). Missing files are skipped. userHome ""
// skips the user file.
func Load(root, userHome string, overrides []string) (Manifest, error) {
	var m Manifest
	for _, o := range overrides {
		r, err := rules.ParseLine(o, "flag")
		if err != nil {
			return Manifest{}, err
		}
		m.Rules.Rules = append(m.Rules.Rules, r)
	}

	// A root that is the user's home has one manifest, not two layers of the same file.
	folder := FolderDoc(root, userHome)
	project, okProject, err := readLayer(folder.Path, folder.Name, folder.user)
	if err != nil {
		return Manifest{}, err
	}
	var user layer
	var okUser bool
	if userHome != "" && !folder.user {
		u := UserDoc(userHome)
		user, okUser, err = readLayer(u.Path, u.Name, true)
		if err != nil {
			return Manifest{}, err
		}
	}

	m.Rules.Rules = append(m.Rules.Rules, project.rules...)
	m.Rules.Rules = append(m.Rules.Rules, user.rules...)
	m.Rules.Rules = append(m.Rules.Rules, rules.Builtins()...)

	if len(user.cache)+len(project.cache) > 0 {
		m.Cache = make(map[string]Cache, len(user.cache)+len(project.cache))
		maps.Copy(m.Cache, user.cache)
		maps.Copy(m.Cache, project.cache)
	}

	m.Env = user.env
	if project.env.Image != "" {
		m.Env.Image = project.env.Image
	}
	if project.env.Setup != nil {
		m.Env.Setup = project.env.Setup
	}

	if okProject {
		m.Files = append(m.Files, File)
	}
	if okUser {
		m.Files = append(m.Files, userFile)
	}
	return m, nil
}

// UserDaemon reads the daemon settings of <userHome>/.o2/ovnfs.yml, a leading
// ~/ standing for userHome. A missing file or key leaves the field empty; the
// whole file is checked, so any manifest error fails here too.
func UserDaemon(userHome string) (Daemon, error) {
	u := UserDoc(userHome)
	l, _, err := readLayer(u.Path, u.Name, true)
	if err != nil {
		return Daemon{}, err
	}
	expand := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return filepath.Join(userHome, filepath.FromSlash(rest))
		}
		return p
	}
	mount := expand(l.daemon.Mount)
	if driveLetter(mount) {
		mount += `\`
	}
	l.daemon.State = expand(l.daemon.State)
	l.daemon.Mount = mount
	return l.daemon, nil
}

// driveLetter reports a bare Windows drive such as O:, a mount meaning the
// drive's root (filepath.IsAbs rejects it: it names the drive's current directory).
func driveLetter(p string) bool {
	return runtime.GOOS == "windows" && len(p) == 2 && p[1] == ':' && ('A' <= p[0] && p[0] <= 'Z' || 'a' <= p[0] && p[0] <= 'z')
}

// readLayer reads and parses one manifest file; ok is false when it does not
// exist. Only the user-wide file may set daemon settings.
func readLayer(name, display string, user bool) (layer, bool, error) {
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return layer{}, false, nil
	}
	if err != nil {
		return layer{}, false, fmt.Errorf("read %s: %w", display, err)
	}
	l, err := parseLayer(data, display, user)
	if err != nil {
		return layer{}, false, err
	}
	return l, true, nil
}

// fileSchema is the YAML shape of a manifest file; Rules stays a node to keep its line number.
type fileSchema struct {
	Rules      yaml.Node        `yaml:"rules"`
	Cache      map[string]Cache `yaml:"cache"`
	Env        Env              `yaml:"env"`
	State      string           `yaml:"state"`
	Mount      string           `yaml:"mount"`
	Proto      string           `yaml:"proto"`
	Memory     string           `yaml:"memory"`
	Fsync      string           `yaml:"fsync"`
	NodeCache  string           `yaml:"node_cache"`
	BlobCache  string           `yaml:"blob_cache"`
	Atime      string           `yaml:"atime"`
	NFSIOSize  string           `yaml:"nfs_io_size"`
	GitRemotes string           `yaml:"git_remotes"`
}

// parseLayer strictly decodes a manifest file, accepting daemon settings only
// in the user's; errors are reported as display:line.
func parseLayer(data []byte, display string, user bool) (layer, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return layer{}, yamlError(display, err)
	}
	if len(doc.Content) == 0 {
		return layer{}, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return layer{}, nil
	}
	if root.Kind != yaml.MappingNode {
		return layer{}, fmt.Errorf("%s:%d: expected a mapping with rules, cache and env", display, root.Line)
	}
	if err := checkKeys(root, display, "", "rules", "cache", "env", "state", "mount", "proto", "memory", "fsync", "node_cache", "blob_cache", "atime", "git_remotes", "nfs_io_size"); err != nil {
		return layer{}, err
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i].Value, root.Content[i+1]
		switch k {
		case "state", "mount":
			if !user {
				return layer{}, fmt.Errorf("%s:%d: %q is read only from %s", display, root.Content[i].Line, k, userFile)
			}
			if p := v.Value; v.Kind == yaml.ScalarNode && p != "" && !strings.HasPrefix(p, "~/") && !filepath.IsAbs(p) && !(k == "mount" && driveLetter(p)) {
				return layer{}, fmt.Errorf("%s:%d: %s must be an absolute path or start with ~/", display, v.Line, k)
			}
		case "proto", "memory", "fsync", "node_cache", "blob_cache", "atime", "git_remotes", "nfs_io_size":
			if !user {
				return layer{}, fmt.Errorf("%s:%d: %q is read only from %s", display, root.Content[i].Line, k, userFile)
			}
			typ := daemonKeys[k]
			valid := v.Kind == yaml.ScalarNode && (v.Tag == "!!str" || v.Tag == "!!null" || typ != "" && v.Tag == typ)
			if !valid {
				return layer{}, fmt.Errorf("%s:%d: invalid value type for %s", display, v.Line, k)
			}
		case "cache":
			if v.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				at := "cache." + v.Content[j].Value + "."
				if err := checkKeys(v.Content[j+1], display, at, "rebuild", "key"); err != nil {
					return layer{}, err
				}
			}
		case "env":
			if err := checkKeys(v, display, "env.", "image", "setup"); err != nil {
				return layer{}, err
			}
		}
	}

	var f fileSchema
	if err := root.Decode(&f); err != nil {
		return layer{}, yamlError(display, err)
	}
	l := layer{env: f.Env, daemon: Daemon{
		State: f.State, Mount: f.Mount, Proto: f.Proto, Memory: f.Memory,
		Fsync: f.Fsync, NodeCache: f.NodeCache, BlobCache: f.BlobCache, Atime: f.Atime, GitRemotes: f.GitRemotes, NFSIOSize: f.NFSIOSize,
	}}
	for k, c := range f.Cache {
		key, ok := cacheRoot(k)
		if !ok {
			return layer{}, fmt.Errorf("%s: invalid cache key %q", display, k)
		}
		if l.cache == nil {
			l.cache = make(map[string]Cache, len(f.Cache))
		}
		l.cache[key] = c
	}

	switch n := &f.Rules; {
	case n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null"):
	case n.Kind != yaml.ScalarNode:
		return layer{}, fmt.Errorf("%s:%d: rules must be a block of rule lines", display, n.Line)
	default:
		// A block scalar's content starts on the line after its `|` indicator.
		first := n.Line
		if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			first++
		}
		rs, err := rules.Parse(n.Value, display, first)
		if err != nil {
			return layer{}, err
		}
		l.rules = rs
	}
	return l, nil
}

// checkKeys rejects mapping keys outside allowed, naming the file, line and dotted key.
func checkKeys(n *yaml.Node, display, at string, allowed ...string) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		k := n.Content[i]
		if !slices.Contains(allowed, k.Value) {
			return fmt.Errorf("%s:%d: unknown key %q", display, k.Line, at+k.Value)
		}
	}
	return nil
}

// yamlLine splits yaml.v3's "line N: message" error form.
var yamlLine = regexp.MustCompile(`^line (\d+): (.*)$`)

// yamlError rewrites yaml.v3 errors into display:line: message form.
func yamlError(display string, err error) error {
	var msgs []string
	if te, ok := errors.AsType[*yaml.TypeError](err); ok {
		msgs = te.Errors
	} else {
		msgs = []string{strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	out := make([]string, len(msgs))
	for i, msg := range msgs {
		if m := yamlLine.FindStringSubmatch(msg); m != nil {
			out[i] = display + ":" + m[1] + ": " + m[2]
		} else {
			out[i] = display + ": " + msg
		}
	}
	return errors.New(strings.Join(out, "\n"))
}

// Lock is the resolved manifest frozen when a checkout is created; it is stored
// with the checkout and every snapshot.
type Lock struct {
	Rules []LockRule       `json:"rules"`
	Cache map[string]Cache `json:"cache,omitempty"`
	Env   Env              `json:"env"`
	Files []string         `json:"files,omitempty"`
}

// LockRule is a rule in serializable form.
type LockRule struct {
	Class  rules.Class `json:"class"`
	Text   string      `json:"text"`
	Origin string      `json:"origin"`
}

// Lock freezes the manifest into a value independent of the files it came from.
func (m Manifest) Lock() Lock {
	l := Lock{
		Rules: make([]LockRule, len(m.Rules.Rules)),
		Env:   Env{Image: m.Env.Image, Setup: slices.Clone(m.Env.Setup)},
		Files: slices.Clone(m.Files),
	}
	for i, r := range m.Rules.Rules {
		l.Rules[i] = LockRule{Class: r.Class, Text: r.Text, Origin: r.Origin}
	}
	if len(m.Cache) > 0 {
		l.Cache = make(map[string]Cache, len(m.Cache))
		for k, c := range m.Cache {
			l.Cache[k] = Cache{Rebuild: c.Rebuild, Key: slices.Clone(c.Key)}
		}
	}
	return l
}

// Set re-parses the locked rules, keeping their Origin.
func (l Lock) Set() (rules.Set, error) {
	s := rules.Set{Rules: make([]rules.Rule, 0, len(l.Rules))}
	for i, lr := range l.Rules {
		r, err := rules.ParseLine(string(lr.Class)+" "+lr.Text, lr.Origin)
		if err != nil {
			return rules.Set{}, fmt.Errorf("lock rule %d: %w", i+1, err)
		}
		s.Rules = append(s.Rules, r)
	}
	return s, nil
}

// Hash identifies the lock: "sha256:<hex>" of its canonical JSON (sorted keys).
func (l Lock) Hash() string {
	if l.Rules == nil {
		l.Rules = []LockRule{} // nil and empty rule lists are the same lock
	}
	// Marshalling plain structs, maps and strings cannot fail; re-decoding into
	// generic values makes encoding/json sort every object's keys.
	b, _ := json.Marshal(l)
	var v any
	_ = json.Unmarshal(b, &v)
	b, _ = json.Marshal(v)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CacheFor finds the cache entry for a cache root path: exact relative path
// key first, then its basename.
func (l Lock) CacheFor(root string) (Cache, bool) {
	if c, ok := l.Cache[root]; ok {
		return c, true
	}
	c, ok := l.Cache[path.Base(root)]
	return c, ok
}

// KeyHash hashes the key files of a cache root; key paths are relative to the
// root's parent directory inside source dir `dir`. Returns "sha256:<hex>"; a
// missing key file contributes its name plus "missing".
func KeyHash(dir, cacheRoot string, keys []string) (string, error) {
	parent := path.Dir(cacheRoot)
	sorted := slices.Clone(keys)
	sort.Strings(sorted)
	h := sha256.New()
	for _, k := range sorted {
		rel := path.Join(parent, k)
		if rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(k) {
			return "", fmt.Errorf("cache %s: key %q is outside the source", cacheRoot, k)
		}
		fmt.Fprintf(h, "%s\x00", k)
		sum, err := fileHash(filepath.Join(dir, filepath.FromSlash(rel)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			io.WriteString(h, "missing\n")
		case err != nil:
			return "", fmt.Errorf("cache %s: key %q: %w", cacheRoot, k, err)
		default:
			fmt.Fprintf(h, "%x\n", sum)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// fileHash returns the sha256 of a regular file's contents.
func fileHash(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
