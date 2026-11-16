//go:build !windows

package overlay

import (
	"io/fs"
	"os"
	"path"
	"syscall"

	"github.com/stencil-hq/ovnfs/internal/search/fsys"
)

// Unix stat records already contain identity; a second per-path map duplicates it.
const cachePathIDs = false

func layerEntries(dir string) ([]os.DirEntry, error) { return fsys.ReadDir(dir) }

func layerInfo(_, _ string, info os.FileInfo) (os.FileInfo, error) { return info, nil }

func retainInfo(p string, info os.FileInfo) os.FileInfo {
	if p == "" {
		return info
	}
	return fsys.RetainInfo(path.Base(p), info)
}

// primeInfo is unnecessary on Unix because Lstat already contains inode identity.
func primeInfo(os.FileInfo) error { return nil }

// identity extracts stable host inode identity from existing Unix stat metadata.
func identity(_ string, p string, info os.FileInfo) (inodeKey, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return inodeKey{}, &fs.PathError{Op: "inode", Path: p, Err: fs.ErrInvalid}
	}
	return inodeKey{device: uint64(st.Dev), inode: uint64(st.Ino)}, nil
}
