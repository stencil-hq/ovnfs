//go:build windows

package overlay

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Windows identity requires a handle query, so keep its existing path cache.
const cachePathIDs = true

func retainInfo(_ string, info os.FileInfo) os.FileInfo { return info }

func layerEntries(dir string) ([]os.DirEntry, error) { return os.ReadDir(dir) }

func layerInfo(dir, name string, info os.FileInfo) (os.FileInfo, error) {
	return withMode(filepath.Join(dir, name), info)
}

// primeInfo makes raw Windows FileInfo independent of the path used by Lstat.
func primeInfo(info os.FileInfo) error {
	if info = rawInfo(info); os.SameFile(info, info) {
		return nil
	}
	return &fs.PathError{Op: "inode", Path: info.Name(), Err: fs.ErrInvalid}
}

// identity reads the volume and file index without following a reparse point.
func identity(root, p string, _ os.FileInfo) (inodeKey, error) {
	name, err := windows.UTF16PtrFromString(filepath.Join(root, p))
	if err != nil {
		return inodeKey{}, err
	}
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return inodeKey{}, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return inodeKey{}, err
	}
	return inodeKey{device: uint64(info.VolumeSerialNumber), inode: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, nil
}
