//go:build windows

package smb

import (
	"encoding/binary"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestSecurityOwnerIsDaemonUser checks that a loopback share reports files as
// owned by the daemon's user and primary group: Git for Windows refuses a
// repository whose owner is not the current user.
func TestSecurityOwnerIsDaemonUser(t *testing.T) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	primary, err := token.GetTokenPrimaryGroup()
	if err != nil {
		t.Fatal(err)
	}
	fixture := startSMB(t, nil, nil)
	c := guestClient(t, fixture)
	id, reply := c.create("file", 2, 0)
	c.expectStatus(reply, wireSuccess)
	defer c.close(id)
	// QUERY_INFO for SMB2_0_INFO_SECURITY, OWNER and GROUP information.
	body := wireQueryBody(id, 0)
	body[2] = 3
	binary.LittleEndian.PutUint32(body[16:], 3)
	r := c.exchange(16, body)
	c.expectStatus(r, wireSuccess)
	if len(r.body) < 8 {
		t.Fatal("short QUERY_INFO response")
	}
	sd := c.wireSecurity(r, int(binary.LittleEndian.Uint16(r.body[2:])), int(binary.LittleEndian.Uint32(r.body[4:])))
	if len(sd) < 20 {
		t.Fatal("short security descriptor")
	}
	for _, want := range []struct {
		label  string
		offset int
		sid    *windows.SID
	}{
		{"owner", 4, user.User.Sid},
		{"group", 8, primary.PrimaryGroup},
	} {
		off := int(binary.LittleEndian.Uint32(sd[want.offset:]))
		if off < 20 || off+8 > len(sd) || off+8+4*int(sd[off+1]) > len(sd) {
			t.Fatalf("invalid %s SID offset %d", want.label, off)
		}
		got := (*windows.SID)(unsafe.Pointer(&sd[off]))
		if !got.IsValid() || !got.Equals(want.sid) {
			t.Errorf("%s %s, want %s", want.label, got, want.sid)
		}
	}
}
