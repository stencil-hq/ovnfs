package overlay

import "time"

// lockRead returns with mu held shared. A cold upper is loaded exclusively,
// then checked again under the shared lock so cooling cannot race that gap.
// Root attributes do not need the index: listing the mount must not warm every
// saved checkout. An error leaves mu unlocked.
func (f *FS) lockRead(needsIndex bool) error {
	for {
		f.mu.RLock()
		if !needsIndex || f.upper == "" || f.index != nil {
			if needsIndex && f.index != nil {
				f.lastAccess.Store(time.Now().UnixNano())
			}
			return nil
		}
		f.mu.RUnlock()
		f.mu.Lock()
		err := f.loadIndex()
		f.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// loadIndex restores the derived metadata and hardlink groups, without changing
// the upper's files or the export's identities. The caller holds mu exclusively.
func (f *FS) loadIndex() error {
	if f.upper == "" || f.index != nil {
		return nil
	}
	index, err := indexLayer(f.upper, true)
	if err != nil {
		return err
	}
	f.restoreOrigins(index)
	f.index = index
	if cachePathIDs {
		f.pathIDs = make(map[string]inodeKey)
	}
	f.aliases = make(map[inodeKey]inodeNames)
	for p, e := range index.entries {
		if err := f.track(p, e.Info); err != nil {
			f.keepOrigins(index)
			f.index, f.pathIDs, f.aliases = nil, nil, nil
			return err
		}
	}
	f.indexed.Store(true)
	return nil
}

// keepOrigins retains only rename hints when the rest of an index is dropped.
// Non-nil, including an empty map, means these hints supersede the disk copy.
func (f *FS) keepOrigins(index *Layer) {
	f.savedOrigins = index.origins
	if f.savedOrigins == nil {
		f.savedOrigins = make(map[string]string)
	}
}

// restoreOrigins reinstates the current hints rather than an older disk copy.
// They are sparse, and directory listings reference entries by canonical path,
// so neither restoring nor clearing one needs to rebuild the child index.
func (f *FS) restoreOrigins(index *Layer) {
	if f.savedOrigins == nil {
		return
	}
	for p := range index.origins {
		e := index.entries[p]
		e.Origin = ""
		index.entries[p] = e
	}
	index.origins = f.savedOrigins
	for p, origin := range index.origins {
		e, ok := index.entries[p]
		if !ok || !validOrigin(e.Kind, origin) {
			delete(index.origins, p)
			continue
		}
		e.Origin = origin
		index.entries[p] = e
	}
	f.savedOrigins = nil
}

// Indexed reports whether the writable upper index is resident. It does not
// wait for a cold load or keep an otherwise idle index warm.
func (f *FS) Indexed() bool { return f.indexed.Load() }

// Cool releases a derived upper index last used before cutoff. It never waits
// for active I/O, writes files, or changes export generations. Open disk and
// memory handles, unflushed data and staged I/O pin the index: their later
// metadata updates must still reach the same live index.
func (f *FS) Cool(cutoff time.Time) bool {
	if !f.indexed.Load() {
		return false
	}
	if !f.staging.TryLock() {
		return false
	}
	defer f.staging.Unlock()
	if !f.mu.TryLock() {
		return false
	}
	defer f.mu.Unlock()
	if f.upper == "" || f.index == nil || f.lastAccess.Load() >= cutoff.UnixNano() ||
		len(f.hosts) != 0 || len(f.files) != 0 || len(f.writers) != 0 || f.memoryHandles != 0 {
		return false
	}
	f.rootInfo = f.index.entries[""].Info
	f.keepOrigins(f.index)
	f.index, f.pathIDs, f.aliases = nil, nil, nil
	f.indexed.Store(false)
	return true
}
