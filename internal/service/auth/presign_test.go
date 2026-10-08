package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// The presigned GET example of the AWS "Authenticating Requests: Using
// Query Parameters" documentation.
const awsPresignedURL = "http://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"

func TestAWSDocumentationPresignedVector(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		now      time.Time
		sentinel error
		code     string
	}{
		{name: "valid", now: awsTime.Add(time.Hour)},
		{name: "last second", now: awsTime.Add(86400 * time.Second)},
		{name: "within skew before the date", now: awsTime.Add(-10 * time.Minute)},
		{name: "expired", now: awsTime.Add(86401 * time.Second), sentinel: domain.ErrRequestExpired, code: "AccessDenied"},
		{name: "not yet valid", now: awsTime.Add(-16 * time.Minute), sentinel: domain.ErrAccessDenied, code: "AccessDenied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := newVerifier(t, clock.NewFake(tt.now))
			id, err := v.Verify(ctx, httptest.NewRequest(http.MethodGet, awsPresignedURL, nil))
			if tt.sentinel != nil {
				assertS3Error(t, err, tt.sentinel, tt.code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, awsAccessKey, id.AccessKey)
			assert.Equal(t, domain.AuthTypeQuery, id.AuthType)
			assert.Equal(t, signatureAgeAt(tt.now), id.SignatureAge)
		})
	}
}

func signatureAgeAt(now time.Time) time.Duration {
	if d := now.Sub(awsTime); d > 0 {
		return d
	}
	return 0
}

func TestPresignedTampering(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(awsTime.Add(time.Hour)))
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"path", func(r *http.Request) { r.URL.Path = "/test2.txt" }},
		{"host", func(r *http.Request) { r.Host = "otherbucket.s3.amazonaws.com" }},
		{"method", func(r *http.Request) { r.Method = http.MethodDelete }},
		{"parameter added", func(r *http.Request) { r.URL.RawQuery += "&versionId=1" }},
		{"expiry extended", func(r *http.Request) {
			r.URL.RawQuery = strings.Replace(r.URL.RawQuery, "X-Amz-Expires=86400", "X-Amz-Expires=86401", 1)
		}},
		{"signature", func(r *http.Request) {
			r.URL.RawQuery = strings.Replace(r.URL.RawQuery, "Signature=aeee", "Signature=beee", 1)
		}},
		{"signed payload hash added", func(r *http.Request) { r.URL.RawQuery += "&X-Amz-Content-Sha256=" + sha256Hex(nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, awsPresignedURL, nil)
			tt.mutate(req)
			_, err := v.Verify(ctx, req)
			assertS3Error(t, err, domain.ErrSignatureMismatch, "SignatureDoesNotMatch")
		})
	}
}

func TestPresignedParameterErrors(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(awsTime.Add(time.Hour)), auth.WithRegion("us-east-1"))
	base, err := url.Parse(awsPresignedURL)
	require.NoError(t, err)
	for _, bad := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA1",
		"X-Amz-Algorithm=",
		"X-Amz-Credential=garbage",
		"X-Amz-Credential=AKIAIOSFODNN7EXAMPLE/20130525/us-east-1/s3/aws4_request",
		"X-Amz-Credential=AKIAIOSFODNN7EXAMPLE/20130524/eu-west-1/s3/aws4_request",
		"X-Amz-Date=20130524",
		"X-Amz-Expires=0",
		"X-Amz-Expires=-5",
		"X-Amz-Expires=604801",
		"X-Amz-Expires=soon",
		"X-Amz-SignedHeaders=range",
		"X-Amz-SignedHeaders=",
		"X-Amz-Signature=short",
		"X-Amz-Content-Sha256=nonsense",
		"X-Amz-Content-Sha256=STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
	} {
		name, value, _ := strings.Cut(bad, "=")
		q := base.Query()
		q.Set(name, value)
		u := *base
		u.RawQuery = q.Encode()
		_, err := v.Verify(ctx, httptest.NewRequest(http.MethodGet, u.String(), nil))
		assertS3Error(t, err, domain.ErrInvalidArgument, "AuthorizationQueryParametersError", bad)
	}
	// Invalid percent-encoding.
	req := httptest.NewRequest(http.MethodGet, awsPresignedURL, nil)
	req.URL.RawQuery += "&prefix=%zz"
	_, err = v.Verify(ctx, req)
	assertS3Error(t, err, domain.ErrInvalidArgument, "AuthorizationQueryParametersError")
	// Both an Authorization header and a presigned query.
	req = httptest.NewRequest(http.MethodGet, awsPresignedURL, nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=x")
	_, err = v.Verify(ctx, req)
	assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidArgument")
	_, err = v.VerifyPresigned(ctx, req)
	assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidArgument")
}

func TestPresignRoundTrip(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}

	targets := []string{
		"http://localhost:9000/my-bucket/dir/My File (1).txt?response-content-type=text%2Fplain",
		"http://localhost:9000/my-bucket/%C3%A8%E2%82%AC%2B%26%3D%3F%23",
		"http://my-bucket.localhost:9000/key%20with%20space?versionId=abc&response-content-disposition=attachment%3B%20filename%3D%22a%20b.txt%22",
		"http://localhost:9000/my-bucket",
		"http://localhost:9000",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			u, err := url.Parse(target)
			require.NoError(t, err)
			out, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodGet, URL: u, Expires: time.Hour, Creds: creds, Region: "us-east-1"})
			require.NoError(t, err)
			q := out.Query()
			assert.Equal(t, "AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
			assert.Equal(t, "3600", q.Get("X-Amz-Expires"))
			assert.Equal(t, "host", q.Get("X-Amz-SignedHeaders"))
			assert.Equal(t, "20261008T120000Z", q.Get("X-Amz-Date"))
			assert.Equal(t, awsAccessKey+"/20261008/us-east-1/s3/aws4_request", q.Get("X-Amz-Credential"))
			assert.True(t, auth.IsPresigned(q))
			for k, vs := range u.Query() {
				assert.Equal(t, vs, q[k], "the original query is kept")
			}
			// Through the wire: the URL string is what a client sends.
			id, err := v.Verify(ctx, httptest.NewRequest(http.MethodGet, out.String(), nil))
			require.NoError(t, err)
			assert.Equal(t, awsAccessKey, id.AccessKey)
			assert.Equal(t, domain.AuthTypeQuery, id.AuthType)
		})
	}

	target, _ := url.Parse(targets[0])
	t.Run("signed headers", func(t *testing.T) {
		upload, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodPut, URL: target, Expires: time.Minute, Creds: creds,
			Headers: http.Header{"Content-Type": {"image/png"}, "Host": {"ignored"}, "X-Amz-Meta-Empty": nil}})
		require.NoError(t, err)
		assert.Equal(t, "content-type;host", upload.Query().Get("X-Amz-SignedHeaders"))
		putReq := httptest.NewRequest(http.MethodPut, upload.String(), strings.NewReader("png"))
		putReq.Header.Set("Content-Type", "image/png")
		_, err = v.VerifyPresigned(ctx, putReq)
		require.NoError(t, err)
		data, err := io.ReadAll(putReq.Body)
		require.NoError(t, err)
		assert.Equal(t, "png", string(data), "UNSIGNED-PAYLOAD: the body is untouched")
		putReq.Header.Set("Content-Type", "image/jpeg")
		_, err = v.VerifyPresigned(ctx, putReq)
		assertS3Error(t, err, domain.ErrSignatureMismatch, "SignatureDoesNotMatch")
	})

	t.Run("expiry", func(t *testing.T) {
		u, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Second, Creds: creds})
		require.NoError(t, err)
		later := newVerifier(t, clock.NewFake(testNow.Add(2*time.Second)))
		_, err = later.Verify(ctx, httptest.NewRequest(http.MethodGet, u.String(), nil))
		assertS3Error(t, err, domain.ErrRequestExpired, "AccessDenied")
		week, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: 7 * 24 * time.Hour, Creds: creds})
		require.NoError(t, err)
		assert.Equal(t, "604800", week.Query().Get("X-Amz-Expires"))
	})

	t.Run("temporary credentials", func(t *testing.T) {
		temp := domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey, SessionToken: tempToken}
		u, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Hour, Creds: temp})
		require.NoError(t, err)
		assert.Equal(t, tempToken, u.Query().Get("X-Amz-Security-Token"))
		id, err := v.Verify(ctx, httptest.NewRequest(http.MethodGet, u.String(), nil))
		require.NoError(t, err)
		assert.Equal(t, tempParent, id.Parent)
		// Moving the token from the signed query to an unsigned header is
		// rejected: the header is not covered by the signature.
		q := u.Query()
		q.Del("X-Amz-Security-Token")
		noToken := *u
		noToken.RawQuery = q.Encode()
		req := httptest.NewRequest(http.MethodGet, noToken.String(), nil)
		req.Header.Set("X-Amz-Security-Token", tempToken)
		_, err = v.Verify(ctx, req)
		assertS3Error(t, err, domain.ErrAccessDenied, "AccessDenied", "the token header is not signed")
		// The token may also travel as a signed header.
		noQueryToken := domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey}
		signedHeader, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Hour, Creds: noQueryToken,
			Headers: http.Header{"X-Amz-Security-Token": {tempToken}}})
		require.NoError(t, err)
		assert.Empty(t, signedHeader.Query().Get("X-Amz-Security-Token"))
		req = httptest.NewRequest(http.MethodGet, signedHeader.String(), nil)
		req.Header.Set("X-Amz-Security-Token", tempToken)
		id, err = v.Verify(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, tempParent, id.Parent)
		// With the token in the query, a duplicate token header is ignored.
		req = httptest.NewRequest(http.MethodGet, u.String(), nil)
		req.Header.Set("X-Amz-Security-Token", "ignored")
		_, err = v.Verify(ctx, req)
		require.NoError(t, err)
	})

	t.Run("re-presign replaces the previous signature", func(t *testing.T) {
		first, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Hour, Creds: creds})
		require.NoError(t, err)
		second, err := s.Presign(ctx, auth.PresignInput{URL: first, Expires: time.Minute, Creds: creds})
		require.NoError(t, err)
		assert.Len(t, second.Query()["X-Amz-Signature"], 1)
		assert.Equal(t, "60", second.Query().Get("X-Amz-Expires"))
		_, err = v.Verify(ctx, httptest.NewRequest(http.MethodGet, second.String(), nil))
		require.NoError(t, err)
	})

	t.Run("signed payload hash", func(t *testing.T) {
		body := "payload"
		u := *target
		q := u.Query()
		q.Set("X-Amz-Content-Sha256", sha256Hex([]byte(body)))
		u.RawQuery = q.Encode()
		out, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodPut, URL: &u, Expires: time.Hour, Creds: creds})
		require.NoError(t, err)
		// The signer always signs UNSIGNED-PAYLOAD, so the verifier, which
		// honors X-Amz-Content-Sha256, rejects this URL.
		_, err = v.Verify(ctx, httptest.NewRequest(http.MethodPut, out.String(), strings.NewReader(body)))
		assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	})
}

func TestPresignInputValidation(t *testing.T) {
	ctx := context.Background()
	s := auth.NewSigner(auth.Deps{})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	target, _ := url.Parse("http://localhost:9000/b/k")
	relative, _ := url.Parse("/b/k")
	badQuery, _ := url.Parse("http://localhost:9000/b/k")
	badQuery.RawQuery = "a=%zz"
	for name, in := range map[string]auth.PresignInput{
		"zero expiry":     {URL: target, Creds: creds},
		"expiry > 7 days": {URL: target, Expires: 7*24*time.Hour + time.Second, Creds: creds},
		"no URL":          {Expires: time.Hour, Creds: creds},
		"relative URL":    {URL: relative, Expires: time.Hour, Creds: creds},
		"no credentials":  {URL: target, Expires: time.Hour},
		"bad query":       {URL: badQuery, Expires: time.Hour, Creds: creds},
	} {
		_, err := s.Presign(ctx, in)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, name)
	}
	// The wall clock is the default.
	u, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Hour, Creds: creds})
	require.NoError(t, err)
	date, err := time.Parse("20060102T150405Z", u.Query().Get("X-Amz-Date"))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), date, time.Minute)
	assert.Contains(t, u.Query().Get("X-Amz-Credential"), "/us-east-1/s3/aws4_request", "default region")
}

// TestPresignedUnsignedAmzHeaders is the regression test of the
// copy-source injection: a presigned URL authorizes exactly the x-amz-*
// headers it signs, so its holder cannot turn an upload link into a
// CopyObject (or add tags, metadata, storage class...).
func TestPresignedUnsignedAmzHeaders(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	target, err := url.Parse("http://localhost:9000/pubb/upload.txt")
	require.NoError(t, err)
	plain, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodPut, URL: target, Expires: time.Hour, Creds: creds})
	require.NoError(t, err)
	withMeta, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodPut, URL: target, Expires: time.Hour, Creds: creds,
		Headers: http.Header{"X-Amz-Meta-Owner": {"alice"}, "X-Amz-Copy-Source": {"/pubb/source.txt"}}})
	require.NoError(t, err)

	tests := []struct {
		name    string
		url     *url.URL
		headers map[string]string
		denied  bool
	}{
		{name: "no extra header", url: plain},
		{name: "copy source injected", url: plain, headers: map[string]string{"X-Amz-Copy-Source": "/secret/salaries.csv"}, denied: true},
		{name: "tagging injected", url: plain, headers: map[string]string{"X-Amz-Tagging": "a=b"}, denied: true},
		{name: "metadata injected", url: plain, headers: map[string]string{"x-amz-meta-evil": "1"}, denied: true},
		{name: "storage class injected", url: plain, headers: map[string]string{"X-Amz-Storage-Class": "GLACIER"}, denied: true},
		{name: "metadata directive injected", url: plain, headers: map[string]string{"X-Amz-Metadata-Directive": "REPLACE"}, denied: true},
		{name: "unsigned date header", url: plain, headers: map[string]string{"X-Amz-Date": "20261008T120000Z"}, denied: true},
		{name: "payload hash header is exempt", url: plain, headers: map[string]string{"X-Amz-Content-Sha256": "UNSIGNED-PAYLOAD"}},
		{name: "non amz header is free", url: plain, headers: map[string]string{"Content-Type": "text/plain", "X-Custom": "1"}},
		{name: "signed headers accepted", url: withMeta, headers: map[string]string{"X-Amz-Meta-Owner": "alice", "X-Amz-Copy-Source": "/pubb/source.txt"}},
		{name: "unsigned header next to signed ones", url: withMeta,
			headers: map[string]string{"X-Amz-Meta-Owner": "alice", "X-Amz-Copy-Source": "/pubb/source.txt", "X-Amz-Meta-Other": "x"}, denied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, tt.url.String(), strings.NewReader("data"))
			for k, val := range tt.headers {
				req.Header[k] = []string{val}
			}
			id, err := v.Verify(ctx, req)
			if tt.denied {
				assertS3Error(t, err, domain.ErrAccessDenied, "AccessDenied")
				var s3err *domain.S3Error
				require.ErrorAs(t, err, &s3err)
				assert.Equal(t, "There were headers present in the request which were not signed", s3err.Message)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, domain.AuthTypeQuery, id.AuthType)
			assert.Contains(t, id.SignedHeaders, "host")
		})
	}
}
