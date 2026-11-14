package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/source"
)

// tuneProject maps the original Cargo workspace into each disposable checkout.
type tuneProject struct {
	Root      string
	Subdir    string
	Touch     string
	Toolchain string
	Tree      string
	Changed   bool
}

func prepareTuneProject(ctx context.Context, dir string) (*tuneProject, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	env := append(os.Environ(), "RUSTUP_AUTO_INSTALL=0", "CARGO_NET_OFFLINE=true")
	out, err := tuneOutput(wait, []string{"cargo", "metadata", "--manifest-path", filepath.Join(dir, "Cargo.toml"), "--format-version=1", "--locked", "--offline", "--no-deps"}, dir, env)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		Root     string   `json:"workspace_root"`
		Members  []string `json:"workspace_members"`
		Packages []struct {
			ID      string `json:"id"`
			Targets []struct {
				Kind []string `json:"kind"`
				Path string   `json:"src_path"`
			} `json:"targets"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(out, &metadata); err != nil {
		return nil, err
	}
	workspace, err := filepath.EvalSymlinks(metadata.Root)
	if err != nil {
		return nil, err
	}
	root, err := source.Resolve(workspace)
	if err != nil {
		return nil, err
	}
	subdir, err := filepath.Rel(root, workspace)
	if err != nil || !filepath.IsLocal(subdir) {
		return nil, fmt.Errorf("Cargo workspace is outside its source root")
	}
	p := &tuneProject{Root: root, Subdir: subdir}
	for _, kind := range []string{"bin", "lib", "proc-macro"} {
		for _, pkg := range metadata.Packages {
			if !slices.Contains(metadata.Members, pkg.ID) {
				continue
			}
			for _, target := range pkg.Targets {
				if !slices.Contains(target.Kind, kind) {
					continue
				}
				path, err := filepath.EvalSymlinks(target.Path)
				if err != nil {
					return nil, err
				}
				rel, err := filepath.Rel(workspace, path)
				if err == nil && filepath.IsLocal(rel) && rel != "." {
					p.Touch = rel
					break
				}
			}
			if p.Touch != "" {
				break
			}
		}
		if p.Touch != "" {
			break
		}
	}
	if p.Touch == "" {
		return nil, errors.New("Cargo workspace has no local Rust target to touch for incremental measurement")
	}
	out, err = tuneOutput(wait, []string{"rustup", "show", "active-toolchain"}, workspace, env)
	if err == nil {
		if fields := strings.Fields(string(out)); len(fields) > 0 {
			p.Toolchain = fields[0]
		}
	} else if !errors.Is(err, exec.ErrNotFound) {
		return nil, err
	}
	return p, nil
}
