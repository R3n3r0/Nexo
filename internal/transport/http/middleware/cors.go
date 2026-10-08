package middleware

import (
	"net/http"

	"github.com/go-chi/cors"
)

// CORSOptions configures the CORS middleware.
type CORSOptions struct {
	// AllowedOrigins lists the origins; "*" allows everyone (default).
	AllowedOrigins []string
	// AllowedMethods defaults to the S3 verbs.
	AllowedMethods []string
	// AllowedHeaders defaults to "*".
	AllowedHeaders []string
	// ExposedHeaders defaults to the ETag and x-amz-* headers S3 clients need.
	ExposedHeaders   []string
	AllowCredentials bool
	MaxAge           int
}

type corsMiddleware struct {
	inner func(http.Handler) http.Handler
}

var _ Middleware = (*corsMiddleware)(nil)

// NewCORS returns a CORS middleware based on github.com/go-chi/cors.
func NewCORS(o CORSOptions) Middleware {
	if len(o.AllowedOrigins) == 0 {
		o.AllowedOrigins = []string{"*"}
	}
	if len(o.AllowedMethods) == 0 {
		o.AllowedMethods = []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodOptions}
	}
	if len(o.AllowedHeaders) == 0 {
		o.AllowedHeaders = []string{"*"}
	}
	if len(o.ExposedHeaders) == 0 {
		o.ExposedHeaders = []string{"ETag", "Content-Length", "Content-Range", "Accept-Ranges", "x-amz-request-id", "x-amz-id-2", "x-amz-version-id", "x-amz-delete-marker"}
	}
	if o.MaxAge == 0 {
		o.MaxAge = 300
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
