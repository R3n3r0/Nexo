package distribution

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Limits of the key groups.
const (
	MaxKeysPerGroup = 10
	minRSABits      = 2048
	maxRSABits      = 4096
)

// parsedKey is a public key ready to verify signatures.
type parsedKey struct {
	group string
	alg   KeyAlgorithm
	pub   crypto.PublicKey
}

// keySet is an immutable index of the key groups and of their keys.
type keySet struct {
	groups map[string]*KeyGroup
	keys   map[string]parsedKey
}

// newKeySet indexes the key groups; a key that cannot be parsed is left
// out (it can verify nothing).
func newKeySet(groups []*KeyGroup) *keySet {
	ks := &keySet{groups: make(map[string]*KeyGroup, len(groups)), keys: make(map[string]parsedKey)}
	for _, g := range groups {
		ks.groups[g.ID] = g
		for _, k := range g.Keys {
			pub, alg, _, err := parsePublicKeyPEM(k.PublicKeyPEM)
			if err != nil {
				continue
			}
			if _, dup := ks.keys[k.ID]; dup {
				continue
			}
			ks.keys[k.ID] = parsedKey{group: g.ID, alg: alg, pub: pub}
		}
	}
	return ks
}

// parsePublicKeyPEM parses a PKIX public key: Ed25519 or RSA of 2048 to
// 4096 bits.
func parsePublicKeyPEM(data string) (crypto.PublicKey, KeyAlgorithm, int, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(data)))
	if block == nil {
		return nil, "", 0, fmt.Errorf("%w: the public key is not PEM", ErrInvalid)
	}
	var (
		pub any
		err error
	)
	switch block.Type {
	case "PUBLIC KEY":
		pub, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		pub, err = x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return nil, "", 0, fmt.Errorf("%w: PEM block %q is not a public key", ErrInvalid, block.Type)
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w: public key: %v", ErrInvalid, err)
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
		return k, KeyEd25519, 0, nil
	case *rsa.PublicKey:
		bits := k.N.BitLen()
		if bits < minRSABits || bits > maxRSABits {
			return nil, "", 0, fmt.Errorf("%w: RSA keys of %d to %d bits only (got %d)", ErrInvalid, minRSABits, maxRSABits, bits)
		}
		return k, KeyRSA, bits, nil
	default:
		return nil, "", 0, fmt.Errorf("%w: only Ed25519 and RSA public keys are supported", ErrInvalid)
	}
}

// loadKeyGroups reads every key group of the store.
func (s *service) loadKeyGroups(ctx context.Context) ([]*KeyGroup, error) {
	docs, err := s.deps.Store.List(ctx, keyGroupsDir)
	if err != nil {
		return nil, fmt.Errorf("distribution: list key groups: %w", err)
	}
	out := make([]*KeyGroup, 0, len(docs))
	for name, data := range docs {
		if !strings.HasSuffix(name, docSuffix) {
			continue
		}
		var g KeyGroup
		if err := json.Unmarshal(data, &g); err != nil || !validID(g.ID) {
			s.deps.Logger.WarnContext(ctx, "distribution: skipping an unreadable key group", "document", name, "err", err)
			continue
		}
		out = append(out, &g)
	}
	slices.SortFunc(out, func(a, b *KeyGroup) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// getKeyGroup reads a key group from the store.
func (s *service) getKeyGroup(ctx context.Context, id string) (KeyGroup, error) {
	if !validID(id) {
		return KeyGroup{}, fmt.Errorf("%w: %q", ErrKeyGroupNotFound, id)
	}
	data, err := s.deps.Store.Get(ctx, docPath(keyGroupsDir, id))
	if errors.Is(err, sysstore.ErrNotFound) {
		return KeyGroup{}, fmt.Errorf("%w: %s", ErrKeyGroupNotFound, id)
	}
	if err != nil {
		return KeyGroup{}, fmt.Errorf("distribution: get key group %s: %w", id, err)
	}
	var g KeyGroup
	if err := json.Unmarshal(data, &g); err != nil {
		return KeyGroup{}, fmt.Errorf("distribution: decode key group %s: %w", id, err)
	}
	return g, nil
}

// putKeyGroup stores a key group and propagates it.
func (s *service) putKeyGroup(ctx context.Context, g KeyGroup) error {
	data, err := json.Marshal(g)
	if err != nil {
		return fmt.Errorf("distribution: encode key group %s: %w", g.ID, err)
	}
	if err := s.deps.Store.Put(ctx, docPath(keyGroupsDir, g.ID), data); err != nil {
		return fmt.Errorf("distribution: store key group %s: %w", g.ID, err)
	}
	s.apply(ctx)
	s.broadcast(ctx, EventKeyGroup, g.ID)
	return nil
}

// CreateKeyGroup implements KeyGroups.
func (s *service) CreateKeyGroup(ctx context.Context, id, comment string) (KeyGroup, error) {
	id = strings.TrimSpace(id)
	if !validID(id) {
		return KeyGroup{}, fmt.Errorf("%w: key group id %q", ErrInvalid, id)
	}
	if !validHeaderValue(comment) || len(comment) > 256 {
		return KeyGroup{}, fmt.Errorf("%w: key group comment", ErrInvalid)
	}
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return KeyGroup{}, err
	}
	defer unlock()
	if _, err := s.getKeyGroup(ctx, id); err == nil {
		return KeyGroup{}, fmt.Errorf("%w: key group %s", ErrAlreadyExists, id)
	} else if !errors.Is(err, ErrKeyGroupNotFound) {
		return KeyGroup{}, err
	}
	now := s.deps.Clock.Now().UTC()
	g := KeyGroup{ID: id, Comment: comment, Keys: []PublicKey{}, CreatedAt: now, UpdatedAt: now}
	if err := s.putKeyGroup(ctx, g); err != nil {
		return KeyGroup{}, err
	}
	return g, nil
}

// GetKeyGroup implements KeyGroups.
func (s *service) GetKeyGroup(ctx context.Context, id string) (KeyGroup, error) {
	return s.getKeyGroup(ctx, id)
}

// ListKeyGroups implements KeyGroups.
func (s *service) ListKeyGroups(ctx context.Context) ([]KeyGroup, error) {
	groups, err := s.loadKeyGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]KeyGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	return out, nil
}

// DeleteKeyGroup implements KeyGroups.
func (s *service) DeleteKeyGroup(ctx context.Context, id string) error {
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.getKeyGroup(ctx, id); err != nil {
		return err
	}
	ds, err := s.loadDistributions(ctx)
	if err != nil {
		return err
	}
	for _, d := range ds {
		for _, b := range append([]Behavior{d.DefaultBehavior}, d.Behaviors...) {
			if b.Signing != nil && b.Signing.KeyGroup == id {
				return fmt.Errorf("%w: key group %s is used by distribution %s", domain.ErrConflict, id, d.ID)
			}
		}
	}
	if err := s.deps.Store.Delete(ctx, docPath(keyGroupsDir, id)); err != nil {
		return fmt.Errorf("distribution: delete key group %s: %w", id, err)
	}
	s.apply(ctx)
	s.broadcast(ctx, EventKeyGroup, id)
	return nil
}

// newKeyID returns a fresh Key-Pair-Id ("K" and 13 base32 characters).
func newKeyID() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "K" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// addKey stores pub in its group under the configuration lock.
func (s *service) addKey(ctx context.Context, group string, key PublicKey) (PublicKey, error) {
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return PublicKey{}, err
	}
	defer unlock()
	g, err := s.getKeyGroup(ctx, group)
	if err != nil {
		return PublicKey{}, err
	}
	if len(g.Keys) >= MaxKeysPerGroup {
		return PublicKey{}, fmt.Errorf("%w: at most %d keys per key group", ErrInvalid, MaxKeysPerGroup)
	}
	groups, err := s.loadKeyGroups(ctx)
	if err != nil {
		return PublicKey{}, err
	}
	used := map[string]bool{}
	for _, other := range groups {
		for _, k := range other.Keys {
			used[k.ID] = true
		}
	}
	for key.ID == "" || used[key.ID] {
		if key.ID, err = newKeyID(); err != nil {
			return PublicKey{}, fmt.Errorf("distribution: key id: %w", err)
		}
	}
	key.CreatedAt = s.deps.Clock.Now().UTC()
	g.Keys = append(g.Keys, key)
	g.UpdatedAt = key.CreatedAt
	if err := s.putKeyGroup(ctx, g); err != nil {
		return PublicKey{}, err
	}
	return key, nil
}

// GenerateKey implements KeyGroups.
func (s *service) GenerateKey(ctx context.Context, in GenerateKeyInput) (GeneratedKey, error) {
	var (
		priv any
		pub  any
		bits int
	)
	switch in.Algorithm {
	case KeyEd25519, "":
		p, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return GeneratedKey{}, fmt.Errorf("distribution: generate key: %w", err)
		}
		pub, priv, in.Algorithm = p, k, KeyEd25519
	case KeyRSA:
		bits = in.Bits
		if bits == 0 {
			bits = minRSABits
		}
		if bits != 2048 && bits != 3072 && bits != 4096 {
			return GeneratedKey{}, fmt.Errorf("%w: RSA keys of 2048, 3072 or 4096 bits", ErrInvalid)
		}
		k, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			return GeneratedKey{}, fmt.Errorf("distribution: generate key: %w", err)
		}
		pub, priv = &k.PublicKey, k
	default:
		return GeneratedKey{}, fmt.Errorf("%w: algorithm %q must be ed25519 or rsa", ErrInvalid, in.Algorithm)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return GeneratedKey{}, fmt.Errorf("distribution: encode public key: %w", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return GeneratedKey{}, fmt.Errorf("distribution: encode private key: %w", err)
	}
	key, err := s.addKey(ctx, in.KeyGroup, PublicKey{
		Algorithm: in.Algorithm, Bits: bits, Comment: in.Comment,
		PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	})
	if err != nil {
		return GeneratedKey{}, err
	}
	return GeneratedKey{PublicKey: key, PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))}, nil
}

// AddPublicKey implements KeyGroups.
func (s *service) AddPublicKey(ctx context.Context, in AddKeyInput) (PublicKey, error) {
	pub, alg, bits, err := parsePublicKeyPEM(in.PEM)
	if err != nil {
		return PublicKey{}, err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: public key: %v", ErrInvalid, err)
	}
	return s.addKey(ctx, in.KeyGroup, PublicKey{
		Algorithm: alg, Bits: bits, Comment: in.Comment,
		PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
	})
}

// DeleteKey implements KeyGroups.
func (s *service) DeleteKey(ctx context.Context, group, keyID string) error {
	unlock, err := s.lockConfig(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	g, err := s.getKeyGroup(ctx, group)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(g.Keys, func(k PublicKey) bool { return k.ID == keyID })
	if i < 0 {
		return fmt.Errorf("%w: %s in key group %s", ErrKeyNotFound, keyID, group)
	}
	g.Keys = slices.Delete(g.Keys, i, i+1)
	g.UpdatedAt = s.deps.Clock.Now().UTC()
	return s.putKeyGroup(ctx, g)
}
