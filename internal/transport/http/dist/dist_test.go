package dist_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/certs"
	certsmock "github.com/r3n3r0/nexo/internal/service/certs/mock"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	dtmock "github.com/r3n3r0/nexo/internal/transport/http/dist/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// routes of the tests: a website distribution, an s3 one, a disabled one.
func testRoutes() map[string]distribution.Route {
	site := &distribution.Distribution{ID: "site", Enabled: true, Mode: distribution.ModeWebsite, Origin: distribution.Origin{Bucket: "web"},
		Logging: distribution.Logging{AccessLog: true, Format: distribution.LogFormatCombined}}
	s3d := &distribution.Distribution{ID: "s3d", Enabled: true, Mode: distribution.ModeS3, Origin: distribution.Origin{Bucket: "cliente"}}
	return map[string]distribution.Route{
		"www.example.com":   {Kind: distribution.RouteDistribution, Host: "www.example.com", Alias: "www.example.com", Distribution: site, Bucket: "web"},
		"s3.cliente.it":     {Kind: distribution.RouteDistribution, Host: "s3.cliente.it", Alias: "s3.cliente.it", Distribution: s3d, Bucket: "cliente"},
		"unknown.example":   {Kind: distribution.RouteMisdirected, Host: "unknown.example"},
		"bucket.s3.example": {Kind: distribution.RouteVirtualHost, Host: "bucket.s3.example", Bucket: "bucket"},
	}
}

func resolver(t *testing.T) *distmock.Resolver {
	t.Helper()
	routes := testRoutes()
	m := distmock.NewResolver(t)
	m.EXPECT().Resolve(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, host string) distribution.Route {
		h := strings.ToLower(host)
		if i := strings.LastIndexByte(h, ':'); i > 0 {
			h = h[:i]
		}
		if r, ok := routes[h]; ok {
			return r
		}
		return distribution.Route{Kind: distribution.RoutePathStyle, Host: h}
	}).Maybe()
	return m
}

func plainResponse(status int, body string) *distribution.WebResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	return &distribution.WebResponse{Status: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestWebsiteRoutesDecodeAndEncode(t *testing.T) {
	engine := distmock.NewWebsite(t)
	var got distribution.WebRequest
	engine.EXPECT().Serve(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, req distribution.WebRequest) (*distribution.WebResponse, error) {
		got = req
		resp := plainResponse(http.StatusOK, "hello")
		resp.Header.Set("Content-Length", "5")
		resp.Header.Set("ETag", `"e"`)
		return resp, nil
	})
	verifier := authmock.NewVerifier(t)
	verifier.EXPECT().Verify(tmock.Anything, tmock.Anything).Return(&domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}, nil)
	routes, err := dist.NewWebsiteRoutes(dist.WebsiteDeps{Website: engine, Resolver: resolver(t), Verifier: verifier, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodGet, "http://www.example.com/a%20b/c.txt?x=1", nil)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=...")
	r = r.WithContext(middleware.WithClient(r.Context(), middleware.Client{IP: "203.0.113.9", Secure: true, Host: "www.example.com"}))
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, r)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hello", w.Body.String())
	assert.Equal(t, `"e"`, w.Header().Get("ETag"))
	assert.Equal(t, "/a b/c.txt", got.Path)
	assert.Equal(t, "/a%20b/c.txt", got.EscapedPath)
	assert.Equal(t, "x=1", got.RawQuery)
	assert.Equal(t, "203.0.113.9", got.ClientIP)
	assert.True(t, got.Secure)
	require.NotNil(t, got.Identity)
	assert.Equal(t, "alice", got.Identity.AccessKey)
	assert.Equal(t, "site", got.Route.Distribution.ID)

	// HEAD sends the headers only; an unknown host is misdirected.
	engine2 := distmock.NewWebsite(t)
	engine2.EXPECT().Serve(tmock.Anything, tmock.Anything).Return(plainResponse(http.StatusNotFound, "not found\n"), nil)
	routes2, err := dist.NewWebsiteRoutes(dist.WebsiteDeps{Website: engine2, Resolver: resolver(t)})
	require.NoError(t, err)
	w = httptest.NewRecorder()
	routes2.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "http://www.example.com/x", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, w.Body.String())
	w = httptest.NewRecorder()
	routes2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://elsewhere.example/x", nil))
	assert.Equal(t, http.StatusMisdirectedRequest, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")

	_, err = dist.NewWebsiteRoutes(dist.WebsiteDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestHostRouter covers the routing by Host on the S3 and web listeners.
//
// covers: S-DST-01, S-DST-08
func TestHostRouter(t *testing.T) {
	website := dtmock.NewWebsiteRoutes(t)
	website.EXPECT().ServeHTTP(tmock.Anything, tmock.Anything).Run(func(w http.ResponseWriter, r *http.Request) {
		route, ok := dist.RouteFromContext(r.Context())
		require.True(t, ok)
		_, _ = io.WriteString(w, "website "+route.Distribution.ID)
	}).Maybe()
	admission := distmock.NewWebsite(t)
	admission.EXPECT().Admit(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, req distribution.WebRequest) *distribution.WebResponse {
		if req.Header.Get("X-Origin-Secret") != "ok" {
			return plainResponse(http.StatusForbidden, "denied")
		}
		return nil
	}).Maybe()
	// The access of the s3 distribution, once authenticated: /private
	// needs credentials.
	admission.EXPECT().AuthorizeS3(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, req distribution.WebRequest) *distribution.WebResponse {
		if req.Path == "/private" && (req.Identity == nil || req.Identity.IsAnonymous) {
			return plainResponse(http.StatusForbidden, "credentials")
		}
		return nil
	}).Maybe()
	registry := metrics.New(metrics.Deps{Logger: slog.New(slog.DiscardHandler)})
	var logged bytes.Buffer
	accessLog := dist.NewAccessLog(dist.AccessLogDeps{Logger: slog.New(slog.NewTextHandler(&logged, nil))})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "s3 api") })

	build := func(opts ...dist.HostRouterOption) http.Handler {
		router, err := dist.NewHostRouter(dist.HostRouterDeps{Resolver: resolver(t), Website: website, Admission: admission,
			Registry: registry, AccessLog: accessLog, Clock: clock.NewReal()}, opts...)
		require.NoError(t, err)
		return router.Wrap(next)
	}
	s3Listener := build(dist.WithListenerName("s3"))
	webListener := build(dist.WithDistributionsOnly(), dist.WithPassthrough(dist.ChallengePrefix))

	tests := []struct {
		name     string
		handler  http.Handler
		host     string
		path     string
		secret   string
		identity bool
		status   int
		body     string
	}{
		{name: "website on S3 listener", handler: s3Listener, host: "www.example.com:9000", path: "/", status: 200, body: "website site"},
		{name: "s3 distribution admitted", handler: s3Listener, host: "s3.cliente.it", path: "/key", secret: "ok", status: 200, body: "s3 api"},
		{name: "s3 distribution refused", handler: s3Listener, host: "s3.cliente.it", path: "/key", status: 403, body: "denied"},
		{name: "s3 distribution anonymous", handler: s3Listener, host: "s3.cliente.it", path: "/private", secret: "ok", status: 403, body: "credentials"},
		{name: "s3 distribution authenticated", handler: s3Listener, host: "s3.cliente.it", path: "/private", secret: "ok", identity: true,
			status: 200, body: "s3 api"},
		{name: "virtual host to the API", handler: s3Listener, host: "bucket.s3.example", path: "/key", status: 200, body: "s3 api"},
		{name: "path style to the API", handler: s3Listener, host: "127.0.0.1:9000", path: "/bucket/key", status: 200, body: "s3 api"},
		{name: "misdirected", handler: s3Listener, host: "unknown.example", path: "/", status: 421},
		{name: "website on web listener", handler: webListener, host: "www.example.com", path: "/x", status: 200, body: "website site"},
		{name: "no API on web listener", handler: webListener, host: "127.0.0.1", path: "/bucket/key", status: 421},
		{name: "ACME challenge on any host", handler: webListener, host: "www.example.com", path: "/.well-known/acme-challenge/t", status: 200, body: "s3 api"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+tc.path, nil)
			r.Host = tc.host
			if tc.secret != "" {
				r.Header.Set("X-Origin-Secret", tc.secret)
			}
			if tc.identity {
				r = r.WithContext(middleware.WithIdentity(r.Context(), &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}))
			}
			w := httptest.NewRecorder()
			tc.handler.ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code)
			if tc.body != "" {
				assert.Equal(t, tc.body, w.Body.String())
			}
		})
	}

	// Metrics per distribution and alias.
	w := httptest.NewRecorder()
	registry.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	text := w.Body.String()
	assert.Contains(t, text, `nexo_distribution_requests_total{distribution="site",host="www.example.com",status="200"} 2`)
	assert.Contains(t, text, `nexo_distribution_requests_total{distribution="s3d",host="s3.cliente.it",status="403"} 2`)
	assert.Contains(t, text, `nexo_distribution_bytes_served_total{distribution="site",host="www.example.com"} 24`)
	assert.Contains(t, text, `nexo_distribution_request_duration_seconds_count{distribution="site"} 2`)
	// Access log in the combined format for the distribution enabling it.
	assert.Contains(t, logged.String(), `\"GET / HTTP/1.1\" 200 12 \"-\" \"-\"`)
	assert.NotContains(t, logged.String(), "s3d")

	// The access log never keeps a replayable URL: signed URL of the
	// distribution, presigned S3 URL, session token.
	logged.Reset()
	signed := "/dl.zip?v=1&Expires=1700000000&Policy=cG9saWN5&Signature=SIGVALUE&Key-Pair-Id=KP1&X-Amz-Credential=AKID%2Fx&X-Amz-Security-Token=TOKVALUE"
	r := httptest.NewRequest(http.MethodGet, "http://www.example.com"+signed, nil)
	s3Listener.ServeHTTP(httptest.NewRecorder(), r)
	line := logged.String()
	assert.Contains(t, line, "/dl.zip?v=1&Expires=1700000000&Policy=REDACTED&Signature=REDACTED&Key-Pair-Id=REDACTED")
	for _, secret := range []string{"SIGVALUE", "cG9saWN5", "KP1", "AKID", "TOKVALUE"} {
		assert.NotContains(t, line, secret)
	}
}

// TestRefusedS3Request selects the requests of an s3 distribution that
// its admission refuses: the authentication of the listener skips them.
func TestRefusedS3Request(t *testing.T) {
	admission := distmock.NewWebsite(t)
	admission.EXPECT().Admit(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, req distribution.WebRequest) *distribution.WebResponse {
		if req.Header.Get("X-Origin-Secret") != "ok" {
			return plainResponse(http.StatusForbidden, "denied")
		}
		return nil
	}).Maybe()
	refused := dist.RefusedS3Request(resolver(t), admission)
	req := func(host, secret string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/key", nil)
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKID/20260101/us-east-1/s3/aws4_request")
		if secret != "" {
			r.Header.Set("X-Origin-Secret", secret)
		}
		return r
	}
	assert.True(t, refused(req("s3.cliente.it", "")), "no origin secret: refused before the signature")
	assert.False(t, refused(req("s3.cliente.it", "ok")))
	assert.False(t, refused(req("www.example.com", "")), "a website distribution")
	assert.False(t, refused(req("bucket.s3.example", "")), "not a distribution")
}

func TestChallengeRoutes(t *testing.T) {
	svc := certsmock.NewService(t)
	svc.EXPECT().HTTPChallenge(tmock.Anything, "tok-1").Return("tok-1.thumb", nil)
	svc.EXPECT().HTTPChallenge(tmock.Anything, "other").Return("", certs.ErrChallengeNotFound)
	routes, err := dist.NewChallengeRoutes(dist.ChallengeDeps{Certs: svc})
	require.NoError(t, err)
	r := chi.NewRouter()
	routes.Register(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://www.example.com/.well-known/acme-challenge/tok-1", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "tok-1.thumb", w.Body.String())
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://www.example.com/.well-known/acme-challenge/other", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
	_, err = dist.NewChallengeRoutes(dist.ChallengeDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// covers: S-DST-08
func TestAccessLogJSON(t *testing.T) {
	var buf bytes.Buffer
	log := dist.NewAccessLog(dist.AccessLogDeps{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	log.Log(context.Background(), dist.AccessEntry{Time: time.Unix(0, 0), Distribution: "site", Format: distribution.LogFormatJSON,
		Host: "www.example.com", Method: "GET", URI: "/a?b", Status: 200, Bytes: 10, UserAgent: `x"y`})
	out := buf.String()
	for _, want := range []string{`"distribution":"site"`, `"status":200`, `"bytes":10`, `"uri":"/a?b"`} {
		assert.Contains(t, out, want)
	}
	buf.Reset()
	log.Log(context.Background(), dist.AccessEntry{Time: time.Unix(0, 0).UTC(), Distribution: "site", Format: distribution.LogFormatCombined,
		ClientIP: "192.0.2.1", Method: "GET", URI: "/a", Protocol: "HTTP/1.1", Status: 404, UserAgent: "agent \"quoted\""})
	assert.Contains(t, buf.String(), `192.0.2.1 - - [01/Jan/1970:00:00:00 +0000] \"GET /a HTTP/1.1\" 404 - \"-\" \"agent \\\"quoted\\\"\"`)
}
