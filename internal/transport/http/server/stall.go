package server

import (
	"context"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// The pieces of the writes of an HTTP/2 stream (stallWriter.piece): the
// streams of a connection share its bandwidth in turns, so the progress of
// a stream is bounded piece by piece, a piece being the share of
// h2MinRate bytes per second the stream gets in half the timeout, at least
// h2MinPiece (the chunk net/http buffers the writes of a stream in). With
// the default timeout (20s), a client that reads steadily at least about
// 40 KB/s from a connection has none of its streams cut, up to the 128 a
// connection may open; a connection that serves one request at a time
// keeps whole writes.
const (
	h2MinPiece = 4 << 10
	h2MinRate  = 32 << 10
)

// connRequestsKey is the context key of the connRequests of a connection.
type connRequestsKey struct{}

// connRequests counts the requests a connection is serving (stallGuard).
type connRequests struct{ n atomic.Int64 }

// withConnRequests returns the context of a new connection (ConnContext of
// the server), carrying the count of its requests.
func withConnRequests(ctx context.Context) context.Context {
	return context.WithValue(ctx, connRequestsKey{}, &connRequests{})
}

// stallGuard bounds the stalls of every response of the server: each
// write (and flush) of the handler must make progress within timeout, or
// it fails and the response is aborted (over HTTP/2 the stream is reset).
// A client that stops reading thus releases within timeout the handler,
// what it holds (the read lock of an object, open files) and the request
// body the server buffered for it: an HTTP/1 client or an HTTP/2 stream
// with a zero flow control window here, an HTTP/2 client that stops
// draining its socket (where the reset of a stream cannot be written) by
// the write timeout of the whole connection (httpServer). A client reading
// slowly but steadily is never cut: over HTTP/2 the writes are split in
// pieces sized on the streams sharing the connection, each with the
// timeout to progress. The deadline is cleared between the writes, so a
// stream idle between two events (server-sent events) is not cut either.
// The authenticated handlers of streams whose pace is not the client's
// lift the bound (httpx.LiftSendBound).
func stallGuard(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, bound := httpx.WithSendBound(r.Context())
		http1 := r.ProtoMajor < 2
		g := &stallWriter{ResponseWriter: w, rc: http.NewResponseController(w), timeout: timeout, bound: bound}
		if http1 {
			// The deadline the previous response of the connection
			// left armed (see finish).
			g.clear()
		} else {
			g.http2 = true
			if g.requests, _ = ctx.Value(connRequestsKey{}).(*connRequests); g.requests != nil {
				g.requests.n.Add(1)
				defer g.requests.n.Add(-1)
			}
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
	// armed is when the deadline was last moved.
	armed time.Time
	// http2 marks an HTTP/2 stream; requests counts the requests of its
	// connection (nil when unknown).
	http2    bool
	requests *connRequests
}

// piece returns the largest piece of a write handed at once to the
// HTTP/2 stream: the share of h2MinRate the stream gets in half the
// timeout (its connection serving the counted requests in turns), at least
// h2MinPiece; h2MinPiece when the requests of the connection are unknown.
func (g *stallWriter) piece() int {
	if g.requests == nil {
		return h2MinPiece
	}
	share := g.timeout.Seconds() * h2MinRate / 2 / float64(max(1, g.requests.n.Load()))
	if share >= math.MaxInt32 {
		return math.MaxInt32
	}
	return max(h2MinPiece, int(share))
}

// arm moves the write deadline to now plus the timeout; false when the
// bound is lifted or the writer cannot set deadlines (a test recorder, a
// hijacked connection).
func (g *stallWriter) arm() bool {
	if g.unsupported || g.bound.Lifted() {
		return false
	}
	now := time.Now()
	if err := g.rc.SetWriteDeadline(now.Add(g.timeout)); err != nil {
		g.unsupported = true
		return false
	}
	g.armed = now
	return true
}

// rearm moves the deadline of a write in progress once a quarter of the
// timeout has passed since it was armed: every piece of the write has at
// least three quarters of the timeout to progress, for one update of the
// deadline per quarter rather than per piece.
func (g *stallWriter) rearm() {
	if time.Since(g.armed) >= g.timeout/4 {
		g.arm()
	}
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

// Write implements http.ResponseWriter within the timeout: over HTTP/2
// piece by piece (see piece).
func (g *stallWriter) Write(p []byte) (int, error) {
	g.wrote = true
	if !g.arm() {
		return g.ResponseWriter.Write(p)
	}
	defer g.clear()
	if !g.http2 {
		return g.ResponseWriter.Write(p)
	}
	n := 0
	for n < len(p) {
		if n > 0 {
			g.rearm()
		}
		m, err := g.ResponseWriter.Write(p[n:min(len(p), n+g.piece())])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
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
