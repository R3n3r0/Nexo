package cache

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/object"
)

type exchangeKey struct{}

// exchange is the cache side of a request: what Begin decided and what
// the reads recorded.
type exchange struct {
	s     *service
	req   Request
	dist  *distribution.Distribution
	start time.Time
	// keyPrefix is the key prefix of the origin ("prod/" or "").
	keyPrefix string
	// bypass is why the cache does not serve the request ("": it does).
	bypass string
	// credential partitions the entries of a request with credentials
	// that a policy lets use the cache.
	credential string
	// compression is allowed (website mode).
	compression bool
	debug       bool

	mu        sync.Mutex
	result    Result
	age       time.Duration
	fromCache bool
	key       string
	node      string
	vary      bool
	browserCC string
	policy    string
	path      string
}

// exchangeOf returns the exchange of the request of ctx.
func exchangeOf(ctx context.Context) *exchange {
	ex, _ := ctx.Value(exchangeKey{}).(*exchange)
	return ex
}

// Begin implements Runtime.
func (s *service) Begin(ctx context.Context, req Request) (context.Context, Exchange) {
	now := s.deps.Clock.Now()
	ex := &exchange{s: s, req: req, dist: req.Distribution, start: now}
	if req.Distribution == nil {
		ex.bypass = "no distribution"
		return ctx, ex
	}
	if req.Header == nil {
		ex.req.Header = http.Header{}
	}
	d := req.Distribution
	s.observeDistribution(ctx, d)
	if p := strings.Trim(d.Origin.PathPrefix, "/"); p != "" {
		ex.keyPrefix = p + "/"
	}
	ex.compression = d.Mode == distribution.ModeWebsite
	path := req.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	sn := s.snap.Load()
	res := sn.resolveFor(d.ID, req.Host, path)
	ex.bypass = bypassReason(&res.p, &ex.req)
	if ex.bypass == "" && req.Credentials {
		// A policy that does not bypass the credentials caches such
		// requests per credential: never served to another viewer.
		ex.credential = credentialOf(&ex.req)
	}
	ex.debug = s.debugAllowed(ctx, sn, ex.req.Header)
	ex.node = s.deps.NodeID
	ex.policy = res.eff.PolicyName
	ex.path = path
	if ex.bypass != "" {
		ex.result = ResultBypass
	}
	return context.WithValue(ctx, exchangeKey{}, ex), ex
}

// debugAllowed reports a request presenting the debug secret.
func (s *service) debugAllowed(ctx context.Context, sn *snapshot, h http.Header) bool {
	d := sn.global.Debug
	if d == nil || d.Secret == "" {
		return false
	}
	name := d.Header
	if name == "" {
		name = DefaultDebugHeader
	}
	got := h.Get(name)
	if got == "" {
		return false
	}
	want := s.resolveSecret(ctx, d.Secret)
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// usable reports whether a read of the request goes through the cache.
func (ex *exchange) usable(bucket string, opts object.GetObjectOptions) bool {
	if ex == nil || ex.bypass != "" || ex.dist == nil {
		return false
	}
	if bucket != ex.dist.Origin.Bucket || opts.VersionID != "" || opts.PartNumber > 0 || !opts.SSECustomerKey.IsZero() || opts.ChecksumMode {
		ex.record(ResultBypass, nil)
		return false
	}
	return true
}

// record notes the outcome of a read; the last read of a request decides
// its X-Cache (the page served after a 404, for instance).
func (ex *exchange) record(r Result, fn func(ex *exchange)) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	ex.result = r
	ex.fromCache = r == ResultHit || r == ResultStale || r == ResultRevalidated
	ex.age = 0
	if fn != nil {
		fn(ex)
	}
}

// Decorate implements Exchange.
func (ex *exchange) Decorate(status int, h http.Header) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.result == "" {
		return
	}
	h.Set(HeaderXCache, string(ex.result))
	if ex.fromCache {
		h.Set("Age", strconv.FormatInt(int64(ex.age/time.Second), 10))
	}
	if ex.vary && !varyHas(h, "Accept-Encoding") {
		h.Add("Vary", "Accept-Encoding")
	}
	if ex.browserCC != "" && (status == http.StatusOK || status == http.StatusPartialContent || status == http.StatusNotModified) {
		h.Set("Cache-Control", ex.browserCC)
	}
	sn := ex.s.snap.Load()
	if ex.debug || sn.global.NodeHeader {
		h.Set(HeaderXCacheNode, ex.node)
	}
	if ex.debug {
		if ex.key != "" {
			h.Set(HeaderXCacheKey, ex.key)
		}
		h.Set(HeaderXCachePolicy, ex.policy)
	}
}

// varyHas reports whether the Vary header lists name.
func varyHas(h http.Header, name string) bool {
	for _, v := range h.Values("Vary") {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), name) || strings.TrimSpace(p) == "*" {
				return true
			}
		}
	}
	return false
}

// Finish implements Exchange.
func (ex *exchange) Finish(ctx context.Context, status int, bytes int64) {
	ex.mu.Lock()
	result, fromCache, path := ex.result, ex.fromCache, ex.path
	ex.mu.Unlock()
	if result == "" || ex.dist == nil {
		return
	}
	ex.s.recordRequest(ctx, requestRecord{
		dist: ex.dist.ID, host: ex.req.Host, path: path, result: result, fromCache: fromCache, status: status, bytes: bytes,
		elapsed: ex.s.deps.Clock.Now().Sub(ex.start),
	})
}

// observeDistribution drops the entries of a distribution whose settings
// changed since its last request on this node, and refreshes the index of
// the distributions when it does not know this version (a distribution
// created or changed on this node: the notifier only tells the peers).
func (s *service) observeDistribution(ctx context.Context, d *distribution.Distribution) {
	if d.ETag == "" {
		return
	}
	prev, loaded := s.distETags.Swap(d.ID, d.ETag)
	if loaded && prev.(string) != d.ETag {
		n := s.store.removeWhere(func(m *entryMeta) bool { return m.Distribution == d.ID }, false)
		s.deps.Logger.DebugContext(ctx, "cache: distribution changed, entries dropped", "distribution", d.ID, "entries", n)
		s.refreshDistributions(ctx)
		return
	}
	if idx := s.dists.Load(); idx == nil || idx.byID[d.ID].ETag != d.ETag {
		// A version not seen yet refreshes at once; the same one again
		// (a resolver lagging behind the store) at most once per
		// distRefreshMin.
		version := d.ID + "\x00" + d.ETag
		if prev := s.distMissed.Swap(&version); prev == nil || *prev != version ||
			time.Since(time.Unix(0, s.distRefreshed.Load())) >= distRefreshMin {
			s.refreshDistributions(ctx)
		}
	}
}
