package nexoadmin

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The cache of the distributions (docs/11): reusable policies, settings of
// the cluster, of a distribution, of its domains and paths, invalidations,
// warm-ups, purge webhooks, the inspector and the statistics.
//
// The durations of the configuration documents are strings in the Go
// syntax or in days ("90s", "1h30m", "365d"), the sizes strings with a
// unit ("512MiB", "20GiB") or a number of bytes; an empty string leaves
// the setting unset (inherited from the more general level).

// X-Cache values (docs/11 §11.7), the keys of CacheCounters.Requests.
const (
	XCacheHit         = "HIT"
	XCacheMiss        = "MISS"
	XCacheStale       = "STALE"
	XCacheBypass      = "BYPASS"
	XCacheExpired     = "EXPIRED"
	XCacheRevalidated = "REVALIDATED"
)

// Built-in cache policies.
const (
	CachePolicyDisabled        = "disabled"
	CachePolicyStandard        = "standard"
	CachePolicyAggressive      = "aggressive"
	CachePolicyImmutableAssets = "immutable-assets"
	CachePolicyRespectOrigin   = "respect-origin"
)

// States of the invalidations and of the warm-ups, on the cluster and on
// a node.
const (
	InvalidationInProgress = "in-progress"
	InvalidationCompleted  = "completed"
	NodePending            = "pending"
	NodeCompleted          = "completed"
	WarmRunning            = "running"
	WarmCompleted          = "completed"
	WarmFailed             = "failed"
)

// Purge webhook types.
const (
	WebhookCloudflare = "cloudflare"
	WebhookFastly     = "fastly"
	WebhookAkamai     = "akamai"
	WebhookNginx      = "nginx"
	WebhookVarnish    = "varnish"
	WebhookGeneric    = "generic"
)

// Problem codes of the cache.
const (
	CodeCachePolicyNotFound  = "cache_policy_not_found"
	CodeInvalidationNotFound = "invalidation_not_found"
	CodeWarmJobNotFound      = "warm_job_not_found"
	CodeWebhookNotFound      = "webhook_not_found"
	CodeInvalidCacheRequest  = "invalid_cache_request"
	CodeBuiltinCachePolicy   = "builtin_cache_policy"
	CodeCachePolicyInUse     = "cache_policy_in_use"
	CodeAlreadyExists        = "already_exists"
)

// CacheTTL bounds how long an entry is fresh: Default for the objects that
// state no lifetime, Min and Max for every lifetime.
type CacheTTL struct {
	Default string `json:"default,omitempty" yaml:"default,omitempty"`
	Min     string `json:"min,omitempty" yaml:"min,omitempty"`
	Max     string `json:"max,omitempty" yaml:"max,omitempty"`
}

// CacheQueryString selects the query parameters of the cache key: Mode
// none, all, allowlist or denylist (of Params).
type CacheQueryString struct {
	Mode   string   `json:"mode" yaml:"mode"`
	Params []string `json:"params,omitempty" yaml:"params,omitempty"`
}

// CacheKeyPolicy is how the cache key of a request is computed.
type CacheKeyPolicy struct {
	QueryString *CacheQueryString `json:"queryString,omitempty" yaml:"queryString,omitempty"`
	Headers     []string          `json:"headers,omitempty" yaml:"headers,omitempty"`
	Cookies     []string          `json:"cookies,omitempty" yaml:"cookies,omitempty"`
	Normalize   *bool             `json:"normalize,omitempty" yaml:"normalize,omitempty"`
	IncludeHost *bool             `json:"includeHost,omitempty" yaml:"includeHost,omitempty"`
}

// CacheBypass lists the conditions under which a request skips the cache.
type CacheBypass struct {
	Always        *bool    `json:"always,omitempty" yaml:"always,omitempty"`
	Authorization *bool    `json:"authorization,omitempty" yaml:"authorization,omitempty"`
	Cookies       []string `json:"cookies,omitempty" yaml:"cookies,omitempty"`
	QueryParams   []string `json:"queryParams,omitempty" yaml:"queryParams,omitempty"`
	Headers       []string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// CacheCompression stores compressed variants of the textual objects.
type CacheCompression struct {
	Enabled      *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Algorithms   []string `json:"algorithms,omitempty" yaml:"algorithms,omitempty"`
	ContentTypes []string `json:"contentTypes,omitempty" yaml:"contentTypes,omitempty"`
	MinSize      string   `json:"minSize,omitempty" yaml:"minSize,omitempty"`
}

// CacheRangeCaching caches the large objects by blocks.
type CacheRangeCaching struct {
	Enabled   *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	BlockSize string `json:"blockSize,omitempty" yaml:"blockSize,omitempty"`
}

// CacheContentTypes filters the cached objects by media type.
type CacheContentTypes struct {
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty" yaml:"deny,omitempty"`
}

// CachePolicySpec are the settings of a cache policy (docs/11 §11.2); an
// unset field inherits from the more general level.
type CachePolicySpec struct {
	TTL                       *CacheTTL          `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	RespectObjectCacheControl *bool              `json:"respectObjectCacheControl,omitempty" yaml:"respectObjectCacheControl,omitempty"`
	BrowserCacheControl       *string            `json:"browserCacheControl,omitempty" yaml:"browserCacheControl,omitempty"`
	CacheKey                  *CacheKeyPolicy    `json:"cacheKey,omitempty" yaml:"cacheKey,omitempty"`
	StaleWhileRevalidate      string             `json:"staleWhileRevalidate,omitempty" yaml:"staleWhileRevalidate,omitempty"`
	StaleIfError              string             `json:"staleIfError,omitempty" yaml:"staleIfError,omitempty"`
	NegativeCaching           string             `json:"negativeCaching,omitempty" yaml:"negativeCaching,omitempty"`
	Bypass                    *CacheBypass       `json:"bypass,omitempty" yaml:"bypass,omitempty"`
	Compression               *CacheCompression  `json:"compression,omitempty" yaml:"compression,omitempty"`
	MinSize                   string             `json:"minSize,omitempty" yaml:"minSize,omitempty"`
	MaxObjectSize             string             `json:"maxObjectSize,omitempty" yaml:"maxObjectSize,omitempty"`
	RangeCaching              *CacheRangeCaching `json:"rangeCaching,omitempty" yaml:"rangeCaching,omitempty"`
	ContentTypes              *CacheContentTypes `json:"contentTypes,omitempty" yaml:"contentTypes,omitempty"`
}

// CachePolicy is a reusable cache policy referenced by name. The policies
// returned by the server are complete.
type CachePolicy struct {
	Name    string `json:"name" yaml:"name"`
	Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
	// Builtin reports a predefined, read-only policy.
	Builtin         bool `json:"builtin,omitempty" yaml:"builtin,omitempty"`
	CachePolicySpec `yaml:",inline"`
	UpdatedAt       *time.Time `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	ETag            string     `json:"etag,omitempty" yaml:"etag,omitempty"`
}

// CachePolicyPreview is a validated policy with the Cache-Control it
// sends to the clients.
type CachePolicyPreview struct {
	Policy       CachePolicy `json:"policy"`
	CacheControl string      `json:"cacheControl"`
	// Inherited reports that the policy sets none: the Cache-Control of
	// the objects is sent.
	Inherited bool   `json:"inherited"`
	Summary   string `json:"summary"`
}

// CacheSettings are the cache settings of a level: whether the cache is
// on, the policy it starts from and the fields it overrides.
type CacheSettings struct {
	Enabled         *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Policy          string `json:"policy,omitempty" yaml:"policy,omitempty"`
	CachePolicySpec `yaml:",inline"`
	// Quota bounds the bytes cached for the distribution or the domain.
	Quota string `json:"quota,omitempty" yaml:"quota,omitempty"`
	// PurgeWebhooks names the webhooks purged for the domain.
	PurgeWebhooks []string `json:"purgeWebhooks,omitempty" yaml:"purgeWebhooks,omitempty"`
	// AutoInvalidate drops the entries of the objects written (global
	// and distribution levels).
	AutoInvalidate *bool `json:"autoInvalidate,omitempty" yaml:"autoInvalidate,omitempty"`
}

// CacheDebugHeader unlocks the debug headers of the answers.
type CacheDebugHeader struct {
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	Secret string `json:"secret,omitempty" yaml:"secret,omitempty"`
}

// CacheGlobalSettings are the cache settings of the cluster.
type CacheGlobalSettings struct {
	Cache      CacheSettings     `json:"cache" yaml:"cache"`
	NodeHeader bool              `json:"nodeHeader,omitempty" yaml:"nodeHeader,omitempty"`
	Debug      *CacheDebugHeader `json:"debug,omitempty" yaml:"debug,omitempty"`
	UpdatedAt  *time.Time        `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	ETag       string            `json:"etag,omitempty" yaml:"etag,omitempty"`
}

// CacheDomain are the overrides of a domain of a distribution.
type CacheDomain struct {
	Host  string        `json:"host" yaml:"host"`
	Cache CacheSettings `json:"cache" yaml:"cache"`
}

// CacheBehavior are the overrides of the paths a pattern selects.
type CacheBehavior struct {
	PathPattern KeyPattern    `json:"pathPattern" yaml:"pathPattern"`
	Cache       CacheSettings `json:"cache" yaml:"cache"`
}

// CacheWebhook purges a CDN or a proxy on the invalidations of a
// distribution; its secrets are masked in answers.
type CacheWebhook struct {
	ID           string            `json:"id" yaml:"id"`
	Type         string            `json:"type" yaml:"type"`
	Domains      []string          `json:"domains,omitempty" yaml:"domains,omitempty"`
	URL          string            `json:"url,omitempty" yaml:"url,omitempty"`
	Zone         string            `json:"zone,omitempty" yaml:"zone,omitempty"`
	Token        string            `json:"token,omitempty" yaml:"token,omitempty"`
	ClientToken  string            `json:"clientToken,omitempty" yaml:"clientToken,omitempty"`
	ClientSecret string            `json:"clientSecret,omitempty" yaml:"clientSecret,omitempty"`
	Network      string            `json:"network,omitempty" yaml:"network,omitempty"`
	CPCode       string            `json:"cpCode,omitempty" yaml:"cpCode,omitempty"`
	Method       string            `json:"method,omitempty" yaml:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Body         string            `json:"body,omitempty" yaml:"body,omitempty"`
	Scheme       string            `json:"scheme,omitempty" yaml:"scheme,omitempty"`
}

// DistributionCache are the cache settings of a distribution: its
// defaults, the overrides of its domains and paths, its purge webhooks
// and the objects warmed on write.
type DistributionCache struct {
	Distribution string          `json:"distribution" yaml:"distribution"`
	Cache        CacheSettings   `json:"cache" yaml:"cache"`
	Domains      []CacheDomain   `json:"domains,omitempty" yaml:"domains,omitempty"`
	Behaviors    []CacheBehavior `json:"behaviors,omitempty" yaml:"behaviors,omitempty"`
	Webhooks     []CacheWebhook  `json:"webhooks,omitempty" yaml:"webhooks,omitempty"`
	WarmOnWrite  []KeyPattern    `json:"warmOnWrite,omitempty" yaml:"warmOnWrite,omitempty"`
	UpdatedAt    *time.Time      `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	ETag         string          `json:"etag,omitempty" yaml:"etag,omitempty"`
}

// CacheEffectivePolicy is the policy that applies to a path of a domain,
// with the level (default, global, distribution, domain, behavior) each
// setting comes from.
type CacheEffectivePolicy struct {
	Enabled      bool            `json:"enabled"`
	EnabledLevel string          `json:"enabledLevel"`
	PolicyName   string          `json:"policyName"`
	PolicyLevel  string          `json:"policyLevel"`
	Policy       CachePolicySpec `json:"policy"`
	// Fields maps every setting ("ttl.default") to its level.
	Fields map[string]string `json:"fields"`
	Domain string            `json:"domain,omitempty"`
	// Behavior is the index of the behavior that matched, -1 for none.
	Behavior          int    `json:"behavior"`
	DistributionQuota string `json:"distributionQuota,omitempty"`
	DomainQuota       string `json:"domainQuota,omitempty"`
	CacheControl      string `json:"cacheControl,omitempty"`
	AutoInvalidate    bool   `json:"autoInvalidate"`
}

// CacheInvalidationRequest is a manual invalidation: exact paths (or
// absolute URLs of a domain), patterns, tags, a domain alone or All.
type CacheInvalidationRequest struct {
	Domain   string       `json:"domain,omitempty"`
	Paths    []string     `json:"paths,omitempty"`
	Patterns []KeyPattern `json:"patterns,omitempty"`
	Tags     []string     `json:"tags,omitempty"`
	All      bool         `json:"all,omitempty"`
	Soft     bool         `json:"soft,omitempty"`
}

// CacheInvalidationRecord is the request of a recorded invalidation.
type CacheInvalidationRecord struct {
	// Distribution is empty for a global purge.
	Distribution string `json:"distribution,omitempty"`
	CacheInvalidationRequest
	Actor string `json:"actor,omitempty"`
}

// CacheNodeStatus is the state of an invalidation or a warm-up on a node.
type CacheNodeStatus struct {
	Node    string    `json:"node"`
	State   string    `json:"state"`
	At      time.Time `json:"at,omitempty"`
	Removed int       `json:"removed,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// CacheInvalidation is an invalidation with its state on every node.
type CacheInvalidation struct {
	ID        string                  `json:"id"`
	Request   CacheInvalidationRecord `json:"request"`
	CreatedAt time.Time               `json:"createdAt"`
	// State is InvalidationCompleted once every node applied it.
	State string            `json:"state"`
	Nodes []CacheNodeStatus `json:"nodes"`
}

// CacheWarmRequest asks to cache objects of a domain before they are
// requested.
type CacheWarmRequest struct {
	Domain      string      `json:"domain,omitempty"`
	Paths       []string    `json:"paths,omitempty"`
	URLs        []string    `json:"urls,omitempty"`
	Prefix      string      `json:"prefix,omitempty"`
	Pattern     *KeyPattern `json:"pattern,omitempty"`
	Concurrency int         `json:"concurrency,omitempty"`
	// Bandwidth bounds the bytes read per second ("10MiB").
	Bandwidth string `json:"bandwidth,omitempty"`
}

// CacheWarmRecord is the request of a warm-up.
type CacheWarmRecord struct {
	Distribution string `json:"distribution"`
	CacheWarmRequest
	Actor string `json:"actor,omitempty"`
}

// CacheWarmNode is the progress of a warm-up on a node.
type CacheWarmNode struct {
	CacheNodeStatus
	Total  int   `json:"total"`
	Done   int   `json:"done"`
	Failed int   `json:"failed"`
	Bytes  int64 `json:"bytes"`
}

// CacheWarmJob is a warm-up with its progress on every node.
type CacheWarmJob struct {
	ID        string          `json:"id"`
	Request   CacheWarmRecord `json:"request"`
	CreatedAt time.Time       `json:"createdAt"`
	State     string          `json:"state"`
	Nodes     []CacheWarmNode `json:"nodes"`
}

// CacheInspectRequest asks how the cache serves a URL.
type CacheInspectRequest struct {
	// URL is an absolute URL, or a path of Distribution.
	URL          string
	Distribution string
	// Headers are request headers of the key, "Name: value".
	Headers []string
}

// CachePresence is the entry of a key on a node.
type CachePresence struct {
	Key              string   `json:"key"`
	Present          bool     `json:"present"`
	Tier             string   `json:"tier,omitempty"`
	Kind             string   `json:"kind,omitempty"`
	AgeSeconds       int64    `json:"ageSeconds"`
	ExpiresInSeconds int64    `json:"expiresInSeconds"`
	Stale            bool     `json:"stale,omitempty"`
	SizeBytes        int64    `json:"sizeBytes"`
	ETag             string   `json:"etag,omitempty"`
	Encodings        []string `json:"encodings"`
	Blocks           int      `json:"blocks,omitempty"`
}

// CacheNodePresence is the presence of the key on a node.
type CacheNodePresence struct {
	Node   string        `json:"node"`
	Online bool          `json:"online"`
	Owner  bool          `json:"owner,omitempty"`
	Entry  CachePresence `json:"entry"`
	Error  string        `json:"error,omitempty"`
}

// CacheInspection is how the cache serves a URL.
type CacheInspection struct {
	URL          string               `json:"url"`
	Distribution string               `json:"distribution,omitempty"`
	Host         string               `json:"host"`
	Path         string               `json:"path"`
	Bucket       string               `json:"bucket,omitempty"`
	Key          string               `json:"key,omitempty"`
	CacheKey     string               `json:"cacheKey,omitempty"`
	Bypass       bool                 `json:"bypass"`
	Reason       string               `json:"reason,omitempty"`
	Policy       CacheEffectivePolicy `json:"policy"`
	Mode         string               `json:"mode"`
	Nodes        []CacheNodePresence  `json:"nodes"`
}

// CacheStatsQuery narrows the statistics.
type CacheStatsQuery struct {
	Distribution string
	Host         string
	// Top bounds the lists of top objects (0: the default of the server).
	Top int
}

// CacheCounters are the counters of the answers.
type CacheCounters struct {
	// Requests counts the answers by X-Cache value.
	Requests    map[string]int64 `json:"requests"`
	BytesCache  int64            `json:"bytesCache"`
	BytesOrigin int64            `json:"bytesOrigin"`
	HitRatio    float64          `json:"hitRatio"`
}

// CacheHostStats are the statistics of a domain.
type CacheHostStats struct {
	Distribution string `json:"distribution"`
	Host         string `json:"host"`
	CacheCounters
	Entries     int   `json:"entries"`
	MemoryBytes int64 `json:"memoryBytes"`
	DiskBytes   int64 `json:"diskBytes"`
}

// CacheObjectStat counts the requests of a path.
type CacheObjectStat struct {
	Distribution string `json:"distribution"`
	Host         string `json:"host"`
	Path         string `json:"path"`
	Requests     int64  `json:"requests"`
	Misses       int64  `json:"misses"`
}

// CacheNodeStats are the statistics of a node.
type CacheNodeStats struct {
	Node   string `json:"node"`
	Online bool   `json:"online"`
	Mode   string `json:"mode"`
	CacheCounters
	Entries          int               `json:"entries"`
	MemoryBytes      int64             `json:"memoryBytes"`
	MemoryLimitBytes int64             `json:"memoryLimitBytes"`
	DiskBytes        int64             `json:"diskBytes"`
	DiskLimitBytes   int64             `json:"diskLimitBytes"`
	Evictions        int64             `json:"evictions"`
	Invalidations    int64             `json:"invalidations"`
	Flushes          int64             `json:"flushes"`
	Synced           bool              `json:"synced"`
	OriginReads      int64             `json:"originReads"`
	WebhookQueue     int               `json:"webhookQueue"`
	Hosts            []CacheHostStats  `json:"hosts"`
	TopObjects       []CacheObjectStat `json:"topObjects"`
	TopMisses        []CacheObjectStat `json:"topMisses"`
	Error            string            `json:"error,omitempty"`
}

// CacheStats are the statistics of the cluster (summed) and of every
// node.
type CacheStats struct {
	CacheCounters
	Entries     int               `json:"entries"`
	MemoryBytes int64             `json:"memoryBytes"`
	DiskBytes   int64             `json:"diskBytes"`
	Hosts       []CacheHostStats  `json:"hosts"`
	TopObjects  []CacheObjectStat `json:"topObjects"`
	TopMisses   []CacheObjectStat `json:"topMisses"`
	Nodes       []CacheNodeStats  `json:"nodes"`
}

// CacheWebhookTestResult is the outcome of a test call of a webhook.
type CacheWebhookTestResult struct {
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// ListCachePolicies implements AdminClient.
func (c *client) ListCachePolicies(ctx context.Context) ([]CachePolicy, error) {
	var out struct {
		Items []CachePolicy `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/cache/policies"}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []CachePolicy{}
	}
	return out.Items, nil
}

// GetCachePolicy implements AdminClient.
func (c *client) GetCachePolicy(ctx context.Context, name string) (*CachePolicy, error) {
	var out CachePolicy
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("cache", "policies", name)}, &out)
}

// CreateCachePolicy implements AdminClient.
func (c *client) CreateCachePolicy(ctx context.Context, p CachePolicy) (*CachePolicy, error) {
	var out CachePolicy
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/cache/policies", body: p}, &out)
}

// UpdateCachePolicy implements AdminClient.
func (c *client) UpdateCachePolicy(ctx context.Context, p CachePolicy, ifMatch string) (*CachePolicy, error) {
	var out CachePolicy
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("cache", "policies", p.Name), header: ifMatchHeader(ifMatch), body: p}, &out)
}

// DeleteCachePolicy implements AdminClient.
func (c *client) DeleteCachePolicy(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("cache", "policies", name)}, nil)
}

// PreviewCachePolicy implements AdminClient.
func (c *client) PreviewCachePolicy(ctx context.Context, p CachePolicy) (*CachePolicyPreview, error) {
	var out CachePolicyPreview
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/cache/preview", body: p}, &out)
}

// GetCacheSettings implements AdminClient.
func (c *client) GetCacheSettings(ctx context.Context) (*CacheGlobalSettings, error) {
	var out CacheGlobalSettings
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/cache/settings"}, &out)
}

// SetCacheSettings implements AdminClient.
func (c *client) SetCacheSettings(ctx context.Context, s CacheGlobalSettings, ifMatch string) (*CacheGlobalSettings, error) {
	var out CacheGlobalSettings
	return &out, c.do(ctx, request{method: http.MethodPut, path: "/cache/settings", header: ifMatchHeader(ifMatch), body: s}, &out)
}

// GetDistributionCache implements AdminClient.
func (c *client) GetDistributionCache(ctx context.Context, id string) (*DistributionCache, error) {
	var out DistributionCache
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("distributions", id, "cache")}, &out)
}

// SetDistributionCache implements AdminClient.
func (c *client) SetDistributionCache(ctx context.Context, cfg DistributionCache, ifMatch string) (*DistributionCache, error) {
	var out DistributionCache
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("distributions", cfg.Distribution, "cache"), header: ifMatchHeader(ifMatch), body: cfg}, &out)
}

// EffectiveCachePolicy implements AdminClient.
func (c *client) EffectiveCachePolicy(ctx context.Context, id, host, path string) (*CacheEffectivePolicy, error) {
	q := url.Values{}
	if host != "" {
		q.Set("host", host)
	}
	if path != "" {
		q.Set("path", path)
	}
	var out CacheEffectivePolicy
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("distributions", id, "cache", "effective"), query: q}, &out)
}

// InvalidateCache implements AdminClient.
func (c *client) InvalidateCache(ctx context.Context, distribution string, in CacheInvalidationRequest) (*CacheInvalidation, error) {
	path := "/cache/invalidations"
	if distribution != "" {
		path = pathOf("distributions", distribution, "invalidations")
	}
	var out CacheInvalidation
	return &out, c.do(ctx, request{method: http.MethodPost, path: path, body: in}, &out)
}

// GetCacheInvalidation implements AdminClient.
func (c *client) GetCacheInvalidation(ctx context.Context, distribution, id string) (*CacheInvalidation, error) {
	path := pathOf("cache", "invalidations", id)
	if distribution != "" {
		path = pathOf("distributions", distribution, "invalidations", id)
	}
	var out CacheInvalidation
	return &out, c.do(ctx, request{method: http.MethodGet, path: path}, &out)
}

// ListCacheInvalidations implements AdminClient.
func (c *client) ListCacheInvalidations(ctx context.Context, distribution string) ([]CacheInvalidation, error) {
	path := "/cache/invalidations"
	if distribution != "" {
		path = pathOf("distributions", distribution, "invalidations")
	}
	var out struct {
		Items []CacheInvalidation `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: path}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []CacheInvalidation{}
	}
	return out.Items, nil
}

// WarmCache implements AdminClient.
func (c *client) WarmCache(ctx context.Context, distribution string, in CacheWarmRequest) (*CacheWarmJob, error) {
	var out CacheWarmJob
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("distributions", distribution, "cache", "warm"), body: in}, &out)
}

// GetCacheWarmJob implements AdminClient.
func (c *client) GetCacheWarmJob(ctx context.Context, distribution, id string) (*CacheWarmJob, error) {
	var out CacheWarmJob
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("distributions", distribution, "cache", "warm", id)}, &out)
}

// InspectCache implements AdminClient.
func (c *client) InspectCache(ctx context.Context, in CacheInspectRequest) (*CacheInspection, error) {
	q := url.Values{"url": {in.URL}}
	if in.Distribution != "" {
		q.Set("distribution", in.Distribution)
	}
	for _, h := range in.Headers {
		q.Add("header", h)
	}
	var out CacheInspection
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/cache/inspect", query: q}, &out)
}

// CacheStats implements AdminClient.
func (c *client) CacheStats(ctx context.Context, in CacheStatsQuery) (*CacheStats, error) {
	q := url.Values{}
	if in.Distribution != "" {
		q.Set("distribution", in.Distribution)
	}
	if in.Host != "" {
		q.Set("host", in.Host)
	}
	if in.Top > 0 {
		q.Set("top", strconv.Itoa(in.Top))
	}
	var out CacheStats
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/cache/stats", query: q}, &out)
}

// ListCacheWebhooks implements AdminClient.
func (c *client) ListCacheWebhooks(ctx context.Context, distribution string) ([]CacheWebhook, error) {
	var out struct {
		Items []CacheWebhook `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: pathOf("distributions", distribution, "cache", "webhooks")}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []CacheWebhook{}
	}
	return out.Items, nil
}

// AddCacheWebhook implements AdminClient.
func (c *client) AddCacheWebhook(ctx context.Context, distribution string, w CacheWebhook) (*CacheWebhook, error) {
	var out CacheWebhook
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("distributions", distribution, "cache", "webhooks"), body: w}, &out)
}

// DeleteCacheWebhook implements AdminClient.
func (c *client) DeleteCacheWebhook(ctx context.Context, distribution, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("distributions", distribution, "cache", "webhooks", id)}, nil)
}

// TestCacheWebhook implements AdminClient.
func (c *client) TestCacheWebhook(ctx context.Context, distribution, id string) (*CacheWebhookTestResult, error) {
	var out CacheWebhookTestResult
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("distributions", distribution, "cache", "webhooks", id, "test")}, &out)
}
