package httpx

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// DefaultSendTimeout bounds how long a response body may stall: a write
// the client does not accept within it fails and the response is aborted.
// It is below the lock timeout of the object layer (30s), so that a writer
// waiting for the read lock of a download whose client stopped reading
// (an SDK holding an unread response, a stalled peer) gets it instead of
// timing out (Ceph s3-tests test_multipart_copy_versioned).
const DefaultSendTimeout = 20 * time.Second

// SendGuard is the writer of a streamed response body whose every write
// must make progress within a timeout: before each write the write
// deadline of the connection (of the stream, over HTTP/2) is moved to now
// plus the timeout. A client that reads slowly but steadily is never cut;
// one that stops reading is, which releases what the handler holds while
// streaming (the read lock of the object). Close flushes the body under
// the same bound and clears the deadline, so that the connection serves
// its next request unbounded. A ResponseWriter that cannot set deadlines
// (a test recorder) is written to unguarded.
type SendGuard struct {
	w           http.ResponseWriter
	rc          *http.ResponseController
	timeout     time.Duration
	armed       bool
	unsupported bool
}

// NewSendGuard returns the guard of w; a timeout <= 0 disables it.
func NewSendGuard(w http.ResponseWriter, timeout time.Duration) *SendGuard {
	return &SendGuard{w: w, rc: http.NewResponseController(w), timeout: timeout, unsupported: timeout <= 0}
}

// arm moves the write deadline to now plus the timeout.
func (g *SendGuard) arm() {
	if g.unsupported {
		return
	}
	if err := g.rc.SetWriteDeadline(time.Now().Add(g.timeout)); err != nil {
		g.unsupported = true
		return
	}
	g.armed = true
}

// Write writes p to the response within the timeout.
func (g *SendGuard) Write(p []byte) (int, error) {
	g.arm()
	return g.w.Write(p)
}

// Close flushes the buffered body within the timeout and clears the
// write deadline. It does not close the response.
func (g *SendGuard) Close() error {
	if !g.armed {
		return nil
	}
	g.arm()
	err := g.rc.Flush()
	if derr := g.rc.SetWriteDeadline(time.Time{}); err == nil {
		err = derr
	}
	g.armed = false
	return err
}

// CopyBody streams src to the body of w like io.Copy, each write bounded
// by timeout (see SendGuard).
func CopyBody(w http.ResponseWriter, src io.Reader, timeout time.Duration) (int64, error) {
	g := NewSendGuard(w, timeout)
	n, err := io.Copy(g, src)
	if cerr := g.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// sendBoundKey is the context key of the SendBound of a request.
type sendBoundKey struct{}

// SendBound is the stall bound the HTTP server puts on every response
// (server.WithSendTimeout): each write must make progress within the send
// timeout. The handlers of the authenticated streams whose pace is not
// the client's own lift it (LiftSendBound): the internode RPC, whose
// shard streams follow the pace of the S3 client downstream.
type SendBound struct{ lifted atomic.Bool }

// WithSendBound returns ctx carrying a new bound, and the bound.
func WithSendBound(ctx context.Context) (context.Context, *SendBound) {
	b := &SendBound{}
	return context.WithValue(ctx, sendBoundKey{}, b), b
}

// Lifted reports a bound lifted by the handler (pure accessor).
func (b *SendBound) Lifted() bool { return b != nil && b.lifted.Load() }

// LiftSendBound lifts the stall bound of the response of the request of
// ctx; a request without one is left alone. Only an authenticated handler
// calls it: an anonymous client that stops reading must keep being cut.
func LiftSendBound(ctx context.Context) {
	if b, ok := ctx.Value(sendBoundKey{}).(*SendBound); ok {
		b.lifted.Store(true)
	}
}
