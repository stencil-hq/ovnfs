//go:build !linux && !windows && !(darwin && cgo)

package main

// footprint reports 0: this build has no cheap way to ask the OS.
func footprint() int64 { return 0 }
