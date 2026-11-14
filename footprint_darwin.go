//go:build darwin && cgo

package main

/*
#include <libproc.h>
#include <unistd.h>

static unsigned long long phys_footprint(void) {
	struct rusage_info_v2 ri;
	if (proc_pid_rusage(getpid(), RUSAGE_INFO_V2, (rusage_info_t *)&ri) != 0) {
		return 0;
	}
	return ri.ri_phys_footprint;
}
*/
import "C"

// footprint returns the process's phys_footprint, the memory macOS charges
// it (Activity Monitor's "Memory"): dirty private pages, compressed or not;
// clean mapped files do not count.
func footprint() int64 { return int64(C.phys_footprint()) }
