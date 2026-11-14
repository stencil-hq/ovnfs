//go:build unix

package mapped

import (
	"os"

	"golang.org/x/sys/unix"
)

func mmap(f *os.File, size int) ([]byte, error) {
	return unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
}

func munmap(b []byte) error { return unix.Munmap(b) }
