package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

func TestSign(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	s := auth.NewSigner(auth.Deps{Clock: clk}, auth.WithSigningKeyCacheSize(0))
	v := newVerifier(t, clk)
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}

	t.Run("headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
		req.Header.Set("User-Agent", "nexoctl/1.0")
		req.Header.Set("Authorization", "stale")
		req.Header.Add("X-Amz-Meta-List", "a")
		req.Header.Add("X-Amz-Meta-List", "b")
		req.Header.Set("Range", "bytes=0-1")
		require.NoError(t, s.Sign(ctx, req, creds, strings.ToUpper(sha256Hex(nil))))
		authz := req.Header.Get("Authorization")
		assert.True(t, strings.HasPrefix(authz, "AWS4-HMAC-SHA256 Credential="+awsAccessKey+"/20261008/us-east-1/s3/aws4_request, "))
		assert.Contains(t, authz, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date;x-amz-meta-list, ")
		assert.Equal(t, sha256Hex(nil), req.Header.Get("X-Amz-Content-Sha256"), "the hash is lowercased")
		assert.Equal(t, "20261008T120000Z", req.Header.Get("X-Amz-Date"))
		id, err := v.Verify(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, awsAccessKey, id.AccessKey)
		// The user agent is not signed: proxies may rewrite it.
		req.Header.Set("User-Agent", "proxy")
		_, err = v.Verify(ctx, req)
		require.NoError(t, err)
	})

	t.Run("streaming markers", func(t *testing.T) {
		for _, marker := range []string{"STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "UNSIGNED-PAYLOAD"} {
			req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
			require.NoError(t, s.Sign(ctx, req, creds, marker))
			assert.Equal(t, marker, req.Header.Get("X-Amz-Content-Sha256"))
		}
	})

	t.Run("host from the URL", func(t *testing.T) {
		req := &http.Request{Method: http.MethodGet, URL: &url.URL{Scheme: "http", Host: "example.com:9000", Path: "/b"}}
		require.NoError(t, s.Sign(ctx, req, creds, ""))
		assert.Equal(t, "example.com:9000", req.Host)
		assert.NotEmpty(t, req.Header.Get("Authorization"))
	})

	t.Run("errors", func(t *testing.T) {
		ok := func() *http.Request { return httptest.NewRequest(http.MethodGet, "http://h/", nil) }
		badQuery := ok()
		badQuery.URL.RawQuery = "a=%zz"
		for name, tc := range map[string]struct {
			req   *http.Request
			creds domain.Credentials
			hash  string
		}{
			"nil request":    {nil, creds, ""},
			"nil URL":        {&http.Request{}, creds, ""},
			"no credentials": {ok(), domain.Credentials{AccessKey: "a"}, ""},
			"no host":        {&http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/"}}, creds, ""},
			"bad hash":       {ok(), creds, "not-a-hash"},
			"bad query":      {badQuery, creds, ""},
		} {
			assert.ErrorIs(t, s.Sign(ctx, tc.req, tc.creds, tc.hash), domain.ErrInvalidArgument, name)
		}
	})

	t.Run("wall clock by default", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://h/", nil)
		require.NoError(t, auth.NewSigner(auth.Deps{}).Sign(ctx, req, creds, ""))
		wall := newVerifier(t, clock.NewReal())
		_, err := wall.Verify(ctx, req)
		require.NoError(t, err)
	})
}
