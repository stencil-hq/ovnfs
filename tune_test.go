package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/manifest"
)

func TestTuneConfirmation(t *testing.T) {
	for _, tc := range []struct {
		input string
		apply bool
	}{
		{"", false}, {"\n", false}, {"no\n", false}, {"apply\n", false}, {"y\n", true}, {" YES\r\n", true}, {"yes", true},
	} {
		var prompt bytes.Buffer
		apply, err := confirmTune(t.Context(), strings.NewReader(tc.input), &prompt)
		if err != nil || apply != tc.apply {
			t.Fatalf("answer %q: %v, %v", tc.input, apply, err)
		}
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if yes, err := confirmTune(ctx, reader, io.Discard); yes || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled prompt: %v, %v", yes, err)
	}
}

func TestTunePersistencePreservesUserSettings(t *testing.T) {
	home := t.TempDir()
	file := manifest.UserDoc(home).Path
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	initial := "# keep this note\nproto: nfs\nmemory: 64M\nfsync: true\n"
	if err := os.WriteFile(file, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := tuneSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated edit made while trials ran must survive application.
	if err := os.WriteFile(file, []byte(initial+"blob_cache: 128M\n"), 0600); err != nil {
		t.Fatal(err)
	}
	chosen := tuneConfig{Proto: "smb", Memory: 256 << 20, NFSIOSize: 64 << 10}
	if err := saveTune(home, before, chosen); err != nil {
		t.Fatal(err)
	}
	conf, err := manifest.UserDaemon(home)
	if err != nil {
		t.Fatal(err)
	}
	if conf.Proto != "smb" || conf.Memory != "256M" || conf.NFSIOSize != "64K" || conf.Fsync != "true" || conf.BlobCache != "128M" {
		t.Fatalf("application changed unrelated settings: %+v", conf)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("# keep this note")) {
		t.Fatal("application erased the user's comment")
	}
	before, err = tuneSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(data, []byte("memory: 256M"), []byte("memory: 1G"), 1)
	if err := os.WriteFile(file, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveTune(home, before, chosen); err == nil {
		t.Fatal("overwrote a concurrently edited tuned setting")
	}
	got, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("failed application changed configuration: %v", err)
	}
}

func TestTuneFailedActivationDoesNotPersist(t *testing.T) {
	home := t.TempDir()
	file := manifest.UserDoc(home).Path
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	initial := []byte("proto: nfs\nmemory: 64M\n")
	if err := os.WriteFile(file, initial, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := tuneSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	c := &cli{ctx: ctx, state: filepath.Join(root, "state"), mount: filepath.Join(root, "mount")}
	// The package TestMain makes spawned serve processes exit immediately.
	err = applyTune(c, home, before, tuneConfig{Proto: "smb", Memory: 256 << 20, NFSIOSize: 128 << 10})
	if err == nil || !strings.Contains(err.Error(), "activation failed") {
		t.Fatalf("failed daemon activation: %v", err)
	}
	got, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(got, initial) {
		t.Fatalf("failed start changed configuration: %v", err)
	}
}

func TestTuneProfilesRespectOverrides(t *testing.T) {
	t.Setenv("OVNFS_MEMORY", "128M")
	t.Setenv("OVNFS_NFS_IO_SIZE", "64K")
	base := serveCmd{Proto: "nfs", Memory: 128 << 20, NFSIOSize: 64 << 10}
	for _, profile := range tuneProfiles(base, []string{"nfs", "smb"}, true) {
		if profile.Memory != base.Memory || profile.NFSIOSize != base.NFSIOSize {
			t.Fatalf("recommendation would be overridden on restart: %+v", profile)
		}
	}
	base.Proto, base.Fsync = "smb", true
	for _, profile := range tuneProfiles(base, []string{"nfs", "smb"}, false) {
		if profile.Proto == "nfs" {
			t.Fatal("candidate cannot preserve enabled fsync forwarding")
		}
	}
}

func TestTuneOutputDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := tuneOutput(ctx, []string{os.Args[0], "-test.run=^TestTuneSleepingChild$"}, "", append(os.Environ(), "OVNFS_TUNE_TEST_SLEEP=1"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled worker: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("worker waited beyond cancellation: %s", elapsed)
	}
}

func TestTuneSleepingChild(t *testing.T) {
	if os.Getenv("OVNFS_TUNE_TEST_SLEEP") == "1" {
		time.Sleep(time.Minute)
	}
}
