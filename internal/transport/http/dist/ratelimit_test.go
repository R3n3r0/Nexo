package dist_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	ratelimitmock "github.com/r3n3r0/nexo/internal/service/ratelimit/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	dtmock "github.com/r3n3r0/nexo/internal/transport/http/dist/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// covers: S-DST-08 (the rate limit of a distribution is enforced on the
// website and s3 routes: 429 plain text for a website, 503 SlowDown for an
// s3 distribution, Retry-After; the admission refusals spend no token;
// TDS-13)
func TestHostRouterRateLimit(t *testing.T) {
	limit := &distribution.RateLimit{RequestsPerSecond: 5, Burst: 10}
	shared := &distribution.RateLimit{RequestsPerSecond: 5, Per: distribution.RateLimitPerDistribution}
	site := &distribution.Distribution{ID: "site", Enabled: true, Mode: distribution.ModeWebsite, Origin: distribution.Origin{Bucket: "web"},
		Restrictions: distribution.Restrictions{RateLimit: limit}}
	s3d := &distribution.Distribution{ID: "s3d", Enabled: true, Mode: distribution.ModeS3, Origin: distribution.Origin{Bucket: "customer"},
		Restrictions: distribution.Restrictions{RateLimit: shared}}
	free := &distribution.Distribution{ID: "free", Enabled: true, Mode: distribution.ModeWebsite, Origin: distribution.Origin{Bucket: "web"}}
	routes := map[string]distribution.Route{
		"www.example.com": {Kind: distribution.RouteDistribution, Host: "www.example.com", Alias: "www.example.com", Distribution: site, Bucket: "web"},
		"s3.customer.it":  {Kind: distribution.RouteDistribution, Host: "s3.customer.it", Alias: "s3.customer.it", Distribution: s3d, Bucket: "customer"},
		"free.example":    {Kind: distribution.RouteDistribution, Host: "free.example", Alias: "free.example", Distribution: free, Bucket: "web"},
	}
	res := distmock.NewResolver(t)
	res.EXPECT().Resolve(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, host string) distribution.Route {
		return routes[host]
	})
	website := dtmock.NewWebsiteRoutes(t)
	website.EXPECT().ServeHTTP(tmock.Anything, tmock.Anything).Run(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "website")
	}).Maybe()
	admission := distmock.NewWebsite(t)
	admission.EXPECT().Admit(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, req distribution.WebRequest) *distribution.WebResponse {
		if req.ClientIP == "198.51.100.66" {
			return plainResponse(http.StatusForbidden, "denied")
		}
		return nil
	}).Maybe()
	admission.EXPECT().AuthorizeS3(tmock.Anything, tmock.Anything).Return(nil).Maybe()

	limiter := ratelimitmock.NewService(t)
	refused := ratelimit.Decision{Kind: ratelimit.KindDistribution, RetryAfter: 1500 * time.Millisecond}
	limiter.EXPECT().AdmitDistribution(tmock.Anything, ratelimit.DistributionRequest{API: ratelimit.APIWeb, Distribution: "site",
		ClientIP: "203.0.113.1", Limit: ratelimit.DistributionLimit{RequestsPerSecond: 5, Burst: 10, PerIP: true}}).Return(ratelimit.Decision{Allowed: true}, nil).Once()
	limiter.EXPECT().AdmitDistribution(tmock.Anything, ratelimit.DistributionRequest{API: ratelimit.APIWeb, Distribution: "site",
		ClientIP: "203.0.113.2", Limit: ratelimit.DistributionLimit{RequestsPerSecond: 5, Burst: 10, PerIP: true}}).Return(refused, nil).Once()
	limiter.EXPECT().AdmitDistribution(tmock.Anything, ratelimit.DistributionRequest{API: ratelimit.APIS3, Distribution: "s3d",
		ClientIP: "203.0.113.3", Limit: ratelimit.DistributionLimit{RequestsPerSecond: 5}}).Return(refused, nil).Once()

	router, err := dist.NewHostRouter(dist.HostRouterDeps{Resolver: res, Website: website, Admission: admission,
		Limiter: limiter, S3Errors: middleware.S3ErrorWriter(httpx.New())})
	require.NoError(t, err)
	h := router.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "s3 api") }))
	serve := func(host, ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/key", nil)
		r = r.WithContext(middleware.WithClient(r.Context(), middleware.Client{IP: ip, Host: host}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := serve("www.example.com", "203.0.113.1")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "website", w.Body.String())
	w = serve("www.example.com", "203.0.113.2")
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "2", w.Header().Get("Retry-After"))
	assert.True(t, strings.HasPrefix(w.Body.String(), "429 Too Many Requests"), w.Body.String())
	w = serve("s3.customer.it", "203.0.113.3")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>SlowDown</Code>")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	w = serve("www.example.com", "198.51.100.66")
	assert.Equal(t, http.StatusOK, w.Code, "refused by the admission (answered by the website): no token spent")
	w = serve("free.example", "203.0.113.1")
	assert.Equal(t, http.StatusOK, w.Code, "no rate limit: the limiter is not asked")
}
