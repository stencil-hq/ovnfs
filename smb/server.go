// Package smb serves an overlay filesystem over SMB2 and SMB3 Direct TCP.
package smb

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// Server exports one filesystem share and accepts NTLM-authenticated connections.
type Server struct {
	FS    Backend
	Share string
	Users map[string]string
	// MountRoots are the paths that name the share's root to its clients,
	// slash-separated (O:/, //127.0.0.1/ovnfs/); see [ntinfo.MountRelative].
	MountRoots []string
	Logf       func(format string, args ...any)
	guid       [16]byte
	st         state
}

// state holds the filesystem-wide open, locking, and notification registries.
type state struct {
	opens  openRegistry
	locks  lockManager
	notify notifyHub
	// epoch counts operations that may have changed any file's metadata; an
	// open's cached metadata is valid only within one epoch (see [open.stat]).
	epoch atomic.Uint64
}

// changed starts a new metadata epoch; call it before and after a change.
func (s *state) changed() { s.epoch.Add(1) }

// NotifyAdded reports a newly visible export entry.
const NotifyAdded uint32 = notifyAdded

// NotifyRemoved reports a removed export entry.
const NotifyRemoved uint32 = notifyRemoved

// NotifyModified reports changed contents or attributes.
const NotifyModified uint32 = notifyModified

// Changed recalls cached contents and notifies share watchers after a backend
// change; callers must not hold backend or open locks while cached handles close.
func (s *Server) Changed(p string, action uint32) {
	s.st.changed()
	breaks := s.st.opens.breakDescendants(p)
	if len(breaks) != 0 {
		s.st.opens.waitBreaks(breaks, nil)
	}
	s.st.notify.changed(p, action)
}

// Retire invalidates open handles after their backend namespace is retired;
// callers must release backend locks before clients drop cached handles.
func (s *Server) Retire(p string) {
	s.Changed(p, NotifyRemoved)
	p = strings.TrimSuffix(p, "/")
	g := &s.st.opens
	var retired []*open
	g.mu.Lock()
	for _, q := range g.under(p) {
		for o := range g.byPath[q] {
			retired = append(retired, o)
		}
	}
	g.mu.Unlock()
	for _, o := range retired {
		o.tree.mu.Lock()
		delete(o.tree.files, o.id)
		o.tree.mu.Unlock()
		o.closeFn()
	}
}

// Serve accepts Direct TCP connections until the listener returns a fatal error.
// On a loopback listener, whose clients are the daemon's own user, files without
// Unix IDs are owned by the daemon's token user and primary group, so Windows
// clients such as Git's ownership check see them as their own; elsewhere the
// daemon's identity says nothing about the client's.
func (s *Server) Serve(l net.Listener) error {
	if s.FS == nil {
		return errors.New("smb: filesystem is required")
	}
	if s.Share == "" {
		s.Share = "ovnfs"
	}
	if _, err := rand.Read(s.guid[:]); err != nil {
		return err
	}
	var owner, group []byte
	if a, ok := l.Addr().(*net.TCPAddr); ok && a.IP.IsLoopback() {
		var err error
		if owner, group, err = processSIDs(); err != nil {
			return fmt.Errorf("smb: process owner: %w", err)
		}
	}
	var delay time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if delay > time.Second {
					delay = time.Second
				}
				if s.Logf != nil {
					s.Logf("smb accept: %v", err)
				}
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		c := &conn{srv: s, nc: nc, owner: owner, group: group, maxRead: 8 << 20, maxWrite: 8 << 20, maxTrans: 1 << 20}
		go c.serve()
	}
}
