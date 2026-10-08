package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

func TestNewVerifierRequiresCreds(t *testing.T) {
	_, err := auth.NewVerifier(auth.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(t)}, nil, auth.WithMaxClockSkew(0), auth.WithSigningKeyCacheSize(-1), auth.WithMaxUnhashedBody(-1))
	require.NoError(t, err, "nil and out-of-range options are ignored")
	id, err := v.Verify(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	assert.True(t, id.IsAnonymous)
}

// awsDocRequest builds one of the header-signed examples of the AWS S3
// documentation ("Signature Calculations for the Authorization Header").
type awsDocRequest struct {
	name      string
	method    string
	target    string
	body      string
	headers   map[string]string
	signed    string
	signature string
}

func (d awsDocRequest) build() *http.Request {
	var body io.Reader
	if d.body != "" {
		body = strings.NewReader(d.body)
	}
	req := httptest.NewRequest(d.method, d.target, body)
	for k, v := range d.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-amz-date", awsAmzDate)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+awsAccessKey+"/"+awsScope+",SignedHeaders="+d.signed+",Signature="+d.signature)
	return req
}

func awsDocRequests() []awsDocRequest {
	return []awsDocRequest{
		{
			name: "GET object", method: http.MethodGet, target: "http://examplebucket.s3.amazonaws.com/test.txt",
			headers:   map[string]string{"Range": "bytes=0-9", "x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			signed:    "host;range;x-amz-content-sha256;x-amz-date",
			signature: "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "PUT object", method: http.MethodPut, target: "http://examplebucket.s3.amazonaws.com/test$file.text", body: "Welcome to Amazon S3.",
			headers: map[string]string{
				"Date": "Fri, 24 May 2013 00:00:00 GMT", "x-amz-storage-class": "REDUCED_REDUNDANCY",
				"x-amz-content-sha256": "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			},
			signed:    "date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
			signature: "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name: "GET bucket lifecycle", method: http.MethodGet, target: "http://examplebucket.s3.amazonaws.com/?lifecycle",
			headers:   map[string]string{"x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			signed:    "host;x-amz-content-sha256;x-amz-date",
			signature: "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "GET bucket (list objects)", method: http.MethodGet, target: "http://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			headers:   map[string]string{"x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			signed:    "host;x-amz-content-sha256;x-amz-date",
			signature: "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
}

func TestAWSDocumentationHeaderVectors(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(awsTime.Add(5*time.Minute)), auth.WithRegion("us-east-1"))
	for _, d := range awsDocRequests() {
		t.Run(d.name, func(t *testing.T) {
			req := d.build()
			id, err := v.Verify(ctx, req)
			require.NoError(t, err)
			assert.Equal(t, awsAccessKey, id.AccessKey)
			assert.True(t, id.IsRoot)
			assert.False(t, id.IsAnonymous)
			assert.Equal(t, domain.AuthTypeHeader, id.AuthType)
			assert.Equal(t, 5*time.Minute, id.SignatureAge)
			data, err := io.ReadAll(req.Body)
			require.NoError(t, err, "the body matches x-amz-content-sha256")
			assert.Equal(t, d.body, string(data))
		})
	}
}

// Every element covered by the signature breaks it when changed.
func TestHeaderSignatureTampering(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(awsTime))
	get := awsDocRequests()[0]
	put := awsDocRequests()[1]
	tests := []struct {
		name   string
		base   awsDocRequest
		mutate func(*http.Request)
	}{
		{"signed header value", get, func(r *http.Request) { r.Header.Set("Range", "bytes=0-10") }},
		{"signed header removed", get, func(r *http.Request) { r.Header.Del("Range") }},
		{"host", get, func(r *http.Request) { r.Host = "otherbucket.s3.amazonaws.com" }},
		{"method", get, func(r *http.Request) { r.Method = http.MethodDelete }},
		{"path", get, func(r *http.Request) { r.URL.Path = "/test.txt2" }},
		{"query added", get, func(r *http.Request) { r.URL.RawQuery = "versionId=1" }},
		{"payload hash", get, func(r *http.Request) { r.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD") }},
		{"date", get, func(r *http.Request) { r.Header.Set("x-amz-date", "20130524T000001Z") }},
		{"signature", get, func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "Signature=f0", "Signature=f1", 1))
		}},
		{"signed headers list", get, func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "host;range;", "host;", 1))
		}},
		{"unsigned date header", put, func(r *http.Request) { r.Header.Set("Date", "Fri, 24 May 2013 00:00:01 GMT") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.base.build()
			tt.mutate(req)
			_, err := v.Verify(ctx, req)
			assertS3Error(t, err, domain.ErrSignatureMismatch, "SignatureDoesNotMatch")
		})
	}
	// Values are trimmed: surrounding spaces do not change the signature.
	req := get.build()
	req.Header.Set("Range", "  bytes=0-9 ")
	_, err := v.Verify(ctx, req)
	require.NoError(t, err)
}

func TestVerifyHeaderErrors(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk, auth.WithRegion("us-east-1"), auth.WithMaxClockSkew(5*time.Minute))
	s := auth.NewSigner(auth.Deps{Clock: clk})
	sign := func(t *testing.T, creds domain.Credentials, mutate func(*http.Request)) *http.Request {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/key", nil)
		require.NoError(t, s.Sign(ctx, req, creds, ""))
		if mutate != nil {
			mutate(req)
		}
		return req
	}
	root := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	key := func(ak string) domain.Credentials { return domain.Credentials{AccessKey: ak, SecretKey: "s"} }
	setAuthz := func(value string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", value) }
	}
	zeros := strings.Repeat("0", 64)
	tests := []struct {
		name     string
		creds    domain.Credentials
		mutate   func(*http.Request)
		sentinel error
		code     string
	}{
		{"wrong secret", domain.Credentials{AccessKey: awsAccessKey, SecretKey: "wrong"}, nil, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"unknown key", key("UNKNOWN"), nil, domain.ErrInvalidAccessKey, "InvalidAccessKeyId"},
		{"deleted user", key("GONE"), nil, domain.ErrInvalidAccessKey, "InvalidAccessKeyId"},
		{"no secret", key("NOSECRET"), nil, domain.ErrInvalidAccessKey, "InvalidAccessKeyId"},
		{"disabled key", key("DISABLED"), nil, domain.ErrAccessDenied, "InvalidAccessKeyId"},
		{"expired key", key("EXPIRED"), nil, domain.ErrAccessDenied, "InvalidAccessKeyId"},
		{"expired session", domain.Credentials{AccessKey: "EXPIREDTEMP", SecretKey: "s", SessionToken: "t"}, nil, domain.ErrAccessDenied, "ExpiredToken"},
		{"lookup failure", key("BROKEN"), nil, domain.ErrFaultyDisk, ""},
		{"wrong session token", domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey, SessionToken: "other"}, nil, domain.ErrAccessDenied, "InvalidToken"},
		{"missing session token", domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey}, nil, domain.ErrAccessDenied, "InvalidToken"},
		{"token for permanent key", domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey, SessionToken: "x"}, nil, domain.ErrAccessDenied, "InvalidToken"},
		{"missing date", root, func(r *http.Request) { r.Header.Del("x-amz-date") }, domain.ErrAccessDenied, "AccessDenied"},
		{"invalid x-amz-date", root, func(r *http.Request) { r.Header.Set("x-amz-date", "yesterday") }, domain.ErrAccessDenied, "AccessDenied"},
		{"invalid Date", root, func(r *http.Request) { r.Header.Del("x-amz-date"); r.Header.Set("Date", "someday") }, domain.ErrAccessDenied, "AccessDenied"},
		{"Date header instead of x-amz-date", root, func(r *http.Request) {
			r.Header.Set("Date", testNow.Format(http.TimeFormat))
			r.Header.Del("x-amz-date")
		}, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"signature version 2", root, setAuthz("AWS AKIAIOSFODNN7EXAMPLE:frJIUN8DYpKDtOLCwo//yllqDzg="), domain.ErrInvalidArgument, "InvalidRequest"},
		{"unknown scheme", root, setAuthz("Bearer token"), domain.ErrInvalidArgument, "InvalidRequest"},
		{"presigned and header", root, func(r *http.Request) { r.URL.RawQuery = "X-Amz-Signature=" + zeros }, domain.ErrInvalidArgument, "InvalidArgument"},
		{"invalid content hash", root, func(r *http.Request) { r.Header.Set("x-amz-content-sha256", "not-a-hash") }, domain.ErrInvalidArgument, "InvalidArgument"},
		{"invalid query encoding", root, func(r *http.Request) { r.URL.RawQuery = "prefix=%zz" }, domain.ErrInvalidArgument, "InvalidArgument"},
		{"missing signed header", root, func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "SignedHeaders=host;", "SignedHeaders=host;x-amz-meta-missing;", 1))
		}, domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"credential date mismatch", root, setAuthz("AWS4-HMAC-SHA256 Credential=" + awsAccessKey + "/20261009/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + zeros), domain.ErrInvalidArgument, "AuthorizationHeaderMalformed"},
		{"wrong region", root, setAuthz("AWS4-HMAC-SHA256 Credential=" + awsAccessKey + "/20261008/eu-west-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + zeros), domain.ErrInvalidArgument, "AuthorizationHeaderMalformed"},
		{"unsupported service", root, setAuthz("AWS4-HMAC-SHA256 Credential=" + awsAccessKey + "/20261008/us-east-1/ec2/aws4_request, SignedHeaders=host;x-amz-date, Signature=" + zeros), domain.ErrInvalidArgument, "AuthorizationHeaderMalformed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.Verify(ctx, sign(t, tt.creds, tt.mutate))
			assertS3Error(t, err, tt.sentinel, tt.code)
		})
	}

	t.Run("clock skew", func(t *testing.T) {
		for _, d := range []time.Duration{-6 * time.Minute, 6 * time.Minute} {
			skewed := newVerifier(t, clock.NewFake(testNow.Add(d)), auth.WithMaxClockSkew(5*time.Minute))
			_, err := skewed.Verify(ctx, sign(t, root, nil))
			assertS3Error(t, err, domain.ErrRequestExpired, "RequestTimeTooSkewed", d)
		}
		within := newVerifier(t, clock.NewFake(testNow.Add(4*time.Minute)), auth.WithMaxClockSkew(5*time.Minute))
		id, err := within.Verify(ctx, sign(t, root, nil))
		require.NoError(t, err)
		assert.Equal(t, 4*time.Minute, id.SignatureAge)
		early := newVerifier(t, clock.NewFake(testNow.Add(-4*time.Minute)), auth.WithMaxClockSkew(5*time.Minute))
		id, err = early.Verify(ctx, sign(t, root, nil))
		require.NoError(t, err)
		assert.Zero(t, id.SignatureAge, "a signature from the future has no age")
	})

	t.Run("default skew is 15 minutes", func(t *testing.T) {
		def := newVerifier(t, clock.NewFake(testNow.Add(14*time.Minute)))
		_, err := def.Verify(ctx, sign(t, root, nil))
		require.NoError(t, err)
		def = newVerifier(t, clock.NewFake(testNow.Add(16*time.Minute)))
		_, err = def.Verify(ctx, sign(t, root, nil))
		assert.ErrorIs(t, err, domain.ErrRequestExpired)
	})

	t.Run("nil request", func(t *testing.T) {
		_, err := v.Verify(ctx, nil)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = v.VerifyPresigned(ctx, nil)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}

func TestMalformedAuthorizationHeaders(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(testNow))
	zeros := strings.Repeat("0", 64)
	cred := "Credential=AK/20261008/us-east-1/s3/aws4_request"
	for _, bad := range []string{
		"AWS4-HMAC-SHA256",
		"AWS4-HMAC-SHA256X Credential=a",
		"AWS4-HMAC-SHA256 Credential=x",
		"AWS4-HMAC-SHA256 " + cred,
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host",
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host, Signature=zz",
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=range, Signature=" + zeros,
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=, Signature=" + zeros,
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host;bad header, Signature=" + zeros,
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host, Signature=" + zeros + ", Extra=1",
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host, Signature=" + zeros + ", Signature=" + zeros,
		"AWS4-HMAC-SHA256 " + cred + ", " + cred + ", SignedHeaders=host, Signature=" + zeros,
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host, SignedHeaders=host, Signature=" + zeros,
		"AWS4-HMAC-SHA256 " + cred + ", SignedHeaders=host, NoEquals",
		"AWS4-HMAC-SHA256 Credential=/20261008/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=" + zeros,
		"AWS4-HMAC-SHA256 Credential=AK/2026-10-08/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=" + zeros,
		"AWS4-HMAC-SHA256 Credential=AK/20261008//s3/aws4_request, SignedHeaders=host, Signature=" + zeros,
		"AWS4-HMAC-SHA256 Credential=AK/20261008/us-east-1/s3/aws5_request, SignedHeaders=host, Signature=" + zeros,
	} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/key", nil)
		req.Header.Set("x-amz-date", testNow.Format("20060102T150405Z"))
		req.Header.Set("Authorization", bad)
		_, err := v.Verify(ctx, req)
		assertS3Error(t, err, domain.ErrInvalidArgument, "AuthorizationHeaderMalformed", bad)
	}
}

func TestAnonymousRequests(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(testNow))
	for _, target := range []string{"http://localhost:9000/public/k", "http://localhost:9000/public/k?prefix=a%zz", "http://localhost:9000/?list-type=2"} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/", nil)
		req.URL.RawQuery = strings.SplitN(target+"?", "?", 2)[1]
		id, err := v.Verify(ctx, req)
		require.NoError(t, err, target)
		assert.True(t, id.IsAnonymous)
		assert.Equal(t, domain.AuthTypeAnonymous, id.AuthType)
		assert.Empty(t, id.AccessKey)
		assert.False(t, auth.IsSigned(req))
	}
	// Signature Version 2 presigned URLs are refused, not treated as
	// anonymous.
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k?AWSAccessKeyId=AK&Expires=1&Signature=abc", nil)
	_, err := v.Verify(ctx, req)
	assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidRequest")
}

func TestSignerVerifierRoundTrip(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk, auth.WithRegion("eu-south-1"))
	s := auth.NewSigner(auth.Deps{Clock: clk}, auth.WithRegion("eu-south-1"))
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	payload := []byte("hello sigv4")

	targets := []string{
		"http://localhost:9000/my-bucket/path%20with%20spaces/file+plus.txt?tagging&versionId=a%2Fb",
		"http://localhost:9000/my-bucket/%C3%A8%20%E2%82%AC/%2A%28%29%21%27/x?prefix=a+b&delimiter=%2F",
		"http://localhost:9000/my-bucket/semi;colon/at@sign/$dollar&amp=?list-type=2&start-after=a;b",
		"http://localhost:9000/my-bucket/a%2Fb/encoded-slash",
		"http://localhost:9000/",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("x-amz-meta-owner", "alice  smith ")
			require.NoError(t, s.Sign(ctx, req, creds, sha256Hex(payload)))
			assert.Contains(t, req.Header.Get("Authorization"), "SignedHeaders=content-length;content-type;host;x-amz-content-sha256;x-amz-date;x-amz-meta-owner")
			id, err := v.Verify(ctx, req)
			require.NoError(t, err)
			assert.Equal(t, awsAccessKey, id.AccessKey)
			data, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			assert.Equal(t, payload, data, "the SHA-256 verifier passes the body through")
			require.NoError(t, req.Body.Close())
		})
	}

	// UNSIGNED-PAYLOAD leaves the body alone.
	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/my-bucket/k", strings.NewReader("anything"))
	require.NoError(t, s.Sign(ctx, req, creds, ""))
	assert.Equal(t, "UNSIGNED-PAYLOAD", req.Header.Get("x-amz-content-sha256"))
	_, err := v.Verify(ctx, req)
	require.NoError(t, err)
	data, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, "anything", string(data))

	// Temporary credentials carry the session token.
	temp := domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey, SessionToken: tempToken}
	req = httptest.NewRequest(http.MethodGet, "http://localhost:9000/", nil)
	require.NoError(t, s.Sign(ctx, req, temp, ""))
	assert.Equal(t, tempToken, req.Header.Get("X-Amz-Security-Token"))
	id, err := v.Verify(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, tempParent, id.Parent)
	assert.Equal(t, tempAccessKey, id.AccessKey)
	assert.True(t, auth.IsSigned(req))
}

func TestPayloadHashVerification(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	payload := []byte("the declared payload")

	signed := func(t *testing.T, body string, declared string) *http.Request {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/b/k", strings.NewReader(body))
		require.NoError(t, s.Sign(ctx, req, creds, declared))
		_, err := v.Verify(ctx, req)
		require.NoError(t, err)
		return req
	}

	t.Run("mismatch detected at EOF", func(t *testing.T) {
		req := signed(t, "tampered payload....", sha256Hex(payload))
		_, err := io.ReadAll(req.Body)
		assertS3Error(t, err, domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch")
		_, err = req.Body.Read(make([]byte, 1))
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch, "the error is sticky")
	})
	t.Run("mismatch detected reading exactly Content-Length", func(t *testing.T) {
		// io.ReadFull drops the error of the read that fills the buffer:
		// the verifier withholds the last bytes instead.
		req := signed(t, "tampered payload....", sha256Hex(payload))
		buf := make([]byte, req.ContentLength)
		n, err := io.ReadFull(req.Body, buf)
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		assert.Less(t, int64(n), req.ContentLength)
		req = signed(t, "tampered payload....", sha256Hex(payload))
		var sink strings.Builder
		_, err = io.CopyN(&sink, req.Body, req.ContentLength)
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		req = signed(t, string(payload), sha256Hex(payload))
		n, err = io.ReadFull(req.Body, buf)
		require.NoError(t, err)
		assert.Equal(t, payload, buf[:n])
	})
	t.Run("body longer than Content-Length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/b/k", strings.NewReader(string(payload)+"extra"))
		req.ContentLength = int64(len(payload))
		require.NoError(t, s.Sign(ctx, req, creds, sha256Hex(payload)))
		_, err := v.Verify(ctx, req)
		require.NoError(t, err)
		_, err = io.ReadAll(req.Body)
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
	})
	t.Run("upper-case hash", func(t *testing.T) {
		req := signed(t, string(payload), strings.ToUpper(sha256Hex(payload)))
		data, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, data)
	})
	t.Run("empty body with a non-empty hash", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
		require.NoError(t, s.Sign(ctx, req, creds, sha256Hex(payload)))
		_, err := v.Verify(ctx, req)
		assertS3Error(t, err, domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch")
	})
	t.Run("empty body with the empty hash", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/b/k", nil)
		require.NoError(t, s.Sign(ctx, req, creds, sha256Hex(nil)))
		_, err := v.Verify(ctx, req)
		require.NoError(t, err)
	})
}

// TestHeaderUnsignedAmzHeaders: a header-signed request carries only the
// x-amz-* headers its signature covers, so a man in the middle cannot add
// x-amz-copy-source or metadata to a signed request. X-Amz-Date and
// X-Amz-Content-Sha256 are bound to the signature through the string to
// sign and the payload hash line.
func TestHeaderUnsignedAmzHeaders(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(awsTime))
	lifecycle := awsDocRequests()[2] // signs host;x-amz-content-sha256;x-amz-date
	tests := []struct {
		name    string
		headers map[string]string
		denied  bool
	}{
		{name: "no extra header"},
		{name: "copy source injected", headers: map[string]string{"X-Amz-Copy-Source": "/secret/salaries.csv"}, denied: true},
		{name: "metadata injected", headers: map[string]string{"x-amz-meta-evil": "1"}, denied: true},
		{name: "security token injected", headers: map[string]string{"X-Amz-Security-Token": "t"}, denied: true},
		{name: "non amz header is free", headers: map[string]string{"X-Forwarded-For": "10.0.0.1", "User-Agent": "test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := lifecycle.build()
			for k, val := range tt.headers {
				req.Header.Set(k, val)
			}
			id, err := v.Verify(ctx, req)
			if tt.denied {
				assertS3Error(t, err, domain.ErrAccessDenied, "AccessDenied")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"host", "x-amz-content-sha256", "x-amz-date"}, id.SignedHeaders)
		})
	}
	// A request that does not list the bound headers still verifies them
	// through the signature: the PUT example signs date;host;... and the
	// payload hash header, but here X-Amz-Date alone is unsigned.
	put := awsDocRequests()[1]
	put.signed = "date;host;x-amz-content-sha256;x-amz-storage-class"
	req := put.build()
	_, err := v.Verify(ctx, req)
	assertS3Error(t, err, domain.ErrSignatureMismatch, "SignatureDoesNotMatch", "the exempt date is still part of the string to sign")
}
