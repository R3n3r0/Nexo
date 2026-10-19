package bucket_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
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
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// newDriveStore returns the real drive-backed metadata store over one
// temporary drive: the service tests exercise the persisted documents
// end to end (integration with the real implementation, no hand-written
// fake).
func newDriveStore(t *testing.T) bucket.MetadataStore {
	t.Helper()
	drive := testutil.NewLocalDrive(t, filepath.Join(t.TempDir(), "d1"))
	store, err := bucket.NewStore(bucket.StoreDeps{Drives: []storage.StorageAPI{drive}, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	return store
}

// doc loads the persisted metadata of a bucket.
func doc(t *testing.T, store bucket.MetadataStore, name string) *bucket.Metadata {
	t.Helper()
	m, err := store.Load(context.Background(), name)
	require.NoError(t, err)
	return m
}

func newService(t *testing.T) (bucket.Service, *objectmock.Service, bucket.MetadataStore, *policymock.Service, *clock.Fake) {
	t.Helper()
	objects := objectmock.NewService(t)
	policies := policymock.NewService(t)
	store := newDriveStore(t)
	clk := clock.NewFake(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC))
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Policy: policies, KMS: newTestKMS(t), Clock: clk, Logger: slog.New(slog.DiscardHandler)}, bucket.WithRegion("eu-south-1"))
	require.NoError(t, err)
	return svc, objects, store, policies, clk
}

func TestNewRequiresDeps(t *testing.T) {
	tests := []struct {
		name string
		deps bucket.Deps
	}{
		{"nothing", bucket.Deps{}},
		{"no store", bucket.Deps{Objects: objectmock.NewService(t), Policy: policymock.NewService(t)}},
		{"no objects", bucket.Deps{Store: bucketmock.NewMetadataStore(t), Policy: policymock.NewService(t)}},
		{"no policy", bucket.Deps{Objects: objectmock.NewService(t), Store: bucketmock.NewMetadataStore(t)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := bucket.New(tt.deps)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
}

func TestCreateGetListDelete(t *testing.T) {
	ctx := context.Background()
	svc, objects, store, _, clk := newService(t)

	// The creation time is the one the object layer recorded.
	recorded := clk.Now().Add(-time.Second)
	objects.EXPECT().MakeBucket(ctx, "photos", object.MakeBucketOptions{Region: "eu-south-1"}).Return(nil).Once()
	objects.EXPECT().GetBucketInfo(ctx, "photos").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "photos", Created: recorded}}, nil).Once()
	b, err := svc.Create(ctx, bucket.CreateInput{Name: "photos"})
	require.NoError(t, err)
	assert.Equal(t, "photos", b.Name)
	assert.Equal(t, "eu-south-1", b.Region)
	assert.Equal(t, recorded, b.Created)
	assert.Equal(t, domain.VersioningOff, doc(t, store, "photos").Versioning)
	assert.Equal(t, recorded, doc(t, store, "photos").Created)

	// Object lock implies versioning; without a recorded time the clock is used.
	objects.EXPECT().MakeBucket(ctx, "locked", object.MakeBucketOptions{Region: "us-west-2", LockEnabled: true, Versioning: domain.VersioningEnabled}).Return(nil).Once()
	objects.EXPECT().GetBucketInfo(ctx, "locked").Return(domain.BucketInfo{}, domain.ErrFaultyDisk).Once()
	b, err = svc.Create(ctx, bucket.CreateInput{Name: "locked", Region: "us-west-2", ObjectLock: true})
	require.NoError(t, err)
	assert.Equal(t, clk.Now(), b.Created)
	assert.Equal(t, domain.VersioningEnabled, doc(t, store, "locked").Versioning)
	assert.True(t, doc(t, store, "locked").ObjectLock.Enabled)
	cfg, err := store.BucketConfig(ctx, "locked")
	require.NoError(t, err)
	assert.True(t, cfg.ObjectLock)

	// Errors from the object layer and invalid names propagate.
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
	// "nexo" is a valid S3 name reserved for the /nexo/ health and metrics
	// paths: the object layer is never asked to create it.
	_, err = svc.Create(ctx, bucket.CreateInput{Name: domain.ReservedBucket})
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
	objects.EXPECT().MakeBucket(ctx, "photos", mock.Anything).Return(domain.ErrBucketAlreadyExists).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "photos"})
	assert.ErrorIs(t, err, domain.ErrBucketAlreadyExists)

	// Get merges the stored configuration.
	photos := doc(t, store, "photos")
	photos.Quota = bucket.Quota{Type: bucket.QuotaHard, Size: 42}
	photos.Tags = map[string]string{"team": "a"}
	require.NoError(t, store.Save(ctx, "photos", photos))
	objects.EXPECT().GetBucketInfo(ctx, "photos").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "photos", Created: clk.Now().Add(-time.Hour)}, Objects: 3}, nil).Once()
	info, err := svc.Get(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, uint64(42), info.QuotaBytes)
	assert.Equal(t, "eu-south-1", info.Region)
	assert.Equal(t, recorded, info.Created, "the stored creation time wins")
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

	// ListPage pages the names with an opaque continuation token.
	objects.EXPECT().ListBuckets(ctx).Return([]domain.BucketInfo{
		{Bucket: domain.Bucket{Name: "zzz"}}, {Bucket: domain.Bucket{Name: "locked"}}, {Bucket: domain.Bucket{Name: "photos"}},
	}, nil).Times(3)
	var names []string
	token := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 3)
		page, err := svc.ListPage(ctx, bucket.ListPageInput{MaxBuckets: 2, ContinuationToken: token})
		require.NoError(t, err)
		for _, b := range page.Buckets {
			names = append(names, b.Name)
		}
		if token = page.ContinuationToken; token == "" {
			break
		}
	}
	assert.Equal(t, []string{"locked", "photos", "zzz"}, names, "name order, no duplicate")
	page, err := svc.ListPage(ctx, bucket.ListPageInput{MaxBuckets: 3})
	require.NoError(t, err)
	assert.Len(t, page.Buckets, 3)
	assert.Empty(t, page.ContinuationToken, "an exactly full page is the last one")
	for _, in := range []bucket.ListPageInput{{MaxBuckets: -1}, {MaxBuckets: bucket.MaxListBuckets + 1}, {ContinuationToken: "%%"}} {
		_, err = svc.ListPage(ctx, in)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", in)
	}

	// Usage comes from a walk of the object layer.
	objects.EXPECT().DataUsage(ctx, "").Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "photos"}, Objects: 7, Size: 70}}, nil).Once()
	list, err = svc.List(ctx, bucket.ListInput{WithUsage: true})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, uint64(7), list[0].Objects)
	assert.Equal(t, uint64(42), list[0].QuotaBytes, "merged with the configuration")
	objects.EXPECT().DataUsage(ctx, "").Return(nil, domain.ErrReadQuorum).Once()
	_, err = svc.List(ctx, bucket.ListInput{WithUsage: true})
	assert.ErrorIs(t, err, domain.ErrReadQuorum)

	// Delete removes the document too.
	objects.EXPECT().DeleteBucket(ctx, "photos", object.DeleteBucketOptions{Force: true}).Return(nil).Once()
	require.NoError(t, svc.Delete(ctx, "photos", bucket.DeleteOptions{Force: true}))
	assert.Zero(t, doc(t, store, "photos").Created, "the document is gone")
	objects.EXPECT().DeleteBucket(ctx, "locked", object.DeleteBucketOptions{}).Return(domain.ErrBucketNotEmpty).Once()
	assert.ErrorIs(t, svc.Delete(ctx, "locked", bucket.DeleteOptions{}), domain.ErrBucketNotEmpty)
	assert.False(t, doc(t, store, "locked").Created.IsZero(), "metadata is kept when the bucket survives")
}

func TestVersioningAndObjectLock(t *testing.T) {
	ctx := context.Background()
	svc, objects, store, _, _ := newService(t)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()
	objects.EXPECT().GetBucketInfo(ctx, "missing").Return(domain.BucketInfo{}, domain.ErrBucketNotFound).Maybe()

	v, err := svc.GetVersioning(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningOff, v)
	_, err = svc.GetVersioning(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, svc.SetVersioning(ctx, "bkt", domain.VersioningOff), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.SetVersioning(ctx, "missing", domain.VersioningEnabled), domain.ErrBucketNotFound)

	// Object lock is enabled at the creation of the bucket, or later once
	// its versioning is Enabled.
	_, err = svc.GetObjectLock(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	err = svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true})
	assert.ErrorIs(t, err, domain.ErrConflict, "not on an unversioned bucket")
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "InvalidBucketState", s3err.Code)
	require.NoError(t, svc.SetVersioning(ctx, "bkt", domain.VersioningSuspended))
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true}), domain.ErrConflict, "nor with versioning suspended")
	require.NoError(t, svc.SetVersioning(ctx, "bkt", domain.VersioningEnabled))
	require.NoError(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true}), "a versioned bucket can enable it")
	assert.True(t, doc(t, store, "bkt").ObjectLock.Enabled)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: false}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: "WEIRD"}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 1, Years: 1}), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Days: 1}), domain.ErrMalformedXML)
	require.NoError(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionCompliance, Days: 30}))
	lock, err := svc.GetObjectLock(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, 30, lock.Days)
	assert.Equal(t, bucket.RetentionCompliance, lock.Mode)
	require.NoError(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true}), "the default retention can be removed")
	require.NoError(t, svc.SetObjectLock(ctx, "bkt", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Years: 1}))

	// Versioning cannot be suspended while object lock is on.
	err = svc.SetVersioning(ctx, "bkt", domain.VersioningSuspended)
	assert.ErrorIs(t, err, domain.ErrConflict)
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "InvalidBucketState", s3err.Code)
	assert.Equal(t, domain.VersioningEnabled, doc(t, store, "bkt").Versioning)
	cfg, err := store.BucketConfig(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, cfg.Versioning)
	assert.True(t, cfg.ObjectLock)
	assert.Equal(t, domain.DefaultRetention{Mode: domain.RetentionGovernance, Years: 1}, cfg.DefaultRetention)
}

func TestConfigurationDocuments(t *testing.T) {
	ctx := context.Background()
	svc, objects, _, _, _ := newService(t)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()

	// Lifecycle / CORS / notification: well-formed XML with the right root.
	_, err := svc.GetLifecycle(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetLifecycle(ctx, "bkt", []byte("<Lifecycle")), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetLifecycle(ctx, "bkt", []byte("<CORSConfiguration/>")), domain.ErrMalformedXML)
	assert.ErrorIs(t, svc.SetLifecycle(ctx, "bkt", nil), domain.ErrMalformedXML)
	lc := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	require.NoError(t, svc.SetLifecycle(ctx, "bkt", lc))
	got, err := svc.GetLifecycle(ctx, "bkt")
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
	cfg, err := svc.GetCORSConfig(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.CORSConfiguration{Rules: []domain.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}}}, cfg)
	for name, bad := range map[string]string{
		"no rule":       `<CORSConfiguration></CORSConfiguration>`,
		"no method":     `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>`,
		"bad method":    `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>PATCH</AllowedMethod></CORSRule></CORSConfiguration>`,
		"two wildcards": `<CORSConfiguration><CORSRule><AllowedOrigin>*.*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`,
		"too many rules": "<CORSConfiguration>" + strings.Repeat(`<CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule>`, 101) +
			"</CORSConfiguration>",
	} {
		err := svc.SetCORS(ctx, "bkt", []byte(bad))
		var s3err *domain.S3Error
		require.ErrorAs(t, err, &s3err, name)
		assert.Contains(t, []string{"MalformedXML", "InvalidRequest"}, s3err.Code, name)
	}
	full := []byte(`<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><CORSRule><ID>web</ID>` +
		`<AllowedOrigin>https://*.example.com</AllowedOrigin><AllowedMethod>PUT</AllowedMethod><AllowedMethod>GET</AllowedMethod>` +
		`<AllowedHeader>*</AllowedHeader><ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>600</MaxAgeSeconds></CORSRule></CORSConfiguration>`)
	require.NoError(t, svc.SetCORS(ctx, "bkt", full))
	cfg, err = svc.GetCORSConfig(ctx, "bkt")
	require.NoError(t, err)
	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, "web", cfg.Rules[0].ID)
	assert.Equal(t, []string{"PUT", "GET"}, cfg.Rules[0].AllowedMethods)
	assert.Equal(t, []string{"ETag"}, cfg.Rules[0].ExposeHeaders)
	require.NotNil(t, cfg.Rules[0].MaxAgeSeconds)
	assert.Equal(t, 600, *cfg.Rules[0].MaxAgeSeconds)
	require.NoError(t, svc.DeleteCORS(ctx, "bkt"))
	_, err = svc.GetCORSConfig(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

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
	_, err = svc.SetQuota(ctx, "bkt", bucket.Quota{Type: "soft", Size: 1})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	set, err := svc.SetQuota(ctx, "bkt", bucket.Quota{Size: 1024})
	require.NoError(t, err)
	assert.Equal(t, bucket.Quota{Type: bucket.QuotaHard, Size: 1024}, set, "a typeless size is a hard quota")
	q, err = svc.GetQuota(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, set, q)
	set, err = svc.SetQuota(ctx, "bkt", bucket.Quota{Type: bucket.QuotaHard, Size: 0})
	require.NoError(t, err)
	assert.Equal(t, bucket.Quota{}, set)
	q, _ = svc.GetQuota(ctx, "bkt")
	assert.Empty(t, q.Type, "a zero size clears the quota")
	objects.EXPECT().GetBucketInfo(ctx, "missing").Return(domain.BucketInfo{}, domain.ErrBucketNotFound).Once()
	_, err = svc.SetQuota(ctx, "missing", bucket.Quota{Size: 1})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)

	// Tags.
	var s3err *domain.S3Error
	_, err = svc.GetTags(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, svc.SetTags(ctx, "bkt", map[string]string{"": "v"}), domain.ErrInvalidArgument)
	big := map[string]string{}
	for i := 0; i < 51; i++ {
		big[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	assert.ErrorIs(t, svc.SetTags(ctx, "bkt", big), domain.ErrInvalidArgument)
	for name, bad := range map[string]map[string]string{
		"reserved prefix":  {"aws:cloudformation": "x"},
		"long key":         {strings.Repeat("k", 129): "v"},
		"long value":       {"k": strings.Repeat("v", 257)},
		"invalid key utf8": {"\xff": "v"},
	} {
		err := svc.SetTags(ctx, "bkt", bad)
		require.ErrorAs(t, err, &s3err, name)
		assert.Equal(t, "InvalidTag", s3err.Code, name)
	}
	require.NoError(t, svc.SetTags(ctx, "bkt", map[string]string{strings.Repeat("é", 128): strings.Repeat("ü", 256)}),
		"lengths count Unicode characters")
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
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Policy: policymock.NewService(t)})
	require.NoError(t, err)

	// A configuration that cannot be saved undoes the creation.
	objects.EXPECT().MakeBucket(ctx, "bkt", mock.Anything).Return(nil).Twice()
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{}, nil).Twice()
	store.EXPECT().Save(ctx, "bkt", mock.Anything).Return(domain.ErrWriteQuorum).Twice()
	objects.EXPECT().DeleteBucket(mock.Anything, "bkt", object.DeleteBucketOptions{}).Return(nil).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "bkt"})
	assert.ErrorIs(t, err, domain.ErrWriteQuorum)
	objects.EXPECT().DeleteBucket(mock.Anything, "bkt", object.DeleteBucketOptions{}).Return(domain.ErrFaultyDisk).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "bkt"})
	assert.ErrorIs(t, err, domain.ErrWriteQuorum, "a failed rollback is logged, the cause reported")

	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{}, nil).Once()
	store.EXPECT().Load(ctx, "bkt").Return(nil, domain.ErrFaultyDisk).Once()
	_, err = svc.GetVersioning(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	objects.EXPECT().DeleteBucket(ctx, "bkt", mock.Anything).Return(nil).Once()
	store.EXPECT().Delete(ctx, "bkt").Return(domain.ErrWriteQuorum).Once()
	assert.ErrorIs(t, svc.Delete(ctx, "bkt", bucket.DeleteOptions{}), domain.ErrWriteQuorum)
}

// TestCreateRollbackSurvivesCanceledClient is a regression test: when the
// client goes away while the configuration is saved, the creation is still
// undone (no bucket directory without its configuration is left behind).
func TestCreateRollbackSurvivesCanceledClient(t *testing.T) {
	objects := objectmock.NewService(t)
	store := bucketmock.NewMetadataStore(t)
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Policy: policymock.NewService(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objects.EXPECT().MakeBucket(mock.Anything, "bkt", mock.Anything).Return(nil).Once()
	objects.EXPECT().GetBucketInfo(mock.Anything, "bkt").Return(domain.BucketInfo{}, nil).Once()
	store.EXPECT().Save(mock.Anything, "bkt", mock.Anything).RunAndReturn(func(ctx context.Context, _ string, _ *bucket.Metadata) error {
		cancel() // the client disconnects during the save
		return ctx.Err()
	}).Once()
	var rollbackCtxErr error
	objects.EXPECT().DeleteBucket(mock.Anything, "bkt", object.DeleteBucketOptions{}).RunAndReturn(func(ctx context.Context, _ string, _ object.DeleteBucketOptions) error {
		rollbackCtxErr = ctx.Err() // a canceled context would make the rollback fail
		return rollbackCtxErr
	}).Once()
	_, err = svc.Create(ctx, bucket.CreateInput{Name: "bkt"})
	assert.ErrorIs(t, err, context.Canceled)
	assert.NoError(t, rollbackCtxErr, "the rollback runs with a live context")
}

func TestPolicyDocuments(t *testing.T) {
	ctx := context.Background()
	svc, objects, _, policies, _ := newService(t)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()
	objects.EXPECT().GetBucketInfo(ctx, "missing").Return(domain.BucketInfo{}, domain.ErrBucketNotFound).Maybe()
	raw := json.RawMessage(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`)
	parsed := &domain.Policy{Version: domain.PolicyVersion}
	malformed := domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", "bad")

	// No policy yet.
	_, err := svc.GetPolicy(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	_, err = svc.IsPublic(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	// Invalid JSON never reaches the engine; the engine decides the rest.
	assert.ErrorIs(t, svc.SetPolicy(ctx, "bkt", json.RawMessage(`{not json`)), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.SetPolicy(ctx, "bkt", nil), domain.ErrInvalidArgument)
	invalid := json.RawMessage(`{"Version":"2008-10-17","Statement":[]}`)
	policies.EXPECT().ParseBucketPolicy(ctx, "bkt", []byte(invalid)).Return(nil, malformed).Once()
	err = svc.SetPolicy(ctx, "bkt", invalid)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	var s3err *domain.S3Error
	require.ErrorAs(t, err, &s3err)
	assert.Equal(t, "MalformedPolicy", s3err.Code)

	policies.EXPECT().ParseBucketPolicy(ctx, "bkt", []byte(raw)).Return(parsed, nil).Once()
	policies.EXPECT().ParseBucketPolicy(ctx, "missing", []byte(raw)).Return(parsed, nil).Once()
	policies.EXPECT().Parse(ctx, []byte(raw)).Return(parsed, nil).Once()
	require.NoError(t, svc.SetPolicy(ctx, "bkt", raw))
	got, err := svc.GetPolicy(ctx, "bkt")
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(got))
	assert.ErrorIs(t, svc.SetPolicy(ctx, "missing", raw), domain.ErrBucketNotFound)

	// IsPublic delegates the policy semantics to the engine.
	policies.EXPECT().IsPublic(ctx, parsed).Return(true).Once()
	public, err := svc.IsPublic(ctx, "bkt")
	require.NoError(t, err)
	assert.True(t, public)

	require.NoError(t, svc.DeletePolicy(ctx, "bkt"))
	_, err = svc.GetPolicy(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
}

func TestIsPublicRejectsCorruptStoredPolicy(t *testing.T) {
	ctx := context.Background()
	objects := objectmock.NewService(t)
	store := bucketmock.NewMetadataStore(t)
	policies := policymock.NewService(t)
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Policy: policies})
	require.NoError(t, err)
	raw := json.RawMessage(`{"Version":"2012-10-17"}`)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{}, nil).Once()
	store.EXPECT().Load(ctx, "bkt").Return(&bucket.Metadata{Name: "bkt", Policy: raw}, nil).Once()
	policies.EXPECT().Parse(ctx, []byte(raw)).Return(nil, domain.ErrInvalidArgument).Once()
	_, err = svc.IsPublic(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestMediaConfiguration(t *testing.T) {
	ctx := context.Background()
	svc, objects, _, _, _ := newService(t)
	objects.EXPECT().GetBucketInfo(ctx, "bkt").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt"}}, nil).Maybe()

	_, err := svc.GetMedia(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	cfg := domain.MediaConfig{Rules: []domain.MediaRule{{ID: "videos", Prefix: "v/", Renditions: []string{"480p", "1080p"}, Poster: true}}}
	require.NoError(t, svc.SetMedia(ctx, "bkt", cfg))
	assert.Equal(t, []string{"480p", "1080p"}, cfg.Rules[0].Renditions, "the caller's document is not modified")
	got, err := svc.GetMedia(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.MediaConfig{Rules: []domain.MediaRule{{ID: "videos", Prefix: "v/", Suffixes: []string{".m4v", ".mov", ".mp4"},
		Renditions: []string{"1080p", "480p"}, Poster: true}}}, got, "stored normalized")
	got.Rules[0].Prefix = "changed"
	again, err := svc.GetMedia(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, "v/", again.Rules[0].Prefix, "a copy is returned")

	err = svc.SetMedia(ctx, "bkt", domain.MediaConfig{Rules: []domain.MediaRule{{ID: "x"}}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "nothing to produce")

	require.NoError(t, svc.SetMedia(ctx, "bkt", domain.MediaConfig{}), "no rule removes the configuration")
	_, err = svc.GetMedia(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	require.NoError(t, svc.SetMedia(ctx, "bkt", cfg))
	require.NoError(t, svc.DeleteMedia(ctx, "bkt"))
	_, err = svc.GetMedia(ctx, "bkt")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
}
