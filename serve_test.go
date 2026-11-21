package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{"OVNFS_MEMORY", "OVNFS_BLOB_CACHE", "OVNFS_ATIME", "OVNFS_FSYNC", "OVNFS_NFS_IO_SIZE", "OVNFS_GIT_HOOKS"} {
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
	t.Setenv("OVNFS_NFS_IO_SIZE", "256K")
	cmd, err = parse("--memory", "0", "--blob-cache", "0", "--proto", "smb", "--fsync=false", "--atime=false", "--nfs-io-size", "64K", "--listen", "localhost:1234")
	if err != nil || cmd.Memory != 0 || cmd.BlobCache != 0 || cmd.Proto != "smb" || cmd.Fsync || cmd.Atime || cmd.NFSIOSize != 64<<10 || cmd.Listen != "localhost:1234" {
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

	writeConfig("")
	t.Setenv("OVNFS_GIT_HOOKS", "")
	if cmd, err := parse(); err != nil || cmd.GitHooks {
		t.Fatalf("git hooks on by default: %+v, %v", cmd, err)
	}
	if cmd, err := parse("--git-hooks"); err != nil || !cmd.GitHooks {
		t.Fatalf("--git-hooks did not turn it on: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_GIT_HOOKS", "1")
	if cmd, err := parse(); err != nil || !cmd.GitHooks {
		t.Fatalf("OVNFS_GIT_HOOKS=1 did not turn it on: %+v, %v", cmd, err)
	}
	if cmd, err := parse("--no-git-hooks"); err != nil || cmd.GitHooks {
		t.Fatalf("--no-git-hooks did not win over the environment: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_GIT_HOOKS", "")
	writeConfig("git_hooks: true\n")
	if cmd, err := parse(); err != nil || !cmd.GitHooks {
		t.Fatalf("git_hooks: true in the user file did not turn it on: %+v, %v", cmd, err)
	}
	if cmd, err := parse("--no-git-hooks"); err != nil || cmd.GitHooks {
		t.Fatalf("--no-git-hooks did not win over the user file: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_GIT_HOOKS", "0")
	if cmd, err := parse(); err != nil || cmd.GitHooks {
		t.Fatalf("OVNFS_GIT_HOOKS=0 did not win over the user file: %+v, %v", cmd, err)
	}
	t.Setenv("OVNFS_GIT_HOOKS", "")

	writeConfig("unknown_key: true\n")
	if err := run([]string{"serve", "--state", filepath.Join(home, "state"), "--mount", filepath.Join(home, "mount")}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("serve with explicit directories and invalid config: %v", err)
	}
	if _, err := newCLIParser(&cli{}, []string{"version"}, &verbs{}); err != nil {
		t.Fatalf("version read user config: %v", err)
	}
}
