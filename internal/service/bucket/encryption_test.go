package bucket_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/kms"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// newTestKMS returns a builtin KMS with a random master key ("master")
// and the key "key-1".
func newTestKMS(t *testing.T) kms.Service {
	t.Helper()
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	master, err := kms.ParseMasterKey("master:" + base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	k, err := kms.NewBuiltin(kms.BuiltinDeps{Logger: slog.New(slog.DiscardHandler)}, kms.WithMasterKey(master))
	require.NoError(t, err)
	require.NoError(t, k.CreateKey(context.Background(), "key-1"))
	return k
}

// TestDefaultEncryption (S-BKT-11): the default encryption needs a KMS
// (and an existing key), and reaches the object layer through the bucket
// configuration.
func TestDefaultEncryption(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		kms     kms.Service
		cfg     bucket.EncryptionConfig
		wantErr error
		code    string
	}{
		{name: "SSE-S3", kms: newTestKMS(t), cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmAES256}},
		{name: "SSE-KMS default key", kms: newTestKMS(t), cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmKMS}},
		{name: "SSE-KMS named key", kms: newTestKMS(t), cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmKMS, KMSKeyID: "key-1"}},
		{name: "SSE-KMS master key by name", kms: newTestKMS(t), cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmKMS, KMSKeyID: "master"}},
		{name: "unknown key", kms: newTestKMS(t), cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmKMS, KMSKeyID: "nope"}, wantErr: domain.ErrKMSKeyNotFound, code: "KMS.NotFoundException"},
		{name: "no KMS", cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmAES256}, wantErr: domain.ErrKMSNotConfigured, code: "NotImplemented"},
		{name: "disabled KMS", kms: kms.NewDisabled(), cfg: bucket.EncryptionConfig{Algorithm: domain.SSEAlgorithmAES256}, wantErr: domain.ErrKMSNotConfigured, code: "NotImplemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newDriveStore(t)
			objects := newBucketObjects(t)
			svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Policy: policymock.NewService(t), KMS: tt.kms})
			require.NoError(t, err)
			err = svc.SetEncryption(ctx, "bkt", tt.cfg)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				var s3err *domain.S3Error
				require.ErrorAs(t, err, &s3err)
				assert.Equal(t, tt.code, s3err.Code)
				return
			}
			require.NoError(t, err)
			cfg, err := store.BucketConfig(ctx, "bkt")
			require.NoError(t, err)
			assert.Equal(t, domain.BucketEncryption{Algorithm: tt.cfg.Algorithm, KMSKeyID: tt.cfg.KMSKeyID}, cfg.Encryption)
			require.NoError(t, svc.DeleteEncryption(ctx, "bkt"))
			cfg, err = store.BucketConfig(ctx, "bkt")
			require.NoError(t, err)
			assert.True(t, cfg.Encryption.IsZero())
		})
	}
}

// newBucketObjects is an object layer that knows the bucket "bkt".
func newBucketObjects(t *testing.T) *objectmock.Service {
	t.Helper()
	m := objectmock.NewService(t)
	m.EXPECT().GetBucketInfo(mock.Anything, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()
	return m
}
