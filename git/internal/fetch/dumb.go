package fetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/internal/mapped"
)

// dumb fetches from a dumb HTTP server (gitprotocol-http), a plain file
// server whose info/refs response is refs: the client walks the history
// itself, getting each object it lacks loose, or else the pack holding it.
func (f *fetcher) dumb(ctx context.Context, s *httpSession, refs io.ReadCloser) (Result, error) {
	err := checkInfoRefs(refs, f.req.format, s.redactedBase())
	refs.Close()
	if err != nil {
		return Result{}, err
	}
	// The target's ref tips are complete, and so is its history down to its
	// shallow boundary; anything else it holds may be left over from an
	// interrupted fetch, so the walk descends into it.
	complete := idSet(f.opts.Haves)
	for _, id := range f.opts.Shallow {
		complete[id] = struct{}{}
	}
	src := &dumbSource{
		ctx:     ctx,
		s:       s,
		dst:     f.opts.Target,
		packDir: f.packDir(),
		maxSize: f.opts.MaxObjectSize,
		allowed: f.opts.Allowed,
		have:    complete,
		stores:  []*dumbStore{{objects: s.base.JoinPath("objects")}},
	}
	defer src.close()
	return copyObjects(ctx, src, f.opts.Target, f.opts.Wants, LocalOptions{
		Depth:         f.opts.Depth,
		TargetShallow: f.opts.Shallow,
		PackDir:       f.packDir(),
	})
}

// checkInfoRefs checks that a dumb server's info/refs starts as a ref list
// ("<id>\t<ref>") of format f; an empty one is an empty repository.
func checkInfoRefs(r io.Reader, f object.Format, base string) error {
	line, err := bufio.NewReaderSize(r, 4096).ReadSlice('\n')
	if err == io.EOF && len(line) == 0 {
		return nil
	}
	if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("fetch: reading %sinfo/refs: %w", base, err)
	}
	hex, _, ok := bytes.Cut(line, []byte{'\t'})
	id, perr := object.ParseID(string(hex))
	if !ok || perr != nil {
		return fmt.Errorf("fetch: %sinfo/refs not valid: is this a git repository?", base)
	}
	if id.Format() != f {
		return fmt.Errorf("fetch: the remote repository uses %s object names, the target uses %s", id.Format(), f)
	}
	return nil
}

// dumbSource is a walk's source on a dumb HTTP server. An object the target
// lacks is requested loose from the repository and its alternates, else the
// pack whose index names it is downloaded whole into the target. Loose
// objects reach the target through the walk; packs are stored directly.
type dumbSource struct {
	ctx     context.Context
	s       *httpSession
	dst     *object.DB
	packDir string
	maxSize int64
	allowed []string
	// have holds the commits whose history the target has.
	have map[object.ID]struct{}
	// stores are the repository's object directory, then its alternates
	// once read (loadAlternates).
	stores     []*dumbStore
	alternates bool
	// ignored lists alternates not followed, for the error when an object
	// is nowhere else.
	ignored []string
	// fetched is the loose object kind fetched, kept for the read that
	// follows it in the walk.
	fetched *looseObject
}

type looseObject struct {
	id   object.ID
	kind object.Kind
	data []byte
}

// dumbStore is an object directory on the server.
type dumbStore struct {
	objects *url.URL
	listed  bool        // objects/info/packs was read
	packs   []*dumbPack // the packs not downloaded yet
}

// dumbPack is a pack the server lists, with its index once downloaded.
type dumbPack struct {
	name  string // "pack-<hex>"
	idx   *object.PackIndex
	file  string // the index, a temporary file in the pack directory
	unmap func() error
}

func (p *dumbPack) close() {
	if p.unmap != nil {
		p.unmap()
		os.Remove(p.file)
		p.idx, p.unmap = nil, nil
	}
}

func (d *dumbSource) close() {
	for _, st := range d.stores {
		for _, p := range st.packs {
			p.close()
		}
	}
}

func (d *dumbSource) complete(id object.ID) bool {
	_, ok := d.have[id]
	return ok
}

func (d *dumbSource) kind(id object.ID) (object.Kind, error) {
	kind, data, held, err := d.read(id)
	if err == nil && !held {
		d.fetched = &looseObject{id, kind, data}
	}
	return kind, err
}

func (d *dumbSource) read(id object.ID) (object.Kind, []byte, bool, error) {
	if o := d.fetched; o != nil && o.id == id {
		d.fetched = nil
		return o.kind, o.data, false, nil
	}
	if d.dst.Contains(id) {
		kind, data, err := d.dst.Read(id)
		return kind, data, true, err
	}
	// Loose from the repository, then from each alternate: stores grows when
	// the repository's miss loads them.
	for i := 0; i < len(d.stores); i++ {
		kind, data, ok, err := d.loose(d.stores[i], id)
		if err != nil || ok {
			return kind, data, false, err
		}
		if i == 0 {
			if err := d.loadAlternates(); err != nil {
				return 0, nil, false, err
			}
		}
	}
	for _, st := range d.stores {
		ok, err := d.fromPack(st, id)
		if err != nil {
			return 0, nil, false, err
		}
		if ok {
			kind, data, err := d.dst.Read(id)
			return kind, data, true, err
		}
	}
	msg := fmt.Sprintf("fetch: unable to find %s under %s", id, d.s.redactedBase())
	if len(d.ignored) > 0 {
		msg += fmt.Sprintf(" (alternate %s not followed: http.followRedirects is not true)", strings.Join(d.ignored, ", "))
	}
	return 0, nil, false, errors.New(msg)
}

// loose requests id from st's loose objects, verifying its name.
func (d *dumbSource) loose(st *dumbStore, id object.ID) (object.Kind, []byte, bool, error) {
	hex := id.String()
	body, found, err := d.s.get(d.ctx, st.objects.JoinPath(hex[:2], hex[2:]))
	if err != nil || !found {
		return 0, nil, false, err
	}
	defer body.Close()
	kind, data, err := object.DecodeLoose(body, d.maxSize)
	if err != nil {
		return 0, nil, false, fmt.Errorf("fetch: %s from %s: %w", id, st.objects.Redacted(), err)
	}
	if got := object.Hash(id.Format(), kind, data); got != id {
		return 0, nil, false, fmt.Errorf("fetch: %s from %s holds object %s", id, st.objects.Redacted(), got)
	}
	return kind, data, true, nil
}

// loadAlternates reads the repository's objects/info/http-alternates, else
// objects/info/alternates. As with git, the alternates are followed only
// when http.followRedirects is true, since they send requests anywhere, and
// their own alternates are not.
func (d *dumbSource) loadAlternates() error {
	if d.alternates {
		return nil
	}
	d.alternates = true
	objects := d.stores[0].objects
	var body io.ReadCloser
	for _, name := range []string{"http-alternates", "alternates"} {
		b, found, err := d.s.get(d.ctx, objects.JoinPath("info", name))
		if err != nil {
			if d.ctx.Err() != nil {
				return d.ctx.Err()
			}
			return nil // optional: a server refusing them has none to offer
		}
		if found {
			body = b
			break
		}
	}
	if body == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(body, 64<<10))
	body.Close()
	if err != nil {
		return fmt.Errorf("fetch: reading alternates of %s: %w", d.s.redactedBase(), err)
	}
	for line := range strings.Lines(string(data)) {
		alt, ok := resolveAlternate(objects, strings.TrimRight(line, "\r\n"))
		if !ok || !slices.Contains(d.allowed, alt.Scheme) {
			continue
		}
		if d.s.redirects != redirectAlways {
			d.ignored = append(d.ignored, alt.Redacted())
			continue
		}
		d.stores = append(d.stores, &dumbStore{objects: alt})
	}
	return nil
}

// resolveAlternate turns an alternates line into the URL of the object
// directory it names: an http or https URL, or a path absolute on the
// server or relative to objects. It must end in "objects", and may not
// downgrade https to http.
func resolveAlternate(objects *url.URL, line string) (*url.URL, bool) {
	if line == "" {
		return nil, false
	}
	ref, err := url.Parse(line)
	if err != nil {
		return nil, false
	}
	dir := *objects
	dir.Path += "/"
	if dir.RawPath != "" {
		dir.RawPath += "/"
	}
	u := dir.ResolveReference(ref)
	if u.Scheme != "http" && u.Scheme != "https" || objects.Scheme == "https" && u.Scheme == "http" {
		return nil, false
	}
	if path.Base(u.Path) != "objects" || strings.HasSuffix(u.Path, "/") {
		return nil, false
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u, true
}

// fromPack downloads into the target the pack of st whose index names id,
// reporting whether there was one.
func (d *dumbSource) fromPack(st *dumbStore, id object.ID) (bool, error) {
	if !st.listed {
		if err := d.listPacks(st); err != nil {
			return false, err
		}
	}
	for i := 0; i < len(st.packs); i++ {
		p := st.packs[i]
		if p.idx == nil {
			found, err := d.loadIndex(st, p)
			if err != nil {
				return false, err
			}
			if !found { // listed but gone: objects/info/packs is stale
				st.packs = slices.Delete(st.packs, i, i+1)
				i--
				continue
			}
		}
		if !p.idx.Has(id) {
			continue
		}
		err := d.downloadPack(st, p)
		p.close()
		st.packs = slices.Delete(st.packs, i, i+1)
		if err != nil {
			return false, err
		}
		if !d.dst.Contains(id) {
			return false, fmt.Errorf("fetch: %s.pack from %s lacks %s, which its index lists", p.name, st.objects.Redacted(), id)
		}
		return true, nil
	}
	return false, nil
}

// listPacks reads st's objects/info/packs: "P pack-<hex>.pack" lines.
func (d *dumbSource) listPacks(st *dumbStore) error {
	st.listed = true
	body, found, err := d.s.get(d.ctx, st.objects.JoinPath("info", "packs"))
	if err != nil || !found {
		return err
	}
	defer body.Close()
	hexSize := d.dst.Format().HexSize()
	sc := bufio.NewScanner(io.LimitReader(body, 16<<20))
	for sc.Scan() {
		kind, file, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		name, ok := strings.CutSuffix(file, ".pack")
		hex, isPack := strings.CutPrefix(name, "pack-")
		if kind != "P" || !ok || !isPack || len(hex) != hexSize || strings.Trim(hex, "0123456789abcdef") != "" {
			continue
		}
		if !slices.ContainsFunc(st.packs, func(p *dumbPack) bool { return p.name == name }) {
			st.packs = append(st.packs, &dumbPack{name: name})
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("fetch: reading %s: %w", st.objects.JoinPath("info", "packs").Redacted(), err)
	}
	return nil
}

// loadIndex downloads p's index to a temporary file and maps it; found is
// false when the server lacks it.
func (d *dumbSource) loadIndex(st *dumbStore, p *dumbPack) (found bool, err error) {
	u := st.objects.JoinPath("pack", p.name+".idx")
	body, found, err := d.s.get(d.ctx, u)
	if err != nil || !found {
		return false, err
	}
	defer body.Close()
	if err := os.MkdirAll(d.packDir, 0o777); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(d.packDir, "tmp_idx_")
	if err != nil {
		return false, err
	}
	_, err = io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	var data []byte
	var unmap func() error
	if err == nil {
		data, unmap, err = mapped.Map(tmp.Name())
	}
	if err == nil {
		if p.idx, err = object.ParseIndex(data, d.dst.Format()); err != nil {
			unmap()
		}
	}
	if err != nil {
		os.Remove(tmp.Name())
		return false, fmt.Errorf("fetch: %s: %w", u.Redacted(), err)
	}
	p.file, p.unmap = tmp.Name(), unmap
	return true, nil
}

// downloadPack stores p into the target, indexing it as it arrives.
func (d *dumbSource) downloadPack(st *dumbStore, p *dumbPack) error {
	u := st.objects.JoinPath("pack", p.name+".pack")
	body, found, err := d.s.get(d.ctx, u)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("fetch: %s is missing, though its index is not", u.Redacted())
	}
	defer body.Close()
	if _, _, err := object.IndexPack(body, d.packDir, d.dst.Format(), object.IndexPackOptions{DB: d.dst, MaxObjectSize: d.maxSize}); err != nil {
		if d.ctx.Err() != nil {
			return d.ctx.Err()
		}
		return fmt.Errorf("fetch: %s: %w", u.Redacted(), err)
	}
	return d.dst.Rescan()
}
