//go:build windows

package mapped

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mmap(f *os.File, size int) ([]byte, error) {
	h, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY,
		uint32(uint64(size)>>32), uint32(size), nil)
	if err != nil {
		return nil, err
	}
	// The view keeps the mapping alive after its handle is closed.
	defer windows.CloseHandle(h)
	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))), size), nil
}

func munmap(b []byte) error {
	return windows.UnmapViewOfFile(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
}
