//go:build windows

package main

import (
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// smbRemote is the UNC path serve maps the mount drive to; drives mapped
// anywhere else are never treated as ours.
const smbRemote = `\\127.0.0.1\ovnfs`

// mountRoots are the names Windows tools give the mount's root: its drive and,
// once they resolve a path, the share mapDrive maps it to.
func mountRoots(mount string) []string {
	return []string{filepath.VolumeName(mount) + "/", filepath.ToSlash(smbRemote) + "/"}
}

// The network provider (mpr) and SSPI (sspicli) calls that map drives load on first use.
var (
	mpr     = windows.NewLazySystemDLL("mpr.dll")
	sspicli = windows.NewLazySystemDLL("sspicli.dll")
	// procWNetGetConnection reads a drive's network mapping without spawning net use.
	procWNetGetConnection = mpr.NewProc("WNetGetConnectionW")
	// procWNetAddConnection4 maps a drive with credentials and use options, as net use does.
	procWNetAddConnection4 = mpr.NewProc("WNetAddConnection4W")
	// The SSPI calls pack credentials into the identity WNetAddConnection4W takes.
	procSspiEncodeStringsAsAuthIdentity = sspicli.NewProc("SspiEncodeStringsAsAuthIdentity")
	procSspiMarshalAuthIdentity         = sspicli.NewProc("SspiMarshalAuthIdentity")
	procSspiFreeAuthIdentity            = sspicli.NewProc("SspiFreeAuthIdentity")
	procSspiLocalFree                   = sspicli.NewProc("SspiLocalFree")
)

// netResource is winnetwk.h's NETRESOURCEW.
type netResource struct {
	scope, resourceType, displayType, usage  uint32
	localName, remoteName, comment, provider *uint16
}

// useOptionTransport is LMUse.h's USE_OPTION_TRANSPORT_PARAMETERS header
// followed by its TRANSPORT_INFO, the use option net use /TCPPORT passes
// (Windows SDK 10.0.26100, um/LMUse.h, as mirrored in microsoft/win32metadata
// generation/WinSDK/RecompiledIdlHeaders/um/LMUse.h).
type useOptionTransport struct {
	tag      uint32 // USE_OPTION_TRANSPORT_PARAMS
	length   uint16 // the header and TRANSPORT_INFO
	reserved uint16
	// TRANSPORT_INFO
	transportType        uint32 // UseTransportType_Wsk is TCP
	skipCertificateCheck bool
	tcpPort              uint16
	quicPort             uint16
	rdmaPort             uint16
	flags                uint32 // TcpPortSetFlag
}

// LMUse.h and winnetwk.h values mapDrive passes.
const (
	// useOptionTransportParams is 'ParT', a multi-character constant MSVC
	// evaluates big-endian ('P' is the most significant byte).
	useOptionTransportParams = 0x50617254
	useTransportTypeWsk      = 1
	tcpPortSetFlag           = 1
	resourceTypeDisk         = 1
	connectTemporary         = 4
)

// mountServer maps mount's drive to the daemon's SMB share as user; Windows has
// no NFS client mount, and a mapped drive has no access-time option.
func mountServer(proto string, addr net.Addr, mount, user, password string, _ mountSettings) error {
	if proto != "smb" {
		return fmt.Errorf("NFS mounts unsupported on Windows; use --proto smb")
	}
	return mapDrive(filepath.VolumeName(mount), smbRemote, addr.(*net.TCPAddr).Port, user, password)
}

// mapDrive maps drive (such as "O:") to remote for this logon session as
// user. The password goes to the redirector in a marshalled SSPI identity,
// never on a command line as net use would take it. A port other than 445
// needs Windows 11 24H2 or later (smbClientPort).
func mapDrive(drive, remote string, port int, user, password string) error {
	for _, p := range []*windows.LazyProc{procWNetAddConnection4, procSspiEncodeStringsAsAuthIdentity, procSspiMarshalAuthIdentity, procSspiFreeAuthIdentity, procSspiLocalFree} {
		if err := p.Find(); err != nil {
			return err
		}
	}
	userName, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	secret, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	var identity uintptr
	if r, _, _ := procSspiEncodeStringsAsAuthIdentity.Call(uintptr(unsafe.Pointer(userName)), 0, uintptr(unsafe.Pointer(secret)), uintptr(unsafe.Pointer(&identity))); r != 0 {
		return fmt.Errorf("SspiEncodeStringsAsAuthIdentity: %w", syscall.Errno(r))
	}
	defer procSspiFreeAuthIdentity.Call(identity)
	var authLen uint32
	var auth *byte
	if r, _, _ := procSspiMarshalAuthIdentity.Call(identity, uintptr(unsafe.Pointer(&authLen)), uintptr(unsafe.Pointer(&auth))); r != 0 {
		return fmt.Errorf("SspiMarshalAuthIdentity: %w", syscall.Errno(r))
	}
	defer procSspiLocalFree.Call(uintptr(unsafe.Pointer(auth)))
	local, err := windows.UTF16PtrFromString(drive)
	if err != nil {
		return err
	}
	remoteName, err := windows.UTF16PtrFromString(remote)
	if err != nil {
		return err
	}
	res := netResource{resourceType: resourceTypeDisk, localName: local, remoteName: remoteName}
	var options *useOptionTransport
	var optionsLen uintptr
	if port != 445 {
		options = &useOptionTransport{tag: useOptionTransportParams, transportType: useTransportTypeWsk, tcpPort: uint16(port), flags: tcpPortSetFlag}
		optionsLen = unsafe.Sizeof(*options)
		options.length = uint16(optionsLen)
	}
	r, _, _ := procWNetAddConnection4.Call(0, uintptr(unsafe.Pointer(&res)), uintptr(unsafe.Pointer(auth)), uintptr(authLen),
		connectTemporary, uintptr(unsafe.Pointer(options)), optionsLen)
	switch e := syscall.Errno(r); e {
	case windows.NO_ERROR:
		return nil
	case windows.ERROR_SESSION_CREDENTIAL_CONFLICT:
		return fmt.Errorf("mapping %s to %s: %w (another daemon or mapping in this logon session holds a connection to the same server; Windows allows one set of credentials per server)", drive, remote, e)
	default:
		return fmt.Errorf("mapping %s to %s: %w", drive, remote, e)
	}
}

// matchMount has no tunable options: mounted already checks the SMB share,
// and listenAndMount binds the persisted port this mapping dials.
func matchMount(string, net.Addr, string, mountSettings) error { return nil }

// unmount removes this daemon's mapped SMB drive holding dir, suppressing the interactive prompt.
func unmount(dir string, _ bool) error {
	if !mounted(dir) {
		return nil
	}
	out, err := exec.Command("net", "use", filepath.VolumeName(dir), "/delete", "/y").CombinedOutput()
	if err != nil {
		return fmt.Errorf("net use /delete: %w: %s", err, out)
	}
	return nil
}

// mounted reports whether dir's drive is mapped to this daemon's share.
func mounted(dir string) bool {
	remote, err := driveRemote(filepath.VolumeName(dir))
	return err == nil && strings.EqualFold(remote, smbRemote)
}

// makeMountPoint checks that mapDrive or WinFsp can take dir: it fails when dir
// is not a drive root, the drive is mapped to a share (connected or only
// remembered), or a volume holds the letter.
func makeMountPoint(dir string) error {
	drive := filepath.VolumeName(dir)
	if len(drive) != 2 || drive[1] != ':' || dir != drive+`\` {
		return fmt.Errorf("%s is not a drive root: checkouts on Windows mount on a free drive letter such as O:", dir)
	}
	remote, err := driveRemote(drive)
	if err != nil {
		return fmt.Errorf("reading drive %s mapping: %w", drive, err)
	}
	if remote != "" {
		return fmt.Errorf("drive %s is mapped to %s; remove it with \"net use %s /delete\" or choose another drive with --mount", drive, remote, drive)
	}
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}
	if drives&(1<<(strings.ToUpper(drive)[0]-'A')) != 0 {
		return fmt.Errorf("drive %s holds a local volume; choose another drive with --mount", drive)
	}
	return nil
}

// driveRemote returns the network path a drive such as "O:" is mapped to, or ""
// when it has none. A remembered mapping that is not connected still counts,
// since a new mapping cannot replace it.
func driveRemote(drive string) (string, error) {
	if err := procWNetGetConnection.Find(); err != nil {
		return "", err
	}
	local, err := windows.UTF16PtrFromString(drive)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n := uint32(len(buf))
		r, _, _ := procWNetGetConnection.Call(uintptr(unsafe.Pointer(local)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
		switch e := syscall.Errno(r); e {
		case windows.NO_ERROR, windows.ERROR_CONNECTION_UNAVAIL:
			return windows.UTF16ToString(buf), nil
		case windows.ERROR_NOT_CONNECTED:
			return "", nil
		case windows.ERROR_MORE_DATA:
			buf = make([]uint16, n)
		default:
			return "", e
		}
	}
}

// smbClientPort is the only port the SMB client dials on this Windows build:
// 445 before Windows 11 24H2 (build 26100) added the transport use option
// (net use /TCPPORT), otherwise 0 (any port).
func smbClientPort() int {
	if windows.RtlGetVersion().BuildNumber < 26100 {
		return 445
	}
	return 0
}
