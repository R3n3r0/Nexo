// Package kms is the key management service of the server-side encryption
// (docs/07 §7.2, docs/05 T-7.5). It generates the data encryption keys
// (DEK) the crypto package seals the object keys with, and unseals them:
// a DEK is returned in plaintext with its ciphertext, sealed under a named
// key of the KMS and bound to an encryption context; only the ciphertext
// is stored with the object.
//
// Three implementations share the contract, selected by the composition
// root from the configuration:
//
//   - NewBuiltin, the internal KMS: a master key read from
//     NEXO_KMS_SECRET_KEY ("<key-name>:<base64 of 32 bytes>"), from which
//     every named key derives (HKDF-SHA256); DEKs are sealed with
//     AES-256-GCM. The names of the keys created through the API are kept
//     in a KeyStore. Previous master keys (WithPreviousKeys) still unseal
//     what they sealed, and ReEncrypt re-seals it under the current one
//     (master key rotation);
//   - NewKES, the client of an external KMS speaking the KES API (MinIO
//     KES, itself in front of Vault, AWS KMS...) over mutual TLS
//     (NEXO_KMS_ENDPOINT);
//   - NewDisabled, the KMS of a deployment without one: every key
//     operation fails with domain.ErrKMSNotConfigured, so that SSE-S3 and
//     SSE-KMS are refused with a clear error while SSE-C keeps working.
package kms

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
)

// Context is an encryption context: key-value pairs bound to a sealed key
// (authenticated, not secret). Unsealing needs the same context.
type Context map[string]string

// DEK is a data encryption key: Plaintext is the 256-bit key, Ciphertext
// the same key sealed under KeyID (the only form ever stored).
type DEK struct {
	KeyID      string
	Plaintext  []byte
	Ciphertext []byte
}

// KeyInfo describes a key of the KMS.
type KeyInfo struct {
	Name string `json:"name"`
	// CreatedAt is when the key was created (zero when unknown, e.g. the
	// master key of the builtin KMS).
	CreatedAt time.Time `json:"createdAt,omitempty"`
	// Default marks the key of SSE-S3 (and of SSE-KMS without key id).
	Default bool `json:"default,omitempty"`
}

// Backends of Status.
const (
	BackendBuiltin = "builtin"
	BackendKES     = "kes"
)

// Status is the state of the KMS.
type Status struct {
	// Configured is false for the KMS of NewDisabled.
	Configured bool `json:"configured"`
	// Backend is BackendBuiltin or BackendKES ("" when not configured).
	Backend string `json:"backend,omitempty"`
	// Endpoints are the servers of an external KMS.
	Endpoints []string `json:"endpoints,omitempty"`
	// DefaultKey is the key of SSE-S3.
	DefaultKey string `json:"defaultKey,omitempty"`
	// Online reports that the KMS answers (always true for the builtin
	// one); Error explains why it does not.
	Online bool   `json:"online"`
	Error  string `json:"error,omitempty"`
	// Version is the version of an external KMS, when it reports one.
	Version string `json:"version,omitempty"`
}

// Service is the key management service. Key names are 1-128 characters
// among letters, digits, '-', '_' and '.' (not first); an empty key id
// selects the default key. Errors wrap the domain sentinels:
// domain.ErrKMSNotConfigured (no KMS), domain.ErrKMSKeyNotFound,
// domain.ErrKMSKeyExists, domain.ErrDecryption (a ciphertext or context
// that does not authenticate), domain.ErrKMSUnavailable (an external KMS
// that cannot be reached), domain.ErrInvalidArgument (an invalid name).
type Service interface {
	// Status reports the state of the KMS.
	Status(ctx context.Context) (Status, error)
	// DefaultKeyID returns the key of SSE-S3 ("" without KMS). It is a
	// pure accessor.
	DefaultKeyID() string
	// CreateKey creates a named key.
	CreateKey(ctx context.Context, keyID string) error
	// ListKeys lists the keys whose name starts with prefix, sorted by
	// name.
	ListKeys(ctx context.Context, prefix string) ([]KeyInfo, error)
	// GenerateKey returns a fresh 256-bit data key sealed under keyID and
	// bound to kctx.
	GenerateKey(ctx context.Context, keyID string, kctx Context) (DEK, error)
	// Decrypt unseals a data key sealed under keyID with the same context.
	Decrypt(ctx context.Context, keyID string, ciphertext []byte, kctx Context) ([]byte, error)
	// ReEncrypt re-seals a data key sealed under keyID with the current
	// master key (or the current version of the key of an external KMS):
	// the data key is unchanged, the ciphertext is new. It is the re-seal
	// of a master key rotation.
	ReEncrypt(ctx context.Context, keyID string, ciphertext []byte, kctx Context) ([]byte, error)
}

// KeyStore persists the registry of the keys created on the builtin KMS:
// their names and creation times (the key material derives from the
// master key and is never stored). The composition root provides the
// store of the deployment mode: NewDocsKeyStore on the replicated system
// documents (SNMD, cluster), NewFileKeyStore on the drive (SNSD).
type KeyStore interface {
	// Create records a new key; domain.ErrKMSKeyExists when its name is
	// taken (several nodes creating the same key agree on one).
	Create(ctx context.Context, key KeyInfo) error
	// Get returns a key (domain.ErrKMSKeyNotFound when unknown).
	Get(ctx context.Context, name string) (KeyInfo, error)
	// List returns every key, sorted by name.
	List(ctx context.Context) ([]KeyInfo, error)
}

// HTTPDoer sends the requests of the KES client: an *http.Client with the
// mutual TLS configuration of the KMS (NewKESClient). The context travels
// in the request.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// BuiltinDeps are the dependencies of the builtin KMS. Keys is optional:
// without it the created keys live in memory (tests, dry runs).
type BuiltinDeps struct {
	Keys   KeyStore
	Clock  clock.Clock
	Logger *slog.Logger
}

// KESDeps are the dependencies of the KES client. HTTP is required.
type KESDeps struct {
	HTTP   HTTPDoer
	Clock  clock.Clock
	Logger *slog.Logger
}
