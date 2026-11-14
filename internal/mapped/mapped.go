// Package mapped reads files read-only through memory maps, so large on-disk
// indexes cost only the pages a lookup touches, and clean mapped pages stay
// out of the process's dirty footprint. Where mapping is unavailable or fails,
// [File] reads through ReadAt and [Map] reads the whole file.
package mapped

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
)

// File is a read-only file, memory-mapped when the platform allows and read
// through ReadAt otherwise. Views returned by Slice and Bytes must not
// outlive Close.
type File struct {
	data   []byte   // the whole file, when mapped or wrapped
	file   *os.File // the open file, when not mapped
	size   int64
	mapped bool // data is a mapping Close must unmap
}

// Enabled turns mapping on; tests turn it off to exercise the ReadAt fallback.
var Enabled = true

// Map returns the contents of the file at path read-only, memory-mapped
// where the platform allows so large files cost only the pages touched, and
// read into memory otherwise. The data must not be modified, nor used after
// unmap is called.
func Map(path string) (data []byte, unmap func() error, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if size := fi.Size(); Enabled && size > 0 && size <= math.MaxInt {
		if data, err := mmap(f, int(size)); err == nil {
			return data, func() error { return munmap(data) }, nil
		}
	}
	data, err = io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}

// Open opens the file at path, mapping it when [Enabled] and the platform allow.
func Open(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := fi.Size()
	if Enabled && size > 0 && size <= math.MaxInt {
		if data, err := mmap(f, int(size)); err == nil {
			// The mapping outlives the descriptor; closing it now keeps
			// descriptor use flat however many files stay open.
			f.Close()
			return &File{data: data, size: size, mapped: true}, nil
		}
	}
	return &File{file: f, size: size}, nil
}

// Bytes wraps data already in memory as a File; Close is then a no-op that
// leaves data alone.
func Bytes(data []byte) *File { return &File{data: data, size: int64(len(data))} }

// Size is the file's length in bytes.
func (m *File) Size() int64 { return m.size }

// Mapped returns the whole file when it is mapped (or wraps bytes), nil when
// reads go through ReadAt.
func (m *File) Mapped() []byte { return m.data }

// ReadAt implements io.ReaderAt.
func (m *File) ReadAt(p []byte, off int64) (int, error) {
	if m.data == nil {
		return m.file.ReadAt(p, off)
	}
	if off < 0 || off >= m.size {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Slice returns the n bytes at off: a view of the mapping, or a copy read
// from the file.
func (m *File) Slice(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off+n > m.size {
		return nil, fmt.Errorf("read of %d bytes at %d beyond end of %d-byte file", n, off, m.size)
	}
	if m.data != nil {
		return m.data[off : off+n], nil
	}
	b := make([]byte, n)
	if _, err := m.file.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}

// Reader returns a byte reader over the file from off to its end.
func (m *File) Reader(off int64) io.Reader {
	if m.data != nil {
		return bytes.NewReader(m.data[off:])
	}
	return bufio.NewReaderSize(io.NewSectionReader(m.file, off, m.size-off), 32<<10)
}

// Close unmaps the file or closes its descriptor.
func (m *File) Close() error {
	if m.file != nil {
		return m.file.Close()
	}
	if !m.mapped {
		return nil
	}
	data := m.data
	m.data, m.mapped = nil, false
	return munmap(data)
}
