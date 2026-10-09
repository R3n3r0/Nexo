package object_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// lockedBucket creates a bucket with object locking (and versioning) and
// the given default retention.
func lockedBucket(t *testing.T, fx *fixture, name string, def domain.DefaultRetention) {
	t.Helper()
	require.NoError(t, fx.svc.MakeBucket(context.Background(), name, object.MakeBucketOptions{LockEnabled: true, Versioning: domain.VersioningEnabled}))
	fx.setBucketConfig(name, object.BucketConfig{Versioning: domain.VersioningEnabled, ObjectLock: true, DefaultRetention: def})
}

// s3Code returns the S3 code of err ("" without one).
func s3Code(err error) string {
	var s3err *domain.S3Error
	if err != nil && errors.As(err, &s3err) {
		return s3err.Code
	}
	return ""
}

func withLock(r domain.ObjectRetention, hold domain.LegalHoldStatus) func(*object.PutObjectInput) {
	return func(in *object.PutObjectInput) {
		in.Retention, in.LegalHold = r, hold
		in.MD5Hex = md5hex(readAll(in))
	}
}

// readAll drains the reader of in and replaces it with a fresh one.
func readAll(in *object.PutObjectInput) []byte {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(in.Reader)
	in.Reader = bytes.NewReader(buf.Bytes())
	return buf.Bytes()
}

func TestObjectLockRetentionBlocksDeletes(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		lockedBucket(t, fx, "vault", domain.DefaultRetention{})
		now := fx.clk.Now()
		gov := put(t, fx.svc, "vault", "gov", []byte("g"), withLock(domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: now.Add(time.Hour)}, ""))
		comp := put(t, fx.svc, "vault", "comp", []byte("c"), withLock(domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: now.Add(time.Hour)}, ""))
		hold := put(t, fx.svc, "vault", "hold", []byte("h"), withLock(domain.ObjectRetention{}, domain.LegalHoldOn))
		assert.Equal(t, domain.RetentionGovernance, gov.Retention.Mode)
		assert.Equal(t, now.Add(time.Hour), gov.Retention.RetainUntil)
		assert.Equal(t, domain.LegalHoldOn, hold.LegalHold)

		// The stored state is reported by reads.
		info, err := fx.svc.GetObjectInfo(ctx, "vault", "comp", object.GetObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, comp.Retention, info.Retention)

		deleteVersion := func(key, version string, bypass bool) error {
			_, err := fx.svc.DeleteObject(ctx, "vault", key, object.DeleteObjectOptions{VersionID: version, BypassGovernance: bypass})
			return err
		}
		for _, tc := range []struct {
			key, version string
			bypass       bool
		}{
			{"gov", gov.VersionID, false},
			{"comp", comp.VersionID, false},
			{"comp", comp.VersionID, true},
			{"hold", hold.VersionID, false},
			{"hold", hold.VersionID, true},
		} {
			err := deleteVersion(tc.key, tc.version, tc.bypass)
			assert.ErrorIs(t, err, domain.ErrObjectLocked, "%s bypass=%v", tc.key, tc.bypass)
			assert.Equal(t, "AccessDenied", s3Code(err))
		}
		res, err := fx.svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "vault", Objects: []object.ObjectToDelete{
			{Key: "comp", VersionID: comp.VersionID}, {Key: "hold", VersionID: hold.VersionID},
		}, BypassGovernance: true})
		require.NoError(t, err)
		require.Len(t, res.Errors, 2)
		for _, e := range res.Errors {
			assert.ErrorIs(t, e.Err, domain.ErrObjectLocked)
		}

		// A delete without version id only adds a delete marker.
		del, err := fx.svc.DeleteObject(ctx, "vault", "comp", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.True(t, del.DeleteMarker)
		_, err = fx.svc.GetObjectInfo(ctx, "vault", "comp", object.GetObjectOptions{VersionID: comp.VersionID})
		require.NoError(t, err, "the locked version survives")
		// The marker itself can go.
		require.NoError(t, deleteVersion("comp", del.DeleteMarkerVersionID, false))

		// Governance yields to the bypass; the legal hold to its removal.
		require.NoError(t, deleteVersion("gov", gov.VersionID, true))
		_, err = fx.svc.PutObjectLegalHold(ctx, "vault", "hold", domain.LegalHoldOff, object.LockOptions{VersionID: hold.VersionID})
		require.NoError(t, err)
		require.NoError(t, deleteVersion("hold", hold.VersionID, false))

		// Compliance ends with its date.
		fx.clk.Advance(2 * time.Hour)
		require.NoError(t, deleteVersion("comp", comp.VersionID, false))
	})
}

func TestObjectLockDefaultRetention(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		lockedBucket(t, fx, "vault", domain.DefaultRetention{Mode: domain.RetentionCompliance, Days: 2})
		info := put(t, fx.svc, "vault", "k", []byte("data"))
		assert.Equal(t, domain.RetentionCompliance, info.Retention.Mode)
		assert.Equal(t, info.ModTime.AddDate(0, 0, 2), info.Retention.RetainUntil)
		r, err := fx.svc.GetObjectRetention(ctx, "vault", "k", object.LockOptions{})
		require.NoError(t, err)
		assert.Equal(t, info.Retention, r)
		_, err = fx.svc.GetObjectLegalHold(ctx, "vault", "k", object.LockOptions{})
		assert.Equal(t, "NoSuchObjectLockConfiguration", s3Code(err))

		// An explicit retention wins over the default.
		explicit := domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: fx.clk.Now().Add(time.Hour).Truncate(time.Millisecond)}
		info = put(t, fx.svc, "vault", "k2", []byte("data"), withLock(explicit, ""))
		assert.Equal(t, explicit, info.Retention)

		// Multipart uploads get theirs at completion.
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "vault", Key: "mp", LegalHold: domain.LegalHoldOn})
		require.NoError(t, err)
		p := uploadPart(t, fx.svc, "vault", "mp", up.UploadID, 1, []byte("part"))
		done, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "vault", Key: "mp", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}})
		require.NoError(t, err)
		assert.Equal(t, domain.LegalHoldOn, done.LegalHold)
		assert.Equal(t, domain.RetentionCompliance, done.Retention.Mode)
		assert.Equal(t, done.ModTime.AddDate(0, 0, 2), done.Retention.RetainUntil)

		// Copies get the settings of their own request, never the source's.
		cp, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "vault", SrcKey: "mp", DstBucket: "vault", DstKey: "copy"})
		require.NoError(t, err)
		assert.Empty(t, cp.LegalHold)
		assert.Equal(t, domain.RetentionCompliance, cp.Retention.Mode, "the default retention applies to the copy")
	})
}

func TestObjectLockRequestValidation(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		lockedBucket(t, fx, "vault", domain.DefaultRetention{})
		require.NoError(t, fx.svc.MakeBucket(ctx, "plain", object.MakeBucketOptions{}))
		future := domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: fx.clk.Now().Add(time.Hour)}
		putErr := func(bucket string, mutate func(*object.PutObjectInput)) error {
			in := object.PutObjectInput{Bucket: bucket, Key: "k", Reader: bytes.NewReader([]byte("x")), Size: 1}
			mutate(&in)
			_, err := fx.svc.PutObject(ctx, in)
			return err
		}
		tests := []struct {
			name   string
			bucket string
			mutate func(*object.PutObjectInput)
			code   string
		}{
			{"bucket without object lock", "plain", withLock(future, ""), "InvalidRequest"},
			{"legal hold without object lock", "plain", withLock(domain.ObjectRetention{}, domain.LegalHoldOn), "InvalidRequest"},
			{"no content digest", "vault", func(in *object.PutObjectInput) { in.Retention = future }, "InvalidRequest"},
			{"past date", "vault", withLock(domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: fx.clk.Now().Add(-time.Hour)}, ""), "InvalidArgument"},
			{"checksum instead of MD5", "vault", func(in *object.PutObjectInput) {
				in.Retention, in.Checksum = future, domain.Checksum{Algorithm: domain.ChecksumCRC32}
			}, ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := putErr(tt.bucket, tt.mutate)
				if tt.code == "" {
					require.NoError(t, err)
					return
				}
				assert.Equal(t, tt.code, s3Code(err), "%v", err)
			})
		}
		_, err := fx.svc.GetObjectRetention(ctx, "plain", "k", object.LockOptions{})
		assert.Equal(t, "InvalidRequest", s3Code(err))
		_, err = fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "plain", Key: "k", Retention: future})
		assert.Equal(t, "InvalidRequest", s3Code(err))
	})
}

func TestObjectLockPutRetention(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		lockedBucket(t, fx, "vault", domain.DefaultRetention{})
		now := fx.clk.Now()
		v := put(t, fx.svc, "vault", "k", []byte("x"))
		_, err := fx.svc.GetObjectRetention(ctx, "vault", "k", object.LockOptions{})
		assert.Equal(t, "NoSuchObjectLockConfiguration", s3Code(err))

		set := func(r domain.ObjectRetention, bypass bool) error {
			_, err := fx.svc.PutObjectRetention(ctx, "vault", "k", r, object.LockOptions{VersionID: v.VersionID, BypassGovernance: bypass})
			return err
		}
		gov := func(d time.Duration) domain.ObjectRetention {
			return domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: now.Add(d)}
		}
		require.NoError(t, set(gov(2*time.Hour), false))
		require.NoError(t, set(gov(3*time.Hour), false), "extension")
		assert.ErrorIs(t, set(gov(time.Hour), false), domain.ErrObjectLocked, "reduction")
		require.NoError(t, set(gov(time.Hour), true), "reduction with bypass")
		require.NoError(t, set(domain.ObjectRetention{}, true), "removal with bypass")
		_, err = fx.svc.GetObjectRetention(ctx, "vault", "k", object.LockOptions{VersionID: v.VersionID})
		assert.Equal(t, "NoSuchObjectLockConfiguration", s3Code(err))

		comp := domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: now.Add(time.Hour)}
		require.NoError(t, set(comp, false))
		assert.ErrorIs(t, set(domain.ObjectRetention{}, true), domain.ErrObjectLocked, "compliance cannot be removed")
		assert.ErrorIs(t, set(gov(2*time.Hour), true), domain.ErrObjectLocked, "nor turned into governance")
		assert.Equal(t, "InvalidArgument", s3Code(set(gov(-time.Hour), true)), "past dates are refused")
		got, err := fx.svc.GetObjectRetention(ctx, "vault", "k", object.LockOptions{})
		require.NoError(t, err)
		assert.Equal(t, comp, got)

		// Legal hold round trip.
		_, err = fx.svc.PutObjectLegalHold(ctx, "vault", "k", domain.LegalHoldOn, object.LockOptions{})
		require.NoError(t, err)
		hold, err := fx.svc.GetObjectLegalHold(ctx, "vault", "k", object.LockOptions{VersionID: v.VersionID})
		require.NoError(t, err)
		assert.Equal(t, domain.LegalHoldOn, hold)
		_, err = fx.svc.PutObjectLegalHold(ctx, "vault", "k", "MAYBE", object.LockOptions{})
		assert.Equal(t, "MalformedXML", s3Code(err))
	})
}

func TestObjectLockForcedBucketDeletion(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		lockedBucket(t, fx, "vault", domain.DefaultRetention{Mode: domain.RetentionGovernance, Days: 1})
		put(t, fx.svc, "vault", "dir/k", []byte("x"))
		err := fx.svc.DeleteBucket(ctx, "vault", object.DeleteBucketOptions{Force: true})
		assert.ErrorIs(t, err, domain.ErrObjectLocked)
		_, err = fx.svc.GetBucketInfo(ctx, "vault")
		require.NoError(t, err, "the bucket stays")
		fx.clk.Advance(48 * time.Hour)
		require.NoError(t, fx.svc.DeleteBucket(ctx, "vault", object.DeleteBucketOptions{Force: true}))
	})
}
