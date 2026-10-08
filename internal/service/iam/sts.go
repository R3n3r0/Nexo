package iam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r3n3r0/nexo/internal/domain"
)

// STS session limits (AWS AssumeRole).
const (
	minSTSDuration     = 15 * time.Minute
	maxSTSDuration     = 12 * time.Hour
	defaultSTSDuration = time.Hour
	stsIssuer          = "nexo-sts"
)

// stsClaims are the claims of an STS session token.
type stsClaims struct {
	jwt.RegisteredClaims
	AccessKey string `json:"accessKey"`
	Parent    string `json:"parent"`
	// SessionPolicy is the base64 of the JSON session policy.
	SessionPolicy   string `json:"sessionPolicy,omitempty"`
	RoleSessionName string `json:"roleSessionName,omitempty"`
}

// signSTSToken issues the session token of temporary credentials.
func signSTSToken(key []byte, ak, parent, sessionName string, p *domain.Policy, issued, exp time.Time) (string, error) {
	cl := stsClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    stsIssuer,
			Subject:   parent,
			ID:        ak,
			IssuedAt:  jwt.NewNumericDate(issued),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		AccessKey:       ak,
		Parent:          parent,
		RoleSessionName: sessionName,
	}
	if p != nil {
		raw, err := json.Marshal(p)
		if err != nil {
			return "", fmt.Errorf("iam: encode session policy: %w", err)
		}
		cl.SessionPolicy = base64.StdEncoding.EncodeToString(raw)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("iam: sign session token: %w", err)
	}
	return token, nil
}

// verifySTSToken checks the signature, issuer and expiry of a session
// token and that it belongs to the record; it returns the claims and the
// decoded session policy. Callers hold mu.
func (s *service) verifySTSToken(rec *stsRecord) (*stsClaims, *domain.Policy, error) {
	if s.stsKey == nil {
		return nil, nil, fmt.Errorf("%w: %q: STS is not available before bootstrap", domain.ErrInvalidAccessKey, rec.AccessKey)
	}
	var cl stsClaims
	_, err := jwt.ParseWithClaims(rec.SessionToken, &cl, func(*jwt.Token) (any, error) { return s.stsKey, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(stsIssuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.deps.Clock.Now),
	)
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return nil, nil, fmt.Errorf("%w: temporary credentials %q have expired", domain.ErrAccessDenied, rec.AccessKey)
	case err != nil:
		return nil, nil, fmt.Errorf("%w: session token of %q is invalid: %w", domain.ErrAccessDenied, rec.AccessKey, err)
	case cl.AccessKey != rec.AccessKey || cl.Parent != rec.ParentUser:
		return nil, nil, fmt.Errorf("%w: session token of %q does not match its credentials", domain.ErrAccessDenied, rec.AccessKey)
	}
	if cl.SessionPolicy == "" {
		return &cl, nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(cl.SessionPolicy)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: session policy of %q: %w", domain.ErrAccessDenied, rec.AccessKey, err)
	}
	p := new(domain.Policy)
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, nil, fmt.Errorf("%w: session policy of %q: %w", domain.ErrAccessDenied, rec.AccessKey, err)
	}
	return &cl, p, nil
}

// claimsMap renders the token claims for Identity.Claims (policy
// conditions jwt:*).
func claimsMap(cl *stsClaims) map[string]any {
	m := map[string]any{
		"iss":       cl.Issuer,
		"sub":       cl.Subject,
		"jti":       cl.ID,
		"accessKey": cl.AccessKey,
		"parent":    cl.Parent,
	}
	if cl.IssuedAt != nil {
		m["iat"] = cl.IssuedAt.Unix()
	}
	if cl.ExpiresAt != nil {
		m["exp"] = cl.ExpiresAt.Unix()
	}
	if cl.RoleSessionName != "" {
		m["roleSessionName"] = cl.RoleSessionName
	}
	return m
}

// AssumeRole implements Service.
func (s *service) AssumeRole(ctx context.Context, in AssumeRoleInput) (*domain.Credentials, error) {
	caller := in.Caller
	switch {
	case caller.IsAnonymous || caller.AccessKey == "":
		return nil, fmt.Errorf("%w: iam: anonymous callers cannot assume a role", domain.ErrAccessDenied)
	case caller.Parent != "":
		return nil, fmt.Errorf("%w: iam: derived credentials cannot assume a role", domain.ErrAccessDenied)
	}
	duration := in.Duration
	if duration == 0 {
		duration = defaultSTSDuration
	}
	if duration < minSTSDuration || duration > maxSTSDuration {
		return nil, invalidArg("duration must be between %d and %d seconds", int(minSTSDuration.Seconds()), int(maxSTSDuration.Seconds()))
	}
	if err := validateInlinePolicy(in.Policy); err != nil {
		return nil, err
	}
	if err := validateSessionName(in.RoleSessionName); err != nil {
		return nil, err
	}
	secret, err := randomString(generatedSecretKeyLen, secretKeyAlphabet)
	if err != nil {
		return nil, err
	}
	var out *domain.Credentials
	err = s.withWrite(ctx, func(st *state) error {
		if s.root == nil {
			return ErrNotBootstrapped
		}
		parent := caller.AccessKey
		if caller.IsRoot != s.isRootKey(parent) {
			return fmt.Errorf("%w: iam: %q is not the root user", domain.ErrAccessDenied, parent)
		}
		if !caller.IsRoot {
			u := st.users[parent]
			if u == nil {
				return fmt.Errorf("%w: iam: unknown user %q", domain.ErrAccessDenied, parent)
			}
			if u.Status == domain.StatusDisabled {
				return fmt.Errorf("%w: iam: user %q is disabled", domain.ErrAccessDenied, parent)
			}
		}
		if err := s.purgeExpiredSTS(ctx, st); err != nil {
			return err
		}
		ak, err := s.newAccessKey(st)
		if err != nil {
			return err
		}
		now := s.now().Truncate(time.Second)
		exp := now.Add(duration)
		token, err := signSTSToken(s.stsKey, ak, parent, in.RoleSessionName, in.Policy, now, exp)
		if err != nil {
			return err
		}
		rec := &stsRecord{AccessKey: ak, SecretKey: secret, SessionToken: token, ParentUser: parent, Expiration: exp, CreatedAt: now}
		if err := s.put(ctx, NamespaceSTS, ak, rec); err != nil {
			return err
		}
		st.sts[ak] = rec
		out = &domain.Credentials{AccessKey: ak, SecretKey: secret, SessionToken: token, Expiration: exp, Status: domain.StatusEnabled}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.deps.Logger.InfoContext(ctx, "iam: temporary credentials issued", "accessKey", out.AccessKey, "parent", caller.AccessKey, "expiration", out.Expiration)
	return out, nil
}

// purgeExpiredSTS deletes the expired STS credentials. Callers hold mu for
// writing.
func (s *service) purgeExpiredSTS(ctx context.Context, st *state) error {
	now := s.now()
	for _, k := range sortedKeys(st.sts) {
		if now.Before(st.sts[k].Expiration) {
			continue
		}
		if err := s.del(ctx, NamespaceSTS, k); err != nil {
			return err
		}
		delete(st.sts, k)
	}
	return nil
}
