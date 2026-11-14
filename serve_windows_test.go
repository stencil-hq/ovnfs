//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
	"golang.org/x/sys/windows"
)

// serverLog collects log lines for assertions, from the SMB server and the log package.
type serverLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// logf records one formatted line.
func (l *serverLog) logf(format string, args ...any) {
	fmt.Fprintf(l, format+"\n", args...)
}

// Write records raw log output.
func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// String returns everything recorded so far.
func (l *serverLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// freeMountDrive returns the last drive root makeMountPoint accepts.
func freeMountDrive(t *testing.T) string {
	t.Helper()
	for c := 'Z'; c >= 'D'; c-- {
		if dir := string(c) + `:\`; makeMountPoint(dir) == nil {
			return dir
		}
	}
	t.Skip("no free drive letter")
	return ""
}

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	totalUserTime, totalKernelTime, thisPeriodTotalUserTime, thisPeriodTotalKernelTime int64
	totalPageFaultCount, totalProcesses, activeProcesses, totalTerminatedProcesses     uint32
}

// processCounter puts the test process in a job object, which its children
// join, and returns a function counting the processes the job has held.
func processCounter(t *testing.T) func() uint32 {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(job) })
	if err = windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		t.Fatal(err)
	}
	return func() uint32 {
		var info jobAccounting
		if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
			t.Fatal(err)
		}
		return info.totalProcesses
	}
}

// TestSMBDriveMapping maps a free drive to the daemon's SMB server the way
// serve does: as the state directory's account, in a signed session, with no
// helper process that could show the password, which no log line holds either.
func TestSMBDriveMapping(t *testing.T) {
	if smbClientPort() != 0 {
		t.Skip("this SMB client dials only port 445")
	}
	lower := t.TempDir()
	if err := os.WriteFile(filepath.Join(lower, "file"), []byte("lower"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	reg, err := checkout.Open(checkout.Config{State: state, Mount: t.TempDir(), Memory: overlay.NewMemory(64<<20, 1<<20, 0), BlobCache: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	e, err := export.New(export.NewRouter(reg), filepath.Join(state, "nodes.log"), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if _, err = reg.New(context.Background(), api.NewArgs{Dir: lower, Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	drive := freeMountDrive(t)
	t.Cleanup(func() {
		if err := unmount(drive, true); err != nil {
			t.Error(err)
		}
	})
	logs := &serverLog{}
	defer log.SetOutput(log.Writer())
	log.SetOutput(logs)
	processes := processCounter(t)
	before := processes()
	served := make(chan error, 1)
	port, _, err := listenAndMount(reg, e, "smb", "127.0.0.1:0", state, drive, mountSettings{}, served, func(s *smb.Server) { s.Logf = logs.logf })
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, logs)
	}
	if n := processes() - before; n != 0 {
		t.Errorf("mounting started %d processes", n)
	}
	got, err := os.ReadFile(filepath.Join(drive, "alpha", "file"))
	if err != nil || string(got) != "lower" {
		t.Fatalf("read through %s = %q, %v; want %q\nlog:\n%s", drive, got, err, "lower", logs)
	}
	// The server refuses unsigned requests in a session that requires signing,
	// so the read above went through a signed session.
	session := fmt.Sprintf("user=%q guest=false anonymous=false signing_required=true", smbUser)
	if l := logs.String(); !strings.Contains(l, session) || strings.Contains(l, "guest=true") || strings.Contains(l, "anonymous=true") {
		t.Fatalf("want only %s sessions; log:\n%s", session, l)
	}

	passwordPath := filepath.Join(state, smbPasswordFile)
	b, err := os.ReadFile(passwordPath)
	if err != nil {
		t.Fatal(err)
	}
	password := string(b)
	if user, again, err := smbAccount(state); err != nil || user != smbUser || again != password {
		t.Errorf("a restart would use another account: %q, %v", user, err)
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(passwordPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	// SDDL abbreviates well-known SIDs (LA for the local Administrator), so
	// the wanted ACL is rendered the same way.
	want, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + tokenUser.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if sd.String() != want.String() {
		t.Errorf("password file ACL %s, want %s", sd, want)
	}

	if err = unmount(drive, true); err != nil {
		t.Fatal(err)
	}
	// A fresh wrong password each run: the client refuses a password a server
	// name has refused before without asking the server again.
	wrong := rand.Text()
	mapErr := mapDrive(filepath.VolumeName(drive), smbRemote, port, smbUser, wrong)
	if mapErr == nil {
		t.Fatal("mapped with a wrong password")
	}
	if !strings.Contains(logs.String(), "authentication: ") {
		t.Errorf("mapping failed without a refused authentication: %v; log:\n%s", mapErr, logs)
	}
	t.Logf("wrong password: %v", mapErr)
	if strings.Contains(logs.String(), password) || strings.Contains(mapErr.Error(), wrong) {
		t.Errorf("a password was logged; log:\n%s", logs)
	}
	t.Logf("log:\n%s", logs)
}
