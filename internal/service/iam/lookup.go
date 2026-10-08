package iam

import (
	"context"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

func errInvalidAccessKey(ak string) error {
	return fmt.Errorf("%w: %q", domain.ErrInvalidAccessKey, ak)
}

// LookupCredentials implements Service (and auth.CredentialLookup).
func (s *service) LookupCredentials(ctx context.Context, accessKey string) (domain.Credentials, domain.Identity, error) {
	if accessKey == "" {
		return domain.Credentials{}, domain.Identity{}, errInvalidAccessKey(accessKey)
	}
	var (
		creds domain.Credentials
		id    domain.Identity
	)
	err := s.withRead(ctx, func(st *state) error {
		var err error
		creds, id, err = s.lookup(st, accessKey)
		return err
	})
	if err != nil {
		return domain.Credentials{}, domain.Identity{}, err
	}
	return creds, id, nil
}

// lookup resolves an access key. Callers hold mu.
func (s *service) lookup(st *state, ak string) (domain.Credentials, domain.Identity, error) {
	if s.isRootKey(ak) {
		return *s.root, domain.Identity{AccessKey: ak, IsRoot: true}, nil
	}
	if u := st.users[ak]; u != nil {
		if u.Status == domain.StatusDisabled {
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: user %q is disabled", domain.ErrCredentialsDisabled, ak)
		}
		id := domain.Identity{AccessKey: ak}
		st.fillUser(&id, u)
		return domain.Credentials{AccessKey: ak, SecretKey: u.SecretKey, Status: u.Status}, id, nil
	}
	now := s.now()
	if sa := st.sas[ak]; sa != nil {
		switch {
		case sa.Status == domain.StatusDisabled:
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: access key %q is disabled", domain.ErrCredentialsDisabled, ak)
		case !sa.Expiration.IsZero() && !now.Before(sa.Expiration):
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: access key %q has expired", domain.ErrAccessDenied, ak)
		}
		id := domain.Identity{AccessKey: ak, Parent: sa.ParentUser, SessionPolicy: clonePolicy(sa.Policy)}
		if err := s.fillParent(st, &id, ak); err != nil {
			return domain.Credentials{}, domain.Identity{}, err
		}
		return domain.Credentials{AccessKey: ak, SecretKey: sa.SecretKey, Expiration: sa.Expiration, Status: sa.Status}, id, nil
	}
	if t := st.sts[ak]; t != nil {
		if !now.Before(t.Expiration) {
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: temporary credentials %q have expired", domain.ErrAccessDenied, ak)
		}
		claims, sessionPolicy, err := s.verifySTSToken(t)
		if err != nil {
			return domain.Credentials{}, domain.Identity{}, err
		}
		id := domain.Identity{AccessKey: ak, Parent: t.ParentUser, SessionPolicy: sessionPolicy, Claims: claimsMap(claims)}
		if err := s.fillParent(st, &id, ak); err != nil {
			return domain.Credentials{}, domain.Identity{}, err
		}
		return domain.Credentials{AccessKey: ak, SecretKey: t.SecretKey, SessionToken: t.SessionToken, Expiration: t.Expiration, Status: domain.StatusEnabled}, id, nil
	}
	return domain.Credentials{}, domain.Identity{}, errInvalidAccessKey(ak)
}

// fillUser sets the enabled groups and the effective policies of a user.
func (st *state) fillUser(id *domain.Identity, u *userRecord) {
	id.Groups = st.groupsOf(u.AccessKey, true)
	id.PolicyNames = st.effectivePolicies(u, id.Groups)
}

// fillParent completes the identity of derived credentials from their
// parent: root (IsRoot, limited by the session policy only) or an enabled
// user. A parent that no longer exists makes the key unknown.
func (s *service) fillParent(st *state, id *domain.Identity, ak string) error {
	if s.isRootKey(id.Parent) {
		id.IsRoot = true
		return nil
	}
	u := st.users[id.Parent]
	if u == nil {
		return fmt.Errorf("%w: %q: parent user %q no longer exists", domain.ErrInvalidAccessKey, ak, id.Parent)
	}
	if u.Status == domain.StatusDisabled {
		return fmt.Errorf("%w: parent user %q of %q is disabled", domain.ErrCredentialsDisabled, id.Parent, ak)
	}
	st.fillUser(id, u)
	return nil
}
