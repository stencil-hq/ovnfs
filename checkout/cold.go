package checkout

import (
	"time"

	"github.com/stencil-hq/ovnfs/overlay"
)

// upperIndexIdle keeps recently used metadata warm while not pinning the upper
// trees of every saved checkout for the daemon's entire lifetime.
const upperIndexIdle = time.Minute

// coolCheckouts drops only reconstructible, unused upper indices. It does not
// retire checkouts or invalidate handles. Registry.mu only protects collecting
// the filesystem pointers; cooling never waits on a checkout under that lock.
func (r *Registry) coolCheckouts() {
	defer r.workers.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var filesystems []*overlay.FS
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
		}
		filesystems = filesystems[:0]
		r.mu.RLock()
		for _, h := range r.checkouts {
			if !h.retired.Load() && h.fs.Indexed() {
				filesystems = append(filesystems, h.fs)
			}
		}
		r.mu.RUnlock()
		cutoff := time.Now().Add(-upperIndexIdle)
		for i, fs := range filesystems {
			fs.Cool(cutoff)
			filesystems[i] = nil
		}
	}
}
