package idp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJWKKeys(t *testing.T) {
	for _, c := range []struct {
		crv   string
		curve elliptic.Curve
	}{{"P-256", elliptic.P256()}, {"P-384", elliptic.P384()}, {"P-521", elliptic.P521()}} {
		key, err := ecdsa.GenerateKey(c.curve, rand.Reader)
		require.NoError(t, err)
		raw, err := key.PublicKey.Bytes()
		require.NoError(t, err)
		size := (len(raw) - 1) / 2
		k := jwk{Kty: "EC", Crv: c.crv,
			X: base64.RawURLEncoding.EncodeToString(raw[1 : 1+size]), Y: base64.RawURLEncoding.EncodeToString(raw[1+size:])}
		pub, err := k.publicKey()
		require.NoError(t, err, c.crv)
		assert.True(t, key.PublicKey.Equal(pub), c.crv)

		// A point off the curve is refused.
		bad := raw[1:]
		bad[len(bad)-1] ^= 1
		k.Y = base64.RawURLEncoding.EncodeToString(bad[size:])
		_, err = k.publicKey()
		require.Error(t, err, c.crv)
	}
	_, err := jwk{Kty: "RSA", N: base64.RawURLEncoding.EncodeToString(make([]byte, 128)), E: "AQAB"}.publicKey()
	require.Error(t, err, "RSA keys under 2048 bits are refused")
	_, err = jwk{Kty: "oct"}.publicKey()
	require.Error(t, err)
}
