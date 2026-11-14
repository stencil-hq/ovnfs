package object

import (
	"container/list"
	"sync"
)

// deltaBaseCacheLimit bounds the delta bases every [DB] in the process keeps
// together. git's per-process default (core.deltaBaseCacheLimit) is 96 MiB,
// but a daemon keeps one DB per work tree open for its whole life; bases
// mostly serve tree walks and short read bursts, which a few MiB cover.
const deltaBaseCacheLimit = 16 << 20

// deltaBases is the process-wide delta base cache all databases share, so the
// budget does not grow with the number of open repositories. Keys name the
// pack, which is unique to its DB.
var deltaBases = newDeltaCache(deltaBaseCacheLimit)

// baseKey names a pack entry.
type baseKey struct {
	pack *pack
	off  int64
}

type cachedBase struct {
	key  baseKey
	kind Kind
	data []byte
}

// deltaCache keeps recently reconstructed delta bases, least recently used
// first out, so reading many objects that share a chain does not re-inflate
// and re-apply it each time. Cached data is shared and never modified.
type deltaCache struct {
	mu    sync.Mutex
	limit int
	size  int
	lru   list.List // of *cachedBase, most recent at front
	byKey map[baseKey]*list.Element
}

func newDeltaCache(limit int) *deltaCache {
	return &deltaCache{limit: limit, byKey: make(map[baseKey]*list.Element)}
}

func (c *deltaCache) get(k baseKey) (Kind, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[k]
	if !ok {
		return 0, nil, false
	}
	c.lru.MoveToFront(e)
	b := e.Value.(*cachedBase)
	return b.kind, b.data, true
}

func (c *deltaCache) put(k baseKey, kind Kind, data []byte) {
	if len(data) > c.limit/4 {
		return // one huge base would evict everything useful
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if k.pack.dropped {
		return // a read finishing after its pack was retired
	}
	if e, ok := c.byKey[k]; ok {
		c.lru.MoveToFront(e)
		return
	}
	c.byKey[k] = c.lru.PushFront(&cachedBase{key: k, kind: kind, data: data})
	c.size += len(data)
	for c.size > c.limit {
		b := c.lru.Remove(c.lru.Back()).(*cachedBase)
		delete(c.byKey, b.key)
		c.size -= len(b.data)
	}
}

// dropPack forgets the entries of a pack that is going away, and refuses its
// later ones, so the shared cache does not pin a closed database's memory.
func (c *deltaCache) dropPack(p *pack) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.dropped = true
	for e := c.lru.Front(); e != nil; {
		next := e.Next()
		if b := e.Value.(*cachedBase); b.key.pack == p {
			c.lru.Remove(e)
			delete(c.byKey, b.key)
			c.size -= len(b.data)
		}
		e = next
	}
}
