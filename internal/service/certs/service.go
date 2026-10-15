// Package certs manages the TLS certificates of the domains of the
// distributions (docs/10 §10.2 "TLS e certificati per dominio"): the
// certificates uploaded by the operators (PEM chain and key, validated:
// chain linkage, key matching the leaf, DNS names, validity) and the ones
// obtained and renewed with ACME (RFC 8555: Let's Encrypt, ZeroSSL, any
// ACME CA) through the HTTP-01 or the TLS-ALPN-01 challenge.
//
// Every certificate is a document of the system store, its key sealed
// (AES-256-GCM, Sealer), shared by the nodes: a node that stores or renews
// a certificate loads it at once and broadcasts the change, the peers
// reload it (cluster.Notifier). The ACME account key is sealed in the
// store too. The ACME work (orders, renewals 30 days before the
// expiration by default) runs on the leader only (cluster.Leader); the
// challenge answers are documents of the store as well, so that any node
// can answer the validation of the CA (HTTP-01 on the web listener,
// TLS-ALPN-01 on the TLS one).
//
// GetCertificate selects the certificate of a TLS handshake by SNI: an
// exact DNS name of a certificate, then its wildcard ("*.example.com"),
// the valid certificate expiring last winning; it answers the
// "acme-tls/1" handshakes of the TLS-ALPN-01 validation with the challenge
// certificate.
//
// Keystore serves the certificates of the server itself, read from the
// certificates directory (--certs-dir, docs/07 §7.3) and reloaded at
// runtime without interrupting the established connections; the listeners
// chain the two: the certificates of the distributions first, for the
// names they cover, then the keystore.
package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Source is where a certificate comes from.
type Source string

// Certificate sources.
const (
	SourceUpload Source = "upload"
	SourceACME   Source = "acme"
)

// Status is the state of a certificate at a given time.
type Status string

// Certificate states.
const (
	StatusValid Status = "valid"
	// StatusExpiring: valid, expiring within the warning window (21 days
	// by default).
	StatusExpiring Status = "expiring"
	StatusExpired  Status = "expired"
	// StatusNotYetValid: NotBefore is in the future.
	StatusNotYetValid Status = "not-yet-valid"
)

// Challenge types of ACME.
const (
	ChallengeHTTP01    = "http-01"
	ChallengeTLSALPN01 = "tls-alpn-01"
)

// Event kinds of the cluster invalidations of the certificates.
const (
	EventCertificate cluster.EventKind = "certificate"
)

// Certificate describes a stored certificate (never its key).
type Certificate struct {
	ID     string `json:"id"`
	Source Source `json:"source"`
	// Domains are the DNS names of the leaf certificate.
	Domains     []string  `json:"domains"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Serial      string    `json:"serial"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
	// Status is computed when the certificate is read.
	Status Status `json:"status"`
	// LastError is the last failure of an ACME renewal (ACME only).
	LastError string    `json:"lastError,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// UploadInput is a certificate provided by the operator.
type UploadInput struct {
	// ID names the certificate (lowercase letters, digits, '.', '_',
	// '-'); default: derived from the first DNS name.
	ID string
	// ChainPEM is the leaf certificate followed by its intermediates.
	ChainPEM string
	// KeyPEM is the private key of the leaf (PKCS #8, PKCS #1 or SEC 1).
	KeyPEM string
}

// ManagedDomains are the DNS names of one ACME certificate.
type ManagedDomains struct {
	// ID is the id of the certificate ("acme-<distribution>").
	ID      string
	Domains []string
}

// DomainSource lists the certificates to obtain with ACME (the
// distributions with tls.certificate "auto").
type DomainSource interface {
	ACMEDomains(ctx context.Context) ([]ManagedDomains, error)
}

// HostSource names the certificate a TLS handshake may use for a host:
// the one the enabled distribution serving the host references
// (tls.certificate "upload:<id>", or the ACME certificate of "auto"). It
// reads the in-memory routing table: no I/O on the handshakes.
type HostSource interface {
	// CertificateFor returns the id of the certificate of host; ok is
	// false when no enabled distribution serving host references one.
	CertificateFor(ctx context.Context, host string) (id string, ok bool)
}

// Sealer encrypts the private keys stored in the system store.
type Sealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	Open(ctx context.Context, sealed, aad []byte) ([]byte, error)
}

// Service manages the certificates and serves them to the TLS listeners.
// It runs the leader job of the ACME renewals and the subscriptions to the
// invalidations of the peers (lifecycle.Runner).
type Service interface {
	lifecycle.Runner
	// Upload validates and stores a certificate (domain.ErrInvalidArgument
	// for a broken chain, a key that does not match, no DNS name, an
	// expired certificate, a name of the deployment: WithMainHosts,
	// WithDomains).
	Upload(ctx context.Context, in UploadInput) (Certificate, error)
	// Get returns a certificate (ErrCertificateNotFound).
	Get(ctx context.Context, id string) (Certificate, error)
	// List returns every certificate, sorted by id.
	List(ctx context.Context) ([]Certificate, error)
	// Delete removes a certificate.
	Delete(ctx context.Context, id string) error
	// GetCertificate selects the certificate of a TLS handshake (the
	// tls.Config callback: the context is hello.Context()): the
	// certificate the distribution serving the name references (Hosts),
	// when it covers the name and is valid now. It returns nil, nil
	// otherwise, so that the listener falls back to its own (the main S3
	// and console hosts always keep the certificate of the server).
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
	// HTTPChallenge returns the key authorization of an HTTP-01 token
	// (ErrChallengeNotFound).
	HTTPChallenge(ctx context.Context, token string) (string, error)
	// Reconcile obtains the missing ACME certificates and renews the
	// expiring ones, now; a no-op on a node that is not the leader.
	Reconcile(ctx context.Context) error
	// Trigger asks the leader job for a reconciliation without waiting
	// for it (a distribution changed on this node; the leader learns the
	// changes of the other nodes from their invalidations).
	Trigger(ctx context.Context)
	// Reload reloads the certificates from the store.
	Reload(ctx context.Context) error
}

// Deps are the dependencies of the certificate service.
type Deps struct {
	// Store holds the certificates (required).
	Store sysstore.Store
	// Locks serializes the writes of a certificate (required).
	Locks lock.Locker
	// Notifier propagates the changes (required).
	Notifier cluster.Notifier
	// Leader elects the node running the ACME renewals (required).
	Leader cluster.Leader
	// Sealer seals the private keys (required).
	Sealer Sealer
	// Domains lists the ACME certificates; nil disables ACME.
	Domains DomainSource
	// Hosts binds the names of the handshakes to the certificates of the
	// distributions; nil serves every certificate for the names it
	// carries (tests only).
	Hosts HostSource
	// Metrics exposes the expiration of the certificates (optional).
	Metrics metrics.Registry
	Clock   clock.Clock
	Logger  *slog.Logger
}

// --- certificates of the server (certificates directory) -----------------------

// KeyPair describes a certificate of the certificates directory (never its
// key).
type KeyPair struct {
	// Name is "" for the default certificate (public.crt), the name of its
	// sub-directory for a certificate selected by SNI.
	Name string `json:"name"`
	// Domains are the DNS names of the leaf certificate, IPs its addresses.
	Domains     []string  `json:"domains,omitempty"`
	IPs         []string  `json:"ips,omitempty"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
}

// Keystore serves the TLS material of the listeners of the server from the
// certificates directory (--certs-dir, docs/07 §7.3):
//
//	<dir>/public.crt, <dir>/private.key        the default certificate (required)
//	<dir>/<name>/public.crt, private.key       more certificates, selected by SNI
//	<dir>/CAs/*                                 the CA certificates trusted for the peers
//
// It reloads the directory without a restart when its files change
// (watched, with a debounce, and polled as a safety net) and on Reload (the
// SIGHUP of the operator): every handshake reads the current material, so a
// reload applies to the next handshake while the established connections
// keep theirs. A reload that fails (a key that does not match its
// certificate, a file half written...) keeps the previous material and is
// logged; the initial load fails NewKeystore.
//
// GetCertificate, GetClientCertificate, ClientCAs and RootCAs read the
// material in memory: they never block nor do I/O (the documented exception
// to the context rule for the TLS callbacks and the pure accessors).
type Keystore interface {
	lifecycle.Runner
	// GetCertificate selects the certificate of a handshake: the
	// certificate of a sub-directory whose leaf names the server name
	// exactly, then one whose wildcard covers it (the valid one expiring
	// last wins), the default certificate otherwise.
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
	// GetClientCertificate returns the default certificate, presented by
	// the node to its peers (mutual TLS between the nodes).
	GetClientCertificate(info *tls.CertificateRequestInfo) (*tls.Certificate, error)
	// ClientCAs returns the CAs verifying the client certificates of the
	// peers: the certificates of CAs/ and the default certificate itself
	// (not the issuers of its chain). Pure accessor.
	ClientCAs() *x509.CertPool
	// RootCAs returns the CAs verifying the server certificates of the
	// peers: the system roots, the certificates of CAs/ and of
	// public.crt. Pure accessor.
	RootCAs() *x509.CertPool
	// Reload reads the directory again now; on error the current material
	// is kept.
	Reload(ctx context.Context) error
	// KeyPairs describes the loaded certificates, the default one first,
	// then by name.
	KeyPairs(ctx context.Context) []KeyPair
}

// KeystoreDeps are the dependencies of the keystore.
type KeystoreDeps struct {
	// Metrics exposes the expiration of the certificates and the reloads
	// (optional).
	Metrics metrics.Registry
	Clock   clock.Clock
	Logger  *slog.Logger
}
