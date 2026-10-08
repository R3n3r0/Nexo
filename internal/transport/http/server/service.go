// Package server implements the HTTP server service: it owns the listener,
// optional TLS, HTTP/2, graceful shutdown and the chi router on which the
// registered Routes and Middlewares are mounted.
package server

import (
	"net/http"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Service is a managed HTTP server. Addr and Handler are pure accessors (no
// I/O, never block): the documented exception to the context rule.
type Service interface {
	lifecycle.Runner
	// Addr returns the address the server listens on. Before Start it is
	// the configured address; after Start the actual one (useful with :0).
	Addr() string
	// Handler returns the fully composed handler (middlewares + routes).
	Handler() http.Handler
}
