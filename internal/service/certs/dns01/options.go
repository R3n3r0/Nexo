package dns01

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ErrInvalidConfig reports a provider configuration refused by its
// constructor.
var ErrInvalidConfig = fmt.Errorf("invalid DNS provider configuration: %w", domain.ErrInvalidArgument)

// Settings are the settings common to the providers.
type Settings struct {
	// TTL is the TTL of the challenge records, in seconds (default 60).
	TTL int
	// PropagationTimeout bounds the wait of the records on the
	// nameservers (default 2m); PollingInterval is the delay between two
	// checks (default 4s).
	PropagationTimeout time.Duration
	PollingInterval    time.Duration
}

func (s Settings) ttl() int {
	if s.TTL <= 0 {
		return DefaultTTL
	}
	return s.TTL
}

func (s Settings) propagation() (time.Duration, time.Duration) {
	timeout, interval := s.PropagationTimeout, s.PollingInterval
	if timeout <= 0 {
		timeout = DefaultPropagationTimeout
	}
	if interval <= 0 {
		interval = DefaultPollingInterval
	}
	return timeout, interval
}

// Option configures a provider.
type Option func(*options)

type options struct {
	httpClient *http.Client
	// customHTTP reports a client set with WithHTTPClient.
	customHTTP bool
	endpoint   string
}

// WithHTTPClient sets the HTTP client of the API providers (default: a
// client with a 30s timeout).
func WithHTTPClient(c *http.Client) Option {
	return func(o *options) {
		if c != nil {
			o.httpClient, o.customHTTP = c, true
		}
	}
}

// WithEndpoint sets the base URL of the API of a provider (a regional or
// private endpoint, tests).
func WithEndpoint(url string) Option {
	return func(o *options) { o.endpoint = strings.TrimRight(strings.TrimSpace(url), "/") }
}

func buildOptions(opts []Option) options {
	o := options{httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// unFqdn drops the trailing dot of a name.
func unFqdn(name string) string { return strings.TrimSuffix(name, ".") }

// parentNames returns the name of fqdn and its parents, up to the second
// level ("a.b.example.com." gives a.b.example.com, b.example.com,
// example.com): the candidate zones of a record.
func parentNames(fqdn string) []string {
	var out []string
	labels := dns.SplitDomainName(fqdn)
	for i := 0; i < len(labels)-1; i++ {
		out = append(out, strings.ToLower(strings.Join(labels[i:], ".")))
	}
	return out
}
