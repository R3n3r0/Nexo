package rpc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
	rpcmock "github.com/r3n3r0/nexo/internal/transport/rpc/mock"
)

// testRoutes are routes of a test service, registered through the
// generated Routes mock.
func testRoutes(t *testing.T, register func(r chi.Router)) rpc.Routes {
	t.Helper()
	m := rpcmock.NewRoutes(t)
	m.EXPECT().Register(mock.Anything).Run(register).Once()
	return m
}

func TestConnCalls(t *testing.T) {
	ctx := context.Background()
	var slow atomic.Int64
	routes := testRoutes(t, func(r chi.Router) {
		r.Post("/test/echo", func(w http.ResponseWriter, r *http.Request) {
			if d := time.Duration(slow.Load()); d > 0 {
				time.Sleep(d)
			}
			caller, _ := rpc.CallerFromContext(r.Context())
			w.Header().Set("Content-Type", "application/x-msgpack")
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body) // msgpack in, msgpack out
			_ = caller
		})
		r.Post("/test/garbage", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		})
	})
	addr, _ := startRPC(t, routes)
	conn := dial(t, newAuth(t, "client:9000"), addr)
	assert.Equal(t, addr, conn.Node())

	var out string
	require.NoError(t, conn.Call(ctx, "test/echo", "hello", &out))
	assert.Equal(t, "hello", out)
	require.NoError(t, conn.Call(ctx, "test/echo", "discarded", nil))

	// Unknown routes and malformed answers.
	err := conn.Call(ctx, "test/missing", "x", nil)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	err = conn.Call(ctx, "test/garbage", "x", nil)
	assert.ErrorIs(t, err, rpc.ErrProtocol)
	assert.True(t, conn.IsOnline(), "answers, even errors, keep the circuit closed")

	// A slow node times out without opening the circuit.
	slow.Store(int64(300 * time.Millisecond))
	err = conn.Call(ctx, "test/echo", "x", nil, rpc.WithTimeout(50*time.Millisecond))
	assert.ErrorIs(t, err, domain.ErrNodeOffline)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, conn.IsOnline())
	slow.Store(0)

	// The caller's context wins over the transport error.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, conn.Call(canceled, "test/echo", "x", nil), context.Canceled)

	require.NoError(t, conn.Close(ctx))
	assert.False(t, conn.IsOnline())
	assert.ErrorIs(t, conn.Call(ctx, "test/echo", "x", nil), domain.ErrNodeOffline)
}

// TestConnAuthentication: calls signed with other credentials are refused
// and the probe reports why the node refuses us.
func TestConnAuthentication(t *testing.T) {
	ctx := context.Background()
	addr, _ := startRPC(t)
	other, err := rpc.NewAuthenticator(rpc.AuthDeps{Logger: discard()}, rpc.WithCredentials(rootUser, "other-secret"), rpc.WithNodeID("intruder:9000"))
	require.NoError(t, err)
	conn := dial(t, other, addr)
	var reply struct{}
	err = conn.Call(ctx, "peer/ping", struct{}{}, &reply)
	assert.ErrorIs(t, err, rpc.ErrInvalidToken)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

// TestServerWrap: requests outside of the RPC prefix go to the next
// handler, the S3 API of the same port.
func TestServerWrap(t *testing.T) {
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, "n:1"), Logger: discard()})
	require.NoError(t, err)
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("s3")) }))
	for path, want := range map[string]int{"/bucket/key": http.StatusOK, "/nexo/rpc/v1/peer/ping": http.StatusUnauthorized, "/nexo/rpc/v1": http.StatusOK} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.ProtoMajor, req.ProtoMinor = 2, 0
		h.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, path)
	}
	// The internode calls are HTTP/2 only: an HTTP/1 request (a reverse
	// proxy, a replayed request) is refused before its token is read.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/nexo/rpc/v1/peer/ping", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/elsewhere", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	_, err = rpc.NewServer(rpc.ServerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = rpc.NewConn(rpc.ConnDeps{}, "n:1")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = rpc.NewConn(rpc.ConnDeps{Auth: newAuth(t, "n:1")}, "no-port")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestConnTLS: with TLS the calls use HTTPS (HTTP/2 negotiated by ALPN).
func TestConnTLS(t *testing.T) {
	ctx := context.Background()
	ln := listen(t)
	addr := ln.Addr().String()
	server, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr), Logger: discard()})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(server)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	conn := dial(t, newAuth(t, "client:9000"), addr, rpc.WithTLSConfig(cfg))
	var pong struct {
		Node     string
		Protocol int
	}
	require.NoError(t, conn.Call(ctx, "health/ping", struct{}{}, &pong))
	assert.Equal(t, addr, pong.Node)
	assert.Equal(t, rpc.ProtocolVersion, pong.Protocol)
}

// TestConnDialer: WithDialer routes the connections of the node (the
// cluster tests put proxies on the links between the nodes); TLS runs
// over the connections it returns.
func TestConnDialer(t *testing.T) {
	ctx := context.Background()
	addr, _ := startRPC(t)
	var dialed atomic.Int32
	var last atomic.Value
	via := func(ctx context.Context, network, target string) (net.Conn, error) {
		dialed.Add(1)
		last.Store(target)
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	conn := dial(t, newAuth(t, "client:9000"), addr, rpc.WithDialer(via))
	var pong struct{ Node string }
	require.NoError(t, conn.Call(ctx, "health/ping", struct{}{}, &pong))
	assert.Equal(t, addr, pong.Node)
	assert.Positive(t, dialed.Load())
	assert.Equal(t, addr, last.Load(), "the dialer receives the address of the node")

	cut := func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("link cut") }
	blocked := dial(t, newAuth(t, "client:9000"), addr, rpc.WithDialer(cut))
	assert.Error(t, blocked.Call(ctx, "health/ping", struct{}{}, &pong))

	// TLS over the dialed connection.
	ln := listen(t)
	tlsAddr := ln.Addr().String()
	server, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, tlsAddr), Logger: discard()})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(server)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	secure := dial(t, newAuth(t, "client:9000"), tlsAddr, rpc.WithTLSConfig(cfg), rpc.WithDialer(via))
	require.NoError(t, secure.Call(ctx, "health/ping", struct{}{}, &pong))
	assert.Equal(t, tlsAddr, pong.Node)
	assert.Equal(t, tlsAddr, last.Load())
}

// TestConnStreams covers the streamed calls beyond the storage suite:
// early answers of uploads, a reader that stops early, cancellation.
func TestConnStreams(t *testing.T) {
	ctx := context.Background()
	routes := testRoutes(t, func(r chi.Router) {
		r.Post("/test/reject", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-msgpack")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte{0x81, 0xa1, 'c', 0xad, 'a', 'c', 'c', 'e', 's', 's', '_', 'd', 'e', 'n', 'i', 'e', 'd'})
		})
	})
	addr, _ := startRPC(t, routes)
	conn := dial(t, newAuth(t, "client:9000"), addr)

	// The node refuses before reading the upload.
	big := bytes.NewReader(bytes.Repeat([]byte("x"), 8<<20))
	err := conn.Upload(ctx, "test/reject", "args", big, nil)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = conn.Download(ctx, "test/reject", "args")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = conn.Stream(ctx, "test/reject", "args")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = conn.Download(ctx, "test/none", "args")
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	assert.True(t, strings.Contains(err.Error(), addr))
}

// TestLockOverRPC runs the distributed locker over the lock routes of three
// nodes.
func TestLockOverRPC(t *testing.T) {
	ctx := context.Background()
	var servers []lock.Server
	var local []lock.Server
	for range 3 {
		s := lock.NewServer(lock.ServerDeps{Logger: discard()})
		local = append(local, s)
		routes, err := rpc.NewLockRoutes(rpc.LockRoutesDeps{Server: s})
		require.NoError(t, err)
		addr, _ := startRPC(t, routes)
		client, err := rpc.NewLockClient(rpc.LockClientDeps{Conn: dial(t, newAuth(t, "client:9000"), addr)})
		require.NoError(t, err)
		servers = append(servers, client)
	}
	l, err := lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: "client:9000", Logger: discard()},
		lock.WithRetryInterval(5*time.Millisecond, 20*time.Millisecond))
	require.NoError(t, err)
	lc, err := l.NewNSLock(ctx, "b", "o").GetLock(ctx, time.Second)
	require.NoError(t, err)
	// The acquisition returns at the quorum; the last grant follows.
	m := lock.NewManager(lock.ManagerDeps{Servers: servers, Logger: discard()})
	require.Eventually(t, func() bool {
		list, err := m.List(ctx)
		return err == nil && len(list) == 1 && list[0].Servers == 3
	}, 5*time.Second, 5*time.Millisecond)
	for _, s := range local {
		infos, err := s.List(ctx)
		require.NoError(t, err)
		require.Len(t, infos, 1)
		assert.Equal(t, "client:9000", infos[0].Owner)
	}
	lc.Unlock(ctx)
	list, err := m.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
	require.NoError(t, m.ForceUnlock(ctx, "b/o"))

	_, err = rpc.NewLockRoutes(rpc.LockRoutesDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = rpc.NewLockClient(rpc.LockClientDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
