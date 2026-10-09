package certs

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// aesSealer seals with AES-256-GCM: nonce || ciphertext.
type aesSealer struct {
	aead cipher.AEAD
}

var _ Sealer = (*aesSealer)(nil)

// NewSealer returns the Sealer keyed by a 32-byte key.
func NewSealer(key []byte) (Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: certs: the sealing key must be 32 bytes", domain.ErrInvalidArgument)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("certs: sealer: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("certs: sealer: %w", err)
	}
	return &aesSealer{aead: aead}, nil
}

// DeriveKey derives the sealing key of the certificates from the root
// credentials and the deployment id: every node of the deployment derives
// the same key, no other deployment does.
func DeriveKey(rootUser, rootPassword, deploymentID string) []byte {
	mac := hmac.New(sha256.New, []byte(rootPassword))
	mac.Write([]byte("nexo-certs-v1\x00" + rootUser + "\x00" + deploymentID))
	return mac.Sum(nil)
}

// Seal implements Sealer.
func (s *aesSealer) Seal(_ context.Context, plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("certs: seal: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open implements Sealer.
func (s *aesSealer) Open(_ context.Context, sealed, aad []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, fmt.Errorf("%w: certs: sealed data too short", domain.ErrInvalidArgument)
	}
	out, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return nil, fmt.Errorf("%w: certs: cannot open sealed data (wrong key?)", domain.ErrInvalidArgument)
	}
	return out, nil
}
