package overlay

import (
	"os"
	"path"
)

// inodeKey identifies one host inode so hardlinked paths share metadata updates.
type inodeKey struct{ device, inode uint64 }

// inodeNames keeps the common one- and two-link cases inline. A map is only
// needed for the less common groups with three or more names.
type inodeNames struct {
	first  string
	second string
	others map[string]struct{}
}

// addIdentity records a name without allocating a map for a hard-link pair.
func (f *FS) addIdentity(p string, key inodeKey) {
	names := f.aliases[key]
	if names.first == "" {
		names.first = p
	} else if names.first != p && names.second != p {
		if names.second == "" {
			names.second = p
		} else {
			if names.others == nil {
				names.others = make(map[string]struct{})
			}
			names.others[p] = struct{}{}
		}
	}
	f.aliases[key] = names
	if f.pathIDs != nil {
		f.pathIDs[p] = key
	}
}

// indexedIdentity uses the Unix stat already owned by the index. Windows
// retains its cache because resolving a file id there needs a host handle.
func (f *FS) indexedIdentity(p string) (inodeKey, bool) {
	if f.pathIDs != nil {
		key, ok := f.pathIDs[p]
		return key, ok
	}
	e, ok := f.index.entries[p]
	if !ok || e.Info == nil || e.Info.IsDir() {
		return inodeKey{}, false
	}
	key, err := identity(f.upper, p, e.Info)
	return key, err == nil
}

// track records one upper name's inode without reading or changing its metadata.
func (f *FS) track(p string, info os.FileInfo) error {
	if info == nil {
		return nil
	}
	if err := primeInfo(info); err != nil {
		return err
	}
	if info.IsDir() {
		return nil
	}
	key, err := identity(f.upper, p, info)
	if err != nil {
		return err
	}
	if old, ok := f.indexedIdentity(p); ok {
		if old == key {
			names := f.aliases[key]
			if names.first == p || names.second == p {
				return nil
			}
			if _, exists := names.others[p]; exists {
				return nil
			}
		}
		f.untrack(p)
	}
	f.addIdentity(p, key)
	return nil
}

// untrack retires one upper name without retaining dead inode groups.
func (f *FS) untrack(p string) {
	key, ok := f.indexedIdentity(p)
	if !ok {
		return
	}
	delete(f.pathIDs, p)
	names := f.aliases[key]
	if names.first == p {
		names.first, names.second = names.second, ""
	} else if names.second == p {
		names.second = ""
	} else {
		delete(names.others, p)
	}
	if names.second == "" {
		for other := range names.others {
			if names.first == "" {
				names.first = other
			} else {
				names.second = other
			}
			delete(names.others, other)
			break
		}
	}
	if names.first == "" {
		delete(f.aliases, key)
	} else {
		if len(names.others) == 0 {
			names.others = nil
		}
		f.aliases[key] = names
	}
}

// dropIndex removes a subtree and all its hardlink bookkeeping together.
func (f *FS) dropIndex(p string) {
	for _, child := range f.index.children[p] {
		f.dropIndex(child)
	}
	if m := f.files[p]; m != nil {
		f.detach(m)
	}
	f.untrack(p)
	f.index.drop(p)
}

// otherAlias finds a surviving name whose link count changes when p is removed.
func (f *FS) otherAlias(p string) string {
	key, ok := f.indexedIdentity(p)
	if !ok {
		return ""
	}
	names := f.aliases[key]
	if names.first != p {
		return names.first
	}
	if names.second != "" {
		return names.second
	}
	for other := range names.others {
		return other
	}
	return ""
}

// moveIdentity temporarily registers a renamed path before the old name retires.
func (f *FS) moveIdentity(from, to string) {
	if key, ok := f.indexedIdentity(from); ok {
		f.addIdentity(to, key)
	}
}

// restat re-reads p's metadata after a descriptor changed the inode's content
// or attributes and shares it with p's other hard-link names; the inode, and
// so the identity bookkeeping, is unchanged.
func (f *FS) restat(p string) error {
	info, err := hostLstat(f.up(p))
	if err != nil {
		return err
	}
	if err := primeInfo(info); err != nil {
		return err
	}
	f.setInfo(p, info)
	f.cacheAliases(p, info)
	return nil
}

// setInfo indexes upper name p's metadata, keeping its opacity.
func (f *FS) setInfo(p string, info os.FileInfo) {
	e := entry(retainInfo(p, info))
	e.Opaque = f.index.entries[p].Opaque
	f.index.set(p, e)
}

// cacheInfo stores a fresh snapshot and shares it with other names of the same inode.
func (f *FS) cacheInfo(p string, info os.FileInfo) error {
	// Track before replacing the old stat so inode replacement retires its old group.
	if err := f.track(p, info); err != nil {
		return err
	}
	f.setInfo(p, info)
	f.cacheAliases(p, info)
	return nil
}

// cacheAliases gives p's other hard-link names info, p's fresh metadata. The
// names share one inode, so only the name differs; re-reading each would cost
// a host lstat per link on every metadata change.
func (f *FS) cacheAliases(p string, info os.FileInfo) {
	key, ok := f.indexedIdentity(p)
	if !ok {
		return
	}
	names := f.aliases[key]
	if names.first != p {
		f.setInfo(names.first, aliasInfo{FileInfo: info, name: path.Base(names.first)})
	}
	if names.second != "" && names.second != p {
		f.setInfo(names.second, aliasInfo{FileInfo: info, name: path.Base(names.second)})
	}
	for other := range names.others {
		if other != p {
			f.setInfo(other, aliasInfo{FileInfo: info, name: path.Base(other)})
		}
	}
}

// aliasInfo is a hard link's metadata read through another of its names.
type aliasInfo struct {
	os.FileInfo
	name string
}

// Name returns the alias's own base name.
func (i aliasInfo) Name() string { return i.name }

// unalias returns the host metadata beneath an [aliasInfo], whose own type
// [os.SameFile] requires.
func unalias(info os.FileInfo) os.FileInfo {
	if a, ok := info.(aliasInfo); ok {
		return a.FileInfo
	}
	return info
}

// bind associates an open descriptor with its inode, not its current name; a
// writable one makes the inode's indexed metadata volatile until it closes.
func (f *FS) bind(h *file, p string) {
	h.inode, h.tracked = f.indexedIdentity(p)
	if h.writable && h.tracked {
		f.writers[h.inode]++
	}
}
