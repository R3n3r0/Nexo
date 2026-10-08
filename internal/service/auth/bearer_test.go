package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// bearerToken signs claims with key using method.
func bearerToken(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	require.NoError(t, err)
	return s
}

func TestVerifyBearerToken(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewFake(testNow))
	exp := testNow.Add(time.Hour).Unix()
	valid := jwt.MapClaims{"sub": awsAccessKey, "iss": "prometheus", "exp": exp}
	with := func(k string, val any) jwt.MapClaims {
		c := jwt.MapClaims{}
		for key, v := range valid {
			c[key] = v
		}
		if val == nil {
			delete(c, k)
		} else {
			c[k] = val
		}
		return c
	}
	secret := []byte(awsSecretKey)
	tests := []struct {
		name     string
		token    string
		sentinel error
	}{
		{name: "HS512 as mc admin prometheus generate", token: bearerToken(t, jwt.SigningMethodHS512, secret, valid)},
		{name: "HS256", token: bearerToken(t, jwt.SigningMethodHS256, secret, valid)},
		{name: "empty", token: "", sentinel: domain.ErrAccessDenied},
		{name: "garbage", token: "not.a.jwt", sentinel: domain.ErrAccessDenied},
		{name: "wrong secret", token: bearerToken(t, jwt.SigningMethodHS512, []byte("other"), valid), sentinel: domain.ErrAccessDenied},
		{name: "expired", token: bearerToken(t, jwt.SigningMethodHS512, secret, with("exp", testNow.Add(-time.Second).Unix())), sentinel: domain.ErrAccessDenied},
		{name: "no expiry", token: bearerToken(t, jwt.SigningMethodHS512, secret, with("exp", nil)), sentinel: domain.ErrAccessDenied},
		{name: "other issuer", token: bearerToken(t, jwt.SigningMethodHS512, secret, with("iss", "nexo")), sentinel: domain.ErrAccessDenied},
		{name: "no subject", token: bearerToken(t, jwt.SigningMethodHS512, secret, with("sub", nil)), sentinel: domain.ErrAccessDenied},
		{name: "unknown access key", token: bearerToken(t, jwt.SigningMethodHS512, secret, with("sub", "ghost")), sentinel: domain.ErrAccessDenied},
		{name: "disabled access key", token: bearerToken(t, jwt.SigningMethodHS512, []byte("s"), with("sub", "DISABLED")), sentinel: domain.ErrAccessDenied},
		{name: "temporary credentials", token: bearerToken(t, jwt.SigningMethodHS512, []byte(tempSecretKey), with("sub", tempAccessKey)), sentinel: domain.ErrAccessDenied},
		{name: "unsigned", token: bearerToken(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid), sentinel: domain.ErrAccessDenied},
		{name: "credential store failure", token: bearerToken(t, jwt.SigningMethodHS512, secret, with("sub", "BROKEN")), sentinel: domain.ErrFaultyDisk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := v.VerifyBearerToken(ctx, tt.token)
			if tt.sentinel != nil {
				require.ErrorIs(t, err, tt.sentinel)
				assert.Nil(t, id)
				return
			}
			require.NoError(t, err)
			assert.True(t, id.IsRoot)
			assert.Equal(t, awsAccessKey, id.AccessKey)
			assert.Equal(t, domain.AuthTypeBearer, id.AuthType)
		})
	}
	// A bearer token is never an S3 credential.
	req := newBearerRequest(bearerToken(t, jwt.SigningMethodHS512, secret, valid))
	_, err := v.Verify(ctx, req)
	assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidRequest")
}

func TestSignBearerToken(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	token, err := s.SignBearerToken(ctx, creds, time.Hour)
	require.NoError(t, err)
	id, err := newVerifier(t, clk).VerifyBearerToken(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)
	_, err = newVerifier(t, clock.NewFake(testNow.Add(time.Hour+time.Second))).VerifyBearerToken(ctx, token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied, "the token expires")

	parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	require.NoError(t, err)
	assert.Equal(t, "HS512", parsed.Method.Alg())
	iss, err := parsed.Claims.GetIssuer()
	require.NoError(t, err)
	assert.Equal(t, auth.BearerTokenIssuer, iss)

	_, err = s.SignBearerToken(ctx, domain.Credentials{AccessKey: "a"}, time.Hour)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = s.SignBearerToken(ctx, creds, 0)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
