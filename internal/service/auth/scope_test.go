package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

func TestExpectedService(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, auth.ServiceS3, auth.ExpectedService(ctx), "S3 by default")
	assert.Equal(t, auth.ServiceS3, auth.ExpectedService(auth.WithExpectedService(ctx, "")))
	assert.Equal(t, auth.ServiceSTS, auth.ExpectedService(auth.WithExpectedService(ctx, auth.ServiceSTS)))
}

// TestCredentialScopeService is a regression test: a signature is valid for
// the service of its credential scope only; a scope naming another service
// than the one the request is addressed to is malformed authorization data.
func TestCredentialScopeService(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: awsAccessKey, SecretAccessKey: awsSecretKey}

	tests := []struct {
		name     string
		signed   string // service of the credential scope
		expected string // service the request is addressed to ("" = default)
		wantCode string // "" = accepted
	}{
		{name: "s3 for s3", signed: auth.ServiceS3},
		{name: "s3 for s3 explicitly", signed: auth.ServiceS3, expected: auth.ServiceS3},
		{name: "sts for sts", signed: auth.ServiceSTS, expected: auth.ServiceSTS},
		{name: "sts for s3", signed: auth.ServiceSTS, wantCode: "AuthorizationHeaderMalformed"},
		{name: "s3 for sts", signed: auth.ServiceS3, expected: auth.ServiceSTS, wantCode: "AuthorizationHeaderMalformed"},
	}
	for _, tt := range tests {
		t.Run("header "+tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/key", nil)
			require.NoError(t, signer.SignHTTP(ctx, creds, req, sha256Hex(nil), tt.signed, "us-east-1", testNow))
			vctx := ctx
			if tt.expected != "" {
				vctx = auth.WithExpectedService(ctx, tt.expected)
			}
			id, err := v.Verify(vctx, req)
			if tt.wantCode != "" {
				assertS3Error(t, err, domain.ErrInvalidArgument, tt.wantCode)
				assert.Contains(t, err.Error(), "service '"+tt.signed+"' is wrong")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, awsAccessKey, id.AccessKey)
		})
	}

	t.Run("presigned sts URL for s3", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://localhost:9000/bucket/key?X-Amz-Expires=300", nil)
		signed, _, err := signer.PresignHTTP(ctx, creds, req, "UNSIGNED-PAYLOAD", auth.ServiceSTS, "us-east-1", testNow)
		require.NoError(t, err)
		_, err = v.VerifyPresigned(ctx, httptest.NewRequest(http.MethodGet, signed, nil))
		assertS3Error(t, err, domain.ErrInvalidArgument, "AuthorizationQueryParametersError")
		_, err = v.Verify(ctx, httptest.NewRequest(http.MethodGet, signed, nil))
		assertS3Error(t, err, domain.ErrInvalidArgument, "AuthorizationQueryParametersError")
	})

	t.Run("post policy scoped to sts", func(t *testing.T) {
		f := newPostFixture()
		f.credential = awsAccessKey + "/" + testNow.Format("20060102") + "/us-east-1/sts/aws4_request"
		_, _, err := v.VerifyPostPolicy(ctx, f.form(f.policy(`{"bucket": "uploads"}`), nil))
		assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidArgument")
		assert.Contains(t, err.Error(), "service 'sts' is wrong")
	})
}
