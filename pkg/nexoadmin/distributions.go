package nexoadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Distributions (docs/10 §10.2): a bucket, or a prefix of it, exposed on
// custom domains as a website or an S3 endpoint.

// Modes and access levels of the distributions.
const (
	ModeWebsite = "website"
	ModeS3      = "s3"

	AccessPublic        = "public"
	AccessSigned        = "signed"
	AccessAuthenticated = "authenticated"
	AccessDeny          = "deny"
)

// TLS certificate sources of a distribution.
const (
	CertificateAuto         = "auto"
	CertificateNone         = "none"
	CertificateUploadPrefix = "upload:"
)

// ACME challenges of a distribution (DistributionTLS.Challenge).
const (
	ChallengeHTTP01    = "http-01"
	ChallengeTLSALPN01 = "tls-alpn-01"
	ChallengeDNS01     = "dns-01"
)

// Origin is the bucket (and prefix) a distribution serves.
type Origin struct {
	Bucket         string `json:"bucket" yaml:"bucket"`
	PathPrefix     string `json:"pathPrefix,omitempty" yaml:"pathPrefix,omitempty"`
	VersionPinning bool   `json:"versionPinning,omitempty" yaml:"versionPinning,omitempty"`
}

// BehaviorCache is the cache of a behavior. Extra keeps the members this
// client does not know (the cache of docs/11).
type BehaviorCache struct {
	CacheControl string                     `json:"cacheControl,omitempty" yaml:"cacheControl,omitempty"`
	Extra        map[string]json.RawMessage `json:"-" yaml:"-"`
}

type behaviorCacheJSON BehaviorCache

// MarshalJSON implements json.Marshaler.
func (c BehaviorCache) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(behaviorCacheJSON(c), c.Extra)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *BehaviorCache) UnmarshalJSON(data []byte) error {
	var v behaviorCacheJSON
	extra, err := unmarshalWithExtra(data, &v)
	if err != nil {
		return err
	}
	*c = BehaviorCache(v)
	c.Extra = extra
	return nil
}

// Signing names the key group of a signed behavior.
type Signing struct {
	KeyGroup string `json:"keyGroup" yaml:"keyGroup"`
}

// DistributionCORS is the cross-origin policy of a distribution or a
// behavior.
type DistributionCORS struct {
	AllowedOrigins   []string `json:"allowedOrigins" yaml:"allowedOrigins"`
	AllowedMethods   []string `json:"allowedMethods,omitempty" yaml:"allowedMethods,omitempty"`
	AllowedHeaders   []string `json:"allowedHeaders,omitempty" yaml:"allowedHeaders,omitempty"`
	ExposeHeaders    []string `json:"exposeHeaders,omitempty" yaml:"exposeHeaders,omitempty"`
	MaxAge           int      `json:"maxAge,omitempty" yaml:"maxAge,omitempty"`
	AllowCredentials bool     `json:"allowCredentials,omitempty" yaml:"allowCredentials,omitempty"`
}

// Behavior is how the requests of a path are served; an unset field
// inherits from the default behavior. Extra keeps the members this client
// does not know.
type Behavior struct {
	PathPattern     *KeyPattern       `json:"pathPattern,omitempty" yaml:"pathPattern,omitempty"`
	Access          string            `json:"access,omitempty" yaml:"access,omitempty"`
	AllowedMethods  []string          `json:"allowedMethods,omitempty" yaml:"allowedMethods,omitempty"`
	IndexDocument   string            `json:"indexDocument,omitempty" yaml:"indexDocument,omitempty"`
	SPAFallback     string            `json:"spaFallback,omitempty" yaml:"spaFallback,omitempty"`
	Cache           *BehaviorCache    `json:"cache,omitempty" yaml:"cache,omitempty"`
	Signing         *Signing          `json:"signing,omitempty" yaml:"signing,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty" yaml:"responseHeaders,omitempty"`
	CORS            *DistributionCORS `json:"cors,omitempty" yaml:"cors,omitempty"`
	// Images serves the variants of the images at their own URL.
	Images *BehaviorImages            `json:"images,omitempty" yaml:"images,omitempty"`
	Extra  map[string]json.RawMessage `json:"-" yaml:"-"`
}

// BehaviorImages serves the variants of the images the media worker
// derived at the URL of the image itself: with Negotiate, the format is
// negotiated with the Accept header (or ?format=), the width chosen with
// ?w=; ?format=original serves the original (website mode).
type BehaviorImages struct {
	Negotiate bool `json:"negotiate" yaml:"negotiate"`
}

type behaviorJSON Behavior

// MarshalJSON implements json.Marshaler.
func (b Behavior) MarshalJSON() ([]byte, error) { return marshalWithExtra(behaviorJSON(b), b.Extra) }

// UnmarshalJSON implements json.Unmarshaler.
func (b *Behavior) UnmarshalJSON(data []byte) error {
	var v behaviorJSON
	extra, err := unmarshalWithExtra(data, &v)
	if err != nil {
		return err
	}
	*b = Behavior(v)
	b.Extra = extra
	return nil
}

// ErrorPage replaces the answer of an error status with a page.
type ErrorPage struct {
	Status         int    `json:"status" yaml:"status"`
	Path           string `json:"path" yaml:"path"`
	ResponseStatus int    `json:"responseStatus,omitempty" yaml:"responseStatus,omitempty"`
}

// Redirect is a redirection rule.
type Redirect struct {
	From      string `json:"from,omitempty" yaml:"from,omitempty"`
	To        string `json:"to,omitempty" yaml:"to,omitempty"`
	HostFrom  string `json:"hostFrom,omitempty" yaml:"hostFrom,omitempty"`
	ToHost    string `json:"toHost,omitempty" yaml:"toHost,omitempty"`
	HTTPSOnly bool   `json:"httpsOnly,omitempty" yaml:"httpsOnly,omitempty"`
	Status    int    `json:"status,omitempty" yaml:"status,omitempty"`
}

// DistributionHeaders are the headers of the answers.
type DistributionHeaders struct {
	Response map[string]string `json:"response,omitempty" yaml:"response,omitempty"`
	CORS     *DistributionCORS `json:"cors,omitempty" yaml:"cors,omitempty"`
}

// IPRestriction filters the clients by address.
type IPRestriction struct {
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty" yaml:"deny,omitempty"`
}

// OriginSecret accepts only the requests with one of the secrets in the
// header Name (the values are stored hashed).
type OriginSecret struct {
	Name   string   `json:"name" yaml:"name"`
	Value  string   `json:"value,omitempty" yaml:"value,omitempty"`
	Values []string `json:"values,omitempty" yaml:"values,omitempty"`
}

// RateLimit limits the requests of the distribution on each node:
// RequestsPerSecond (0: no limit), Burst at once (0: the rate), Per "ip"
// (default) or "distribution".
type RateLimit struct {
	RequestsPerSecond int    `json:"requestsPerSecond,omitempty" yaml:"requestsPerSecond,omitempty"`
	Burst             int    `json:"burst,omitempty" yaml:"burst,omitempty"`
	Per               string `json:"per,omitempty" yaml:"per,omitempty"`
}

// Restrictions are the admission rules of the requests.
type Restrictions struct {
	SourceIP           *IPRestriction `json:"sourceIp,omitempty" yaml:"sourceIp,omitempty"`
	OriginSecretHeader *OriginSecret  `json:"originSecretHeader,omitempty" yaml:"originSecretHeader,omitempty"`
	RateLimit          *RateLimit     `json:"rateLimit,omitempty" yaml:"rateLimit,omitempty"`
}

// DistributionTLS is the certificate of the aliases.
type DistributionTLS struct {
	// Certificate is CertificateAuto, CertificateNone or
	// CertificateUploadPrefix + id.
	Certificate string `json:"certificate,omitempty" yaml:"certificate,omitempty"`
	// Challenge is the ACME challenge of CertificateAuto: "" (the one of
	// the deployment), ChallengeHTTP01, ChallengeTLSALPN01 or
	// ChallengeDNS01 (required by a wildcard alias).
	Challenge string `json:"challenge,omitempty" yaml:"challenge,omitempty"`
	// DNSProvider names the DNS provider of ChallengeDNS01.
	DNSProvider string `json:"dnsProvider,omitempty" yaml:"dnsProvider,omitempty"`
}

// Logging configures the access log.
type Logging struct {
	AccessLog bool   `json:"accessLog,omitempty" yaml:"accessLog,omitempty"`
	Format    string `json:"format,omitempty" yaml:"format,omitempty"`
	Target    string `json:"target,omitempty" yaml:"target,omitempty"`
}

// Distribution exposes a bucket on custom domains. Extra keeps the members
// this client does not know, so that UpdateDistribution never drops them.
type Distribution struct {
	ID              string                     `json:"id" yaml:"id"`
	Enabled         bool                       `json:"enabled" yaml:"enabled"`
	Comment         string                     `json:"comment,omitempty" yaml:"comment,omitempty"`
	Aliases         []string                   `json:"aliases" yaml:"aliases"`
	Origin          Origin                     `json:"origin" yaml:"origin"`
	Mode            string                     `json:"mode,omitempty" yaml:"mode,omitempty"`
	DefaultBehavior Behavior                   `json:"defaultBehavior" yaml:"defaultBehavior"`
	Behaviors       []Behavior                 `json:"behaviors,omitempty" yaml:"behaviors,omitempty"`
	ErrorPages      []ErrorPage                `json:"errorPages,omitempty" yaml:"errorPages,omitempty"`
	Redirects       []Redirect                 `json:"redirects,omitempty" yaml:"redirects,omitempty"`
	Headers         DistributionHeaders        `json:"headers" yaml:"headers,omitempty"`
	Restrictions    Restrictions               `json:"restrictions" yaml:"restrictions,omitempty"`
	TLS             DistributionTLS            `json:"tls" yaml:"tls,omitempty"`
	Logging         Logging                    `json:"logging" yaml:"logging,omitempty"`
	CreatedAt       *time.Time                 `json:"createdAt,omitempty" yaml:"-"`
	UpdatedAt       *time.Time                 `json:"updatedAt,omitempty" yaml:"-"`
	ETag            string                     `json:"etag,omitempty" yaml:"-"`
	Extra           map[string]json.RawMessage `json:"-" yaml:"-"`
}

type distributionJSON Distribution

// MarshalJSON implements json.Marshaler.
func (d Distribution) MarshalJSON() ([]byte, error) {
	return marshalWithExtra(distributionJSON(d), d.Extra)
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Distribution) UnmarshalJSON(data []byte) error {
	var v distributionJSON
	extra, err := unmarshalWithExtra(data, &v)
	if err != nil {
		return err
	}
	*d = Distribution(v)
	d.Extra = extra
	return nil
}

// Route is the resolution of a host.
type Route struct {
	// Kind is distribution, virtual-host, main, path-style or misdirected.
	Kind         string `json:"kind"`
	Host         string `json:"host"`
	Alias        string `json:"alias,omitempty"`
	Distribution string `json:"distribution,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Enabled      *bool  `json:"enabled,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
}

// DistributionTestRequest asks how a distribution serves a URL.
type DistributionTestRequest struct {
	// URL is an absolute URL on an alias, or a path.
	URL      string `json:"url"`
	Method   string `json:"method,omitempty"`
	ClientIP string `json:"clientIp,omitempty"`
}

// DistributionTestResult is how a distribution serves a URL.
type DistributionTestResult struct {
	Route Route `json:"route"`
	// BehaviorIndex is the index of the behavior, -1 for the default one;
	// Behavior is the effective one.
	BehaviorIndex    int               `json:"behaviorIndex"`
	Behavior         Behavior          `json:"behavior"`
	Bucket           string            `json:"bucket,omitempty"`
	Key              string            `json:"key,omitempty"`
	RedirectStatus   int               `json:"redirectStatus,omitempty"`
	RedirectLocation string            `json:"redirectLocation,omitempty"`
	RefusalStatus    int               `json:"refusalStatus,omitempty"`
	Refusal          string            `json:"refusal,omitempty"`
	ResponseHeaders  map[string]string `json:"responseHeaders"`
}

// RotateOriginSecretRequest rotates the origin secret.
type RotateOriginSecretRequest struct {
	Header       string `json:"header,omitempty"`
	Keep         int    `json:"keep,omitempty"`
	DropPrevious bool   `json:"dropPrevious,omitempty"`
}

// RotatedOriginSecret is the new origin secret, shown once.
type RotatedOriginSecret struct {
	Secret       string       `json:"secret"`
	Header       string       `json:"header"`
	Distribution Distribution `json:"distribution"`
}

// SignRequest signs a URL, or cookies, of a signed behavior.
type SignRequest struct {
	// URL is an absolute URL on an alias, or a path completed with Scheme
	// (default https) and Host (default: the first exact alias).
	URL    string `json:"url"`
	Scheme string `json:"scheme,omitempty"`
	Host   string `json:"host,omitempty"`
	// PrivateKeyPEM is the private key of a key of the key group of the
	// behavior; KeyPairID names it (default: found from the key).
	PrivateKeyPEM string `json:"privateKeyPem"`
	KeyPairID     string `json:"keyPairId,omitempty"`
	// ExpiresIn (seconds) or ExpiresAt.
	ExpiresIn int64      `json:"expiresIn,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	IPRange   string     `json:"ipRange,omitempty"`
	Resource  string     `json:"resource,omitempty"`
	Cookies   bool       `json:"cookies,omitempty"`
}

// SignedCookie is a signed cookie.
type SignedCookie struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

// SignedURL is a signed URL or signed cookies.
type SignedURL struct {
	URL       string         `json:"url"`
	Cookies   []SignedCookie `json:"cookies"`
	KeyGroup  string         `json:"keyGroup"`
	KeyPairID string         `json:"keyPairId"`
	ExpiresAt time.Time      `json:"expiresAt"`
}

// Key algorithms of the key groups.
const (
	KeyEd25519 = "ed25519"
	KeyRSA     = "rsa"
)

// PublicKey is a key of a key group.
type PublicKey struct {
	// ID is the Key-Pair-Id of the signed URLs.
	ID           string    `json:"id"`
	Algorithm    string    `json:"algorithm"`
	Bits         int       `json:"bits,omitempty"`
	PublicKeyPEM string    `json:"publicKeyPem"`
	Comment      string    `json:"comment,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// KeyGroup is a set of keys signing the URLs and cookies.
type KeyGroup struct {
	ID        string      `json:"id"`
	Comment   string      `json:"comment,omitempty"`
	Keys      []PublicKey `json:"keys"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// GenerateKeyRequest describes a key pair to generate.
type GenerateKeyRequest struct {
	Algorithm string `json:"algorithm,omitempty"`
	Bits      int    `json:"bits,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

// CreateKeyGroupRequest creates a key group, with a first key pair when
// GenerateKey is set.
type CreateKeyGroupRequest struct {
	ID          string              `json:"id"`
	Comment     string              `json:"comment,omitempty"`
	GenerateKey *GenerateKeyRequest `json:"generateKey,omitempty"`
}

// GeneratedKey is a generated key pair: the private key is returned once.
type GeneratedKey struct {
	PublicKey     PublicKey `json:"publicKey"`
	PrivateKeyPEM string    `json:"privateKeyPem"`
}

// CreatedKeyGroup is a new key group and its generated key pair.
type CreatedKeyGroup struct {
	KeyGroup KeyGroup      `json:"keyGroup"`
	Key      *GeneratedKey `json:"key,omitempty"`
}

// AddKeyRequest adds a public key (PublicKeyPEM) or generates a key pair.
type AddKeyRequest struct {
	PublicKeyPEM string `json:"publicKeyPem,omitempty"`
	Algorithm    string `json:"algorithm,omitempty"`
	Bits         int    `json:"bits,omitempty"`
	Comment      string `json:"comment,omitempty"`
}

// AddedKey is the added key; PrivateKeyPEM is set (once) for a generated
// key pair.
type AddedKey struct {
	PublicKey     PublicKey `json:"publicKey"`
	PrivateKeyPEM string    `json:"privateKeyPem,omitempty"`
}

// Error codes of the distributions (APIError.Code).
const (
	CodeAliasConflict        = "alias_conflict"
	CodeDistributionNotFound = "distribution_not_found"
	CodeKeyGroupNotFound     = "keygroup_not_found"
	CodeKeyNotFound          = "key_not_found"
	CodeInvalidDistribution  = "invalid_distribution"
)

// ListDistributions implements AdminClient.
func (c *client) ListDistributions(ctx context.Context) ([]Distribution, error) {
	var out struct {
		Items []Distribution `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/distributions"}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []Distribution{}
	}
	return out.Items, nil
}

// CreateDistribution implements AdminClient.
func (c *client) CreateDistribution(ctx context.Context, d Distribution) (*Distribution, error) {
	var out Distribution
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/distributions", body: d}, &out)
}

// GetDistribution implements AdminClient.
func (c *client) GetDistribution(ctx context.Context, id string) (*Distribution, error) {
	var out Distribution
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("distributions", id)}, &out)
}

// UpdateDistribution implements AdminClient.
func (c *client) UpdateDistribution(ctx context.Context, d Distribution, ifMatch string) (*Distribution, error) {
	var out Distribution
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("distributions", d.ID), header: ifMatchHeader(ifMatch), body: d}, &out)
}

// DeleteDistribution implements AdminClient.
func (c *client) DeleteDistribution(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("distributions", id)}, nil)
}

// TestDistribution implements AdminClient.
func (c *client) TestDistribution(ctx context.Context, id string, in DistributionTestRequest) (*DistributionTestResult, error) {
	var out DistributionTestResult
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("distributions", id, "test"), body: in}, &out)
}

// RotateOriginSecret implements AdminClient.
func (c *client) RotateOriginSecret(ctx context.Context, id string, in RotateOriginSecretRequest) (*RotatedOriginSecret, error) {
	var out RotatedOriginSecret
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("distributions", id, "origin-secret", "rotate"), body: in}, &out)
}

// SignDistributionURL implements AdminClient.
func (c *client) SignDistributionURL(ctx context.Context, id string, in SignRequest) (*SignedURL, error) {
	var out SignedURL
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("distributions", id, "sign"), body: in}, &out)
}

// ResolveHost implements AdminClient.
func (c *client) ResolveHost(ctx context.Context, host string) (*Route, error) {
	var out Route
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/routing/resolve", query: url.Values{"host": {host}}}, &out)
}

// ListKeyGroups implements AdminClient.
func (c *client) ListKeyGroups(ctx context.Context) ([]KeyGroup, error) {
	var out struct {
		Items []KeyGroup `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/keygroups"}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []KeyGroup{}
	}
	return out.Items, nil
}

// CreateKeyGroup implements AdminClient.
func (c *client) CreateKeyGroup(ctx context.Context, in CreateKeyGroupRequest) (*CreatedKeyGroup, error) {
	var out CreatedKeyGroup
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/keygroups", body: in}, &out)
}

// GetKeyGroup implements AdminClient.
func (c *client) GetKeyGroup(ctx context.Context, id string) (*KeyGroup, error) {
	var out KeyGroup
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("keygroups", id)}, &out)
}

// DeleteKeyGroup implements AdminClient.
func (c *client) DeleteKeyGroup(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("keygroups", id)}, nil)
}

// AddKeyGroupKey implements AdminClient.
func (c *client) AddKeyGroupKey(ctx context.Context, group string, in AddKeyRequest) (*AddedKey, error) {
	var out AddedKey
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("keygroups", group, "keys"), body: in}, &out)
}

// DeleteKeyGroupKey implements AdminClient.
func (c *client) DeleteKeyGroupKey(ctx context.Context, group, keyID string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("keygroups", group, "keys", keyID)}, nil)
}
