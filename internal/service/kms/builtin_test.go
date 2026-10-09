package kms_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
)

// newMasterKey returns a random master key named name.
func newMasterKey(t *testing.T, name string) kms.MasterKey {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	k, err := kms.ParseMasterKey(name + ":" + base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	return k
}

func newBuiltin(t *testing.T, deps kms.BuiltinDeps, opts ...kms.Option) kms.Service {
	t.Helper()
	k, err := kms.NewBuiltin(deps, opts...)
	require.NoError(t, err)
	return k
}

func TestParseMasterKey(t *testing.T) {
	key32 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "valid", in: "my-key:" + key32, want: "my-key"},
		{name: "surrounding spaces", in: "  k1:" + key32 + " \n", want: "k1"},
		{name: "no separator", in: key32, wantErr: true},
		{name: "empty name", in: ":" + key32, wantErr: true},
		{name: "invalid name", in: "my key:" + key32, wantErr: true},
		{name: "not base64", in: "k:!!!", wantErr: true},
		{name: "short key", in: "k:" + base64.StdEncoding.EncodeToString(make([]byte, 16)), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := kms.ParseMasterKey(tt.in)
			if tt.wantErr {
				require.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, k.Name)
		})
	}
}

func TestNewBuiltinRequiresMasterKey(t *testing.T) {
	_, err := kms.NewBuiltin(kms.BuiltinDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestBuiltinGenerateDecrypt(t *testing.T) {
	ctx := context.Background()
	k := newBuiltin(t, kms.BuiltinDeps{}, kms.WithMasterKey(newMasterKey(t, "master")))
	require.NoError(t, k.CreateKey(ctx, "tenant-a"))
	kctx := kms.Context{"aws:s3:arn": "arn:aws:s3:::b/o", "team": "x"}

	dek, err := k.GenerateKey(ctx, "", kctx)
	require.NoError(t, err)
	assert.Equal(t, "master", dek.KeyID, "the empty key id is the default key")
	assert.Len(t, dek.Plaintext, 32)
	assert.NotContains(t, string(dek.Ciphertext), string(dek.Plaintext))

	named, err := k.GenerateKey(ctx, "tenant-a", kctx)
	require.NoError(t, err)

	tests := []struct {
		name    string
		keyID   string
		cipher  []byte
		kctx    kms.Context
		want    []byte
		wantErr error
	}{
		{name: "default key", keyID: "master", cipher: dek.Ciphertext, kctx: kctx, want: dek.Plaintext},
		{name: "empty key id is the default", keyID: "", cipher: dek.Ciphertext, kctx: kctx, want: dek.Plaintext},
		{name: "named key", keyID: "tenant-a", cipher: named.Ciphertext, kctx: kctx, want: named.Plaintext},
		{name: "context in another order", keyID: "tenant-a", cipher: named.Ciphertext, kctx: kms.Context{"team": "x", "aws:s3:arn": "arn:aws:s3:::b/o"}, want: named.Plaintext},
		{name: "other context", keyID: "master", cipher: dek.Ciphertext, kctx: kms.Context{"aws:s3:arn": "arn:aws:s3:::b/other"}, wantErr: domain.ErrDecryption},
		{name: "no context", keyID: "master", cipher: dek.Ciphertext, wantErr: domain.ErrDecryption},
		{name: "other key", keyID: "tenant-a", cipher: dek.Ciphertext, kctx: kctx, wantErr: domain.ErrDecryption},
		{name: "tampered", keyID: "master", cipher: flip(dek.Ciphertext, len(dek.Ciphertext)-1), kctx: kctx, wantErr: domain.ErrDecryption},
		{name: "truncated", keyID: "master", cipher: dek.Ciphertext[:20], kctx: kctx, wantErr: domain.ErrDecryption},
		{name: "invalid key name", keyID: "a/b", cipher: dek.Ciphertext, kctx: kctx, wantErr: domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := k.Decrypt(ctx, tt.keyID, tt.cipher, tt.kctx)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	_, err = k.GenerateKey(ctx, "missing", kctx)
	require.ErrorIs(t, err, domain.ErrKMSKeyNotFound)
}

// flip returns a copy of b with the byte at i inverted.
func flip(b []byte, i int) []byte {
	out := append([]byte(nil), b...)
	out[i] ^= 0xff
	return out
}

func TestBuiltinKeys(t *testing.T) {
	ctx := context.Background()
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	stores := map[string]func(t *testing.T) kms.KeyStore{
		"memory": func(*testing.T) kms.KeyStore { return nil },
		"file": func(t *testing.T) kms.KeyStore {
			s, err := kms.NewKeyStore(kms.KeyStoreDeps{}, kms.WithKeyStorePath(t.TempDir()))
			require.NoError(t, err)
			return s
		},
		"docs": newDocsKeyStore,
	}
	for name, newStore := range stores {
		t.Run(name, func(t *testing.T) {
			k := newBuiltin(t, kms.BuiltinDeps{Keys: newStore(t), Clock: fake}, kms.WithMasterKey(newMasterKey(t, "master")))
			require.NoError(t, k.CreateKey(ctx, "b-key"))
			require.NoError(t, k.CreateKey(ctx, "a-key"))
			require.ErrorIs(t, k.CreateKey(ctx, "a-key"), domain.ErrKMSKeyExists)
			require.ErrorIs(t, k.CreateKey(ctx, "master"), domain.ErrKMSKeyExists, "the master key exists")
			require.ErrorIs(t, k.CreateKey(ctx, ".hidden"), domain.ErrInvalidArgument)

			keys, err := k.ListKeys(ctx, "")
			require.NoError(t, err)
			require.Len(t, keys, 3)
			assert.Equal(t, []string{"a-key", "b-key", "master"}, names(keys))
			assert.True(t, keys[2].Default)
			if name != "memory" {
				assert.Equal(t, fake.Now().UTC(), keys[0].CreatedAt.UTC())
			}
			keys, err = k.ListKeys(ctx, "b")
			require.NoError(t, err)
			assert.Equal(t, []string{"b-key"}, names(keys))

			_, err = k.GenerateKey(ctx, "b-key", nil)
			require.NoError(t, err)
			st, err := k.Status(ctx)
			require.NoError(t, err)
			assert.Equal(t, kms.Status{Configured: true, Backend: kms.BackendBuiltin, DefaultKey: "master", Online: true}, st)
			assert.Equal(t, "master", k.DefaultKeyID())
		})
	}
}

func names(keys []kms.KeyInfo) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Name
	}
	return out
}

// TestBuiltinKeyCreatedByAnotherNode: the nodes of a cluster share the
// key registry; a key created by one node is found by the others.
func TestBuiltinKeyCreatedByAnotherNode(t *testing.T) {
	ctx := context.Background()
	store := newDocsKeyStore(t)
	master := newMasterKey(t, "master")
	a := newBuiltin(t, kms.BuiltinDeps{Keys: store}, kms.WithMasterKey(master))
	b := newBuiltin(t, kms.BuiltinDeps{Keys: store}, kms.WithMasterKey(master))
	_, err := b.GenerateKey(ctx, "shared", nil)
	require.ErrorIs(t, err, domain.ErrKMSKeyNotFound)
	require.NoError(t, a.CreateKey(ctx, "shared"))
	dek, err := b.GenerateKey(ctx, "shared", nil)
	require.NoError(t, err)
	plain, err := a.Decrypt(ctx, "shared", dek.Ciphertext, nil)
	require.NoError(t, err)
	assert.Equal(t, dek.Plaintext, plain)
}

// TestBuiltinMasterKeyRotation (TS-U-13): a KMS keyed with a new master
// key and the previous one unseals what the previous one sealed, and
// ReEncrypt re-seals it under the new key, which the KMS of the new key
// alone then unseals.
func TestBuiltinMasterKeyRotation(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := newMasterKey(t, "nexo-key"), newMasterKey(t, "nexo-key")
	kctx := kms.Context{"aws:s3:arn": "arn:aws:s3:::bucket/object"}
	old := newBuiltin(t, kms.BuiltinDeps{}, kms.WithMasterKey(oldKey))
	dek, err := old.GenerateKey(ctx, "", kctx)
	require.NoError(t, err)

	rotated := newBuiltin(t, kms.BuiltinDeps{}, kms.WithMasterKey(newKey), kms.WithPreviousKeys(oldKey))
	plain, err := rotated.Decrypt(ctx, "nexo-key", dek.Ciphertext, kctx)
	require.NoError(t, err, "the previous master key still unseals")
	assert.Equal(t, dek.Plaintext, plain)

	resealed, err := rotated.ReEncrypt(ctx, "nexo-key", dek.Ciphertext, kctx)
	require.NoError(t, err)
	assert.NotEqual(t, dek.Ciphertext, resealed)

	current := newBuiltin(t, kms.BuiltinDeps{}, kms.WithMasterKey(newKey))
	plain, err = current.Decrypt(ctx, "nexo-key", resealed, kctx)
	require.NoError(t, err, "the re-sealed key needs only the new master key")
	assert.Equal(t, dek.Plaintext, plain)
	_, err = current.Decrypt(ctx, "nexo-key", dek.Ciphertext, kctx)
	require.ErrorIs(t, err, domain.ErrDecryption, "without the previous master key the old seal is lost")
	_, err = old.Decrypt(ctx, "nexo-key", resealed, kctx)
	require.ErrorIs(t, err, domain.ErrDecryption, "the old master key cannot unseal the new seal")
	_, err = rotated.ReEncrypt(ctx, "nexo-key", dek.Ciphertext, kms.Context{"other": "ctx"})
	require.ErrorIs(t, err, domain.ErrDecryption)
}

func TestDisabled(t *testing.T) {
	ctx := context.Background()
	k := kms.NewDisabled()
	st, err := k.Status(ctx)
	require.NoError(t, err)
	assert.False(t, st.Configured)
	assert.Empty(t, k.DefaultKeyID())
	calls := map[string]func() error{
		"CreateKey": func() error { return k.CreateKey(ctx, "k") },
		"ListKeys": func() error {
			_, err := k.ListKeys(ctx, "")
			return err
		},
		"GenerateKey": func() error {
			_, err := k.GenerateKey(ctx, "", nil)
			return err
		},
		"Decrypt": func() error {
			_, err := k.Decrypt(ctx, "", []byte("x"), nil)
			return err
		},
		"ReEncrypt": func() error {
			_, err := k.ReEncrypt(ctx, "", []byte("x"), nil)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.ErrorIs(t, err, domain.ErrKMSNotConfigured)
			require.ErrorIs(t, err, domain.ErrNotImplemented)
		})
	}
}

// TestMasterKeyRedacted checks that a master key printed or logged by
// mistake shows its name and fingerprint, never its secret.
func TestMasterKeyRedacted(t *testing.T) {
	secret := bytes.Repeat([]byte{0x5A}, 32)
	k, err := kms.ParseMasterKey("master:" + base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	opts := struct{ Master kms.MasterKey }{k}

	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("kms", "master", k, "opts", opts)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("kms", "master", k, "opts", opts)
	outputs := []string{
		fmt.Sprintf("%v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k), fmt.Sprintf("%x", k), fmt.Sprintf("%d", k),
		fmt.Sprintf("%+v", opts), fmt.Sprintf("%#v", opts), logs.String(),
	}
	for _, out := range outputs {
		for _, leak := range []string{base64.StdEncoding.EncodeToString(secret), hex.EncodeToString(secret), "90 90 90", "5a5a5a", string(secret)} {
			assert.NotContains(t, out, leak)
		}
		assert.Contains(t, out, "master")
	}
	assert.Contains(t, k.String(), "Fingerprint:")
}

// TestDEKRedacted (docs/07 §7.2): a data key printed, logged or encoded
// in JSON by mistake shows its key id and the size of its sealed form,
// never the plaintext key.
func TestDEKRedacted(t *testing.T) {
	k := newBuiltin(t, kms.BuiltinDeps{}, kms.WithMasterKey(newMasterKey(t, "master")))
	dek, err := k.GenerateKey(context.Background(), "", kms.Context{"bucket": "b"})
	require.NoError(t, err)
	require.Len(t, dek.Plaintext, 32)
	holder := struct{ DEK kms.DEK }{dek}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("kms", "dek", dek, "holder", holder)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("kms", "dek", dek, "holder", holder)
	asJSON, err := json.Marshal(holder)
	require.NoError(t, err)
	outputs := []string{
		fmt.Sprintf("%v", dek), fmt.Sprintf("%+v", dek), fmt.Sprintf("%#v", dek), fmt.Sprintf("%x", dek), fmt.Sprintf("%d", dek),
		fmt.Sprintf("%+v", holder), fmt.Sprintf("%#v", holder), logs.String(), string(asJSON),
	}
	for _, out := range outputs {
		for _, leak := range []string{hex.EncodeToString(dek.Plaintext), base64.StdEncoding.EncodeToString(dek.Plaintext), fmt.Sprintf("%d %d %d %d", dek.Plaintext[0], dek.Plaintext[1], dek.Plaintext[2], dek.Plaintext[3]), string(dek.Plaintext)} {
			assert.NotContains(t, out, leak)
		}
		assert.Contains(t, out, dek.KeyID)
	}
}

// TestNewKeyStore: the registry is on the document store or in a
// directory, exactly one of them.
func TestNewKeyStore(t *testing.T) {
	_, err := kms.NewKeyStore(kms.KeyStoreDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "neither")
	_, err = kms.NewKeyStore(kms.KeyStoreDeps{Docs: sysmock.NewStore(t)}, kms.WithKeyStorePath(t.TempDir()))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "both")

	dir := filepath.Join(t.TempDir(), "keys")
	s, err := kms.NewKeyStore(kms.KeyStoreDeps{}, kms.WithKeyStorePath(dir))
	require.NoError(t, err)
	assert.DirExists(t, dir, "created when missing")
	require.NoError(t, s.Create(context.Background(), kms.KeyInfo{Name: "k"}))
	got, err := s.Get(context.Background(), "k")
	require.NoError(t, err)
	assert.Equal(t, "k", got.Name)
}

// TestFingerprint: the fingerprint of a KMS configuration tells the
// configurations that read each other's objects (same master keys, same
// KES servers and default key, in any order) from the others, without
// revealing the master key.
func TestFingerprint(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	a, err := kms.ParseMasterKey("master:" + secret)
	require.NoError(t, err)
	aRenamed, err := kms.ParseMasterKey("renamed:" + secret)
	require.NoError(t, err)
	b, c := newMasterKey(t, "master"), newMasterKey(t, "old")
	kes := func(key string, endpoints ...string) []kms.Option {
		return []kms.Option{kms.WithEndpoints(endpoints...), kms.WithDefaultKey(key)}
	}
	assert.Empty(t, kms.Fingerprint(), "no KMS")

	base := kms.Fingerprint(kms.WithMasterKey(a), kms.WithPreviousKeys(b, c))
	assert.Len(t, base, 64)
	assert.Equal(t, base, kms.Fingerprint(kms.WithMasterKey(a), kms.WithPreviousKeys(c, b)), "previous keys in any order")
	for name, other := range map[string]string{
		"other master":         kms.Fingerprint(kms.WithMasterKey(b), kms.WithPreviousKeys(a, c)),
		"missing previous":     kms.Fingerprint(kms.WithMasterKey(a), kms.WithPreviousKeys(b)),
		"no previous":          kms.Fingerprint(kms.WithMasterKey(a)),
		"kes":                  kms.Fingerprint(kes("k", "https://kes1:7373")...),
		"same secret new name": kms.Fingerprint(kms.WithMasterKey(aRenamed), kms.WithPreviousKeys(b, c)),
	} {
		assert.NotEqual(t, base, other, name)
	}
	assert.NotContains(t, base, hex.EncodeToString(bytes.Repeat([]byte{7}, 8)), "the secret does not show")

	k := kms.Fingerprint(kes("k", "https://kes1:7373", "https://kes2:7373")...)
	assert.Equal(t, k, kms.Fingerprint(kes("k", "https://kes2:7373/", "https://kes1:7373")...), "endpoints in any order")
	assert.NotEqual(t, k, kms.Fingerprint(kes("other", "https://kes1:7373", "https://kes2:7373")...), "default key")
	assert.NotEqual(t, k, kms.Fingerprint(kes("k", "https://kes1:7373")...), "endpoints")
}
