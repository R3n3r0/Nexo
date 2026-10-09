// Package dist is the HTTP transport of the distributions (docs/10 §10.2):
// the routing of the requests by Host (HostRouter), the website routes
// (WebsiteRoutes), the HTTP-01 challenges of ACME (ChallengeRoutes), the
// metrics and the access log of every distribution.
//
// The HostRouter is the innermost middleware of a listener: it resolves
// the host the client addressed (the Host header, or the host a trusted
// proxy forwarded: middleware.HostOf) and
//
//   - serves the website distributions with the WebsiteRoutes (HTML or
//     plain text answers, never S3 XML);
//   - admits the requests of the s3 distributions (disabled, source
//     address, origin secret) and hands them to the S3 API of the
//     listener, which resolves the bucket of the host (S3BucketOfHost);
//   - answers 421 Misdirected Request to the unknown hosts when the
//     deployment asks for it, or on a listener serving the distributions
//     only (the web listeners, WithDistributionsOnly);
//   - leaves every other request to the routes of the listener.
//
// The handlers decode the request, call distribution.Website and encode
// its answer: the decisions belong to the service.
package dist

import (
	"context"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Routes registers a group of routes on a chi router.
type Routes = httpx.Routes

// WebsiteRoutes serves the requests of the website distributions: every
// method on every path of their hosts. ServeHTTP serves a request routed
// by the HostRouter (or resolves its host itself).
type WebsiteRoutes interface {
	Routes
	http.Handler
}

// HostRouter selects by Host what serves a request (see the package
// documentation).
type HostRouter interface {
	middleware.Middleware
}

// ChallengeRoutes serves the HTTP-01 challenges of ACME
// (/.well-known/acme-challenge/{token}), on every host.
type ChallengeRoutes interface {
	Routes
}

// AccessEntry is a line of the access log of a distribution.
type AccessEntry struct {
	Time         time.Time
	Distribution string
	// Format is distribution.LogFormatJSON or LogFormatCombined.
	Format string
	// Host is the host the client addressed, Alias the alias it matched.
	Host      string
	Alias     string
	Method    string
	URI       string
	Protocol  string
	Status    int
	Bytes     int64
	Duration  time.Duration
	ClientIP  string
	UserAgent string
	Referer   string
	RequestID string
}

// AccessLogger writes the access log of the distributions.
type AccessLogger interface {
	Log(ctx context.Context, e AccessEntry)
}
