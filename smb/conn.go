package smb

import (
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/smb/auth"
)

// conn owns transport serialization and all resources bound to a TCP connection.
type conn struct {
	srv                         *Server
	nc                          net.Conn
	dialect                     uint16
	clientGUID                  [16]byte
	clientCaps                  uint32
	clientSecMode               uint16
	clientDialects              []uint16
	serverCaps                  uint32
	serverSecMode               uint16
	signingAlg                  uint16
	preauth                     [64]byte
	posix                       bool
	appleReadDir                atomic.Bool
	appleCaseSensitive          atomic.Bool
	maxRead, maxWrite, maxTrans uint32
	mu                          sync.Mutex
	sessions                    map[uint64]*session
	operations                  map[uint64]*asyncRecord
	closed                      bool
	writeMu                     sync.Mutex
	ioTail                      <-chan struct{} // earlier WRITE/FLUSH frames, assigned by serve
	creditMu                    sync.Mutex
	creditInitialized           bool
	credits                     uint32
	// owner and group are the binary SIDs that own files without Unix IDs;
	// nil unless the client is the daemon's own user (see [Server.Serve]).
	owner, group []byte
}

// session contains an authenticated identity and its independently locked trees.
type session struct {
	id               uint64
	user             string
	guest, anonymous bool
	signingKey       []byte
	signRequired     bool
	preauth          [64]byte
	auth             any
	conn             *conn
	mu               sync.Mutex
	trees            map[uint32]*tree
	closed           bool
}

// tree binds a share connection to its open handles.
type tree struct {
	id     uint32
	sess   *session
	ipc    bool
	mu     sync.Mutex
	files  map[fileID]*open
	closed bool
}

// fileID is the persistent and volatile handle pair carried on the wire.
type fileID struct{ Persistent, Volatile uint64 }

// relatedFileID selects the last successful CREATE in a related compound chain.
var relatedFileID = fileID{^uint64(0), ^uint64(0)}

// resourceSequence provides connection-independent, nonzero tree and handle IDs.
var resourceSequence atomic.Uint64

// open holds an individual SMB file, directory, or named pipe handle.
type open struct {
	id                           fileID
	tree                         *tree
	path                         string
	file                         billy.File
	isDir                        bool
	access, shareAccess, options uint32
	mu                           sync.Mutex
	deleteOnClose                bool
	dir                          *dirCursor
	pipe                         *pipe
	oplock                       uint8
	leaseKey                     [16]byte
	leaseState                   uint32
	closeFn                      func()
	info                         os.FileInfo // last metadata read for this open, see [open.stat]
	infoEpoch                    uint64
	infoAt                       time.Time
}

// infoLifetime bounds how long an open reuses its metadata when nothing on the
// server changed: folder edits behind live paths reach no SMB command. It is
// shorter than Windows clients' own attribute cache.
const infoLifetime = time.Second

// stat returns the open's metadata for QUERY_INFO. Windows clients query each
// handle two or three times right after CREATE; the CREATE's lookup serves
// them while no command that could change any file has run since (see
// [state.epoch]). The caller holds o.mu.
func (o *open) stat(s *Server) (os.FileInfo, error) {
	epoch := s.st.epoch.Load()
	if o.info != nil && o.infoEpoch == epoch && time.Since(o.infoAt) < infoLifetime {
		return o.info, nil
	}
	info, err := s.FS.Lstat(o.path)
	if err != nil {
		return nil, err
	}
	o.info, o.infoEpoch, o.infoAt = info, epoch, time.Now()
	return info, nil
}

// request carries one decoded command and its resolved session/share state.
type request struct {
	c              *conn
	hdr            header
	raw            []byte
	body           []byte
	sess           *session
	tree           *tree
	onSent         func(msg []byte)
	compound       *compoundState
	pendingReply   *wireResponse
	asyncStarted   bool
	chainFailed    bool
	ioBefore       <-chan struct{}
	ioDone         chan struct{}
	cacheProtected bool
}

// handler returns a command-specific response or an NTSTATUS failure.
type handler func(r *request) (body []byte, status uint32)

// handlers is populated by command implementations during package initialization.
var handlers [cmdOplockBreak + 1]handler

// asyncOp identifies a pending request and exposes its cancellation signal.
type asyncOp struct {
	id        uint64
	cancelled <-chan struct{}
}

// asyncRecord tracks one pending response until completion or connection teardown.
type asyncRecord struct {
	op        *asyncOp
	r         *request
	cancel    chan struct{}
	once      sync.Once
	ready     chan struct{}
	readyOnce sync.Once
}

// compoundState carries identifiers inherited by related commands in one frame.
type compoundState struct {
	sessionID uint64
	treeID    uint32
	file      fileID
	hasFile   bool
	failed    bool
}

// wireResponse delays signing until compound padding and NextCommand are final.
type wireResponse struct {
	r       *request
	h       header
	body    []byte
	pending *asyncRecord
}

// session looks up a session without retaining the connection lock.
func (c *conn) session(id uint64) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[id]
}

// addSession publishes a session unless this connection has already closed.
func (c *conn) addSession(s *session) {
	s.conn = c
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		s.close()
		return
	}
	if c.sessions == nil {
		c.sessions = make(map[uint64]*session)
	}
	c.sessions[s.id] = s
	c.mu.Unlock()
}

// removeSession tears down its trees and cancels outstanding asynchronous work.
func (c *conn) removeSession(id uint64) {
	c.mu.Lock()
	s := c.sessions[id]
	delete(c.sessions, id)
	for _, a := range c.operations {
		if a.r.hdr.SessionID == id {
			a.once.Do(func() { close(a.cancel) })
		}
	}
	c.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// tree looks up a live share connection.
func (s *session) tree(id uint32) *tree {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trees[id]
}

// addTree assigns a nonzero tree identifier and publishes its parent session.
func (s *session) addTree(t *tree) {
	t.id = uint32(resourceSequence.Add(1))
	if t.id == 0 {
		t.id = uint32(resourceSequence.Add(1))
	}
	t.sess = s
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		t.close()
		return
	}
	if s.trees == nil {
		s.trees = make(map[uint32]*tree)
	}
	s.trees[t.id] = t
	s.mu.Unlock()
}

// removeTree closes all handles belonging to a disconnected tree.
func (s *session) removeTree(id uint32) {
	s.mu.Lock()
	t := s.trees[id]
	delete(s.trees, id)
	s.mu.Unlock()
	if t != nil {
		t.close()
	}
}

// close detaches and closes a session's trees once.
func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	trees := s.trees
	s.trees = nil
	s.mu.Unlock()
	for _, t := range trees {
		t.close()
	}
}

// addOpen assigns a globally unique handle before exposing it to other requests.
func (t *tree) addOpen(o *open) {
	n := resourceSequence.Add(1)
	o.id = fileID{n, n}
	o.tree = t
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		closeImplicit(o)
		return
	}
	if t.files == nil {
		t.files = make(map[fileID]*open)
	}
	t.files[o.id] = o
	t.mu.Unlock()
}

// open looks up the complete handle pair, preventing stale persistent IDs.
func (t *tree) open(id fileID) *open {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.files[id]
}

// removeOpen removes a handle whose explicit CLOSE owns resource disposal.
func (t *tree) removeOpen(id fileID) {
	t.mu.Lock()
	delete(t.files, id)
	t.mu.Unlock()
}

// opens returns a stable snapshot of all handles belonging to the tree.
func (t *tree) opens() []*open {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*open, 0, len(t.files))
	for _, o := range t.files {
		out = append(out, o)
	}
	return out
}

// close atomically detaches a tree's handles before disposing their resources.
func (t *tree) close() {
	if t.sess != nil && t.sess.conn != nil {
		c := t.sess.conn
		c.mu.Lock()
		for _, a := range c.operations {
			if a.r.hdr.SessionID == t.sess.id && a.r.hdr.TreeID == t.id {
				a.once.Do(func() { close(a.cancel) })
			}
		}
		c.mu.Unlock()
	}
	t.mu.Lock()
	t.closed = true
	files := t.files
	t.files = nil
	t.mu.Unlock()
	for _, o := range files {
		closeImplicit(o)
	}
}

// closeImplicit performs the Files owner's close hook and releases its file.
func closeImplicit(o *open) {
	if o.tree != nil && o.tree.sess != nil && o.tree.sess.conn != nil {
		o.tree.sess.conn.srv.st.opens.loseCaching(o, errors.New("write lease handle closed without SMB CLOSE"))
	}
	o.mu.Lock()
	fn := o.closeFn
	o.closeFn = nil
	o.mu.Unlock()
	if fn != nil {
		fn()
	}
	o.mu.Lock()
	f := o.file
	o.file = nil
	o.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
}

// openFile resolves the inherited handle only inside a related compound request.
func (r *request) openFile(id fileID) (*open, uint32) {
	if id == relatedFileID {
		if r.hdr.Flags&flagRelated == 0 || r.compound == nil || !r.compound.hasFile {
			return nil, statusFileClosed
		}
		id = r.compound.file
	}
	if r.tree == nil {
		return nil, statusFileClosed
	}
	o := r.tree.open(id)
	if o == nil {
		return nil, statusFileClosed
	}
	r.setRelated(o.id)
	return o, statusSuccess
}

// setRelated records a supplied or created handle for subsequent related operations.
func (r *request) setRelated(id fileID) {
	if r.compound != nil {
		r.compound.file = id
		r.compound.hasFile = true
	}
}

// shutdown releases every connection-owned resource and wakes pending handlers.
func (c *conn) shutdown() {
	_ = c.nc.Close()
	c.mu.Lock()
	c.closed = true
	sessions := c.sessions
	c.sessions = nil
	for _, a := range c.operations {
		a.once.Do(func() { close(a.cancel) })
		a.readyOnce.Do(func() { close(a.ready) })
	}
	c.operations = nil
	c.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

// serve reads Direct TCP records and runs independent requests concurrently.
func (c *conn) serve() {
	defer c.shutdown()
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(c.nc, prefix[:]); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(prefix[:])
		if prefix[0] != 0 || length < 4 || length > 0x00ffffff {
			return
		}
		msg := make([]byte, int(length))
		if _, err := io.ReadFull(c.nc, msg); err != nil {
			return
		}
		switch string(msg[:4]) {
		case "\xfdSMB", "\xfcSMB":
			return
		case "\xffSMB":
			response := handleSMB1Negotiate(c, msg)
			if len(response) == 0 || c.writeFrame(response) != nil {
				return
			}
		case "\xfeSMB":
			requests, err := c.decodeRequests(msg)
			if err != nil {
				return
			}
			if c.srv.WriteCache && len(requests) == 1 {
				// Preserve standalone I/O arrival order through dispatch goroutines.
				// Compounds execute in order themselves; a WRITE following a
				// blocked CREATE must not prevent that CREATE's break ACK.
				for _, r := range requests {
					drain := r.hdr.Command == cmdWrite || r.hdr.Command == cmdFlush
					if r.hdr.Command == cmdSetInfo && len(r.body) >= 4 && r.body[2] == 1 {
						switch r.body[3] {
						case 4, 19, 20, 39:
							drain = true // mtime, allocation and EOF can finish a client flush
						}
					}
					if r.hdr.Command == cmdIoctl && len(r.body) >= 8 && binary.LittleEndian.Uint32(r.body[4:]) == ioctlZeroData {
						drain = true
					}
					if drain {
						r.ioBefore = c.ioTail
						r.ioDone = make(chan struct{})
						c.ioTail = r.ioDone
					} else if r.hdr.Command == cmdOplockBreak || r.hdr.Command == cmdClose {
						r.ioBefore = c.ioTail
					}
				}
			}
			go c.runRequests(requests)
		default:
			return
		}
	}
}

// decodeRequests validates compound boundaries before any handler can run.
func (c *conn) decodeRequests(msg []byte) ([]*request, error) {
	var requests []*request
	for offset := 0; ; {
		h, err := decodeHeader(msg[offset:])
		if err != nil {
			return nil, err
		}
		if h.Flags&flagResponse != 0 {
			return nil, errors.New("smb: response received from client")
		}
		end := len(msg)
		if h.NextCommand != 0 {
			if h.NextCommand < 64 || h.NextCommand%8 != 0 || uint64(offset)+uint64(h.NextCommand)+64 > uint64(len(msg)) {
				return nil, errors.New("smb: invalid compound offset")
			}
			end = offset + int(h.NextCommand)
		}
		raw := msg[offset:end]
		requests = append(requests, &request{c: c, hdr: h, raw: raw, body: raw[64:]})
		if h.NextCommand == 0 {
			break
		}
		offset = end
	}
	return requests, nil
}

// runRequests processes each compound in order and emits a single response frame.
func (c *conn) runRequests(requests []*request) {
	var compound compoundState
	responses := make([]*wireResponse, 0, len(requests))
	for i, r := range requests {
		if len(requests) > 1 {
			r.compound = &compound
		}
		related := r.hdr.Flags&flagRelated != 0
		if related && i == 0 {
			r.chainFailed = true
			if response := c.dispatch(r); response != nil {
				responses = append(responses, response)
			}
			compound.sessionID, compound.treeID = r.hdr.SessionID, r.hdr.TreeID
			compound.failed = true
			continue
		}
		if related {
			r.hdr.SessionID = compound.sessionID
			r.hdr.TreeID = compound.treeID
		} else {
			compound = compoundState{sessionID: r.hdr.SessionID, treeID: r.hdr.TreeID}
		}
		r.chainFailed = related && compound.failed
		response := c.dispatch(r)
		compound.sessionID = r.hdr.SessionID
		compound.treeID = r.hdr.TreeID
		if response != nil {
			responses = append(responses, response)
			if response.h.Status&0xc0000000 == 0xc0000000 && response.h.Status != statusMoreProcessingRequired {
				compound.failed = true
			}
		}
	}
	if len(responses) > 0 {
		_ = c.sendResponses(responses)
	}
}

// dispatch resolves security and share state before entering a command handler.
func (c *conn) dispatch(r *request) *wireResponse {
	if r.ioBefore != nil {
		<-r.ioBefore
	}
	if r.ioDone != nil {
		defer close(r.ioDone)
	}
	if r.hdr.Command != cmdNegotiate && r.hdr.SessionID != 0 {
		r.sess = c.session(r.hdr.SessionID)
	}
	if r.hdr.Command == cmdCancel {
		if r.sess == nil || !sessionAuthenticated(r.sess) {
			return nil
		}
		if len(r.sess.signingKey) != 0 && (r.hdr.Flags&flagSigned != 0 || r.sess.signRequired) {
			if r.hdr.Flags&flagSigned == 0 || !c.verify(r) {
				return nil
			}
		}
		c.cancel(r)
		return nil
	}
	if status := c.consumeCredits(r); status != statusSuccess {
		return c.response(r, nil, status, 0, false)
	}
	if r.hdr.Command != cmdNegotiate && r.hdr.Command != cmdEcho && r.hdr.Command != cmdSessionSetup {
		if r.sess == nil || !sessionAuthenticated(r.sess) {
			return c.response(r, nil, statusUserSessionDeleted, 0, false)
		}
	}
	if r.hdr.Command == cmdSessionSetup && r.hdr.SessionID != 0 && r.sess == nil {
		return c.response(r, nil, statusUserSessionDeleted, 0, false)
	}
	if r.sess != nil && len(r.sess.signingKey) != 0 {
		if r.hdr.Flags&flagSigned != 0 {
			if !c.verify(r) {
				return c.response(r, nil, statusAccessDenied, 0, false)
			}
		} else if r.sess.signRequired {
			return c.response(r, nil, statusAccessDenied, 0, false)
		}
	}
	// Optional queries can fail after a successful open. Preserve their error,
	// but still release that open when the related chain reaches CLOSE.
	// No inherited handle exists after a failed CREATE or malformed first link.
	if r.chainFailed && !(r.hdr.Command == cmdClose && r.compound != nil && r.compound.hasFile) {
		return c.response(r, nil, statusInvalidParameter, 0, false)
	}
	leaseAck := r.hdr.Command == cmdOplockBreak && r.hdr.TreeID == 0 && len(r.body) >= 36 && binary.LittleEndian.Uint16(r.body) == 36
	if r.hdr.Command == cmdTreeDisconnect || (r.hdr.Command >= cmdCreate && r.hdr.Command <= cmdOplockBreak && r.hdr.Command != cmdEcho && !leaseAck) {
		if r.sess == nil {
			return c.response(r, nil, statusUserSessionDeleted, 0, false)
		}
		r.tree = r.sess.tree(r.hdr.TreeID)
		if r.tree == nil {
			return c.response(r, nil, statusNetworkNameDeleted, 0, false)
		}
	}
	if r.hdr.Command == cmdEcho {
		if len(r.body) < 4 || binary.LittleEndian.Uint16(r.body) != 4 {
			return c.response(r, nil, statusInvalidParameter, 0, false)
		}
		return c.response(r, []byte{4, 0, 0, 0}, statusSuccess, 0, false)
	}
	if int(r.hdr.Command) >= len(handlers) || handlers[r.hdr.Command] == nil {
		return c.response(r, nil, statusNotSupported, 0, false)
	}
	if c.srv.st.opens.cacheError() != nil {
		switch r.hdr.Command {
		case cmdWrite, cmdFlush, cmdClose, cmdOplockBreak, cmdTreeDisconnect, cmdLogoff:
			// Keep drain and teardown available, but never clear the fault.
		default:
			return c.response(r, nil, statusUnsuccessful, 0, false)
		}
	}
	mutating := r.hdr.Command == cmdWrite || r.hdr.Command == cmdSetInfo || r.hdr.Command == cmdIoctl
	if mutating {
		c.srv.st.changed()
	}
	body, status := handlers[r.hdr.Command](r)
	if mutating {
		c.srv.st.changed()
	}
	if status == statusPending && r.asyncStarted {
		return r.pendingReply
	}
	body, status = r.identityReply(body, status)
	return c.response(r, body, status, 0, false)
}

// identityReply applies equally to synchronous and resumed asynchronous calls:
// no successful reply may expose an unusable backend identity.
func (r *request) identityReply(body []byte, status uint32) ([]byte, uint32) {
	if fs, ok := r.c.srv.FS.(interface{ IdentityError() error }); ok && r.tree != nil && r.hdr.Command != cmdClose && fs.IdentityError() != nil {
		return nil, statusInternalError
	}
	return body, status
}

// consumeCredits charges large I/O in 64-KiB units before it reaches the backend.
func (c *conn) consumeCredits(r *request) uint32 {
	charge := uint32(r.hdr.CreditCharge)
	if charge == 0 {
		charge = 1
	}
	var size uint32
	if r.hdr.Command == cmdRead || r.hdr.Command == cmdWrite {
		if len(r.body) >= 8 {
			size = binary.LittleEndian.Uint32(r.body[4:8])
		}
	}
	if c.dialect != 0x0202 && size > charge*65536 {
		return statusInvalidParameter
	}
	c.creditMu.Lock()
	defer c.creditMu.Unlock()
	if !c.creditInitialized {
		c.credits = 1
		c.creditInitialized = true
	}
	if charge > c.credits {
		return statusInvalidParameter
	}
	c.credits -= charge
	return statusSuccess
}

// grantCredits replenishes the requested window while bounding normal grants.
func (c *conn) grantCredits(wanted uint16) uint16 {
	c.creditMu.Lock()
	defer c.creditMu.Unlock()
	if !c.creditInitialized {
		c.credits = 1
		c.creditInitialized = true
	}
	grant := uint32(wanted)
	room := uint32(0)
	if c.credits < 512 {
		room = 512 - c.credits
	}
	if grant > room {
		grant = room
	}
	if grant == 0 {
		grant = 1
	}
	c.credits += grant
	return uint16(grant)
}

// response constructs a response after handlers have updated the session or tree ID.
func (c *conn) response(r *request, body []byte, status uint32, asyncID uint64, finalAsync bool) *wireResponse {
	if c.srv.Logf != nil {
		c.srv.Logf("smb %s command=%#04x message=%d session=%d tree=%d status=%#08x", c.nc.RemoteAddr(), r.hdr.Command, r.hdr.MessageID, r.hdr.SessionID, r.hdr.TreeID, status)
	}
	h := r.hdr
	h.Status, h.NextCommand = status, 0
	h.Flags = flagResponse | (r.hdr.Flags & (flagRelated | flagPriorityMask))
	h.Signature = [16]byte{}
	if asyncID != 0 {
		h.Flags |= flagAsync
		h.AsyncID = asyncID
	}
	if finalAsync {
		h.Credits = 0
	} else {
		h.Credits = c.grantCredits(r.hdr.Credits)
	}
	if body == nil && status != statusSuccess {
		body = []byte{9, 0, 0, 0, 0, 0, 0, 0, 0}
	}
	return &wireResponse{r: r, h: h, body: body}
}

// signature computes the dialect-specific message authentication code.
func (c *conn) signature(key []byte, h header, msg []byte) [16]byte {
	if c.dialect < 0x0300 {
		return auth.SignHMACSHA256(key, msg)
	}
	if c.signingAlg == 2 {
		var nonce [12]byte
		binary.LittleEndian.PutUint64(nonce[:8], h.MessageID)
		var flags uint32
		if h.Flags&flagResponse != 0 {
			flags |= 1
		}
		if h.Command == cmdCancel {
			flags |= 2
		}
		binary.LittleEndian.PutUint32(nonce[8:], flags)
		return auth.SignAESGMAC(key, nonce, msg)
	}
	return auth.SignAESCMAC(key, msg)
}

// verify authenticates the original wire bytes, including compound padding.
func (c *conn) verify(r *request) bool {
	saved := r.hdr.Signature
	clear(r.raw[48:64])
	actual := c.signature(r.sess.signingKey, r.hdr, r.raw)
	copy(r.raw[48:64], saved[:])
	return subtle.ConstantTimeCompare(saved[:], actual[:]) == 1
}

// sendResponses finalizes padding, signatures, and preauthentication callbacks.
func (c *conn) sendResponses(responses []*wireResponse) error {
	var frame []byte
	for i, response := range responses {
		r, h := response.r, response.h
		length := 64 + len(response.body)
		if i+1 < len(responses) {
			length = (length + 7) &^ 7
			h.NextCommand = uint32(length)
		}
		shouldSign := r != nil && r.sess != nil && len(r.sess.signingKey) != 0 && h.Status != statusPending && (r.hdr.Flags&flagSigned != 0 || r.sess.signRequired || (h.Command == cmdSessionSetup && h.Status == statusSuccess))
		if shouldSign {
			h.Flags |= flagSigned
		}
		start := len(frame)
		frame = h.appendTo(frame)
		frame = append(frame, response.body...)
		frame = append(frame, make([]byte, length-(64+len(response.body)))...)
		msg := frame[start:]
		if shouldSign {
			sig := c.signature(r.sess.signingKey, h, msg)
			copy(msg[48:64], sig[:])
		}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	offset := 0
	for _, response := range responses {
		length := 64 + len(response.body)
		if response != responses[len(responses)-1] {
			length = (length + 7) &^ 7
		}
		if response.r != nil && response.r.onSent != nil {
			response.r.onSent(frame[offset : offset+length])
		}
		offset += length
	}
	err := c.writeFrameLocked(frame)
	for _, response := range responses {
		if response.pending != nil {
			response.pending.readyOnce.Do(func() { close(response.pending.ready) })
		}
	}
	return err
}

// writeFrame serializes a complete Direct TCP frame against concurrent writers.
func (c *conn) writeFrame(msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrameLocked(msg)
}

// writeFrameLocked emits the length and payload with full-write handling.
func (c *conn) writeFrameLocked(msg []byte) error {
	if len(msg) > 0x00ffffff {
		_ = c.nc.Close()
		return errors.New("smb: response exceeds Direct TCP maximum")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(msg)))
	buffers := net.Buffers{prefix[:], msg}
	var err error
	// Listener wrappers can retain the underlying socket's writev path;
	// net.Buffers otherwise writes the prefix and payload separately.
	if writer, ok := c.nc.(interface {
		WriteBuffers(net.Buffers) (int64, error)
	}); ok {
		_, err = writer.WriteBuffers(buffers)
	} else {
		_, err = buffers.WriteTo(c.nc)
	}
	if err != nil {
		_ = c.nc.Close()
	}
	return err
}

// sendBreak sends the unsigned server-initiated oplock or lease break notification.
func (c *conn) sendBreak(body []byte) {
	h := header{Command: cmdOplockBreak, Flags: flagResponse, MessageID: ^uint64(0)}
	if err := c.writeFrame(append(h.appendTo(nil), body...)); err != nil {
		c.shutdown()
	}
}

// async sends the initial unsigned pending response and registers cancellation.
func (r *request) async() *asyncOp {
	id := resourceSequence.Add(1)
	cancel := make(chan struct{})
	a := &asyncOp{id: id, cancelled: cancel}
	record := &asyncRecord{op: a, r: r, cancel: cancel, ready: make(chan struct{})}
	r.c.mu.Lock()
	if r.c.operations == nil {
		r.c.operations = make(map[uint64]*asyncRecord)
	}
	r.c.operations[id] = record
	closed := r.c.closed
	if r.sess != nil {
		r.sess.mu.Lock()
		closed = closed || r.sess.closed
		r.sess.mu.Unlock()
	}
	if r.tree != nil {
		r.tree.mu.Lock()
		closed = closed || r.tree.closed
		r.tree.mu.Unlock()
	}
	if closed {
		record.once.Do(func() { close(cancel) })
	}
	r.c.mu.Unlock()
	r.asyncStarted = true
	response := r.c.response(r, nil, statusPending, id, false)
	response.pending = record
	if r.compound != nil {
		r.pendingReply = response
	} else {
		_ = r.c.sendResponses([]*wireResponse{response})
	}
	return a
}

// complete sends exactly one final response with the registered asynchronous ID.
func (r *request) complete(a *asyncOp, body []byte, status uint32) {
	if a == nil {
		return
	}
	// A pending rename or delete commits here, after dispatch returned.
	r.c.srv.st.changed()
	r.c.mu.Lock()
	record := r.c.operations[a.id]
	r.c.mu.Unlock()
	if record == nil || record.r != r {
		return
	}
	<-record.ready
	r.c.mu.Lock()
	if r.c.operations[a.id] != record || r.c.closed {
		r.c.mu.Unlock()
		return
	}
	delete(r.c.operations, a.id)
	r.c.mu.Unlock()
	body, status = r.identityReply(body, status)
	response := r.c.response(r, body, status, a.id, true)
	_ = r.c.sendResponses([]*wireResponse{response})
}

// cancel signals only the pending request belonging to this session and identifier.
func (c *conn) cancel(r *request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, a := range c.operations {
		if a.r.hdr.SessionID != r.hdr.SessionID {
			continue
		}
		if (r.hdr.Flags&flagAsync != 0 && id == r.hdr.AsyncID) || (r.hdr.Flags&flagAsync == 0 && a.r.hdr.MessageID == r.hdr.MessageID) {
			a.once.Do(func() { close(a.cancel) })
		}
	}
}
