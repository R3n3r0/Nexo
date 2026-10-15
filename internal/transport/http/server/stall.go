package server

import (
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// stallGuard bounds the stalls of every response of the server: each
// write (and flush) of the handler must make progress within timeout, or
// it fails and the response is aborted (over HTTP/2 the stream is reset).
// A client that stops reading (an HTTP/2 client announcing a zero flow
// control window, a socket nobody drains) thus releases within timeout the
// handler, what it holds (the read lock of an object, open files) and the
// request body the server buffered for it. A client reading slowly but
// steadily is never cut, and the deadline is cleared between the writes,
// so a stream idle between two events (server-sent events) is not cut
// either. The authenticated handlers of streams whose pace is not the
// client's lift the bound (httpx.LiftSendBound).
func stallGuard(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, bound := httpx.WithSendBound(r.Context())
		g := &stallWriter{ResponseWriter: w, rc: http.NewResponseController(w), timeout: timeout, bound: bound}
		http1 := r.ProtoMajor < 2
		if http1 {
			// The deadline the previous response of the connection
			// left armed (see finish).
			g.clear()
		}
		next.ServeHTTP(g, r.WithContext(ctx))
		g.finish(http1)
	})
}

// stallWriter is the ResponseWriter of stallGuard. It exposes the wrapped
// writer (http.ResponseController) and no SetWriteDeadline of its own: the
// handlers that move the deadline themselves (httpx.SendGuard) reach the
// connection directly.
type stallWriter struct {
	http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	bound       *httpx.SendBound
	wrote       bool
	unsupported bool
}

// arm moves the write deadline to now plus the timeout; false when the
// bound is lifted or the writer cannot set deadlines (a test recorder, a
// hijacked connection).
func (g *stallWriter) arm() bool {
	if g.unsupported || g.bound.Lifted() {
		return false
	}
	if err := g.rc.SetWriteDeadline(time.Now().Add(g.timeout)); err != nil {
		g.unsupported = true
		return false
	}
	return true
}

// clear removes the write deadline.
func (g *stallWriter) clear() {
	if g.unsupported {
		return
	}
	if err := g.rc.SetWriteDeadline(time.Time{}); err != nil {
		g.unsupported = true
	}
}

// Write implements http.ResponseWriter within the timeout.
func (g *stallWriter) Write(p []byte) (int, error) {
	g.wrote = true
	if g.arm() {
		defer g.clear()
	}
	return g.ResponseWriter.Write(p)
}

// Flush implements http.Flusher within the timeout.
func (g *stallWriter) Flush() { _ = g.FlushError() }

// FlushError flushes within the timeout (http.ResponseController).
func (g *stallWriter) FlushError() error {
	if g.arm() {
		defer g.clear()
	}
	return g.rc.Flush()
}

// Unwrap exposes the wrapped writer (http.ResponseController).
func (g *stallWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// finish flushes within the timeout what the handler left buffered. Over
// HTTP/1 the deadline stays armed for the last bytes the server writes
// after the handler (the end of a chunked body), and the next request of
// the connection clears it; an HTTP/2 stream ends with a frame that needs
// no flow control.
func (g *stallWriter) finish(http1 bool) {
	if !g.wrote || !g.arm() {
		return
	}
	_ = g.rc.Flush()
	if !http1 {
		g.clear()
	}
}
