package bucket_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
)

// memStore is an in-memory MetadataStore used by the service tests; the
// drive-backed store has its own tests.
type memStore struct {
	docs map[string]*bucket.Metadata
}

func newMemStore() *memStore { return &memStore{docs: map[string]*bucket.Metadata{}} }

// cloneMeta deep-copies a document through its JSON form.
func cloneMeta(m *bucket.Metadata) *bucket.Metadata {
	data, _ := json.Marshal(m)
	var out bucket.Metadata
	_ = json.Unmarshal(data, &out)
	return &out
}

func (m *memStore) Load(_ context.Context, name string) (*bucket.Metadata, error) {
	if d, ok := m.docs[name]; ok {
		return cloneMeta(d), nil
	}
	return &bucket.Metadata{Name: name}, nil
}

func (m *memStore) Save(_ context.Context, name string, d *bucket.Metadata) error {
	m.docs[name] = cloneMeta(d)
	return nil
}

func (m *memStore) Delete(_ context.Context, name string) error {
	delete(m.docs, name)
	return nil
}

func (m *memStore) BucketConfig(ctx context.Context, name string) (object.BucketConfig, error) {
	d, _ := m.Load(ctx, name)
	return object.BucketConfig{Versioning: d.Versioning, ObjectLock: d.ObjectLock.Enabled, QuotaBytes: d.Quota.Size}, nil
}

func newService(t *testing.T) (bucket.Service, *objectmock.Service, *memStore, *clock.Fake) {
	t.Helper()
	objects := objectmock.NewService(t)
	store := newMemStore()
	clk := clock.NewFake(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC))
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Clock: clk, Logger: slog.New(slog.DiscardHandler)}, bucket.WithRegion("eu-south-1"))
	require.NoError(t, err)
	return svc, objects, store, clk
}

func TestNewRequiresDeps(t *testing.T) {
	_, err := bucket.New(bucket.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = bucket.New(bucket.Deps{Objects: objectmock.NewService(t)})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = bucket.New(bucket.Deps{Store: newMemStore()})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestCreateGetListDelete(t *testing.T) {
	ctx := context.Background()
	svc, objects, store, clk := newService(t)

	objects.EXPECT().MakeBucket(ctx, "photos", object.MakeBucketOptions{Region: "eu-south-1"}).Return(nil).Once()
	b, err := svc.Create(ctx, bucket.CreateInput{Name: "photos"})
	require.NoError(t, err)
	assert.Equal(t, "photos", b.Name)
	assert.Equal(t, "eu-south-1", b.Region)
	assert.Equal(t, clk.Now(), b.Created)
	assert.Equal(t, domain.VersioningOff, store.docs["photos"].Versioning)

	// Object lock implies versioning.
	objects.EXPECT().MakeBucket(ctx, "locked", object.MakeBucketOptions{Region: "us-west-2", LockEnabled: true, Versioning: domain.VersioningEnabled}).Return(nil).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "locked", Region: "us-west-2", ObjectLock: true})
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, store.docs["locked"].Versioning)
	assert.True(t, store.docs["locked"].ObjectLock.Enabled)
	cfg, err := store.BucketConfig(ctx, "locked")
	require.NoError(t, err)
	assert.True(t, cfg.ObjectLock)

	// Errors from the object layer and invalid names propagate.
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
	objects.EXPECT().MakeBucket(ctx, "photos", mock.Anything).Return(domain.ErrBucketAlreadyExists).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "photos"})
	assert.ErrorIs(t, err, domain.ErrBucketAlreadyExists)

	// Get merges the stored configuration.
	store.docs["photos"].Quota = bucket.Quota{Type: bucket.QuotaHard, Size: 42}
	store.docs["photos"].Tags = map[string]string{"team": "a"}
	objects.EXPECT().GetBucketInfo(ctx, "photos").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "photos", Created: clk.Now().Add(-time.Hour)}, Objects: 3}, nil).Once()
	info, err := svc.Get(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, uint64(42), info.QuotaBytes)
	assert.Equal(t, "eu-south-1", info.Region)
	assert.Equal(t, clk.Now(), info.Created, "the stored creation time wins")
	assert.Equal(t, uint64(3), info.Objects)
	assert.Equal(t, map[string]string{"team": "a"}, info.Tags)
	objects.EXPECT().GetBucketInfo(ctx, "missing").Return(domain.BucketInfo{}, domain.ErrBucketNotFound).Once()
	_, err = svc.Get(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)

	// List filters by prefix.
	objects.EXPECT().ListBuckets(ctx).Return([]domain.BucketInfo{
		{Bucket: domain.Bucket{Name: "locked"}}, {Bucket: domain.Bucket{Name: "photos"}}, {Bucket: domain.Bucket{Name: "zzz"}},
	}, nil).Twice()
	list, err := svc.List(ctx, bucket.ListInput{})
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.True(t, list[0].ObjectLockEnabled)
	assert.Equal(t, uint64(42), list[1].QuotaBytes)
	list, err = svc.List(ctx, bucket.ListInput{Prefix: "ph"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "photos", list[0].Name)
	objects.EXPECT().ListBuckets(ctx).Return(nil, errors.New("down")).Once()
	_, err = svc.List(ctx, bucket.ListInput{})
	assert.Error(t, err)

	// Delete removes the document too.
	objects.EXPECT().DeleteBucket(ctx, "photos", object.DeleteBucketOptions{Force: true}).Return(nil).Once()
	require.NoError(t, svc.Delete(ctx, "photos", bucket.DeleteOptions{Force: true}))
	_, ok := store.docs["photos"]
	assert.False(t, ok)
	objects.EXPECT().DeleteBucket(ctx, "locked", object.DeleteBucketOptions{}).Return(domain.ErrBucketNotEmpty).Once()
	assert.ErrorIs(t, svc.Delete(ctx, "locked", bucket.DeleteOptions{}), domain.ErrBucketNotEmpty)
	_, ok = store.docs["locked"]
	assert.True(t, ok, "metadata is kept when the bucket survives")
}

func TestVersioningAndObjectLock(t *testing.T) {
	ctx := context.Background()
	svc, objects, store, _ := newService(t)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()
	objects.EXPECT().GetBucketInfo(ctx, "missing").Return(domain.BucketInfo{}, domain.ErrBucketNotFound).Maybe()

	v, err := svc.GetVersioning(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningOff, v)
	_, err = svc.GetVersioning(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, svc.SetVersioning(ctx, "bkt", domain.VersioningOff), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.SetVersioning(ctx, "missing", domain.VersioningEnabled), domain.ErrBucketNotFound)

	// Object lock needs versioning first.
	_, err = svc.GetObjectLock(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true}), domain.ErrConflict)
	require.NoError(t, svc.SetVersioning(ctx, "bkt", domain.VersioningEnabled))
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: false}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: "WEIRD"}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 1, Years: 1}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Days: 1}), domain.ErrMalformedXML)
	require.NoError(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionCompliance, Days: 30}))
	lock, err := svc.GetObjectLock(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, 30, lock.Days)
	assert.Equal(t, bucket.RetentionCompliance, lock.Mode)

	// Versioning cannot be suspended while object lock is on.
	err = svc.SetVersioning(ctx, "bkt", domain.VersioningSuspended)
	assert.ErrorIs(t, err, domain.ErrConflict)
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "InvalidBucketState", s3err.Code)
	assert.Equal(t, domain.VersioningEnabled, store.docs["bkt"].Versioning)
	cfg, err := store.BucketConfig(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, cfg.Versioning)
	assert.True(t, cfg.ObjectLock)
}

func TestConfigurationDocuments(t *testing.T) {
	ctx := context.Background()
	svc, objects, _, _ := newService(t)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()

	// Policy.
	_, err := svc.GetPolicy(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetPolicy(ctx, "bkt", json.RawMessage(`{not json`)), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.SetPolicy(ctx, "bkt", json.RawMessage(`{"Version":"2008-10-17","Statement":[]}`)), domain.ErrInvalidArgument)
	policy := json.RawMessage(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`)
	require.NoError(t, svc.SetPolicy(ctx, "bkt", policy))
	got, err := svc.GetPolicy(ctx, "bkt")
	require.NoError(t, err)
	assert.JSONEq(t, string(policy), string(got))
	require.NoError(t, svc.DeletePolicy(ctx, "bkt"))
	_, err = svc.GetPolicy(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	// Lifecycle / CORS / notification: well-formed XML with the right root.
	_, err = svc.GetLifecycle(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetLifecycle(ctx, "bkt", []byte("<Lifecycle")), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetLifecycle(ctx, "bkt", []byte("<CORSConfiguration/>")), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetLifecycle(ctx, "bkt", nil), domain.ErrMalformedXML)
	lc := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	require.NoError(t, svc.SetLifecycle(ctx, "bkt", lc))
	got, err = svc.GetLifecycle(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, string(lc), string(got))
	require.NoError(t, svc.DeleteLifecycle(ctx, "bkt"))
	_, err = svc.GetLifecycle(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	cors := []byte(`<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)
	_, err = svc.GetCORS(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	require.NoError(t, svc.SetCORS(ctx, "bkt", cors))
	got, err = svc.GetCORS(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, string(cors), string(got))
	require.NoError(t, svc.DeleteCORS(ctx, "bkt"))

	got, err = svc.GetNotification(ctx, "bkt")
	require.NoError(t, err)
	assert.Contains(t, string(got), "<NotificationConfiguration")
	notif := []byte(`<NotificationConfiguration><QueueConfiguration><Id>q</Id></QueueConfiguration></NotificationConfiguration>`)
	require.NoError(t, svc.SetNotification(ctx, "bkt", notif))
	got, err = svc.GetNotification(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, string(notif), string(got))

	// Encryption.
	_, err = svc.GetEncryption(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetEncryption(ctx, "bkt", bucket.EncryptionConfig{Algorithm: "ROT13"}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetEncryption(ctx, "bkt", bucket.EncryptionConfig{Algorithm: "AES256", KMSKeyID: "k"}), domain.ErrInvalidArgument)
	require.NoError(t, svc.SetEncryption(ctx, "bkt", bucket.EncryptionConfig{Algorithm: "aws:kms", KMSKeyID: "key-1"}))
	enc, err := svc.GetEncryption(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, "key-1", enc.KMSKeyID)
	require.NoError(t, svc.DeleteEncryption(ctx, "bkt"))
	_, err = svc.GetEncryption(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	// bucket.Quota.
	q, err := svc.GetQuota(ctx, "bkt")
	require.NoError(t, err)
	assert.Zero(t, q.Size)
	assert.ErrorIs(t, svc.SetQuota(ctx, "bkt", bucket.Quota{Type: "soft", Size: 1}), domain.ErrInvalidArgument)
	require.NoError(t, svc.SetQuota(ctx, "bkt", bucket.Quota{Size: 1024}))
	q, err = svc.GetQuota(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, bucket.QuotaHard, q.Type)
	assert.Equal(t, uint64(1024), q.Size)
	require.NoError(t, svc.SetQuota(ctx, "bkt", bucket.Quota{Type: bucket.QuotaHard, Size: 0}))
	q, _ = svc.GetQuota(ctx, "bkt")
	assert.Empty(t, q.Type, "a zero size clears the quota")

	// Tags.
	_, err = svc.GetTags(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetTags(ctx, "bkt", map[string]string{"": "v"}), domain.ErrInvalidArgument)
	big := map[string]string{}
	for i := 0; i < 51; i++ {
		big[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	assert.ErrorIs(t, svc.SetTags(ctx, "bkt", big), domain.ErrInvalidArgument)
	require.NoError(t, svc.SetTags(ctx, "bkt", map[string]string{"env": "prod"}))
	tags, err := svc.GetTags(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod"}, tags)
	require.NoError(t, svc.DeleteTags(ctx, "bkt"))
	_, err = svc.GetTags(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
}

func TestStoreErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	objects := objectmock.NewService(t)
	store := bucketmock.NewMetadataStore(t)
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store})
	require.NoError(t, err)

	objects.EXPECT().MakeBucket(ctx, "bkt", mock.Anything).Return(nil).Once()
	store.EXPECT().Save(ctx, "bkt", mock.Anything).Return(domain.ErrWriteQuorum).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "bkt"})
	assert.ErrorIs(t, err, domain.ErrWriteQuorum)

	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{}, nil).Once()
	store.EXPECT().Load(ctx, "bkt").Return(nil, domain.ErrFaultyDisk).Once()
	_, err = svc.GetVersioning(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	objects.EXPECT().DeleteBucket(ctx, "bkt", mock.Anything).Return(nil).Once()
	store.EXPECT().Delete(ctx, "bkt").Return(domain.ErrWriteQuorum).Once()
	assert.ErrorIs(t, svc.Delete(ctx, "bkt", bucket.DeleteOptions{}), domain.ErrWriteQuorum)
}
