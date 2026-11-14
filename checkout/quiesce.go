package checkout

import (
	"context"
	"time"
)

// Quiesce drains client write caches before a direct filesystem operation. Its
// release must be held through that operation, even after ctx expires. The
// export's counted grant gate permits concurrent and nested acquisitions; this
// wrapper must never serialize them or acquire registry/freeze locks, because
// the client's WRITE and FLUSH requests still need Enter while draining.
//
// Acquisition is bounded to five seconds or the caller's earlier deadline. A
// nil Config.Quiesce is safe only for exports that never grant write caching.
func (r *Registry) Quiesce(ctx context.Context) (func(), error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.config.Quiesce == nil {
		return func() {}, nil
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	release, err := r.config.Quiesce(wait)
	if err == nil {
		err = wait.Err()
	}
	if err != nil {
		if release != nil {
			release()
		}
		return nil, coded(err)
	}
	if release == nil {
		return nil, failure("io", "write-cache quiesce returned no release")
	}
	return release, nil
}
