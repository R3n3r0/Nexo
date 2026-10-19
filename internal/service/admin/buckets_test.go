package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	accessrulemock "github.com/r3n3r0/nexo/internal/service/accessrule/mock"
	"github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// allowing returns an authorizer mock deciding with allow(action, bucket,
// object).
func allowing(t *testing.T, allow func(in admin.AuthorizeInput) bool) *adminmock.Authorizer {
	a := adminmock.NewAuthorizer(t)
	a.EXPECT().Authorize(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in admin.AuthorizeInput) (admin.Decision, error) {
		if allow(in) {
			return admin.Decision{Allowed: true}, nil
		}
		return admin.Decision{DeniedAction: in.Actions[0], Reason: "denied"}, nil
	}).Maybe()
	return a
}

func allowAll(t *testing.T) *adminmock.Authorizer {
	return allowing(t, func(admin.AuthorizeInput) bool { return true })
}

type bucketEnv struct {
	buckets *bucketmock.Service
	objects *objectmock.Service
	policy  *policymock.Service
	mgr     admin.BucketManager
}

func newBucketEnv(t *testing.T, authz admin.Authorizer) *bucketEnv {
	e := &bucketEnv{buckets: bucketmock.NewService(t), objects: objectmock.NewService(t), policy: policymock.NewService(t)}
	mgr, err := admin.NewBucketManager(admin.BucketManagerDeps{Buckets: e.buckets, Objects: e.objects, Policy: e.policy, Authz: authz})
	require.NoError(t, err)
	e.mgr = mgr
	return e
}

func info(name string) domain.BucketInfo { return domain.BucketInfo{Bucket: domain.Bucket{Name: name}} }

func TestBucketManagerList(t *testing.T) {
	ctx := context.Background()
	t.Run("filters by s3:ListBucket without s3:ListAllMyBuckets", func(t *testing.T) {
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool {
			return in.Actions[0] == policy.ActionListBucket && in.Bucket == "team"
		}))
		e.buckets.EXPECT().List(mock.Anything, bucket.ListInput{WithUsage: true}).Return([]domain.BucketInfo{info("other"), info("team")}, nil)
		e.buckets.EXPECT().IsPublic(mock.Anything, "team").Return(false, fmt.Errorf("x: %w", domain.ErrPolicyNotFound))
		got, err := e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("bob")})
		require.NoError(t, err)
		require.Len(t, got.Items, 1)
		assert.Equal(t, "team", got.Items[0].Info.Name)
		assert.False(t, got.Items[0].Public)
		assert.Empty(t, got.NextCursor)
	})
	// Regression: a bucket listed in part (home directory) disclosed its
	// usage, tags, quota and public flag.
	t.Run("a bucket listed in part shows only its name", func(t *testing.T) {
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool {
			return in.Actions[0] == policy.ActionListBucket && (in.Bucket == "whole" || in.Partial)
		}))
		home := domain.BucketInfo{Bucket: domain.Bucket{Name: "home", Region: "r1"}, Versioning: domain.VersioningEnabled, Objects: 9, Size: 99, QuotaBytes: 5, Tags: map[string]string{"k": "v"}}
		whole := domain.BucketInfo{Bucket: domain.Bucket{Name: "whole"}, Objects: 1}
		e.buckets.EXPECT().List(mock.Anything, bucket.ListInput{WithUsage: true}).Return([]domain.BucketInfo{home, whole}, nil)
		e.buckets.EXPECT().IsPublic(mock.Anything, "whole").Return(true, nil)
		got, err := e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("carol")})
		require.NoError(t, err)
		require.Len(t, got.Items, 2)
		assert.Equal(t, admin.BucketSummary{Info: domain.BucketInfo{Bucket: home.Bucket}, Partial: true}, got.Items[0])
		assert.Equal(t, admin.BucketSummary{Info: whole, Public: true}, got.Items[1])
	})
	t.Run("s3:ListAllMyBuckets alone shows the names", func(t *testing.T) {
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Actions[0] == policy.ActionListAllMyBuckets }))
		e.buckets.EXPECT().List(mock.Anything, bucket.ListInput{WithUsage: true}).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "b"}, Objects: 3}}, nil)
		got, err := e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("bob")})
		require.NoError(t, err)
		require.Len(t, got.Items, 1)
		assert.True(t, got.Items[0].Partial)
		assert.Zero(t, got.Items[0].Info.Objects)
	})
	t.Run("every bucket with s3:ListAllMyBuckets", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.buckets.EXPECT().List(mock.Anything, bucket.ListInput{Prefix: "t", WithUsage: true}).Return([]domain.BucketInfo{info("ta"), info("tb")}, nil)
		e.buckets.EXPECT().IsPublic(mock.Anything, "ta").Return(true, nil)
		e.buckets.EXPECT().IsPublic(mock.Anything, "tb").Return(false, errDown)
		got, err := e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("bob"), Prefix: "t"})
		require.NoError(t, err)
		require.Len(t, got.Items, 2)
		assert.True(t, got.Items[0].Public)
		assert.False(t, got.Items[1].Public, "a failure is not public")
	})
	// covers: docs/12 §12.3 (the service selects the page; regression: the
	// handler sorted and paginated the whole list)
	t.Run("pages by name, authorizing only what it serves", func(t *testing.T) {
		var asked []string
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool {
			if in.Actions[0] == policy.ActionListAllMyBuckets {
				return false
			}
			if !in.Partial {
				asked = append(asked, in.Bucket)
			}
			return in.Bucket != "b"
		}))
		e.buckets.EXPECT().List(mock.Anything, bucket.ListInput{WithUsage: true}).Return([]domain.BucketInfo{info("d"), info("a"), info("c"), info("b"), info("e")}, nil).Twice()
		e.buckets.EXPECT().IsPublic(mock.Anything, mock.Anything).Return(false, nil)
		got, err := e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("bob"), Page: admin.Page{Limit: 2}})
		require.NoError(t, err)
		require.Len(t, got.Items, 2)
		assert.Equal(t, []string{"a", "c"}, []string{got.Items[0].Info.Name, got.Items[1].Info.Name}, "b is not visible")
		require.NotEmpty(t, got.NextCursor)
		assert.Equal(t, []string{"a", "b", "c", "d"}, asked, "one bucket past the page tells that more follow")
		got, err = e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("bob"), Page: admin.Page{Cursor: got.NextCursor, Limit: 2}})
		require.NoError(t, err)
		require.Len(t, got.Items, 2)
		assert.Equal(t, []string{"d", "e"}, []string{got.Items[0].Info.Name, got.Items[1].Info.Name})
		assert.Empty(t, got.NextCursor)
	})
	t.Run("invalid page", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		for _, p := range []admin.Page{{Limit: -1}, {Limit: admin.MaxPageLimit + 1}, {Cursor: "%%%"}} {
			_, err := e.mgr.List(ctx, admin.ListBucketsInput{Requester: req("bob"), Page: p})
			assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", p)
		}
	})
}

func TestBucketManagerGet(t *testing.T) {
	ctx := context.Background()
	t.Run("whole bucket", func(t *testing.T) {
		var asked []admin.AuthorizeInput
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return true }))
		b := info("photos")
		e.buckets.EXPECT().Get(mock.Anything, "photos").Return(&b, nil)
		e.objects.EXPECT().DataUsage(mock.Anything, "photos").Return([]domain.BucketInfo{{Objects: 3, Size: 30, Versions: 1}}, nil)
		e.buckets.EXPECT().IsPublic(mock.Anything, "photos").Return(true, nil)
		got, err := e.mgr.Get(ctx, req("bob"), "photos")
		require.NoError(t, err)
		assert.Equal(t, uint64(3), got.Info.Objects)
		assert.Equal(t, uint64(30), got.Info.Size)
		assert.True(t, got.Public)
		assert.False(t, got.Partial)
		require.Len(t, asked, 1)
		assert.Equal(t, admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionListBucket}, Bucket: "photos"}, asked[0],
			"the whole bucket: no partial authorization")
	})
	// Regression: GET /buckets/{bucket} answered a home directory user with
	// the usage, tags, quota and public flag of the whole bucket.
	t.Run("listed in part: name, creation date and region only", func(t *testing.T) {
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Partial }))
		created := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		b := domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt", Created: created, Region: "eu"}, Versioning: domain.VersioningEnabled,
			ObjectLockEnabled: true, Objects: 7, Versions: 2, DeleteMarkers: 1, Size: 70, QuotaBytes: 100, Tags: map[string]string{"team": "x"}}
		e.buckets.EXPECT().Get(mock.Anything, "bkt").Return(&b, nil)
		got, err := e.mgr.Get(ctx, req("carol"), "bkt")
		require.NoError(t, err)
		assert.Equal(t, &admin.BucketSummary{Info: domain.BucketInfo{Bucket: domain.Bucket{Name: "bkt", Created: created, Region: "eu"}}, Partial: true}, got)
	})
	t.Run("missing bucket", func(t *testing.T) {
		e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Partial }))
		e.buckets.EXPECT().Get(mock.Anything, "nope").Return(nil, fmt.Errorf("x: %w", domain.ErrBucketNotFound))
		_, err := e.mgr.Get(ctx, req("carol"), "nope")
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	})
}

func TestBucketManagerCreate(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		in        admin.CreateBucketInput
		actions   []policy.Action
		deny      policy.Action
		setup     func(e *bucketEnv)
		wantErr   error
		versioned bool
	}{
		{name: "invalid name", in: admin.CreateBucketInput{Name: "A"}, wantErr: domain.ErrInvalidBucketName},
		{name: "settings need their actions", in: admin.CreateBucketInput{Name: "photos", Versioning: true, QuotaBytes: 10},
			deny: policy.ActionAdminSetBucketQuota, wantErr: domain.ErrAccessDenied},
		{name: "object lock implies versioning", in: admin.CreateBucketInput{Name: "vault", ObjectLock: true, Versioning: true},
			actions: []policy.Action{policy.ActionCreateBucket, policy.ActionPutBucketObjectLock},
			setup: func(e *bucketEnv) {
				e.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "vault", ObjectLock: true}).Return(&domain.Bucket{Name: "vault"}, nil)
				b := info("vault")
				e.buckets.EXPECT().Get(mock.Anything, "vault").Return(&b, nil)
				e.objects.EXPECT().DataUsage(mock.Anything, "vault").Return(nil, nil)
				e.buckets.EXPECT().IsPublic(mock.Anything, "vault").Return(false, nil)
			}},
		{name: "hls sets the media rule", in: admin.CreateBucketInput{Name: "videos", HLS: true},
			actions: []policy.Action{policy.ActionCreateBucket, policy.ActionAdminSetBucketMedia},
			setup: func(e *bucketEnv) {
				e.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "videos"}).Return(&domain.Bucket{Name: "videos"}, nil)
				e.buckets.EXPECT().SetMedia(mock.Anything, "videos", domain.MediaConfig{Rules: []domain.MediaRule{domain.HLSMediaRule()}}).Return(nil)
				b := info("videos")
				e.buckets.EXPECT().Get(mock.Anything, "videos").Return(&b, nil)
				e.objects.EXPECT().DataUsage(mock.Anything, "videos").Return(nil, nil)
				e.buckets.EXPECT().IsPublic(mock.Anything, "videos").Return(false, nil)
			}},
		{name: "hls needs its action", in: admin.CreateBucketInput{Name: "videos", HLS: true},
			deny: policy.ActionAdminSetBucketMedia, wantErr: domain.ErrAccessDenied},
		{name: "images sets the media rule", in: admin.CreateBucketInput{Name: "photos", Images: true},
			actions: []policy.Action{policy.ActionCreateBucket, policy.ActionAdminSetBucketMedia},
			setup: func(e *bucketEnv) {
				e.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos"}).Return(&domain.Bucket{Name: "photos"}, nil)
				e.buckets.EXPECT().SetMedia(mock.Anything, "photos", domain.MediaConfig{Rules: []domain.MediaRule{domain.ImagesMediaRule()}}).Return(nil)
				b := info("photos")
				e.buckets.EXPECT().Get(mock.Anything, "photos").Return(&b, nil)
				e.objects.EXPECT().DataUsage(mock.Anything, "photos").Return(nil, nil)
				e.buckets.EXPECT().IsPublic(mock.Anything, "photos").Return(false, nil)
			}},
		{name: "hls and images set both rules", in: admin.CreateBucketInput{Name: "media", HLS: true, Images: true},
			actions: []policy.Action{policy.ActionCreateBucket, policy.ActionAdminSetBucketMedia},
			setup: func(e *bucketEnv) {
				e.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "media"}).Return(&domain.Bucket{Name: "media"}, nil)
				e.buckets.EXPECT().SetMedia(mock.Anything, "media",
					domain.MediaConfig{Rules: []domain.MediaRule{domain.HLSMediaRule(), domain.ImagesMediaRule()}}).Return(nil)
				b := info("media")
				e.buckets.EXPECT().Get(mock.Anything, "media").Return(&b, nil)
				e.objects.EXPECT().DataUsage(mock.Anything, "media").Return(nil, nil)
				e.buckets.EXPECT().IsPublic(mock.Anything, "media").Return(false, nil)
			}},
		{name: "images needs its action", in: admin.CreateBucketInput{Name: "photos", Images: true},
			deny: policy.ActionAdminSetBucketMedia, wantErr: domain.ErrAccessDenied},
		{name: "a failed setting removes the bucket", in: admin.CreateBucketInput{Name: "photos", Versioning: true, QuotaBytes: 10},
			setup: func(e *bucketEnv) {
				e.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos"}).Return(&domain.Bucket{Name: "photos"}, nil)
				e.buckets.EXPECT().SetVersioning(mock.Anything, "photos", domain.VersioningEnabled).Return(nil)
				e.buckets.EXPECT().SetQuota(mock.Anything, "photos", bucket.Quota{Type: bucket.QuotaHard, Size: 10}).Return(bucket.Quota{}, errDown)
				e.buckets.EXPECT().Delete(mock.Anything, "photos", bucket.DeleteOptions{}).Return(nil)
			}, wantErr: errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []policy.Action
			e := newBucketEnv(t, allowing(t, func(in admin.AuthorizeInput) bool {
				asked = append(asked, in.Actions...)
				for _, a := range in.Actions {
					if a == tt.deny {
						return false
					}
				}
				return in.Bucket == tt.in.Name
			}))
			if tt.setup != nil {
				tt.setup(e)
			}
			_, err := e.mgr.Create(ctx, tt.in)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.actions, asked)
		})
	}
}

func TestBucketManagerForcedDelete(t *testing.T) {
	ctx := context.Background()
	t.Run("empties through the object layer", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.objects.EXPECT().ListObjectVersions(mock.Anything, domain.ListObjectsInput{Bucket: "b", MaxKeys: 1000}).
			Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "a", VersionID: "v1"}, {Key: "a", VersionID: "v2", IsDeleteMarker: true}}, IsTruncated: true, NextMarker: "a", NextVersionIDMarker: "v2"}, nil)
		e.objects.EXPECT().ListObjectVersions(mock.Anything, domain.ListObjectsInput{Bucket: "b", Marker: "a", VersionIDMarker: "v2", MaxKeys: 1000}).
			Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "b"}}}, nil)
		e.objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "b", Quiet: true, Objects: []object.ObjectToDelete{{Key: "a", VersionID: "v1"}, {Key: "a", VersionID: "v2"}}}).
			Return(object.DeleteObjectsResult{}, nil)
		// The null version listed without id goes for good: as "null"
		// (without a version id a versioned bucket would get a marker).
		e.objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "b", Quiet: true, Objects: []object.ObjectToDelete{{Key: "b", VersionID: "null"}}}).
			Return(object.DeleteObjectsResult{}, nil)
		e.objects.EXPECT().ListMultipartUploads(mock.Anything, domain.ListMultipartUploadsInput{Bucket: "b"}).
			Return(domain.ListMultipartUploadsResult{Uploads: []domain.MultipartUpload{{Key: "c", UploadID: "u1"}}}, nil)
		e.objects.EXPECT().AbortMultipartUpload(mock.Anything, "b", "c", "u1").Return(nil)
		e.buckets.EXPECT().Delete(mock.Anything, "b", bucket.DeleteOptions{}).Return(nil)
		require.NoError(t, e.mgr.Delete(ctx, admin.DeleteBucketInput{Name: "b", Force: true}))
	})
	t.Run("a locked version keeps the bucket", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.objects.EXPECT().ListObjectVersions(mock.Anything, mock.Anything).Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "a", VersionID: "v1"}}}, nil)
		e.objects.EXPECT().DeleteObjects(mock.Anything, mock.Anything).
			Return(object.DeleteObjectsResult{Errors: []object.DeleteError{{Key: "a", VersionID: "v1", Err: domain.ErrObjectLocked}}}, nil)
		e.objects.EXPECT().ListMultipartUploads(mock.Anything, mock.Anything).Return(domain.ListMultipartUploadsResult{}, nil)
		err := e.mgr.Delete(ctx, admin.DeleteBucketInput{Name: "b", Force: true})
		assert.ErrorIs(t, err, domain.ErrBucketNotEmpty)
		assert.ErrorIs(t, err, domain.ErrObjectLocked)
	})
	t.Run("without force the bucket must be empty", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.buckets.EXPECT().Delete(mock.Anything, "b", bucket.DeleteOptions{}).Return(fmt.Errorf("x: %w", domain.ErrBucketNotEmpty))
		assert.ErrorIs(t, e.mgr.Delete(ctx, admin.DeleteBucketInput{Name: "b"}), domain.ErrBucketNotEmpty)
	})
}

func TestBucketManagerAccess(t *testing.T) {
	ctx := context.Background()
	doc := &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{Effect: domain.Allow}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)

	t.Run("no policy", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(nil, fmt.Errorf("x: %w", domain.ErrPolicyNotFound))
		a, err := e.mgr.Access(ctx, "b")
		require.NoError(t, err)
		assert.Equal(t, &admin.BucketAccess{Bucket: "b", Rules: []policy.PublicAccessRule{}}, a)
	})
	t.Run("set a preset stores the policy", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(nil, fmt.Errorf("x: %w", domain.ErrPolicyNotFound)).Once()
		e.policy.EXPECT().ApplyPublicPreset(mock.Anything, (*domain.Policy)(nil), "b", "pub/", policy.PresetDownload).Return(doc, nil)
		e.buckets.EXPECT().SetPolicy(mock.Anything, "b", json.RawMessage(raw)).Return(nil)
		e.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(raw, nil).Once()
		e.policy.EXPECT().ParseBucketPolicy(mock.Anything, "b", []byte(raw)).Return(doc, nil)
		e.policy.EXPECT().IsPublic(mock.Anything, doc).Return(true)
		e.policy.EXPECT().ListPublicAccess(mock.Anything, doc, "b").Return([]policy.PublicAccessRule{{Prefix: "pub/", Preset: policy.PresetDownload}})
		a, err := e.mgr.SetAccess(ctx, admin.SetAccessInput{Bucket: "b", Prefix: "pub/", Preset: policy.PresetDownload})
		require.NoError(t, err)
		assert.True(t, a.Public)
		assert.Len(t, a.Rules, 1)
	})
	t.Run("private on the last rule deletes the policy", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		e.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(raw, nil).Once()
		e.policy.EXPECT().ParseBucketPolicy(mock.Anything, "b", []byte(raw)).Return(doc, nil)
		e.policy.EXPECT().ApplyPublicPreset(mock.Anything, doc, "b", "", policy.PresetPrivate).Return(nil, nil)
		e.buckets.EXPECT().DeletePolicy(mock.Anything, "b").Return(nil)
		e.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(nil, fmt.Errorf("x: %w", domain.ErrPolicyNotFound)).Once()
		a, err := e.mgr.SetAccess(ctx, admin.SetAccessInput{Bucket: "b", Preset: policy.PresetPrivate})
		require.NoError(t, err)
		assert.False(t, a.HasPolicy)
	})
	t.Run("custom cannot be applied", func(t *testing.T) {
		e := newBucketEnv(t, allowAll(t))
		_, err := e.mgr.SetAccess(ctx, admin.SetAccessInput{Bucket: "b", Preset: policy.PresetCustom})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}

// TestBucketManagerTagsAndObjectLockDefaults: the Admin API represents a
// bucket without tags as an empty set and one without object lock as
// disabled; other errors pass through.
func TestBucketManagerTagsAndObjectLockDefaults(t *testing.T) {
	ctx := context.Background()
	notFound := fmt.Errorf("x: %w", domain.ErrPolicyNotFound)
	tests := []struct {
		name     string
		tags     map[string]string
		tagsErr  error
		wantTags map[string]string
		lock     bucket.ObjectLockConfig
		lockErr  error
		wantLock bucket.ObjectLockConfig
		wantErr  error
	}{
		{name: "none configured", tagsErr: notFound, lockErr: notFound, wantTags: map[string]string{}},
		{name: "nil set", wantTags: map[string]string{}},
		{name: "configured", tags: map[string]string{"team": "a"}, wantTags: map[string]string{"team": "a"},
			lock: bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionMode("GOVERNANCE"), Days: 1}, wantLock: bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionMode("GOVERNANCE"), Days: 1}},
		{name: "failures pass through", tagsErr: errDown, lockErr: errDown, wantErr: errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newBucketEnv(t, allowAll(t))
			e.buckets.EXPECT().GetTags(mock.Anything, "b").Return(tt.tags, tt.tagsErr)
			e.buckets.EXPECT().GetObjectLock(mock.Anything, "b").Return(tt.lock, tt.lockErr)
			tags, err := e.mgr.Tags(ctx, "b")
			lock, lockErr := e.mgr.ObjectLock(ctx, "b")
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.ErrorIs(t, lockErr, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.NoError(t, lockErr)
			assert.Equal(t, tt.wantTags, tags)
			assert.NotNil(t, tags)
			assert.Equal(t, tt.wantLock, lock)
		})
	}
}

// TestBucketManagerPublicThroughAccessRules covers docs/10 D-ACC-03: a
// bucket public through an access rule is shown public.
func TestBucketManagerPublicThroughAccessRules(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		policyPublic bool
		rules        *accessrule.PublicStatus
		rulesErr     error
		want         bool
	}{
		{name: "public bucket policy", policyPublic: true, want: true},
		{name: "public access rule", rules: &accessrule.PublicStatus{Public: true, Preset: accessrule.PresetPublicRead}, want: true},
		{name: "blocked access rule", rules: &accessrule.PublicStatus{Preset: accessrule.PresetPublicRead, Blocked: true}},
		{name: "rules unreadable", rulesErr: errors.New("boom")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buckets, objects, rules := bucketmock.NewService(t), objectmock.NewService(t), accessrulemock.NewService(t)
			mgr, err := admin.NewBucketManager(admin.BucketManagerDeps{Buckets: buckets, Objects: objects, Policy: policymock.NewService(t),
				Authz: allowAll(t), AccessRules: rules})
			require.NoError(t, err)
			b := info("photos")
			buckets.EXPECT().Get(mock.Anything, "photos").Return(&b, nil)
			objects.EXPECT().DataUsage(mock.Anything, "photos").Return(nil, nil)
			buckets.EXPECT().IsPublic(mock.Anything, "photos").Return(tc.policyPublic, nil)
			if !tc.policyPublic {
				rules.EXPECT().PublicStatus(mock.Anything, "photos").Return(tc.rules, tc.rulesErr)
			}
			got, err := mgr.Get(ctx, req("bob"), "photos")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Public)
		})
	}
}
