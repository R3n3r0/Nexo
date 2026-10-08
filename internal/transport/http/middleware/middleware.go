// Package middleware defines the Middleware contract and the cross-cutting
// HTTP middlewares: request ids, panic recovery, logging, CORS, metrics,
// authentication and body limits.
package middleware

import (
	"net/http"
)

// Middleware wraps an http.Handler.
type Middleware interface {
	Wrap(next http.Handler) http.Handler
}

// Func adapts a function to Middleware.
type Func func(next http.Handler) http.Handler

// Wrap implements Middleware.
func (f Func) Wrap(next http.Handler) http.Handler { return f(next) }

// Chain applies the middlewares so that the first one is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			h = mws[i].Wrap(h)
		}
	}
	return h
}

// responseWriter records the status and the bytes written. It forwards
// Flush and exposes the wrapped writer through Unwrap so that
// http.ResponseController keeps working.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func wrapWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *responseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer (for http.ResponseController).
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Status returns the response status (200 when nothing was written yet).
func (w *responseWriter) Status() int { return w.status }
