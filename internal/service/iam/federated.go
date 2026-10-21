package iam

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Federated credentials (S-IAM-08, S-IAM-09): temporary credentials of an
// identity authenticated by an external provider (OpenID Connect, LDAP).
// It has no IAM user: its record carries the policies and the groups the
// provider mapped it to, and its claims. A group is the Nexo group of the
// same name: its policies apply while it exists and is enabled, checked at
// every lookup. The parent of the credentials is "<provider>:<name>", which
// no IAM user can be (an access key has no ':'), so that no policy attached
// to a user applies to it.

// federatedRecord is the federated part of an stsRecord.
type federatedRecord struct {
	Provider    string         `json:"provider"`
	Subject     string         `json:"subject"`
	Username    string         `json:"username"`
	Groups      []string       `json:"groups,omitempty"`
	PolicyNames []string       `json:"policyNames,omitempty"`
	Claims      map[string]any `json:"claims,omitempty"`
}

// FederatedParent is the parent user of the credentials of a federated
// identity.
func FederatedParent(provider, username string) string { return provider + ":" + username }

// AssumeFederated implements Service.
func (s *service) AssumeFederated(ctx context.Context, in FederatedInput) (*domain.Credentials, error) {
	switch {
	case in.Provider == "" || strings.ContainsAny(in.Provider, ": "):
		return nil, invalidArg("invalid identity provider %q", in.Provider)
	case in.Username == "" || in.Subject == "":
		return nil, fmt.Errorf("%w: iam: the identity has no subject", domain.ErrAccessDenied)
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
	parent := FederatedParent(in.Provider, in.Username)
	var out *domain.Credentials
	err = s.withWrite(ctx, func(st *state) error {
		if s.root == nil {
			return ErrNotBootstrapped
		}
		fed := &federatedRecord{
			Provider: in.Provider, Subject: in.Subject, Username: in.Username,
			Groups: sortedUnique(slices.Clone(in.Groups)), PolicyNames: sortedUnique(slices.Clone(in.PolicyNames)),
			Claims: maps.Clone(in.Claims),
		}
		if _, policies := st.federatedAccess(fed); len(policies) == 0 {
			return fmt.Errorf("%w: iam: %s user %q maps to no policy (no policy claim and no Nexo group with policies)", domain.ErrAccessDenied, in.Provider, in.Username)
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
		if !in.NotAfter.IsZero() && in.NotAfter.Before(exp) {
			// Never beyond the validity of the token of the provider.
			exp = in.NotAfter.Truncate(time.Second)
			if !exp.After(now) {
				return fmt.Errorf("%w: iam: the identity token has expired", domain.ErrAccessDenied)
			}
		}
		token, err := signSTSToken(s.stsKey, ak, parent, in.RoleSessionName, in.Policy, now, exp)
		if err != nil {
			return err
		}
		rec := &stsRecord{AccessKey: ak, SecretKey: secret, SessionToken: token, ParentUser: parent, Expiration: exp, CreatedAt: now, Federated: fed}
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
	s.deps.Logger.InfoContext(ctx, "iam: federated credentials issued", "accessKey", out.AccessKey, "provider", in.Provider, "user", in.Username, "expiration", out.Expiration)
	return out, nil
}

// federatedAccess returns the enabled Nexo groups of a federated identity
// and its effective policies (its own and those of the groups). Callers
// hold mu.
func (st *state) federatedAccess(fed *federatedRecord) (groups, policies []string) {
	names := slices.Clone(fed.PolicyNames)
	for _, g := range fed.Groups {
		rec := st.groups[g]
		if rec == nil || rec.Status == domain.StatusDisabled {
			continue
		}
		groups = append(groups, g)
		names = append(names, rec.PolicyNames...)
	}
	return groups, sortedUnique(names)
}

// fillFederated completes the identity of federated credentials.
func (st *state) fillFederated(id *domain.Identity, fed *federatedRecord) {
	id.Groups, id.PolicyNames = st.federatedAccess(fed)
	if id.Claims == nil {
		id.Claims = map[string]any{}
	}
	for k, v := range fed.Claims {
		// The claims of the provider never shadow those of the
		// credentials (accessKey, parent, exp...).
		if _, ok := id.Claims[k]; !ok {
			id.Claims[k] = v
		}
	}
	id.Claims["provider"] = fed.Provider
	id.Claims["username"] = fed.Username
}
