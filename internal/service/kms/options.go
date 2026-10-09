package kms

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// masterKeySize is the size of a master key of the builtin KMS.
const masterKeySize = 32

// MasterKey is a master key of the builtin KMS: its name is the default
// key id (NEXO_KMS_SECRET_KEY="<name>:<base64 key>").
type MasterKey struct {
	Name   string
	secret []byte
}

// ParseMasterKey parses "<key-name>:<base64 of 32 bytes>" (the value of
// NEXO_KMS_SECRET_KEY).
func ParseMasterKey(s string) (MasterKey, error) {
	name, encoded, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return MasterKey{}, fmt.Errorf("%w: kms secret key must be <key-name>:<base64 key>", domain.ErrInvalidArgument)
	}
	if err := validKeyName(name); err != nil {
		return MasterKey{}, err
	}
	secret, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return MasterKey{}, fmt.Errorf("%w: kms secret key %q is not valid base64", domain.ErrInvalidArgument, name)
	}
	if len(secret) != masterKeySize {
		return MasterKey{}, fmt.Errorf("%w: kms secret key %q must be %d bytes, got %d", domain.ErrInvalidArgument, name, masterKeySize, len(secret))
	}
	return MasterKey{Name: name, secret: secret}, nil
}

// fingerprint identifies a master key in the ciphertexts it seals,
// without revealing it.
func (m MasterKey) fingerprint() [fingerprintSize]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("nexo-kms-master-fingerprint-v1"))
	_, _ = h.Write(m.secret)
	var out [fingerprintSize]byte
	copy(out[:], h.Sum(nil))
	return out
}

// validKeyName checks a key name: 1-128 letters, digits, '-', '_' or '.'
// ('.' not first), so that it is safe in a path and in an URL.
func validKeyName(name string) error {
	if name == "" || len(name) > 128 || name[0] == '.' {
		return fmt.Errorf("%w: invalid kms key name %q", domain.ErrInvalidArgument, name)
	}
	for _, c := range name {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("%w: invalid kms key name %q", domain.ErrInvalidArgument, name)
		}
	}
	return nil
}

// Option configures a KMS implementation. An option that does not concern
// an implementation is ignored by it.
type Option func(*options)

type options struct {
	// master is the current master key of the builtin KMS; previous are
	// the master keys it replaced (unseal only).
	master   *MasterKey
	previous []MasterKey
	// endpoints and defaultKey configure the KES client.
	endpoints  []string
	defaultKey string
}

func newOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithMasterKey sets the master key of the builtin KMS (required there).
func WithMasterKey(k MasterKey) Option {
	return func(o *options) { o.master = &k }
}

// WithPreviousKeys adds the master keys the current one replaced: the
// builtin KMS still unseals the keys they sealed (ReEncrypt moves them to
// the current master key).
func WithPreviousKeys(keys ...MasterKey) Option {
	return func(o *options) { o.previous = append(o.previous, keys...) }
}

// WithEndpoints sets the base URLs of the KES servers (required by
// NewKES); requests fail over from one to the next.
func WithEndpoints(urls ...string) Option {
	return func(o *options) {
		for _, u := range urls {
			if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
				o.endpoints = append(o.endpoints, u)
			}
		}
	}
}

// WithDefaultKey sets the default key of the KES client (required by
// NewKES): the key of SSE-S3, NEXO_KMS_KEY_NAME.
func WithDefaultKey(name string) Option {
	return func(o *options) { o.defaultKey = strings.TrimSpace(name) }
}
