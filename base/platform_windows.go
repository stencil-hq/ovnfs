//go:build windows

package base

import (
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// metadataHandle opens a file or directory without following a reparse point.
func metadataHandle(p string, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// volumeID uses the native volume serial rather than relying on drive letters.
func volumeID(p string) (uint64, error) {
	h, err := metadataHandle(p, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &info)
	return uint64(info.VolumeSerialNumber), err
}

// fileIdentity combines a volume serial and native file index to tell one file from a lookalike.
func fileIdentity(_ os.FileInfo, root, p string) (uint64, uint64, error) {
	h, err := metadataHandle(filepath.Join(root, filepath.FromSlash(p)), windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &info)
	return uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), err
}

// birthTime returns when the entry info describes was made, in Unix
// nanoseconds; false when info carries no Windows attribute data.
func birthTime(_ string, info os.FileInfo) (int64, bool) {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return 0, false
	}
	return data.CreationTime.Nanoseconds(), true
}

// onVolume reports true: Windows file information names no volume, and a
// volume mounted in a folder is a reparse point no view follows.
func onVolume(string, os.FileInfo, uint64) bool { return true }

// setTimes updates the symlink itself through a reparse-point handle.
func setTimes(p string, t time.Time, symlink bool) error {
	if !symlink {
		return os.Chtimes(p, t, t)
	}
	h, err := metadataHandle(p, windows.FILE_WRITE_ATTRIBUTES)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ft := windows.NsecToFiletime(t.UnixNano())
	return windows.SetFileTime(h, nil, &ft, &ft)
}

// syncDirectory is unnecessary on Windows because directory handles cannot be fsynced.
func syncDirectory(_ string) error { return nil }
