package checkout

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strconv"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// eolAction is what git's clean conversion does to a path's line endings
// (convert.c's crlf_action, as far as storing a blob is concerned).
type eolAction int

const (
	eolKeep eolAction = iota // stored as is
	eolText                  // every CRLF stored as LF
	eolAuto                  // as eolText when the content is text and the index blob holds no CRLF
)

// cleanAction decides a path's conversion from its text, eol, crlf, filter, ident
// and working-tree-encoding attributes as git does when it stores the path. It
// reports false for a conversion only git can perform: a filter driver with a
// command, ident, or a working-tree encoding.
func cleanAction(values []string, c git.Conversion) (eolAction, bool) {
	text, eol, crlf, filter, ident, encoding := values[0], values[1], values[2], values[3], values[4], values[5]
	if c.Filters[filter] || ident == git.AttrSet || isAttrValue(encoding) {
		return eolKeep, false
	}
	action, decided := crlfAttr(text)
	if !decided {
		action, decided = crlfAttr(crlf)
	}
	if !decided {
		switch {
		case eol == "lf" || eol == "crlf":
			return eolText, true
		case c.AutoCRLF:
			return eolAuto, true
		default:
			return eolKeep, true
		}
	}
	return action, true
}

// crlfAttr reads a text or crlf attribute; false when it leaves the decision open.
func crlfAttr(v string) (eolAction, bool) {
	switch v {
	case git.AttrSet, "input":
		return eolText, true
	case git.AttrUnset:
		return eolKeep, true
	case "auto":
		return eolAuto, true
	}
	return eolKeep, false
}

// isAttrValue reports an attribute set to a value rather than a bare state.
func isAttrValue(v string) bool {
	return v != git.AttrUnspecified && v != git.AttrSet && v != git.AttrUnset
}

// textStats counts what git's gather_stats does to tell text from binary.
type textStats struct {
	crlf, loneCR, nul, printable, nonprintable int
}

func statsOf(data []byte) textStats {
	var s textStats
	for i := 0; i < len(data); i++ {
		switch c := data[i]; {
		case c == '\r':
			if i+1 < len(data) && data[i+1] == '\n' {
				s.crlf++
				i++
			} else {
				s.loneCR++
			}
		case c == '\n':
		case c == 127:
			s.nonprintable++
		case c < 32:
			switch c {
			case '\b', '\t', '\033', '\014':
				s.printable++
			case 0:
				s.nul++
				s.nonprintable++
			default:
				s.nonprintable++
			}
		default:
			s.printable++
		}
	}
	// A trailing ^Z (DOS end of file) does not make a file binary.
	if n := len(data); n > 0 && data[n-1] == '\032' {
		s.nonprintable--
	}
	return s
}

// binary is git's convert_is_binary.
func (s textStats) binary() bool {
	return s.loneCR > 0 || s.nul > 0 || s.printable>>7 < s.nonprintable
}

// eolConverter stores work-tree contents of one checkout's paths the way git
// would, for patches git applies to its blobs.
type eolConverter struct {
	ctx     context.Context
	repo    *git.Repo
	actions map[string]eolAction
	index   map[string]string // the captured index's blob ids, read on first need
	indexAt string            // the captured index file
	objects *git.Objects
	crlf    map[string]bool // index paths whose blob is text holding CRLF
}

// newEOLConverter decides how git stores each of paths in view's root
// repository. A path git would pass through a filter, ident or encoding, or one in
// another repository, is refused as unsupported: only git can store it.
func newEOLConverter(ctx context.Context, view *base.Base, paths []string) (*eolConverter, error) {
	repo := view.Domains().Repo("")
	if repo == nil || len(paths) == 0 {
		return nil, nil
	}
	for _, p := range paths {
		if view.Domains().RepoOf(p) != repo {
			return nil, failure("unsupported", "a git patch of %s needs its own repository's rules", p)
		}
	}
	conv, err := repo.Conversion(ctx)
	if err != nil {
		return nil, err
	}
	c := &eolConverter{ctx: ctx, repo: repo, actions: make(map[string]eolAction, len(paths))}
	for _, rr := range view.Repos() {
		if rr.Path == "" {
			c.indexAt = rr.Index
		}
	}
	attrs, err := repo.Attributes([]string{"text", "eol", "crlf", "filter", "ident", "working-tree-encoding"}, git.AttributeOptions{})
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		values, err := attrs.Check(p)
		if err != nil {
			return nil, err
		}
		action, ok := cleanAction(values, conv)
		if !ok {
			return nil, failure("unsupported", "git stores %s through a filter, ident or encoding", p)
		}
		c.actions[p] = action
	}
	return c, nil
}

// Close releases the object reader, if one was opened.
func (c *eolConverter) Close() {
	if c != nil && c.objects != nil {
		c.objects.Close()
	}
}

// blob returns data as git would store it at p, and its git blob id; f describes
// data in the checkout. A side that git stores unchanged keeps f.
func (c *eolConverter) blob(p string, f store.File, data []byte) (store.File, []byte, error) {
	if c == nil || f.ID == "" || f.Mode == store.ModeSymlink {
		return f, data, nil
	}
	switch c.actions[p] {
	case eolKeep:
		return f, data, nil
	case eolText:
		if bytes.IndexByte(data, '\r') < 0 {
			return f, data, nil
		}
		data = stripCRLF(data)
	case eolAuto:
		s := statsOf(data)
		if s.crlf == 0 || s.binary() {
			return f, data, nil
		}
		crlf, err := c.indexHasCRLF(p)
		if err != nil {
			return f, nil, err
		}
		if crlf {
			return f, data, nil
		}
		// A text guess admits no lone CR, so every CR goes.
		data = bytes.ReplaceAll(data, []byte{'\r'}, nil)
	}
	f.ID = blobID(data)
	return f, data, nil
}

// indexHasCRLF reports whether the captured index's blob at p is text holding a
// CRLF, in which case git stores the file unconverted (has_crlf_in_index).
func (c *eolConverter) indexHasCRLF(p string) (bool, error) {
	if c.index == nil {
		index, err := c.repo.ReadIndex(c.indexAt)
		if err != nil {
			return false, err
		}
		c.index = make(map[string]string, len(index.Entries))
		for _, e := range index.Entries {
			c.index[e.Path] = e.OID
		}
		c.crlf = map[string]bool{}
	}
	oid, ok := c.index[p]
	if !ok {
		return false, nil
	}
	if crlf, ok := c.crlf[p]; ok {
		return crlf, nil
	}
	if c.objects == nil {
		objects, err := c.repo.Objects()
		if err != nil {
			return false, err
		}
		c.objects = objects
	}
	data, err := c.objects.Blob(oid)
	if err != nil {
		return false, err
	}
	s := statsOf(data)
	crlf := s.crlf > 0 && !s.binary()
	c.crlf[p] = crlf
	return crlf, nil
}

// stripCRLF drops each CR that ends a line.
func stripCRLF(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i, b := range data {
		if b == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			continue
		}
		out = append(out, b)
	}
	return out
}

// blobID is the git object id of a blob holding data.
func blobID(data []byte) store.OID {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return store.OID(hex.EncodeToString(h.Sum(nil)))
}
