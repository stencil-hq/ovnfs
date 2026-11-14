//go:build darwin || linux

package main

import (
	"net"
	"slices"
	"testing"
)

func TestMountCommandSMBIgnoresNFSIOSize(t *testing.T) {
	addr := &net.TCPAddr{Port: 1234}
	original := mountCmd("smb", addr, "/mounted", mountSettings{}, "ovnfs", "", "")
	changed := mountCmd("smb", addr, "/mounted", mountSettings{NFSIOSize: 1 << 20}, "ovnfs", "", "")
	if !slices.Equal(original, changed) {
		t.Fatalf("NFS size changed SMB command: %v vs %v", original, changed)
	}
}
