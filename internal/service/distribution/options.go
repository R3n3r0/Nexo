package distribution

import (
	"strings"
	"time"
)

// Option configures the distribution service.
type Option func(*options)

type options struct {
	// domains are the virtual-host domains (NEXO_DOMAIN).
	domains []string
	// mainHosts are the S3/console host names.
	mainHosts []string
	// misdirected answers 421 for the unknown hosts.
	misdirected bool
	lockTimeout time.Duration
	// maxDistributions bounds the number of distributions.
	maxDistributions int
}

func defaultOptions() options {
	return options{lockTimeout: 10 * time.Second, maxDistributions: 1000}
}

// WithDomains sets the virtual-host domains: "<bucket>.<domain>" resolves
// to the bucket (RouteVirtualHost), the domain itself to RouteMain.
func WithDomains(domains ...string) Option {
	return func(o *options) {
		for _, d := range domains {
			if d = normalizeHost(d); d != "" {
				o.domains = append(o.domains, d)
			}
		}
	}
}

// WithMainHosts sets the host names of the S3 API and of the console
// (NEXO_SERVER_URL, NEXO_BROWSER_REDIRECT_URL): they resolve to RouteMain
// and cannot be aliases of a distribution.
func WithMainHosts(hosts ...string) Option {
	return func(o *options) {
		for _, h := range hosts {
			if h = normalizeHost(h); h != "" {
				o.mainHosts = append(o.mainHosts, h)
			}
		}
	}
}

// WithUnknownHostMisdirected answers 421 Misdirected Request to the hosts
// that are neither an alias, a virtual host nor a main host, instead of
// serving them the path-style S3 API.
func WithUnknownHostMisdirected(on bool) Option {
	return func(o *options) { o.misdirected = on }
}

// WithLockTimeout bounds the wait for the configuration lock (default
// 10s).
func WithLockTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.lockTimeout = d
		}
	}
}

// WithMaxDistributions bounds the number of distributions (default 1000).
func WithMaxDistributions(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxDistributions = n
		}
	}
}

// WebsiteOption configures the website engine.
type WebsiteOption func(*websiteOptions)

type websiteOptions struct {
	// serverHeader is the Server header of the answers ("" for none).
	serverHeader string
}

// WithServerHeader sets the Server header of the website answers.
func WithServerHeader(v string) WebsiteOption {
	return func(o *websiteOptions) { o.serverHeader = strings.TrimSpace(v) }
}
