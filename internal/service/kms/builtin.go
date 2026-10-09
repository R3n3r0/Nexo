package kms

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Layout of a data key sealed by the builtin KMS:
//
//	version (1) | master fingerprint (8) | salt (32) | nonce (12) | AES-256-GCM(DEK) + tag (48)
//
// The key encryption key is HKDF-SHA256(master, salt, "nexo-kms-v1/<key id>"):
// one derived key per named key, fresh for every seal; the encryption
// context (canonical JSON) is the additional authenticated data.
const (
	sealVersion     = 1
	fingerprintSize = 8
	saltSize        = 32
	nonceSize       = 12
	dekSize         = 32
	sealedSize      = 1 + fingerprintSize + saltSize + nonceSize + dekSize + 16
	kekInfoPrefix   = "nexo-kms-v1/"
)

// builtin is the internal KMS.
type builtin struct {
	deps BuiltinDeps
	// master seals; masters unseal, by fingerprint (the current one
	// included).
	master  MasterKey
	masters map[[fingerprintSize]byte]MasterKey
	// implicit are the names of the master keys: keys that always exist.
	implicit map[string]bool

	mu sync.Mutex
	// known caches the created keys found in the store; memory holds them
	// without a store.
	known  map[string]KeyInfo
	memory map[string]KeyInfo
}

var _ Service = (*builtin)(nil)

// NewBuiltin returns the internal KMS keyed by the master key of
// WithMasterKey (required).
func NewBuiltin(deps BuiltinDeps, opts ...Option) (Service, error) {
	o := newOptions(opts)
	if o.master == nil || len(o.master.secret) != masterKeySize {
		return nil, fmt.Errorf("%w: builtin kms: a master key is required (NEXO_KMS_SECRET_KEY)", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	k := &builtin{
		deps:     deps,
		master:   *o.master,
		masters:  map[[fingerprintSize]byte]MasterKey{},
		implicit: map[string]bool{o.master.Name: true},
		known:    map[string]KeyInfo{},
		memory:   map[string]KeyInfo{},
	}
	k.masters[o.master.fingerprint()] = *o.master
	for _, p := range o.previous {
		if len(p.secret) != masterKeySize {
			return nil, fmt.Errorf("%w: builtin kms: invalid previous master key %q", domain.ErrInvalidArgument, p.Name)
		}
		if _, dup := k.masters[p.fingerprint()]; !dup {
			k.masters[p.fingerprint()] = p
		}
		k.implicit[p.Name] = true
	}
	return k, nil
}

// Status implements Service.
func (k *builtin) Status(context.Context) (Status, error) {
	return Status{Configured: true, Backend: BackendBuiltin, DefaultKey: k.master.Name, Online: true}, nil
}

// DefaultKeyID implements Service.
func (k *builtin) DefaultKeyID() string { return k.master.Name }

// CreateKey implements Service.
func (k *builtin) CreateKey(ctx context.Context, keyID string) error {
	if err := validKeyName(keyID); err != nil {
		return err
	}
	if k.implicit[keyID] {
		return fmt.Errorf("kms: key %q: %w", keyID, domain.ErrKMSKeyExists)
	}
	info := KeyInfo{Name: keyID, CreatedAt: k.deps.Clock.Now().UTC()}
	if k.deps.Keys == nil {
		k.mu.Lock()
		defer k.mu.Unlock()
		if _, ok := k.memory[keyID]; ok {
			return fmt.Errorf("kms: key %q: %w", keyID, domain.ErrKMSKeyExists)
		}
		k.memory[keyID] = info
		return nil
	}
	if err := k.deps.Keys.Create(ctx, info); err != nil {
		return fmt.Errorf("kms: create key %q: %w", keyID, err)
	}
	k.mu.Lock()
	k.known[keyID] = info
	k.mu.Unlock()
	k.deps.Logger.InfoContext(ctx, "kms: key created", "key", keyID)
	return nil
}

// ListKeys implements Service.
func (k *builtin) ListKeys(ctx context.Context, prefix string) ([]KeyInfo, error) {
	byName := map[string]KeyInfo{}
	for name := range k.implicit {
		byName[name] = KeyInfo{Name: name, Default: name == k.master.Name}
	}
	if k.deps.Keys == nil {
		k.mu.Lock()
		for name, info := range k.memory {
			byName[name] = info
		}
		k.mu.Unlock()
	} else {
		stored, err := k.deps.Keys.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("kms: list keys: %w", err)
		}
		for _, info := range stored {
			if _, ok := byName[info.Name]; !ok {
				byName[info.Name] = info
			}
		}
	}
	out := make([]KeyInfo, 0, len(byName))
	for name, info := range byName {
		if strings.HasPrefix(name, prefix) {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// exists reports whether keyID names a key: a master key, or a key
// created through CreateKey (read through to the store on a cache miss,
// so that a key created by another node is found).
func (k *builtin) exists(ctx context.Context, keyID string) error {
	if k.implicit[keyID] {
		return nil
	}
	k.mu.Lock()
	_, known := k.known[keyID]
	_, inMemory := k.memory[keyID]
	k.mu.Unlock()
	if known || inMemory {
		return nil
	}
	if k.deps.Keys == nil {
		return fmt.Errorf("kms: key %q: %w", keyID, domain.ErrKMSKeyNotFound)
	}
	info, err := k.deps.Keys.Get(ctx, keyID)
	if err != nil {
		return fmt.Errorf("kms: key %q: %w", keyID, err)
	}
	k.mu.Lock()
	k.known[keyID] = info
	k.mu.Unlock()
	return nil
}

// keyID resolves the default key and validates the name.
func (k *builtin) keyID(keyID string) (string, error) {
	if keyID == "" {
		return k.master.Name, nil
	}
	if err := validKeyName(keyID); err != nil {
		return "", err
	}
	return keyID, nil
}

// GenerateKey implements Service.
func (k *builtin) GenerateKey(ctx context.Context, keyID string, kctx Context) (DEK, error) {
	id, err := k.keyID(keyID)
	if err != nil {
		return DEK{}, err
	}
	if err := k.exists(ctx, id); err != nil {
		return DEK{}, err
	}
	plaintext := make([]byte, dekSize)
	if _, err := rand.Read(plaintext); err != nil {
		return DEK{}, fmt.Errorf("kms: random key: %w", err)
	}
	ciphertext, err := k.seal(id, plaintext, kctx)
	if err != nil {
		return DEK{}, err
	}
	return DEK{KeyID: id, Plaintext: plaintext, Ciphertext: ciphertext}, nil
}

// Decrypt implements Service. The key material derives from the master
// key: a ciphertext sealed under a key id unseals without a lookup of the
// key registry.
func (k *builtin) Decrypt(_ context.Context, keyID string, ciphertext []byte, kctx Context) ([]byte, error) {
	id, err := k.keyID(keyID)
	if err != nil {
		return nil, err
	}
	return k.open(id, ciphertext, kctx)
}

// ReEncrypt implements Service.
func (k *builtin) ReEncrypt(ctx context.Context, keyID string, ciphertext []byte, kctx Context) ([]byte, error) {
	plaintext, err := k.Decrypt(ctx, keyID, ciphertext, kctx)
	if err != nil {
		return nil, err
	}
	id, _ := k.keyID(keyID)
	return k.seal(id, plaintext, kctx)
}

// kek derives the key encryption key of keyID from a master key and a
// salt.
func kek(master MasterKey, keyID string, salt []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, master.secret, salt, kekInfoPrefix+keyID, 32)
	if err != nil {
		return nil, fmt.Errorf("kms: derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("kms: cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// seal seals a data key under keyID with the current master key.
func (k *builtin) seal(keyID string, plaintext []byte, kctx Context) ([]byte, error) {
	aad, err := encodeContext(kctx)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, sealedSize)
	out = append(out, sealVersion)
	fp := k.master.fingerprint()
	out = append(out, fp[:]...)
	salt := make([]byte, saltSize+nonceSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("kms: random salt: %w", err)
	}
	out = append(out, salt...)
	aead, err := kek(k.master, keyID, salt[:saltSize])
	if err != nil {
		return nil, err
	}
	return aead.Seal(out, salt[saltSize:], plaintext, aad), nil
}

// open unseals a data key sealed by seal with the master key whose
// fingerprint it carries.
func (k *builtin) open(keyID string, ciphertext []byte, kctx Context) ([]byte, error) {
	if len(ciphertext) != sealedSize || ciphertext[0] != sealVersion {
		return nil, fmt.Errorf("kms: %w: malformed sealed key", domain.ErrDecryption)
	}
	var fp [fingerprintSize]byte
	copy(fp[:], ciphertext[1:1+fingerprintSize])
	master, ok := k.masters[fp]
	if !ok {
		return nil, fmt.Errorf("kms: %w: the key was sealed with an unknown master key", domain.ErrDecryption)
	}
	aad, err := encodeContext(kctx)
	if err != nil {
		return nil, err
	}
	rest := ciphertext[1+fingerprintSize:]
	aead, err := kek(master, keyID, rest[:saltSize])
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, rest[saltSize:saltSize+nonceSize], rest[saltSize+nonceSize:], aad)
	if err != nil {
		return nil, fmt.Errorf("kms: %w: sealed key, key id or context does not authenticate", domain.ErrDecryption)
	}
	return plaintext, nil
}

// encodeContext returns the canonical form of an encryption context: JSON
// with sorted keys ("{}" for an empty context).
func encodeContext(kctx Context) ([]byte, error) {
	if len(kctx) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string(kctx)); err != nil {
		return nil, fmt.Errorf("%w: kms context: %v", domain.ErrInvalidArgument, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// isNotFound reports a missing key.
func isNotFound(err error) bool { return errors.Is(err, domain.ErrKMSKeyNotFound) }
