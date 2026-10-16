package nexoadmin

import (
	"context"
	"net/http"
	"time"
)

// TLS certificates of the domains of the distributions (docs/10 §10.2).

// Certificate states (Certificate.Status).
const (
	CertificateValid       = "valid"
	CertificateExpiring    = "expiring"
	CertificateExpired     = "expired"
	CertificateNotYetValid = "not-yet-valid"
)

// Certificate describes a stored certificate (never its key).
type Certificate struct {
	ID string `json:"id"`
	// Source is upload or acme.
	Source      string    `json:"source"`
	Domains     []string  `json:"domains"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
	// Status is valid, expiring (within 21 days), expired or
	// not-yet-valid.
	Status    string    `json:"status"`
	LastError string    `json:"lastError,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// UsedBy are the distributions whose TLS names the certificate.
	UsedBy []string `json:"usedBy,omitempty"`
}

// ManagedCertificate is the ACME certificate of distributions.
type ManagedCertificate struct {
	ID            string   `json:"id"`
	Domains       []string `json:"domains"`
	Distributions []string `json:"distributions"`
	// Challenge is the ACME challenge ("" for the one of the deployment),
	// DNSProvider the DNS provider of dns-01.
	Challenge   string `json:"challenge,omitempty"`
	DNSProvider string `json:"dnsProvider,omitempty"`
	// State is issued or pending.
	State       string       `json:"state"`
	Certificate *Certificate `json:"certificate,omitempty"`
}

// ACMEStatus is the state of the ACME certificates.
type ACMEStatus struct {
	Directory string               `json:"directory"`
	Challenge string               `json:"challenge"`
	Managed   []ManagedCertificate `json:"managed"`
}

// CertificateList is the outcome of ListCertificates.
type CertificateList struct {
	Items []Certificate `json:"items"`
	ACME  ACMEStatus    `json:"acme"`
}

// UploadCertificateRequest uploads a PEM certificate and its key.
type UploadCertificateRequest struct {
	// ID names the certificate (default: from its first DNS name); a
	// certificate of the same id is replaced.
	ID             string `json:"id,omitempty"`
	CertificatePEM string `json:"certificatePem"`
	PrivateKeyPEM  string `json:"privateKeyPem"`
}

// Error codes of the certificates and of the DNS providers
// (APIError.Code).
const (
	CodeCertificateNotFound = "certificate_not_found"
	CodeInvalidCertificate  = "invalid_certificate"
	CodeDNSProviderNotFound = "dns_provider_not_found"
	CodeInvalidDNSProvider  = "invalid_dns_provider"
	CodeDNSProviderExists   = "dns_provider_exists"
	CodeDNSProviderInUse    = "dns_provider_in_use"
)

// DNS provider types (DNSProvider.Type).
const (
	DNSProviderCloudflare = "cloudflare"
	DNSProviderRoute53    = "route53"
	DNSProviderRFC2136    = "rfc2136"
)

// CloudflareSettings are the settings of a Cloudflare provider.
type CloudflareSettings struct {
	ZoneID string `json:"zoneId,omitempty"`
}

// Route53Settings are the settings of a Route 53 provider; without
// AccessKeyID the ambient AWS credentials of the nodes are used.
type Route53Settings struct {
	AccessKeyID  string `json:"accessKeyId,omitempty"`
	Region       string `json:"region,omitempty"`
	HostedZoneID string `json:"hostedZoneId,omitempty"`
}

// RFC2136Settings are the settings of an RFC 2136 provider.
type RFC2136Settings struct {
	Nameserver    string `json:"nameserver"`
	Zone          string `json:"zone,omitempty"`
	TSIGKeyName   string `json:"tsigKeyName,omitempty"`
	TSIGAlgorithm string `json:"tsigAlgorithm,omitempty"`
}

// DNSProvider is a DNS provider of the ACME DNS-01 challenge (never its
// credentials).
type DNSProvider struct {
	Name       string              `json:"name"`
	Type       string              `json:"type"`
	Comment    string              `json:"comment,omitempty"`
	Cloudflare *CloudflareSettings `json:"cloudflare,omitempty"`
	Route53    *Route53Settings    `json:"route53,omitempty"`
	RFC2136    *RFC2136Settings    `json:"rfc2136,omitempty"`
	// TTL of the records, PropagationTimeout and PollingInterval of the
	// propagation check, in seconds (0: the defaults).
	TTL                int `json:"ttl,omitempty"`
	PropagationTimeout int `json:"propagationTimeout,omitempty"`
	PollingInterval    int `json:"pollingInterval,omitempty"`
	// HasCredentials reports stored credentials.
	HasCredentials bool      `json:"hasCredentials"`
	UsedBy         []string  `json:"usedBy"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// DNSCredentials are the secrets of a DNS provider, write-only.
type DNSCredentials struct {
	// APIToken of Cloudflare.
	APIToken string `json:"apiToken,omitempty"`
	// SecretAccessKey and SessionToken of Route 53.
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`
	// TSIGSecret (base64) of RFC 2136.
	TSIGSecret string `json:"tsigSecret,omitempty"`
}

// String hides the credentials.
func (DNSCredentials) String() string { return "[REDACTED]" }

// GoString hides the credentials.
func (DNSCredentials) GoString() string { return "nexoadmin.DNSCredentials{[REDACTED]}" }

// DNSProviderRequest creates or replaces a DNS provider. On a replacement
// the empty credentials keep the stored ones.
type DNSProviderRequest struct {
	Name               string              `json:"name,omitempty"`
	Type               string              `json:"type"`
	Comment            string              `json:"comment,omitempty"`
	Cloudflare         *CloudflareSettings `json:"cloudflare,omitempty"`
	Route53            *Route53Settings    `json:"route53,omitempty"`
	RFC2136            *RFC2136Settings    `json:"rfc2136,omitempty"`
	TTL                int                 `json:"ttl,omitempty"`
	PropagationTimeout int                 `json:"propagationTimeout,omitempty"`
	PollingInterval    int                 `json:"pollingInterval,omitempty"`
	Credentials        DNSCredentials      `json:"credentials"`
}

// DNSProviderTestResult is the outcome of TestDNSProvider.
type DNSProviderTestResult struct {
	OK     bool   `json:"ok"`
	Record string `json:"record"`
	// Step is the failed step: present, propagation or cleanup.
	Step       string `json:"step,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// ListDNSProviders implements AdminClient.
func (c *client) ListDNSProviders(ctx context.Context) ([]DNSProvider, error) {
	var out struct {
		Items []DNSProvider `json:"items"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/dns-providers"}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []DNSProvider{}
	}
	return out.Items, nil
}

// GetDNSProvider implements AdminClient.
func (c *client) GetDNSProvider(ctx context.Context, name string) (*DNSProvider, error) {
	var out DNSProvider
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("dns-providers", name)}, &out)
}

// CreateDNSProvider implements AdminClient.
func (c *client) CreateDNSProvider(ctx context.Context, in DNSProviderRequest) (*DNSProvider, error) {
	var out DNSProvider
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/dns-providers", body: in}, &out)
}

// UpdateDNSProvider implements AdminClient.
func (c *client) UpdateDNSProvider(ctx context.Context, name string, in DNSProviderRequest) (*DNSProvider, error) {
	var out DNSProvider
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("dns-providers", name), body: in}, &out)
}

// DeleteDNSProvider implements AdminClient.
func (c *client) DeleteDNSProvider(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("dns-providers", name)}, nil)
}

// TestDNSProvider implements AdminClient.
func (c *client) TestDNSProvider(ctx context.Context, name, domain string) (*DNSProviderTestResult, error) {
	var out DNSProviderTestResult
	body := struct {
		Domain string `json:"domain"`
	}{domain}
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("dns-providers", name) + "/test", body: body}, &out)
}

// ListCertificates implements AdminClient.
func (c *client) ListCertificates(ctx context.Context) (*CertificateList, error) {
	var out CertificateList
	if err := c.do(ctx, request{method: http.MethodGet, path: "/certificates"}, &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		out.Items = []Certificate{}
	}
	return &out, nil
}

// UploadCertificate implements AdminClient.
func (c *client) UploadCertificate(ctx context.Context, in UploadCertificateRequest) (*Certificate, error) {
	var out Certificate
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/certificates", body: in}, &out)
}

// GetCertificate implements AdminClient.
func (c *client) GetCertificate(ctx context.Context, id string) (*Certificate, error) {
	var out Certificate
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("certificates", id)}, &out)
}

// DeleteCertificate implements AdminClient.
func (c *client) DeleteCertificate(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("certificates", id)}, nil)
}
