package rev

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// Commit-graph constants from gitformat-commit-graph.
const (
	graphNoParent    = 0x70000000
	graphEdgeFlag    = 0x80000000 // second parent slot: index into EDGE
	graphLastEdge    = 0x80000000 // EDGE entry: last parent
	graphOverflowBit = 0x80000000 // GDA2 entry: index into GDO2
	graphHeaderSize  = 8
	graphChunkEntry  = 12
	graphFanoutSize  = 256 * 4
	graphMaxLayers   = 512 // sanity bound on chain length
)

var errBadGraph = errors.New("malformed commit-graph")

// genInfinity is the generation of commits the graph does not cover.
const genInfinity = ^uint64(0)

// commitGraph is a repository's commit-graph: one file or a chain of
// layers, base first. Positions are global across layers.
type commitGraph struct {
	format object.Format
	layers []*graphLayer
	// v2 reports corrected commit dates in every layer; otherwise the
	// topological levels (v1) are the generation numbers.
	v2 bool
}

type graphLayer struct {
	data   []byte
	unmap  func() error
	base   uint32 // commits in the layers below
	n      uint32
	fanout []byte
	oids   []byte
	cdat   []byte
	gda2   []byte
	gdo2   []byte
	edge   []byte
}

// graphCommit is one commit's record.
type graphCommit struct {
	parents []uint32 // global positions
	gen     uint64
	date    int64
}

// openCommitGraph loads the commit-graph from the first of dirs (object
// directories, own first, then alternates) holding one: a single
// info/commit-graph file, else info/commit-graphs/commit-graph-chain whose
// layers may live in any of dirs. It returns nil, nil when there is none.
// A malformed graph is ignored (nil, nil) as git ignores it with a warning:
// the commits themselves are still there to parse.
func openCommitGraph(dirs []string, f object.Format) (*commitGraph, error) {
	for _, dir := range dirs {
		single := filepath.Join(dir, "info", "commit-graph")
		if _, err := os.Stat(single); err == nil {
			g := &commitGraph{format: f}
			if err := g.addLayer(single, 0); err != nil {
				g.close()
				return nil, ignoreBadGraph(err)
			}
			g.finish()
			return g, nil
		}
		chain := filepath.Join(dir, "info", "commit-graphs", "commit-graph-chain")
		hashes, err := readChain(chain)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, ignoreBadGraph(err)
		}
		g, err := openChain(dirs, hashes, f)
		if err != nil {
			return nil, ignoreBadGraph(err)
		}
		return g, nil
	}
	return nil, nil
}

func ignoreBadGraph(err error) error {
	if errors.Is(err, errBadGraph) {
		return nil
	}
	return err
}

func readChain(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var hashes []string
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !isHex(line) {
			return nil, fmt.Errorf("%w: bad chain line %q", errBadGraph, line)
		}
		hashes = append(hashes, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(hashes) == 0 || len(hashes) > graphMaxLayers {
		return nil, fmt.Errorf("%w: chain of %d layers", errBadGraph, len(hashes))
	}
	return hashes, nil
}

func openChain(dirs []string, hashes []string, f object.Format) (*commitGraph, error) {
	g := &commitGraph{format: f}
	for i, h := range hashes {
		path := ""
		for _, dir := range dirs {
			p := filepath.Join(dir, "info", "commit-graphs", "graph-"+h+".graph")
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
		if path == "" {
			g.close()
			return nil, fmt.Errorf("%w: missing layer %s", errBadGraph, h)
		}
		if err := g.addLayer(path, i); err != nil {
			g.close()
			return nil, err
		}
	}
	g.finish()
	return g, nil
}

func (g *commitGraph) finish() {
	g.v2 = true
	for _, l := range g.layers {
		if l.gda2 == nil {
			g.v2 = false
		}
	}
}

func (g *commitGraph) close() {
	for _, l := range g.layers {
		if l.unmap != nil {
			l.unmap()
		}
	}
	g.layers = nil
}

// addLayer maps one graph file and checks its header and chunks; depth is
// the number of layers it must sit on.
func (g *commitGraph) addLayer(path string, depth int) error {
	data, unmap, err := mapped.Map(path)
	if err != nil {
		return err
	}
	l := &graphLayer{data: data, unmap: unmap}
	g.layers = append(g.layers, l)
	hashSize := g.format.Size()
	if len(data) < graphHeaderSize+graphChunkEntry+hashSize || !bytes.Equal(data[:4], []byte("CGPH")) || data[4] != 1 {
		return fmt.Errorf("%w: %s: bad header", errBadGraph, path)
	}
	wantHash := byte(1)
	if g.format == object.SHA256 {
		wantHash = 2
	}
	if data[5] != wantHash {
		return fmt.Errorf("%w: %s: hash version %d", errBadGraph, path, data[5])
	}
	chunks, bases := int(data[6]), int(data[7])
	if bases != depth {
		return fmt.Errorf("%w: %s: %d base graphs, expected %d", errBadGraph, path, bases, depth)
	}
	tocEnd := graphHeaderSize + (chunks+1)*graphChunkEntry
	end := uint64(len(data) - hashSize)
	if tocEnd > int(end) {
		return fmt.Errorf("%w: %s: truncated chunk table", errBadGraph, path)
	}
	var base []byte
	for i := range chunks {
		e := data[graphHeaderSize+i*graphChunkEntry:]
		id := string(e[:4])
		off := binary.BigEndian.Uint64(e[4:12])
		next := binary.BigEndian.Uint64(e[16:24])
		if off < uint64(tocEnd) || next < off || next > end {
			return fmt.Errorf("%w: %s: bad chunk offset", errBadGraph, path)
		}
		chunk := data[off:next]
		switch id {
		case "OIDF":
			l.fanout = chunk
		case "OIDL":
			l.oids = chunk
		case "CDAT":
			l.cdat = chunk
		case "GDA2":
			l.gda2 = chunk
		case "GDO2":
			l.gdo2 = chunk
		case "EDGE":
			l.edge = chunk
		case "BASE":
			base = chunk
		}
	}
	if len(l.fanout) != graphFanoutSize || l.oids == nil || l.cdat == nil {
		return fmt.Errorf("%w: %s: missing required chunk", errBadGraph, path)
	}
	l.n = binary.BigEndian.Uint32(l.fanout[255*4:])
	n := int(l.n)
	if len(l.oids) != n*hashSize || len(l.cdat) != n*(hashSize+16) ||
		l.gda2 != nil && len(l.gda2) != n*4 || len(l.edge)%4 != 0 || len(l.gdo2)%8 != 0 {
		return fmt.Errorf("%w: %s: chunk sizes disagree with %d commits", errBadGraph, path, n)
	}
	if len(base) != depth*hashSize {
		return fmt.Errorf("%w: %s: BASE chunk size", errBadGraph, path)
	}
	for i := range depth {
		below := g.layers[i].data
		if !bytes.Equal(base[i*hashSize:(i+1)*hashSize], below[len(below)-hashSize:]) {
			return fmt.Errorf("%w: %s: BASE does not match the chain", errBadGraph, path)
		}
	}
	if depth > 0 {
		prev := g.layers[depth-1]
		if uint64(prev.base)+uint64(prev.n)+uint64(l.n) >= 1<<31 {
			return fmt.Errorf("%w: %s: too many commits", errBadGraph, path)
		}
		l.base = prev.base + prev.n
	}
	return nil
}

func (g *commitGraph) total() uint32 {
	last := g.layers[len(g.layers)-1]
	return last.base + last.n
}

// find returns id's global position.
func (g *commitGraph) find(id object.ID) (uint32, bool) {
	raw := id.Raw()
	hashSize := len(raw)
	for i := len(g.layers) - 1; i >= 0; i-- {
		l := g.layers[i]
		lo := uint32(0)
		if raw[0] > 0 {
			lo = binary.BigEndian.Uint32(l.fanout[(int(raw[0])-1)*4:])
		}
		hi := binary.BigEndian.Uint32(l.fanout[int(raw[0])*4:])
		if hi > l.n || lo > hi {
			continue
		}
		for lo < hi {
			mid := lo + (hi-lo)/2
			c := bytes.Compare(l.oids[int(mid)*hashSize:int(mid+1)*hashSize], raw)
			switch {
			case c == 0:
				return l.base + mid, true
			case c < 0:
				lo = mid + 1
			default:
				hi = mid
			}
		}
	}
	return 0, false
}

func (g *commitGraph) layerOf(pos uint32) (*graphLayer, uint32, bool) {
	for i := len(g.layers) - 1; i >= 0; i-- {
		l := g.layers[i]
		if pos >= l.base {
			if pos-l.base >= l.n {
				return nil, 0, false
			}
			return l, pos - l.base, true
		}
	}
	return nil, 0, false
}

// id returns the commit at global position pos.
func (g *commitGraph) id(pos uint32) (object.ID, error) {
	l, i, ok := g.layerOf(pos)
	if !ok {
		return object.ID{}, fmt.Errorf("%w: position %d out of range", errBadGraph, pos)
	}
	hashSize := g.format.Size()
	return object.IDFromRaw(g.format, l.oids[int(i)*hashSize:int(i+1)*hashSize])
}

// commit decodes the record at global position pos.
func (g *commitGraph) commit(pos uint32) (graphCommit, error) {
	l, i, ok := g.layerOf(pos)
	if !ok {
		return graphCommit{}, fmt.Errorf("%w: position %d out of range", errBadGraph, pos)
	}
	hashSize := g.format.Size()
	rec := l.cdat[int(i)*(hashSize+16):]
	p1 := binary.BigEndian.Uint32(rec[hashSize:])
	p2 := binary.BigEndian.Uint32(rec[hashSize+4:])
	w1 := binary.BigEndian.Uint32(rec[hashSize+8:])
	w2 := binary.BigEndian.Uint32(rec[hashSize+12:])
	var c graphCommit
	c.date = int64(w1&3)<<32 | int64(w2)
	total := g.total()
	addParent := func(p uint32) error {
		if p >= total {
			return fmt.Errorf("%w: parent position %d out of range", errBadGraph, p)
		}
		c.parents = append(c.parents, p)
		return nil
	}
	if p1 != graphNoParent {
		if err := addParent(p1); err != nil {
			return graphCommit{}, err
		}
	}
	switch {
	case p2 == graphNoParent:
	case p2&graphEdgeFlag != 0:
		for k := int(p2 &^ graphEdgeFlag); ; k++ {
			if (k+1)*4 > len(l.edge) {
				return graphCommit{}, fmt.Errorf("%w: edge list overrun", errBadGraph)
			}
			e := binary.BigEndian.Uint32(l.edge[k*4:])
			if err := addParent(e &^ graphLastEdge); err != nil {
				return graphCommit{}, err
			}
			if e&graphLastEdge != 0 {
				break
			}
		}
	default:
		if err := addParent(p2); err != nil {
			return graphCommit{}, err
		}
	}
	if !g.v2 {
		c.gen = uint64(w1 >> 2)
		if c.gen == 0 {
			c.gen = genInfinity // written by a git that did not compute levels
		}
		return c, nil
	}
	off := uint64(binary.BigEndian.Uint32(l.gda2[int(i)*4:]))
	if off&graphOverflowBit != 0 {
		k := int(off &^ graphOverflowBit)
		if (k+1)*8 > len(l.gdo2) {
			return graphCommit{}, fmt.Errorf("%w: generation overflow index out of range", errBadGraph)
		}
		off = binary.BigEndian.Uint64(l.gdo2[k*8:])
	}
	c.gen = uint64(c.date) + off
	return c, nil
}
