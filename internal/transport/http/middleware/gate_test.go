package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestGateClosed(t *testing.T) {
	tests := []struct {
		name        string
		opts        []middleware.GateOption
		path        string
		status      int
		contentType string
		body        string
	}{
		{name: "liveness", opts: []middleware.GateOption{middleware.WithS3Errors()}, path: domain.HealthLivePath,
			status: http.StatusOK, contentType: "text/plain; charset=utf-8", body: "ok\n"},
		{name: "readiness", opts: []middleware.GateOption{middleware.WithS3Errors(), middleware.WithClosedReason("bootstrap")},
			path: domain.HealthReadyPath, status: http.StatusServiceUnavailable, contentType: "application/problem+json", body: `"detail":"bootstrap"`},
		{name: "s3", opts: []middleware.GateOption{middleware.WithS3Errors(), middleware.WithClosedReason("bootstrap <1>")},
			path: "/bkt/key", status: http.StatusServiceUnavailable, contentType: "application/xml",
			body: "<Code>XNexoServerNotInitialized</Code><Message>bootstrap &lt;1&gt;</Message><Resource>/bkt/key</Resource>"},
		{name: "console", path: "/api/v1/info", status: http.StatusServiceUnavailable, contentType: "application/problem+json",
			body: `"code":"server_not_initialized","detail":"the server is starting"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := middleware.NewGate(middleware.GateDeps{}, tc.opts...)
			rec := serve(g, tc.path)
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, tc.contentType, rec.Header().Get("Content-Type"))
			assert.Contains(t, rec.Body.String(), tc.body)
			if tc.status == http.StatusServiceUnavailable {
				assert.Equal(t, "1", rec.Header().Get("Retry-After"), "the SDKs retry")
			}
		})
	}
}

func TestGateOpenClose(t *testing.T) {
	g := middleware.NewGate(middleware.GateDeps{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	assert.Same(t, g, g.Wrap(inner), "the gate replaces the router")
	g.Open(inner)
	assert.Equal(t, http.StatusTeapot, serve(g, "/x").Code)
	g.Close("the node is stopping")
	rec := serve(g, "/x")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "the node is stopping")
}

// TestGateDrain: Drain waits for the requests admitted while the gate was
// open (a node stops its background components only after them), and
// gives up when its context ends.
func TestGateDrain(t *testing.T) {
	g := middleware.NewGate(middleware.GateDeps{})
	require.NoError(t, g.Drain(context.Background()), "nothing in progress")
	started, release := make(chan struct{}), make(chan struct{})
	g.Open(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	served := make(chan int, 1)
	go func() { served <- serve(g, "/slow").Code }()
	<-started
	g.Close("stopping")
	assert.Equal(t, http.StatusServiceUnavailable, serve(g, "/new").Code, "new requests are turned away")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, g.Drain(ctx), context.DeadlineExceeded, "the slow request is still in progress")

	drained := make(chan error, 1)
	go func() { drained <- g.Drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned before the request ended: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-drained)
	assert.Equal(t, http.StatusNoContent, <-served)
}
