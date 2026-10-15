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

	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// Test credentials shared by the nodes of a test deployment.
const (
	rootUser     = "nexoadmin"
	rootPassword = "nexoadmin-secret"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testDeployment is the deployment of the test nodes.
const testDeployment = "dep-test"

// newAuth returns the authenticator of a node of the test deployment
// (rpc.WithDeploymentID("") makes a node that does not know it yet).
func newAuth(t testing.TB, node string, opts ...rpc.AuthOption) rpc.Authenticator {
	t.Helper()
	base := []rpc.AuthOption{rpc.WithCredentials(rootUser, rootPassword), rpc.WithNodeID(node), rpc.WithDeploymentID(testDeployment)}
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
	srv := h2cServer(handler, &http.HTTP2Config{MaxReceiveBufferPerStream: 16 << 20, MaxReceiveBufferPerConnection: 64 << 20, MaxReadFrameSize: 1 << 20})
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// startRPC starts an RPC server with the routes on a fresh port and
// returns its address, which is its node id (the audience of the tokens
// of its callers).
func startRPC(t testing.TB, routes ...rpc.Routes) (string, *httptest.Server) {
	t.Helper()
	ln := listen(t)
	addr := ln.Addr().String()
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr), Routes: routes, Logger: discard()})
	require.NoError(t, err)
	return addr, serve(t, ln, s)
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
