package rpc_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// Test credentials shared by the nodes of a test deployment.
const (
	rootUser     = "nexoadmin"
	rootPassword = "nexoadmin-secret"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newAuth returns the authenticator of a node of the test deployment.
func newAuth(t testing.TB, node string, opts ...rpc.AuthOption) rpc.Authenticator {
	t.Helper()
	base := []rpc.AuthOption{rpc.WithCredentials(rootUser, rootPassword), rpc.WithNodeID(node)}
	a, err := rpc.NewAuthenticator(rpc.AuthDeps{Logger: discard()}, append(base, opts...)...)
	require.NoError(t, err)
	return a
}

// listen reserves a loopback port for a node.
func listen(t testing.TB) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}

// listenOn listens again on the address of a stopped node.
func listenOn(addr string) (net.Listener, error) {
	var err error
	for range 50 {
		var ln net.Listener
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

// serve serves handler with cleartext HTTP/2 on ln, like the S3 server.
func serve(t testing.TB, ln net.Listener, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h2c.NewHandler(handler, &http2.Server{}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// startRPC starts an RPC server with the routes on a fresh port and
// returns its address.
func startRPC(t testing.TB, auth rpc.Authenticator, routes ...rpc.Routes) (string, *httptest.Server) {
	t.Helper()
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: auth, Routes: routes, Logger: discard()})
	require.NoError(t, err)
	ln := listen(t)
	return ln.Addr().String(), serve(t, ln, s)
}

// dial returns a connection to node.
func dial(t testing.TB, auth rpc.Authenticator, node string, opts ...rpc.ConnOption) rpc.Conn {
	t.Helper()
	base := []rpc.ConnOption{rpc.WithHealthInterval(20*time.Millisecond, 100*time.Millisecond), rpc.WithCallTimeout(10 * time.Second)}
	c, err := rpc.NewConn(rpc.ConnDeps{Auth: auth, Logger: discard()}, node, append(base, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}
