package middleware

import (
	"net/http"

	"github.com/go-chi/cors"
)

// CORSDeps are the dependencies of the CORS middleware (none: the struct
// keeps the constructor shape of every middleware).
type CORSDeps struct{}

// CORSOption configures the CORS middleware.
type CORSOption func(*corsConfig)

// WithAllowedOrigins sets the allowed origins; "*" allows everyone (default).
func WithAllowedOrigins(origins ...string) CORSOption {
	return func(o *corsConfig) { o.AllowedOrigins = origins }
}

// WithAllowedMethods sets the allowed methods (default: the S3 verbs).
func WithAllowedMethods(methods ...string) CORSOption {
	return func(o *corsConfig) { o.AllowedMethods = methods }
}

// WithAllowedHeaders sets the allowed request headers (default "*").
func WithAllowedHeaders(headers ...string) CORSOption {
	return func(o *corsConfig) { o.AllowedHeaders = headers }
}

// WithExposedHeaders sets the response headers exposed to the browser
// (default: ETag, the content headers and the x-amz-* headers S3 clients need).
func WithExposedHeaders(headers ...string) CORSOption {
	return func(o *corsConfig) { o.ExposedHeaders = headers }
}

// WithAllowCredentials allows cookies and authorization headers.
func WithAllowCredentials(allow bool) CORSOption {
	return func(o *corsConfig) { o.AllowCredentials = allow }
}

// WithMaxAge sets how long, in seconds, preflight results are cached
// (default 300).
func WithMaxAge(seconds int) CORSOption {
	return func(o *corsConfig) { o.MaxAge = seconds }
}

// corsConfig is the CORS configuration; the go-chi/cors types stay inside
// NewCORS.
type corsConfig struct {
	AllowedOrigins   []string
	AllowedMethods   []string
	AllowedHeaders   []string
	ExposedHeaders   []string
	AllowCredentials bool
	MaxAge           int
}

type corsMiddleware struct {
	inner func(http.Handler) http.Handler
}

var _ Middleware = (*corsMiddleware)(nil)

// NewCORS returns a CORS middleware based on github.com/go-chi/cors.
func NewCORS(_ CORSDeps, opts ...CORSOption) Middleware {
	o := corsConfig{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"*"},
		ExposedHeaders: []string{"ETag", "Content-Length", "Content-Range", "Accept-Ranges", "x-amz-request-id", "x-amz-id-2", "x-amz-version-id", "x-amz-delete-marker"},
		MaxAge:         300,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &corsMiddleware{inner: cors.Handler(cors.Options{
		AllowedOrigins:   o.AllowedOrigins,
		AllowedMethods:   o.AllowedMethods,
		AllowedHeaders:   o.AllowedHeaders,
		ExposedHeaders:   o.ExposedHeaders,
		AllowCredentials: o.AllowCredentials,
		MaxAge:           o.MaxAge,
	})}
}

// Wrap implements Middleware.
func (m *corsMiddleware) Wrap(next http.Handler) http.Handler { return m.inner(next) }
