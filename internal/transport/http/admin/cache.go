package admin

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// CacheRoutes exposes the cache of the distributions (docs/11 §11.11):
// the reusable policies (/cache/policies, with the preview of the
// Cache-Control they send), the settings of the cluster (/cache/settings)
// and of a distribution with the overrides of its domains and paths
// (/distributions/{id}/cache, with the effective policy of a URL), the
// invalidations with their state on every node, the warm-ups, the purge
// webhooks, the inspector of a URL and the statistics.
//
// The configuration documents (policies, settings) are those of
// internal/service/cache carried as is, durations and sizes in their
// readable form ("1d", "20GiB"); the reports (statistics, inspector) give
// them as numbers (bytes, seconds) for the charts of the console.
type CacheRoutes interface{ Routes }

// CacheDeps are the dependencies of the cache routes.
type CacheDeps struct {
	// Cache manages the cache (required).
	Cache adminsvc.CacheManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// CacheOption configures the cache routes.
type CacheOption func(*cacheRoutes)

type cacheRoutes struct {
	deps  CacheDeps
	guard guard
}

var _ CacheRoutes = (*cacheRoutes)(nil)

// NewCacheRoutes returns the cache routes.
func NewCacheRoutes(deps CacheDeps, opts ...CacheOption) (CacheRoutes, error) {
	if deps.Cache == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: cache routes require the cache manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &cacheRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *cacheRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *cacheRoutes) routes() []route {
	// The configuration and the diagnostics need admin:ManageCache; the
	// operations on the cached content (invalidations, warm-ups) need
	// admin:InvalidateCache alone, so that a deploy pipeline purges and
	// warms without changing the policies (TD-C-06).
	manage := global(policy.ActionAdminManageCache)
	operate := global(policy.ActionAdminInvalidateCache)
	return []route{
		{http.MethodGet, "/cache/policies", manage, h.listPolicies},
		{http.MethodPost, "/cache/policies", manage, h.createPolicy},
		{http.MethodGet, "/cache/policies/{name}", manage, h.getPolicy},
		{http.MethodPut, "/cache/policies/{name}", manage, h.updatePolicy},
		{http.MethodDelete, "/cache/policies/{name}", manage, h.deletePolicy},
		{http.MethodPost, "/cache/preview", manage, h.previewPolicy},
		{http.MethodGet, "/cache/settings", manage, h.getSettings},
		{http.MethodPut, "/cache/settings", manage, h.updateSettings},
		{http.MethodGet, "/cache/inspect", manage, h.inspect},
		{http.MethodGet, "/cache/stats", manage, h.stats},
		{http.MethodPost, "/cache/invalidations", operate, h.purgeAll},
		{http.MethodGet, "/cache/invalidations", operate, h.listGlobalInvalidations},
		{http.MethodGet, "/cache/invalidations/{inv}", operate, h.findInvalidation},
		{http.MethodGet, "/distributions/{id}/cache", manage, h.getDistribution},
		{http.MethodPut, "/distributions/{id}/cache", manage, h.updateDistribution},
		{http.MethodGet, "/distributions/{id}/cache/effective", manage, h.effective},
		{http.MethodPost, "/distributions/{id}/cache/warm", operate, h.warm},
		{http.MethodGet, "/distributions/{id}/cache/warm/{job}", operate, h.warmJob},
		{http.MethodGet, "/distributions/{id}/cache/webhooks", manage, h.listWebhooks},
		{http.MethodPost, "/distributions/{id}/cache/webhooks", manage, h.addWebhook},
		{http.MethodDelete, "/distributions/{id}/cache/webhooks/{webhook}", manage, h.deleteWebhook},
		{http.MethodPost, "/distributions/{id}/cache/webhooks/{webhook}/test", manage, h.testWebhook},
		{http.MethodPost, "/distributions/{id}/invalidations", operate, h.invalidate},
		{http.MethodGet, "/distributions/{id}/invalidations", operate, h.listInvalidations},
		{http.MethodGet, "/distributions/{id}/invalidations/{inv}", operate, h.getInvalidation},
	}
}

// --- wire types ------------------------------------------------------------------------

// CachePolicyListResponse is the body of GET /cache/policies.
type CachePolicyListResponse struct {
	Items []cache.Policy `json:"items"`
}

// CacheInvalidationRequest is the body of POST
// /distributions/{id}/invalidations and POST /cache/invalidations (the
// global purge: all only).
type CacheInvalidationRequest struct {
	// Domain limits the invalidation to a domain of the distribution.
	Domain string `json:"domain,omitempty"`
	// Paths are exact paths, or absolute URLs on a domain of the
	// distribution (their host sets the domain).
	Paths    []string                   `json:"paths,omitempty"`
	Patterns []distribution.PathPattern `json:"patterns,omitempty"`
	Tags     []string                   `json:"tags,omitempty"`
	All      bool                       `json:"all,omitempty"`
	Soft     bool                       `json:"soft,omitempty"`
}

// CacheInvalidationListResponse is the history of the invalidations.
type CacheInvalidationListResponse struct {
	Items []cache.Invalidation `json:"items"`
}

// CacheWarmRequest is the body of POST /distributions/{id}/cache/warm.
type CacheWarmRequest struct {
	Domain  string                    `json:"domain,omitempty"`
	Paths   []string                  `json:"paths,omitempty"`
	URLs    []string                  `json:"urls,omitempty"`
	Prefix  string                    `json:"prefix,omitempty"`
	Pattern *distribution.PathPattern `json:"pattern,omitempty"`
	// Concurrency bounds the parallel reads (default 4), Bandwidth the
	// bytes read per second ("10MiB" or a number of bytes; default
	// 64 MiB/s).
	Concurrency int        `json:"concurrency,omitempty"`
	Bandwidth   cache.Size `json:"bandwidth,omitempty"`
}

// CacheWebhookListResponse is the body of GET
// /distributions/{id}/cache/webhooks.
type CacheWebhookListResponse struct {
	Items []cache.Webhook `json:"items"`
}

// CacheWebhookTestResponse is the outcome of a test call of a webhook.
type CacheWebhookTestResponse struct {
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// CacheCountersResponse are the counters of the answers.
type CacheCountersResponse struct {
	// Requests counts the answers by X-Cache value (HIT, MISS, STALE,
	// BYPASS, EXPIRED, REVALIDATED).
	Requests    map[string]int64 `json:"requests"`
	BytesCache  int64            `json:"bytesCache"`
	BytesOrigin int64            `json:"bytesOrigin"`
	HitRatio    float64          `json:"hitRatio"`
}

func newCacheCounters(c cache.Counters) CacheCountersResponse {
	out := CacheCountersResponse{Requests: make(map[string]int64, len(c.Requests)), BytesCache: c.BytesCache, BytesOrigin: c.BytesOrigin, HitRatio: c.HitRatio}
	for k, v := range c.Requests {
		out.Requests[string(k)] = v
	}
	return out
}

// CacheHostStatsResponse are the statistics of a domain.
type CacheHostStatsResponse struct {
	Distribution string `json:"distribution"`
	Host         string `json:"host"`
	CacheCountersResponse
	Entries     int   `json:"entries"`
	MemoryBytes int64 `json:"memoryBytes"`
	DiskBytes   int64 `json:"diskBytes"`
}

func newCacheHostStats(list []cache.HostStats) []CacheHostStatsResponse {
	out := make([]CacheHostStatsResponse, 0, len(list))
	for _, h := range list {
		out = append(out, CacheHostStatsResponse{Distribution: h.Distribution, Host: h.Host, CacheCountersResponse: newCacheCounters(h.Counters),
			Entries: h.Entries, MemoryBytes: int64(h.MemorySize), DiskBytes: int64(h.DiskSize)})
	}
	return out
}

// CacheNodeStatsResponse are the statistics of a node.
type CacheNodeStatsResponse struct {
	Node   string     `json:"node"`
	Online bool       `json:"online"`
	Mode   cache.Mode `json:"mode"`
	CacheCountersResponse
	Entries          int   `json:"entries"`
	MemoryBytes      int64 `json:"memoryBytes"`
	MemoryLimitBytes int64 `json:"memoryLimitBytes"`
	DiskBytes        int64 `json:"diskBytes"`
	DiskLimitBytes   int64 `json:"diskLimitBytes"`
	Evictions        int64 `json:"evictions"`
	Invalidations    int64 `json:"invalidations"`
	Flushes          int64 `json:"flushes"`
	// Synced reports a node receiving the invalidations of every peer.
	Synced       bool                     `json:"synced"`
	OriginReads  int64                    `json:"originReads"`
	WebhookQueue int                      `json:"webhookQueue"`
	Hosts        []CacheHostStatsResponse `json:"hosts"`
	TopObjects   []cache.ObjectStat       `json:"topObjects"`
	TopMisses    []cache.ObjectStat       `json:"topMisses"`
	Error        string                   `json:"error,omitempty"`
}

// CacheStatsResponse is the body of GET /cache/stats: the cluster (or
// the distribution, or the domain) and every node.
type CacheStatsResponse struct {
	CacheCountersResponse
	Entries     int                      `json:"entries"`
	MemoryBytes int64                    `json:"memoryBytes"`
	DiskBytes   int64                    `json:"diskBytes"`
	Hosts       []CacheHostStatsResponse `json:"hosts"`
	TopObjects  []cache.ObjectStat       `json:"topObjects"`
	TopMisses   []cache.ObjectStat       `json:"topMisses"`
	Nodes       []CacheNodeStatsResponse `json:"nodes"`
}

func newCacheStats(r cache.StatsReport) CacheStatsResponse {
	out := CacheStatsResponse{
		CacheCountersResponse: newCacheCounters(r.Counters), Entries: r.Entries, MemoryBytes: int64(r.MemorySize), DiskBytes: int64(r.DiskSize),
		Hosts: newCacheHostStats(r.Hosts), TopObjects: nonNil(r.TopObjects), TopMisses: nonNil(r.TopMisses),
		Nodes: make([]CacheNodeStatsResponse, 0, len(r.Nodes)),
	}
	for _, n := range r.Nodes {
		out.Nodes = append(out.Nodes, CacheNodeStatsResponse{
			Node: n.Node, Online: n.Online, Mode: n.Mode, CacheCountersResponse: newCacheCounters(n.Counters), Entries: n.Entries,
			MemoryBytes: int64(n.MemorySize), MemoryLimitBytes: int64(n.MemoryLimit), DiskBytes: int64(n.DiskSize), DiskLimitBytes: int64(n.DiskLimit),
			Evictions: n.Evictions, Invalidations: n.Invalidations, Flushes: n.Flushes, Synced: n.Synced, OriginReads: n.OriginReads,
			WebhookQueue: n.WebhookQueue, Hosts: newCacheHostStats(n.Hosts), TopObjects: nonNil(n.TopObjects), TopMisses: nonNil(n.TopMisses), Error: n.Error,
		})
	}
	return out
}

// CachePresenceResponse is the entry of a key on a node.
type CachePresenceResponse struct {
	Key     string `json:"key"`
	Present bool   `json:"present"`
	// Tier is memory or disk; Kind object, head, info, negative or pass.
	Tier string `json:"tier,omitempty"`
	Kind string `json:"kind,omitempty"`
	// AgeSeconds is how long ago the entry was stored or revalidated,
	// ExpiresInSeconds how long it stays fresh (negative: stale).
	AgeSeconds       int64    `json:"ageSeconds"`
	ExpiresInSeconds int64    `json:"expiresInSeconds"`
	Stale            bool     `json:"stale,omitempty"`
	SizeBytes        int64    `json:"sizeBytes"`
	ETag             string   `json:"etag,omitempty"`
	Encodings        []string `json:"encodings"`
	Blocks           int      `json:"blocks,omitempty"`
}

// CacheNodePresenceResponse is the presence of the key on a node.
type CacheNodePresenceResponse struct {
	Node   string                `json:"node"`
	Online bool                  `json:"online"`
	Owner  bool                  `json:"owner,omitempty"`
	Entry  CachePresenceResponse `json:"entry"`
	Error  string                `json:"error,omitempty"`
}

// CacheInspectResponse is the body of GET /cache/inspect.
type CacheInspectResponse struct {
	URL          string `json:"url"`
	Distribution string `json:"distribution,omitempty"`
	Host         string `json:"host"`
	Path         string `json:"path"`
	Bucket       string `json:"bucket,omitempty"`
	Key          string `json:"key,omitempty"`
	CacheKey     string `json:"cacheKey,omitempty"`
	// Bypass reports a request the cache does not serve, Reason why.
	Bypass bool                        `json:"bypass"`
	Reason string                      `json:"reason,omitempty"`
	Policy cache.EffectivePolicy       `json:"policy"`
	Mode   cache.Mode                  `json:"mode"`
	Nodes  []CacheNodePresenceResponse `json:"nodes"`
}

func newCacheInspect(r cache.InspectResult) CacheInspectResponse {
	out := CacheInspectResponse{
		URL: r.URL, Distribution: r.Distribution, Host: r.Host, Path: r.Path, Bucket: r.Bucket, Key: r.Key, CacheKey: r.CacheKey,
		Bypass: r.Bypass, Reason: r.Reason, Policy: r.Policy, Mode: r.Mode, Nodes: make([]CacheNodePresenceResponse, 0, len(r.Nodes)),
	}
	if out.Policy.Fields == nil {
		out.Policy.Fields = map[string]cache.Level{}
	}
	for _, n := range r.Nodes {
		e := n.Entry
		out.Nodes = append(out.Nodes, CacheNodePresenceResponse{Node: n.Node, Online: n.Online, Owner: n.Owner, Error: n.Error, Entry: CachePresenceResponse{
			Key: e.Key, Present: e.Present, Tier: e.Tier, Kind: e.Kind, AgeSeconds: int64(e.Age.D().Seconds()),
			ExpiresInSeconds: int64(e.ExpiresIn.D().Seconds()), Stale: e.Stale, SizeBytes: int64(e.Size), ETag: e.ETag,
			Encodings: nonNil(e.Encodings), Blocks: e.Blocks,
		}})
	}
	return out
}

// --- handlers --------------------------------------------------------------------------

func (h *cacheRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// writeDocument writes a configuration document with its ETag.
func (h *cacheRoutes) writeDocument(w http.ResponseWriter, status int, etag string, v any) {
	setETag(w, etag)
	h.deps.Enc.WriteJSON(w, status, v)
}

func (h *cacheRoutes) listPolicies(w http.ResponseWriter, r *http.Request) {
	list, err := h.deps.Cache.ListPolicies(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, CachePolicyListResponse{Items: nonNil(list)})
}

func (h *cacheRoutes) createPolicy(w http.ResponseWriter, r *http.Request) {
	var p cache.Policy
	if err := decodeJSON(h.deps.Enc, r, &p, false); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.deps.Cache.CreatePolicy(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusCreated, out.ETag, out)
}

func (h *cacheRoutes) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := h.deps.Cache.GetPolicy(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusOK, p.ETag, p)
}

func (h *cacheRoutes) updatePolicy(w http.ResponseWriter, r *http.Request) {
	var p cache.Policy
	if err := decodeJSON(h.deps.Enc, r, &p, false); err != nil {
		h.fail(w, r, err)
		return
	}
	name := chi.URLParam(r, "name")
	if p.Name == "" {
		p.Name = name
	}
	if p.Name != name {
		h.fail(w, r, fmt.Errorf("%w: the name of the body (%s) is not the one of the path (%s)", domain.ErrInvalidArgument, p.Name, name))
		return
	}
	out, err := h.deps.Cache.UpdatePolicy(r.Context(), p, ifMatch(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusOK, out.ETag, out)
}

func (h *cacheRoutes) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Cache.DeletePolicy(r.Context(), chi.URLParam(r, "name")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *cacheRoutes) previewPolicy(w http.ResponseWriter, r *http.Request) {
	var p cache.Policy
	if err := decodeJSON(h.deps.Enc, r, &p, false); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.deps.Cache.PreviewPolicy(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *cacheRoutes) getSettings(w http.ResponseWriter, r *http.Request) {
	g, err := h.deps.Cache.GetSettings(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusOK, g.ETag, g)
}

func (h *cacheRoutes) updateSettings(w http.ResponseWriter, r *http.Request) {
	var g cache.GlobalConfig
	if err := decodeJSON(h.deps.Enc, r, &g, false); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.deps.Cache.UpdateSettings(r.Context(), g, ifMatch(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusOK, out.ETag, out)
}

// requestHeaders parses the repeated "header" query parameter ("Name:
// value") of the inspector.
func requestHeaders(r *http.Request) (http.Header, error) {
	out := http.Header{}
	for _, v := range r.URL.Query()["header"] {
		name, value, ok := strings.Cut(v, ":")
		if name = strings.TrimSpace(name); !ok || name == "" {
			return nil, fmt.Errorf("%w: header %q is not \"Name: value\"", domain.ErrInvalidArgument, v)
		}
		out.Add(name, strings.TrimSpace(value))
	}
	return out, nil
}

func (h *cacheRoutes) inspect(w http.ResponseWriter, r *http.Request) {
	header, err := requestHeaders(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	res, err := h.deps.Cache.Inspect(r.Context(), adminsvc.InspectCacheInput{URL: q.Get("url"), Distribution: q.Get("distribution"), Header: header})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newCacheInspect(res))
}

func (h *cacheRoutes) stats(w http.ResponseWriter, r *http.Request) {
	top, err := queryInt(r, "top")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	rep, err := h.deps.Cache.Stats(r.Context(), cache.StatsQuery{Distribution: q.Get("distribution"), Host: q.Get("host"), Top: top})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newCacheStats(rep))
}

// invalidationRequest decodes the body of an invalidation of dist.
func (h *cacheRoutes) invalidationRequest(r *http.Request, dist string) (cache.InvalidationRequest, error) {
	var in CacheInvalidationRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		return cache.InvalidationRequest{}, err
	}
	return cache.InvalidationRequest{Distribution: dist, Domain: in.Domain, Paths: in.Paths, Patterns: in.Patterns,
		Tags: in.Tags, All: in.All, Soft: in.Soft}, nil
}

func (h *cacheRoutes) runInvalidation(w http.ResponseWriter, r *http.Request, dist string) {
	req, err := h.invalidationRequest(r, dist)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	inv, err := h.deps.Cache.Invalidate(r.Context(), adminsvc.InvalidateCacheInput{Requester: requesterOf(r), Request: req})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, inv)
}

func (h *cacheRoutes) purgeAll(w http.ResponseWriter, r *http.Request) { h.runInvalidation(w, r, "") }

func (h *cacheRoutes) invalidate(w http.ResponseWriter, r *http.Request) {
	h.runInvalidation(w, r, chi.URLParam(r, "id"))
}

func (h *cacheRoutes) writeInvalidations(w http.ResponseWriter, r *http.Request, dist string) {
	list, err := h.deps.Cache.ListInvalidations(r.Context(), dist)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, CacheInvalidationListResponse{Items: nonNil(list)})
}

func (h *cacheRoutes) listGlobalInvalidations(w http.ResponseWriter, r *http.Request) {
	h.writeInvalidations(w, r, "")
}

func (h *cacheRoutes) listInvalidations(w http.ResponseWriter, r *http.Request) {
	h.writeInvalidations(w, r, chi.URLParam(r, "id"))
}

func (h *cacheRoutes) findInvalidation(w http.ResponseWriter, r *http.Request) {
	inv, err := h.deps.Cache.FindInvalidation(r.Context(), chi.URLParam(r, "inv"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, inv)
}

func (h *cacheRoutes) getInvalidation(w http.ResponseWriter, r *http.Request) {
	inv, err := h.deps.Cache.GetInvalidation(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "inv"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, inv)
}

func (h *cacheRoutes) getDistribution(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.Cache.GetDistribution(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusOK, cfg.ETag, cfg)
}

func (h *cacheRoutes) updateDistribution(w http.ResponseWriter, r *http.Request) {
	var cfg cache.DistributionConfig
	if err := decodeJSON(h.deps.Enc, r, &cfg, false); err != nil {
		h.fail(w, r, err)
		return
	}
	id := chi.URLParam(r, "id")
	if cfg.Distribution == "" {
		cfg.Distribution = id
	}
	if cfg.Distribution != id {
		h.fail(w, r, fmt.Errorf("%w: the distribution of the body (%s) is not the one of the path (%s)", domain.ErrInvalidArgument, cfg.Distribution, id))
		return
	}
	out, err := h.deps.Cache.UpdateDistribution(r.Context(), cfg, ifMatch(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeDocument(w, http.StatusOK, out.ETag, out)
}

func (h *cacheRoutes) effective(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	eff, err := h.deps.Cache.Effective(r.Context(), cache.EffectiveInput{Distribution: chi.URLParam(r, "id"), Host: q.Get("host"), Path: q.Get("path")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if eff.Fields == nil {
		eff.Fields = map[string]cache.Level{}
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, eff)
}

func (h *cacheRoutes) warm(w http.ResponseWriter, r *http.Request) {
	var in CacheWarmRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	job, err := h.deps.Cache.Warm(r.Context(), adminsvc.WarmCacheInput{Requester: requesterOf(r), Request: cache.WarmRequest{
		Distribution: chi.URLParam(r, "id"), Domain: in.Domain, Paths: in.Paths, URLs: in.URLs, Prefix: in.Prefix, Pattern: in.Pattern,
		Concurrency: in.Concurrency, Bandwidth: in.Bandwidth,
	}})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusAccepted, job)
}

func (h *cacheRoutes) warmJob(w http.ResponseWriter, r *http.Request) {
	job, err := h.deps.Cache.GetWarmJob(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "job"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, job)
}

func (h *cacheRoutes) listWebhooks(w http.ResponseWriter, r *http.Request) {
	list, err := h.deps.Cache.ListWebhooks(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, CacheWebhookListResponse{Items: nonNil(list)})
}

func (h *cacheRoutes) addWebhook(w http.ResponseWriter, r *http.Request) {
	var wh cache.Webhook
	if err := decodeJSON(h.deps.Enc, r, &wh, false); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.deps.Cache.AddWebhook(r.Context(), chi.URLParam(r, "id"), wh)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, out)
}

func (h *cacheRoutes) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Cache.DeleteWebhook(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "webhook")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *cacheRoutes) testWebhook(w http.ResponseWriter, r *http.Request) {
	res, err := h.deps.Cache.TestWebhook(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "webhook"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, CacheWebhookTestResponse{OK: res.OK, Status: res.Status, Error: res.Error, DurationMs: res.Duration.Milliseconds()})
}
