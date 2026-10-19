package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// errObjectChanged reports an object that changed while its blocks were
// read: the read starts again from its description.
var errObjectChanged = errors.New("cache: the object changed while it was read")

// piece is an entry served by a lookup or a fill.
type piece struct {
	meta   *entryMeta
	body   []byte
	result Result
	// node is the node that served it (the owner in the shared mode).
	node string
}

// pieceSpec describes a piece to look up or fill: what the owner of a
// key needs in the shared mode (encoded in PieceRequest.Spec).
type pieceSpec struct {
	Kind         entryKind `json:"kind"`
	Key          string    `json:"key"`
	Bucket       string    `json:"bucket"`
	Object       string    `json:"object"`
	Distribution string    `json:"distribution"`
	Host         string    `json:"host,omitempty"`
	Path         string    `json:"path"`
	QuotaIDs     []string  `json:"quotaIds,omitempty"`
	QuotaLimits  []int64   `json:"quotaLimits,omitempty"`
	Params       params    `json:"params"`
	// WantBody asks for the content of an object (GET), not only its
	// description (HEAD).
	WantBody bool `json:"wantBody,omitempty"`
	// Block, Size and ETag select a block of a version; ETag and Encoding
	// a variant.
	Block    int64  `json:"block,omitempty"`
	Size     int64  `json:"size,omitempty"`
	ETag     string `json:"etag,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	// NoStore reads the origin without caching (the owner of the key
	// could not be reached).
	NoStore bool `json:"noStore,omitempty"`
	// identity and identityInfo are the content a variant is compressed
	// from and its description, when the caller has them.
	identity     []byte
	identityInfo *domain.ObjectInfo
}

// owns reports an entry of the object, distribution and host of the
// piece: a lookup never serves the entry of another object, whatever its
// key (the key is not trusted as the identity of the content).
func (ps *pieceSpec) owns(m *entryMeta) bool {
	return m != nil && m.Bucket == ps.Bucket && m.Object == ps.Object && m.Distribution == ps.Distribution && m.Host == ps.Host
}

// quotas are the quota scopes of the piece.
func (ps *pieceSpec) quotas() []quota {
	out := make([]quota, 0, len(ps.QuotaIDs))
	for i, id := range ps.QuotaIDs {
		var limit int64
		if i < len(ps.QuotaLimits) {
			limit = ps.QuotaLimits[i]
		}
		out = append(out, quota{id: id, limit: limit})
	}
	return out
}

// readSpec is what the reads of an object for a request share.
type readSpec struct {
	p      params
	dist   string
	host   string
	bucket string
	object string
	path   string
	// base is the cache key of the object (without variant or block).
	base   string
	policy string
	piece  pieceSpec
}

// specFor resolves the policy and the key of an object of the request of
// ex.
func (s *service) specFor(ctx context.Context, ex *exchange, bucket, objectKey string) *readSpec {
	rel := strings.TrimPrefix(objectKey, ex.keyPrefix)
	if source, _, ok := domain.MediaSidecarSource(objectKey); ok && strings.HasPrefix(source, ex.keyPrefix) {
		// A file the media worker derived from an object (the variant of
		// an image served at the URL of the image): the policy and the
		// path of its entries are those of the object, so that the
		// settings of its behavior apply and an invalidation of its path
		// drops them too. The key stays the one of the file: the format
		// and the width of a variant key its entries.
		rel = strings.TrimPrefix(source, ex.keyPrefix)
	}
	path := "/" + rel
	res := s.snap.Load().resolveFor(ctx, ex.dist.ID, ex.req.Host, path)
	sp := &readSpec{p: res.p, dist: ex.dist.ID, host: ex.req.Host, bucket: bucket, object: objectKey, path: path, policy: res.eff.PolicyName}
	sp.base = keyOf(&sp.p, scopeOf(&sp.p, sp.dist, sp.host), bucket, objectKey, &ex.req, ex.credential)
	domainScope := res.domainKey
	if domainScope == "" {
		domainScope = ex.req.Host
	}
	entryHost := ""
	if sp.p.IncludeHost {
		entryHost = sp.host
	}
	sp.piece = pieceSpec{
		Key: sp.base, Bucket: bucket, Object: objectKey, Distribution: sp.dist, Host: entryHost, Path: path,
		QuotaIDs: []string{"d:" + sp.dist, "h:" + sp.dist + "|" + domainScope}, QuotaLimits: []int64{res.distQuota, res.domainQuota},
		Params: sp.p,
	}
	return sp
}

// objectSpec is the spec of the description (and content) of the object.
func (sp *readSpec) objectSpec(wantBody bool) *pieceSpec {
	ps := sp.piece
	ps.Kind, ps.WantBody = kindObject, wantBody
	return &ps
}

// blockSpec is the spec of block n of the version etag.
func (sp *readSpec) blockSpec(n, size int64, etag string) *pieceSpec {
	ps := sp.piece
	ps.Kind, ps.Key, ps.Block, ps.Size, ps.ETag = kindBlock, sp.base+"#b"+strconv.FormatInt(n, 10), n, size, etag
	return &ps
}

// variantSpec is the spec of the variant enc of a version, compressed
// from identity (described by info) when the caller has it.
func (sp *readSpec) variantSpec(enc string, info *domain.ObjectInfo, identity []byte) *pieceSpec {
	ps := sp.piece
	ps.Kind, ps.Key, ps.Encoding, ps.ETag = kindVariant, sp.base+"#e:"+enc, enc, info.ETag
	if identity != nil {
		ps.identity, ps.identityInfo = identity, info
	}
	return &ps
}

// --- Reader -----------------------------------------------------------------------

// GetObject implements Reader.
func (s *service) GetObject(ctx context.Context, bucket, key string, opts object.GetObjectOptions) (*object.Reader, bool, error) {
	ex := exchangeOf(ctx)
	if !ex.usable(bucket, opts) {
		return nil, false, nil
	}
	sp := s.specFor(ctx, ex, bucket, key)
	if !sp.p.Enabled {
		ex.record(ResultBypass, nil)
		return nil, false, nil
	}
	rd, handled, err := s.getObject(ctx, ex, sp, opts)
	if errors.Is(err, errObjectChanged) {
		// Written while its blocks were read: once more from its new
		// description, then from the origin.
		if rd, handled, err = s.getObject(ctx, ex, sp, opts); errors.Is(err, errObjectChanged) {
			ex.record(ResultBypass, nil)
			return nil, false, nil
		}
	}
	return rd, handled, err
}

func (s *service) getObject(ctx context.Context, ex *exchange, sp *readSpec, opts object.GetObjectOptions) (*object.Reader, bool, error) {
	pc, err := s.getPiece(ctx, sp.objectSpec(true))
	if err != nil {
		ex.record(ResultMiss, func(ex *exchange) { ex.key = sp.base })
		return nil, true, err
	}
	m := pc.meta
	switch m.Kind {
	case kindPass:
		ex.record(ResultBypass, func(ex *exchange) { ex.key = sp.base })
		return nil, false, nil
	case kindNegative:
		s.note(ex, sp, pc, false)
		return nil, true, m.Neg.err(m.Bucket, m.Object)
	}
	info, body := *m.Info, pc.body
	vary := false
	if m.Kind == kindObject && opts.Range == nil && s.compressible(ex, sp, info) {
		vary = true
		if enc := negotiate(ex.req.Header.Get("Accept-Encoding"), sp.p.Algorithms); enc != "" {
			base := info
			if vp, err := s.getPiece(ctx, sp.variantSpec(enc, &base, body)); err == nil && vp.meta.Encoding == enc && vp.meta.Info != nil {
				info, body = *vp.meta.Info, vp.body
			}
		}
	}
	s.note(ex, sp, pc, vary)
	if err := checkConditions(info, opts.Conditions); err != nil {
		return nil, true, err
	}
	off, length := int64(0), info.Size
	if opts.Range != nil {
		o, n, err := opts.Range.Resolve(info.Size)
		if err != nil {
			return nil, true, err
		}
		off, length = o, n
		if m.Kind == kindHead && !sp.p.RangeCaching {
			ex.record(ResultBypass, func(ex *exchange) { ex.key = sp.base })
			return nil, false, nil
		}
	}
	if m.Kind != kindHead {
		if int64(len(body)) < off+length {
			return nil, true, fmt.Errorf("cache: entry %s shorter than its object: %w", sp.base, domain.ErrReadQuorum)
		}
		return &object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(body[off : off+length])), Info: info, Offset: off, Length: length}, true, nil
	}
	br := &blockReader{ctx: ctx, s: s, sp: sp, info: info, off: off, end: off + length, bs: sp.p.BlockSize}
	if length > 0 {
		first, err := br.load()
		if err != nil {
			return nil, true, err
		}
		if first != ResultHit && pc.result == ResultHit {
			ex.record(first, func(ex *exchange) { ex.key = sp.base; ex.vary = vary; ex.browserCC = sp.p.BrowserCC })
		}
	}
	return &object.Reader{ReadCloser: br, Info: info, Offset: off, Length: length}, true, nil
}

// GetObjectInfo implements Reader.
func (s *service) GetObjectInfo(ctx context.Context, bucket, key string, opts object.GetObjectOptions) (domain.ObjectInfo, bool, error) {
	ex := exchangeOf(ctx)
	if !ex.usable(bucket, opts) {
		return domain.ObjectInfo{}, false, nil
	}
	sp := s.specFor(ctx, ex, bucket, key)
	if !sp.p.Enabled {
		ex.record(ResultBypass, nil)
		return domain.ObjectInfo{}, false, nil
	}
	pc, err := s.getPiece(ctx, sp.objectSpec(false))
	if err != nil {
		ex.record(ResultMiss, func(ex *exchange) { ex.key = sp.base })
		return domain.ObjectInfo{}, true, err
	}
	m := pc.meta
	switch m.Kind {
	case kindPass:
		ex.record(ResultBypass, func(ex *exchange) { ex.key = sp.base })
		return domain.ObjectInfo{}, false, nil
	case kindNegative:
		s.note(ex, sp, pc, false)
		return domain.ObjectInfo{}, true, m.Neg.err(m.Bucket, m.Object)
	}
	info := *m.Info
	vary := false
	if m.Kind != kindHead && opts.Range == nil && s.compressible(ex, sp, info) {
		vary = true
		// The variant a GET would serve, also when only the description
		// is cached (it is then compressed from the origin): HEAD and GET
		// answer the same validators (RFC 9110 §9.3.2).
		if enc := negotiate(ex.req.Header.Get("Accept-Encoding"), sp.p.Algorithms); enc != "" {
			base := info
			var identity []byte
			if m.Kind == kindObject {
				identity = pc.body
			}
			if vp, err := s.getPiece(ctx, sp.variantSpec(enc, &base, identity)); err == nil && vp.meta.Encoding == enc && vp.meta.Info != nil {
				info = *vp.meta.Info
			}
		}
	}
	s.note(ex, sp, pc, vary)
	return info, true, checkConditions(info, opts.Conditions)
}

// compressible reports whether the object has compressed variants.
func (s *service) compressible(ex *exchange, sp *readSpec, info domain.ObjectInfo) bool {
	return ex.compression && info.ContentEncoding == "" && info.Size >= sp.p.CompressMin && info.Size <= sp.p.BlockSize &&
		sp.p.compressible(info.ContentType)
}

// note records how a read was served.
func (s *service) note(ex *exchange, sp *readSpec, pc piece, vary bool) {
	now := s.now()
	ex.record(pc.result, func(ex *exchange) {
		ex.key = sp.base
		ex.vary = vary
		ex.varyOn = sp.p.varyHeaders()
		ex.browserCC = sp.p.BrowserCC
		ex.policy = sp.policy
		ex.path = sp.path
		if pc.node != "" {
			ex.node = pc.node
		}
		if ex.fromCache {
			ex.age = max(now.Sub(pc.meta.StoredAt), 0)
		}
	})
}

// --- pieces ----------------------------------------------------------------------------

// localPiece serves a piece from the cache of this node, filling it from
// the origin on a miss.
func (s *service) localPiece(ctx context.Context, ps *pieceSpec) (piece, error) {
	var (
		p   piece
		err error
	)
	switch ps.Kind {
	case kindBlock:
		p, err = s.localBlock(ctx, ps)
	case kindVariant:
		p, err = s.localVariant(ctx, ps)
	default:
		p, err = s.localObject(ctx, ps)
	}
	if p.node == "" {
		p.node = s.deps.NodeID
	}
	return p, err
}

// localObject serves the description (and content) of an object.
func (s *service) localObject(ctx context.Context, ps *pieceSpec) (piece, error) {
	now := s.now()
	if !ps.NoStore {
		if meta, body, _, ok := s.store.get(ctx, ps.Key); ok && ps.owns(meta) {
			switch meta.Kind {
			case kindPass:
				if now.Before(meta.Expires) {
					return piece{meta: meta, result: ResultBypass}, nil
				}
			case kindNegative:
				// A refusal is content too: served only while the node
				// receives every invalidation (an object created on a
				// peer is visible at once, TCC-12).
				if now.Before(meta.Expires) && !meta.Unverified && s.trusted() {
					return piece{meta: meta, result: ResultHit}, nil
				}
			case kindInfo:
				if !ps.WantBody {
					return s.serveEntry(ctx, ps, meta, nil, now)
				}
			case kindObject, kindHead:
				return s.serveEntry(ctx, ps, meta, body, now)
			}
		}
	}
	if ps.WantBody {
		return s.fill(ctx, "o|", ps, s.fetchObject)
	}
	return s.fill(ctx, "i|", ps, s.fetchInfo)
}

// serveEntry serves a cached description: fresh, stale while revalidated,
// or revalidated with the origin.
func (s *service) serveEntry(ctx context.Context, ps *pieceSpec, meta *entryMeta, body []byte, now time.Time) (piece, error) {
	trusted := !meta.Unverified && s.trusted()
	if trusted && now.Before(meta.Expires) {
		return piece{meta: meta, body: body, result: ResultHit}, nil
	}
	if trusted && now.Before(meta.Expires.Add(meta.SWR)) {
		s.refreshAsync(ctx, ps, meta, body)
		return piece{meta: meta, body: body, result: ResultStale}, nil
	}
	return s.revalidate(ctx, ps, meta, body)
}

// fill runs a fill of the piece once for the concurrent callers.
func (s *service) fill(ctx context.Context, prefix string, ps *pieceSpec, fetch func(context.Context, *pieceSpec) (piece, error)) (piece, error) {
	// The blocks and variants of two versions never share a call.
	p, err, shared := s.flights.do(prefix+ps.Key+"\x00"+ps.ETag, objRef{ps.Bucket, ps.Object}, func() (piece, error) {
		// The fill outlives a caller that goes away: the others wait for
		// it.
		return fetch(context.WithoutCancel(ctx), ps)
	})
	if shared && err == nil && !ps.owns(p.meta) {
		// Never the content of another object.
		return fetch(context.WithoutCancel(ctx), ps)
	}
	return p, err
}

// refreshAsync revalidates an entry in the background (stale while
// revalidate).
func (s *service) refreshAsync(ctx context.Context, ps *pieceSpec, meta *entryMeta, body []byte) {
	if s.flights.inFlight("r|" + ps.Key) {
		return
	}
	cp := *ps
	cctx := context.WithoutCancel(ctx)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		if _, err := s.revalidate(cctx, &cp, meta, body); err != nil {
			s.deps.Logger.DebugContext(cctx, "cache: background revalidation failed", "key", cp.Key, "err", err)
		}
	}()
}

// revalidate asks the origin whether an entry is still current: unchanged
// it is fresh again (REVALIDATED), changed it is read again (EXPIRED); an
// origin that fails serves it stale within stale-if-error (STALE).
func (s *service) revalidate(ctx context.Context, ps *pieceSpec, meta *entryMeta, body []byte) (piece, error) {
	p, err, _ := s.flights.do("r|"+ps.Key, objRef{ps.Bucket, ps.Object}, func() (piece, error) {
		cctx := context.WithoutCancel(ctx)
		// The barrier is taken before the origin is read: a write
		// acknowledged meanwhile refuses what this revalidation stores.
		since := s.store.begin()
		defer s.store.end(since)
		info, err := s.deps.Origin.GetObjectInfo(cctx, ps.Bucket, ps.Object, object.GetObjectOptions{})
		s.countOrigin()
		now := s.now()
		if err != nil {
			if neg := negativeOf(err); neg != nil {
				s.store.dropObject(ps.Bucket, ps.Object)
				p, err := s.storeNegative(ps, neg, since, now)
				p.result = ResultExpired
				return p, err
			}
			if !meta.Unverified && now.Before(meta.Expires.Add(meta.SIE)) {
				s.deps.Logger.WarnContext(cctx, "cache: origin unavailable, serving a stale entry", "key", ps.Key, "err", err)
				return piece{meta: meta, body: body, result: ResultStale}, nil
			}
			return piece{}, err
		}
		if meta.Info != nil && sameVersion(info, *meta.Info) {
			f := freshnessOf(&ps.Params, info, now)
			nm := *meta
			nm.Info, nm.StoredAt, nm.Expires, nm.SWR, nm.SIE, nm.Unverified = &info, now, now.Add(f.ttl), f.swr, f.sie, false
			if !f.cacheable {
				nm.Expires = now
			}
			// Only the entry revalidated, and only if no write came
			// meanwhile; this request is served either way (the origin
			// confirmed the version after it began).
			if s.store.update(meta, &nm, since) {
				s.markVerified(ps, since)
			}
			return piece{meta: &nm, body: body, result: ResultRevalidated}, nil
		}
		// Changed: the entries of the object on this node are outdated.
		// A drop, not an invalidation: the write already set its barrier
		// (and the callers of this revalidation keep its result).
		s.store.dropObject(ps.Bucket, ps.Object)
		var p piece
		if ps.WantBody || meta.Kind != kindInfo {
			p, err = s.fetchObject(cctx, ps)
		} else {
			p, err = s.fetchInfo(cctx, ps)
		}
		if err == nil && p.result == ResultMiss {
			p.result = ResultExpired
		}
		return p, err
	})
	return p, err
}

// markVerified clears the unverified flag of the blocks and variants of a
// version confirmed by the origin.
func (s *service) markVerified(ps *pieceSpec, since uint64) {
	for _, k := range s.store.keysOf(ps.Bucket, ps.Object) {
		if m, _, ok := s.store.peek(k); ok && m.Unverified {
			nm := *m
			nm.Unverified = false
			s.store.update(m, &nm, since)
		}
	}
}

// newMeta starts the description of an entry of a piece.
func newMeta(ps *pieceSpec, kind entryKind, key string, now time.Time) *entryMeta {
	return &entryMeta{
		Key: key, Kind: kind, Bucket: ps.Bucket, Object: ps.Object, Distribution: ps.Distribution, Host: ps.Host, Path: ps.Path,
		Scopes: ps.QuotaIDs, StoredAt: now, Expires: now,
	}
}

// put stores an entry unless the piece is read without caching.
func (s *service) put(ps *pieceSpec, meta *entryMeta, body []byte, since uint64) {
	if ps.NoStore {
		return
	}
	s.store.put(meta, body, since, ps.quotas())
}

// fetchObject reads an object from the origin and caches it: whole up to
// a block (its content), by blocks beyond (its description and its first
// block), not at all outside the limits of the policy (hit-for-pass).
func (s *service) fetchObject(ctx context.Context, ps *pieceSpec) (piece, error) {
	since := s.store.begin()
	defer s.store.end(since)
	rd, err := s.deps.Origin.GetObject(ctx, ps.Bucket, ps.Object, object.GetObjectOptions{})
	s.countOrigin()
	now := s.now()
	if err != nil {
		if neg := negativeOf(err); neg != nil {
			return s.storeNegative(ps, neg, since, now)
		}
		return piece{}, err
	}
	defer func() { _ = rd.Close() }()
	info := rd.Info
	p := &ps.Params
	f := freshnessOf(p, info, now)
	if !f.cacheable || info.Size > p.MaxSize || info.Size < p.MinSize || !p.typeAllowed(info.ContentType) {
		meta := newMeta(ps, kindPass, ps.Key, now)
		meta.Expires = now.Add(passTTL)
		s.put(ps, meta, nil, since)
		return piece{meta: meta, result: ResultBypass}, nil
	}
	memoryOnly := !info.Encryption.IsZero()
	if info.Size <= p.BlockSize {
		body := make([]byte, info.Size)
		if _, err := io.ReadFull(rd, body); err != nil {
			return piece{}, fmt.Errorf("cache: read %s/%s: %w", ps.Bucket, ps.Object, err)
		}
		meta := newMeta(ps, kindObject, ps.Key, now)
		meta.Info, meta.Expires, meta.SWR, meta.SIE = &info, now.Add(f.ttl), f.swr, f.sie
		meta.Size, meta.CRC, meta.MemoryOnly, meta.Tags = int64(len(body)), crc32.Checksum(body, s.crc), memoryOnly, tagsOf(info)
		s.put(ps, meta, body, since)
		return piece{meta: meta, body: body, result: ResultMiss}, nil
	}
	// By blocks: the first one comes with the open stream.
	first := make([]byte, p.BlockSize)
	if _, err := io.ReadFull(rd, first); err != nil {
		return piece{}, fmt.Errorf("cache: read %s/%s: %w", ps.Bucket, ps.Object, err)
	}
	bm := newMeta(ps, kindBlock, ps.Key+"#b0", now)
	bm.ETag, bm.Block, bm.BlockSize, bm.Size, bm.CRC, bm.MemoryOnly = info.ETag, 0, p.BlockSize, int64(len(first)), crc32.Checksum(first, s.crc), memoryOnly
	bm.Expires = now.Add(f.ttl)
	s.put(ps, bm, first, since)
	head := newMeta(ps, kindHead, ps.Key, now)
	head.Info, head.Expires, head.SWR, head.SIE, head.BlockSize, head.MemoryOnly, head.Tags = &info, now.Add(f.ttl), f.swr, f.sie, p.BlockSize, memoryOnly, tagsOf(info)
	s.put(ps, head, nil, since)
	return piece{meta: head, result: ResultMiss}, nil
}

// fetchInfo reads the description of an object from the origin (HEAD).
func (s *service) fetchInfo(ctx context.Context, ps *pieceSpec) (piece, error) {
	since := s.store.begin()
	defer s.store.end(since)
	info, err := s.deps.Origin.GetObjectInfo(ctx, ps.Bucket, ps.Object, object.GetObjectOptions{})
	s.countOrigin()
	now := s.now()
	if err != nil {
		if neg := negativeOf(err); neg != nil {
			return s.storeNegative(ps, neg, since, now)
		}
		return piece{}, err
	}
	p := &ps.Params
	f := freshnessOf(p, info, now)
	if !f.cacheable || info.Size > p.MaxSize || info.Size < p.MinSize || !p.typeAllowed(info.ContentType) {
		meta := newMeta(ps, kindPass, ps.Key, now)
		meta.Expires = now.Add(passTTL)
		s.put(ps, meta, nil, since)
		return piece{meta: meta, result: ResultBypass}, nil
	}
	meta := newMeta(ps, kindInfo, ps.Key, now)
	meta.Info, meta.Expires, meta.SWR, meta.SIE, meta.Tags, meta.MemoryOnly = &info, now.Add(f.ttl), f.swr, f.sie, tagsOf(info), true
	s.put(ps, meta, nil, since)
	return piece{meta: meta, result: ResultMiss}, nil
}

// storeNegative caches a refusal of the origin (negative caching).
func (s *service) storeNegative(ps *pieceSpec, neg *negative, since uint64, now time.Time) (piece, error) {
	meta := newMeta(ps, kindNegative, ps.Key, now)
	meta.Neg = neg
	meta.Expires = now.Add(ps.Params.Negative)
	if ps.Params.Negative > 0 {
		s.put(ps, meta, nil, since)
	}
	return piece{meta: meta, result: ResultMiss}, nil
}

// localBlock serves a block of a version.
func (s *service) localBlock(ctx context.Context, ps *pieceSpec) (piece, error) {
	if !ps.NoStore {
		if meta, body, _, ok := s.store.get(ctx, ps.Key); ok && ps.owns(meta) && meta.Kind == kindBlock && meta.ETag == ps.ETag {
			return piece{meta: meta, body: body, result: ResultHit}, nil
		}
	}
	return s.fill(ctx, "b|", ps, s.fetchBlock)
}

// fetchBlock reads a block of a version from the origin.
func (s *service) fetchBlock(ctx context.Context, ps *pieceSpec) (piece, error) {
	since := s.store.begin()
	defer s.store.end(since)
	bs := ps.Params.BlockSize
	start := ps.Block * bs
	end := min(start+bs, ps.Size)
	if start >= end {
		return piece{}, fmt.Errorf("%w: block %d of a %d-byte object", domain.ErrInvalidRange, ps.Block, ps.Size)
	}
	rd, err := s.deps.Origin.GetObject(ctx, ps.Bucket, ps.Object, object.GetObjectOptions{
		Range: &domain.Range{Start: start, End: end - 1}, Conditions: domain.Conditions{IfMatch: ps.ETag},
	})
	s.countOrigin()
	if err != nil {
		if errors.Is(err, domain.ErrPreconditionFailed) || negativeOf(err) != nil {
			s.store.dropObject(ps.Bucket, ps.Object)
			return piece{}, errObjectChanged
		}
		return piece{}, err
	}
	defer func() { _ = rd.Close() }()
	body := make([]byte, end-start)
	if _, err := io.ReadFull(rd, body); err != nil {
		return piece{}, fmt.Errorf("cache: read block %d of %s/%s: %w", ps.Block, ps.Bucket, ps.Object, err)
	}
	now := s.now()
	meta := newMeta(ps, kindBlock, ps.Key, now)
	meta.ETag, meta.Block, meta.BlockSize, meta.Size, meta.CRC = ps.ETag, ps.Block, bs, int64(len(body)), crc32.Checksum(body, s.crc)
	meta.MemoryOnly = !rd.Info.Encryption.IsZero()
	s.put(ps, meta, body, since)
	return piece{meta: meta, body: body, result: ResultMiss}, nil
}

// identityEncoding marks a variant not worth storing (the compressed
// content is not smaller).
const identityEncoding = "identity"

// localVariant serves a compressed variant of a version.
func (s *service) localVariant(ctx context.Context, ps *pieceSpec) (piece, error) {
	if !ps.NoStore {
		if meta, body, _, ok := s.store.get(ctx, ps.Key); ok && ps.owns(meta) && meta.Kind == kindVariant && meta.ETag == ps.ETag {
			return piece{meta: meta, body: body, result: ResultHit}, nil
		}
	}
	return s.fill(ctx, "e|", ps, s.fetchVariant)
}

// fetchVariant compresses a version (read from the origin unless the
// caller has it).
func (s *service) fetchVariant(ctx context.Context, ps *pieceSpec) (piece, error) {
	since := s.store.begin()
	defer s.store.end(since)
	identity := ps.identity
	var info domain.ObjectInfo
	if ps.identityInfo != nil {
		info = *ps.identityInfo
	}
	if identity == nil {
		rd, err := s.deps.Origin.GetObject(ctx, ps.Bucket, ps.Object, object.GetObjectOptions{Conditions: domain.Conditions{IfMatch: ps.ETag}})
		s.countOrigin()
		if err != nil {
			return piece{}, err
		}
		info = rd.Info
		identity, err = io.ReadAll(io.LimitReader(rd, ps.Params.BlockSize+1))
		_ = rd.Close()
		if err != nil {
			return piece{}, err
		}
	}
	now := s.now()
	meta := newMeta(ps, kindVariant, ps.Key, now)
	meta.ETag, meta.Expires = ps.ETag, now.Add(ps.Params.TTLMax)
	compressed, err := compress(ps.Encoding, identity)
	if err != nil || len(compressed) >= len(identity) || info.ETag == "" {
		meta.Encoding = identityEncoding
		s.put(ps, meta, nil, since)
		return piece{meta: meta, result: ResultMiss}, nil
	}
	vi := info
	vi.ETag, vi.Size, vi.ContentEncoding = variantETag(ps.ETag, ps.Encoding), int64(len(compressed)), ps.Encoding
	meta.Encoding, meta.Info, meta.Size, meta.CRC = ps.Encoding, &vi, int64(len(compressed)), crc32.Checksum(compressed, s.crc)
	meta.MemoryOnly = !info.Encryption.IsZero()
	s.put(ps, meta, compressed, since)
	return piece{meta: meta, body: compressed, result: ResultMiss}, nil
}

// --- block reader -------------------------------------------------------------------

// blockReader streams a range of an object cached by blocks, reading the
// next block ahead.
type blockReader struct {
	ctx  context.Context
	s    *service
	sp   *readSpec
	info domain.ObjectInfo
	off  int64
	end  int64
	bs   int64
	cur  []byte
}

// load fetches the block of the current offset; it returns how it was
// served.
func (r *blockReader) load() (Result, error) {
	n := r.off / r.bs
	pc, err := r.s.getPiece(r.ctx, r.sp.blockSpec(n, r.info.Size, r.info.ETag))
	if errors.Is(err, domain.ErrPreconditionFailed) {
		// The owner of the block (shared mode) found another version.
		err = errObjectChanged
	}
	if err != nil {
		return "", err
	}
	start := r.off - n*r.bs
	stop := min(int64(len(pc.body)), r.end-n*r.bs)
	if start > stop || int64(len(pc.body)) < stop {
		return "", fmt.Errorf("cache: block %d of %s is short: %w", n, r.sp.base, errObjectChanged)
	}
	r.cur = pc.body[start:stop]
	if next := (n + 1) * r.bs; next < r.end {
		// Read ahead: the next block is filled while this one is sent.
		spec := r.sp.blockSpec(n+1, r.info.Size, r.info.ETag)
		ctx := r.ctx
		r.s.bg.Add(1)
		go func() {
			defer r.s.bg.Done()
			_, _ = r.s.getPiece(ctx, spec)
		}()
	}
	return pc.result, nil
}

// Read implements io.Reader.
func (r *blockReader) Read(p []byte) (int, error) {
	if len(r.cur) == 0 {
		if r.off >= r.end {
			return 0, io.EOF
		}
		if _, err := r.load(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	r.off += int64(n)
	return n, nil
}

// Close implements io.Closer.
func (r *blockReader) Close() error { return nil }

// --- helpers ----------------------------------------------------------------------------

// sameVersion reports two descriptions of the same version.
func sameVersion(a, b domain.ObjectInfo) bool {
	return a.ETag == b.ETag && a.Size == b.Size && a.ModTime.Equal(b.ModTime) && a.VersionID == b.VersionID
}

// negativeOf classifies the refusals of the origin that are cached (nil
// for the other errors).
func negativeOf(err error) *negative {
	var marker *domain.DeleteMarkerError
	switch {
	case errors.As(err, &marker):
		return &negative{Code: "delete-marker", VersionID: marker.VersionID, ModTime: marker.ModTime}
	case errors.Is(err, domain.ErrBucketNotFound):
		return &negative{Code: "bucket-not-found"}
	case errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrNoSuchVersion):
		return &negative{Code: "not-found"}
	case errors.Is(err, domain.ErrSSECustomerKeyRequired):
		return nil
	case errors.Is(err, domain.ErrAccessDenied):
		return &negative{Code: "access-denied"}
	}
	return nil
}

// err rebuilds the error of a cached refusal.
func (n *negative) err(bucket, key string) error {
	switch n.Code {
	case "bucket-not-found":
		return domain.NewS3Error(domain.ErrBucketNotFound, "NoSuchBucket", "The specified bucket does not exist").WithResource(bucket)
	case "delete-marker":
		return &domain.DeleteMarkerError{VersionID: n.VersionID, ModTime: n.ModTime,
			Err: domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")}
	case "access-denied":
		return fmt.Errorf("cache: %s/%s: %w", bucket, key, domain.ErrAccessDenied)
	}
	return domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")
}

// checkConditions evaluates the conditional headers of a read (RFC 9110
// §13.2.2 precedence), as the object layer does.
func checkConditions(info domain.ObjectInfo, c domain.Conditions) error {
	if c.IsZero() {
		return nil
	}
	modTime := info.ModTime.Truncate(time.Second)
	if c.IfMatch != "" {
		if !etagMatches(c.IfMatch, info.ETag) {
			return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
		}
	} else if !c.IfUnmodifiedSince.IsZero() && modTime.After(c.IfUnmodifiedSince) {
		return fmt.Errorf("%w: If-Unmodified-Since", domain.ErrPreconditionFailed)
	}
	if c.IfNoneMatch != "" {
		if etagMatches(c.IfNoneMatch, info.ETag) {
			return fmt.Errorf("%w: If-None-Match", domain.ErrNotModified)
		}
	} else if !c.IfModifiedSince.IsZero() && !modTime.After(c.IfModifiedSince) {
		return fmt.Errorf("%w: If-Modified-Since", domain.ErrNotModified)
	}
	return nil
}

// etagMatches reports whether an If-Match/If-None-Match value lists etag.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	want := strings.Trim(etag, `"`)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if strings.EqualFold(strings.Trim(candidate, `"`), want) {
			return true
		}
	}
	return false
}

// Limits of the cache tags of an object.
const (
	maxTags      = 32
	maxTagLength = 128
	// cacheTagKey names the metadata and the S3 tag holding the cache
	// tags of an object.
	cacheTagKey = "cache-tag"
)

// tagsOf returns the cache tags of an object: its x-amz-meta-cache-tag
// metadata and its S3 tag "cache-tag", split on commas and spaces.
func tagsOf(info domain.ObjectInfo) []string {
	var raw []string
	for k, v := range info.UserMetadata {
		if strings.EqualFold(k, cacheTagKey) {
			raw = append(raw, v)
		}
	}
	for k, v := range info.Tags {
		if strings.EqualFold(k, cacheTagKey) {
			raw = append(raw, v)
		}
	}
	return splitTags(raw...)
}

// splitTags splits and deduplicates cache tags.
func splitTags(values ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range values {
		for _, t := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if len(t) > maxTagLength || seen[t] || len(out) >= maxTags {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// encodeSpec and decodeSpec carry a piece spec over the internode RPC.
func encodeSpec(ps *pieceSpec) ([]byte, error) { return json.Marshal(ps) }

func decodeSpec(data []byte) (*pieceSpec, error) {
	var ps pieceSpec
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, fmt.Errorf("%w: piece spec: %v", ErrInvalid, err)
	}
	if ps.Key == "" || ps.Bucket == "" || ps.Object == "" || ps.Params.BlockSize < minBlockSize {
		return nil, fmt.Errorf("%w: piece spec incomplete", ErrInvalid)
	}
	return &ps, nil
}
