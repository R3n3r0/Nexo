package integration_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preflight sends a CORS preflight request (unauthenticated, as browsers
// send it).
func (e *env) preflight(path, origin, method, headers string) response {
	e.t.Helper()
	req := newRequest(e.t, http.MethodOptions, e.srv.S3URL+path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method != "" {
		req.Header.Set("Access-Control-Request-Method", method)
	}
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	return do(e.t, req)
}

// TestBucketCORS covers the CORS configuration of a bucket: validation,
// round trip, preflight answers, Access-Control-* headers of the actual
// responses (errors included), Vary: Origin, and the deployment-wide
// policy (api.cors_allow_origin) of the buckets without configuration.
//
// covers: S-BKT-06
func TestBucketCORS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "web"
	e.mkBucket(bucket)
	e.put(bucket, "index.html", []byte("<html/>"))

	// Without configuration: the deployment-wide policy (every origin).
	resp := e.preflight("/"+bucket+"/index.html", "https://any.example", "PUT", "content-type")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	_, err := e.root.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucket)})
	assert.Equal(t, "NoSuchCORSConfiguration", apiCode(err))

	// Validation.
	tooMany := make([]types.CORSRule, 101)
	for i := range tooMany {
		tooMany[i] = types.CORSRule{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}
	}
	_, err = e.root.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(bucket), CORSConfiguration: &types.CORSConfiguration{CORSRules: tooMany}})
	assert.Equal(t, "InvalidRequest", apiCode(err))
	_, err = e.root.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(bucket), CORSConfiguration: &types.CORSConfiguration{
		CORSRules: []types.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"PATCH"}}}}})
	assert.Equal(t, "InvalidRequest", apiCode(err))

	_, err = e.root.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(bucket), CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{
		{ID: aws.String("app"), AllowedOrigins: []string{"https://app.example.com", "https://*.preview.example.com"}, AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"content-type", "x-amz-*"}, ExposeHeaders: []string{"ETag", "x-amz-version-id"}, MaxAgeSeconds: aws.Int32(600)},
		{ID: aws.String("anyone"), AllowedOrigins: []string{"*"}, AllowedMethods: []string{"HEAD"}},
	}}})
	require.NoError(t, err)
	got, err := e.root.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, got.CORSRules, 2)
	assert.Equal(t, "app", aws.ToString(got.CORSRules[0].ID))
	assert.Equal(t, []string{"GET", "PUT"}, got.CORSRules[0].AllowedMethods)
	assert.Equal(t, int32(600), aws.ToInt32(got.CORSRules[0].MaxAgeSeconds))

	// Preflights.
	tests := []struct {
		name, origin, method, headers string
		status                        int
		allowOrigin                   string
	}{
		{"allowed origin, method and headers", "https://app.example.com", "PUT", "Content-Type, X-Amz-Date", http.StatusOK, "https://app.example.com"},
		{"wildcard origin", "https://pr-7.preview.example.com", "GET", "", http.StatusOK, "https://pr-7.preview.example.com"},
		{"any origin for HEAD", "https://other.example", "HEAD", "", http.StatusOK, "*"},
		{"origin not allowed", "https://evil.example", "GET", "", http.StatusForbidden, ""},
		{"method not allowed", "https://app.example.com", "DELETE", "", http.StatusForbidden, ""},
		{"header not allowed", "https://app.example.com", "PUT", "authorization", http.StatusForbidden, ""},
	}
	for _, tt := range tests {
		resp := e.preflight("/"+bucket+"/index.html", tt.origin, tt.method, tt.headers)
		if tt.status != http.StatusOK {
			requireS3Error(t, resp, tt.status, "AccessForbidden")
			continue
		}
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", tt.name, resp.Body)
		assert.Equal(t, tt.allowOrigin, resp.Header.Get("Access-Control-Allow-Origin"), tt.name)
		assert.Contains(t, resp.Header.Values("Vary"), "Origin", tt.name)
	}
	resp = e.preflight("/"+bucket+"/index.html", "https://app.example.com", "PUT", "Content-Type, X-Amz-Date")
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "GET, PUT", resp.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Content-Type, X-Amz-Date", resp.Header.Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "600", resp.Header.Get("Access-Control-Max-Age"))
	assert.Equal(t, "ETag, x-amz-version-id", resp.Header.Get("Access-Control-Expose-Headers"))
	resp = e.preflight("/"+bucket, "https://app.example.com", "GET", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the bucket itself (listings) follows the same rules")
	resp = e.preflight("/"+bucket+"/index.html", "", "GET", "")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a preflight needs an Origin")

	// Actual requests: the headers of the matching rule, errors included.
	resp = e.signedDo(http.MethodGet, "/"+bucket+"/index.html", e.rootCreds(), nil, http.Header{"Origin": {"https://app.example.com"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "ETag, x-amz-version-id", resp.Header.Get("Access-Control-Expose-Headers"))
	assert.Contains(t, resp.Header.Values("Vary"), "Origin")
	resp = e.signedDo(http.MethodGet, "/"+bucket+"/missing", e.rootCreds(), nil, http.Header{"Origin": {"https://app.example.com"}})
	requireS3Error(t, resp, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"), "errors are readable by the page")
	resp = e.signedDo(http.MethodGet, "/"+bucket+"/index.html", e.rootCreds(), nil, http.Header{"Origin": {"https://evil.example"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"), "no rule for this origin")
	resp = e.signedDo(http.MethodHead, "/"+bucket+"/index.html", e.rootCreds(), nil, http.Header{"Origin": {"https://evil.example"}})
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	resp = e.signedDo(http.MethodGet, "/"+bucket+"/index.html", e.rootCreds(), nil, nil)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"), "same-origin requests carry no CORS headers")

	// Without the configuration the deployment-wide policy applies again.
	_, err = e.root.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	resp = e.preflight("/"+bucket+"/index.html", "https://evil.example", "DELETE", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))

	// api.cors_allow_origin narrows it (hot reloaded).
	console := e.consoleLogin(e.rootCreds())
	cresp := console.do(http.MethodPut, "/config/api", `{"settings":{"cors_allow_origin":"https://only.example"}}`)
	require.Equal(t, http.StatusOK, cresp.StatusCode, "%s", cresp.Body)
	resp = e.preflight("/"+bucket+"/index.html", "https://evil.example", "GET", "")
	requireS3Error(t, resp, http.StatusForbidden, "AccessForbidden")
	resp = e.preflight("/"+bucket+"/index.html", "https://only.example", "GET", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "https://only.example", resp.Header.Get("Access-Control-Allow-Origin"))
	resp = e.preflight("/", "https://only.example", "GET", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the service-level requests follow the deployment-wide policy")
	resp = e.preflight("/", "https://evil.example", "GET", "")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	cresp = console.do(http.MethodPut, "/config/api", `{"settings":{"cors_allow_origin":"*"}}`)
	require.Equal(t, http.StatusOK, cresp.StatusCode, "%s", cresp.Body)
	assert.True(t, strings.HasPrefix(e.preflight("/"+bucket, "https://evil.example", "GET", "").Header.Get("Access-Control-Allow-Origin"), "*"))
}
