// Package hostfile opens, renames and removes host files, and sets a symlink's
// own times, with POSIX semantics on every platform. On Unix it is the os
// package and utimensat. On Windows, files open with FILE_SHARE_DELETE and
// renames and removes use POSIX semantics, so a file the
// daemon holds open for a client can still be renamed, replaced or deleted, and
// read-only files are replaced or deleted without touching their attribute, as
// on Unix.
// Windows still refuses to rename a directory while any file below it is open;
// see [overlay.FS.CloseFiles]. [Lstat] describes Windows junctions as symlinks,
// and [Symlink] links a directory with a junction where Windows refuses the
// account a symlink. [Unresolvable] tells a stat error that never clears (a
// symlink loop) from one that may.
package hostfile

import "os"

// Open opens name for reading; see [OpenFile].
func Open(name string) (*os.File, error) { return OpenFile(name, os.O_RDONLY, 0) }

// Lstat is [os.Lstat] with [Linked] applied.
func Lstat(name string) (os.FileInfo, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	return Linked(info, name), nil
}
