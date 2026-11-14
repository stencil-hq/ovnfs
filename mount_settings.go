package main

import "fmt"

const defaultNFSIOSize = 128 << 10

// mountSettings controls native client mounts. A zero NFSIOSize uses the default.
type mountSettings struct {
	Atime     bool
	NFSIOSize int
}

func (s mountSettings) nfsIOSize() int {
	if s.NFSIOSize == 0 {
		return defaultNFSIOSize
	}
	return s.NFSIOSize
}

func (s mountSettings) validate() error {
	n := s.nfsIOSize()
	if n < 4<<10 || n > 1<<20 || n&(n-1) != 0 {
		return fmt.Errorf("NFS I/O size must be a power of two from 4 KiB to 1 MiB, got %d bytes", n)
	}
	return nil
}
