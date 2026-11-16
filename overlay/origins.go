package overlay

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// originsName keeps transient rename hints beside a raw frozen layer.
const originsName = metaPrefix + "origins"

// saveOrigins records hints before Freeze transfers ownership of the upper.
func (l *Layer) saveOrigins(dir string) (bool, error) {
	if len(l.origins) == 0 {
		return false, nil
	}
	data, err := json.Marshal(l.origins)
	if err != nil {
		return false, err
	}
	tmp := filepath.Join(dir, originsName+".tmp")
	if err = os.WriteFile(tmp, data, 0o600); err == nil {
		err = os.Rename(tmp, filepath.Join(dir, originsName))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err == nil, err
}

// loadOrigins restores optional optimization hints, never paths used for host
// I/O: an unreadable file or a hint that no longer fits its entry is dropped,
// costing at most a delta base, never the layer.
func (l *Layer) loadOrigins() error {
	data, err := os.ReadFile(filepath.Join(l.dir, originsName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var origins map[string]string
	if json.Unmarshal(data, &origins) != nil {
		return nil
	}
	for p, origin := range origins {
		if e, ok := l.entries[p]; ok && validOrigin(e.Kind, origin) {
			e.Origin = origin
			l.set(p, e)
		}
	}
	return nil
}

// validOrigin reports whether origin can hint the delta base of an entry of kind.
func validOrigin(kind Kind, origin string) bool {
	return (kind == KindFile || kind == KindSymlink) && origin != "" && origin == Clean(origin) && !reserved(origin)
}
