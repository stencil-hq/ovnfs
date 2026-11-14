package main

import (
	"runtime/debug"
	"runtime/metrics"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// An idle daemon returns its garbage and free heap to the OS: every
// idleCheck, one that allocated less than idleAlloc since the previous check
// and holds more than idleExcess of heap beyond its live heap collects and
// releases it, so its footprint falls back to its live heap after a burst.
const (
	idleCheck  = 10 * time.Second
	idleAlloc  = 16 << 20
	idleExcess = 16 << 20
)

// heapMetrics are the runtime/metrics samples readHeap takes, in heapSample order.
var heapMetrics = [...]string{
	"/gc/heap/live:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/total:bytes",
	"/memory/classes/heap/released:bytes",
	"/gc/heap/allocs:bytes",
}

// heapSample is the Go runtime's memory as memory policy and status see it.
type heapSample struct {
	live   uint64 // heap the last collection found reachable
	inUse  uint64 // heap spans holding objects: live, garbage and fragmentation
	free   uint64 // free heap still backed by memory, not yet returned to the OS
	held   uint64 // all memory the runtime holds from the OS, heap or not, minus what it returned
	allocs uint64 // cumulative heap allocation
}

// readHeap samples the runtime's memory statistics without stopping the world.
func readHeap() heapSample {
	var s [len(heapMetrics)]metrics.Sample
	for i, name := range heapMetrics {
		s[i].Name = name
	}
	metrics.Read(s[:])
	v := func(i int) uint64 { return s[i].Value.Uint64() }
	return heapSample{live: v(0), inUse: v(1) + v(2), free: v(3), held: v(4) - v(5), allocs: v(6)}
}

// reportHeap fills the status fields that make the daemon's memory visible
// without a profile.
func reportHeap(s *api.Status) {
	h := readHeap()
	s.HeapLive, s.HeapInUse, s.GoHeld = int64(h.live), int64(h.inUse), int64(h.held)
	s.Footprint = footprint()
}

// releaseIdle returns the heap beyond the live heap to the OS whenever the
// daemon has gone quiet, so its footprint falls back to its live heap after a
// burst instead of keeping GOGC's headroom; serve runs it for the life of the
// process.
func releaseIdle() {
	last := readHeap().allocs
	for range time.Tick(idleCheck) {
		h := readHeap()
		quiet := h.allocs-last < idleAlloc
		last = h.allocs
		if quiet && h.inUse+h.free > h.live+idleExcess {
			debug.FreeOSMemory()
			last = readHeap().allocs
		}
	}
}
