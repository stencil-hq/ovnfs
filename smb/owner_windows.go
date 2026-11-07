//go:build windows

package smb

import (
	"bytes"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processSIDs returns the binary SIDs of this process's token user and primary group.
func processSIDs() (owner, group []byte, err error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	primary, err := token.GetTokenPrimaryGroup()
	if err != nil {
		return nil, nil, err
	}
	return sidBytes(user.User.Sid), sidBytes(primary.PrimaryGroup), nil
}

// sidBytes copies sid out of the token buffer that holds it.
func sidBytes(sid *windows.SID) []byte {
	return bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(sid)), windows.GetLengthSid(sid)))
}
