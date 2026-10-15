package certs

import (
	"net/http"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/service/cluster"
)

// Defaults of the certificate service.
const (
	// LetsEncryptDirectory is the ACME directory of Let's Encrypt.
	LetsEncryptDirectory = "https://acme-v02.api.letsencrypt.org/directory"
	// DefaultRenewBefore renews an ACME certificate 30 days before it
	// expires.
	DefaultRenewBefore = 30 * 24 * time.Hour
	// DefaultExpiryWarning is the window of the expiration warnings
	// (docs/10: alert at 21 days).
	DefaultExpiryWarning = 21 * 24 * time.Hour
	// DefaultCheckInterval is the period of the leader job.
	DefaultCheckInterval = time.Hour
	// DefaultRetryInterval is the delay before a failed order is retried.
	DefaultRetryInterval = 10 * time.Minute
)

// Option configures the certificate service.
type Option func(*options)

type options struct {
	directory     string
	email         string
	challenge     string
	renewBefore   time.Duration
	warnBefore    time.Duration
	checkInterval time.Duration
	retryInterval time.Duration
	httpClient    *http.Client
	lockTimeout   time.Duration
	reconcileOn   []cluster.EventKind
	// mainHosts and domains are the names of the deployment no uploaded
	// certificate may carry.
	mainHosts []string
	domains   []string
}

func defaultOptions() options {
	return options{
		directory: LetsEncryptDirectory, challenge: ChallengeHTTP01,
		renewBefore: DefaultRenewBefore, warnBefore: DefaultExpiryWarning,
		checkInterval: DefaultCheckInterval, retryInterval: DefaultRetryInterval,
		httpClient: http.DefaultClient, lockTimeout: 30 * time.Second,
	}
}

// WithACMEDirectory sets the directory URL of the ACME CA (default Let's
// Encrypt).
func WithACMEDirectory(url string) Option {
	return func(o *options) {
		if url = strings.TrimSpace(url); url != "" {
			o.directory = url
		}
	}
}

// WithACMEEmail sets the contact of the ACME account.
func WithACMEEmail(email string) Option {
	return func(o *options) { o.email = strings.TrimSpace(email) }
}

// WithChallenge selects the ACME challenge: ChallengeHTTP01 (default,
// answered on the web listener, port 80) or ChallengeTLSALPN01 (answered
// on the TLS web listener, port 443).
func WithChallenge(c string) Option {
	return func(o *options) {
		if c == ChallengeHTTP01 || c == ChallengeTLSALPN01 {
			o.challenge = c
		}
	}
}

// WithRenewBefore sets how long before the expiration an ACME certificate
// is renewed (default 30 days).
func WithRenewBefore(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.renewBefore = d
		}
	}
}

// WithExpiryWarning sets the window of the expiration warnings (default
// 21 days).
func WithExpiryWarning(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.warnBefore = d
		}
	}
}

// WithCheckInterval sets the period of the leader job (default 1h).
func WithCheckInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.checkInterval = d
		}
	}
}

// WithRetryInterval sets the delay before a failed order is retried
// (default 10m).
func WithRetryInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.retryInterval = d
		}
	}
}

// WithHTTPClient sets the client of the ACME server (default
// http.DefaultClient): a client trusting a private CA, a proxy...
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) {
		if c != nil {
			o.httpClient = c
		}
	}
}

// WithReconcileOn makes the leader reconcile the ACME certificates when
// the peers broadcast an event of these kinds (the distributions).
func WithReconcileOn(kinds ...cluster.EventKind) Option {
	return func(o *options) { o.reconcileOn = append(o.reconcileOn, kinds...) }
}

// WithMainHosts sets the S3 and console hosts of the deployment
// (NEXO_SERVER_URL, NEXO_BROWSER_REDIRECT_URL): an uploaded certificate
// for one of them, or for a wildcard covering one, is refused.
func WithMainHosts(hosts ...string) Option {
	return func(o *options) { o.mainHosts = append(o.mainHosts, normalizeNames(hosts)...) }
}

// WithDomains sets the virtual-host domains of the deployment
// (NEXO_DOMAIN): an uploaded certificate for one of them, for a wildcard
// covering one or for every bucket of one ("*.<domain>") is refused.
func WithDomains(domains ...string) Option {
	return func(o *options) { o.domains = append(o.domains, normalizeNames(domains)...) }
}

// normalizeNames lowercases the names and drops the empty ones and the
// trailing dots.
func normalizeNames(names []string) []string {
	var out []string
	for _, n := range names {
		if n = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), "."); n != "" {
			out = append(out, n)
		}
	}
	return out
}
