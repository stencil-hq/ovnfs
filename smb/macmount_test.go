package smb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/dirbottom"
	"github.com/stencil-hq/ovnfs/overlay"
)

// TestMacNegotiateZeroCreditRequest keeps SESSION_SETUP usable after macOS requests no credits.
func TestMacNegotiateZeroCreditRequest(t *testing.T) {
	c := &conn{}
	r := &request{hdr: header{Command: cmdNegotiate, CreditCharge: 1}}
	if st := c.consumeCredits(r); st != statusSuccess {
		t.Fatalf("NEGOTIATE credits: %#x", st)
	}
	if grant := c.grantCredits(0); grant == 0 {
		t.Fatal("NEGOTIATE did not replenish the initial credit")
	}
	r.hdr.Command = cmdSessionSetup
	if st := c.consumeCredits(r); st != statusSuccess {
		t.Fatalf("SESSION_SETUP credits: %#x", st)
	}
}

// TestMacAppleDirectoryModes reports actual Unix permission bits only after Apple negotiation.
func TestMacAppleDirectoryModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix permission bits")
	}
	lower, upper := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(lower, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs, err := overlay.New(upper, nil, dirbottom.New(lower), overlay.NewMemory(64<<20, 1<<20, 0))
	if err != nil {
		t.Fatal(err)
	}
	backend := &testBackend{FS: fs, ids: make(map[string]uint64)}
	for name, caps := range map[string]uint64{"none": 0, "v1": 1, "v2": 0x10, "both": 0x11} {
		t.Run(name, func(t *testing.T) {
			c := &conn{srv: &Server{FS: backend}, maxTrans: 4096}
			in := make([]byte, 24)
			binary.LittleEndian.PutUint64(in[8:], 1)
			binary.LittleEndian.PutUint64(in[16:], caps)
			reply := appleCreateResponse(c, in)
			gotCaps := binary.LittleEndian.Uint64(reply[16:])
			wantCaps := uint64(4 | 8)
			if caps&0x10 != 0 {
				wantCaps |= 0x10
			} else if caps&1 != 0 {
				wantCaps |= 1
			}
			if gotCaps != wantCaps {
				t.Fatalf("Apple server capabilities: %#x, want %#x", gotCaps, wantCaps)
			}
			tree := &tree{}
			o := &open{isDir: true, access: 1}
			tree.addOpen(o)
			b := make([]byte, 32)
			binary.LittleEndian.PutUint16(b, 33)
			b[2] = 37
			putOpenID(b[8:], o.id)
			binary.LittleEndian.PutUint16(b[24:], 96)
			binary.LittleEndian.PutUint16(b[26:], uint16(utf16Len("run.sh")))
			binary.LittleEndian.PutUint32(b[28:], 4096)
			b = utf16Append(b, "run.sh")
			out, st := handleQueryDirectory(&request{c: c, tree: tree, body: b})
			if st != statusSuccess || len(out) < 112 {
				t.Fatalf("QUERY_DIRECTORY: status %#x, length %d", st, len(out))
			}
			mode := binary.LittleEndian.Uint16(out[8+94:])
			if caps == 0 {
				if mode != 0 {
					t.Fatalf("unnegotiated reserved mode: %#o", mode)
				}
			} else if mode != 0100755 {
				t.Fatalf("Apple Unix mode: %#o, want 0100755", mode)
			}
		})
	}
}

// TestNFSModeACEPermissions accepts macOS and Linux mode ACEs without changing lower.
func TestNFSModeACEPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix permission bits")
	}
	lower, upper := t.TempDir(), t.TempDir()
	name := filepath.Join(lower, "file")
	if err := os.WriteFile(name, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := overlay.New(upper, nil, dirbottom.New(lower), overlay.NewMemory(64<<20, 1<<20, 0))
	if err != nil {
		t.Fatal(err)
	}
	info, err := fs.Lstat("file")
	if err != nil {
		t.Fatal(err)
	}
	sd := securityDescriptor(info, 4, nil, nil)
	acl := int(binary.LittleEndian.Uint32(sd[16:]))
	off := acl + 8
	for range 3 {
		off += int(binary.LittleEndian.Uint16(sd[off+2:]))
	}
	if sd[off] != 1 || binary.LittleEndian.Uint32(sd[off+4:]) != 0 {
		t.Fatal("NFS mode ACE must be denied with no access rights")
	}
	binary.LittleEndian.PutUint32(sd[off+24:], 0100755)
	if st := applySecurity(&testBackend{FS: fs}, "file", sd); st != statusSuccess {
		t.Fatalf("apply denied NFS mode ACE: %#x", st)
	}
	if err := fs.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Join(upper, "file"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("upper chmod: info %v, error %v", info, err)
	}
	sd[off] = 0
	binary.LittleEndian.PutUint32(sd[off+24:], 0100644)
	if st := applySecurity(&testBackend{FS: fs}, "file", sd); st != statusSuccess {
		t.Fatalf("apply allowed NFS mode ACE: %#x", st)
	}
	info, err = os.Stat(filepath.Join(upper, "file"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("upper Linux chmod: info %v, error %v", info, err)
	}
	info, err = os.Stat(name)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("lower changed: info %v, error %v", info, err)
	}
}

// TestMacSymlinkModeACE never applies reparse-point permissions to the link target.
func TestMacSymlinkModeACE(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix symlinks")
	}
	lower, upper := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(lower, "target"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := overlay.New(upper, nil, dirbottom.New(lower), overlay.NewMemory(64<<20, 1<<20, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Symlink("target", "link"); err != nil {
		t.Fatal(err)
	}
	info, err := fs.Lstat("link")
	if err != nil {
		t.Fatal(err)
	}
	sd := securityDescriptor(info, 4, nil, nil)
	backend := &testBackend{FS: fs}
	if st := applySecurity(backend, "link", sd); st != statusSuccess {
		t.Fatalf("unchanged symlink mode: %#x", st)
	}
	mode := binary.LittleEndian.Uint32(sd[len(sd)-4:])
	binary.LittleEndian.PutUint32(sd[len(sd)-4:], mode^0100)
	if st := applySecurity(backend, "link", sd); st != statusNotSupported {
		t.Fatalf("unsupported symlink mode change: %#x", st)
	}
	info, err = fs.Stat("target")
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target changed: info %v, error %v", info, err)
	}
}

// TestMacReadonlyMetadataOpen allows rename rights without demanding a writable data descriptor.
func TestMacReadonlyMetadataOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix permission bits")
	}
	fixture := startSMB(t, nil, func(lower, upper string) {
		putDiskFile(t, lower, "readonly", "content")
		if err := os.Chmod(filepath.Join(lower, "readonly"), 0o444); err != nil {
			t.Fatal(err)
		}
	})
	c := newWireClient(t, fixture.address)
	c.negotiate([]uint16{0x0210})
	c.expectStatus(c.login("guest", "", true), wireSuccess)
	c.expectStatus(c.treeConnect("test"), wireSuccess)
	b := wireCreateBody("readonly", 1, 0)
	binary.LittleEndian.PutUint32(b[24:], 0x001f0180)
	reply := c.exchange(5, b)
	c.expectStatus(reply, wireSuccess)
	var id wireID
	copy(id[:], reply.body[64:80])
	c.expectStatus(c.rename(id, "renamed", false), wireSuccess)
	c.expectStatus(c.close(id), wireSuccess)
	fixture.flush(t)
	info, err := os.Stat(filepath.Join(fixture.upper, "renamed"))
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("renamed readonly file: info %v, error %v", info, err)
	}
}
