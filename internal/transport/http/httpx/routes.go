// Package httpx holds the HTTP helpers shared by every transport: response
// encoding (XML, JSON, problem+json, S3 errors), the S3 error table,
// request-id generation and x-amz-* header helpers.
package httpx

import "github.com/go-chi/chi/v5"

// Routes registers a group of routes on a router. Every route group of the
// S3, admin and console transports implements it.
type Routes interface {
	Register(r chi.Router)
}

// RoutesFunc adapts a function to Routes.
type RoutesFunc func(r chi.Router)

// Register implements Routes.
func (f RoutesFunc) Register(r chi.Router) { f(r) }

// HostBucketResolver returns the bucket a host addresses as a custom S3
// endpoint (the alias of a distribution in the s3 mode, docs/10 §10.2):
// the path of such a request is the object key, as for a virtual host,
// except under the reserved "/nexo/" prefix (domain.SystemPathPrefix),
// which stays the server's. The host is the one the client addressed
// (middleware.HostOf), with its port. The S3 API, its lifecycle and
// notification middlewares and the target middleware resolve the requests
// with the same resolver.
type HostBucketResolver interface {
	// BucketOfHost is a pure accessor (a lookup in the routes the node
	// holds in memory: no I/O, never blocks).
	BucketOfHost(host string) (bucket string, ok bool)
}

// HostBucketFunc adapts a function to HostBucketResolver.
type HostBucketFunc func(host string) (bucket string, ok bool)

// BucketOfHost implements HostBucketResolver.
func (f HostBucketFunc) BucketOfHost(host string) (string, bool) { return f(host) }
