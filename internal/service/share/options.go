package share

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// defaultServerURL is the public URL used when WithServerURL is not given.
const defaultServerURL = "http://localhost:9000"

// Option configures the share service.
type Option func(*options)

type options struct {
	// serverURL is the public base URL of the S3 API.
	serverURL *url.URL
	// serverURLErr is the error of an invalid WithServerURL, reported by
	// Create and Presign.
	serverURLErr error
	// region is the signing region; empty means the signer's.
	region string
}

func defaultOptions() options {
	u, _ := url.Parse(defaultServerURL)
	return options{serverURL: u}
}

// WithServerURL sets the public URL of the S3 API the links point to
// (NEXO_SERVER_URL, e.g. "https://s3.example.com"; docs/09 §9.1): scheme
// http or https, a host, an optional base path, no query. Links use the
// path-style form <server>/<bucket>/<key>. An invalid URL makes Create and
// Presign fail with domain.ErrInvalidArgument.
func WithServerURL(raw string) Option {
	return func(o *options) {
		u, err := parseServerURL(raw)
		if err != nil {
			o.serverURLErr = err
			return
		}
		o.serverURL, o.serverURLErr = u, nil
	}
}

// WithRegion sets the region the links are signed for (default: the
// signer's region).
func WithRegion(region string) Option {
	return func(o *options) { o.region = region }
}

func parseServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: share: server URL %q: %w", domain.ErrInvalidArgument, raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("%w: share: server URL %q must be http(s)://host[:port][/path]", domain.ErrInvalidArgument, raw)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u, nil
}
