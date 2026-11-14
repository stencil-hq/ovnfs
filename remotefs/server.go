// Package remotefs serves one checkout over remote-fs v1, the Unix-socket protocol
// a vibevmm VM's virtio-fs `home` device (`--remote-fs TAG:SOCK[:rw]`) speaks:
// 9-byte frames carrying path-addressed JSON requests, one in flight per
// connection, no hello, no handles and no inode numbers.
package remotefs

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Share is one checkout as a remote-fs socket serves it.
type Share struct {
	// FS is the export tree with every checkout as a top-level directory, e.g.
	// (*export.Export).Nodes(), so mutations keep the export's node ids in step.
	FS billy.Filesystem
	// Checkout names the served checkout; every wire path resolves below "<Checkout>/".
	Checkout string
	// Writable admits mutations; a read-only share answers them with rofs.
	Writable bool
}

// IDRange maps Count consecutive ids starting at Guest (as the VM sees them) to ones starting at Host.
type IDRange struct {
	// Guest is the first id inside the VM.
	Guest uint32
	// Host is the first id on the host.
	Host uint32
	// Count is the length of the range.
	Count uint32
}

// Options configures who may connect and how ownership crosses the wire.
type Options struct {
	// PeerUIDs lists the Unix peer uids allowed to connect; empty admits only the server's effective uid.
	PeerUIDs []uint32
	// UIDs maps guest uids to host uids. Replies show host owners through the
	// inverse map; create and mkdir chown new nodes only to mapped owners, and
	// leave unmapped callers' nodes owned by the server; setattr passes
	// unmapped ids through unchanged.
	UIDs []IDRange
	// GIDs maps guest gids to host gids like UIDs.
	GIDs []IDRange
	// Quiesce flushes and excludes external write caches for each request.
	// Its release runs after backend access; errors reject the request.
	Quiesce func(context.Context) (func(), error)
	// Logf receives rejected peers and broken connections; nil discards them.
	Logf func(format string, args ...any)
}

// toHost maps a guest id to the host, passing unmapped ids through.
func toHost(maps []IDRange, id uint32) uint32 {
	if host, ok := mapped(maps, id); ok {
		return host
	}
	return id
}

// mapped reports the host id of a guest id that falls inside one of maps.
func mapped(maps []IDRange, id uint32) (uint32, bool) {
	for _, m := range maps {
		if id >= m.Guest && id-m.Guest < m.Count {
			return m.Host + (id - m.Guest), true
		}
	}
	return 0, false
}

// toGuest maps a host id back to the guest, passing unmapped ids through.
func toGuest(maps []IDRange, id uint32) uint32 {
	for _, m := range maps {
		if id >= m.Host && id-m.Host < m.Count {
			return m.Guest + (id - m.Host)
		}
	}
	return id
}

// server holds the share and options shared by every connection.
type server struct {
	share Share
	opts  Options
}

// Serve answers remote-fs v1 connections from l for share until l is closed,
// then closes the open connections and returns nil. Connections from peers
// outside opts.PeerUIDs are dropped.
func Serve(l net.Listener, share Share, opts Options) error {
	if share.FS == nil {
		return errors.New("remotefs: share filesystem is required")
	}
	if !validSegment(share.Checkout) {
		return fmt.Errorf("remotefs: invalid checkout name %q", share.Checkout)
	}
	s := &server{share: share, opts: opts}
	var (
		mu    sync.Mutex
		conns = make(map[net.Conn]struct{})
		wg    sync.WaitGroup
		delay time.Duration
	)
	defer func() {
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return fmt.Errorf("remotefs: accept: %w", err)
		}
		delay = 0
		mu.Lock()
		conns[c] = struct{}{}
		mu.Unlock()
		wg.Go(func() {
			defer func() {
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
				c.Close()
			}()
			if err := checkPeer(c, s.opts.PeerUIDs); err != nil {
				s.logf("remotefs %s: reject peer: %v", s.share.Checkout, err)
				return
			}
			if err := s.serveConn(c); err != nil {
				s.logf("remotefs %s: connection: %v", s.share.Checkout, err)
			}
		})
	}
}

// logf forwards to Options.Logf when set.
func (s *server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// conn is one connection's reusable buffers.
type conn struct {
	*server
	frame []byte
	data  []byte
	enc   *encoder
}

// reply is one response frame.
type reply struct {
	ty   byte
	body []byte
}

// serveConn answers frames in order until the peer closes or breaks framing.
func (s *server) serveConn(nc net.Conn) error {
	c := &conn{server: s, enc: newEncoder()}
	r := bufio.NewReaderSize(nc, 64<<10)
	for {
		ty, id, payload, err := readFrame(r, c.frame)
		c.frame = payload[:0]
		if err != nil {
			if errors.Is(err, net.ErrClosed) || err == io.EOF {
				return nil
			}
			return err
		}
		var out reply
		switch ty {
		case FrameReq:
			out = c.handle(payload, nil)
		case FrameReqData:
			body, data, err := splitData(payload)
			if err != nil {
				out = c.fail(errorf(CodeBadRequest, err.Error()))
			} else {
				out = c.handle(body, data)
			}
		default:
			return fmt.Errorf("unexpected frame type %d", ty)
		}
		var header [HeaderLen]byte
		binary.LittleEndian.PutUint32(header[:4], uint32(len(out.body)))
		header[4] = out.ty
		binary.LittleEndian.PutUint32(header[5:], id)
		bufs := net.Buffers{header[:], out.body}
		if _, err := bufs.WriteTo(nc); err != nil {
			return fmt.Errorf("write reply: %w", err)
		}
	}
}

// handle decodes and runs one request.
func (c *conn) handle(body, data []byte) reply {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return c.fail(errorf(CodeBadRequest, "decode request: "+err.Error()))
	}
	if req.Op == opHello {
		return c.fail(errorf(CodeUnsupported, "remote-fs v2 is not served; v1 clients send no hello"))
	}
	if f := req.missing(); f != "" {
		return c.fail(errorf(CodeBadRequest, fmt.Sprintf("%s request lacks %s", req.Op, f)))
	}
	if c.opts.Quiesce != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		release, err := c.opts.Quiesce(ctx)
		if err != nil {
			return c.fail(wireError(err))
		}
		defer release()
	}
	out, err := c.run(&req, data)
	if err != nil {
		return c.fail(wireError(err))
	}
	return out
}

// fail encodes an ERR reply.
func (c *conn) fail(e *Error) reply {
	body, err := c.enc.marshal(e)
	if err != nil {
		body = []byte(`{"code":"io","msg":"encode error"}`)
	}
	return reply{ty: FrameErr, body: body}
}

// ok encodes an OK_JSON reply.
func (c *conn) ok(v any) (reply, error) {
	body, err := c.enc.marshal(v)
	if err != nil {
		return reply{}, fmt.Errorf("encode reply: %w", err)
	}
	if len(body) > MaxFrame {
		return reply{}, errorf(CodeTooBig, fmt.Sprintf("reply of %d bytes exceeds the 2 MiB frame limit", len(body)))
	}
	return reply{ty: FrameOKJSON, body: body}, nil
}

// validSegment reports whether s is one usable path segment (vibevmm valid_segment).
func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\x00")
}

// validPath reports whether p is "" (the checkout root) or slash-joined valid segments (vibevmm valid_path).
func validPath(p string) bool {
	if p == "" {
		return true
	}
	for seg := range strings.SplitSeq(p, "/") {
		if !validSegment(seg) {
			return false
		}
	}
	return true
}
