package auth_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
)

const (
	awsAccessKey = "AKIAIOSFODNN7EXAMPLE"
	awsSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var awsTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

// hmacSHA256 and friends re-implement the SigV4 key derivation so that the
// tests cross-check the implementation rather than reuse it.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func testSigningKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newVerifier returns a verifier whose credential lookup knows the AWS
// example key plus a disabled, an expired and a temporary one.
func newVerifier(t *testing.T, clk clock.Clock, opts ...auth.Option) (auth.Verifier, *authmock.CredentialLookup) {
	t.Helper()
	creds := authmock.NewCredentialLookup(t)
	creds.EXPECT().LookupCredentials(mock.Anything, awsAccessKey).
		Return(domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey, Status: domain.StatusEnabled}, domain.Identity{IsRoot: true}, nil).Maybe()
	creds.EXPECT().LookupCredentials(mock.Anything, "DISABLED").
		Return(domain.Credentials{AccessKey: "DISABLED", SecretKey: "s", Status: domain.StatusDisabled}, domain.Identity{}, nil).Maybe()
	creds.EXPECT().LookupCredentials(mock.Anything, "EXPIRED").
		Return(domain.Credentials{AccessKey: "EXPIRED", SecretKey: "s", Expiration: awsTime.Add(-time.Hour)}, domain.Identity{}, nil).Maybe()
	creds.EXPECT().LookupCredentials(mock.Anything, "TEMP").
		Return(domain.Credentials{AccessKey: "TEMP", SecretKey: "tempsecret", SessionToken: "tok"}, domain.Identity{Parent: "alice"}, nil).Maybe()
	creds.EXPECT().LookupCredentials(mock.Anything, "BROKEN").
		Return(domain.Credentials{}, domain.Identity{}, domain.ErrFaultyDisk).Maybe()
	creds.EXPECT().LookupCredentials(mock.Anything, mock.Anything).
		Return(domain.Credentials{}, domain.Identity{}, domain.ErrInvalidAccessKey).Maybe()
	v, err := auth.NewVerifier(auth.Deps{Creds: creds, Clock: clk}, opts...)
	require.NoError(t, err)
	return v, creds
}

func TestNewVerifierRequiresCreds(t *testing.T) {
	_, err := auth.NewVerifier(auth.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// The GET example of the AWS "Signature Calculations for the Authorization
// Header" documentation.
func TestAWSHeaderVector(t *testing.T) {
	v, _ := newVerifier(t, clock.NewFake(awsTime), auth.WithRegion("us-east-1"))
	req := httptest.NewRequest(http.MethodGet, "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	req.Header.Set("x-amz-content-sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	req.Header.Set("x-amz-date", "20130524T000000Z")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	id, err := v.Verify(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)
	assert.True(t, id.IsRoot)
	assert.False(t, id.IsAnonymous)

	// Any change to a signed element breaks the signature.
	req.Header.Set("Range", "bytes=0-10")
	_, err = v.Verify(context.Background(), req)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
}

// The presigned GET example of the AWS "Authenticating Requests: Using Query
// Parameters" documentation.
func TestAWSPresignedVector(t *testing.T) {
	v, _ := newVerifier(t, clock.NewFake(awsTime.Add(time.Hour)))
	target := "http://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	req := httptest.NewRequest(http.MethodGet, target, nil)
	id, err := v.Verify(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)

	// Expired after 86400 seconds.
	late, _ := newVerifier(t, clock.NewFake(awsTime.Add(25*time.Hour)))
	_, err = late.Verify(context.Background(), httptest.NewRequest(http.MethodGet, target, nil))
	assert.ErrorIs(t, err, domain.ErrRequestExpired)
}

// The streaming PUT example of the AWS "Signature Calculations: Transferring
// Payload in Multiple Chunks" documentation: 65536 + 1024 bytes of "a".
func TestAWSChunkedVector(t *testing.T) {
	v, _ := newVerifier(t, clock.NewFake(awsTime))
	var body bytes.Buffer
	body.WriteString("10000;chunk-signature=ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648\r\n")
	body.Write(bytes.Repeat([]byte("a"), 65536))
	body.WriteString("\r\n400;chunk-signature=0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497\r\n")
	body.Write(bytes.Repeat([]byte("a"), 1024))
	body.WriteString("\r\n0;chunk-signature=b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9\r\n\r\n")

	req := httptest.NewRequest(http.MethodPut, "http://s3.amazonaws.com/examplebucket/chunkObject.txt", bytes.NewReader(body.Bytes()))
	req.ContentLength = 66824
	req.Header.Set("Content-Length", "66824")
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", "66560")
	req.Header.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")
	req.Header.Set("x-amz-content-sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	req.Header.Set("x-amz-date", "20130524T000000Z")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")
	_, err := v.Verify(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, int64(66560), req.ContentLength, "the decoded length replaces the wire length")
	data, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, bytes.Repeat([]byte("a"), 66560), data)
}

func TestSignerVerifierRoundTrip(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	v, _ := newVerifier(t, clk, auth.WithRegion("eu-south-1"))
	s := auth.NewSigner(auth.Deps{Clock: clk}, auth.WithRegion("eu-south-1"))
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}

	payload := []byte("hello sigv4")
	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/my-bucket/path%20with%20spaces/file+plus.txt?tagging&versionId=a%2Fb", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("x-amz-meta-owner", "alice  smith ")
	req.ContentLength = int64(len(payload))
	require.NoError(t, s.Sign(ctx, req, creds, sha256Hex(payload)))
	assert.Contains(t, req.Header.Get("Authorization"), "SignedHeaders=content-length;content-type;host;x-amz-content-sha256;x-amz-date;x-amz-meta-owner")

	id, err := v.Verify(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)
	data, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, data, "the SHA-256 verifier passes the body through")

	// A body that does not match the declared hash is rejected on read.
	req2 := httptest.NewRequest(http.MethodPut, "http://localhost:9000/my-bucket/k", bytes.NewReader([]byte("tampered")))
	req2.ContentLength = 8
	require.NoError(t, s.Sign(ctx, req2, creds, sha256Hex(payload)))
	_, err = v.Verify(ctx, req2)
	require.NoError(t, err)
	_, err = io.ReadAll(req2.Body)
	assert.ErrorIs(t, err, domain.ErrChecksumMismatch)

	// UNSIGNED-PAYLOAD leaves the body alone.
	req3 := httptest.NewRequest(http.MethodPut, "http://localhost:9000/my-bucket/k", bytes.NewReader(payload))
	require.NoError(t, s.Sign(ctx, req3, creds, ""))
	assert.Equal(t, "UNSIGNED-PAYLOAD", req3.Header.Get("x-amz-content-sha256"))
	_, err = v.Verify(ctx, req3)
	require.NoError(t, err)

	// Temporary credentials need the matching token.
	temp := domain.Credentials{AccessKey: "TEMP", SecretKey: "tempsecret", SessionToken: "tok"}
	req4 := httptest.NewRequest(http.MethodGet, "http://localhost:9000/", nil)
	require.NoError(t, s.Sign(ctx, req4, temp, ""))
	id, err = v.Verify(ctx, req4)
	require.NoError(t, err)
	assert.Equal(t, "alice", id.Parent)
	req4.Header.Set("x-amz-security-token", "other")
	_, err = v.Verify(ctx, req4)
	assert.ErrorIs(t, err, domain.ErrAccessDenied, "token mismatch")

	// Anonymous requests pass through as anonymous.
	anon := httptest.NewRequest(http.MethodGet, "http://localhost:9000/public/k", nil)
	id, err = v.Verify(ctx, anon)
	require.NoError(t, err)
	assert.True(t, id.IsAnonymous)
	assert.False(t, auth.IsSigned(anon))
	assert.True(t, auth.IsSigned(req))

	// Signing errors.
	assert.ErrorIs(t, s.Sign(ctx, nil, creds, ""), domain.ErrInvalidArgument)
	assert.ErrorIs(t, s.Sign(ctx, httptest.NewRequest(http.MethodGet, "http://h/", nil), domain.Credentials{}, ""), domain.ErrInvalidArgument)
}

func TestVerifyFailures(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	v, _ := newVerifier(t, clk, auth.WithRegion("us-east-1"), auth.WithMaxClockSkew(5*time.Minute))
	s := auth.NewSigner(auth.Deps{Clock: clk})
	sign := func(accessKey, secret string, mutate func(*http.Request)) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/key", nil)
		require.NoError(t, s.Sign(ctx, req, domain.Credentials{AccessKey: accessKey, SecretKey: secret}, ""))
		if mutate != nil {
			mutate(req)
		}
		return req
	}
	var s3err *domain.S3Error

	_, err := v.Verify(ctx, sign(awsAccessKey, "wrong-secret", nil))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	_, err = v.Verify(ctx, sign("UNKNOWN", "x", nil))
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)
	_, err = v.Verify(ctx, sign("DISABLED", "s", nil))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = v.Verify(ctx, sign("EXPIRED", "s", nil))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = v.Verify(ctx, sign("BROKEN", "s", nil))
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	// Clock skew.
	skewed := clock.NewFake(clk.Now().Add(10 * time.Minute))
	lateV, _ := newVerifier(t, skewed, auth.WithRegion("us-east-1"), auth.WithMaxClockSkew(5*time.Minute))
	_, err = lateV.Verify(ctx, sign(awsAccessKey, awsSecretKey, nil))
	assert.ErrorIs(t, err, domain.ErrRequestExpired)

	// Region mismatch and malformed headers.
	otherRegion := auth.NewSigner(auth.Deps{Clock: clk}, auth.WithRegion("eu-west-1"))
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/key", nil)
	require.NoError(t, otherRegion.Sign(ctx, req, domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}, ""))
	_, err = v.Verify(ctx, req)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "AuthorizationHeaderMalformed", s3err.Code)

	_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) { r.Header.Del("x-amz-date") }))
	assert.ErrorIs(t, err, domain.ErrAccessDenied, "missing date")
	_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) { r.Header.Set("x-amz-date", "yesterday") }))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) {
		r.Header.Set("Authorization", "AWS AKIAIOSFODNN7EXAMPLE:frJIUN8DYpKDtOLCwo//yllqDzg=")
	}))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch, "SigV2 is refused")
	for _, bad := range []string{
		"AWS4-HMAC-SHA256 Credential=x",
		"AWS4-HMAC-SHA256 Credential=a/b/c/d/e, SignedHeaders=host, Signature=zz",
		"AWS4-HMAC-SHA256 Credential=AK/20261008/us-east-1/s3/aws4_request, SignedHeaders=range, Signature=" + strings.Repeat("0", 64),
		"AWS4-HMAC-SHA256 Credential=AK/20261008/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=" + strings.Repeat("0", 64) + ", Extra=1",
		"AWS4-HMAC-SHA256 Credential=AK/20261008/us-east-1/ec2/aws4_request, SignedHeaders=host, Signature=" + strings.Repeat("0", 64),
		"AWS4-HMAC-SHA256 Credential=AK/20261009/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=" + strings.Repeat("0", 64),
	} {
		_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) { r.Header.Set("Authorization", bad) }))
		assert.ErrorIs(t, err, domain.ErrSignatureMismatch, bad)
	}
	// A signed header missing from the request.
	_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) {
		r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "SignedHeaders=host;", "SignedHeaders=host;x-amz-meta-missing;", 1))
	}))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	// Date header instead of x-amz-date is accepted for the time.
	_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) {
		r.Header.Set("Date", clk.Now().Format(http.TimeFormat))
		r.Header.Del("x-amz-date")
	}))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch, "the signature covered x-amz-date, but the time parsed")

	// Invalid content hash declarations.
	_, err = v.Verify(ctx, sign(awsAccessKey, awsSecretKey, func(r *http.Request) {
		r.Header.Set("x-amz-content-sha256", "not-a-hash")
		r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "Signature=", "Signature=", 1))
	}))
	assert.Error(t, err)
}

func TestPresignRoundTrip(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	v, _ := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	target, _ := url.Parse("http://localhost:9000/my-bucket/dir/My File (1).txt?response-content-type=text%2Fplain")

	u, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodGet, URL: target, Expires: time.Hour, Creds: creds, Region: "eu-south-1"})
	require.NoError(t, err)
	q := u.Query()
	assert.Equal(t, "AWS4-HMAC-SHA256", q.Get("X-Amz-Algorithm"))
	assert.Equal(t, "3600", q.Get("X-Amz-Expires"))
	assert.Equal(t, "host", q.Get("X-Amz-SignedHeaders"))
	assert.Equal(t, "text/plain", q.Get("response-content-type"))
	assert.Contains(t, q.Get("X-Amz-Credential"), "/eu-south-1/s3/aws4_request")
	assert.True(t, auth.IsPresigned(q))

	req := httptest.NewRequest(http.MethodGet, u.String(), nil)
	id, err := v.Verify(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)

	// Extra headers can be signed too (content-type for uploads).
	upload, err := s.Presign(ctx, auth.PresignInput{Method: http.MethodPut, URL: target, Expires: time.Minute, Creds: creds,
		Headers: http.Header{"Content-Type": {"image/png"}}})
	require.NoError(t, err)
	assert.Equal(t, "content-type;host", upload.Query().Get("X-Amz-SignedHeaders"))
	putReq := httptest.NewRequest(http.MethodPut, upload.String(), strings.NewReader("png"))
	putReq.Header.Set("Content-Type", "image/png")
	_, err = v.VerifyPresigned(ctx, putReq)
	require.NoError(t, err)
	putReq.Header.Set("Content-Type", "image/jpeg")
	_, err = v.VerifyPresigned(ctx, putReq)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)

	// Failures: tampering, wrong method, expiry, bad parameters.
	tampered := strings.Replace(u.String(), "my-bucket", "other-bucket", 1)
	_, err = v.Verify(ctx, httptest.NewRequest(http.MethodGet, tampered, nil))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	_, err = v.Verify(ctx, httptest.NewRequest(http.MethodDelete, u.String(), nil))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	clk.Advance(2 * time.Hour)
	_, err = v.Verify(ctx, httptest.NewRequest(http.MethodGet, u.String(), nil))
	assert.ErrorIs(t, err, domain.ErrRequestExpired)
	clk.Advance(-2 * time.Hour)
	var s3err *domain.S3Error
	for _, bad := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA1",
		"X-Amz-Credential=garbage",
		"X-Amz-Date=20261008",
		"X-Amz-Expires=0",
		"X-Amz-Expires=9999999",
		"X-Amz-SignedHeaders=range",
		"X-Amz-Signature=short",
	} {
		name, value, _ := strings.Cut(bad, "=")
		bq := u.Query()
		bq.Set(name, value)
		badURL := *u
		badURL.RawQuery = bq.Encode()
		_, err = v.Verify(ctx, httptest.NewRequest(http.MethodGet, badURL.String(), nil))
		require.ErrorIs(t, err, domain.ErrSignatureMismatch, bad)
		require.ErrorAs(t, err, &s3err)
		assert.Equal(t, "AuthorizationQueryParametersError", s3err.Code, bad)
	}
	// Presign input validation.
	_, err = s.Presign(ctx, auth.PresignInput{URL: target, Expires: 0, Creds: creds})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = s.Presign(ctx, auth.PresignInput{URL: target, Expires: 8 * 24 * time.Hour, Creds: creds})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = s.Presign(ctx, auth.PresignInput{Expires: time.Hour, Creds: creds})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Hour})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	// Temporary credentials carry the token in the query.
	temp, err := s.Presign(ctx, auth.PresignInput{URL: target, Expires: time.Hour, Creds: domain.Credentials{AccessKey: "TEMP", SecretKey: "tempsecret", SessionToken: "tok"}})
	require.NoError(t, err)
	assert.Equal(t, "tok", temp.Query().Get("X-Amz-Security-Token"))
	id, err = v.Verify(ctx, httptest.NewRequest(http.MethodGet, temp.String(), nil))
	require.NoError(t, err)
	assert.Equal(t, "alice", id.Parent)
}

// chunkEncoder builds aws-chunked bodies with valid chunk signatures.
type chunkEncoder struct {
	key     []byte
	date    string
	scope   string
	prevSig string
	buf     bytes.Buffer
}

func (c *chunkEncoder) chunk(data []byte) {
	sts := strings.Join([]string{"AWS4-HMAC-SHA256-PAYLOAD", c.date, c.scope, c.prevSig, sha256Hex(nil), sha256Hex(data)}, "\n")
	sig := hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
	fmt.Fprintf(&c.buf, "%x;chunk-signature=%s\r\n", len(data), sig)
	c.buf.Write(data)
	c.buf.WriteString("\r\n")
	c.prevSig = sig
}

// final emits the terminating chunk; without trailers it is followed by
// the empty-data CRLF, with trailers the trailer lines follow directly.
func (c *chunkEncoder) final(withTrailer bool) {
	sts := strings.Join([]string{"AWS4-HMAC-SHA256-PAYLOAD", c.date, c.scope, c.prevSig, sha256Hex(nil), sha256Hex(nil)}, "\n")
	sig := hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
	fmt.Fprintf(&c.buf, "0;chunk-signature=%s\r\n", sig)
	if !withTrailer {
		c.buf.WriteString("\r\n")
	}
	c.prevSig = sig
}

// trailer emits a trailing header and its signature. On the wire lines end
// with CRLF; the signature covers the canonical form "name:value\n".
func (c *chunkEncoder) trailer(name, value string) {
	canonical := name + ":" + value + "\n"
	sts := strings.Join([]string{"AWS4-HMAC-SHA256-TRAILER", c.date, c.scope, c.prevSig, sha256Hex([]byte(canonical))}, "\n")
	sig := hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
	c.buf.WriteString(name + ":" + value + "\r\n")
	c.buf.WriteString("x-amz-trailer-signature:" + sig + "\r\n\r\n")
}

func TestChunkedUploads(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	v, _ := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	date := clk.Now().Format("20060102T150405Z")
	key := testSigningKey(awsSecretKey, clk.Now().Format("20060102"), "us-east-1")
	scope := clk.Now().Format("20060102") + "/us-east-1/s3/aws4_request"

	signStreaming := func(marker string, payload, body []byte) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/bucket/key", nil)
		req.Header.Set("x-amz-decoded-content-length", fmt.Sprint(len(payload)))
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.ContentLength = int64(len(body))
		require.NoError(t, s.Sign(ctx, req, creds, marker))
		req.Body = io.NopCloser(bytes.NewReader(body))
		return req
	}
	seedOf := func(req *http.Request) string {
		_, after, _ := strings.Cut(req.Header.Get("Authorization"), "Signature=")
		return after
	}

	payload := bytes.Repeat([]byte("nexo"), 5000)
	// Signed chunks, no trailer.
	req := signStreaming("STREAMING-AWS4-HMAC-SHA256-PAYLOAD", payload, nil)
	enc := &chunkEncoder{key: key, date: date, scope: scope, prevSig: seedOf(req)}
	enc.chunk(payload[:8192])
	enc.chunk(payload[8192:])
	enc.final(false)
	req.Body = io.NopCloser(bytes.NewReader(enc.buf.Bytes()))
	_, err := v.Verify(ctx, req)
	require.NoError(t, err)
	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.Equal(t, int64(len(payload)), req.ContentLength)

	// Signed chunks with a signed trailer.
	req = signStreaming("STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", payload, nil)
	enc = &chunkEncoder{key: key, date: date, scope: scope, prevSig: seedOf(req)}
	enc.chunk(payload)
	enc.final(true)
	enc.trailer("x-amz-checksum-crc32c", "wdBDMA==")
	req.Body = io.NopCloser(bytes.NewReader(enc.buf.Bytes()))
	_, err = v.Verify(ctx, req)
	require.NoError(t, err)
	got, err = io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	trailers := req.Body.(interface{ Trailers() http.Header }).Trailers()
	assert.Equal(t, "wdBDMA==", trailers.Get("x-amz-checksum-crc32c"))

	// A tampered chunk fails on its signature; a tampered trailer too.
	req = signStreaming("STREAMING-AWS4-HMAC-SHA256-PAYLOAD", payload, nil)
	enc = &chunkEncoder{key: key, date: date, scope: scope, prevSig: seedOf(req)}
	enc.chunk(payload)
	enc.final(false)
	tampered := bytes.Replace(enc.buf.Bytes(), []byte("nexonexo"), []byte("NEXOnexo"), 1)
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	_, err = v.Verify(ctx, req)
	require.NoError(t, err)
	_, err = io.ReadAll(req.Body)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)

	req = signStreaming("STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", payload, nil)
	enc = &chunkEncoder{key: key, date: date, scope: scope, prevSig: seedOf(req)}
	enc.chunk(payload)
	enc.final(true)
	enc.trailer("x-amz-checksum-crc32c", "wdBDMA==")
	req.Body = io.NopCloser(bytes.NewReader(bytes.Replace(enc.buf.Bytes(), []byte("wdBDMA=="), []byte("AAAAAA=="), 1)))
	_, err = v.Verify(ctx, req)
	require.NoError(t, err)
	_, err = io.ReadAll(req.Body)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)

	// Unsigned chunks with trailer.
	var unsigned bytes.Buffer
	fmt.Fprintf(&unsigned, "%x\r\n", len(payload))
	unsigned.Write(payload)
	unsigned.WriteString("\r\n0\r\nx-amz-checksum-sha256:abc=\r\n\r\n")
	req = signStreaming("STREAMING-UNSIGNED-PAYLOAD-TRAILER", payload, unsigned.Bytes())
	_, err = v.Verify(ctx, req)
	require.NoError(t, err)
	got, err = io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.Equal(t, "abc=", req.Body.(interface{ Trailers() http.Header }).Trailers().Get("x-amz-checksum-sha256"))

	// Malformed encodings.
	var s3err *domain.S3Error
	for _, body := range []string{
		"zz\r\nabc\r\n0\r\n\r\n", // bad size
		"3\r\nabcXX0\r\n\r\n",    // missing CRLF after data
		"3\r\nab",                // truncated
		"3;chunk-signature=short\r\nabc\r\n0\r\n\r\n", // signature required
	} {
		marker := "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
		if strings.Contains(body, "chunk-signature") {
			marker = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
		}
		req = signStreaming(marker, []byte("abc"), []byte(body))
		_, err = v.Verify(ctx, req)
		require.NoError(t, err)
		_, err = io.ReadAll(req.Body)
		require.Error(t, err, body)
		require.ErrorAs(t, err, &s3err, body)
		assert.Contains(t, []string{"InvalidChunkEncoding", "IncompleteBody"}, s3err.Code, body)
	}
	// Missing decoded length.
	req = httptest.NewRequest(http.MethodPut, "http://localhost:9000/bucket/key", strings.NewReader("x"))
	require.NoError(t, s.Sign(ctx, req, creds, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"))
	_, err = v.Verify(ctx, req)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestPostPolicy(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	v, _ := newVerifier(t, clk)
	date := clk.Now().Format("20060102T150405Z")
	credential := awsAccessKey + "/" + clk.Now().Format("20060102") + "/us-east-1/s3/aws4_request"
	key := testSigningKey(awsSecretKey, clk.Now().Format("20060102"), "us-east-1")

	policyJSON := fmt.Sprintf(`{"expiration": "%s", "conditions": [
		{"bucket": "uploads"},
		["starts-with", "$key", "user/alice/"],
		{"acl": "private"},
		["eq", "$Content-Type", "image/png"],
		["content-length-range", 1, 1048576],
		{"x-amz-credential": "%s"},
		{"x-amz-algorithm": "AWS4-HMAC-SHA256"},
		{"x-amz-date": "%s"},
		["starts-with", "$x-amz-meta-tag", ""]
	]}`, clk.Now().Add(time.Hour).Format(time.RFC3339), credential, date)
	policyB64 := base64.StdEncoding.EncodeToString([]byte(policyJSON))
	signature := hex.EncodeToString(hmacSHA256(key, []byte(policyB64)))

	form := func(mutate func(map[string]string)) auth.PostForm {
		fields := map[string]string{
			"key": "user/alice/${filename}", "acl": "private", "Content-Type": "image/png",
			"x-amz-credential": credential, "x-amz-algorithm": "AWS4-HMAC-SHA256", "x-amz-date": date,
			"x-amz-meta-tag": "anything", "policy": policyB64, "x-amz-signature": signature, "x-ignore-debug": "1",
		}
		if mutate != nil {
			mutate(fields)
		}
		return auth.PostForm{Bucket: "uploads", Fields: fields, FileName: "photo.png"}
	}

	id, policy, err := v.VerifyPostPolicy(ctx, form(nil))
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)
	require.NotNil(t, policy)
	lo, hi, ok := policy.LengthRange()
	assert.True(t, ok)
	assert.Equal(t, int64(1), lo)
	assert.Equal(t, int64(1048576), hi)
	assert.NoError(t, policy.CheckLength(500))
	assert.ErrorIs(t, policy.CheckLength(0), domain.ErrEntityTooSmall)
	assert.ErrorIs(t, policy.CheckLength(2<<20), domain.ErrEntityTooLarge)
	assert.Len(t, policy.Conditions, 9)

	// Condition failures and extra fields.
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["key"] = "other/${filename}" }))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["acl"] = "public-read" }))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["x-amz-meta-extra"] = "nope" }))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { delete(f, "acl") }))
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	other := form(nil)
	other.Bucket = "another"
	_, _, err = v.VerifyPostPolicy(ctx, other)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// Signature and parameter failures.
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["x-amz-signature"] = strings.Repeat("0", 64) }))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { delete(f, "policy") }))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["x-amz-algorithm"] = "HMAC" }))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["x-amz-credential"] = "bad" }))
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["x-amz-date"] = "today" }))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["x-amz-signature"] = "zz" }))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	// Expired policy (still correctly signed).
	clk.Advance(2 * time.Hour)
	_, _, err = v.VerifyPostPolicy(ctx, form(nil))
	assert.Error(t, err)
	clk.Advance(-2 * time.Hour)

	// Malformed policy documents.
	badDocs := []string{
		`not json`,
		`{"expiration": "soon", "conditions": []}`,
		fmt.Sprintf(`{"expiration": "%s", "conditions": [["eq", "key", "v"]]}`, clk.Now().Add(time.Hour).Format(time.RFC3339)),
		fmt.Sprintf(`{"expiration": "%s", "conditions": [["unknown", "$key", "v"]]}`, clk.Now().Add(time.Hour).Format(time.RFC3339)),
		fmt.Sprintf(`{"expiration": "%s", "conditions": [["content-length-range", 10, 1]]}`, clk.Now().Add(time.Hour).Format(time.RFC3339)),
		fmt.Sprintf(`{"expiration": "%s", "conditions": [{"a": "1", "b": "2"}]}`, clk.Now().Add(time.Hour).Format(time.RFC3339)),
	}
	for _, doc := range badDocs {
		b64 := base64.StdEncoding.EncodeToString([]byte(doc))
		sig := hex.EncodeToString(hmacSHA256(key, []byte(b64)))
		_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["policy"], f["x-amz-signature"] = b64, sig }))
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, doc)
	}
	// Not base64.
	sig := hex.EncodeToString(hmacSHA256(key, []byte("***")))
	_, _, err = v.VerifyPostPolicy(ctx, form(func(f map[string]string) { f["policy"], f["x-amz-signature"] = "***", sig }))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
