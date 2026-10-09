package server_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
)

func TestServerStartServeStop(t *testing.T) {
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("pong"))
		})
		r.Get("/slow", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte("done"))
		})
	})
	header := middleware.Func(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test", "1")
			next.ServeHTTP(w, r)
		})
	})
	srv := server.New(server.Deps{
		Routes:      []httpx.Routes{routes, nil},
		Middlewares: []middleware.Middleware{header},
		NotFound: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "custom 404", http.StatusNotFound)
		}),
	}, server.WithAddr("127.0.0.1:0"), server.WithName("test"), server.WithReadHeaderTimeout(time.Second), server.WithShutdownTimeout(2*time.Second))

	assert.Equal(t, "127.0.0.1:0", srv.Addr())
	ctx := context.Background()
	require.NoError(t, srv.Start(ctx))
	assert.Error(t, srv.Start(ctx), "double start")
	addr := srv.Addr()
	assert.NotEqual(t, "127.0.0.1:0", addr)

	resp, err := http.Get("http://" + addr + "/ping")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "pong", string(body))
	assert.Equal(t, "1", resp.Header.Get("X-Test"))

	resp, err = http.Get("http://" + addr + "/missing")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, string(body), "custom 404")

	// Graceful shutdown waits for in-flight requests.
	done := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			done <- err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- string(b)
	}()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, srv.Stop(ctx))
	assert.Equal(t, "done", <-done)
	require.NoError(t, srv.Stop(ctx), "stop is idempotent")

	_, err = http.Get("http://" + addr + "/ping")
	assert.Error(t, err, "server must be closed")
}

func TestServerHandlerIsUsableWithoutStart(t *testing.T) {
	srv := server.New(server.Deps{Routes: []httpx.Routes{httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/x", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})}})
	assert.NotNil(t, srv.Handler())
	assert.Equal(t, ":9000", srv.Addr())
}

func TestServerListenError(t *testing.T) {
	first := server.New(server.Deps{}, server.WithAddr("127.0.0.1:0"))
	require.NoError(t, first.Start(context.Background()))
	defer func() { _ = first.Stop(context.Background()) }()
	second := server.New(server.Deps{}, server.WithAddr(first.Addr()))
	err := second.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen")

	bad := server.New(server.Deps{}, server.WithTLS("/nonexistent/cert.pem", "/nonexistent/key.pem"), server.WithAddr("127.0.0.1:0"))
	assert.Error(t, bad.Start(context.Background()))
}

// TestServerH2CStop: the cleartext HTTP/2 connections (prior knowledge, as
// the internode RPC dials them) are served and closed by Stop, so that a
// stopped node no longer answers on a connection opened before.
func TestServerH2CStop(t *testing.T) {
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/proto", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.Proto)) })
	})
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}}, server.WithAddr("127.0.0.1:0"), server.WithShutdownTimeout(5*time.Second))
	ctx := context.Background()
	require.NoError(t, srv.Start(ctx))
	addr := srv.Addr()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	get := func() (string, error) {
		resp, err := client.Get("http://" + addr + "/proto")
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	proto, err := get()
	require.NoError(t, err)
	assert.Equal(t, "HTTP/2.0", proto)

	start := time.Now()
	require.NoError(t, srv.Stop(ctx))
	t.Logf("stopped in %s", time.Since(start))
	_, err = get()
	assert.Error(t, err, "the open HTTP/2 connection is closed with the server")
}
