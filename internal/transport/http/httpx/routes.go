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
