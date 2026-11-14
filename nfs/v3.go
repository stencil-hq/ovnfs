package nfs

import (
	"strings"
)

// NFSv3 procedures (RFC 1813 section 3).
const (
	v3Null = iota
	v3GetAttr
	v3SetAttr
	v3Lookup
	v3Access
	v3Readlink
	v3Read
	v3Write
	v3Create
	v3Mkdir
	v3Symlink
	v3Mknod
	v3Remove
	v3Rmdir
	v3Rename
	v3Link
	v3ReadDir
	v3ReadDirPlus
	v3FSStat
	v3FSInfo
	v3PathConf
	v3Commit
)

// stNotSync is NFS3ERR_NOT_SYNC: a SETATTR guard did not match.
const stNotSync status = 10002

// NFSv3 limits.
const (
	// maxFH3 is NFS3_FHSIZE, the longest handle a client may send.
	maxFH3 = 64
	// maxPath3 bounds names and symlink targets as decoded; longer names
	// than maxName decode and fail NAMETOOLONG.
	maxPath3 = 4096
)

// v3State is what the server remembers between NFSv3 calls, a stateless
// protocol: cached writers and permission fallbacks (keptFiles), and the
// verifiers of EXCLUSIVE creates.
type v3State struct {
	kept     keptFiles
	verfs    createVerfs
	maxSizes maxReportedSizes
}

// v3proc is an NFSv3 procedure. run appends the result after the NFS3_OK
// status that nfs3 writes; on another status nfs3 replaces it with the
// failure arm: the status and fail bytes of zeros, the discriminants of the
// absent post_op_attr and wcc_data fields the procedure's resfail carries.
// When the arguments do not decode, run returns without acting and its
// status is ignored.
type v3proc struct {
	run  func(s *Server, d *decoder, res *encoder) status
	fail int
}

// v3procs are the procedures by number; NULL is answered by nfs3 itself.
var v3procs = [...]v3proc{
	v3GetAttr:     {(*Server).getattr3, 0},
	v3SetAttr:     {(*Server).setattr3, 8},
	v3Lookup:      {(*Server).lookup3, 4},
	v3Access:      {(*Server).access3, 4},
	v3Readlink:    {(*Server).readlink3, 4},
	v3Read:        {(*Server).read3, 4},
	v3Write:       {(*Server).write3, 8},
	v3Create:      {(*Server).create3, 8},
	v3Mkdir:       {(*Server).mkdir3, 8},
	v3Symlink:     {(*Server).symlink3, 8},
	v3Mknod:       {(*Server).mknod3, 8},
	v3Remove:      {(*Server).remove3, 8},
	v3Rmdir:       {(*Server).rmdir3, 8},
	v3Rename:      {(*Server).rename3, 16},
	v3Link:        {(*Server).link3, 12},
	v3ReadDir:     {(*Server).readdir3, 4},
	v3ReadDirPlus: {(*Server).readdirplus3, 4},
	v3FSStat:      {(*Server).fsstat3, 4},
	v3FSInfo:      {(*Server).fsinfo3, 4},
	v3PathConf:    {(*Server).pathconf3, 4},
	v3Commit:      {(*Server).commit3, 8},
}

// nfs3 answers NFSv3 calls.
func (s *Server) nfs3(c *call, res *encoder) acceptStat {
	if c.proc == v3Null {
		return rpcSuccess
	}
	if c.proc >= uint32(len(v3procs)) {
		return rpcProcUnavail
	}
	p := v3procs[c.proc]
	at := res.len()
	res.u32(uint32(stOK))
	st := p.run(s, &c.args, res)
	if identityError(s.FS) != nil {
		st = stIO
	}
	if c.args.err != nil {
		return rpcGarbageArgs
	}
	if st != stOK {
		res.sizeAccepted = false
		res.truncate(at)
		res.u32(uint32(st))
		for range p.fail / 4 {
			res.u32(0)
		}
	}
	return rpcSuccess
}

// path3 resolves a decoded file handle: BADHANDLE for one this server cannot
// have issued, STALE for a file id the backend no longer knows.
func (s *Server) path3(id uint64, ok bool) (string, status) {
	if !ok {
		return "", stBadHandle
	}
	p, live := s.FS.Path(id)
	if !live {
		return "", stStale
	}
	return p, stOK
}

// checkName3 vets a file name a client sends. Empty names and names with a
// slash or NUL get ACCES, as Linux's nfsd answers (lookup_one_len refuses
// them with EACCES); "." and ".." are the callers' to treat.
func checkName3(name string) status {
	switch {
	case len(name) > maxName:
		return stNameTooLong
	case name == "", strings.ContainsAny(name, "/\x00"):
		return stAccess
	}
	return stOK
}

// isDot reports the names that refer to a directory itself or its parent,
// which name no entry a procedure could create or remove.
func isDot(name string) bool { return name == "." || name == ".." }

// parentOf is the directory containing p; the root is its own parent.
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
