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

// Error codes of the certificates (APIError.Code).
const (
	CodeCertificateNotFound = "certificate_not_found"
	CodeInvalidCertificate  = "invalid_certificate"
)

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
