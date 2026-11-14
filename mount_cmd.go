//go:build !windows

package main

import (
	"net"
	"net/url"
	"os"
	"runtime"
	"strconv"
)

// darwinNFSOptions keeps locking local and the mount out of Finder.
const darwinNFSOptions = "locallocks,nobrowse,intr"

// mountCmd builds the native client command for the localhost server at addr.
// NFS mounts use v3 with client-local locks and normal attribute caching;
// refreshRoot makes checkout additions and removals visible immediately.
// A Unix socket address is an NFS socket for macOS's mount_nfs, which takes
// a path as port (netid ticotsord; undocumented, see Apple's mount_nfs.c).
// settings controls generic mount flags and NFS I/O buffer sizes.
// SMB uses NTLM credentials when configured, otherwise an unsigned guest.
// Linux cifs mounts belong to the daemon's user: the kernel gives a file it
// has just created the mount's owner and file_mode rather than the POSIX
// reply's, so a root-owned mount refuses `>` redirects into new files.
func mountCmd(proto string, addr net.Addr, dir string, settings mountSettings, share, user, password string) []string {
	extra := atimeOption(settings.Atime)
	if sock, ok := addr.(*net.UnixAddr); ok {
		o := "vers=3,port=" + sock.Name + ",mountport=" + sock.Name + "," + darwinNFSOptions + ",rwsize=" + strconv.Itoa(settings.nfsIOSize()) + "," + extra
		return []string{"mount_nfs", "-o", o, "<" + sock.Name + ">:/", dir}
	}
	p := strconv.Itoa(addr.(*net.TCPAddr).Port)
	if proto == "smb" {
		if runtime.GOOS == "darwin" {
			u := url.User("guest").String()
			if user != "" {
				u = url.UserPassword(user, password).String()
			}
			o := "nobrowse," + extra
			return []string{"mount_smbfs", "-N", "-o", o, "//" + u + "@127.0.0.1:" + p + "/" + url.PathEscape(share), dir}
		}
		o := "port=" + p + ",vers=3.1.1,"
		if user == "" {
			o += "guest"
		} else {
			o += "username=" + user + ",password=" + password
		}
		o += ",posix,cache=strict,actimeo=1,uid=" + strconv.Itoa(os.Geteuid()) + ",gid=" + strconv.Itoa(os.Getegid())
		o += "," + extra
		return []string{"mount", "-t", "cifs", "//127.0.0.1/" + share, dir, "-o", o}
	}
	size := strconv.Itoa(settings.nfsIOSize())
	var o string
	var cmd []string
	if runtime.GOOS == "darwin" {
		o = "vers=3,tcp,port=" + p + ",mountport=" + p + ",noresvport," + darwinNFSOptions + ",rwsize=" + size
		cmd = []string{"mount_nfs"}
	} else {
		o = "vers=3,tcp,port=" + p + ",mountport=" + p + ",nolock,rsize=" + size + ",wsize=" + size
		cmd = []string{"mount", "-t", "nfs"}
	}
	o += "," + extra
	return append(cmd, "-o", o, "127.0.0.1:/", dir)
}

// atimeOption is the generic mount option for the access-time setting (serve
// --atime). Off by default: with access times, the macOS NFS client sends a
// SETATTR for every exec of a checkout binary.
func atimeOption(atime bool) string {
	if atime {
		return "atime"
	}
	return "noatime"
}
