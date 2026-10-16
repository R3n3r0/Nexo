package iam

import (
	"context"
	"fmt"
	"slices"

	"github.com/r3n3r0/nexo/internal/domain"
)

func errUserNotFound(ak string) error {
	return fmt.Errorf("%w: %q", domain.ErrUserNotFound, ak)
}

func errRootImmutable() error {
	return invalidArg("the root user is configured at startup and cannot be changed")
}

// toUser converts a record to the public type. Callers hold mu.
func (st *state) toUser(r *userRecord) *User {
	return &User{
		AccessKey:   r.AccessKey,
		Status:      r.Status,
		PolicyNames: slices.Clone(r.PolicyNames),
		Groups:      st.groupsOf(r.AccessKey, false),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

// CreateUser implements Service.
func (s *service) CreateUser(ctx context.Context, in CreateUserInput) (*User, error) {
	if err := validateAccessKey(in.AccessKey); err != nil {
		return nil, err
	}
	if err := validateSecretKey(in.SecretKey); err != nil {
		return nil, err
	}
	status, err := parseStatus(in.Status, domain.StatusEnabled)
	if err != nil {
		return nil, err
	}
	policies, err := normalizePolicyNames(in.PolicyNames)
	if err != nil {
		return nil, err
	}
	var out *User
	err = s.withWrite(ctx, func(st *state) error {
		switch {
		case s.isRootKey(in.AccessKey):
			return fmt.Errorf("%w: %q is the root user", domain.ErrUserAlreadyExists, in.AccessKey)
		case st.users[in.AccessKey] != nil:
			return fmt.Errorf("%w: %q", domain.ErrUserAlreadyExists, in.AccessKey)
		case st.accessKeyInUse(in.AccessKey):
			return fmt.Errorf("%w: %q", domain.ErrAccessKeyAlreadyExists, in.AccessKey)
		}
		now := s.now()
		rec := &userRecord{AccessKey: in.AccessKey, SecretKey: in.SecretKey, Status: status, PolicyNames: policies, CreatedAt: now, UpdatedAt: now}
		if err := s.put(ctx, NamespaceUsers, rec.AccessKey, rec); err != nil {
			return err
		}
		st.users[rec.AccessKey] = rec
		out = st.toUser(rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.deps.Logger.InfoContext(ctx, "iam: user created", "accessKey", in.AccessKey)
	return out, nil
}

// GetUser implements Service. Root is reported as an enabled user without
// attached policies.
func (s *service) GetUser(ctx context.Context, accessKey string) (*User, error) {
	var out *User
	err := s.withRead(ctx, func(st *state) error {
		if s.isRootKey(accessKey) {
			out = &User{AccessKey: accessKey, Status: domain.StatusEnabled}
			return nil
		}
		rec := st.users[accessKey]
		if rec == nil {
			return errUserNotFound(accessKey)
		}
		out = st.toUser(rec)
		return nil
	})
	return out, err
}

// ListUsers implements Service.
func (s *service) ListUsers(ctx context.Context) ([]User, error) {
	var out []User
	err := s.withRead(ctx, func(st *state) error {
		out = make([]User, 0, len(st.users))
		for _, k := range sortedKeys(st.users) {
			out = append(out, *st.toUser(st.users[k]))
		}
		return nil
	})
	return out, err
}

// DeleteUser implements Service.
func (s *service) DeleteUser(ctx context.Context, accessKey string) error {
	err := s.withWrite(ctx, func(st *state) error {
		if s.isRootKey(accessKey) {
			return errRootImmutable()
		}
		if st.users[accessKey] == nil {
			return errUserNotFound(accessKey)
		}
		// Derived credentials first: a failure leaves the user in place.
		for _, k := range sortedKeys(st.sts) {
			if st.sts[k].ParentUser == accessKey {
				if err := s.del(ctx, NamespaceSTS, k); err != nil {
					return err
				}
				delete(st.sts, k)
			}
		}
		for _, k := range sortedKeys(st.sas) {
			if st.sas[k].ParentUser == accessKey {
				if err := s.del(ctx, NamespaceServiceAccounts, k); err != nil {
					return err
				}
				delete(st.sas, k)
			}
		}
		for _, name := range st.groupsOf(accessKey, false) {
			g := *st.groups[name]
			g.Members = difference(g.Members, []string{accessKey})
			g.UpdatedAt = s.now()
			if err := s.put(ctx, NamespaceGroups, name, &g); err != nil {
				return err
			}
			st.groups[name] = &g
		}
		if err := s.del(ctx, NamespaceUsers, accessKey); err != nil {
			return err
		}
		delete(st.users, accessKey)
		return nil
	})
	if err == nil {
		s.deps.Logger.InfoContext(ctx, "iam: user deleted", "accessKey", accessKey)
	}
	return err
}

// updateUser applies change to a copy of the user record and persists it.
func (s *service) updateUser(ctx context.Context, accessKey string, change func(u *userRecord)) error {
	return s.withWrite(ctx, func(st *state) error {
		if s.isRootKey(accessKey) {
			return errRootImmutable()
		}
		cur := st.users[accessKey]
		if cur == nil {
			return errUserNotFound(accessKey)
		}
		u := *cur
		change(&u)
		u.UpdatedAt = s.now()
		if err := s.put(ctx, NamespaceUsers, accessKey, &u); err != nil {
			return err
		}
		st.users[accessKey] = &u
		return nil
	})
}

// SetUserSecret implements Service.
func (s *service) SetUserSecret(ctx context.Context, accessKey, secretKey string) error {
	if err := validateSecretKey(secretKey); err != nil {
		return err
	}
	return s.updateUser(ctx, accessKey, func(u *userRecord) { u.SecretKey = secretKey })
}

// SetUserStatus implements Service.
func (s *service) SetUserStatus(ctx context.Context, accessKey string, status domain.CredentialStatus) error {
	st, err := parseStatus(status, "")
	if err != nil {
		return err
	}
	return s.updateUser(ctx, accessKey, func(u *userRecord) {
		if st == domain.StatusDisabled && u.Status != domain.StatusDisabled {
			// A new epoch ends the console sessions for good: enabling
			// the user again does not bring them back.
			u.RevocationEpoch++
		}
		u.Status = st
	})
}

// SetUserPolicy implements Service.
func (s *service) SetUserPolicy(ctx context.Context, accessKey string, policyNames []string) error {
	names, err := normalizePolicyNames(policyNames)
	if err != nil {
		return err
	}
	return s.updateUser(ctx, accessKey, func(u *userRecord) { u.PolicyNames = names })
}
