package mapped

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	want := bytes.Repeat([]byte("mapped "), 100000)
	os.WriteFile(path, want, 0o644)
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, nil, 0o644)
	for _, on := range []bool{true, false} {
		Enabled = on
		for p, w := range map[string][]byte{path: want, empty: {}} {
			data, unmap, err := Map(p)
			if err != nil || !bytes.Equal(data, w) {
				t.Errorf("Map(%s) mapped=%v: %d bytes, %v", p, on, len(data), err)
				continue
			}
			if err := unmap(); err != nil {
				t.Errorf("unmap: %v", err)
			}
		}
	}
	Enabled = true
	if _, _, err := Map(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Map(missing) error %v", err)
	}
}
