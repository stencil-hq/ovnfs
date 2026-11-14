package main

import (
	"context"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/remotefs"
)

// shares serves checkouts over remote-fs sockets in <state>/shares, one per checkout.
type shares struct {
	dir string
	reg *checkout.Registry
	e   *export.Export
	mu  sync.Mutex
	on  map[string]share
}

// share is one live remote-fs listener.
type share struct {
	l        net.Listener
	writable bool
}

// newShares prepares the socket directory, removing sockets a previous run left.
func newShares(state string, reg *checkout.Registry, e *export.Export) (*shares, error) {
	dir := filepath.Join(state, "shares")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &shares{dir: dir, reg: reg, e: e, on: map[string]share{}}, nil
}

// Share starts (or replaces) a checkout's remote-fs socket.
func (s *shares) Share(_ context.Context, args api.ShareArgs) (api.ShareReply, error) {
	if _, ok := s.reg.Gen(args.Checkout); !ok {
		return api.ShareReply{}, &checkout.Error{Code: "not_found", Message: "no checkout " + args.Checkout}
	}
	sock := filepath.Join(s.dir, args.Checkout+".sock")
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.on[args.Checkout]; ok {
		if old.writable == args.Writable {
			return api.ShareReply{Socket: sock, Writable: args.Writable}, nil
		}
		old.l.Close()
		delete(s.on, args.Checkout)
	}
	l, err := remotefs.Listen(sock)
	if err != nil {
		return api.ShareReply{}, &checkout.Error{Code: "io", Message: err.Error()}
	}
	s.on[args.Checkout] = share{l: l, writable: args.Writable}
	go func() {
		err := remotefs.Serve(l, remotefs.Share{FS: s.e.Nodes(), Checkout: args.Checkout, Writable: args.Writable}, remotefs.Options{Logf: log.Printf, Quiesce: s.reg.Quiesce})
		if err != nil {
			log.Printf("share %s: %v", args.Checkout, err)
		}
	}()
	return api.ShareReply{Socket: sock, Writable: args.Writable}, nil
}

// Close stops a checkout's share, if any; removed checkouts call it.
func (s *shares) Close(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh, ok := s.on[name]; ok {
		sh.l.Close()
		delete(s.on, name)
	}
}

// CloseAll stops every share at shutdown.
func (s *shares) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, sh := range s.on {
		sh.l.Close()
		delete(s.on, name)
	}
}
