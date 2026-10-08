package iam

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
)

// maxGenerateAttempts bounds the retries on generated access key
// collisions (practically never needed).
const maxGenerateAttempts = 8

func errAccessKeyNotFound(ak string) error {
	return fmt.Errorf("%w: %q", domain.ErrAccessKeyNotFound, ak)
}

// toAccessKey converts a record to the public type, without the secret.
func toAccessKey(r *serviceAccountRecord) *AccessKey {
	return &AccessKey{
		AccessKey:   r.AccessKey,
		ParentUser:  r.ParentUser,
		Name:        r.Name,
		Description: r.Description,
		Status:      r.Status,
		Expiration:  r.Expiration,
		Policy:      clonePolicy(r.Policy),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func validateKeyText(name, description string) error {
	if utf8.RuneCountInString(name) > maxKeyNameLen || !utf8.ValidString(name) {
		return invalidArg("access key name must be at most %d characters", maxKeyNameLen)
	}
	if utf8.RuneCountInString(description) > maxDescriptionLen || !utf8.ValidString(description) {
		return invalidArg("access key description must be at most %d characters", maxDescriptionLen)
	}
	return nil
}

// isParent reports whether ak may own service accounts: root or a user.
// Callers hold mu.
func (s *service) isParent(st *state, ak string) bool {
	return s.isRootKey(ak) || st.users[ak] != nil
}

// newAccessKey returns a random access key not in use. Callers hold mu.
func (s *service) newAccessKey(st *state) (string, error) {
	for range maxGenerateAttempts {
		ak, err := randomString(generatedAccessKeyLen, accessKeyAlphabet)
		if err != nil {
			return "", err
		}
		if !s.isRootKey(ak) && !st.accessKeyInUse(ak) {
			return ak, nil
		}
	}
	return "", fmt.Errorf("%w: iam: could not generate a unique access key", domain.ErrConflict)
}

// CreateAccessKey implements Service.
func (s *service) CreateAccessKey(ctx context.Context, in CreateAccessKeyInput) (*AccessKey, error) {
	parent, err := keyParent(in.Caller, in.ParentUser, false)
	if err != nil {
		return nil, err
	}
	if in.AccessKey != "" {
		if err := validateAccessKey(in.AccessKey); err != nil {
			return nil, err
		}
	}
	secret := in.SecretKey
	if secret == "" {
		if secret, err = randomString(generatedSecretKeyLen, secretKeyAlphabet); err != nil {
			return nil, err
		}
	} else if err := validateSecretKey(secret); err != nil {
		return nil, err
	}
	if err := validateKeyText(in.Name, in.Description); err != nil {
		return nil, err
	}
	if err := validateInlinePolicy(in.Policy); err != nil {
		return nil, err
	}
	now := s.now()
	if !in.Expiration.IsZero() && !in.Expiration.After(now) {
		return nil, invalidArg("access key expiration must be in the future")
	}
	var out *AccessKey
	err = s.withWrite(ctx, func(st *state) error {
		if !s.isParent(st, parent) {
			return errUserNotFound(parent)
		}
		ak := in.AccessKey
		if ak == "" {
			var err error
			if ak, err = s.newAccessKey(st); err != nil {
				return err
			}
		} else if s.isRootKey(ak) || st.accessKeyInUse(ak) {
			return fmt.Errorf("%w: %q", domain.ErrAccessKeyAlreadyExists, ak)
		}
		rec := &serviceAccountRecord{
			AccessKey: ak, SecretKey: secret, ParentUser: parent,
			Name: in.Name, Description: in.Description, Status: domain.StatusEnabled,
			Policy: clonePolicy(in.Policy), CreatedAt: now, UpdatedAt: now,
		}
		if !in.Expiration.IsZero() {
			rec.Expiration = in.Expiration.UTC()
		}
		if err := s.put(ctx, NamespaceServiceAccounts, ak, rec); err != nil {
			return err
		}
		st.sas[ak] = rec
		out = toAccessKey(rec)
		out.SecretKey = secret
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.deps.Logger.InfoContext(ctx, "iam: access key created", "accessKey", out.AccessKey, "parent", parent)
	return out, nil
}

// ownedKey returns the service account ak after checking that caller may
// manage it. Callers hold mu.
func ownedKey(st *state, caller domain.Identity, ak string) (*serviceAccountRecord, error) {
	rec := st.sas[ak]
	if rec == nil {
		return nil, errAccessKeyNotFound(ak)
	}
	if err := checkKeyOwner(caller, rec.ParentUser); err != nil {
		return nil, err
	}
	return rec, nil
}

// GetAccessKey implements Service.
func (s *service) GetAccessKey(ctx context.Context, caller domain.Identity, accessKey string) (*AccessKey, error) {
	if err := checkKeyManager(caller); err != nil {
		return nil, err
	}
	var out *AccessKey
	err := s.withRead(ctx, func(st *state) error {
		rec, err := ownedKey(st, caller, accessKey)
		if err != nil {
			return err
		}
		out = toAccessKey(rec)
		return nil
	})
	return out, err
}

// ListAccessKeys implements Service.
func (s *service) ListAccessKeys(ctx context.Context, caller domain.Identity, parent string) ([]AccessKey, error) {
	owner, err := keyParent(caller, parent, true)
	if err != nil {
		return nil, err
	}
	var out []AccessKey
	err = s.withRead(ctx, func(st *state) error {
		if owner != "" && !s.isParent(st, owner) {
			return errUserNotFound(owner)
		}
		out = []AccessKey{}
		for _, k := range sortedKeys(st.sas) {
			if rec := st.sas[k]; owner == "" || rec.ParentUser == owner {
				out = append(out, *toAccessKey(rec))
			}
		}
		return nil
	})
	return out, err
}

// UpdateAccessKey implements Service.
func (s *service) UpdateAccessKey(ctx context.Context, caller domain.Identity, accessKey string, in UpdateAccessKeyInput) (*AccessKey, error) {
	if err := checkKeyManager(caller); err != nil {
		return nil, err
	}
	if in.SecretKey != nil {
		if err := validateSecretKey(*in.SecretKey); err != nil {
			return nil, err
		}
	}
	var status domain.CredentialStatus
	if in.Status != nil {
		var err error
		if status, err = parseStatus(*in.Status, ""); err != nil {
			return nil, err
		}
	}
	name, description := "", ""
	if in.Name != nil {
		name = *in.Name
	}
	if in.Description != nil {
		description = *in.Description
	}
	if err := validateKeyText(name, description); err != nil {
		return nil, err
	}
	if in.ClearPolicy && in.Policy != nil {
		return nil, invalidArg("policy and clearPolicy are mutually exclusive")
	}
	if err := validateInlinePolicy(in.Policy); err != nil {
		return nil, err
	}
	now := s.now()
	if in.Expiration != nil && !in.Expiration.IsZero() && !in.Expiration.After(now) {
		return nil, invalidArg("access key expiration must be in the future")
	}
	var out *AccessKey
	err := s.withWrite(ctx, func(st *state) error {
		cur, err := ownedKey(st, caller, accessKey)
		if err != nil {
			return err
		}
		rec := *cur
		if in.SecretKey != nil {
			rec.SecretKey = *in.SecretKey
		}
		if in.Name != nil {
			rec.Name = name
		}
		if in.Description != nil {
			rec.Description = description
		}
		if in.Status != nil {
			rec.Status = status
		}
		if in.Expiration != nil {
			rec.Expiration = time.Time{}
			if !in.Expiration.IsZero() {
				rec.Expiration = in.Expiration.UTC()
			}
		}
		switch {
		case in.ClearPolicy:
			rec.Policy = nil
		case in.Policy != nil:
			rec.Policy = clonePolicy(in.Policy)
		}
		rec.UpdatedAt = now
		if err := s.put(ctx, NamespaceServiceAccounts, accessKey, &rec); err != nil {
			return err
		}
		st.sas[accessKey] = &rec
		out = toAccessKey(&rec)
		return nil
	})
	return out, err
}

// DeleteAccessKey implements Service.
func (s *service) DeleteAccessKey(ctx context.Context, caller domain.Identity, accessKey string) error {
	if err := checkKeyManager(caller); err != nil {
		return err
	}
	err := s.withWrite(ctx, func(st *state) error {
		if _, err := ownedKey(st, caller, accessKey); err != nil {
			return err
		}
		if err := s.del(ctx, NamespaceServiceAccounts, accessKey); err != nil {
			return err
		}
		delete(st.sas, accessKey)
		return nil
	})
	if err == nil {
		s.deps.Logger.InfoContext(ctx, "iam: access key deleted", "accessKey", accessKey)
	}
	return err
}
