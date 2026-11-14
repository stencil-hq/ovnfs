package smb

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

type vectoredTestConn struct {
	net.Conn
	scalar, vectors int
}

func (c *vectoredTestConn) Write(p []byte) (int, error) {
	c.scalar++
	return c.Conn.Write(p)
}

func (c *vectoredTestConn) WriteBuffers(buffers net.Buffers) (int64, error) {
	c.vectors++
	return buffers.WriteTo(c.Conn)
}

func TestSMBWrappedVectoredResponses(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	transport := &vectoredTestConn{Conn: server}
	c := &conn{nc: transport}
	var observed []byte
	responses := []*wireResponse{
		{r: &request{onSent: func(b []byte) { observed = b }}, h: header{Command: cmdEcho, Flags: flagResponse, MessageID: 1}, body: []byte{4, 0, 0, 0}},
		{h: header{Command: cmdEcho, Flags: flagResponse, MessageID: 2}, body: []byte{4, 0, 0, 0}},
	}
	done := make(chan error, 1)
	go func() { done <- c.sendResponses(responses) }()
	wire := &wireClient{t: t, conn: client}
	frame, err := wire.readFrame()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if transport.vectors != 1 || transport.scalar != 0 {
		t.Fatalf("transport writes: vectored=%d scalar=%d", transport.vectors, transport.scalar)
	}
	if len(frame) != 140 || binary.LittleEndian.Uint32(frame[20:]) != 72 {
		t.Fatalf("compound framing: length %d, next command %d", len(frame), binary.LittleEndian.Uint32(frame[20:]))
	}
	for _, raw := range [][]byte{frame[:72], frame[72:]} {
		reply, err := decodeWireReply(raw)
		if err != nil || reply.command != cmdEcho || !bytes.Equal(reply.body[:4], []byte{4, 0, 0, 0}) {
			t.Fatalf("decoded response = %+v, error %v", reply, err)
		}
	}
	if !bytes.Equal(observed, frame[:72]) {
		t.Fatal("preauthentication callback did not receive the padded SMB message without its transport prefix")
	}
}
