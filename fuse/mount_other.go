//go:build !linux

package fuse

import (
	"fmt"
	"runtime"

	"github.com/stencil-hq/ovnfs/export"
)

// Server is an active FUSE mount; FUSE is served only on Linux.
type Server struct{}

// Mount reports that FUSE serving is unsupported on this OS.
func Mount(*export.Export, string, Options) (*Server, error) {
	return nil, fmt.Errorf("fuse: unsupported on %s", runtime.GOOS)
}

// Unmount reports that no mount can exist on this OS.
func (*Server) Unmount(bool) error { return fmt.Errorf("fuse: unsupported on %s", runtime.GOOS) }

// Wait returns at once because no mount can exist on this OS.
func (*Server) Wait() {}

// Invalidate does nothing because no mount can exist on this OS.
func (*Server) Invalidate(string, []string, []string) {}
