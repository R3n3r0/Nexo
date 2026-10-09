package dist_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	dtmock "github.com/r3n3r0/nexo/internal/transport/http/dist/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// exchangeKey marks the context of an exchange begun by the cache.
type exchangeKey struct{}

// TestHostRouterCache checks the insertion of the cache in the path of
// the distributions: the exchange is begun before the engine (whose reads
// see it in the context), its headers are added to the answer and it is
// finished with the status and the size; the other routes skip it.
func TestHostRouterCache(t *testing.T) {
	var (
		begun    []cache.Request
		finished []int64
		statuses []int
	)
	runtime := cachemock.NewRuntime(t)
	runtime.EXPECT().Begin(tmock.Anything, tmock.Anything).RunAndReturn(func(ctx context.Context, req cache.Request) (context.Context, cache.Exchange) {
		begun = append(begun, req)
		ex := cachemock.NewExchange(t)
		ex.EXPECT().Decorate(tmock.Anything, tmock.Anything).Run(func(status int, h http.Header) {
			h.Set(cache.HeaderXCache, "HIT")
			h.Set("Age", "12")
		}).Maybe()
		ex.EXPECT().Finish(tmock.Anything, tmock.Anything, tmock.Anything).Run(func(_ context.Context, status int, n int64) {
			statuses = append(statuses, status)
			finished = append(finished, n)
		}).Once()
		return context.WithValue(ctx, exchangeKey{}, true), ex
	}).Maybe()
	website := dtmock.NewWebsiteRoutes(t)
	website.EXPECT().ServeHTTP(tmock.Anything, tmock.Anything).Run(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(exchangeKey{}) == nil {
			http.Error(w, "no exchange", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/missing" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "cached page")
	}).Maybe()
	admission := distmock.NewWebsite(t)
	admission.EXPECT().Admit(tmock.Anything, tmock.Anything).Return(nil).Maybe()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(exchangeKey{}) != nil {
			_, _ = io.WriteString(w, "s3 on domain")
			return
		}
		_, _ = io.WriteString(w, "s3 api")
	})
	router, err := dist.NewHostRouter(dist.HostRouterDeps{Resolver: resolver(t), Website: website, Admission: admission,
		Cache: runtime, Clock: clock.NewReal()})
	require.NoError(t, err)
	h := router.Wrap(next)

	serve := func(target string, mod func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r = r.WithContext(middleware.WithClient(r.Context(), middleware.Client{IP: "198.51.100.7", Secure: true, Host: r.Host}))
		if mod != nil {
			mod(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := serve("http://www.example.com/index.html?utm_source=x", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "cached page", w.Body.String())
	assert.Equal(t, "HIT", w.Header().Get(cache.HeaderXCache))
	assert.Equal(t, "12", w.Header().Get("Age"))
	require.Len(t, begun, 1)
	req := begun[0]
	assert.Equal(t, "site", req.Distribution.ID)
	assert.Equal(t, "www.example.com", req.Host)
	assert.Equal(t, "www.example.com", req.Alias)
	assert.Equal(t, "/index.html", req.Path)
	assert.Equal(t, "utm_source=x", req.RawQuery)
	assert.True(t, req.Secure)
	assert.Equal(t, "198.51.100.7", req.ClientIP)
	assert.False(t, req.Credentials)
	assert.Equal(t, []int64{int64(len("cached page"))}, finished)
	assert.Equal(t, []int{http.StatusOK}, statuses)

	// An error answer is decorated and finished too.
	w = serve("http://www.example.com/missing", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "HIT", w.Header().Get(cache.HeaderXCache))
	assert.Equal(t, http.StatusNotFound, statuses[1])

	// The credentials of the request partition (or bypass) the cache; a
	// signed URL of the distribution is not a credential.
	tests := []struct {
		name   string
		target string
		mod    func(*http.Request)
		creds  bool
	}{
		{"authorization", "http://www.example.com/a", func(r *http.Request) { r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=x") }, true},
		{"presigned v4", "http://www.example.com/a?X-Amz-Signature=abc&X-Amz-Credential=k", nil, true},
		{"presigned v2", "http://www.example.com/a?AWSAccessKeyId=k&Signature=s&Expires=1", nil, true},
		{"distribution signed URL", "http://www.example.com/a?Expires=1&Signature=s&Key-Pair-Id=k", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := len(begun)
			serve(tc.target, tc.mod)
			require.Len(t, begun, n+1)
			assert.Equal(t, tc.creds, begun[n].Credentials)
		})
	}

	// The S3 mode on a domain begins an exchange; the API does not.
	n := len(begun)
	w = serve("http://s3.cliente.it/key", nil)
	assert.Equal(t, "s3 on domain", w.Body.String())
	require.Len(t, begun, n+1)
	assert.Equal(t, "s3d", begun[n].Distribution.ID)
	w = serve("http://bucket.s3.example/key", nil)
	assert.Equal(t, "s3 api", w.Body.String())
	w = serve("http://127.0.0.1:9000/bucket/key", nil)
	assert.Equal(t, "s3 api", w.Body.String())
	assert.Len(t, begun, n+1)
	assert.Empty(t, w.Header().Get(cache.HeaderXCache))
}
