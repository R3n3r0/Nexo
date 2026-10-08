package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r3n3r0/nexo/internal/domain"
)

// BearerTokenIssuer is the issuer claim of the metrics bearer tokens.
const BearerTokenIssuer = "prometheus"

// bearerMethods are the signing methods accepted for bearer tokens: HMAC
// only, so that the secret key of the subject is the verification key.
func bearerMethods() []string {
	return []string{jwt.SigningMethodHS256.Alg(), jwt.SigningMethodHS384.Alg(), jwt.SigningMethodHS512.Alg()}
}

// errBearer reports a bearer token that does not authenticate. The detail
// reaches the logs, not the client.
func errBearer(detail string) error {
	return &domain.S3Error{
		Code:    codeAccessDenied,
		Message: "The bearer token is not valid.",
		Err:     wrapDetail(domain.ErrAccessDenied, detail),
	}
}

// VerifyBearerToken implements Verifier.
func (v *verifier) VerifyBearerToken(ctx context.Context, token string) (*domain.Identity, error) {
	if token == "" {
		return nil, errBearer("empty token")
	}
	var (
		identity  domain.Identity
		lookupErr error
	)
	keyFunc := func(t *jwt.Token) (any, error) {
		sub, err := t.Claims.GetSubject()
		if err != nil || sub == "" {
			return nil, errors.New("the token has no subject")
		}
		creds, id, err := v.lookup(ctx, sub, "")
		if err != nil {
			lookupErr = err
			return nil, err
		}
		identity = id
		return []byte(creds.SecretKey), nil
	}
	_, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, keyFunc,
		jwt.WithValidMethods(bearerMethods()),
		jwt.WithIssuer(BearerTokenIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.deps.Clock.Now),
	)
	if err != nil {
		if lookupErr != nil && !errors.Is(lookupErr, domain.ErrAccessDenied) && !errors.Is(lookupErr, domain.ErrInvalidAccessKey) {
			return nil, lookupErr // the credential store failed: not the token's fault
		}
		v.deps.Logger.DebugContext(ctx, "auth: bearer token rejected", "err", err)
		return nil, errBearer(err.Error())
	}
	identity.AuthType = domain.AuthTypeBearer
	return &identity, nil
}

// SignBearerToken implements Signer.
func (s *signer) SignBearerToken(_ context.Context, creds domain.Credentials, ttl time.Duration) (string, error) {
	if !validCreds(creds) {
		return "", fmt.Errorf("%w: bearer token requires credentials", domain.ErrInvalidArgument)
	}
	if ttl <= 0 {
		return "", fmt.Errorf("%w: bearer token lifetime must be positive", domain.ErrInvalidArgument)
	}
	now := s.signingTime(time.Time{})
	claims := jwt.RegisteredClaims{
		Subject:   creds.AccessKey,
		Issuer:    BearerTokenIssuer,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(creds.SecretKey))
	if err != nil {
		return "", fmt.Errorf("auth: sign bearer token: %w", err)
	}
	return token, nil
}
