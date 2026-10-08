package auth_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
)

// The example credentials of the AWS S3 SigV4 documentation.
const (
	awsAccessKey = "AKIAIOSFODNN7EXAMPLE"
	awsSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	awsAmzDate   = "20130524T000000Z"
	awsScope     = "20130524/us-east-1/s3/aws4_request"
)

// Test credentials known by the mock lookup.
const (
	tempAccessKey = "TEMPKEY"
	tempSecretKey = "tempsecret"
	tempToken     = "session-token"
	tempParent    = "alice"
)

// awsTime is the signing time of the AWS documentation examples.
var awsTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

// testNow is the clock of the tests that sign their own requests.
var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// hmacSHA256 and friends re-implement the SigV4 key derivation so that the
// tests cross-check the implementation rather than reuse it.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func testSigningKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newLookup returns a credential lookup that knows the AWS example key
// (root) plus a disabled, an expired, a temporary, an expired temporary and
// a broken one.
func newLookup(t testing.TB) *authmock.CredentialLookup {
	t.Helper()
	creds := authmock.NewCredentialLookup(t)
	known := map[string]struct {
		creds domain.Credentials
		id    domain.Identity
		err   error
	}{
		awsAccessKey:  {creds: domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey, Status: domain.StatusEnabled}, id: domain.Identity{IsRoot: true}},
		"DISABLED":    {creds: domain.Credentials{AccessKey: "DISABLED", SecretKey: "s", Status: domain.StatusDisabled}},
		"EXPIRED":     {creds: domain.Credentials{AccessKey: "EXPIRED", SecretKey: "s", Expiration: awsTime.Add(-time.Hour)}},
		"EXPIREDTEMP": {creds: domain.Credentials{AccessKey: "EXPIREDTEMP", SecretKey: "s", SessionToken: "t", Expiration: awsTime.Add(-time.Hour)}},
		"NOSECRET":    {creds: domain.Credentials{AccessKey: "NOSECRET"}},
		tempAccessKey: {creds: domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey, SessionToken: tempToken}, id: domain.Identity{Parent: tempParent}},
		"BROKEN":      {err: domain.ErrFaultyDisk},
		"GONE":        {err: fmt.Errorf("lookup: %w", domain.ErrUserNotFound)},
	}
	creds.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, accessKey string) (domain.Credentials, domain.Identity, error) {
			k, ok := known[accessKey]
			if !ok {
				return domain.Credentials{}, domain.Identity{}, domain.ErrInvalidAccessKey
			}
			return k.creds, k.id, k.err
		}).Maybe()
	return creds
}

// newVerifier returns a verifier backed by newLookup.
func newVerifier(t *testing.T, clk clock.Clock, opts ...auth.Option) auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(t), Clock: clk}, opts...)
	require.NoError(t, err)
	return v
}

// assertS3Error checks the sentinel and the S3 code of an error.
func assertS3Error(t *testing.T, err error, sentinel error, code string, msgAndArgs ...any) {
	t.Helper()
	require.Error(t, err, msgAndArgs...)
	assert.ErrorIs(t, err, sentinel, msgAndArgs...)
	if code == "" {
		return
	}
	var s3err *domain.S3Error
	if assert.True(t, errors.As(err, &s3err), msgAndArgs...) {
		assert.Equal(t, code, s3err.Code, msgAndArgs...)
	}
}

// chunkEncoder builds aws-chunked bodies with valid chunk signatures.
type chunkEncoder struct {
	key     []byte
	date    string
	scope   string
	prevSig string
	signed  bool
	buf     bytes.Buffer
}

func newChunkEncoder(secret string, now time.Time, seed string, signed bool) *chunkEncoder {
	return &chunkEncoder{
		key:     testSigningKey(secret, now.Format("20060102"), "us-east-1"),
		date:    now.Format("20060102T150405Z"),
		scope:   now.Format("20060102") + "/us-east-1/s3/aws4_request",
		prevSig: seed,
		signed:  signed,
	}
}

func (c *chunkEncoder) signature(data []byte) string {
	sts := strings.Join([]string{"AWS4-HMAC-SHA256-PAYLOAD", c.date, c.scope, c.prevSig, sha256Hex(nil), sha256Hex(data)}, "\n")
	sig := hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
	c.prevSig = sig
	return sig
}

// chunk emits a data chunk.
func (c *chunkEncoder) chunk(data []byte) *chunkEncoder {
	if c.signed {
		fmt.Fprintf(&c.buf, "%x;chunk-signature=%s\r\n", len(data), c.signature(data))
	} else {
		fmt.Fprintf(&c.buf, "%x\r\n", len(data))
	}
	c.buf.Write(data)
	c.buf.WriteString("\r\n")
	return c
}

// final emits the terminating chunk; without trailers it is followed by
// the empty line, with trailers the trailer lines follow directly.
func (c *chunkEncoder) final(withTrailer bool) *chunkEncoder {
	if c.signed {
		fmt.Fprintf(&c.buf, "0;chunk-signature=%s\r\n", c.signature(nil))
	} else {
		c.buf.WriteString("0\r\n")
	}
	if !withTrailer {
		c.buf.WriteString("\r\n")
	}
	return c
}

// trailers emits trailing headers (in order) and, for signed payloads,
// their signature. The signature covers the canonical form
// "name:value\n" of the trailers sorted by name.
func (c *chunkEncoder) trailers(kv ...string) *chunkEncoder {
	var canonical strings.Builder
	type pair struct{ k, v string }
	var pairs []pair
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, pair{kv[i], kv[i+1]})
		c.buf.WriteString(kv[i] + ":" + kv[i+1] + "\r\n")
	}
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j].k < pairs[j-1].k; j-- {
			pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
		}
	}
	for _, p := range pairs {
		canonical.WriteString(p.k + ":" + p.v + "\n")
	}
	if c.signed {
		sts := strings.Join([]string{"AWS4-HMAC-SHA256-TRAILER", c.date, c.scope, c.prevSig, sha256Hex([]byte(canonical.String()))}, "\n")
		c.buf.WriteString("x-amz-trailer-signature:" + hex.EncodeToString(hmacSHA256(c.key, []byte(sts))) + "\r\n")
	}
	c.buf.WriteString("\r\n")
	return c
}

func (c *chunkEncoder) bytes() []byte { return c.buf.Bytes() }

// newBearerRequest is a GET carrying an Authorization: Bearer header.
func newBearerRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/nexo/metrics/v1/node", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}
