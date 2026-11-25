//go:build linux

package base

import (
	"os"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/internal/durable"
	"golang.org/x/sys/unix"
)

// volumeID distinguishes filesystems that cannot share reflink extents.
// On Linux this is statfs(2) f_fsid, not st_dev: btrfs subvolumes of one
// filesystem have distinct device numbers, but FICLONE still works between
// them. A zero f_fsid falls back to st_dev.
func volumeID(p string) (uint64, error) {
	return linuxVolumeID(p, false)
}

func linuxVolumeID(p string, noFollow bool) (uint64, error) {
	var st unix.Statfs_t
	if noFollow {
		fd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return 0, err
		}
		defer unix.Close(fd)
		if err := unix.Fstatfs(fd, &st); err != nil {
			return 0, err
		}
		if id := packFsid(st.Fsid); id != 0 {
			return id, nil
		}
		var s unix.Stat_t
		if err := unix.Fstat(fd, &s); err != nil {
			return 0, err
		}
		return uint64(s.Dev), nil
	}
	if err := unix.Statfs(p, &st); err != nil {
		return 0, err
	}
	if id := packFsid(st.Fsid); id != 0 {
		return id, nil
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return uint64(info.Sys().(*syscall.Stat_t).Dev), nil
}

func packFsid(id unix.Fsid) uint64 {
	return uint64(uint32(id.Val[0])) | uint64(uint32(id.Val[1]))<<32
}

// fileIdentity returns the device and inode that tell one file from a lookalike.
func fileIdentity(info os.FileInfo, _, _ string) (uint64, uint64, error) {
	s := info.Sys().(*syscall.Stat_t)
	return uint64(s.Dev), uint64(s.Ino), nil
}

// birthTime returns when the entry at host was made, in Unix nanoseconds;
// false for a filesystem or kernel that reports no birth time. It costs a
// statx: the stat data info carries has none.
func birthTime(host string, _ os.FileInfo) (int64, bool) {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, host, unix.AT_SYMLINK_NOFOLLOW|unix.AT_STATX_DONT_SYNC, unix.STATX_BTIME, &stx); err != nil || stx.Mask&unix.STATX_BTIME == 0 {
		return 0, false
	}
	return stx.Btime.Sec*int64(time.Second) + int64(stx.Btime.Nsec), true
}

// onVolume reports whether the entry at host belongs to volume (see volumeID).
// It looks up the path so btrfs subvolumes that share a filesystem compare
// equal, matching volumeID. A failed lookup is treated like missing stat data.
func onVolume(host string, _ os.FileInfo, volume uint64) bool {
	id, err := linuxVolumeID(host, true)
	if err != nil {
		// Same as missing stat data: do not hide the entry.
		return true
	}
	return id == volume
}

// setTimes preserves symlink timestamps rather than changing their targets.
func setTimes(p string, t time.Time, symlink bool) error {
	flags := 0
	if symlink {
		flags = unix.AT_SYMLINK_NOFOLLOW
	}
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, []unix.Timespec{ts, ts}, flags)
}

// syncDirectory makes the metadata rename durable across crashes.
func syncDirectory(p string) error { return durable.Dir(p) }
