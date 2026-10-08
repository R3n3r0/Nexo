package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Export document: a clear-text envelope with the Argon2id parameters and
// the AES-256-GCM encryption of exportPayload.
const (
	exportFormat  = "nexo-iam-export"
	exportVersion = 1
	exportAAD     = "nexo/iam/export/v1"
	// maxExportSize bounds the documents accepted by Import.
	maxExportSize = 64 << 20
)

type exportKDF struct {
	Algorithm string `json:"algorithm"`
	Salt      []byte `json:"salt"`
	KDFParams
}

type exportEnvelope struct {
	Format  string    `json:"format"`
	Version int       `json:"version"`
	KDF     exportKDF `json:"kdf"`
	Nonce   []byte    `json:"nonce"`
	Data    []byte    `json:"data"`
}

type exportPayload struct {
	ExportedAt      time.Time               `json:"exportedAt"`
	Users           []*userRecord           `json:"users"`
	Groups          []*groupRecord          `json:"groups"`
	ServiceAccounts []*serviceAccountRecord `json:"serviceAccounts"`
	STS             []*stsRecord            `json:"sts"`
}

// rootSecret returns the root secret key, or ErrNotBootstrapped.
func (s *service) rootSecret() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.root == nil {
		return "", ErrNotBootstrapped
	}
	return s.root.SecretKey, nil
}

// Export implements Service.
func (s *service) Export(ctx context.Context) ([]byte, error) {
	secret, err := s.rootSecret()
	if err != nil {
		return nil, err
	}
	payload := exportPayload{ExportedAt: s.now()}
	err = s.withRead(ctx, func(st *state) error {
		for _, k := range sortedKeys(st.users) {
			payload.Users = append(payload.Users, st.users[k])
		}
		for _, k := range sortedKeys(st.groups) {
			payload.Groups = append(payload.Groups, st.groups[k])
		}
		for _, k := range sortedKeys(st.sas) {
			payload.ServiceAccounts = append(payload.ServiceAccounts, st.sas[k])
		}
		for _, k := range sortedKeys(st.sts) {
			payload.STS = append(payload.STS, st.sts[k])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("iam: export: encode: %w", err)
	}
	salt, err := randomBytes(saltSize)
	if err != nil {
		return nil, err
	}
	params := s.opts.exportKDF
	nonce, data, err := seal(deriveKey(secret, salt, params), plain, []byte(exportAAD))
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(exportEnvelope{
		Format: exportFormat, Version: exportVersion,
		KDF:   exportKDF{Algorithm: kdfAlgorithm, Salt: salt, KDFParams: params},
		Nonce: nonce, Data: data,
	})
	if err != nil {
		return nil, fmt.Errorf("iam: export: encode: %w", err)
	}
	return out, nil
}

// decodeExport decrypts and decodes an export document.
func decodeExport(data []byte, secret string) (*exportPayload, error) {
	const what = "export document"
	if len(data) > maxExportSize {
		return nil, corrupted(what, fmt.Errorf("larger than %d bytes", maxExportSize))
	}
	var env exportEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, corrupted(what, err)
	}
	if env.Format != exportFormat || env.KDF.Algorithm != kdfAlgorithm {
		return nil, corrupted(what, fmt.Errorf("unknown format %q/%q", env.Format, env.KDF.Algorithm))
	}
	if env.Version != exportVersion {
		return nil, corrupted(what, fmt.Errorf("unsupported version %d", env.Version))
	}
	if len(env.KDF.Salt) < saltSize {
		return nil, corrupted(what, errors.New("salt too short"))
	}
	if err := env.KDF.validate(); err != nil {
		return nil, corrupted(what, err)
	}
	plain, err := open(deriveKey(secret, env.KDF.Salt, env.KDF.KDFParams), env.Nonce, env.Data, []byte(exportAAD))
	if err != nil {
		if errors.Is(err, errOpen) {
			return nil, fmt.Errorf("%w: %s", ErrWrongKey, what)
		}
		return nil, corrupted(what, err)
	}
	var payload exportPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, corrupted(what, err)
	}
	return &payload, nil
}

// Import implements Service.
func (s *service) Import(ctx context.Context, data []byte) error {
	secret, err := s.rootSecret()
	if err != nil {
		return err
	}
	payload, err := decodeExport(data, secret)
	if err != nil {
		return err
	}
	if err := payload.validate(); err != nil {
		return err
	}
	var imported int
	err = s.withWrite(ctx, func(st *state) error {
		now := s.now()
		for _, u := range payload.Users {
			if s.isRootKey(u.AccessKey) || st.sas[u.AccessKey] != nil || st.sts[u.AccessKey] != nil {
				s.deps.Logger.WarnContext(ctx, "iam: import: user skipped, access key in use", "accessKey", u.AccessKey)
				continue
			}
			if err := s.put(ctx, NamespaceUsers, u.AccessKey, u); err != nil {
				return err
			}
			st.users[u.AccessKey] = u
			imported++
		}
		for _, g := range payload.Groups {
			g.Members = keepUsers(st, g.Members)
			if err := s.put(ctx, NamespaceGroups, g.Name, g); err != nil {
				return err
			}
			st.groups[g.Name] = g
			imported++
		}
		for _, sa := range payload.ServiceAccounts {
			if s.isRootKey(sa.AccessKey) || st.users[sa.AccessKey] != nil || st.sts[sa.AccessKey] != nil {
				s.deps.Logger.WarnContext(ctx, "iam: import: access key skipped, access key in use", "accessKey", sa.AccessKey)
				continue
			}
			if err := s.put(ctx, NamespaceServiceAccounts, sa.AccessKey, sa); err != nil {
				return err
			}
			st.sas[sa.AccessKey] = sa
			imported++
		}
		for _, t := range payload.STS {
			if !now.Before(t.Expiration) || s.isRootKey(t.AccessKey) || st.users[t.AccessKey] != nil || st.sas[t.AccessKey] != nil {
				continue
			}
			if err := s.put(ctx, NamespaceSTS, t.AccessKey, t); err != nil {
				return err
			}
			st.sts[t.AccessKey] = t
			imported++
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.deps.Logger.InfoContext(ctx, "iam: state imported", "entries", imported)
	return nil
}

// keepUsers returns the members that are existing users, sorted.
func keepUsers(st *state, members []string) []string {
	var out []string
	for _, m := range members {
		if st.users[m] != nil {
			out = append(out, m)
		}
	}
	return sortedUnique(out)
}

// validate checks the records of an export before anything is written.
func (p *exportPayload) validate() error {
	for _, u := range p.Users {
		if u == nil {
			return corrupted("export document", errors.New("null user"))
		}
		if err := validateAccessKey(u.AccessKey); err != nil {
			return err
		}
		if err := validateSecretKey(u.SecretKey); err != nil {
			return err
		}
		if _, err := parseStatus(u.Status, ""); err != nil {
			return err
		}
	}
	for _, g := range p.Groups {
		if g == nil {
			return corrupted("export document", errors.New("null group"))
		}
		if err := validateName("group", g.Name, maxGroupNameLen); err != nil {
			return err
		}
		if _, err := parseStatus(g.Status, ""); err != nil {
			return err
		}
	}
	for _, sa := range p.ServiceAccounts {
		if sa == nil {
			return corrupted("export document", errors.New("null access key"))
		}
		if err := validateAccessKey(sa.AccessKey); err != nil {
			return err
		}
		if err := validateSecretKey(sa.SecretKey); err != nil {
			return err
		}
		if sa.ParentUser == "" {
			return invalidArg("access key %q has no parent user", sa.AccessKey)
		}
		if err := validateInlinePolicy(sa.Policy); err != nil {
			return err
		}
	}
	for _, t := range p.STS {
		if t == nil || t.AccessKey == "" || t.SecretKey == "" || t.SessionToken == "" {
			return corrupted("export document", errors.New("incomplete temporary credentials"))
		}
	}
	return nil
}
