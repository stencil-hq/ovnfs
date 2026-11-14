package nfs

import (
	"encoding/binary"
	"errors"
	"slices"
)

// errGarbage reports arguments that do not decode; the call is answered
// GARBAGE_ARGS.
var errGarbage = errors.New("nfs: undecodable arguments")

// decoder reads XDR (RFC 4506) items from a request. The first failure
// sticks: later reads return zero values and err reports it, so a procedure
// decodes all its arguments and checks err once.
//
// Variable-length items alias the request buffer, which is reused once the
// call is answered: copy anything kept longer (string conversion copies).
type decoder struct {
	buf []byte
	err error
}

// fail records a decoding failure and stops further reads.
func (d *decoder) fail() {
	d.err = errGarbage
	d.buf = nil
}

// u32 reads an unsigned int, enum or bool discriminant.
func (d *decoder) u32() uint32 {
	if len(d.buf) < 4 {
		d.fail()
		return 0
	}
	v := binary.BigEndian.Uint32(d.buf)
	d.buf = d.buf[4:]
	return v
}

// u64 reads an unsigned hyper.
func (d *decoder) u64() uint64 {
	if len(d.buf) < 8 {
		d.fail()
		return 0
	}
	v := binary.BigEndian.Uint64(d.buf)
	d.buf = d.buf[8:]
	return v
}

// bool reads a boolean; values other than 0 and 1 are garbage.
func (d *decoder) bool() bool {
	switch d.u32() {
	case 0:
		return false
	case 1:
		return true
	}
	d.fail()
	return false
}

// fixed reads fixed-length opaque data of n bytes and its padding.
func (d *decoder) fixed(n int) []byte {
	padded := n + pad(n)
	if n < 0 || len(d.buf) < padded {
		d.fail()
		return nil
	}
	v := d.buf[:n:n]
	d.buf = d.buf[padded:]
	return v
}

// opaque reads variable-length opaque data of at most limit bytes.
func (d *decoder) opaque(limit int) []byte {
	n := d.u32()
	if d.err != nil || n > uint32(limit) {
		d.fail()
		return nil
	}
	return d.fixed(int(n))
}

// str reads a string of at most limit bytes.
func (d *decoder) str(limit int) string { return string(d.opaque(limit)) }

// fh reads a file handle (nfs_fh3, nfs_fh4) of at most limit bytes and
// returns the file id it encodes; ok is false for a handle this server
// cannot have issued.
func (d *decoder) fh(limit int) (id uint64, ok bool) { return fileID(d.opaque(limit)) }

// encoder appends XDR items to a reply.
type encoder struct {
	buf []byte

	// NFSv3 reply state; nil for callback and test encoders.
	sizes                     *clientReportedSizes
	maxSizes                  *maxReportedSizes
	sizeID, size              uint64
	sizePending, sizeAccepted bool
}

// u32 appends an unsigned int, enum or bool discriminant.
func (e *encoder) u32(v uint32) { e.buf = binary.BigEndian.AppendUint32(e.buf, v) }

// u64 appends an unsigned hyper.
func (e *encoder) u64(v uint64) { e.buf = binary.BigEndian.AppendUint64(e.buf, v) }

// bool appends a boolean.
func (e *encoder) bool(v bool) {
	if v {
		e.u32(1)
	} else {
		e.u32(0)
	}
}

// fixed appends fixed-length opaque data and its padding.
func (e *encoder) fixed(b []byte) {
	e.buf = append(e.buf, b...)
	e.buf = append(e.buf, zeros[:pad(len(b))]...)
}

// opaque appends variable-length opaque data.
func (e *encoder) opaque(b []byte) {
	e.u32(uint32(len(b)))
	e.fixed(b)
}

// str appends a string.
func (e *encoder) str(s string) {
	e.u32(uint32(len(s)))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, zeros[:pad(len(s))]...)
}

// fh appends the file handle (nfs_fh3, nfs_fh4) of file id.
func (e *encoder) fh(id uint64) {
	e.u32(fhSize)
	e.u64(id)
}

// opaqueFrom appends variable-length opaque data of at most n bytes that
// fill writes into the reply in place, saving READ a copy. It returns what
// fill returns; the opaque holds the bytes fill reported, even with an error.
func (e *encoder) opaqueFrom(n int, fill func([]byte) (int, error)) (int, error) {
	at := len(e.buf)
	e.buf = slices.Grow(e.buf, 4+n+3)[:at+4+n]
	got, err := fill(e.buf[at+4:])
	got = min(max(got, 0), n)
	binary.BigEndian.PutUint32(e.buf[at:], uint32(got))
	e.buf = e.buf[:at+4+got]
	e.buf = append(e.buf, zeros[:pad(got)]...)
	return got, err
}

// len is the reply's length so far, a mark for put32 and truncate.
func (e *encoder) len() int { return len(e.buf) }

// put32 overwrites the unsigned int at mark at, such as a count or status
// written before the items it describes were known.
func (e *encoder) put32(at int, v uint32) { binary.BigEndian.PutUint32(e.buf[at:], v) }

// truncate drops everything appended after mark at.
func (e *encoder) truncate(at int) { e.buf = e.buf[:at] }

// zeros supplies XDR padding.
var zeros [4]byte

// pad is the padding that follows n bytes of opaque data.
func pad(n int) int { return -n & 3 }
