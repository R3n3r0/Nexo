package iam

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/argon2"
)

const (
	// kdfAlgorithm is the only key derivation function.
	kdfAlgorithm = "argon2id"
	// keySize is the AES-256 key size.
	keySize = 32
	// saltSize is the size of the random Argon2id salt.
	saltSize = 16

	// Bounds of the Argon2id parameters accepted from files and exports,
	// so that a crafted document cannot exhaust the memory or the CPU.
	maxKDFTime    = 16
	maxKDFMemory  = 1 << 20 // KiB (1 GiB)
	maxKDFThreads = 64

	// Alphabets of the generated credentials.
	accessKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	secretKeyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// Lengths of the generated credentials.
	generatedAccessKeyLen = 20
	generatedSecretKeyLen = 40
)

// defaultKDFParams are the RFC 9106 §4 second recommended option.
func defaultKDFParams() KDFParams {
	return KDFParams{Time: 1, MemoryKiB: 64 << 10, Threads: 4}
}

// withDefaults returns p, or the defaults when p is the zero value.
func (p KDFParams) withDefaults() KDFParams {
	if p == (KDFParams{}) {
		return defaultKDFParams()
	}
	return p
}

// validate checks the parameters against Argon2 rules and the bounds.
func (p KDFParams) validate() error {
	switch {
	case p.Time < 1 || p.Time > maxKDFTime:
		return fmt.Errorf("argon2id time %d out of range 1..%d", p.Time, maxKDFTime)
	case p.Threads < 1 || p.Threads > maxKDFThreads:
		return fmt.Errorf("argon2id threads %d out of range 1..%d", p.Threads, maxKDFThreads)
	case p.MemoryKiB < 8*uint32(p.Threads) || p.MemoryKiB > maxKDFMemory:
		return fmt.Errorf("argon2id memory %d KiB out of range %d..%d", p.MemoryKiB, 8*uint32(p.Threads), maxKDFMemory)
	}
	return nil
}

// deriveKey derives an AES-256 key from the root secret with Argon2id.
func deriveKey(secret string, salt []byte, p KDFParams) []byte {
	return argon2.IDKey([]byte(secret), salt, p.Time, p.MemoryKiB, p.Threads, keySize)
}

// errOpen reports a failed authenticated decryption: wrong key or tampered
// data, which AES-GCM cannot tell apart.
var errOpen = errors.New("authentication failed")

// seal encrypts plaintext with AES-256-GCM under a random nonce.
func seal(key, plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("iam: nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

// open decrypts and authenticates ciphertext; errOpen on failure.
func open(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, fmt.Errorf("nonce of %d bytes, want %d", len(nonce), aead.NonceSize())
	}
	plain, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, errOpen
	}
	return plain, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("iam: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("iam: gcm: %w", err)
	}
	return aead, nil
}

// randomBytes returns n random bytes.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("iam: random: %w", err)
	}
	return b, nil
}

// randomString returns n characters drawn uniformly from alphabet.
func randomString(n int, alphabet string) (string, error) {
	out := make([]byte, n)
	size := big.NewInt(int64(len(alphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", fmt.Errorf("iam: random: %w", err)
		}
		out[i] = alphabet[v.Int64()]
	}
	return string(out), nil
}

// stsSigningKey derives the HMAC key of the STS session tokens from the
// root credentials, so that every node of a deployment shares it and a
// change of the root secret invalidates every outstanding token.
func stsSigningKey(rootAccessKey, rootSecretKey string) []byte {
	mac := hmac.New(sha256.New, []byte(rootSecretKey))
	mac.Write([]byte("nexo/iam/sts-token/v1\x00" + rootAccessKey))
	return mac.Sum(nil)
}
