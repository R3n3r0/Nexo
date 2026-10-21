package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/idp"
)

// Bounds of the federated credentials (those of iam.AssumeFederated).
const (
	minFederatedDuration = 15 * time.Minute
	maxFederatedDuration = 12 * time.Hour
)

// ldapThrottleKey is the throttled "access key" of an LDAP login: no
// access key contains ':', so it never shares a counter with one.
func ldapThrottleKey(username string) string { return idp.ProviderLDAP + ":" + username }

// LoginLDAP implements Service.
func (s *service) LoginLDAP(ctx context.Context, in LDAPLoginInput) (*Session, error) {
	if s.deps.IDP == nil {
		return nil, fmt.Errorf("%w: session: no identity provider", domain.ErrNotImplemented)
	}
	now := s.deps.Clock.Now()
	key := ldapThrottleKey(in.Username)
	if wait := s.throttle.check(in.Client, key, now); wait > 0 {
		return nil, fmt.Errorf("session: %w", &ThrottledError{RetryAfter: wait})
	}
	id, err := s.deps.IDP.LDAP(ctx, in.Username, in.Password)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDenied) {
			if lockout := s.throttle.fail(in.Client, key, now); lockout > 0 {
				s.deps.Logger.WarnContext(ctx, "session: LDAP console login locked out after repeated failures",
					"user", in.Username, "client", in.Client, "lockout", lockout)
			}
		}
		return nil, err
	}
	s.throttle.reset(in.Client, key)
	return s.federated(ctx, id, now)
}

// LoginOpenID implements Service.
func (s *service) LoginOpenID(ctx context.Context, in OpenIDLoginInput) (*Session, error) {
	if s.deps.IDP == nil {
		return nil, fmt.Errorf("%w: session: no identity provider", domain.ErrNotImplemented)
	}
	id, err := s.deps.IDP.Exchange(ctx, idp.ExchangeInput{
		Code: in.Code, RedirectURI: in.RedirectURI, CodeVerifier: in.CodeVerifier, Nonce: in.Nonce,
	})
	if err != nil {
		return nil, err
	}
	return s.federated(ctx, id, s.deps.Clock.Now())
}

// federated issues the temporary credentials of an identity and a session
// on them. The credentials last as long as the session (the provider
// authenticated the user at the login; the ID token is not a session), so
// that the session ends with them; the Nexo groups and policies of the
// identity are resolved again at each request (Validate).
func (s *service) federated(ctx context.Context, id idp.Identity, now time.Time) (*Session, error) {
	duration := min(max(s.ttl, minFederatedDuration), maxFederatedDuration)
	creds, err := s.deps.IAM.AssumeFederated(ctx, iam.FederatedInput{
		Provider: id.Provider, Subject: id.Subject, Username: id.Username,
		Groups: id.Groups, PolicyNames: id.PolicyNames, Claims: id.Claims,
		Duration: duration, RoleSessionName: "console",
	})
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	full, identity, err := s.deps.IAM.LookupCredentials(ctx, creds.AccessKey)
	if err != nil {
		return nil, fmt.Errorf("session: lookup federated credentials: %w", err)
	}
	exp := now.Add(s.ttl)
	if !full.Expiration.IsZero() && full.Expiration.Before(exp) {
		exp = full.Expiration
	}
	s.deps.Logger.InfoContext(ctx, "session: federated console login", "provider", id.Provider, "user", id.Username, "accessKey", creds.AccessKey)
	return s.issue(creds.AccessKey, full, identity, now, exp)
}
