package nfs

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
)

// ONC RPC (RFC 5531) programs this server answers.
const (
	progNFS   = 100003
	progMount = 100005
)

// RPC message fields.
const (
	msgCall           = 0
	msgReply          = 1
	replyAccepted     = 0
	replyDenied       = 1
	rejectRPCMismatch = 0
	rejectAuthError   = 1
	authNone          = 0
	authSys           = 1
	authBadCred       = 1
	maxAuthBytes      = 400
)

// acceptStat is an accepted reply's outcome (accept_stat).
type acceptStat uint32

// Accepted reply outcomes. Only rpcSuccess and rpcProgMismatch carry a body.
const (
	rpcSuccess acceptStat = iota
	rpcProgUnavail
	rpcProgMismatch
	rpcProcUnavail
	rpcGarbageArgs
	rpcSystemErr
)

// Record marking (RFC 5531 section 11).
const (
	lastFragment = 1 << 31
	// maxRecord bounds a request: a WRITE of maxIO and its headers. The
	// record is buffered whole, so a larger bound would let each call in
	// flight pin that much memory.
	maxRecord = maxIO + 64<<10
	// maxPooled bounds the buffers kept for reuse: a 1 MiB READ reply fits.
	maxPooled = 2 << 20
)

// errRecordTooLarge closes a connection whose request exceeds maxRecord.
var errRecordTooLarge = errors.New("nfs: RPC record too large")

// call is one RPC call being answered.
type call struct {
	xid              uint32
	prog, vers, proc uint32
	// args holds the procedure's arguments, after the call header.
	args decoder
	conn *conn
}

// conn is one client connection. Replies may go out in any order: each
// carries its call's xid.
type conn struct {
	srv     *Server
	nc      net.Conn
	wmu     sync.Mutex // serializes replies
	v3sizes clientReportedSizes
	// v4client is the NFSv4 client last identified on this connection, for
	// requests that name none (an NFSv4.0 client uses its own connections).
	v4client atomic.Uint64
}

// bufs recycles request and reply buffers.
var bufs = sync.Pool{New: func() any { return new([]byte) }}

// putBuf returns b to the pool unless it grew too large to keep.
func putBuf(b *[]byte) {
	if cap(*b) <= maxPooled {
		*b = (*b)[:0]
		bufs.Put(b)
	}
}

// serve answers calls until the client disconnects or a read fails. Up to
// Server.Concurrency calls run at once, since Linux multiplexes a mount's
// I/O over one connection and one slow call must not stall the rest.
func (c *conn) serve() {
	defer c.nc.Close()
	var wg sync.WaitGroup
	defer wg.Wait()
	limit := max(c.srv.Concurrency, 1)
	// jobs is unbuffered: a send succeeds only when a worker is idle, so at
	// most limit calls are in flight. Workers start lazily up to limit and
	// exit when jobs closes, after finishing every accepted call.
	jobs := make(chan *[]byte)
	defer close(jobs)
	workers := 0
	work := func(first *[]byte) {
		defer wg.Done()
		var cl call
		var reply encoder
		for req := first; req != nil; req = <-jobs {
			c.answer(*req, &cl, &reply)
			putBuf(req)
		}
	}
	r := bufio.NewReaderSize(c.nc, 64<<10)
	var cl call
	var reply encoder
	for {
		req := bufs.Get().(*[]byte)
		rec, err := readRecord(r, *req)
		if err != nil {
			putBuf(req)
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				c.srv.logf("nfs: read from %v: %v", c.nc.RemoteAddr(), err)
			}
			return
		}
		*req = rec
		if c.srv.Concurrency <= 1 {
			c.answer(rec, &cl, &reply)
			putBuf(req)
			continue
		}
		select {
		case jobs <- req:
		default:
			if workers < limit {
				workers++
				wg.Add(1)
				go work(req)
			} else {
				jobs <- req
			}
		}
	}
}

// readRecord reads one record into buf, joining its fragments.
func readRecord(r *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	for {
		mark, err := r.Peek(4)
		if err != nil {
			if (len(mark) != 0 || len(buf) != 0) && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		h := binary.BigEndian.Uint32(mark)
		_, _ = r.Discard(4)
		n := int(h &^ lastFragment)
		if len(buf)+n > maxRecord {
			return nil, errRecordTooLarge
		}
		at := len(buf)
		buf = slices.Grow(buf, n)[:at+n]
		if _, err := io.ReadFull(r, buf[at:]); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if h&lastFragment != 0 {
			return buf, nil
		}
	}
}

// answer decodes one call, dispatches it and sends the reply. A record that
// is not a well-formed call closes the connection: its stream cannot be
// trusted to stay in step. Each worker owns its call and encoder storage;
// dispatch never retains them after returning.
func (c *conn) answer(rec []byte, cl *call, e *encoder) {
	d := decoder{buf: rec}
	xid, mtype, rpcvers := d.u32(), d.u32(), d.u32()
	*cl = call{xid: xid, prog: d.u32(), vers: d.u32(), proc: d.u32(), conn: c}
	flavor := d.u32()
	d.opaque(maxAuthBytes)
	d.u32()
	d.opaque(maxAuthBytes)
	if d.err != nil || mtype != msgCall {
		c.srv.logf("nfs: malformed call from %v", c.nc.RemoteAddr())
		c.nc.Close()
		return
	}

	rb := bufs.Get().(*[]byte)
	defer putBuf(rb)
	*e = encoder{
		buf:      *rb,
		sizes:    &c.v3sizes,
		maxSizes: &c.srv.v3.maxSizes,
	}
	e.u32(0) // record mark, set below
	e.u32(xid)
	e.u32(msgReply)
	switch {
	case rpcvers != 2:
		e.u32(replyDenied)
		e.u32(rejectRPCMismatch)
		e.u32(2)
		e.u32(2)
	case flavor != authNone && flavor != authSys:
		e.u32(replyDenied)
		e.u32(rejectAuthError)
		e.u32(authBadCred)
	default:
		e.u32(replyAccepted)
		e.u32(authNone) // verifier
		e.u32(0)
		at := e.len()
		e.u32(0) // accept_stat, set below
		cl.args = d
		stat := c.srv.dispatch(cl, e)
		if stat != rpcSuccess && stat != rpcProgMismatch {
			e.truncate(at + 4)
		}
		e.put32(at, uint32(stat))
	}
	e.put32(0, lastFragment|uint32(e.len()-4))
	*rb = e.buf
	sent := c.send(e.buf)
	e.finishSize(sent)
	cl.args = decoder{}
	e.buf = nil
}

// send writes one reply record; a failed write closes the connection.
func (c *conn) send(b []byte) bool {
	c.wmu.Lock()
	_, err := c.nc.Write(b)
	c.wmu.Unlock()
	if err != nil {
		c.nc.Close()
	}
	return err == nil
}

// dispatch answers a call, appending the result body to res, and returns the
// accept_stat to reply with. The program handlers (mount, nfs3, nfs4) decode
// their arguments from c.args and return rpcGarbageArgs when they do not
// decode, rpcProcUnavail for unknown procedures, and rpcSuccess otherwise.
func (s *Server) dispatch(c *call, res *encoder) acceptStat {
	switch c.prog {
	case progMount:
		if c.vers == 3 {
			return s.mount(c, res)
		}
		res.u32(3)
		res.u32(3)
		return rpcProgMismatch
	case progNFS:
		switch c.vers {
		case 3:
			return s.nfs3(c, res)
		case 4:
			return s.nfs4(c, res)
		}
		res.u32(3)
		res.u32(4)
		return rpcProgMismatch
	}
	return rpcProgUnavail
}
