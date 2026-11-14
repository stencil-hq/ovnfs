package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// getProcessMemoryInfo is psapi's GetProcessMemoryInfo as kernel32 exports it.
var getProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMemoryCountersEx is PROCESS_MEMORY_COUNTERS_EX.
type processMemoryCountersEx struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

// footprint returns the process's private bytes, the commit charge Windows
// bills it; mapped files do not count. It is 0 when the call fails.
func footprint() int64 {
	c := processMemoryCountersEx{cb: uint32(unsafe.Sizeof(processMemoryCountersEx{}))}
	r, _, _ := getProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		return 0
	}
	return int64(c.privateUsage)
}
