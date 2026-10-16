// Package middleware defines the Middleware contract and the cross-cutting
// HTTP middlewares: request ids, panic recovery, logging, CORS, metrics,
// authentication and body limits.
package middleware

import (
	"context"
	"io"
	"net/http"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Middleware wraps an http.Handler.
type Middleware interface {
	Wrap(next http.Handler) http.Handler
}

// Gate is the router of a listener that opens before its APIs exist: the
// listener of a cluster node serves the internode RPC to its peers while
// they bootstrap, the S3 and console APIs are composed after the
// bootstrap. While closed the gate answers the liveness probe and refuses
// the readiness probe and every other request with 503 (Retry-After: the
// SDKs retry); once opened it serves the composed handler. Close turns the
// new requests away again while the node stops, and Drain waits for the
// requests already admitted.
type Gate interface {
	Middleware
	http.Handler
	// Open serves h from now on.
	Open(h http.Handler)
	// Close turns the new requests away with reason.
	Close(reason string)
	// Drain waits until the requests admitted by the opened gate are
	// done, or ctx ends (its error). Close it first: the requests
	// admitted after Drain returns are not waited for.
	Drain(ctx context.Context) error
}

// GateDeps are the dependencies of the gate (none: the type keeps the
// constructor shape of docs/12 §12.3).
type GateDeps struct{}

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

// ReadFrom implements io.ReaderFrom: a file section (httpx.FileSection)
// goes on to the ReadFrom of the wrapped writer (sendfile, see httpx),
// counted; any other reader is copied through Write as io.Copy does.
func (w *responseWriter) ReadFrom(src io.Reader) (int64, error) {
	if _, ok := httpx.FileSection(src); !ok {
		return httpx.CopyPooled(struct{ io.Writer }{w}, src)
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := httpx.CopyPooled(w.ResponseWriter, src)
	w.bytes += n
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
