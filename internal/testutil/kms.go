package testutil

import (
	"crypto/rand"
	"encoding/base64"

	"github.com/r3n3r0/nexo/internal/config"
)

// KMSKeyName is the default key of the builtin KMS of WithKMS: the key of
// SSE-S3, and the master key name of NEXO_KMS_SECRET_KEY.
const KMSKeyName = "nexo-test-key"

// WithKMS configures the builtin KMS of the server-side encryption
// (NEXO_KMS_SECRET_KEY) with a random master key named KMSKeyName; a
// restart keeps it.
func WithKMS() Option {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret) // never fails (Go 1.24)
	key := KMSKeyName + ":" + base64.StdEncoding.EncodeToString(secret)
	return WithConfig(func(c *config.Static) { c.KMSSecretKey = key })
}
