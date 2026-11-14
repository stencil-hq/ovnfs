package main

import (
	"bytes"
	"os"
	"strconv"
)

// footprint returns the process's resident anonymous memory plus its swap
// (RssAnon and VmSwap in /proc/self/status), the Linux counterpart of the
// macOS footprint: clean mapped files do not count. It is 0 when unreadable.
func footprint() int64 {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	var total int64
	for line := range bytes.Lines(status) {
		for _, key := range []string{"RssAnon:", "VmSwap:"} {
			if rest, ok := bytes.CutPrefix(line, []byte(key)); ok {
				kb, _ := strconv.ParseInt(string(bytes.TrimSuffix(bytes.TrimSpace(rest), []byte(" kB"))), 10, 64)
				total += kb << 10
			}
		}
	}
	return total
}
