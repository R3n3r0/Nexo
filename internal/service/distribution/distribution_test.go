package distribution_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// covers: S-DST-01, S-DST-03
func TestValidate(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	_, err := svc.CreateKeyGroup(ctx, "rilasci", "")
	require.NoError(t, err)

	mutate := func(fn func(d *distribution.Distribution)) distribution.Distribution {
		d := sample("site", "www.example.com")
		fn(&d)
		return d
	}
	tests := []struct {
		name  string
		d     distribution.Distribution
		valid bool
		check func(t *testing.T, d distribution.Distribution)
	}{
		{name: "minimal", d: sample("site", "WWW.Example.com."), valid: true, check: func(t *testing.T, d distribution.Distribution) {
			assert.Equal(t, []string{"www.example.com"}, d.Aliases)
			assert.Equal(t, distribution.ModeWebsite, d.Mode)
		}},
		{name: "path prefix normalized", d: mutate(func(d *distribution.Distribution) { d.Origin.PathPrefix = "prod/site/" }), valid: true,
			check: func(t *testing.T, d distribution.Distribution) { assert.Equal(t, "/prod/site", d.Origin.PathPrefix) }},
		{name: "path prefix escaping", d: mutate(func(d *distribution.Distribution) { d.Origin.PathPrefix = "/prod/../secret" })},
		{name: "path prefix dot", d: mutate(func(d *distribution.Distribution) { d.Origin.PathPrefix = "./prod" })},
		{name: "path prefix in s3 mode", d: mutate(func(d *distribution.Distribution) {
			d.Mode = distribution.ModeS3
			d.Origin.PathPrefix = "/prod"
		})},
		{name: "s3 mode with path behaviors", d: mutate(func(d *distribution.Distribution) {
			d.Mode, d.DefaultBehavior.IndexDocument = distribution.ModeS3, ""
			d.Behaviors = []distribution.Behavior{{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/"},
				Access: distribution.AccessDeny}}
		})},
		{name: "s3 mode with signed access", d: mutate(func(d *distribution.Distribution) {
			d.Mode, d.DefaultBehavior = distribution.ModeS3, distribution.Behavior{Access: distribution.AccessSigned,
				Signing: &distribution.Signing{KeyGroup: "rilasci"}}
		})},
		{name: "s3 mode with methods", d: mutate(func(d *distribution.Distribution) {
			d.Mode, d.DefaultBehavior = distribution.ModeS3, distribution.Behavior{AllowedMethods: []string{"GET"}}
		})},
		{name: "s3 mode with an index document", d: mutate(func(d *distribution.Distribution) { d.Mode = distribution.ModeS3 })},
		{name: "s3 mode with error pages", d: mutate(func(d *distribution.Distribution) {
			d.Mode, d.DefaultBehavior.IndexDocument = distribution.ModeS3, ""
			d.ErrorPages = []distribution.ErrorPage{{Status: 404, Path: "/404.html"}}
		})},
		{name: "s3 mode with response headers", d: mutate(func(d *distribution.Distribution) {
			d.Mode, d.DefaultBehavior.IndexDocument = distribution.ModeS3, ""
			d.Headers.Response = map[string]string{"X-Frame-Options": "DENY"}
		})},
		{name: "s3 mode authenticated", valid: true, d: mutate(func(d *distribution.Distribution) {
			d.Mode, d.DefaultBehavior = distribution.ModeS3, distribution.Behavior{Access: distribution.AccessAuthenticated}
		})},
		{name: "CORS credentials with any origin", d: mutate(func(d *distribution.Distribution) {
			d.Headers.CORS = &distribution.CORS{AllowedOrigins: []string{"*"}, AllowCredentials: true}
		})},
		{name: "CORS credentials with an origin pattern", d: mutate(func(d *distribution.Distribution) {
			d.Headers.CORS = &distribution.CORS{AllowedOrigins: []string{"https://*.example.com"}, AllowCredentials: true}
		})},
		{name: "CORS credentials with exact origins", valid: true, d: mutate(func(d *distribution.Distribution) {
			d.Headers.CORS = &distribution.CORS{AllowedOrigins: []string{"https://app.example.com"}, AllowCredentials: true}
		})},
		{name: "invalid id", d: mutate(func(d *distribution.Distribution) { d.ID = "Site/1" })},
		{name: "no alias", d: mutate(func(d *distribution.Distribution) { d.Aliases = nil })},
		{name: "invalid alias", d: mutate(func(d *distribution.Distribution) { d.Aliases = []string{"www.exa mple.com"} })},
		{name: "IP alias", d: mutate(func(d *distribution.Distribution) { d.Aliases = []string{"10.0.0.1"} })},
		{name: "bare wildcard", d: mutate(func(d *distribution.Distribution) { d.Aliases = []string{"*.com"} })},
		{name: "inner wildcard", d: mutate(func(d *distribution.Distribution) { d.Aliases = []string{"www.*.example.com"} })},
		{name: "reserved bucket", d: mutate(func(d *distribution.Distribution) { d.Origin.Bucket = domain.SystemBucket })},
		{name: "unknown mode", d: mutate(func(d *distribution.Distribution) { d.Mode = "ftp" })},
		{name: "invalid regex behavior", d: mutate(func(d *distribution.Distribution) {
			d.Behaviors = []distribution.Behavior{{PathPattern: &distribution.PathPattern{Type: distribution.PatternRegex, Pattern: "^/(a"}}}
		})},
		{name: "regex too long", d: mutate(func(d *distribution.Distribution) {
			d.Behaviors = []distribution.Behavior{{PathPattern: &distribution.PathPattern{Type: distribution.PatternRegex, Pattern: strings.Repeat("a", 1025)}}}
		})},
		{name: "behavior without pattern", d: mutate(func(d *distribution.Distribution) {
			d.Behaviors = []distribution.Behavior{{Access: distribution.AccessDeny}}
		})},
		{name: "unknown access", d: mutate(func(d *distribution.Distribution) { d.DefaultBehavior.Access = "everyone" })},
		{name: "PUT without signed", d: mutate(func(d *distribution.Distribution) {
			d.DefaultBehavior.AllowedMethods = []string{"get", "put"}
		})},
		{name: "PUT on signed behavior", valid: true, d: mutate(func(d *distribution.Distribution) {
			d.Behaviors = []distribution.Behavior{{
				PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/uploads/"},
				Access:      distribution.AccessSigned, AllowedMethods: []string{"PUT", "GET"},
				Signing: &distribution.Signing{KeyGroup: "rilasci"},
			}}
		}), check: func(t *testing.T, d distribution.Distribution) {
			assert.Equal(t, []string{http.MethodPut, http.MethodGet}, d.Behaviors[0].AllowedMethods)
		}},
		{name: "signed without key group", d: mutate(func(d *distribution.Distribution) { d.DefaultBehavior.Access = distribution.AccessSigned })},
		{name: "unknown key group", d: mutate(func(d *distribution.Distribution) {
			d.DefaultBehavior.Access = distribution.AccessSigned
			d.DefaultBehavior.Signing = &distribution.Signing{KeyGroup: "nope"}
		})},
		{name: "invalid redirect regex", d: mutate(func(d *distribution.Distribution) {
			d.Redirects = []distribution.Redirect{{From: "^/(x", To: "/y"}}
		})},
		{name: "redirect with two kinds", d: mutate(func(d *distribution.Distribution) {
			d.Redirects = []distribution.Redirect{{From: "^/x", To: "/y", HTTPSOnly: true}}
		})},
		{name: "redirect status", d: mutate(func(d *distribution.Distribution) {
			d.Redirects = []distribution.Redirect{{HTTPSOnly: true, Status: 200}}
		})},
		{name: "redirects normalized", valid: true, d: mutate(func(d *distribution.Distribution) {
			d.Redirects = []distribution.Redirect{{HostFrom: "Example.com", ToHost: "WWW.example.com"}, {From: "^/old/(.*)$", To: "/new/$1", Status: 302}}
		}), check: func(t *testing.T, d distribution.Distribution) {
			assert.Equal(t, "example.com", d.Redirects[0].HostFrom)
			assert.Equal(t, http.StatusMovedPermanently, d.Redirects[0].Status)
		}},
		{name: "error page status", d: mutate(func(d *distribution.Distribution) {
			d.ErrorPages = []distribution.ErrorPage{{Status: 200, Path: "/x.html"}}
		})},
		{name: "error page path", d: mutate(func(d *distribution.Distribution) {
			d.ErrorPages = []distribution.ErrorPage{{Status: 404, Path: "../x.html"}}
		})},
		{name: "invalid CIDR", d: mutate(func(d *distribution.Distribution) {
			d.Restrictions.SourceIP = &distribution.IPRestriction{Deny: []string{"10.0.0.0/33"}}
		})},
		{name: "origin secret hashed", valid: true, d: mutate(func(d *distribution.Distribution) {
			d.Restrictions.OriginSecretHeader = &distribution.OriginSecret{Name: "x-origin-secret", Value: "s3cret"}
		}), check: func(t *testing.T, d distribution.Distribution) {
			sec := d.Restrictions.OriginSecretHeader
			require.NotNil(t, sec)
			assert.Equal(t, "X-Origin-Secret", sec.Name)
			assert.Empty(t, sec.Value)
			require.Len(t, sec.Values, 1)
			assert.True(t, strings.HasPrefix(sec.Values[0], "sha256:"))
			assert.NotContains(t, sec.Values[0], "s3cret")
		}},
		{name: "invalid header", d: mutate(func(d *distribution.Distribution) { d.Headers.Response = map[string]string{"Bad Header": "x"} })},
		{name: "CORS without origins", d: mutate(func(d *distribution.Distribution) { d.Headers.CORS = &distribution.CORS{} })},
		{name: "tls", d: mutate(func(d *distribution.Distribution) { d.TLS.Certificate = "letsencrypt" })},
		{name: "tls upload", valid: true, d: mutate(func(d *distribution.Distribution) { d.TLS.Certificate = "upload:my-cert" })},
		{name: "log format", d: mutate(func(d *distribution.Distribution) { d.Logging.Format = "xml" })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.Validate(ctx, tc.d)
			if !tc.valid {
				require.Error(t, err)
				assert.ErrorIs(t, err, domain.ErrInvalidArgument, "400: %v", err)
				return
			}
			require.NoError(t, err)
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// TestPropagation creates a distribution on a node and resolves it on the
// others without restart (TCD-03 at the service level).
//
// covers: S-DST-01, S-STO-06
func TestPropagation(t *testing.T) {
	ctx := context.Background()
	nodes := newDeployment(t, 3)
	_, err := nodes[0].svc.Create(ctx, sample("site", "www.example.com"))
	require.NoError(t, err)
	for i, n := range nodes {
		r := n.svc.Resolve(ctx, "www.example.com")
		require.Equal(t, distribution.RouteDistribution, r.Kind, "node %d", i+1)
	}
	// Changes and deletions too.
	d, err := nodes[1].svc.Get(ctx, "site")
	require.NoError(t, err)
	d.Aliases = []string{"new.example.com"}
	_, err = nodes[1].svc.Update(ctx, d, "")
	require.NoError(t, err)
	for _, n := range nodes {
		assert.Equal(t, distribution.RoutePathStyle, n.svc.Resolve(ctx, "www.example.com").Kind)
		assert.Equal(t, distribution.RouteDistribution, n.svc.Resolve(ctx, "new.example.com").Kind)
	}
	require.NoError(t, nodes[2].svc.Delete(ctx, "site"))
	for _, n := range nodes {
		assert.Equal(t, distribution.RoutePathStyle, n.svc.Resolve(ctx, "new.example.com").Kind)
	}
	require.ErrorIs(t, nodes[0].svc.Delete(ctx, "site"), distribution.ErrDistributionNotFound)

	list, err := nodes[0].svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, sortedAliases(list))
}

// covers: S-DST-05
func TestEnableAndRotateOriginSecret(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	_, err := svc.Create(ctx, sample("site", "www.example.com"))
	require.NoError(t, err)

	d, err := svc.SetEnabled(ctx, "site", false)
	require.NoError(t, err)
	assert.False(t, d.Enabled)
	assert.False(t, svc.Resolve(ctx, "www.example.com").Distribution.Enabled)

	first, d, err := svc.RotateOriginSecret(ctx, distribution.RotateSecretInput{ID: "site"})
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.Len(t, d.Restrictions.OriginSecretHeader.Values, 1)
	assert.Equal(t, distribution.DefaultOriginSecretHeader, d.Restrictions.OriginSecretHeader.Name)

	second, d, err := svc.RotateOriginSecret(ctx, distribution.RotateSecretInput{ID: "site"})
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.Len(t, d.Restrictions.OriginSecretHeader.Values, 2, "the previous secret stays active")

	_, d, err = svc.RotateOriginSecret(ctx, distribution.RotateSecretInput{ID: "site", Header: "X-Proxy-A"})
	require.NoError(t, err)
	assert.Len(t, d.Restrictions.OriginSecretHeader.Values, 2, "only the last previous secret is kept")
	assert.Equal(t, "X-Proxy-A", d.Restrictions.OriginSecretHeader.Name)

	_, d, err = svc.RotateOriginSecret(ctx, distribution.RotateSecretInput{ID: "site", DropPrevious: true})
	require.NoError(t, err)
	assert.Len(t, d.Restrictions.OriginSecretHeader.Values, 1)
}

// TestServiceTest covers nexoctl dist test: behavior, key, redirect and
// headers of a URL (TDS-05 at the service level).
//
// covers: S-DST-03
func TestServiceTest(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	d := sample("site", "www.example.com", "example.com")
	d.Origin.PathPrefix = "/prod"
	d.Headers.Response = map[string]string{"X-Content-Type-Options": "nosniff"}
	d.Behaviors = []distribution.Behavior{
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"},
			Cache: &distribution.Cache{CacheControl: "public, max-age=31536000, immutable"}},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/assets/private/"}, Access: distribution.AccessDeny},
		{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/"}, Access: distribution.AccessDeny},
	}
	d.Redirects = []distribution.Redirect{{HostFrom: "example.com", ToHost: "www.example.com"}}
	_, err := svc.Create(ctx, d)
	require.NoError(t, err)

	tests := []struct {
		url, method string
		behavior    int
		key         string
		refusal     int
		location    string
	}{
		{url: "https://www.example.com/assets/app.js", behavior: 0, key: "prod/assets/app.js"},
		// First match wins: the deny behavior after the glob never applies.
		{url: "https://www.example.com/assets/private/x", behavior: 0, key: "prod/assets/private/x"},
		{url: "https://www.example.com/internal/x", behavior: 2, refusal: http.StatusForbidden},
		{url: "https://www.example.com/", behavior: -1, key: "prod/"},
		{url: "https://www.example.com/a", method: http.MethodDelete, behavior: -1, refusal: http.StatusMethodNotAllowed},
		{url: "https://www.example.com/../secret", behavior: -1, refusal: http.StatusNotFound},
		{url: "http://example.com/a?b=c", behavior: -1, location: "http://www.example.com/a?b=c"},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			res, err := svc.Test(ctx, distribution.TestInput{URL: tc.url, Method: tc.method})
			require.NoError(t, err)
			assert.Equal(t, distribution.RouteDistribution, res.Route.Kind)
			assert.Equal(t, tc.behavior, res.BehaviorIndex)
			assert.Equal(t, tc.key, res.Key)
			assert.Equal(t, tc.refusal, res.RefusalStatus, res.Refusal)
			assert.Equal(t, tc.location, res.RedirectLocation)
			if tc.location == "" {
				assert.Equal(t, "nosniff", res.ResponseHeaders["X-Content-Type-Options"])
			}
		})
	}
	// Refused by the admission (no origin secret: the request of a proxy
	// without its header): the behavior of the path is still reported.
	d, err = svc.Get(ctx, "site")
	require.NoError(t, err)
	d.Restrictions.OriginSecretHeader = &distribution.OriginSecret{Value: "proxy-secret"}
	_, err = svc.Update(ctx, d, "")
	require.NoError(t, err)
	res, err := svc.Test(ctx, distribution.TestInput{URL: "https://www.example.com/internal/x"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, res.RefusalStatus, res.Refusal)
	assert.Equal(t, 2, res.BehaviorIndex)
	assert.Equal(t, distribution.AccessDeny, res.Behavior.Access)

	// The s3 mode: the S3 API decides the methods and the keys; only the
	// access of the default behavior applies (TCL-D-03 coherent with the
	// requests).
	s3d := distribution.Distribution{ID: "s3ep", Enabled: true, Aliases: []string{"s3.customer.test"}, Mode: distribution.ModeS3,
		Origin: distribution.Origin{Bucket: "customer"}, DefaultBehavior: distribution.Behavior{Access: distribution.AccessAuthenticated}}
	_, err = svc.Create(ctx, s3d)
	require.NoError(t, err)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		res, err = svc.Test(ctx, distribution.TestInput{URL: "https://s3.customer.test/internal/secret.txt", Method: method})
		require.NoError(t, err)
		assert.Zero(t, res.RefusalStatus, "%s: %s", method, res.Refusal)
		assert.Equal(t, "internal/secret.txt", res.Key)
		assert.Equal(t, distribution.AccessAuthenticated, res.Behavior.Access)
		assert.Empty(t, res.ResponseHeaders)
	}
	s3d, err = svc.Get(ctx, "s3ep")
	require.NoError(t, err)
	s3d.DefaultBehavior.Access = distribution.AccessDeny
	_, err = svc.Update(ctx, s3d, "")
	require.NoError(t, err)
	res, err = svc.Test(ctx, distribution.TestInput{URL: "https://s3.customer.test/a"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, res.RefusalStatus)

	res, err = svc.Test(ctx, distribution.TestInput{URL: "https://unknown.example.net/"})
	require.NoError(t, err)
	assert.Equal(t, distribution.RoutePathStyle, res.Route.Kind)
	_, err = svc.Test(ctx, distribution.TestInput{URL: "ftp://x"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
