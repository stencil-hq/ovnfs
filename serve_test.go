package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/export"
)

func TestServeOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{"OVNFS_NODE_CACHE", "OVNFS_MEMORY", "OVNFS_BLOB_CACHE", "OVNFS_ATIME", "OVNFS_FSYNC"} {
		t.Setenv(name, "")
	}
	parse := func(args ...string) (serveCmd, error) {
		var commands verbs
		args = append([]string{"serve"}, args...)
		parser, err := newCLIParser(&cli{}, args, &commands)
		if err != nil {
			return commands.Serve, err
		}
		_, err = parser.Parse(args)
		return commands.Serve, err
	}

	cmd, err := parse()
	if err != nil || cmd.Proto != defaultProto() || cmd.Memory != 64<<20 || cmd.BlobCache != 16<<20 || cmd.Atime || cmd.Fsync {
		t.Fatalf("defaults: %+v, %v", cmd, err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".o2"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".o2", "ovnfs.yml")
	writeConfig := func(s string) {
		t.Helper()
		if err := os.WriteFile(config, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("proto: smb\nmemory: 3G\nblob_cache: 128M\nfsync: true\natime: true\n")
	cmd, err = parse()
	if err != nil || cmd.Proto != "smb" || cmd.Memory != 3<<30 || cmd.BlobCache != 128<<20 || !cmd.Fsync || !cmd.Atime {
		t.Fatalf("user config: %+v, %v", cmd, err)
	}
	writeConfig("proto: nfs\nmemory: 0\nblob_cache: 0\nfsync: false\natime: false\n")
	cmd, err = parse()
	if err != nil || cmd.Proto != "nfs" || cmd.Memory != 0 || cmd.BlobCache != 0 || cmd.Fsync || cmd.Atime {
		t.Fatalf("zero user config: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_MEMORY", "2G")
	t.Setenv("OVNFS_BLOB_CACHE", "64MiB")
	t.Setenv("OVNFS_ATIME", "1")
	t.Setenv("OVNFS_FSYNC", "1")
	cmd, err = parse()
	if err != nil || cmd.Proto != "nfs" || cmd.Memory != 2<<30 || cmd.BlobCache != 64<<20 || !cmd.Atime || !cmd.Fsync {
		t.Fatalf("environment: %+v, %v", cmd, err)
	}
	writeConfig("proto: nfs\nmemory: 3G\nblob_cache: 128M\nfsync: true\natime: true\n")
	t.Setenv("OVNFS_MEMORY", "0")
	t.Setenv("OVNFS_BLOB_CACHE", "0")
	t.Setenv("OVNFS_FSYNC", "false")
	t.Setenv("OVNFS_ATIME", "false")
	cmd, err = parse()
	if err != nil || cmd.Memory != 0 || cmd.BlobCache != 0 || cmd.Fsync || cmd.Atime {
		t.Fatalf("zero environment over user config: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_MEMORY", "2G")
	t.Setenv("OVNFS_BLOB_CACHE", "64MiB")
	t.Setenv("OVNFS_FSYNC", "1")
	t.Setenv("OVNFS_ATIME", "1")
	cmd, err = parse("--memory", "0", "--blob-cache", "0", "--proto", "smb", "--fsync=false", "--atime=false", "--listen", "localhost:1234")
	if err != nil || cmd.Memory != 0 || cmd.BlobCache != 0 || cmd.Proto != "smb" || cmd.Fsync || cmd.Atime || cmd.Listen != "localhost:1234" {
		t.Fatalf("flags: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_FSYNC", "false")
	cmd, err = parse("--fsync")
	if err != nil || !cmd.Fsync {
		t.Fatalf("fsync flag: %+v, %v", cmd, err)
	}
	if _, err := parse("--memory", "-1"); err == nil {
		t.Fatal("negative memory accepted")
	}
	if _, err := parse("--proto", "unknown"); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	if _, err := parse("--unknown-option"); err == nil {
		t.Fatal("unknown serve flag accepted")
	}

	writeConfig("unknown_key: true\n")
	if err := run([]string{"serve", "--state", filepath.Join(home, "state"), "--mount", filepath.Join(home, "mount")}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("serve with explicit directories and invalid config: %v", err)
	}
	if _, err := newCLIParser(&cli{}, []string{"version"}, &verbs{}); err != nil {
		t.Fatalf("version read user config: %v", err)
	}
}

// TestNodeCacheOptions checks user-file, environment and flag precedence.
func TestNodeCacheOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OVNFS_NODE_CACHE", "")
	if err := os.MkdirAll(filepath.Join(home, ".o2"), 0755); err != nil {
		t.Fatal(err)
	}
	parse := func(flags ...string) (byteSize, error) {
		var commands verbs
		args := append([]string{"serve"}, flags...)
		parser, err := newCLIParser(&cli{}, args, &commands)
		if err == nil {
			_, err = parser.Parse(args)
		}
		return commands.Serve.NodeCache, err
	}
	if n, err := parse(); err != nil || n != export.DefaultNodeCache {
		t.Fatalf("default %d: %v", n, err)
	}
	if err := os.WriteFile(filepath.Join(home, ".o2", "ovnfs.yml"), []byte("node_cache: 2M\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if n, err := parse(); err != nil || n != 2<<20 {
		t.Fatalf("user %d: %v", n, err)
	}
	t.Setenv("OVNFS_NODE_CACHE", "1M")
	if n, err := parse(); err != nil || n != 1<<20 {
		t.Fatalf("env %d: %v", n, err)
	}
	if n, err := parse("--node-cache", "0"); err != nil || n != 0 {
		t.Fatalf("flag %d: %v", n, err)
	}
	if _, err := parse("--node-cache", "-1"); err == nil {
		t.Fatal("negative node cache accepted")
	}
}
