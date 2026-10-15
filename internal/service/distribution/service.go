// Package distribution implements the distributions of docs/10 §10.2: a
// distribution exposes a bucket (or a prefix of it) on one or more custom
// domains, in the "website" mode (static sites, downloads: index document,
// SPA fallback, error pages, redirects, behaviors per path, signed URLs
// and cookies) or in the "s3" mode (the domain is an S3 endpoint aliasing
// the bucket).
//
// The distributions and the key groups of the signed URLs are documents of
// the system store (distributions/<id>.json, keygroups/<id>.json), shared
// by every node: a change is written there, applied to the routing table
// of the node at once and broadcast to the peers (cluster.Notifier), which
// reload it, so that a distribution created on one node answers on every
// node without a restart.
//
// # Host resolution
//
// Resolver.Resolve maps the Host of a request (docs/10 §10.2 "Risoluzione
// dell'host") in this order: an exact alias of a distribution; the most
// specific wildcard alias ("*.preview.example.com" matches any host below
// preview.example.com, a longer suffix wins over a shorter one);
// "<bucket>.<NEXO_DOMAIN>" (virtual-host S3); the main S3/console domains
// (and the IP literals); otherwise the path-style S3 API, or a 421
// Misdirected Request when the deployment asks for it
// (WithUnknownHostMisdirected). The table is a map of the exact aliases and
// a trie of the wildcard ones, swapped atomically on every change.
//
// # Viewer access
//
// The objects are read through the object layer on behalf of the
// distribution: the policy engine is asked with the identity of the viewer
// (anonymous for the public and signed behaviors, the SigV4 identity for
// the authenticated ones) and the request conditions of
// policy.RequestConditions, nexo:Distribution (policy.KeyDistribution) set
// to the id of the distribution and nexo:Host to the host of the request,
// so that the access rules can name the principal "distribution:<id>"
// (PrincipalPrefix). A public or signed behavior grants the read itself:
// only an explicit deny (access rules, bucket policy) or the block public
// access of the bucket (public only) refuse it. An authenticated behavior
// needs an explicit allow.
//
// # Signed URLs and cookies
//
// The signed behaviors accept CloudFront-style URLs (?Expires=&Signature=
// &Key-Pair-Id= for the canned policy, ?Policy=&Signature=&Key-Pair-Id=
// for a custom policy with a wildcard Resource, DateLessThan,
// DateGreaterThan and IpAddress) and the equivalent cookies
// (CloudFront-Policy or CloudFront-Expires, CloudFront-Signature,
// CloudFront-Key-Pair-Id). The policy is signed with Ed25519 or with RSA
// PKCS #1 v1.5 over SHA-256 (CloudFront uses SHA-1: Nexo does not accept
// it), with a key of the key group of the behavior; the signature and the
// policy use the CloudFront URL-safe base64 alphabet ('-', '_', '~'). A key
// group holds several keys, so that a new key is added before the old one
// is removed (rotation without downtime).
package distribution

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Mode is how a distribution serves its origin.
type Mode string

// Distribution modes.
const (
	// ModeWebsite serves GET/HEAD/OPTIONS (POST/PUT on signed behaviors
	// that allow them) with HTML or plain text answers: index document,
	// error pages, SPA fallback, redirects; never S3 XML.
	ModeWebsite Mode = "website"
	// ModeS3 makes the domain an S3 endpoint of the origin bucket: the
	// path is the object key, SigV4 is verified with the public host.
	ModeS3 Mode = "s3"
)

// Access is who may read the objects of a behavior.
type Access string

// Access levels of a behavior.
const (
	// AccessPublic lets anyone read, subject to the explicit denies of the
	// access rules and bucket policy and to the block public access.
	AccessPublic Access = "public"
	// AccessSigned needs a signed URL or signed cookies of the key group
	// of the behavior.
	AccessSigned Access = "signed"
	// AccessAuthenticated needs S3 credentials (SigV4) allowed by the
	// policies.
	AccessAuthenticated Access = "authenticated"
	// AccessDeny always refuses (403, or the error page of 403).
	AccessDeny Access = "deny"
)

// PatternType is the syntax of a path pattern: the one of the access
// rules (match.Type), so that behaviors, cache settings and access rules
// share a single matcher (internal/service/match).
type PatternType = match.Type

// Path pattern syntaxes (docs/10 §10.1 "Tipi di pattern").
const (
	// PatternPrefix is a literal prefix.
	PatternPrefix = match.TypePrefix
	// PatternGlob supports "*" (one segment), "**" (any number of
	// segments), "?", "[abc]" and "{a,b}".
	PatternGlob = match.TypeGlob
	// PatternRegex is an RE2 regular expression (linear time).
	PatternRegex = match.TypeRegex
)

// MaxPatternLength bounds a path pattern or a redirect expression, in
// characters (docs/10 §10.1).
const MaxPatternLength = match.MaxPatternLength

// PrincipalPrefix prefixes the id of a distribution in the principal of
// the access rules ("distribution:sito-web"): every policy evaluation of a
// distribution request carries the id in the nexo:Distribution condition
// (policy.KeyDistribution).
const PrincipalPrefix = "distribution:"

// Event kinds of the cluster invalidations of the distributions and of
// the key groups.
const (
	EventDistribution cluster.EventKind = "distribution"
	EventKeyGroup     cluster.EventKind = "keygroup"
)

// PathPattern selects the request paths of a behavior. The path includes
// its leading "/", is decoded and is matched in its normalized form
// (match.Service.Normalize): it is the pattern of the access rules.
type PathPattern = match.Pattern

// Origin is the bucket (and prefix) a distribution serves.
type Origin struct {
	Bucket string `json:"bucket" yaml:"bucket"`
	// PathPrefix restricts the distribution to the keys under this prefix
	// ("/prod" serves prod/...): the requests cannot leave it. Website
	// mode only. Stored as "/prod" (or empty for the whole bucket).
	PathPrefix string `json:"pathPrefix,omitempty" yaml:"pathPrefix,omitempty"`
	// VersionPinning is reserved (docs/10): stored, not applied yet.
	VersionPinning bool `json:"versionPinning,omitempty" yaml:"versionPinning,omitempty"`
}

// Cache are the cache settings of a behavior. Only CacheControl is
// applied by the distributions (the Cache-Control sent to the browsers,
// instead of the one of the object); the cache of docs/11 extends it.
type Cache struct {
	CacheControl string `json:"cacheControl,omitempty" yaml:"cacheControl,omitempty"`
}

// Signing names the key group whose keys sign the URLs and cookies of a
// signed behavior.
type Signing struct {
	KeyGroup string `json:"keyGroup" yaml:"keyGroup"`
}

// CORS is the cross-origin policy of a distribution or a behavior.
type CORS struct {
	// AllowedOrigins are origins with at most one "*" wildcard each, or
	// "*" alone for every origin.
	AllowedOrigins []string `json:"allowedOrigins" yaml:"allowedOrigins"`
	// AllowedMethods default to GET and HEAD.
	AllowedMethods []string `json:"allowedMethods,omitempty" yaml:"allowedMethods,omitempty"`
	AllowedHeaders []string `json:"allowedHeaders,omitempty" yaml:"allowedHeaders,omitempty"`
	ExposeHeaders  []string `json:"exposeHeaders,omitempty" yaml:"exposeHeaders,omitempty"`
	// MaxAge is the preflight cache duration in seconds (0: not sent).
	MaxAge           int  `json:"maxAge,omitempty" yaml:"maxAge,omitempty"`
	AllowCredentials bool `json:"allowCredentials,omitempty" yaml:"allowCredentials,omitempty"`
}

// Behavior is how the requests of a path are served. The behaviors of a
// distribution inherit every unset field from its default behavior.
type Behavior struct {
	// PathPattern selects the paths of the behavior; nil for the default
	// behavior.
	PathPattern *PathPattern `json:"pathPattern,omitempty" yaml:"pathPattern,omitempty"`
	Access      Access       `json:"access,omitempty" yaml:"access,omitempty"`
	// AllowedMethods default to GET, HEAD and OPTIONS. POST and PUT are
	// accepted only by the signed behaviors of a website distribution.
	AllowedMethods []string `json:"allowedMethods,omitempty" yaml:"allowedMethods,omitempty"`
	// IndexDocument is appended to the paths ending with "/".
	IndexDocument string `json:"indexDocument,omitempty" yaml:"indexDocument,omitempty"`
	// SPAFallback is the path served (200) instead of a missing object.
	SPAFallback string   `json:"spaFallback,omitempty" yaml:"spaFallback,omitempty"`
	Cache       *Cache   `json:"cache,omitempty" yaml:"cache,omitempty"`
	Signing     *Signing `json:"signing,omitempty" yaml:"signing,omitempty"`
	// ResponseHeaders are added to the answers (over the distribution
	// headers).
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty" yaml:"responseHeaders,omitempty"`
	// CORS replaces the CORS policy of the distribution on these paths.
	CORS *CORS `json:"cors,omitempty" yaml:"cors,omitempty"`
}

// ErrorPage replaces the answer of an error status with an object of the
// distribution.
type ErrorPage struct {
	// Status is the error answered (403, 404, 5xx).
	Status int `json:"status" yaml:"status"`
	// Path is the page, a path of the distribution ("/404.html").
	Path string `json:"path" yaml:"path"`
	// ResponseStatus is the status sent with the page (default Status):
	// 404 for a 403 hides the existence of the objects.
	ResponseStatus int `json:"responseStatus,omitempty" yaml:"responseStatus,omitempty"`
}

// Redirect is a redirection rule; the first rule that applies wins.
type Redirect struct {
	// From is an RE2 expression matched against the path; To is the
	// target, with $1, ${name} expanded from the groups of From.
	From string `json:"from,omitempty" yaml:"from,omitempty"`
	To   string `json:"to,omitempty" yaml:"to,omitempty"`
	// HostFrom redirects the requests for this host to ToHost, keeping
	// the path and the query.
	HostFrom string `json:"hostFrom,omitempty" yaml:"hostFrom,omitempty"`
	ToHost   string `json:"toHost,omitempty" yaml:"toHost,omitempty"`
	// HTTPSOnly redirects the plain HTTP requests to HTTPS.
	HTTPSOnly bool `json:"httpsOnly,omitempty" yaml:"httpsOnly,omitempty"`
	// Status is 301 (default), 302, 303, 307 or 308.
	Status int `json:"status,omitempty" yaml:"status,omitempty"`
}

// Headers are the headers of the answers of a distribution.
type Headers struct {
	Response map[string]string `json:"response,omitempty" yaml:"response,omitempty"`
	CORS     *CORS             `json:"cors,omitempty" yaml:"cors,omitempty"`
}

// IPRestriction filters the clients by address (the real client behind the
// trusted proxies): a client in Deny is refused, and when Allow is not
// empty a client outside it is refused too. Entries are IPs or CIDRs.
type IPRestriction struct {
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty" yaml:"deny,omitempty"`
}

// OriginSecret accepts only the requests carrying one of the secrets in the
// header Name: the traffic of the proxies in front of the cluster, not the
// direct one. Several values are active at once during a rotation. The
// values are stored hashed ("sha256:<hex>"): a value given in clear is
// hashed when the distribution is saved; a reference "${secret:<name>}"
// is kept and resolved by the website engine (SecretSource).
type OriginSecret struct {
	Name string `json:"name" yaml:"name"`
	// Value is a single secret, merged into Values when saved.
	Value  string   `json:"value,omitempty" yaml:"value,omitempty"`
	Values []string `json:"values,omitempty" yaml:"values,omitempty"`
}

// RateLimit is stored and validated, not enforced yet (docs/10 P2).
type RateLimit struct {
	RequestsPerSecond int    `json:"requestsPerSecond,omitempty" yaml:"requestsPerSecond,omitempty"`
	Burst             int    `json:"burst,omitempty" yaml:"burst,omitempty"`
	Per               string `json:"per,omitempty" yaml:"per,omitempty"`
}

// Restrictions are the admission rules of every request of a
// distribution, in both modes.
type Restrictions struct {
	SourceIP           *IPRestriction `json:"sourceIp,omitempty" yaml:"sourceIp,omitempty"`
	OriginSecretHeader *OriginSecret  `json:"originSecretHeader,omitempty" yaml:"originSecretHeader,omitempty"`
	RateLimit          *RateLimit     `json:"rateLimit,omitempty" yaml:"rateLimit,omitempty"`
}

// TLS certificate sources of a distribution.
const (
	// CertificateAuto obtains and renews the certificate of the exact
	// aliases with ACME.
	CertificateAuto = "auto"
	// CertificateUploadPrefix prefixes the id of an uploaded certificate
	// ("upload:<id>").
	CertificateUploadPrefix = "upload:"
	// CertificateNone leaves TLS to a proxy in front of the cluster.
	CertificateNone = "none"
)

// TLS is the certificate of the aliases of a distribution.
type TLS struct {
	Certificate string `json:"certificate,omitempty" yaml:"certificate,omitempty"`
}

// Access log formats.
const (
	LogFormatJSON     = "json"
	LogFormatCombined = "combined"
)

// Logging configures the access log of a distribution.
type Logging struct {
	AccessLog bool `json:"accessLog,omitempty" yaml:"accessLog,omitempty"`
	// Format is LogFormatJSON (default) or LogFormatCombined.
	Format string `json:"format,omitempty" yaml:"format,omitempty"`
	// Target is reserved ("webhook:<id>"): the entries go to the server
	// log.
	Target string `json:"target,omitempty" yaml:"target,omitempty"`
}

// Distribution exposes a bucket on custom domains (docs/10 §10.2).
type Distribution struct {
	// ID is lowercase letters, digits, '.', '_' and '-' (at most 64).
	ID      string `json:"id" yaml:"id"`
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
	// Aliases are the host names: exact ("www.example.com") or wildcard
	// ("*.preview.example.com"), unique across the distributions.
	Aliases         []string     `json:"aliases" yaml:"aliases"`
	Origin          Origin       `json:"origin" yaml:"origin"`
	Mode            Mode         `json:"mode" yaml:"mode"`
	DefaultBehavior Behavior     `json:"defaultBehavior" yaml:"defaultBehavior"`
	Behaviors       []Behavior   `json:"behaviors,omitempty" yaml:"behaviors,omitempty"`
	ErrorPages      []ErrorPage  `json:"errorPages,omitempty" yaml:"errorPages,omitempty"`
	Redirects       []Redirect   `json:"redirects,omitempty" yaml:"redirects,omitempty"`
	Headers         Headers      `json:"headers,omitempty" yaml:"headers,omitempty"`
	Restrictions    Restrictions `json:"restrictions,omitempty" yaml:"restrictions,omitempty"`
	TLS             TLS          `json:"tls,omitempty" yaml:"tls,omitempty"`
	Logging         Logging      `json:"logging,omitempty" yaml:"logging,omitempty"`
	CreatedAt       time.Time    `json:"createdAt,omitempty" yaml:"createdAt,omitempty"`
	UpdatedAt       time.Time    `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	// ETag identifies the stored version (optimistic concurrency of
	// Update); computed, never stored.
	ETag string `json:"etag,omitempty" yaml:"-"`
}

// RouteKind is the outcome of the host resolution.
type RouteKind string

// Route kinds, in the order of the resolution.
const (
	// RouteDistribution: an alias (exact or wildcard) of a distribution.
	RouteDistribution RouteKind = "distribution"
	// RouteVirtualHost: "<bucket>.<NEXO_DOMAIN>".
	RouteVirtualHost RouteKind = "virtual-host"
	// RouteMain: the main S3/console domain, an IP literal or no host.
	RouteMain RouteKind = "main"
	// RoutePathStyle: an unknown host served by the path-style S3 API.
	RoutePathStyle RouteKind = "path-style"
	// RouteMisdirected: an unknown host refused with 421.
	RouteMisdirected RouteKind = "misdirected"
)

// Route is the resolution of a host.
type Route struct {
	Kind RouteKind `json:"kind"`
	// Host is the normalized host (lowercase, no port, no trailing dot).
	Host string `json:"host"`
	// Alias is the alias that matched ("*.preview.example.com" for a
	// wildcard), for RouteDistribution.
	Alias string `json:"alias,omitempty"`
	// Distribution is the matched distribution (a shared, read-only
	// snapshot), for RouteDistribution.
	Distribution *Distribution `json:"distribution,omitempty"`
	// Bucket is the bucket the host addresses: the origin of the
	// distribution, or the bucket of a virtual host.
	Bucket string `json:"bucket,omitempty"`
}

// Resolver maps the Host of a request to what serves it (see the package
// documentation). It reads the in-memory routing table only: no I/O.
type Resolver interface {
	Resolve(ctx context.Context, host string) Route
}

// TestInput asks how a distribution would serve a URL (nexoctl dist
// test, POST /distributions/{id}/test).
type TestInput struct {
	URL    string
	Method string
	// ClientIP is the address of the viewer (restrictions, signed
	// policies); the scheme of URL tells whether it uses HTTPS.
	ClientIP string
}

// TestResult is how a URL would be served, without reading the object.
type TestResult struct {
	Route Route `json:"route"`
	// BehaviorIndex is the index of the matching behavior, -1 for the
	// default behavior.
	BehaviorIndex int `json:"behaviorIndex"`
	// Behavior is the effective behavior (inherited fields resolved).
	Behavior Behavior `json:"behavior"`
	// Bucket and Key are the object the path maps to (Key is empty when
	// the path is refused).
	Bucket string `json:"bucket,omitempty"`
	Key    string `json:"key,omitempty"`
	// Redirect is the redirection that applies, if any.
	RedirectStatus   int    `json:"redirectStatus,omitempty"`
	RedirectLocation string `json:"redirectLocation,omitempty"`
	// Refusal explains why the request would be refused before reading
	// the object (method, restrictions, deny, invalid path).
	RefusalStatus int    `json:"refusalStatus,omitempty"`
	Refusal       string `json:"refusal,omitempty"`
	// ResponseHeaders are the headers added by the distribution.
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
}

// Service manages the distributions and answers the host resolution. It
// runs the subscription to the invalidations of the peers
// (lifecycle.Runner): Start loads every distribution and key group.
type Service interface {
	lifecycle.Runner
	Resolver
	KeyGroups
	SignedRequestVerifier

	// Create stores a new distribution (domain.ErrConflict when the id or
	// an alias is taken).
	Create(ctx context.Context, d Distribution) (Distribution, error)
	// Update replaces a distribution; a non-empty ifMatch must equal its
	// ETag (domain.ErrPreconditionFailed otherwise).
	Update(ctx context.Context, d Distribution, ifMatch string) (Distribution, error)
	// Get returns a distribution (ErrDistributionNotFound).
	Get(ctx context.Context, id string) (Distribution, error)
	// List returns every distribution, sorted by id.
	List(ctx context.Context) ([]Distribution, error)
	// Delete removes a distribution (ErrDistributionNotFound).
	Delete(ctx context.Context, id string) error
	// SetEnabled enables or disables a distribution.
	SetEnabled(ctx context.Context, id string, enabled bool) (Distribution, error)
	// Validate checks and normalizes a distribution without storing it
	// (domain.ErrInvalidArgument, domain.ErrConflict).
	Validate(ctx context.Context, d Distribution) (Distribution, error)
	// RotateOriginSecret generates a new origin secret, returned in clear
	// (the only time it is shown), and keeps the keep most recent previous
	// ones active (default 1). The header name defaults to
	// X-Origin-Secret.
	RotateOriginSecret(ctx context.Context, in RotateSecretInput) (secret string, d Distribution, err error)
	// Test reports how a URL would be served.
	Test(ctx context.Context, in TestInput) (TestResult, error)
	// Reload reloads every distribution and key group from the store.
	Reload(ctx context.Context) error
	// ListenerResolver returns the resolver of a listener: the aliases of
	// the distributions resolve to them only when serveAliases is true;
	// otherwise a host resolves as it would without the distributions
	// (virtual host, main domain, path-style or misdirected): the S3
	// listener of a deployment that did not opt in to serving the
	// distributions there. Pure accessor: no I/O.
	ListenerResolver(serveAliases bool) Resolver
}

// RotateSecretInput are the parameters of RotateOriginSecret.
type RotateSecretInput struct {
	ID string
	// Header names the header (default X-Origin-Secret, or the current
	// one).
	Header string
	// Keep is how many previous secrets stay active (default 1, 0 with
	// DropPrevious).
	Keep         int
	DropPrevious bool
}

// KeyAlgorithm is the algorithm of a signing key.
type KeyAlgorithm string

// Key algorithms.
const (
	KeyEd25519 KeyAlgorithm = "ed25519"
	KeyRSA     KeyAlgorithm = "rsa"
)

// PublicKey is a public key of a key group.
type PublicKey struct {
	// ID is the Key-Pair-Id of the signed URLs, unique across the key
	// groups.
	ID        string       `json:"id" yaml:"id"`
	Algorithm KeyAlgorithm `json:"algorithm" yaml:"algorithm"`
	// Bits is the size of an RSA key.
	Bits         int       `json:"bits,omitempty" yaml:"bits,omitempty"`
	PublicKeyPEM string    `json:"publicKeyPem" yaml:"publicKeyPem"`
	Comment      string    `json:"comment,omitempty" yaml:"comment,omitempty"`
	CreatedAt    time.Time `json:"createdAt" yaml:"createdAt"`
}

// KeyGroup is a set of public keys that sign the URLs and cookies of the
// signed behaviors.
type KeyGroup struct {
	ID        string      `json:"id" yaml:"id"`
	Comment   string      `json:"comment,omitempty" yaml:"comment,omitempty"`
	Keys      []PublicKey `json:"keys" yaml:"keys"`
	CreatedAt time.Time   `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt" yaml:"updatedAt"`
}

// GenerateKeyInput are the parameters of GenerateKey.
type GenerateKeyInput struct {
	KeyGroup  string
	Algorithm KeyAlgorithm
	// Bits is 2048 (default) or 4096 for RSA.
	Bits    int
	Comment string
}

// GeneratedKey is a key generated by the server: the private key is
// returned once and never stored.
type GeneratedKey struct {
	PublicKey     PublicKey `json:"publicKey"`
	PrivateKeyPEM string    `json:"privateKeyPem"`
}

// AddKeyInput are the parameters of AddPublicKey.
type AddKeyInput struct {
	KeyGroup string
	// PEM is a PKIX "PUBLIC KEY" (Ed25519, RSA of 2048 to 4096 bits).
	PEM     string
	Comment string
}

// KeyGroups manages the key groups of the signed behaviors.
type KeyGroups interface {
	CreateKeyGroup(ctx context.Context, id, comment string) (KeyGroup, error)
	GetKeyGroup(ctx context.Context, id string) (KeyGroup, error)
	ListKeyGroups(ctx context.Context) ([]KeyGroup, error)
	// DeleteKeyGroup refuses a group referenced by a distribution
	// (domain.ErrConflict).
	DeleteKeyGroup(ctx context.Context, id string) error
	// GenerateKey generates a key pair, stores the public key and
	// returns the private one.
	GenerateKey(ctx context.Context, in GenerateKeyInput) (GeneratedKey, error)
	// AddPublicKey stores a public key provided by the user.
	AddPublicKey(ctx context.Context, in AddKeyInput) (PublicKey, error)
	// DeleteKey removes a key from its group.
	DeleteKey(ctx context.Context, group, keyID string) error
}

// SignedRequest describes a request to a signed behavior.
type SignedRequest struct {
	// KeyGroup is the key group of the behavior.
	KeyGroup string
	// Scheme, Host (as received, with its port) and EscapedPath are the
	// URL of the request; RawQuery is its query, signature parameters
	// included.
	Scheme      string
	Host        string
	EscapedPath string
	RawQuery    string
	// Cookies are the request cookies by name.
	Cookies  map[string]string
	ClientIP string
	// Now is the verification time (default: the clock of the service).
	Now time.Time
}

// SignedRequestVerifier verifies the signed URLs and cookies.
type SignedRequestVerifier interface {
	// VerifySignedRequest checks the signature of the URL, or else of the
	// cookies, against the keys of the key group: a missing, expired,
	// not yet valid, tampered signature, an unknown key or a client
	// outside the IpAddress of the policy fail with an error wrapping
	// domain.ErrAccessDenied.
	VerifySignedRequest(ctx context.Context, in SignedRequest) error
}

// SignInput are the parameters of a signed URL or of signed cookies.
type SignInput struct {
	// URL is the resource ("https://www.example.com/downloads/v1.zip"); a
	// query it carries is kept and signed.
	URL string
	// KeyPairID is the id of the public key of the private key.
	KeyPairID string
	// PrivateKeyPEM is the private key (PKCS #8, PKCS #1 RSA or the
	// "ED25519 PRIVATE KEY" seed).
	PrivateKeyPEM string
	// Expires is when the signature expires (required).
	Expires time.Time
	// NotBefore, IPRange (an IP or a CIDR) and Resource (a URL with "*"
	// and "?" wildcards, default URL) make a custom policy; without them
	// the canned policy is used.
	NotBefore time.Time
	IPRange   string
	Resource  string
}

// SignedCookie is a cookie of a signed-cookie set.
type SignedCookie struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

// Signer signs URLs and cookies with a private key provided by the caller
// (the Admin API and nexoctl dist sign-url): no state, no I/O.
type Signer interface {
	SignURL(ctx context.Context, in SignInput) (string, error)
	// SignCookies returns CloudFront-Policy (or CloudFront-Expires for a
	// canned policy), CloudFront-Signature and CloudFront-Key-Pair-Id.
	SignCookies(ctx context.Context, in SignInput) ([]SignedCookie, error)
}

// SecretSource resolves the secrets referenced as "${secret:<name>}" (the
// NEXO_SECRET_<NAME> variables): an origin secret may name one instead of
// carrying its value.
type SecretSource interface {
	LookupSecret(ctx context.Context, name string) (string, bool)
}

// PublicAccessBlock reports whether the anonymous access to a bucket is
// blocked (block public access of the cluster or of the bucket): the
// public behaviors then refuse every request.
type PublicAccessBlock interface {
	PublicAccessBlocked(ctx context.Context, bucket string) (bool, error)
}

// Deps are the dependencies of the distribution service.
type Deps struct {
	// Store holds the documents (required): the system store, shared by
	// the nodes.
	Store sysstore.Store
	// Locks serializes the writes of the configuration across the nodes
	// (required): alias uniqueness is checked under it.
	Locks lock.Locker
	// Notifier broadcasts the changes to the peers and delivers theirs
	// (required; the local notifier on a single node).
	Notifier cluster.Notifier
	// Matcher compiles the path patterns and normalizes the request paths
	// (default match.New: the matcher of the access rules).
	Matcher match.Service
	Clock   clock.Clock
	Logger  *slog.Logger
}

// WebRequest is a viewer request to a website distribution, decoded by the
// transport.
type WebRequest struct {
	// Route is the resolution of the host (RouteDistribution).
	Route Route
	// Host is the Host as received (with its port).
	Host   string
	Method string
	// Path is the decoded path, EscapedPath the path as sent.
	Path        string
	EscapedPath string
	RawQuery    string
	Header      http.Header
	// ClientIP is the real client (behind the trusted proxies) and
	// Secure reports HTTPS between the client and the first proxy.
	ClientIP string
	Secure   bool
	// Identity is the SigV4 identity of the request, nil when it is not
	// signed; AuthErr reports a signature that could not be verified.
	Identity *domain.Identity
	AuthErr  error
	// Body and ContentLength are the content of an upload (PUT and POST
	// on the signed behaviors that allow them); ContentLength is -1 when
	// unknown.
	Body          io.Reader
	ContentLength int64
}

// WebResponse is the answer of a website request. The transport writes
// Status, Header and Body (nil: no body) and closes Body.
type WebResponse struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
	// Outcome classifies the answer for the logs ("object", "redirect",
	// "not-modified", "error", "error-page", "spa", "preflight", "denied").
	Outcome string
	// Bucket and Key are the object served or attempted.
	Bucket string
	Key    string
}

// Website serves the website distributions and admits the requests of
// the s3 ones.
type Website interface {
	// Serve answers a request to a website distribution. Errors of the
	// storage become answers (5xx); the error is non-nil only for a
	// request the transport must not have sent (no distribution).
	Serve(ctx context.Context, req WebRequest) (*WebResponse, error)
	// Admit applies to a request of any mode the checks that come before
	// the origin and before its authentication (docs/10 §10.2: the origin
	// secret "precede ogni altra valutazione"): disabled distribution,
	// source IP, origin secret, a default behavior denying everything. It
	// returns the refusal, nil when the request may go on (to the S3 API
	// for the s3 mode).
	Admit(ctx context.Context, req WebRequest) *WebResponse
	// AuthorizeS3 applies the access of an s3 distribution to a request
	// once authenticated (req.Identity): an authenticated default
	// behavior refuses the anonymous requests. nil when the request may
	// go on to the S3 API (IAM and the policies decide), and for the
	// other modes.
	AuthorizeS3(ctx context.Context, req WebRequest) *WebResponse
}

// WebsiteDeps are the dependencies of the website engine.
type WebsiteDeps struct {
	// Objects reads the objects (required).
	Objects object.Service
	// Policy authorizes the reads (required).
	Policy policy.Service
	// Signatures verifies the signed URLs and cookies (required: the
	// distribution Service).
	Signatures SignedRequestVerifier
	// Matcher compiles the path patterns of the behaviors and normalizes
	// the request paths (default match.New: the matcher of the access
	// rules).
	Matcher match.Service
	// PublicAccess reports the block public access of the buckets;
	// nil blocks nothing.
	PublicAccess PublicAccessBlock
	// Secrets resolves the origin secrets given as "${secret:<name>}";
	// nil leaves them unresolved (no request matches them).
	Secrets SecretSource
	Clock   clock.Clock
	Logger  *slog.Logger
}
