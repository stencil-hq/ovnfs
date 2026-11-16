package overlay

import (
	"os"
	"path/filepath"
	"time"
)

// lookupRead checks only the requested path and ancestor barriers while cold.
// A native stat follows intermediate symlinks, so inspect the ancestors first.
func (f *FS) lookupRead(p string) (source, os.FileInfo, error) {
	if f.index != nil || f.upper == "" || p == "" || reserved(p) {
		return f.lookup(p)
	}
	blocked := false
	for end := 0; end <= len(p); end++ {
		if end > 0 && end < len(p) && p[end] != '/' {
			continue
		}
		q := p[:end]
		real := f.up(q)
		info, err := hostLstat(real)
		if err == nil {
			if q == p {
				info, err = withMode(real, info)
				return source{upper: true}, info, err
			}
			if !info.IsDir() {
				return source{}, nil, notExist("lstat", p)
			}
			if _, err = os.Lstat(filepath.Join(real, opaqueName)); err == nil {
				blocked = true
			} else if !missing(err) {
				return source{}, nil, err
			}
		} else if !missing(err) {
			return source{}, nil, err
		} else {
			if q != "" {
				if _, err = os.Lstat(f.whiteoutPath(q)); err == nil {
					return source{}, nil, notExist("lstat", p)
				} else if !missing(err) {
					return source{}, nil, err
				}
			}
			break // descendants of a missing upper directory are missing too
		}
	}
	if blocked {
		return source{}, nil, notExist("lstat", p)
	}
	return f.lookup(p)
}

// loadIndex is called under mu exclusively, never for a cold directory listing.
func (f *FS) loadIndex() error {
	if f.upper == "" {
		return nil
	}
	f.lastAccess.Store(time.Now().UnixNano())
	if f.index != nil {
		return nil
	}
	index, err := indexLayer(f.upper, true)
	if err != nil {
		return err
	}
	index.addOrigins(f.origins)
	f.index = index
	f.pathIDs = make(map[string]inodeKey)
	f.aliases = make(map[inodeKey]inodeNames)
	for p, e := range index.entries {
		if err := f.track(p, e.Info); err != nil {
			f.index, f.pathIDs, f.aliases = nil, nil, nil
			return err
		}
	}
	f.origins = nil
	return nil
}

// Cool drops reconstructible metadata only. Handles and staged or pending writes
// pin it; no file, checkout generation or exported identity is changed.
func (f *FS) Cool(before time.Time) {
	if !f.staging.TryLock() {
		return
	}
	defer f.staging.Unlock()
	if !f.mu.TryLock() {
		return
	}
	defer f.mu.Unlock()
	if f.index == nil || f.upper == "" || f.lastAccess.Load() >= before.UnixNano() ||
		len(f.hosts) != 0 || len(f.files) != 0 || f.memoryHandles != 0 {
		return
	}
	f.rootInfo, f.origins = f.index.entries[""].Info, f.index.origins()
	f.index, f.pathIDs, f.aliases = nil, nil, nil
}
