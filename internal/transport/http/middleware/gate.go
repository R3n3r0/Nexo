package middleware

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/domain"
)

// GateOption configures a gate.
type GateOption func(*gate)

// WithS3Errors makes the gate answer with S3 error documents (XML) instead
// of JSON problems (RFC 9457): the gate of the S3 listener.
func WithS3Errors() GateOption { return func(g *gate) { g.s3 = true } }

// WithClosedReason sets the reason of the 503 answers until the gate opens
// (default "the server is starting").
func WithClosedReason(reason string) GateOption {
	return func(g *gate) { g.reason.Store(&reason) }
}

type gate struct {
	s3      bool
	handler atomic.Pointer[http.Handler]
	reason  atomic.Pointer[string]

	// mu guards inflight and idle: the requests in progress and the
	// channel closed when they are all done.
	mu       sync.Mutex
	inflight int
	idle     chan struct{}
}

var _ Gate = (*gate)(nil)

// NewGate returns a closed gate.
func NewGate(_ GateDeps, opts ...GateOption) Gate {
	g := &gate{}
	reason := "the server is starting"
	g.reason.Store(&reason)
	for _, o := range opts {
		o(g)
	}
	return g
}

// Wrap implements Middleware: the gate replaces the router of its
// listener.
func (g *gate) Wrap(http.Handler) http.Handler { return g }

// Open implements Gate.
func (g *gate) Open(h http.Handler) { g.handler.Store(&h) }

// Close implements Gate.
func (g *gate) Close(reason string) {
	g.reason.Store(&reason)
	g.handler.Store(nil)
}

// Drain implements Gate.
func (g *gate) Drain(ctx context.Context) error {
	g.mu.Lock()
	if g.inflight == 0 {
		g.mu.Unlock()
		return nil
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// admit counts a request in progress.
func (g *gate) admit() {
	g.mu.Lock()
	g.inflight++
	g.mu.Unlock()
}

// done ends a request counted by admit.
func (g *gate) done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight--
	if g.inflight == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// ServeHTTP implements http.Handler.
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Counted before the handler is loaded: a Drain after Close waits
	// for every request that saw the gate open.
	g.admit()
	defer g.done()
	if h := g.handler.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	reason := *g.reason.Load()
	if r.URL.Path == domain.HealthLivePath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	w.Header().Set("Retry-After", "1")
	if g.s3 && r.URL.Path != domain.HealthReadyPath {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, xml.Header+"<Error><Code>XNexoServerNotInitialized</Code><Message>")
		_ = xml.EscapeText(w, []byte(reason))
		_, _ = io.WriteString(w, "</Message><Resource>")
		_ = xml.EscapeText(w, []byte(r.URL.Path))
		_, _ = io.WriteString(w, "</Resource></Error>")
		return
	}
	body, _ := json.Marshal(map[string]any{
		"type": "about:blank", "title": "Service Unavailable", "status": http.StatusServiceUnavailable,
		"code": "server_not_initialized", "detail": reason,
	})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}
