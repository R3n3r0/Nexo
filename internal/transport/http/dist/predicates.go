package dist

import (
	"context"
	"net/http"

	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// S3BucketOfHost returns the function telling the S3 API the bucket a host
// addresses as an alias of an s3 distribution (the custom S3 endpoint of
// a bucket, docs/10 §10.2 mode s3): the path of such a request is the
// object key.
func S3BucketOfHost(res distribution.Resolver) func(host string) (string, bool) {
	return func(host string) (string, bool) {
		route := res.Resolve(context.Background(), host)
		if route.Kind != distribution.RouteDistribution || route.Distribution == nil || route.Distribution.Mode != distribution.ModeS3 {
			return "", false
		}
		return route.Distribution.Origin.Bucket, true
	}
}

// WebsiteRequest returns the predicate selecting the requests addressed to
// a website distribution: the S3 authentication of the listener leaves
// them to the website routes (which verify the signatures of their
// authenticated behaviors and answer in plain text).
func WebsiteRequest(res distribution.Resolver) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		route := res.Resolve(r.Context(), middleware.HostOf(r))
		return route.Kind == distribution.RouteDistribution && route.Distribution != nil && route.Distribution.Mode == distribution.ModeWebsite
	}
}

// DistributionRequest returns the predicate selecting the requests
// addressed to any distribution.
func DistributionRequest(res distribution.Resolver) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		return res.Resolve(r.Context(), middleware.HostOf(r)).Kind == distribution.RouteDistribution
	}
}

// RefusedS3Request returns the predicate selecting the requests of an s3
// distribution that its admission refuses (disabled, source address,
// origin secret, denied): the S3 authentication of the listener skips
// them, so that a client bypassing the proxy gets the plain refusal of
// the distribution before any signature is parsed or verified (no
// credential oracle, no work), and the host router answers and records
// it (docs/10 §10.2: the origin secret precedes every other evaluation).
func RefusedS3Request(res distribution.Resolver, admission distribution.Website) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		route := res.Resolve(r.Context(), middleware.HostOf(r))
		if route.Kind != distribution.RouteDistribution || route.Distribution == nil || route.Distribution.Mode != distribution.ModeS3 {
			return false
		}
		return admission.Admit(r.Context(), webRequest(r, route)) != nil
	}
}
