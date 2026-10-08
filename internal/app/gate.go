package app

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/domain"
)

// gate is the handler of a listener of a cluster node: the listener opens
// before the bootstrap (the peers reach this node's RPC server on the S3
// port while they bootstrap) but the APIs are composed after it. Until
// open is called the gate answers the liveness probe, refuses the
// readiness probe and every other request with 503 (Retry-After: the SDKs
// retry), then it serves the composed handler; close turns the requests
// away again while the node stops.
type gate struct {
	// s3 selects the S3 error document (XML) instead of a JSON problem.
	s3      bool
	handler atomic.Pointer[http.Handler]
	reason  atomic.Pointer[string]
}

func newGate(s3 bool, reason string) *gate {
	g := &gate{s3: s3}
	g.reason.Store(&reason)
	return g
}

// Wrap implements middleware.Middleware: the gate replaces the router of
// its listener.
func (g *gate) Wrap(http.Handler) http.Handler { return g }

// open serves h from now on.
func (g *gate) open(h http.Handler) { g.handler.Store(&h) }

// close turns the requests away with reason.
func (g *gate) close(reason string) {
	g.reason.Store(&reason)
	g.handler.Store(nil)
}

// ServeHTTP implements http.Handler.
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
