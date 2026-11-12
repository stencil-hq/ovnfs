package export

// Export shares persistent file identities between the NFS and SMB protocols.
type Export struct {
	fs     *nodeFS
	volume string
	rt     *Router
}

// New opens the persistent node log and wraps the checkout router. logf, if
// not nil, receives one line saying how the node ids loaded and how long
// that took.
func New(rt *Router, nodesLog, upperVolume string, logf func(format string, args ...any)) (*Export, error) {
	fs := &nodeFS{Filesystem: rt, Change: rt}
	nodes, err := openNodeTable(nodesLog, rt.ReadDir)
	if err != nil {
		return nil, err
	}
	if logf != nil {
		logf("%v", nodes.opened)
	}
	fs.nodes = nodes
	return &Export{fs: fs, volume: upperVolume, rt: rt}, nil
}

// Retire invalidates a checkout's directory and every descendant handle,
// recalling clients' cached copies first while the handles still resolve.
func (e *Export) Retire(name string) {
	e.fs.recallTree(name)
	if e.rt != nil {
		e.rt.retire(name)
	}
	e.fs.nodes.remove(name)
}

// Close flushes outstanding identity records and closes the log.
func (e *Export) Close() error { return e.fs.nodes.close() }
