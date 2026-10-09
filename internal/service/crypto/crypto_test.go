package crypto_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/kms"
)

const pkg = crypto.PackageSize

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func masterKey(t *testing.T, name string) kms.MasterKey {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	k, err := kms.ParseMasterKey(name + ":" + base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	return k
}

func builtinKMS(t *testing.T, opts ...kms.Option) kms.Service {
	t.Helper()
	k, err := kms.NewBuiltin(kms.BuiltinDeps{Logger: discard()}, opts...)
	require.NoError(t, err)
	return k
}

func newService(t *testing.T, opts ...crypto.Option) (crypto.Service, kms.Service) {
	t.Helper()
	k := builtinKMS(t, kms.WithMasterKey(masterKey(t, "master")))
	require.NoError(t, k.CreateKey(context.Background(), "tenant"))
	return crypto.New(crypto.Deps{KMS: k, Logger: discard()}, opts...), k
}

func customerKey(t *testing.T) domain.SSECustomerKey {
	t.Helper()
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	k, err := domain.NewSSECustomerKey(raw, "")
	require.NoError(t, err)
	return k
}

func randomData(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// encrypt returns the stored form of part n of plain.
func encrypt(t *testing.T, k crypto.ObjectKey, n int, plain []byte) []byte {
	t.Helper()
	r, err := k.EncryptPart(n, bytes.NewReader(plain))
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}

// decrypt reads [offset, offset+length) of part n of a part of plainSize
// bytes stored as stored.
func decrypt(k crypto.ObjectKey, n int, stored []byte, plainSize, offset, length int64) ([]byte, error) {
	rng, err := crypto.PackageRange(plainSize, offset, length)
	if err != nil {
		return nil, err
	}
	end := rng.StoredOffset + rng.StoredLength
	if end > int64(len(stored)) {
		end = int64(len(stored))
	}
	r, err := k.DecryptPart(n, bytes.NewReader(stored[rng.StoredOffset:end]), rng)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestSizes(t *testing.T) {
	tests := []struct {
		plain, stored int64
	}{
		{0, 16},
		{1, 17},
		{pkg - 1, pkg + 15},
		{pkg, pkg + 16},
		{pkg + 1, pkg + 1 + 32},
		{3 * pkg, 3*pkg + 48},
		{3*pkg + 100, 3*pkg + 100 + 64},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.stored, crypto.EncryptedSize(tt.plain), "EncryptedSize(%d)", tt.plain)
		got, err := crypto.DecryptedSize(tt.stored)
		require.NoError(t, err)
		assert.Equal(t, tt.plain, got, "DecryptedSize(%d)", tt.stored)
	}
	assert.Equal(t, int64(-1), crypto.EncryptedSize(-1))
	for _, bad := range []int64{0, 15, pkg + 16 + 5, pkg + 16 + 16} {
		_, err := crypto.DecryptedSize(bad)
		require.ErrorIs(t, err, domain.ErrDecryption, "DecryptedSize(%d)", bad)
	}
}

// TestEncryptDecrypt (TS-U-13): every size around the package boundaries
// round-trips with both ciphers, and the stored stream has the announced
// size and never contains the plaintext.
func TestEncryptDecrypt(t *testing.T) {
	ctx := context.Background()
	for _, c := range []crypto.Cipher{crypto.CipherAESGCM, crypto.CipherChaCha20Poly1305} {
		t.Run(string(c), func(t *testing.T) {
			svc, _ := newService(t, crypto.WithCipher(c))
			k, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSES3}})
			require.NoError(t, err)
			assert.Equal(t, c, k.Cipher())
			for _, size := range []int{0, 1, 100, pkg - 1, pkg, pkg + 1, 2 * pkg, 3*pkg + 7} {
				plain := bytes.Repeat([]byte("nexo-secret-marker "), size/19+1)[:size]
				stored := encrypt(t, k, 1, plain)
				assert.Len(t, stored, int(crypto.EncryptedSize(int64(size))))
				if size >= 19 {
					assert.NotContains(t, string(stored), "nexo-secret-marker", "size %d", size)
				}
				got, err := decrypt(k, 1, stored, int64(size), 0, int64(size))
				require.NoError(t, err, "size %d", size)
				assert.True(t, bytes.Equal(plain, got), "size %d", size)
			}
		})
	}
}

// TestRangeAtPackageBoundaries (TS-U-13): a range starting and ending at
// every offset around the package boundaries decrypts to the same bytes
// as the plaintext, reading only the packages it needs.
func TestRangeAtPackageBoundaries(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	k, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSES3}})
	require.NoError(t, err)
	size := int64(3*pkg + 1000)
	plain := randomData(t, int(size))
	stored := encrypt(t, k, 2, plain)
	var points []int64
	for _, b := range []int64{0, pkg, 2 * pkg, 3 * pkg, size} {
		for _, d := range []int64{-2, -1, 0, 1, 2} {
			if p := b + d; p >= 0 && p <= size {
				points = append(points, p)
			}
		}
	}
	for _, start := range points {
		for _, end := range points {
			if end < start {
				continue
			}
			got, err := decrypt(k, 2, stored, size, start, end-start)
			require.NoError(t, err, "range [%d,%d)", start, end)
			require.True(t, bytes.Equal(plain[start:end], got), "range [%d,%d)", start, end)
		}
	}
	rng, err := crypto.PackageRange(size, pkg+10, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(pkg+16), rng.StoredOffset, "the range starts at its package")
	assert.Equal(t, int64(pkg+16), rng.StoredLength, "one package is read")
	_, err = crypto.PackageRange(size, size-1, 2)
	require.ErrorIs(t, err, domain.ErrInvalidRange)
}

// TestTamperingDetected (TS-U-13): truncated, extended, modified,
// reordered packages and the data of another part fail to decrypt.
func TestTamperingDetected(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	k, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSES3}})
	require.NoError(t, err)
	size := int64(2*pkg + 500)
	plain := randomData(t, int(size))
	stored := encrypt(t, k, 1, plain)
	full := int64(pkg + 16)

	swapped := append([]byte(nil), stored...)
	copy(swapped[:full], stored[full:2*full])
	copy(swapped[full:2*full], stored[:full])
	flipped := append([]byte(nil), stored...)
	flipped[full+3] ^= 0x01

	tests := []struct {
		name      string
		part      int
		stored    []byte
		plainSize int64
	}{
		{name: "truncated mid-package", part: 1, stored: stored[:len(stored)-10], plainSize: size},
		{name: "last package dropped", part: 1, stored: stored[:2*full], plainSize: size},
		// The truncation of a stream at a package boundary, with the size
		// lowered to match, is caught by the final flag.
		{name: "truncated with the size lowered", part: 1, stored: stored[:2*full], plainSize: 2 * pkg},
		{name: "flipped bit", part: 1, stored: flipped, plainSize: size},
		{name: "reordered packages", part: 1, stored: swapped, plainSize: size},
		{name: "other part key", part: 2, stored: stored, plainSize: size},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decrypt(k, tt.part, tt.stored, tt.plainSize, 0, tt.plainSize)
			require.ErrorIs(t, err, domain.ErrDecryption)
		})
	}
}

func TestSealUnseal(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	ckey, otherKey := customerKey(t), customerKey(t)
	tests := []struct {
		name     string
		req      domain.SSERequest
		unseal   domain.SSECustomerKey
		bucket   string
		object   string
		wantErr  error
		wantCode string
	}{
		{name: "SSE-S3", req: domain.SSERequest{Type: domain.SSES3}},
		{name: "SSE-KMS default key", req: domain.SSERequest{Type: domain.SSEKMS}},
		{name: "SSE-KMS named key and context", req: domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: "tenant", Context: map[string]string{"app": "x"}}},
		{name: "SSE-C", req: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, unseal: ckey},
		{name: "SSE-C wrong key", req: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, unseal: otherKey, wantErr: domain.ErrAccessDenied, wantCode: "AccessDenied"},
		{name: "SSE-C without key", req: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, wantErr: domain.ErrInvalidArgument, wantCode: "InvalidRequest"},
		{name: "SSE-C key on SSE-S3", req: domain.SSERequest{Type: domain.SSES3}, unseal: ckey, wantErr: domain.ErrInvalidArgument, wantCode: "InvalidRequest"},
		{name: "SSE-S3 moved to another object", req: domain.SSERequest{Type: domain.SSES3}, object: "other", wantErr: domain.ErrDecryption},
		{name: "SSE-C moved to another bucket", req: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}, unseal: ckey, bucket: "other", wantErr: domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := svc.Seal(ctx, crypto.SealInput{Bucket: "bucket", Object: "a/b", Request: tt.req})
			require.NoError(t, err)
			meta := k.Metadata()
			assert.True(t, crypto.IsEncrypted(meta))
			for key, v := range meta {
				assert.True(t, crypto.IsMetadataKey(key), key)
				if tt.req.Type == domain.SSEC {
					assert.NotContains(t, v, base64.StdEncoding.EncodeToString(ckey.Key), "the SSE-C key is never stored")
				}
			}
			info := crypto.InfoOf(meta)
			assert.Equal(t, tt.req.Type, info.Type)
			if tt.req.Type != domain.SSEC {
				assert.NotEmpty(t, info.KMSKeyID)
			}
			if tt.req.KMSKeyID != "" {
				assert.Equal(t, tt.req.KMSKeyID, info.KMSKeyID)
			}
			assert.Equal(t, tt.req.Context, info.Context)
			plain := []byte("hello encrypted world")
			stored := encrypt(t, k, 1, plain)

			bucket, object := "bucket", "a/b"
			if tt.bucket != "" {
				bucket = tt.bucket
			}
			if tt.object != "" {
				object = tt.object
			}
			u, err := svc.Unseal(ctx, crypto.UnsealInput{Bucket: bucket, Object: object, Metadata: meta, CustomerKey: tt.unseal})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				if tt.wantCode != "" {
					var s3err *domain.S3Error
					require.True(t, errors.As(err, &s3err))
					assert.Equal(t, tt.wantCode, s3err.Code)
				}
				return
			}
			require.NoError(t, err)
			got, err := decrypt(u, 1, stored, int64(len(plain)), 0, int64(len(plain)))
			require.NoError(t, err)
			assert.Equal(t, plain, got)
			if tt.req.Type == domain.SSEC {
				assert.Equal(t, ckey.KeyMD5, u.Info().CustomerKeyMD5)
			}
		})
	}
}

func TestSealWithoutKMS(t *testing.T) {
	ctx := context.Background()
	svc := crypto.New(crypto.Deps{Logger: discard()})
	_, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSES3}})
	require.ErrorIs(t, err, domain.ErrKMSNotConfigured)
	var s3err *domain.S3Error
	require.True(t, errors.As(err, &s3err))
	assert.Contains(t, s3err.Message, "KMS is not configured")
	ckey := customerKey(t)
	k, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}})
	require.NoError(t, err, "SSE-C needs no KMS")
	_, err = svc.Unseal(ctx, crypto.UnsealInput{Bucket: "b", Object: "o", Metadata: k.Metadata(), CustomerKey: ckey})
	require.NoError(t, err)
	st, err := svc.Status(ctx)
	require.NoError(t, err)
	assert.False(t, st.Configured)
}

func TestSealUnknownKMSKey(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Seal(context.Background(), crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: "missing"}})
	require.ErrorIs(t, err, domain.ErrKMSKeyNotFound)
	var s3err *domain.S3Error
	require.True(t, errors.As(err, &s3err))
	assert.Equal(t, "KMS.NotFoundException", s3err.Code)
}

func TestETag(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	sum := md5.Sum([]byte("content"))
	md5Hex := hex.EncodeToString(sum[:])
	s3, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSES3}})
	require.NoError(t, err)
	assert.Equal(t, md5Hex, s3.ETag(md5Hex), "SSE-S3 reports the MD5 of the plaintext")
	for _, req := range []domain.SSERequest{{Type: domain.SSEKMS}, {Type: domain.SSEC, CustomerKey: customerKey(t)}} {
		k, err := svc.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: req})
		require.NoError(t, err)
		etag := k.ETag(md5Hex)
		assert.NotEqual(t, md5Hex, etag, "%s never reveals the MD5 of the plaintext", req.Type)
		assert.Len(t, etag, 32)
		_, err = hex.DecodeString(etag)
		require.NoError(t, err)
		assert.Equal(t, etag, k.ETag(md5Hex), "stable for the object")
	}
}

// TestResealAfterMasterKeyRotation (TS-U-13): after a rotation of the
// master key of the builtin KMS, Reseal moves the data key of an object to
// the new master key without touching the data.
func TestResealAfterMasterKeyRotation(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := masterKey(t, "nexo"), masterKey(t, "nexo")
	before := crypto.New(crypto.Deps{KMS: builtinKMS(t, kms.WithMasterKey(oldKey)), Logger: discard()})
	k, err := before.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSES3}})
	require.NoError(t, err)
	plain := randomData(t, pkg+5)
	stored := encrypt(t, k, 1, plain)

	rotated := crypto.New(crypto.Deps{KMS: builtinKMS(t, kms.WithMasterKey(newKey), kms.WithPreviousKeys(oldKey)), Logger: discard()})
	meta, err := rotated.Reseal(ctx, crypto.UnsealInput{Bucket: "b", Object: "o", Metadata: k.Metadata()})
	require.NoError(t, err)
	assert.NotEqual(t, k.Metadata()[crypto.MetaKMSSealedKey], meta[crypto.MetaKMSSealedKey])
	assert.Equal(t, k.Metadata()[crypto.MetaSealedKey], meta[crypto.MetaSealedKey], "the object key is unchanged")

	after := crypto.New(crypto.Deps{KMS: builtinKMS(t, kms.WithMasterKey(newKey)), Logger: discard()})
	_, err = after.Unseal(ctx, crypto.UnsealInput{Bucket: "b", Object: "o", Metadata: k.Metadata()})
	require.ErrorIs(t, err, domain.ErrDecryption, "the old seal needs the old master key")
	u, err := after.Unseal(ctx, crypto.UnsealInput{Bucket: "b", Object: "o", Metadata: meta})
	require.NoError(t, err)
	got, err := decrypt(u, 1, stored, int64(len(plain)), 0, int64(len(plain)))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(plain, got))

	ckey := customerKey(t)
	c, err := after.Seal(ctx, crypto.SealInput{Bucket: "b", Object: "o", Request: domain.SSERequest{Type: domain.SSEC, CustomerKey: ckey}})
	require.NoError(t, err)
	_, err = after.Reseal(ctx, crypto.UnsealInput{Bucket: "b", Object: "o", Metadata: c.Metadata()})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestNewSSECustomerKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	sum := md5.Sum(raw)
	good := base64.StdEncoding.EncodeToString(sum[:])
	tests := []struct {
		name    string
		key     []byte
		md5     string
		wantErr bool
	}{
		{name: "declared MD5", key: raw, md5: good},
		{name: "computed MD5", key: raw},
		{name: "wrong MD5", key: raw, md5: base64.StdEncoding.EncodeToString(make([]byte, 16)), wantErr: true},
		{name: "short key", key: raw[:16], wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := domain.NewSSECustomerKey(tt.key, tt.md5)
			if tt.wantErr {
				require.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, good, k.KeyMD5)
		})
	}
}
