package rpc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/net/http2"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

func TestAuthenticator(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	auth := func(node string, opts ...rpc.AuthOption) rpc.Authenticator {
		base := []rpc.AuthOption{rpc.WithCredentials(rootUser, rootPassword), rpc.WithNodeID(node), rpc.WithBuildVersion("v1.4.0")}
		a, err := rpc.NewAuthenticator(rpc.AuthDeps{Clock: clk, Logger: discard()}, append(base, opts...)...)
		require.NoError(t, err)
		return a
	}
	alice, bob := auth("alice:9000"), auth("bob:9000")
	assert.Equal(t, "alice:9000", alice.NodeID())
	sum := sha256.Sum256([]byte("arguments"))
	call := rpc.CallBinding{Method: http.MethodPost, Target: "/nexo/rpc/v1/storage/read-all?drive=%2Fd1", BodyHash: sum[:]}

	token, err := alice.Token(ctx, "bob:9000", call)
	require.NoError(t, err)
	claims, err := bob.Verify(ctx, token, call)
	require.NoError(t, err)
	assert.Equal(t, rpc.Claims{NodeID: "alice:9000", Protocol: rpc.ProtocolVersion, Version: "v1.4.0", IssuedAt: now, ExpiresAt: now.Add(rpc.DefaultTokenTTL),
		Unverified: true, BodyHash: sum[:]}, claims, "bob does not know its deployment: the one of the caller cannot be checked")
	_, err = bob.Verify(ctx, token, call)
	assert.ErrorIs(t, err, rpc.ErrInvalidToken, "a token is accepted once")
	again, err := alice.Token(ctx, "bob:9000", call)
	require.NoError(t, err)
	assert.NotEqual(t, token, again, "every call gets its own token")
	_, err = alice.Token(ctx, "", call)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a token is issued for a node")
	_, err = alice.Token(ctx, "bob:9000", rpc.CallBinding{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a token is issued for a call")

	// A token is only valid for its call: other method, target or
	// arguments are refused (and do not consume it).
	other := sha256.Sum256([]byte("other arguments"))
	for name, c := range map[string]rpc.CallBinding{
		"method":    {Method: http.MethodGet, Target: call.Target, BodyHash: call.BodyHash},
		"target":    {Method: call.Method, Target: "/nexo/rpc/v1/storage/write-all?drive=%2Fd1", BodyHash: call.BodyHash},
		"query":     {Method: call.Method, Target: "/nexo/rpc/v1/storage/read-all?drive=%2Fd2", BodyHash: call.BodyHash},
		"arguments": {Method: call.Method, Target: call.Target, BodyHash: other[:]},
	} {
		_, err := bob.Verify(ctx, again, c)
		assert.ErrorIs(t, err, rpc.ErrInvalidToken, name)
	}
	unchecked, err := bob.Verify(ctx, again, rpc.CallBinding{Method: call.Method, Target: call.Target})
	require.NoError(t, err, "the arguments are checked by the route when the caller does not hash them")
	assert.Equal(t, sum[:], unchecked.BodyHash)

	// A token is only valid on the node it was issued for.
	carolToken, err := alice.Token(ctx, "carol:9000", call)
	require.NoError(t, err)
	_, err = bob.Verify(ctx, carolToken, call)
	assert.ErrorIs(t, err, rpc.ErrInvalidToken, "replayed on another node")

	// Expired tokens, beyond the tolerated clock skew (15s).
	late, err := alice.Token(ctx, "bob:9000", call)
	require.NoError(t, err)
	clk.Advance(rpc.DefaultTokenTTL + 16*time.Second)
	_, err = bob.Verify(ctx, late, call)
	assert.ErrorIs(t, err, rpc.ErrInvalidToken)
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)

	// Other credentials, garbage, no token.
	mallory, err := rpc.NewAuthenticator(rpc.AuthDeps{Clock: clk}, rpc.WithCredentials(rootUser, "guessed"), rpc.WithNodeID("mallory:9000"))
	require.NoError(t, err)
	forged, err := mallory.Token(ctx, "bob:9000", call)
	require.NoError(t, err)
	for name, tok := range map[string]string{"forged": forged, "garbage": "not-a-jwt", "empty": ""} {
		_, err = bob.Verify(ctx, tok, call)
		assert.ErrorIs(t, err, rpc.ErrInvalidToken, name)
	}

	// Deployment binding: refused once both know their (different) ids,
	// accepted from a node that does not know it yet (bootstrap), which
	// is then flagged as unbound.
	alice.SetDeploymentID(ctx, "dep-1")
	bob.SetDeploymentID(ctx, "dep-2")
	token, err = alice.Token(ctx, "bob:9000", call)
	require.NoError(t, err)
	_, err = bob.Verify(ctx, token, call)
	assert.ErrorIs(t, err, rpc.ErrDeploymentMismatch)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	assert.NotContains(t, err.Error(), "dep-2", "the deployment of this node is never disclosed")
	bob.SetDeploymentID(ctx, "dep-1")
	claims, err = bob.Verify(ctx, token, call)
	require.NoError(t, err)
	assert.Equal(t, "dep-1", claims.DeploymentID)
	assert.False(t, claims.Unbound)
	assert.False(t, claims.Unverified)
	carol := auth("carol:9000")
	token, err = carol.Token(ctx, "bob:9000", call)
	require.NoError(t, err)
	claims, err = bob.Verify(ctx, token, call)
	require.NoError(t, err, "a node without deployment id proves the root credentials")
	assert.True(t, claims.Unbound)

	// Major versions must match.
	dave := auth("dave:9000", rpc.WithBuildVersion("v2.0.0"))
	token, err = dave.Token(ctx, "carol:9000", call)
	require.NoError(t, err)
	_, err = carol.Verify(ctx, token, call)
	assert.ErrorIs(t, err, rpc.ErrIncompatibleVersion)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = alice.Token(canceled, "bob:9000", call)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestTokenReplayOverTheWire: a token captured on the wire is good for
// nothing: another call with it (other route, other drive, other
// arguments) is refused, and so is the same call replayed.
func TestTokenReplayOverTheWire(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	addr, srv := startRPC(t, routes)
	// The capture: a write-all call as a peer sends it.
	var captured *http.Request
	var body []byte
	sniff := func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/storage/write-all") && captured == nil {
				data, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				captured, body = r.Clone(context.Background()), data
				r.Body = io.NopCloser(bytes.NewReader(data))
			}
			return rt.RoundTrip(r)
		})
	}
	conn := dial(t, newAuth(t, "client:9000"), addr, rpc.WithTransportWrapper(sniff))
	client, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: discard()}, domain.Endpoint{Scheme: "http", Host: addr, Path: dir})
	require.NoError(t, err)
	require.NoError(t, client.WriteAll(ctx, domain.SystemBucket, "config/legit", []byte("ok")))
	require.NotNil(t, captured)

	h2 := &http.Client{Transport: &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}}}
	send := func(path string, payload []byte) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", captured.Header.Get("Authorization"))
		req.Header.Set("Content-Type", captured.Header.Get("Content-Type"))
		resp, err := h2.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	forged, err := msgpack.Marshal(map[string]any{"Volume": domain.SystemBucket, "Path": "config/iam/takeover", "Data": []byte("evil")})
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, send(captured.URL.RequestURI(), forged), "other arguments")
	assert.Equal(t, http.StatusUnauthorized, send("/nexo/rpc/v1/storage/delete?"+captured.URL.RawQuery, body), "other route")
	assert.Equal(t, http.StatusUnauthorized, send(captured.URL.RequestURI(), body), "the same call replayed")
	_, err = local.ReadAll(ctx, domain.SystemBucket, "config/iam/takeover")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// unsignedToken returns a JWT with the given claims and no signature
// ("alg": "none" is refused; an HS256 header with a bogus signature).
func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." + enc(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("bogus"))
}

// TestVerifySignatureFirst: a token claiming another deployment is
// verified with the key of this node before anything it claims is
// trusted: an unauthenticated caller gets the answer of any forged token,
// never the deployment id of the node (the salt of its keys) nor a
// distinct answer telling it apart.
func TestVerifySignatureFirst(t *testing.T) {
	ctx := context.Background()
	const secretDep = "6f1c2a5e-secret-deployment-id"
	bob := newAuth(t, "bob:9000")
	bob.SetDeploymentID(ctx, secretDep)
	now := time.Now()
	for name, dep := range map[string]string{"other deployment": "x", "same deployment": secretDep, "no deployment": ""} {
		tok := unsignedToken(t, map[string]any{
			"iss": "nexo", "aud": []string{"nexo-internode", "bob:9000"}, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
			"node": "evil", "dep": dep, "pv": rpc.ProtocolVersion, "ver": "dev",
		})
		_, err := bob.Verify(ctx, tok, rpc.CallBinding{Method: http.MethodPost, Target: "/nexo/rpc/v1/health/ping"})
		require.ErrorIs(t, err, rpc.ErrInvalidToken, name)
		assert.NotErrorIs(t, err, rpc.ErrDeploymentMismatch, name)
		assert.NotContains(t, err.Error(), secretDep, name)
		assert.NotContains(t, err.Error(), "bob:9000", name)
	}
}

// TestRPCUnauthenticatedAnswer: over the wire, an unsigned token claiming
// another deployment gets a uniform 401 whose body carries neither the
// deployment id of the node nor the claims.
func TestRPCUnauthenticatedAnswer(t *testing.T) {
	const secretDep = "6f1c2a5e-secret-deployment-id"
	ln := listen(t)
	addr := ln.Addr().String()
	auth := newAuth(t, addr)
	auth.SetDeploymentID(context.Background(), secretDep)
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: auth, Logger: discard()})
	require.NoError(t, err)
	srv := serve(t, ln, s)
	now := time.Now()
	tok := unsignedToken(t, map[string]any{
		"iss": "nexo", "aud": []string{"nexo-internode", addr}, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"node": "evil", "dep": "x", "pv": rpc.ProtocolVersion, "ver": "dev",
	})
	client := &http.Client{Transport: &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, a)
	}}}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/nexo/rpc/v1/health/ping", strings.NewReader(""))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.NotContains(t, string(body), secretDep)
	assert.NotContains(t, string(body), "evil")
}

// TestUnboundCallerRestricted: once a node is bound to its deployment, a
// caller without deployment id (it proves the root credentials, but has
// not read the format: bootstrap, or another deployment sharing the
// credentials) only reaches the calls of the bootstrap.
func TestUnboundCallerRestricted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	ln := listen(t)
	addr := ln.Addr().String()
	serverAuth := newAuth(t, addr)
	serverAuth.SetDeploymentID(ctx, "dep-A")
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: serverAuth, Routes: []rpc.Routes{routes}, Logger: discard()})
	require.NoError(t, err)
	serve(t, ln, s)
	require.NoError(t, local.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, []byte(`{"format":"xl"}`)))
	ep := domain.Endpoint{Scheme: "http", Host: addr, Path: dir}
	client := func(auth rpc.Authenticator) storage.StorageAPI {
		c, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: dial(t, auth, addr), Logger: discard()}, ep)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close(ctx) })
		return c
	}
	unbound := client(newAuth(t, "newcomer:9000", rpc.WithDeploymentID("")))
	// The bootstrap reads.
	data, err := unbound.ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
	require.NoError(t, err)
	assert.JSONEq(t, `{"format":"xl"}`, string(data))
	_, err = unbound.DiskInfo(ctx, storage.DiskInfoOptions{})
	require.NoError(t, err)
	infos, err := unbound.StatInfoFile(ctx, domain.SystemBucket, storage.FormatFile, false)
	require.NoError(t, err)
	assert.Len(t, infos, 1)
	// Nothing else: no listing (volumes, system entries, object names).
	_, err = unbound.StatInfoFile(ctx, domain.SystemBucket, "*", true)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = unbound.StatInfoFile(ctx, domain.SystemBucket, storage.FormatFile, true)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = unbound.ListVols(ctx)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	assert.ErrorIs(t, unbound.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, []byte("{}")), domain.ErrAccessDenied,
		"a bound node receives no format")
	assert.ErrorIs(t, unbound.WriteAll(ctx, domain.SystemBucket, "planted", []byte("from-elsewhere")), domain.ErrAccessDenied)
	_, err = unbound.ReadAll(ctx, domain.SystemBucket, "config/iam/users.json")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	assert.ErrorIs(t, unbound.Delete(ctx, domain.SystemBucket, storage.FormatFile, storage.DeleteOptions{}), domain.ErrAccessDenied)
	_, err = unbound.RenameData(ctx, domain.SystemBucket, "tmp/x", storage.FileInfo{}, "b", "k", storage.RenameOptions{})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = local.ReadAll(ctx, domain.SystemBucket, "planted")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing was written")

	// A caller bound to the deployment reaches everything.
	boundAuth := newAuth(t, "peer:9000")
	boundAuth.SetDeploymentID(ctx, "dep-A")
	require.NoError(t, client(boundAuth).WriteAll(ctx, domain.SystemBucket, "allowed", []byte("ok")))
}

// TestUnverifiedNodeRestricted: a node that does not know its deployment
// yet cannot tell the deployment of its callers: whatever they claim
// (none, or any deployment proving the root credentials), they only reach
// the calls of the bootstrap and the writes that format its drives.
func TestUnverifiedNodeRestricted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}})
	require.NoError(t, err)
	ln := listen(t)
	addr := ln.Addr().String()
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr, rpc.WithDeploymentID("")), Routes: []rpc.Routes{routes}, Logger: discard()})
	require.NoError(t, err)
	serve(t, ln, s)
	ep := domain.Endpoint{Scheme: "http", Host: addr, Path: dir}
	for name, auth := range map[string]rpc.Authenticator{
		"unbound caller":           newAuth(t, "newcomer:9000", rpc.WithDeploymentID("")),
		"caller of any deployment": newAuth(t, "foreign:9000", rpc.WithDeploymentID("11111111-other-deployment")),
	} {
		t.Run(name, func(t *testing.T) {
			c, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: dial(t, auth, addr), Logger: discard()}, ep)
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close(ctx) })
			assert.ErrorIs(t, c.WriteAll(ctx, domain.SystemBucket, "tmp/foreign-write", []byte("x")), domain.ErrAccessDenied)
			assert.ErrorIs(t, c.WriteAll(ctx, domain.SystemBucket, "config/iam/users.json", []byte("{}")), domain.ErrAccessDenied)
			assert.ErrorIs(t, c.WriteAll(ctx, "bucket", storage.FormatFile, []byte("{}")), domain.ErrAccessDenied)
			assert.ErrorIs(t, c.MakeVol(ctx, "bucket"), domain.ErrAccessDenied)
			_, err = c.ReadAll(ctx, domain.SystemBucket, "config/iam/users.json")
			assert.ErrorIs(t, err, domain.ErrAccessDenied)
			assert.ErrorIs(t, c.Delete(ctx, domain.SystemBucket, storage.FormatFile, storage.DeleteOptions{}), domain.ErrAccessDenied)
			// The bootstrap: the format of the drive and its healing tracker.
			require.NoError(t, c.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, []byte(`{"format":"xl"}`)))
			require.NoError(t, c.WriteAll(ctx, domain.SystemBucket, storage.HealingFile, []byte(`{}`)))
			data, err := c.ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
			require.NoError(t, err)
			assert.JSONEq(t, `{"format":"xl"}`, string(data))
		})
	}
	_, err = local.ReadAll(ctx, domain.SystemBucket, "tmp/foreign-write")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing was written")
}

// TestFormatWriteBindsNode: the format a peer writes on a drive of a node
// that does not know its deployment binds the node to that deployment: a
// caller of another deployment is refused from then on.
func TestFormatWriteBindsNode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/d"
	local := openLocal(t, dir)
	ln := listen(t)
	addr := ln.Addr().String()
	serverAuth := newAuth(t, addr, rpc.WithDeploymentID(""))
	routes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: []storage.StorageAPI{local}, Auth: serverAuth})
	require.NoError(t, err)
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: serverAuth, Routes: []rpc.Routes{routes}, Logger: discard()})
	require.NoError(t, err)
	serve(t, ln, s)
	ep := domain.Endpoint{Scheme: "http", Host: addr, Path: dir}
	client := func(auth rpc.Authenticator) storage.StorageAPI {
		c, err := rpc.NewStorageClient(rpc.StorageClientDeps{Conn: dial(t, auth, addr), Logger: discard()}, ep)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close(ctx) })
		return c
	}
	topo := topology.New(topology.Deps{Logger: discard()})
	layout, err := topo.Parse(ctx, []string{t.TempDir() + "/x{1...4}"})
	require.NoError(t, err)
	formats, err := topo.NewFormats(ctx, layout, 0, testDeployment)
	require.NoError(t, err)
	doc, err := formats[0].Marshal()
	require.NoError(t, err)

	formatter, foreign := client(newAuth(t, "first:9000")), client(newAuth(t, "foreign:9000", rpc.WithDeploymentID("other")))
	require.NoError(t, formatter.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, doc))
	require.NoError(t, formatter.WriteAll(ctx, domain.SystemBucket, "config/after-the-format", []byte("ok")),
		"the node is bound to the deployment of its format")
	err = foreign.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, []byte("{}"))
	assert.ErrorIs(t, err, rpc.ErrDeploymentMismatch)
}

func TestAuthenticatorValidation(t *testing.T) {
	_, err := rpc.NewAuthenticator(rpc.AuthDeps{}, rpc.WithNodeID("n:1"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "credentials required")
	_, err = rpc.NewAuthenticator(rpc.AuthDeps{}, rpc.WithCredentials("a", "b"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "node id required")
}
