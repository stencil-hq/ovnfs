package checkout

import (
	"iter"
	"strings"
)

// pathSet is a set of slash paths stored as a tree of components, so
// everything below a path is found, and removed, without visiting the rest.
type pathSet struct {
	root pathNode
}

type pathNode struct {
	in   bool // the path ending here is a member
	kids map[string]*pathNode
}

// node returns p's node, creating missing ones when create is set; nil when
// absent.
func (s *pathSet) node(p string, create bool) *pathNode {
	n := &s.root
	for rest := p; ; {
		name, tail, more := strings.Cut(rest, "/")
		k := n.kids[name]
		if k == nil {
			if !create {
				return nil
			}
			if n.kids == nil {
				n.kids = make(map[string]*pathNode)
			}
			k = &pathNode{}
			n.kids[name] = k
		}
		n = k
		if !more {
			return n
		}
		rest = tail
	}
}

// add makes p a member.
func (s *pathSet) add(p string) { s.node(p, true).in = true }

// remove drops p, keeping members below it.
func (s *pathSet) remove(p string) {
	if n := s.node(p, false); n != nil {
		n.in = false
	}
}

// removeUnder drops every member strictly below p, calling fn (if not nil)
// with each one first.
func (s *pathSet) removeUnder(p string, fn func(string)) {
	n := s.node(p, false)
	if n == nil || n.kids == nil {
		return
	}
	if fn != nil {
		walkPaths(n, p, func(q string) bool { fn(q); return true })
	}
	n.kids = nil
}

// all yields every member, in no particular order.
func (s *pathSet) all() iter.Seq[string] {
	return func(yield func(string) bool) {
		for name, k := range s.root.kids {
			if !walkMember(k, name, yield) {
				return
			}
		}
	}
}

// walkPaths yields the members strictly below n, whose path is p.
func walkPaths(n *pathNode, p string, yield func(string) bool) bool {
	for name, k := range n.kids {
		if !walkMember(k, p+"/"+name, yield) {
			return false
		}
	}
	return true
}

// walkMember yields n's path p if it is a member, then the members below it.
func walkMember(n *pathNode, p string, yield func(string) bool) bool {
	if n.in && !yield(p) {
		return false
	}
	return walkPaths(n, p, yield)
}
