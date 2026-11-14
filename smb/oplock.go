package smb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// oplockGrant is shared by every open belonging to the same client file lease.
// A returned break snapshot retains source and done so a subsequent break cannot
// redirect an earlier waiter.
type oplockGrant struct {
	owner     *open
	c         *conn
	lease     bool
	key       [16]byte
	parentKey [16]byte
	state     uint32
	level     uint8
	version   int
	epoch     uint16
	done      chan struct{}
	timer     *time.Timer
	source    *oplockGrant
	activeIO  uint64
	acked     bool
}

// ErrWriteCacheLost fences the server after data may have been lost in a client
// write cache. Restarting/recovering the share is an operator decision; a late
// ACK, a successful retry, or another transport cannot erase this fault.
var ErrWriteCacheLost = errors.New("smb: client write cache could not be safely drained")

// Quiesce prevents WRITE grants and promotions until its idempotent release is
// called. It recalls all existing WRITE leases, including unpublished CREATEs,
// and waits for ACK/CLOSE and in-flight backend writes. Gates are counted, not
// exclusive: nested callers can coexist without blocking client flush traffic.
// A missing deadline is bounded to five seconds. Failed calls release only their
// gate; an outstanding break remains pending and dirty-owner loss stays fenced.
func (s *Server) Quiesce(ctx context.Context) (func(), error) {
	if !s.WriteCache {
		return func() {}, nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	g := &s.st.opens
	release := g.holdWriteGrants()
	// Namespace bookkeeping may itself be waiting on a backend syscall. Do
	// not make the caller's deadline depend on acquiring its mutex.
	result := make(chan error, 1)
	go func() {
		if err := ctx.Err(); err != nil {
			result <- err
			return
		}
		breaks := g.breakCaching("", true, func(grant *oplockGrant) bool { return grant.state&4 != 0 })
		for _, pending := range breaks {
			select {
			case <-pending.done:
			case <-ctx.Done():
				result <- ctx.Err()
				return
			}
		}
		result <- g.cacheError()
	}()
	select {
	case err := <-result:
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			return release, nil
		}
		release()
		return nil, err
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// withWriteBarrier recalls only caches whose contents the operation cannot
// coordinate locally. Ordinary metadata/EOF operations preserve their exact
// owning lease, just like WRITE; they may themselves be part of a break's flush.
// Directory metadata preserves that client's child leases. Server-side content
// reads (COPYCHUNK) pass nil to recall every owner, including the requester.
// External Quiesce never exempts an owner.
func (r *request) withWriteBarrier(o *open, descendants, preserveOwner bool, run func() ([]byte, uint32)) ([]byte, uint32) {
	g := &r.c.srv.st.opens
	if o != nil && !descendants && preserveOwner {
		g.mu.Lock()
		owner := g.grants[o]
		owned := owner != nil && owner.lease && owner.c.clientGUID == r.c.clientGUID
		failed := g.writeFault != nil
		g.mu.Unlock()
		if failed {
			return nil, statusUnsuccessful
		}
		if owned {
			// This live lease handle already excludes a different WRITE
			// owner. Do not deny unrelated CREATEs caching with a global gate
			// for a metadata update that coordinates only this client's cache.
			r.cacheProtected = true
			return run()
		}
	}
	release := g.holdWriteGrants()
	p := ""
	var owner *oplockGrant
	if o != nil {
		g.mu.Lock()
		p, owner = o.path, g.grants[o]
		g.mu.Unlock()
	}
	breaks := g.breakCaching(p, descendants, func(grant *oplockGrant) bool {
		if grant.state&4 == 0 {
			return false
		}
		if preserveOwner {
			if descendants && grant.c.clientGUID == r.c.clientGUID || !descendants && grant == owner {
				return false
			}
		}
		return true
	})
	finish := func(cancelled <-chan struct{}) ([]byte, uint32) {
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, pending := range breaks {
			select {
			case <-pending.done:
			case <-cancelled:
				return nil, statusCancelled
			case <-ctx.Done():
				return nil, statusUnsuccessful
			}
		}
		if g.cacheError() != nil {
			return nil, statusUnsuccessful
		}
		r.cacheProtected = true
		return run()
	}
	if len(breaks) == 0 || r.compound != nil {
		return finish(nil)
	}
	async := r.async()
	go func() {
		body, status := finish(async.cancelled)
		r.complete(async, body, status)
	}()
	return nil, statusPending
}

func (g *openRegistry) holdWriteGrants() func() {
	g.writeGates.Add(1)
	g.writeEpoch.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() { g.writeGates.Add(-1) })
	}
}

func (g *openRegistry) cacheError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writeFault
}

// failWriteLocked must precede any transition that forgets a dirty owner.
func (g *openRegistry) failWriteLocked(grant *oplockGrant, cause error) {
	if grant != nil && grant.state&4 != 0 && g.writeFault == nil {
		g.writeFault = fmt.Errorf("%w: %v", ErrWriteCacheLost, cause)
	}
}

func (g *openRegistry) loseCaching(o *open, cause error) {
	g.mu.Lock()
	grant := g.grants[o]
	g.failWriteLocked(grant, cause)
	if grant != nil && grant.state&4 != 0 {
		g.finishBreakLocked(grant)
	}
	g.mu.Unlock()
}

// beginCacheIO registers backend WRITE/FLUSH before taking the open lock. An ACK
// may revoke caching only after these operations complete successfully.
func (g *openRegistry) beginCacheIO(o *open) *oplockGrant {
	g.mu.Lock()
	defer g.mu.Unlock()
	grant := g.grants[o]
	if grant == nil || grant.state&4 == 0 {
		return nil
	}
	grant.activeIO++
	return grant
}

func (g *openRegistry) endCacheIO(grant *oplockGrant, err error) {
	if grant == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.failWriteLocked(grant, err)
	}
	grant.activeIO--
	if err != nil || grant.acked && grant.activeIO == 0 {
		g.finishBreakLocked(grant)
	}
}

// init registers both classic oplock and lease break acknowledgements.
func init() { handlers[cmdOplockBreak] = handleOplockBreak }

// grantCaching grants caching only to an isolated open or handles sharing a client lease key.
func (g *openRegistry) grantCaching(c *conn, o *open, a createArgs) (uint16, uint32) {
	o.mu.Lock()
	p := o.path
	o.mu.Unlock()
	policyEpoch := g.writeEpoch.Load()
	cacheable := c.srv.FS.Cacheable(p)
	writeCacheable := false
	if backend, ok := c.srv.FS.(interface{ WriteCacheable(string) bool }); ok && c.srv.WriteCache {
		writeCacheable = backend.WriteCacheable(p)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if previous := g.grants[o]; previous != nil {
		return previous.epoch, previous.leaseFlags()
	}
	lease := a.oplock == 255 && a.leaseVersion != 0
	isolated := true
	var grant *oplockGrant
	// Include grants whose CREATE has not published its handle yet. Quiesce
	// uses the same registry, so a concurrent grant cannot escape its snapshot.
	for other, previous := range g.grants {
		other.mu.Lock()
		samePath := other.path == p
		other.mu.Unlock()
		if other == o || !samePath {
			continue
		}
		if lease && previous != nil && previous.lease && previous.key == a.leaseKey && previous.c.clientGUID == c.clientGUID {
			grant = previous
			continue
		}
		isolated = false
	}
	if grant == nil {
		grant = &oplockGrant{owner: o, c: c, lease: lease, key: a.leaseKey, version: a.leaseVersion}
		if lease && a.leaseVersion == 52 {
			grant.epoch = a.leaseEpoch
			if a.leaseFlags&4 != 0 {
				grant.parentKey = a.leaseParentKey
			}
		}
	}
	if g.grants == nil {
		g.grants = make(map[*open]*oplockGrant)
	}
	g.grants[o] = grant
	if !o.isDir {
		if grant.lease {
			o.mu.Lock()
			o.oplock = 255
			o.leaseKey = grant.key
			o.mu.Unlock()
			requested := a.leaseState & 3
			// A protected mutation (notably a hardlink) may have invalidated
			// the hook's answer while it ran outside the registry lock.
			if writeCacheable && g.writeGates.Load() == 0 && g.writeEpoch.Load() == policyEpoch && g.writeFault == nil {
				requested |= a.leaseState & 4
			}
			// A reopen cannot cancel a pending break, combine incomparable states,
			// or replace the server's epoch with a client-supplied epoch.
			if cacheable && isolated && c.dialect >= 0x210 && requested&1 != 0 && grant.done == nil && requested != grant.state && requested&grant.state == grant.state {
				grant.state = requested
				if grant.version == 52 {
					grant.epoch++
				}
			}
			// A lease key names one path, so its opens are all at p, besides
			// o, which is not published yet.
			o.mu.Lock()
			o.leaseState = grant.state
			o.mu.Unlock()
			for other := range g.byPath[p] {
				if other != o && g.grants[other] == grant {
					other.mu.Lock()
					other.leaseState = grant.state
					other.mu.Unlock()
				}
			}
		} else if cacheable && isolated && (a.oplock == 1 || a.oplock == 8 || a.oplock == 9) {
			// Exclusive and batch oplocks also allow unsafe client write caching.
			grant.level = 1
			o.mu.Lock()
			o.oplock = 1
			o.mu.Unlock()
		}
	}
	return grant.epoch, grant.leaseFlags()
}

// leaseFlags describes the current lease, not a later reopen's request.
func (grant *oplockGrant) leaseFlags() uint32 {
	var flags uint32
	if grant.done != nil {
		flags |= 2
	}
	if grant.parentKey != [16]byte{} {
		flags |= 4
	}
	return flags
}

// breakConflicting revokes other lease keys before a newly opened handle can access their data.
func (g *openRegistry) breakConflicting(c *conn, p string, key [16]byte, lease bool) []*oplockGrant {
	return g.breakCaching(p, false, func(grant *oplockGrant) bool {
		return !(lease && grant.lease && key == grant.key && c.clientGUID == grant.c.clientGUID)
	})
}

func withinPath(p, root string) bool {
	return root == "" || root == "." || p == root ||
		len(p) > len(root) && strings.HasPrefix(p, root) && p[len(root)] == '/'
}

// breakDescendants revokes all caches on a path and its children, including the caller's lease key.
func (g *openRegistry) breakDescendants(p string) []*oplockGrant {
	return g.breakCaching(strings.TrimSuffix(p, "/"), true, func(*oplockGrant) bool { return true })
}

// breakNamespaceSource preserves a lease owner's source inode through rename
// or delete disposition. The client coordinates its own cached data and may
// hold the vnode lock while awaiting this request, so recalling that lease
// would deadlock its flush. Directory renames likewise preserve that client's
// descendants. Targets being replaced and external Quiesce never use this
// exemption; delete disposition retains the inode until its last handle closes.
func (g *openRegistry) breakNamespaceSource(c *conn, p string, key [16]byte, lease, directory bool) []*oplockGrant {
	if !c.srv.WriteCache {
		return g.breakDescendants(p)
	}
	return g.breakCaching(strings.TrimSuffix(p, "/"), true, func(grant *oplockGrant) bool {
		return !(grant.lease && grant.c.clientGUID == c.clientGUID && (directory || lease && grant.key == key))
	})
}

// breakCaching selects each matching lease of the opens at p (and, with
// subtree, below p) once, snapshots its wait channel, and sends outside registry locks.
func (g *openRegistry) breakCaching(p string, subtree bool, matches func(*oplockGrant) bool) []*oplockGrant {
	g.mu.Lock()
	var breaks []*oplockGrant
	var notifications []struct {
		c    *conn
		body []byte
	}
	for o, grant := range g.grants {
		o.mu.Lock()
		q := o.path
		o.mu.Unlock()
		if q != p && !(subtree && withinPath(q, p)) {
			continue
		}
		if grant == nil || grant.state == 0 && grant.level == 0 {
			continue
		}
		if !matches(grant) {
			continue
		}
		selected := false
		for _, pending := range breaks {
			if pending.source == grant {
				selected = true
				break
			}
		}
		if selected {
			continue
		}
		if grant.done == nil {
			grant.done = make(chan struct{})
			if (grant.lease && grant.state&6 != 0) || (!grant.lease && grant.level != 1) {
				done := grant.done
				grant.timer = time.AfterFunc(35*time.Second, func() {
					g.mu.Lock()
					if grant.done == done {
						g.failWriteLocked(grant, errors.New("lease break timed out"))
						g.finishBreakLocked(grant)
					}
					g.mu.Unlock()
				})
			}
			if grant.lease {
				if grant.version == 52 {
					grant.epoch++
				} else {
					grant.epoch = 0
				}
			}
			notifications = append(notifications, struct {
				c    *conn
				body []byte
			}{grant.c, grant.breakBody()})
		}
		snapshot := *grant
		snapshot.source = grant
		breaks = append(breaks, &snapshot)
		if (grant.lease && grant.state&6 == 0) || (!grant.lease && grant.level == 1) {
			g.finishBreakLocked(grant)
		}
	}
	g.mu.Unlock()
	for _, notification := range notifications {
		// Sending can block on a stalled peer or another response. Neither
		// namespace transactions nor a caller's deadline may wait for it.
		go notification.c.sendBreak(notification.body)
	}
	return breaks
}

// breakBody always revokes to NONE; only READ-only leases and level II oplocks need no ACK.
func (grant *oplockGrant) breakBody() []byte {
	if grant.lease {
		b := make([]byte, 44)
		binary.LittleEndian.PutUint16(b, 44)
		binary.LittleEndian.PutUint16(b[2:], grant.epoch)
		if grant.state&6 != 0 {
			binary.LittleEndian.PutUint32(b[4:], 1)
		}
		copy(b[8:], grant.key[:])
		binary.LittleEndian.PutUint32(b[24:], grant.state)
		return b
	}
	b := make([]byte, 24)
	binary.LittleEndian.PutUint16(b, 24)
	putOpenID(b[8:], grant.owner.id)
	return b
}

// finishBreakLocked revokes a cache group and wakes every waiter for this break.
func (g *openRegistry) finishBreakLocked(grant *oplockGrant) {
	grant.state, grant.level = 0, 0
	grant.acked = false
	for o, previous := range g.grants {
		if previous != grant {
			continue
		}
		o.mu.Lock()
		o.leaseState = 0
		if !grant.lease {
			o.oplock = 0
		}
		o.mu.Unlock()
	}
	if grant.timer != nil {
		grant.timer.Stop()
		grant.timer = nil
	}
	if grant.done != nil {
		close(grant.done)
		grant.done = nil
	}
}

// waitBreaks waits for acknowledgements or the grant-owned timeout. Cancelling an
// operation does not cancel or extend the outstanding cache break's deadline.
func (g *openRegistry) waitBreaks(breaks []*oplockGrant, cancelled <-chan struct{}) bool {
	for _, grant := range breaks {
		select {
		case <-grant.done:
		case <-cancelled:
			return false
		}
	}
	g.mu.Lock()
	ok := g.writeFault == nil
	g.mu.Unlock()
	return ok
}

// dropCaching keeps a shared lease until its last open closes. If its notification
// transport closes during a break, another surviving transport receives the break.
func (g *openRegistry) dropCaching(o *open) {
	g.mu.Lock()
	grant := g.grants[o]
	var previous, resend *conn
	var body []byte
	if grant != nil {
		var replacement *open
		o.mu.Lock()
		p := o.path
		o.mu.Unlock()
		// A lease key names one path, so the opens sharing grant are all at p.
		for other := range g.byPath[p] {
			if previous := g.grants[other]; other != o && previous == grant {
				replacement = other
				if other.tree.sess.conn != grant.c {
					break
				}
			}
		}
		if replacement == nil {
			if grant.activeIO == 0 {
				g.finishBreakLocked(grant)
			} else {
				grant.acked = true
			}
		} else if grant.owner == o {
			previous = grant.c
			grant.owner = replacement
			grant.c = replacement.tree.sess.conn
			if grant.done != nil && grant.c != previous {
				resend, body = grant.c, grant.breakBody()
			}
		}
		delete(g.grants, o)
	}
	g.mu.Unlock()
	if resend == nil {
		return
	}
	// A connection's lock is never taken under g.mu: request.async holds one
	// while it waits for a tree's lock, which publishOpen holds while it waits
	// for g.mu. A connection never reopens, so checking after the fact is safe.
	previous.mu.Lock()
	disconnected := previous.closed
	previous.mu.Unlock()
	if disconnected {
		go resend.sendBreak(body)
	}
}

// handleOplockBreak accepts only acknowledgements for breaks issued to the authenticated client.
func handleOplockBreak(r *request) ([]byte, uint32) {
	b := r.body
	if len(b) < 2 {
		return nil, statusInvalidParameter
	}
	size := binary.LittleEndian.Uint16(b)
	g := &r.c.srv.st.opens
	if size == 24 {
		if len(b) < 24 || b[2] > 1 {
			return nil, statusInvalidParameter
		}
		o, st := r.openFile(getOpenID(b[8:]))
		if st != statusSuccess {
			return nil, st
		}
		g.mu.Lock()
		grant := g.grants[o]
		if grant == nil || grant.lease || grant.done == nil || b[2] != 0 {
			g.mu.Unlock()
			return nil, statusInvalidOplockProtocol
		}
		g.finishBreakLocked(grant)
		g.mu.Unlock()
		out := make([]byte, 24)
		binary.LittleEndian.PutUint16(out, 24)
		putOpenID(out[8:], o.id)
		return out, statusSuccess
	}
	if size != 36 || len(b) < 36 {
		return nil, statusInvalidParameter
	}
	var key [16]byte
	copy(key[:], b[8:24])
	g.mu.Lock()
	var found *oplockGrant
	for _, grant := range g.grants {
		if grant.lease && grant.key == key && grant.c.clientGUID == r.c.clientGUID {
			found = grant
			break
		}
	}
	if found == nil {
		g.mu.Unlock()
		return nil, statusObjectNameNotFound
	}
	if found.done == nil {
		g.mu.Unlock()
		return nil, statusUnsuccessful
	}
	// Every break issued here targets NONE, so no caching bit is acceptable.
	if binary.LittleEndian.Uint32(b[24:]) != 0 {
		g.mu.Unlock()
		return nil, statusRequestNotAccepted
	}
	found.acked = true
	if found.activeIO == 0 {
		g.finishBreakLocked(found)
	}
	failed := g.writeFault != nil
	g.mu.Unlock()
	if failed {
		return nil, statusUnsuccessful
	}
	out := make([]byte, 36)
	binary.LittleEndian.PutUint16(out, 36)
	copy(out[8:], key[:])
	return out, statusSuccess
}

const (
	statusUnsuccessful          uint32 = 0xc0000001
	statusInvalidOplockProtocol uint32 = 0xc00000e3
)
