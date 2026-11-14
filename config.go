package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/stencil-hq/ovnfs/manifest"
	"github.com/stencil-hq/ovnfs/source"
)

// configCmd reads and writes .o2/ovnfs.yml settings by dotted key, as git
// config does its files: the folder's file at its source root and the user's
// ~/.o2/ovnfs.yml (see docs/configuration.md). It never talks to the daemon.
type configCmd struct {
	List  configListCmd  `cmd:"" help:"print every setting as KEY=VALUE, the folder's file first"`
	Get   configGetCmd   `cmd:"" help:"print a key's values as a checkout opened here takes them; exit 1 when unset"`
	Set   configSetCmd   `cmd:"" help:"set a key; rules, env.setup and cache.ROOT.key take several values"`
	Unset configUnsetCmd `cmd:"" help:"remove a key; exit 5 when the file does not set it"`
	Edit  configEditCmd  `cmd:"" help:"open the file in $VISUAL or $EDITOR, then check that it loads"`
}

// keyHelp names the keys a config verb accepts.
const keyHelp = "rules, cache.ROOT.rebuild, cache.ROOT.key, env.image, env.setup, or a daemon setting: state, mount, proto, memory, fsync, node_cache, blob_cache, atime, nfs_io_size, git_remotes"

// configScope picks the files a config verb works on. Reads default to both,
// writes to the folder's file, or the user's for a daemon setting.
type configScope struct {
	Global bool `xor:"scope" help:"~/.o2/ovnfs.yml, which every folder reads"`
	Local  bool `xor:"scope" help:".o2/ovnfs.yml at the source root of the current folder"`
}

// files returns the unread file --global or --local names, else every file
// a checkout of the current folder reads, highest precedence first.
func (s configScope) files() ([]*manifest.Doc, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if s.Global {
		return []*manifest.Doc{manifest.UserDoc(home)}, nil
	}
	root, err := source.Resolve(".")
	if err != nil {
		return nil, err
	}
	if s.Local {
		return []*manifest.Doc{manifest.FolderDoc(root, home)}, nil
	}
	return manifest.Docs(root, home), nil
}

// read returns the files of [configScope.files], read.
func (s configScope) read() ([]*manifest.Doc, error) {
	docs, err := s.files()
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if err := d.Read(); err != nil {
			return nil, err
		}
	}
	return docs, nil
}

// file returns the one unread file a write works on: the user's under
// --global, else the folder's.
func (s configScope) file() (*manifest.Doc, error) {
	s.Local = !s.Global
	docs, err := s.files()
	if err != nil {
		return nil, err
	}
	return docs[0], nil
}

// writing returns the scope a write of key uses: a daemon setting goes to the
// user's file, the only one that may hold it, unless --local says otherwise.
func (s configScope) writing(key string) configScope {
	if !s.Local && manifest.UserOnly(key) {
		s.Global = true
	}
	return s
}

// originFlag is the --show-origin option of config list and get.
type originFlag struct {
	ShowOrigin bool `help:"print the file that sets each value first, tab-separated"`
}

// print writes settings one per line as line renders each, after its file
// under --show-origin; under --json it writes them as one JSON array.
func (o originFlag) print(c *cli, settings []manifest.Setting, line func(manifest.Setting) string) error {
	if c.json {
		if settings == nil {
			settings = []manifest.Setting{}
		}
		return json.NewEncoder(stdout).Encode(settings)
	}
	for _, s := range settings {
		if o.ShowOrigin {
			fmt.Fprintf(stdout, "%s\t", s.File)
		}
		fmt.Fprintln(stdout, line(s))
	}
	return nil
}

// configListCmd prints the settings of each file in scope.
type configListCmd struct {
	configScope
	originFlag
}

func (cmd *configListCmd) Run(c *cli) error {
	docs, err := cmd.read()
	if err != nil {
		return err
	}
	var all []manifest.Setting
	for _, d := range docs {
		settings, err := d.List()
		if err != nil {
			return err
		}
		all = append(all, settings...)
	}
	return cmd.print(c, all, func(s manifest.Setting) string { return s.Key + "=" + s.Value })
}

// configGetCmd prints one key's values, merged across files as [manifest.Load] merges them.
type configGetCmd struct {
	configScope
	originFlag
	Key string `arg:"" help:"${key_help}"`
}

func (cmd *configGetCmd) Run(c *cli) error {
	docs, err := cmd.read()
	if err != nil {
		return err
	}
	settings, err := manifest.Resolve(cmd.Key, docs...)
	if err != nil {
		return err
	}
	if len(settings) == 0 {
		return exitCode(1)
	}
	return cmd.print(c, settings, func(s manifest.Setting) string { return s.Value })
}

// configSetCmd replaces or extends one key's value and saves the file, refusing
// a result that would not load.
type configSetCmd struct {
	configScope
	Append bool     `help:"add the values after the key's current ones instead of replacing them"`
	Key    string   `arg:"" help:"${key_help}"`
	Values []string `arg:"" name:"VALUE" help:"one value; a list key takes several, an item or rule line each"`
}

func (cmd *configSetCmd) Run(c *cli) error {
	d, err := cmd.writing(cmd.Key).file()
	if err != nil {
		return err
	}
	if err := d.Read(); err != nil {
		return err
	}
	set := d.Set
	if cmd.Append {
		set = d.Append
	}
	if err := set(cmd.Key, cmd.Values...); err != nil {
		return err
	}
	return d.Save()
}

// configUnsetCmd removes one key and saves the file.
type configUnsetCmd struct {
	configScope
	Key string `arg:"" help:"${key_help}"`
}

func (cmd *configUnsetCmd) Run(c *cli) error {
	d, err := cmd.writing(cmd.Key).file()
	if err != nil {
		return err
	}
	if err := d.Read(); err != nil {
		return err
	}
	found, err := d.Unset(cmd.Key)
	if err != nil {
		return err
	}
	if !found {
		return exitCode(5)
	}
	return d.Save()
}

// configEditCmd opens a file in the user's editor, then reports whether it
// loads; a file the editor did not save is not created.
type configEditCmd struct {
	configScope
}

func (cmd *configEditCmd) Run(c *cli) error {
	d, err := cmd.file()
	if err != nil {
		return err
	}
	dir := filepath.Dir(d.Path)
	created := true
	if err := os.Mkdir(dir, 0o755); errors.Is(err, fs.ErrExist) {
		created = false
	} else if err != nil {
		return err
	}
	editor := cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR"), defaultEditor)
	run := editorCommand(editor, d.Path)
	run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = run.Run()
	if created {
		os.Remove(dir) // fails, keeping it, once the editor saved the file
	}
	if err != nil {
		return fmt.Errorf("editor %s: %w", editor, err)
	}
	if err := d.Read(); err != nil {
		return err
	}
	return d.Check()
}
