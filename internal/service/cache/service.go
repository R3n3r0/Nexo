// Package cache implements the HTTP cache of the distributions (docs/11):
// reusable cache policies inherited from the cluster to the distribution,
// the domain and the behavior; a memory tier (W-TinyLFU) and an optional
// disk tier with quotas per cluster, distribution and domain; the
// invalidations, automatic on every write of the object layer and manual
// (paths, patterns, tags, domains, everything, soft or hard), propagated to
// every node with a sequence number; the shared mode of a cluster, where a
// key has an owner node chosen by rendezvous hashing; the purge webhooks of
// the CDNs and proxies in front of the cluster; the warm-up, the inspector
// and the statistics.
//
// # What is cached
//
// The cache sits between the distributions and the object layer: it
// serves the object reads (GetObject, GetObjectInfo) of the requests of a
// distribution, never the decisions about them. A request is admitted,
// redirected, matched to its behavior, its signed URL or cookie is
// verified and its access authorized (access rules, bucket policy, block
// public access) on every request, before the cache is looked up: a change
// of the access rules applies to the next request, a signed URL that
// expired is refused even when the object is cached, the response headers
// computed for a viewer (CORS, cookies) are never stored. The requests
// carrying credentials (an Authorization header, a presigned query) bypass
// the cache by default; a policy that lets them use it keys their entries
// by credential, so that they are never served to another viewer.
//
// Cached are the descriptions and the content of the objects (the
// "pieces"): the whole content of the objects up to the block size, the
// blocks of the larger ones (range caching), the compressed variants of
// the textual ones, the negative answers (missing object, refused read)
// and the objects found uncacheable (hit-for-pass). The objects encrypted
// at rest (SSE) stay in memory only, never on the cache disk.
//
// # Invalidation
//
// Every write of the object layer (PUT, POST, copy, completed multipart
// upload, delete, delete marker, tags, lifecycle expiration, bucket
// creation and removal) reaches the cache synchronously through the event
// hook (NewEvents) before the write is acknowledged: the entries of the
// object are dropped on this node and on every peer, so that no node serves
// the previous content after the 200 of the write. A fill that read the
// origin before an invalidation of its object is never stored. The
// invalidations carry a per-node sequence number: a node that misses one
// (it was unreachable) drops its whole cache; while a peer is unreachable,
// or before its sequence is known, the entries are revalidated with the
// origin before they are served.
package cache

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Mode is how the nodes of a cluster share their caches (docs/11 §11.4).
type Mode string

// Cluster modes.
const (
	// ModeLocal gives every node its own cache (the default).
	ModeLocal Mode = "local"
	// ModeShared assigns every key to an owner node (rendezvous hashing
	// over the online nodes): the other nodes ask it through the internode
	// RPC, the cluster keeps one copy of an object.
	ModeShared Mode = "shared"
	// ModeSharedLocal is ModeShared plus a small local memory tier for the
	// hottest objects.
	ModeSharedLocal Mode = "shared+local"
)

// Result is the X-Cache value of an answer.
type Result string

// Results of a cache lookup (docs/11 §11.7).
const (
	// ResultHit is a fresh entry.
	ResultHit Result = "HIT"
	// ResultMiss is an object read from the origin (and stored).
	ResultMiss Result = "MISS"
	// ResultStale is an expired entry served while it is refreshed in the
	// background (stale-while-revalidate) or because the origin failed
	// (stale-if-error).
	ResultStale Result = "STALE"
	// ResultBypass is a request the cache does not serve (method,
	// credentials, bypass conditions, disabled cache, object out of the
	// limits of the policy).
	ResultBypass Result = "BYPASS"
	// ResultExpired is an expired entry replaced with the content of the
	// origin.
	ResultExpired Result = "EXPIRED"
	// ResultRevalidated is an expired (or unverified) entry the origin
	// confirmed unchanged.
	ResultRevalidated Result = "REVALIDATED"
)

// Response headers of the cache.
const (
	HeaderXCache     = "X-Cache"
	HeaderXCacheNode = "X-Cache-Node"
	HeaderXCacheKey  = "X-Cache-Key"
	// HeaderXCachePolicy names the effective policy (debug answers only).
	HeaderXCachePolicy = "X-Cache-Policy"
	// DefaultDebugHeader is the request header that unlocks the debug
	// headers when it carries the debug secret of the global settings.
	DefaultDebugHeader = "X-Nexo-Cache-Debug"
)

// Level is where a setting of the effective policy comes from (docs/11
// §11.1): the levels are applied from the most general to the most
// specific.
type Level string

// Configuration levels.
const (
	// LevelDefault is the built-in default (the standard policy).
	LevelDefault Level = "default"
	// LevelGlobal is the cluster-wide setting.
	LevelGlobal Level = "global"
	// LevelDistribution is the default of a distribution.
	LevelDistribution Level = "distribution"
	// LevelDomain is the override of a domain (alias) of a distribution.
	LevelDomain Level = "domain"
	// LevelBehavior is the override of a path pattern of a distribution.
	LevelBehavior Level = "behavior"
)

// Names of the built-in policies (docs/11 §11.2).
const (
	PolicyDisabled        = "disabled"
	PolicyStandard        = "standard"
	PolicyAggressive      = "aggressive"
	PolicyImmutableAssets = "immutable-assets"
	PolicyRespectOrigin   = "respect-origin"
)

// Query string modes of the cache key.
const (
	// QueryNone leaves the query out of the key (the default).
	QueryNone = "none"
	// QueryAll keys the entries by the whole query.
	QueryAll = "all"
	// QueryAllowlist keys the entries by the listed parameters only.
	QueryAllowlist = "allowlist"
	// QueryDenylist keys the entries by every parameter but the listed
	// ones.
	QueryDenylist = "denylist"
)

// Content encodings of the compressed variants.
const (
	EncodingGzip   = "gzip"
	EncodingBrotli = "br"
	EncodingZstd   = "zstd"
)

// Duration is a duration of the cache configuration: Go syntax ("1h30m",
// "90s") or a number of days ("365d"); JSON numbers are seconds. It is
// encoded in the Go syntax, days included ("8760h0m0s" is "365d").
type Duration time.Duration

// Size is a size in bytes of the cache configuration: a number, or a
// number with a unit ("20GiB", "512MiB", "8MB", "1KiB"). It is encoded
// with the largest binary unit that divides it.
type Size int64

// TTL bounds how long an entry is fresh. Default applies to the objects
// that state no lifetime (Cache-Control, Expires) or when the policy does
// not respect theirs; Min and Max bound every lifetime, the objects' own
// included.
type TTL struct {
	Default *Duration `json:"default,omitempty" yaml:"default,omitempty"`
	Min     *Duration `json:"min,omitempty" yaml:"min,omitempty"`
	Max     *Duration `json:"max,omitempty" yaml:"max,omitempty"`
}

// QueryString selects the query parameters of the cache key. It is
// written as a string ("none", "all", "allowlist:v,w", "denylist:utm") or
// as an object ({"mode": "allowlist", "params": ["v", "w"]}).
type QueryString struct {
	Mode   string   `json:"mode" yaml:"mode"`
	Params []string `json:"params,omitempty" yaml:"params,omitempty"`
}

// KeyPolicy is how the cache key of a request is computed.
type KeyPolicy struct {
	QueryString *QueryString `json:"queryString,omitempty" yaml:"queryString,omitempty"`
	// Headers are request headers whose values enter the key
	// (Accept-Language).
	Headers []string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// Cookies are request cookies whose values enter the key.
	Cookies []string `json:"cookies,omitempty" yaml:"cookies,omitempty"`
	// Normalize sorts the query, lowercases the parameter names and the
	// header values, and drops the tracking parameters (utm_*, fbclid,
	// gclid).
	Normalize *bool `json:"normalize,omitempty" yaml:"normalize,omitempty"`
	// IncludeHost keys the entries by domain; false shares them between
	// the aliases of the distribution.
	IncludeHost *bool `json:"includeHost,omitempty" yaml:"includeHost,omitempty"`
}

// Bypass lists the conditions under which a request skips the cache.
// Requests other than GET and HEAD always do.
type Bypass struct {
	// Always skips the cache for every request (the disabled policy).
	Always *bool `json:"always,omitempty" yaml:"always,omitempty"`
	// Authorization skips the requests carrying credentials (an
	// Authorization header, a presigned query); false caches them per
	// credential.
	Authorization *bool `json:"authorization,omitempty" yaml:"authorization,omitempty"`
	// Cookies skips the requests carrying one of these cookies ("*": any
	// cookie).
	Cookies []string `json:"cookies,omitempty" yaml:"cookies,omitempty"`
	// QueryParams skips the requests with one of these query parameters
	// ("nocache").
	QueryParams []string `json:"queryParams,omitempty" yaml:"queryParams,omitempty"`
	// Headers skips the requests carrying one of these headers.
	Headers []string `json:"headers,omitempty" yaml:"headers,omitempty"`
}

// Compression stores compressed variants of the textual objects, served
// to the clients that accept them (Vary: Accept-Encoding).
type Compression struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Algorithms are gzip, br and zstd, in order of preference.
	Algorithms []string `json:"algorithms,omitempty" yaml:"algorithms,omitempty"`
	// ContentTypes are the compressible media types ("text/*",
	// "application/json").
	ContentTypes []string `json:"contentTypes,omitempty" yaml:"contentTypes,omitempty"`
	// MinSize is the smallest object compressed.
	MinSize *Size `json:"minSize,omitempty" yaml:"minSize,omitempty"`
}

// RangeCaching caches the objects larger than a block by blocks, so that
// the range requests (video) are served from the cache.
type RangeCaching struct {
	Enabled   *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	BlockSize *Size `json:"blockSize,omitempty" yaml:"blockSize,omitempty"`
}

// ContentTypes filters the cached objects by media type ("image/*").
// Deny wins over Allow; an empty Allow allows every type.
type ContentTypes struct {
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty" yaml:"deny,omitempty"`
}

// PolicySpec are the settings of a cache policy (docs/11 §11.2). Unset
// (nil) fields inherit from the more general level.
type PolicySpec struct {
	TTL *TTL `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	// RespectObjectCacheControl uses the Cache-Control and Expires stored
	// with the objects (bounded by TTL.Min and TTL.Max).
	RespectObjectCacheControl *bool `json:"respectObjectCacheControl,omitempty" yaml:"respectObjectCacheControl,omitempty"`
	// BrowserCacheControl is the Cache-Control sent to the clients; empty
	// keeps the one of the object (and of the behavior).
	BrowserCacheControl *string    `json:"browserCacheControl,omitempty" yaml:"browserCacheControl,omitempty"`
	CacheKey            *KeyPolicy `json:"cacheKey,omitempty" yaml:"cacheKey,omitempty"`
	// StaleWhileRevalidate serves an expired entry while it is refreshed
	// in the background, up to this long after its expiry.
	StaleWhileRevalidate *Duration `json:"staleWhileRevalidate,omitempty" yaml:"staleWhileRevalidate,omitempty"`
	// StaleIfError serves an expired entry when the origin fails (a set
	// without quorum), up to this long after its expiry.
	StaleIfError *Duration `json:"staleIfError,omitempty" yaml:"staleIfError,omitempty"`
	// NegativeCaching is the lifetime of the 404 and 403 answers (0: not
	// cached).
	NegativeCaching *Duration     `json:"negativeCaching,omitempty" yaml:"negativeCaching,omitempty"`
	Bypass          *Bypass       `json:"bypass,omitempty" yaml:"bypass,omitempty"`
	Compression     *Compression  `json:"compression,omitempty" yaml:"compression,omitempty"`
	MinSize         *Size         `json:"minSize,omitempty" yaml:"minSize,omitempty"`
	MaxObjectSize   *Size         `json:"maxObjectSize,omitempty" yaml:"maxObjectSize,omitempty"`
	RangeCaching    *RangeCaching `json:"rangeCaching,omitempty" yaml:"rangeCaching,omitempty"`
	ContentTypes    *ContentTypes `json:"contentTypes,omitempty" yaml:"contentTypes,omitempty"`
}

// Policy is a reusable cache policy referenced by name, built-in or
// defined by the administrator. A stored policy is complete: its unset
// fields take the values of the standard policy.
type Policy struct {
	// Name is lowercase letters, digits, '.', '_' and '-' (at most 64).
	Name    string `json:"name" yaml:"name"`
	Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
	// Builtin reports a predefined policy (read-only); computed.
	Builtin    bool `json:"builtin,omitempty" yaml:"-"`
	PolicySpec `json:",inline" yaml:",inline"`
	UpdatedAt  time.Time `json:"updatedAt,omitempty" yaml:"-"`
	// ETag identifies the stored version (If-Match); computed.
	ETag string `json:"etag,omitempty" yaml:"-"`
}

// Settings are the cache settings of a level: whether the cache is on,
// the policy it uses and the fields of the policy it overrides. Naming a
// policy replaces whatever the more general levels set; the inline fields
// override the policy.
type Settings struct {
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// Policy names a policy (built-in or defined).
	Policy     string `json:"policy,omitempty" yaml:"policy,omitempty"`
	PolicySpec `json:",inline" yaml:",inline"`
	// Quota bounds the bytes cached for the distribution (distribution
	// level) or for the domain (domain level).
	Quota *Size `json:"quota,omitempty" yaml:"quota,omitempty"`
	// PurgeWebhooks names the webhooks of the distribution called on the
	// invalidations of the domain (domain level).
	PurgeWebhooks []string `json:"purgeWebhooks,omitempty" yaml:"purgeWebhooks,omitempty"`
	// AutoInvalidate drops the entries of the objects written (default
	// true; global and distribution levels).
	AutoInvalidate *bool `json:"autoInvalidate,omitempty" yaml:"autoInvalidate,omitempty"`
}

// DomainSettings are the overrides of a domain of a distribution: an
// alias, exact ("www.example.com") or wildcard ("*.preview.example.com").
type DomainSettings struct {
	Host  string   `json:"host" yaml:"host"`
	Cache Settings `json:"cache" yaml:"cache"`
}

// BehaviorSettings are the overrides of the paths a pattern selects
// (matched against the path of the object in the distribution, "/"
// first); the first matching behavior wins.
type BehaviorSettings struct {
	PathPattern distribution.PathPattern `json:"pathPattern" yaml:"pathPattern"`
	Cache       Settings                 `json:"cache" yaml:"cache"`
}

// Webhook types (docs/11 §11.5 "Propagazione esterna").
const (
	WebhookCloudflare = "cloudflare"
	WebhookFastly     = "fastly"
	WebhookAkamai     = "akamai"
	WebhookNginx      = "nginx"
	WebhookVarnish    = "varnish"
	WebhookGeneric    = "generic"
)

// Webhook purges a CDN or a proxy in front of the cluster on the
// invalidations of a distribution. Secrets (Token, ClientSecret) may be
// given as "${secret:<name>}" (NEXO_SECRET_<NAME>); they are never
// returned.
type Webhook struct {
	// ID is lowercase letters, digits, '.', '_' and '-' (at most 64).
	ID   string `json:"id" yaml:"id"`
	Type string `json:"type" yaml:"type"`
	// Domains are the domains whose invalidations call the webhook; empty
	// for every domain of the distribution not bound to another webhook
	// (DomainSettings.Cache.PurgeWebhooks binds one too).
	Domains []string `json:"domains,omitempty" yaml:"domains,omitempty"`
	// URL is the endpoint: the API base of Cloudflare and Fastly
	// (default their public API), of Akamai (https://akab-...), the
	// address of nginx and Varnish, the URL template of a generic one.
	URL string `json:"url,omitempty" yaml:"url,omitempty"`
	// Zone is the Cloudflare zone id or the Fastly service id.
	Zone string `json:"zone,omitempty" yaml:"zone,omitempty"`
	// Token authenticates the calls (Cloudflare API token, Fastly key,
	// Akamai access token).
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
	// ClientToken and ClientSecret are the EdgeGrid credentials of Akamai;
	// Network is "production" (default) or "staging"; CPCode purges a
	// whole domain.
	ClientToken  string `json:"clientToken,omitempty" yaml:"clientToken,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty" yaml:"clientSecret,omitempty"`
	Network      string `json:"network,omitempty" yaml:"network,omitempty"`
	CPCode       string `json:"cpCode,omitempty" yaml:"cpCode,omitempty"`
	// Method is the HTTP method (nginx: PURGE, Varnish: BAN, generic:
	// POST).
	Method string `json:"method,omitempty" yaml:"method,omitempty"`
	// Headers are added to every call (templates for a generic webhook).
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// Body is the body template of a generic webhook (text/template over
	// PurgeTemplateData; default the JSON of the purge).
	Body string `json:"body,omitempty" yaml:"body,omitempty"`
	// Scheme is the scheme of the purged URLs (default https).
	Scheme string `json:"scheme,omitempty" yaml:"scheme,omitempty"`
}

// PurgeTemplateData is what the templates of a generic webhook see.
type PurgeTemplateData struct {
	ID           string
	Distribution string
	Hosts        []string
	Paths        []string
	URLs         []string
	Tags         []string
	All          bool
	Soft         bool
	// Automatic reports an invalidation caused by a write.
	Automatic bool
	// Test reports the call of TestWebhook.
	Test bool
}

// DistributionConfig is the cache configuration of a distribution
// (docs/11 §11.1): its defaults, the overrides of its domains and paths,
// its purge webhooks and the objects warmed on write.
type DistributionConfig struct {
	Distribution string             `json:"distribution" yaml:"distribution"`
	Cache        Settings           `json:"cache" yaml:"cache"`
	Domains      []DomainSettings   `json:"domains,omitempty" yaml:"domains,omitempty"`
	Behaviors    []BehaviorSettings `json:"behaviors,omitempty" yaml:"behaviors,omitempty"`
	Webhooks     []Webhook          `json:"webhooks,omitempty" yaml:"webhooks,omitempty"`
	// WarmOnWrite caches at once the objects written whose path matches
	// one of the patterns (docs/11 §11.6).
	WarmOnWrite []distribution.PathPattern `json:"warmOnWrite,omitempty" yaml:"warmOnWrite,omitempty"`
	UpdatedAt   time.Time                  `json:"updatedAt,omitempty" yaml:"-"`
	ETag        string                     `json:"etag,omitempty" yaml:"-"`
}

// DebugHeader unlocks the debug headers of the answers (X-Cache-Key,
// X-Cache-Policy, X-Cache-Node) for the requests carrying Secret in
// Header.
type DebugHeader struct {
	// Header defaults to DefaultDebugHeader.
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	// Secret is the value to present, or "${secret:<name>}"; never
	// returned.
	Secret string `json:"secret,omitempty" yaml:"secret,omitempty"`
}

// GlobalConfig is the cluster level of the cache configuration.
type GlobalConfig struct {
	Cache Settings `json:"cache" yaml:"cache"`
	// NodeHeader adds X-Cache-Node to every answer.
	NodeHeader bool         `json:"nodeHeader,omitempty" yaml:"nodeHeader,omitempty"`
	Debug      *DebugHeader `json:"debug,omitempty" yaml:"debug,omitempty"`
	UpdatedAt  time.Time    `json:"updatedAt,omitempty" yaml:"-"`
	ETag       string       `json:"etag,omitempty" yaml:"-"`
}

// EffectiveInput asks for the effective policy of a path of a domain of a
// distribution.
type EffectiveInput struct {
	Distribution string
	// Host is the domain (an alias); empty for the distribution level.
	Host string
	// Path is the path of the object in the distribution ("/" first).
	Path string
}

// EffectivePolicy is the policy that applies to a request and where each
// of its settings comes from (TCC-01, the inspector).
type EffectivePolicy struct {
	Enabled      bool  `json:"enabled"`
	EnabledLevel Level `json:"enabledLevel"`
	// PolicyName is the policy the settings start from and PolicyLevel the
	// level that named it.
	PolicyName  string `json:"policyName"`
	PolicyLevel Level  `json:"policyLevel"`
	// Policy is complete: every field is set.
	Policy PolicySpec `json:"policy"`
	// Fields maps every setting ("ttl.default", "staleIfError", ...) to
	// the level that set it last.
	Fields map[string]Level `json:"fields"`
	// Domain is the domain settings that matched, Behavior the index of
	// the behavior (-1: none).
	Domain   string `json:"domain,omitempty"`
	Behavior int    `json:"behavior"`
	// DistributionQuota and DomainQuota are the quotas that apply (0:
	// none).
	DistributionQuota Size `json:"distributionQuota,omitempty"`
	DomainQuota       Size `json:"domainQuota,omitempty"`
	// CacheControl is the Cache-Control sent to the clients (empty: the
	// one of the object).
	CacheControl string `json:"cacheControl,omitempty"`
	// AutoInvalidate reports whether the writes invalidate the entries.
	AutoInvalidate bool `json:"autoInvalidate"`
}

// PolicyPreview is the validation of a policy with the Cache-Control it
// sends to the clients (D-CCH-03).
type PolicyPreview struct {
	Policy Policy `json:"policy"`
	// CacheControl is the header sent to the clients; Inherited reports
	// that it is the one of the objects (the policy sets none).
	CacheControl string `json:"cacheControl"`
	Inherited    bool   `json:"inherited"`
	// Summary describes the policy in one line.
	Summary string `json:"summary"`
}

// Request is a request of a distribution, decoded by the transport before
// it is served.
type Request struct {
	// Distribution is the distribution of the host (a snapshot of the
	// routing table) and Alias the alias that matched.
	Distribution *distribution.Distribution
	Alias        string
	// Host is the normalized host (lowercase, no port).
	Host   string
	Method string
	// Path is the decoded path ("/assets/app.js").
	Path     string
	RawQuery string
	Header   http.Header
	Secure   bool
	ClientIP string
	// Credentials reports a request carrying credentials: an
	// Authorization header, a presigned query, a verified identity.
	// Credential is their identity when known (the access key), keying
	// the entries of the policies that cache such requests.
	Credentials bool
	Credential  string
}

// Exchange is the cache side of a request: the transport asks it for the
// headers of the answer and reports the answer.
type Exchange interface {
	// Decorate adds the cache headers to the header of the answer about
	// to be sent with status (X-Cache, Age, Vary, the Cache-Control of the
	// policy, the debug headers). It is a pure function of what the reads
	// of the request recorded (no I/O): the documented exception to the
	// context rule.
	Decorate(status int, header http.Header)
	// Finish records the answer (metrics, statistics).
	Finish(ctx context.Context, status int, bytes int64)
}

// Runtime serves the requests of the distributions.
type Runtime interface {
	// Begin decides how the cache serves a request and returns the
	// context of its reads (the object layer decorated by NewObjects
	// reads it) and its exchange.
	Begin(ctx context.Context, req Request) (context.Context, Exchange)
}

// Reader serves the object reads of the requests begun with
// Runtime.Begin, with the semantics of object.Service (conditions, range,
// errors): the object layer decorated by NewObjects asks it first.
type Reader interface {
	// GetObject reads an object for the request of ctx. handled is false
	// when the read does not go through the cache (no request begun,
	// bypass, version, part, SSE-C key): the caller reads the origin.
	GetObject(ctx context.Context, bucket, key string, opts object.GetObjectOptions) (rd *object.Reader, handled bool, err error)
	// GetObjectInfo describes an object for the request of ctx, like
	// GetObject.
	GetObjectInfo(ctx context.Context, bucket, key string, opts object.GetObjectOptions) (info domain.ObjectInfo, handled bool, err error)
}

// ObjectChange is a change of an object (or of a whole bucket) made by
// the object layer.
type ObjectChange struct {
	Bucket string
	Key    string
	// VersionID is the version written or removed.
	VersionID string
	// Event is the event name ("s3:ObjectCreated:Put").
	Event string
	// BucketWide reports a bucket created or removed.
	BucketWide bool
	// Tags are the cache tags of the new version (x-amz-meta-cache-tag).
	Tags []string
}

// Invalidator receives the changes of the objects (NewEvents).
type Invalidator interface {
	// ObjectChanged drops the entries of the object on every node, before
	// it returns (a node that cannot be reached is retried in the
	// background and detects the gap), and queues the purge webhooks of
	// the distributions serving it. The error reports the peers missed;
	// the local invalidation always happens.
	ObjectChanged(ctx context.Context, change ObjectChange) error
}

// InvalidationRequest is a manual invalidation (docs/11 §11.5).
type InvalidationRequest struct {
	// Distribution scopes the invalidation; empty with All for every
	// distribution (global purge).
	Distribution string `json:"distribution,omitempty"`
	// Domain limits it to a domain of the distribution (an alias, a
	// wildcard alias matches its hosts); empty for every domain.
	Domain string `json:"domain,omitempty"`
	// Paths are exact paths ("/index.html"; "/docs/" also drops its index
	// document).
	Paths []string `json:"paths,omitempty"`
	// Patterns are prefix, glob or regex patterns of paths.
	Patterns []distribution.PathPattern `json:"patterns,omitempty"`
	// Tags are cache tags (surrogate keys).
	Tags []string `json:"tags,omitempty"`
	// All drops every entry of the scope.
	All bool `json:"all,omitempty"`
	// Soft marks the entries expired instead of dropping them: they are
	// served stale while revalidated.
	Soft bool `json:"soft,omitempty"`
	// Actor is who asked (for the history).
	Actor string `json:"actor,omitempty"`
}

// Invalidation states.
const (
	InvalidationInProgress = "in-progress"
	InvalidationCompleted  = "completed"
	// NodePending is a node not reached yet (retried); NodeCompleted one
	// that applied the invalidation.
	NodePending   = "pending"
	NodeCompleted = "completed"
)

// NodeStatus is the state of an invalidation or a warm-up on a node.
type NodeStatus struct {
	Node  string    `json:"node"`
	State string    `json:"state"`
	At    time.Time `json:"at,omitempty"`
	// Removed is the number of entries dropped or marked (invalidations).
	Removed int    `json:"removed,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Invalidation is a manual invalidation with its state on every node.
type Invalidation struct {
	ID        string              `json:"id"`
	Request   InvalidationRequest `json:"request"`
	CreatedAt time.Time           `json:"createdAt"`
	// State is InvalidationCompleted once every node applied it.
	State string       `json:"state"`
	Nodes []NodeStatus `json:"nodes"`
}

// Warm-up states.
const (
	WarmRunning   = "running"
	WarmCompleted = "completed"
	WarmFailed    = "failed"
)

// WarmRequest asks to cache objects of a domain of a distribution before
// they are requested (docs/11 §11.6).
type WarmRequest struct {
	Distribution string `json:"distribution"`
	// Domain is the domain the entries are keyed by (default the first
	// exact alias).
	Domain string `json:"domain,omitempty"`
	// Paths are paths of the distribution, URLs absolute URLs of its
	// domains.
	Paths []string `json:"paths,omitempty"`
	URLs  []string `json:"urls,omitempty"`
	// Prefix warms every object below a path prefix ("/assets/"),
	// Pattern every object a glob or regex pattern matches (below its
	// literal prefix).
	Prefix  string                    `json:"prefix,omitempty"`
	Pattern *distribution.PathPattern `json:"pattern,omitempty"`
	// Concurrency bounds the parallel reads (default 4) and Bandwidth
	// the bytes read per second (0: the default, 64 MiB/s).
	Concurrency int    `json:"concurrency,omitempty"`
	Bandwidth   Size   `json:"bandwidth,omitempty"`
	Actor       string `json:"actor,omitempty"`
}

// WarmNode is the progress of a warm-up on a node.
type WarmNode struct {
	NodeStatus
	Total  int   `json:"total"`
	Done   int   `json:"done"`
	Failed int   `json:"failed"`
	Bytes  int64 `json:"bytes"`
}

// WarmJob is a warm-up with its progress on every node.
type WarmJob struct {
	ID        string      `json:"id"`
	Request   WarmRequest `json:"request"`
	CreatedAt time.Time   `json:"createdAt"`
	State     string      `json:"state"`
	Nodes     []WarmNode  `json:"nodes"`
}

// WebhookTestResult is the outcome of a test call of a webhook.
type WebhookTestResult struct {
	OK       bool          `json:"ok"`
	Status   int           `json:"status,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

// InspectInput asks how the cache serves a URL.
type InspectInput struct {
	URL string
	// Header are the request headers (Accept-Encoding, the headers and
	// cookies of the key).
	Header http.Header
}

// Presence is the state of a cache key on a node.
type Presence struct {
	Key     string `json:"key"`
	Present bool   `json:"present"`
	// Tier is "memory" or "disk"; Kind "object", "head" (block mode),
	// "info", "negative" or "pass".
	Tier string `json:"tier,omitempty"`
	Kind string `json:"kind,omitempty"`
	// Age is how long ago the entry was stored or revalidated, ExpiresIn
	// how long it stays fresh (negative: stale).
	Age       Duration `json:"age,omitempty"`
	ExpiresIn Duration `json:"expiresIn,omitempty"`
	Stale     bool     `json:"stale,omitempty"`
	Size      Size     `json:"size,omitempty"`
	ETag      string   `json:"etag,omitempty"`
	// Encodings are the compressed variants cached; Blocks the number of
	// blocks cached (block mode).
	Encodings []string `json:"encodings,omitempty"`
	Blocks    int      `json:"blocks,omitempty"`
}

// NodePresence is the presence of a key on a node.
type NodePresence struct {
	Node   string `json:"node"`
	Online bool   `json:"online"`
	// Owner reports the owner of the key (shared mode).
	Owner bool     `json:"owner,omitempty"`
	Entry Presence `json:"entry"`
	Error string   `json:"error,omitempty"`
}

// InspectResult is how the cache serves a URL (docs/11 §11.7).
type InspectResult struct {
	URL          string `json:"url"`
	Distribution string `json:"distribution,omitempty"`
	Host         string `json:"host"`
	Path         string `json:"path"`
	Bucket       string `json:"bucket,omitempty"`
	Key          string `json:"key,omitempty"`
	// CacheKey is the key of the entry (without the variant).
	CacheKey string `json:"cacheKey,omitempty"`
	// Bypass reports a request the cache does not serve, Reason why.
	Bypass bool            `json:"bypass"`
	Reason string          `json:"reason,omitempty"`
	Policy EffectivePolicy `json:"policy"`
	Mode   Mode            `json:"mode"`
	Nodes  []NodePresence  `json:"nodes,omitempty"`
}

// StatsQuery filters the statistics.
type StatsQuery struct {
	Distribution string `json:"distribution,omitempty"`
	Host         string `json:"host,omitempty"`
	// Top bounds the lists of top objects (default 10).
	Top int `json:"top,omitempty"`
}

// Counters are the counters of the answers.
type Counters struct {
	Requests map[Result]int64 `json:"requests"`
	// BytesCache and BytesOrigin are the bytes served from the cache and
	// from the origin; HitRatio the share of the requests served from the
	// cache (HIT, STALE, REVALIDATED) among the cacheable ones.
	BytesCache  int64   `json:"bytesCache"`
	BytesOrigin int64   `json:"bytesOrigin"`
	HitRatio    float64 `json:"hitRatio"`
}

// HostStats are the statistics of a domain of a distribution.
type HostStats struct {
	Distribution string `json:"distribution"`
	Host         string `json:"host"`
	Counters
	// Entries and the sizes are those cached for the host.
	Entries    int  `json:"entries"`
	MemorySize Size `json:"memorySize"`
	DiskSize   Size `json:"diskSize"`
}

// ObjectStat counts the requests of a path.
type ObjectStat struct {
	Distribution string `json:"distribution"`
	Host         string `json:"host"`
	Path         string `json:"path"`
	Requests     int64  `json:"requests"`
	Misses       int64  `json:"misses"`
}

// NodeStats are the statistics of a node.
type NodeStats struct {
	Node   string `json:"node"`
	Online bool   `json:"online"`
	Mode   Mode   `json:"mode"`
	Counters
	Entries       int   `json:"entries"`
	MemorySize    Size  `json:"memorySize"`
	MemoryLimit   Size  `json:"memoryLimit"`
	DiskSize      Size  `json:"diskSize"`
	DiskLimit     Size  `json:"diskLimit"`
	Evictions     int64 `json:"evictions"`
	Invalidations int64 `json:"invalidations"`
	Flushes       int64 `json:"flushes"`
	// Synced reports a node that receives the invalidations of every peer:
	// otherwise its entries are revalidated with the origin before they
	// are served.
	Synced bool `json:"synced"`
	// OriginReads counts the reads of the origin made by the cache
	// (fills and revalidations); WebhookQueue the purges waiting.
	OriginReads  int64        `json:"originReads"`
	WebhookQueue int          `json:"webhookQueue"`
	Hosts        []HostStats  `json:"hosts,omitempty"`
	TopObjects   []ObjectStat `json:"topObjects,omitempty"`
	TopMisses    []ObjectStat `json:"topMisses,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// StatsReport are the statistics of the cluster (docs/11 §11.8).
type StatsReport struct {
	Counters
	Entries    int          `json:"entries"`
	MemorySize Size         `json:"memorySize"`
	DiskSize   Size         `json:"diskSize"`
	Hosts      []HostStats  `json:"hosts,omitempty"`
	TopObjects []ObjectStat `json:"topObjects,omitempty"`
	TopMisses  []ObjectStat `json:"topMisses,omitempty"`
	Nodes      []NodeStats  `json:"nodes"`
}

// Manager manages the cache configuration (Admin API, CLI, dashboard).
type Manager interface {
	// ListPolicies returns the built-in and defined policies, by name.
	ListPolicies(ctx context.Context) ([]Policy, error)
	// GetPolicy returns a policy (ErrPolicyNotFound).
	GetPolicy(ctx context.Context, name string) (Policy, error)
	// CreatePolicy stores a new policy (domain.ErrConflict when the name
	// is taken or built-in).
	CreatePolicy(ctx context.Context, p Policy) (Policy, error)
	// UpdatePolicy replaces a defined policy; a non-empty ifMatch must
	// equal its ETag (domain.ErrPreconditionFailed).
	UpdatePolicy(ctx context.Context, p Policy, ifMatch string) (Policy, error)
	// DeletePolicy removes a defined policy (domain.ErrConflict while a
	// level references it).
	DeletePolicy(ctx context.Context, name string) error
	// PreviewPolicy validates a policy and computes the Cache-Control it
	// sends to the clients, without storing it.
	PreviewPolicy(ctx context.Context, p Policy) (PolicyPreview, error)

	// GetGlobal and UpdateGlobal read and replace the cluster level.
	GetGlobal(ctx context.Context) (GlobalConfig, error)
	UpdateGlobal(ctx context.Context, g GlobalConfig, ifMatch string) (GlobalConfig, error)

	// GetDistribution returns the cache configuration of a distribution
	// (an empty one when it has none; distribution.ErrDistributionNotFound
	// for an unknown distribution).
	GetDistribution(ctx context.Context, id string) (DistributionConfig, error)
	// UpdateDistribution replaces it (the webhooks given without their
	// secrets keep the stored ones).
	UpdateDistribution(ctx context.Context, cfg DistributionConfig, ifMatch string) (DistributionConfig, error)
	// DeleteDistribution removes it and drops the entries of the
	// distribution.
	DeleteDistribution(ctx context.Context, id string) error
	// Effective returns the effective policy of a path of a domain.
	Effective(ctx context.Context, in EffectiveInput) (EffectivePolicy, error)

	// Webhooks of a distribution.
	ListWebhooks(ctx context.Context, distribution string) ([]Webhook, error)
	AddWebhook(ctx context.Context, distribution string, w Webhook) (Webhook, error)
	DeleteWebhook(ctx context.Context, distribution, id string) error
	// TestWebhook calls a webhook once, outside of the queue.
	TestWebhook(ctx context.Context, distribution, id string) (WebhookTestResult, error)
}

// Operations are the operations on the cached content.
type Operations interface {
	// Invalidate applies a manual invalidation on every node and records
	// it; the nodes not reached are retried in the background (State
	// stays in-progress until they apply it).
	Invalidate(ctx context.Context, req InvalidationRequest) (Invalidation, error)
	// GetInvalidation returns an invalidation of the history
	// (ErrInvalidationNotFound).
	GetInvalidation(ctx context.Context, distribution, id string) (Invalidation, error)
	// ListInvalidations returns the history of a distribution ("" for the
	// global purges), the newest first.
	ListInvalidations(ctx context.Context, distribution string) ([]Invalidation, error)

	// Warm starts a warm-up (on every node in the local mode) and returns
	// at once.
	Warm(ctx context.Context, req WarmRequest) (WarmJob, error)
	// GetWarmJob returns a warm-up with its progress (ErrWarmJobNotFound).
	GetWarmJob(ctx context.Context, distribution, id string) (WarmJob, error)

	// Inspect reports how the cache serves a URL on every node.
	Inspect(ctx context.Context, in InspectInput) (InspectResult, error)
	// Stats returns the statistics of the cluster.
	Stats(ctx context.Context, q StatsQuery) (StatsReport, error)
}

// SyncStatus is the invalidation sequence of a node: the peers compare it
// with what they received to detect the invalidations they missed.
type SyncStatus struct {
	Node string `json:"node"`
	// Epoch identifies the run of the cache service (a restart changes
	// it) and Seq the last invalidation it sent.
	Epoch string `json:"epoch"`
	Seq   uint64 `json:"seq"`
}

// InvalidationMessage is an invalidation sent to the peers.
type InvalidationMessage struct {
	Seq uint64 `json:"seq"`
	// ID is the id of a manual invalidation (its history record).
	ID string `json:"id,omitempty"`
	// Change is an automatic invalidation, Request a manual one.
	Change  *ObjectChange        `json:"change,omitempty"`
	Request *InvalidationRequest `json:"request,omitempty"`
	// FlushAll drops every entry: the queue of the sender overflowed.
	FlushAll bool `json:"flushAll,omitempty"`
}

// InvalidationBatch is a sequence of invalidations of a node, in order.
type InvalidationBatch struct {
	Origin   string                `json:"origin"`
	Epoch    string                `json:"epoch"`
	Messages []InvalidationMessage `json:"messages"`
}

// InvalidationAck is the answer of a node to a batch.
type InvalidationAck struct {
	Node string `json:"node"`
	// LastSeq is the last sequence applied; Removed counts the entries
	// dropped by the batch, by invalidation id.
	LastSeq uint64         `json:"lastSeq"`
	Removed map[string]int `json:"removed,omitempty"`
	// Flushed reports that the node dropped its cache (gap or FlushAll).
	Flushed bool `json:"flushed,omitempty"`
}

// PieceRequest asks the owner of a key for a piece of an object (shared
// mode). Spec is opaque to the transport.
type PieceRequest struct {
	Spec []byte `json:"spec"`
}

// PieceReply is a piece of an object: its description (opaque), its
// content and how the owner served it.
type PieceReply struct {
	Node   string `json:"node"`
	Result Result `json:"result"`
	Meta   []byte `json:"meta"`
	Body   []byte `json:"body,omitempty"`
}

// PresenceRequest asks a node which keys it holds.
type PresenceRequest struct {
	Keys []string `json:"keys"`
}

// PresenceReply is the presence of the keys on a node.
type PresenceReply struct {
	Node    string     `json:"node"`
	Entries []Presence `json:"entries"`
}

// Node is the internode side of the cache of a node, served to its peers
// by the internode RPC (rpc.CacheRoutes).
type Node interface {
	// ApplyInvalidations applies a batch of invalidations of a peer, in
	// order, and drops the whole cache when the sequence has a gap.
	ApplyInvalidations(ctx context.Context, batch InvalidationBatch) (InvalidationAck, error)
	// SyncStatus returns the invalidation sequence of this node.
	SyncStatus(ctx context.Context) (SyncStatus, error)
	// FetchPiece serves a piece this node owns, from its cache or the
	// origin (shared mode).
	FetchPiece(ctx context.Context, req PieceRequest) (PieceReply, error)
	// Presence reports the keys this node holds.
	Presence(ctx context.Context, req PresenceRequest) (PresenceReply, error)
	// NodeStats returns the statistics of this node.
	NodeStats(ctx context.Context, q StatsQuery) (NodeStats, error)
	// StartWarm runs a warm-up on this node (local mode).
	StartWarm(ctx context.Context, job WarmJob) error
}

// Peer is the cache of another node (the internode RPC client,
// rpc.NewCacheClient).
type Peer interface {
	Node
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// IsOnline reports whether the peer is reachable. It is a pure
	// accessor.
	IsOnline() bool
}

// Service is the cache of a node: it runs the background work (index of
// the disk tier, retries, synchronization, webhooks, warm-ups:
// lifecycle.Runner).
type Service interface {
	lifecycle.Runner
	Runtime
	Reader
	Invalidator
	Manager
	Operations
	Node
}

// Distributions is what the cache needs of the distributions.
type Distributions interface {
	distribution.Resolver
	// Get returns a distribution (distribution.ErrDistributionNotFound).
	Get(ctx context.Context, id string) (distribution.Distribution, error)
	// List returns every distribution.
	List(ctx context.Context) ([]distribution.Distribution, error)
	// Test reports how a URL is served (the inspector).
	Test(ctx context.Context, in distribution.TestInput) (distribution.TestResult, error)
}

// SecretSource resolves the secrets referenced as "${secret:<name>}".
type SecretSource interface {
	LookupSecret(ctx context.Context, name string) (string, bool)
}

// HTTPDoer sends the calls of the purge webhooks. The request carries its
// context (http.Request.Context), which is why Do has no context
// parameter.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Deps are the dependencies of the cache service.
type Deps struct {
	// Origin reads the objects (required): the object layer, not
	// decorated by NewObjects.
	Origin object.Service
	// Store holds the configuration and the history (required): the
	// system store, shared by the nodes.
	Store sysstore.Store
	// Locks serializes the writes of the configuration (required).
	Locks lock.Locker
	// Notifier broadcasts the configuration changes to the peers and
	// delivers theirs (required).
	Notifier cluster.Notifier
	// Distributions resolves the hosts and the distributions (required).
	Distributions Distributions
	// Matcher compiles the path patterns (default
	// distribution.NewMatcher()).
	Matcher distribution.Matcher
	// Peers are the caches of the other nodes (none on a single node).
	Peers []Peer
	// NodeID identifies this node ("local" on a single node).
	NodeID string
	// Secrets resolves the ${secret:<name>} of the webhooks and of the
	// debug header (optional).
	Secrets SecretSource
	// HTTP sends the webhook calls (default an http.Client that follows
	// no redirect).
	HTTP HTTPDoer
	// Registry receives the metrics (optional).
	Registry metrics.Registry
	Clock    clock.Clock
	Logger   *slog.Logger
}

// ObjectsDeps are the dependencies of the object layer decorated by the
// cache.
type ObjectsDeps struct {
	// Objects is the object layer (required).
	Objects object.Service
	// Cache serves the reads of the distribution requests (required).
	Cache Reader
}

// EventsDeps are the dependencies of the event hook.
type EventsDeps struct {
	// Events is the notification service the object layer publishes to
	// (required).
	Events event.Service
	// Invalidator receives the changes of the objects (required): the
	// cache service, or a holder installed once it is composed.
	Invalidator Invalidator
	Logger      *slog.Logger
}
