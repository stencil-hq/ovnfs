//go:build !windows

package main

import (
	"strings"
	"testing"
)

func TestNFSMountMismatch(t *testing.T) {
	const report = `{
		"<\/state\/nfs.sock>:/": {
			"Mount Point": "/mounted",
			"Original mount options": {"NFS parameters": ["rsize=1048576", "wsize=1048576"]},
			"Current mount parameters": {
				"General mount flags": {"Flags": ["noatime", "nobrowse", "nodev", "nosuid"]},
				"NFS parameters": ["vers=3", "ticotsord", "port=/state/nfs.sock", "mountport=/state/nfs.sock", "intr", "locallocks", "rsize=131072", "wsize=131072", "readahead=16"]
			}
		}
	}`
	const options = "vers=3,port=/state/nfs.sock,mountport=/state/nfs.sock,locallocks,nobrowse,intr,rwsize=131072,noatime"
	for _, tc := range []struct {
		name, data, options, mismatch string
		badReport                     bool
	}{
		{name: "current matches despite old original values", data: report, options: options},
		{name: "read size", data: strings.Replace(report, "rsize=131072", "rsize=1048576", 1), options: options, mismatch: "rsize="},
		{name: "write size", data: strings.Replace(report, "wsize=131072", "wsize=1048576", 1), options: options, mismatch: "wsize="},
		{name: "missing required flag", data: strings.Replace(report, `"intr",`, "", 1), options: options, mismatch: "intr"},
		{name: "atime disabled", data: report, options: strings.Replace(options, "noatime", "atime", 1), mismatch: "access times"},
		{name: "atime enabled by default", data: strings.Replace(report, `"noatime",`, "", 1), options: strings.Replace(options, "noatime", "atime", 1)},
		{name: "wrong target", data: strings.Replace(report, `"/mounted"`, `"/other"`, 1), options: options, badReport: true},
		{name: "original parameters only", data: `{"source":{"Mount Point":"/mounted","Original mount options":{"NFS parameters":["rsize=131072"]}}}`, options: options, badReport: true},
		{name: "malformed", data: `{`, options: options, badReport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mismatch, err := nfsMountMismatch([]byte(tc.data), "/mounted", []string{"mount_nfs", "-o", tc.options, "source", "/mounted"})
			if tc.badReport {
				if err == nil || mismatch != "" {
					t.Fatalf("unsafe report must fail without requesting unmount: mismatch %q, error %v", mismatch, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.mismatch == "" && mismatch != "" || tc.mismatch != "" && !strings.Contains(mismatch, tc.mismatch) {
				t.Fatalf("mismatch %q, want %q", mismatch, tc.mismatch)
			}
		})
	}
}
