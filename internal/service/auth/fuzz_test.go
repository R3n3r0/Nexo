package auth_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// FuzzVerifyAuthorization feeds arbitrary Authorization and X-Amz-Date
// headers to the verifier: it must never panic and never authenticate a
// request whose signature was not computed with the secret.
func FuzzVerifyAuthorization(f *testing.F) {
	get := awsDocRequests()[0].build()
	f.Add(get.Header.Get("Authorization"), awsAmzDate, "bytes=0-9")
	f.Add("AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host,Signature="+strings.Repeat("a", 64), awsAmzDate, "")
	f.Add("AWS AKIAIOSFODNN7EXAMPLE:frJIUN8DYpKDtOLCwo//yllqDzg=", "", "")
	f.Add("AWS4-HMAC-SHA256 ,,,=,", "x", "y")
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(f), Clock: clock.NewFake(awsTime)})
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, authz, date, rng string) {
		req := awsDocRequests()[0].build()
		req.Header.Set("Authorization", authz)
		req.Header.Set("X-Amz-Date", date)
		req.Header.Set("Range", rng)
		id, err := v.Verify(context.Background(), req)
		if err != nil {
			require.Nil(t, id)
			return
		}
		require.NotNil(t, id)
		if !id.IsAnonymous {
			// Only the untouched documentation example verifies.
			require.Equal(t, get.Header.Get("Authorization"), authz)
			require.Equal(t, "bytes=0-9", rng)
		}
	})
}

// FuzzVerifyPresignedQuery feeds arbitrary query strings: no panic, and
// nothing but the original presigned URL verifies.
func FuzzVerifyPresignedQuery(f *testing.F) {
	original := strings.SplitN(awsPresignedURL, "?", 2)[1]
	f.Add(original)
	f.Add("X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=999999999999999999999")
	f.Add("X-Amz-Signature=%zz&a=;b")
	f.Add("AWSAccessKeyId=a&Signature=b&Expires=1")
	f.Add("")
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(f), Clock: clock.NewFake(awsTime.Add(time.Hour))})
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, rawQuery string) {
		req := httptest.NewRequest(http.MethodGet, "http://examplebucket.s3.amazonaws.com/test.txt", nil)
		req.URL.RawQuery = rawQuery
		id, err := v.Verify(context.Background(), req)
		if err != nil {
			require.Nil(t, id)
			return
		}
		if id.IsAnonymous {
			require.Equal(t, domain.AuthTypeAnonymous, id.AuthType)
			return
		}
		require.Equal(t, domain.AuthTypeQuery, id.AuthType)
		require.Equal(t, awsAccessKey, id.AccessKey)
	})
}

// FuzzChunkedBody feeds arbitrary aws-chunked bodies (signed and unsigned
// with trailers): reading never panics and a successful read always
// yields exactly the declared length.
func FuzzChunkedBody(f *testing.F) {
	clk := clock.NewFake(testNow)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(f), Clock: clk})
	require.NoError(f, err)
	f.Add([]byte("3\r\nabc\r\n0\r\nx-amz-checksum-crc32:NSRBwg==\r\n\r\n"), uint8(3), false)
	f.Add([]byte("3;chunk-signature="+strings.Repeat("0", 64)+"\r\nabc\r\n0;chunk-signature="+strings.Repeat("0", 64)+"\r\n\r\n"), uint8(3), true)
	f.Add([]byte("ffffffffffffffff\r\n"), uint8(0), false)
	f.Add([]byte("0\r\n"+strings.Repeat("x-amz-checksum-crc32:a\r\n", 20)), uint8(0), false)
	f.Fuzz(func(t *testing.T, body []byte, decoded uint8, signed bool) {
		marker := "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
		if signed {
			marker = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
		}
		req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
		req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(decoded))
		if !signed {
			req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		}
		require.NoError(t, s.Sign(context.Background(), req, domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}, marker))
		req.Body = io.NopCloser(bytes.NewReader(body))
		_, err := v.Verify(context.Background(), req)
		require.NoError(t, err)
		data, err := io.ReadAll(req.Body)
		if err == nil {
			require.Len(t, data, int(decoded))
		}
	})
}

// FuzzPostPolicyDocument signs arbitrary policy documents: no panic, and
// an accepted form satisfies every eq condition of its policy.
func FuzzPostPolicyDocument(f *testing.F) {
	fx := newPostFixture()
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(f), Clock: clock.NewFake(testNow)})
	require.NoError(f, err)
	f.Add(fx.policy(`{"bucket": "uploads"}`, `["starts-with", "$key", ""]`), "k")
	f.Add(`{"expiration": "2030-01-01T00:00:00Z", "conditions": [["content-length-range", "1", 2]]}`, "")
	f.Add(`{"expiration": 1, "conditions": null}`, "x")
	f.Add(`[`, "")
	f.Fuzz(func(t *testing.T, policyJSON, key string) {
		form := fx.form(policyJSON, map[string]string{"key": key})
		_, policy, err := v.VerifyPostPolicy(context.Background(), form)
		if err != nil {
			return
		}
		for _, c := range policy.Conditions {
			if c.Operator == auth.ConditionEq && c.Key == "key" {
				require.Equal(t, c.Value, key)
			}
		}
	})
}
