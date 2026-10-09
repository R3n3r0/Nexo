package domain

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
)

// SSEType is the kind of server-side encryption of an object (docs/02
// S-OBJ-13): with a key managed by the KMS of the deployment (SSE-S3), with
// a named KMS key (SSE-KMS), or with a key the client sends with every
// request (SSE-C).
type SSEType string

// Server-side encryption kinds.
const (
	SSENone SSEType = ""
	SSES3   SSEType = "SSE-S3"
	SSEKMS  SSEType = "SSE-KMS"
	SSEC    SSEType = "SSE-C"
)

// Values of the x-amz-server-side-encryption header and of the SSEAlgorithm
// of a bucket encryption rule; SSE-C keys are AES256 keys too
// (x-amz-server-side-encryption-customer-algorithm).
const (
	SSEAlgorithmAES256 = "AES256"
	SSEAlgorithmKMS    = "aws:kms"
)

// SSECustomerKeySize is the size of an SSE-C key (AES-256).
const SSECustomerKeySize = 32

// Errors of the server-side encryption.
var (
	// ErrKMSNotConfigured reports a request for SSE-S3 or SSE-KMS on a
	// deployment without KMS (NEXO_KMS_SECRET_KEY or NEXO_KMS_ENDPOINT).
	// SSE-C needs no KMS. It wraps ErrNotImplemented (HTTP 501).
	ErrKMSNotConfigured = fmt.Errorf("server side encryption specified but KMS is not configured: %w", ErrNotImplemented)
	// ErrKMSKeyNotFound reports an unknown KMS key (S3
	// KMS.NotFoundException). It wraps ErrInvalidArgument.
	ErrKMSKeyNotFound = fmt.Errorf("kms key not found: %w", ErrInvalidArgument)
	// ErrKMSKeyExists reports the creation of a KMS key that already
	// exists. It wraps ErrConflict.
	ErrKMSKeyExists = fmt.Errorf("kms key already exists: %w", ErrConflict)
	// ErrKMSUnavailable reports a KMS that cannot be reached or answered
	// with an unexpected error.
	ErrKMSUnavailable = errors.New("kms unavailable")
	// ErrDecryption reports encrypted data or a sealed key that does not
	// authenticate: tampered, truncated, or unsealed with the wrong key.
	ErrDecryption = errors.New("decryption failed")
)

// SSECustomerKey is the key of an SSE-C request
// (x-amz-server-side-encryption-customer-key and -key-MD5).
type SSECustomerKey struct {
	// Key is the raw 256-bit key.
	Key []byte
	// KeyMD5 is the base64-encoded MD5 of Key.
	KeyMD5 string
}

// IsZero reports whether no key is set.
func (k SSECustomerKey) IsZero() bool { return len(k.Key) == 0 }

// NewSSECustomerKey validates an SSE-C key: 32 bytes whose MD5 is the one
// the client declared (the MD5 is computed when declared is empty).
func NewSSECustomerKey(key []byte, declaredMD5 string) (SSECustomerKey, error) {
	if len(key) != SSECustomerKeySize {
		return SSECustomerKey{}, NewS3Error(ErrInvalidArgument, "InvalidArgument",
			"The secret key was invalid for the specified algorithm.")
	}
	sum := md5.Sum(key)
	computed := base64.StdEncoding.EncodeToString(sum[:])
	if declaredMD5 != "" && declaredMD5 != computed {
		return SSECustomerKey{}, NewS3Error(ErrInvalidArgument, "InvalidArgument",
			"The calculated MD5 hash of the key did not match the hash that was provided.")
	}
	return SSECustomerKey{Key: append([]byte(nil), key...), KeyMD5: computed}, nil
}

// SSERequest are the server-side encryption parameters of a write
// (x-amz-server-side-encryption*): the zero value asks for none (the
// default encryption of the bucket applies).
type SSERequest struct {
	Type SSEType
	// KMSKeyID is the key of SSE-KMS (empty: the default key of the KMS).
	KMSKeyID string
	// Context is the SSE-KMS encryption context
	// (x-amz-server-side-encryption-context), bound to the sealed key.
	Context map[string]string
	// CustomerKey is the key of SSE-C.
	CustomerKey SSECustomerKey
}

// IsZero reports whether no encryption is requested.
func (r SSERequest) IsZero() bool { return r.Type == SSENone }

// EncryptionInfo describes the encryption of a stored version, as the S3
// responses report it.
type EncryptionInfo struct {
	Type SSEType
	// KMSKeyID is the KMS key of SSE-S3 and SSE-KMS.
	KMSKeyID string
	// Context is the SSE-KMS encryption context given by the client.
	Context map[string]string
	// CustomerKeyMD5 is the base64 MD5 of the SSE-C key of the request
	// that read or wrote the version (never stored).
	CustomerKeyMD5 string
}

// IsZero reports an unencrypted version.
func (e EncryptionInfo) IsZero() bool { return e.Type == SSENone }

// Algorithm returns the value of x-amz-server-side-encryption: AES256 for
// SSE-S3, aws:kms for SSE-KMS, empty for SSE-C (which reports
// x-amz-server-side-encryption-customer-algorithm instead) and for an
// unencrypted version.
func (e EncryptionInfo) Algorithm() string {
	switch e.Type {
	case SSES3:
		return SSEAlgorithmAES256
	case SSEKMS:
		return SSEAlgorithmKMS
	default:
		return ""
	}
}

// BucketEncryption is the default encryption of a bucket (S-BKT-11): the
// writes that ask for none are encrypted with it.
type BucketEncryption struct {
	// Algorithm is SSEAlgorithmAES256 (SSE-S3) or SSEAlgorithmKMS
	// (SSE-KMS); empty means no default encryption.
	Algorithm string
	// KMSKeyID is the key of SSE-KMS (empty: the default key).
	KMSKeyID string
}

// IsZero reports a bucket without default encryption.
func (b BucketEncryption) IsZero() bool { return b.Algorithm == "" }

// Request returns the encryption request of a write the default applies
// to (zero without default encryption).
func (b BucketEncryption) Request() SSERequest {
	switch b.Algorithm {
	case SSEAlgorithmAES256:
		return SSERequest{Type: SSES3}
	case SSEAlgorithmKMS:
		return SSERequest{Type: SSEKMS, KMSKeyID: b.KMSKeyID}
	default:
		return SSERequest{}
	}
}
