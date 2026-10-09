package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func noLock() error {
	return domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchObjectLockConfiguration", "none")
}

func TestBrowserRetentionAndLegalHoldReads(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowAll(t))
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ref := admin.VersionRef{Bucket: "bkt", Key: "doc", VersionID: "v1"}
	objects.EXPECT().GetObjectRetention(mock.Anything, "bkt", "doc", object.LockOptions{VersionID: "v1"}).
		Return(domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until}, nil)
	objects.EXPECT().GetObjectRetention(mock.Anything, "bkt", "plain", object.LockOptions{}).Return(domain.ObjectRetention{}, noLock())
	objects.EXPECT().GetObjectLegalHold(mock.Anything, "bkt", "doc", object.LockOptions{VersionID: "v1"}).Return(domain.LegalHoldOn, nil)
	objects.EXPECT().GetObjectLegalHold(mock.Anything, "bkt", "plain", object.LockOptions{}).Return("", noLock())
	objects.EXPECT().GetObjectLegalHold(mock.Anything, "unlocked", "x", object.LockOptions{}).
		Return("", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Bucket is missing Object Lock Configuration"))

	r, err := b.Retention(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until}, r)
	r, err = b.Retention(ctx, admin.VersionRef{Bucket: "bkt", Key: "plain"})
	require.NoError(t, err)
	assert.True(t, r.IsZero(), "no retention is the zero retention")
	hold, err := b.LegalHold(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, domain.LegalHoldOn, hold)
	hold, err = b.LegalHold(ctx, admin.VersionRef{Bucket: "bkt", Key: "plain"})
	require.NoError(t, err)
	assert.Equal(t, domain.LegalHoldOff, hold, "never set is OFF")
	_, err = b.LegalHold(ctx, admin.VersionRef{Bucket: "unlocked", Key: "x"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = b.Retention(ctx, admin.VersionRef{Bucket: "bkt", Key: "a//b"})
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
}

// TestBrowserSetRetentionMatrix (TS-U-12 at the console level): the
// retention changes are authorized as the S3 PutObjectRetention, the
// governance bypass asked explicitly needs s3:BypassGovernanceRetention,
// and the object layer decides what a lock forbids.
func TestBrowserSetRetentionMatrix(t *testing.T) {
	ctx := context.Background()
	until := time.Date(2030, 1, 1, 0, 0, 0, 0, time.FixedZone("CET", 3600))
	tests := []struct {
		name      string
		retention domain.ObjectRetention
		bypass    bool
		// may lists the actions the requester is allowed.
		may        []policy.Action
		layerErr   error
		wantErr    error
		wantCall   bool
		wantBypass bool
	}{
		{name: "governance set", retention: domain.ObjectRetention{Mode: "governance", RetainUntil: until},
			may: []policy.Action{policy.ActionPutObjectRetention}, wantCall: true},
		{name: "compliance set", retention: domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: until},
			may: []policy.Action{policy.ActionPutObjectRetention}, wantCall: true},
		{name: "no permission", retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until}, wantErr: domain.ErrAccessDenied},
		{name: "remove with bypass allowed", bypass: true,
			may: []policy.Action{policy.ActionPutObjectRetention, policy.ActionBypassGovernanceRetention}, wantCall: true, wantBypass: true},
		{name: "remove with bypass denied", bypass: true, may: []policy.Action{policy.ActionPutObjectRetention}, wantErr: domain.ErrAccessDenied},
		{name: "shorten without bypass", retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until},
			may: []policy.Action{policy.ActionPutObjectRetention}, wantCall: true, layerErr: domain.ErrObjectLocked, wantErr: domain.ErrObjectLocked},
		{name: "compliance cannot be bypassed", bypass: true, may: []policy.Action{policy.ActionPutObjectRetention, policy.ActionBypassGovernanceRetention},
			wantCall: true, wantBypass: true, layerErr: domain.ErrObjectLocked, wantErr: domain.ErrObjectLocked},
		{name: "mode without date", retention: domain.ObjectRetention{Mode: domain.RetentionGovernance}, wantErr: domain.ErrInvalidArgument},
		{name: "unknown mode", retention: domain.ObjectRetention{Mode: "FOREVER", RetainUntil: until}, wantErr: domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []admin.AuthorizeInput
			b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool {
				asked = append(asked, in)
				for _, a := range tt.may {
					if a == in.Actions[0] {
						return true
					}
				}
				return false
			}))
			if tt.wantCall {
				want := domain.ObjectRetention{}
				if !tt.retention.IsZero() {
					mode, err := domain.ParseRetentionMode(string(tt.retention.Mode))
					require.NoError(t, err)
					want = domain.ObjectRetention{Mode: mode, RetainUntil: until.UTC()}
				}
				objects.EXPECT().PutObjectRetention(mock.Anything, "bkt", "doc", want, object.LockOptions{VersionID: "v1", BypassGovernance: tt.wantBypass}).
					Return(domain.ObjectInfo{Key: "doc", VersionID: "v1", Retention: want}, tt.layerErr)
			}
			info, err := b.SetRetention(ctx, admin.SetRetentionInput{Requester: req("bob"), Object: admin.VersionRef{Bucket: "bkt", Key: "doc", VersionID: "v1"},
				Retention: tt.retention, BypassGovernance: tt.bypass})
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "v1", info.VersionID)
			for _, in := range asked {
				assert.Equal(t, []string{"v1"}, in.Conditions["s3:versionid"], "the version is a condition value")
				if tt.bypass {
					assert.Equal(t, []string{"true"}, in.Conditions["s3:x-amz-bypass-governance-retention"], "the bypass header is a condition value")
				}
			}
		})
	}
}

func TestBrowserSetLegalHold(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowAll(t))
	objects.EXPECT().PutObjectLegalHold(mock.Anything, "bkt", "doc", domain.LegalHoldOn, object.LockOptions{VersionID: "v2"}).
		Return(domain.ObjectInfo{Key: "doc", VersionID: "v2", LegalHold: domain.LegalHoldOn}, nil)
	info, err := b.SetLegalHold(ctx, admin.SetLegalHoldInput{Object: admin.VersionRef{Bucket: "bkt", Key: "doc", VersionID: "v2"}, Hold: "on"})
	require.NoError(t, err)
	assert.Equal(t, domain.LegalHoldOn, info.LegalHold)
	_, err = b.SetLegalHold(ctx, admin.SetLegalHoldInput{Object: admin.VersionRef{Bucket: "bkt", Key: "doc"}, Hold: "maybe"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestBrowserAttributes(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowAll(t))
	want := object.ObjectAttributes{Info: domain.ObjectInfo{Key: "big", Size: 10}, PartsCount: 2,
		Parts: []domain.ObjectPart{{Number: 2, Size: 4}}, NextPartNumberMarker: 2}
	objects.EXPECT().GetObjectAttributes(mock.Anything, "bkt", "big", object.AttributesOptions{VersionID: "v", MaxParts: 1, PartNumberMarker: 1}).Return(want, nil)
	got, err := b.Attributes(ctx, admin.AttributesInput{Object: admin.VersionRef{Bucket: "bkt", Key: "big", VersionID: "v"}, MaxParts: 1, PartNumberMarker: 1})
	require.NoError(t, err)
	assert.Equal(t, &want, got)
	for _, in := range []admin.AttributesInput{
		{Object: admin.VersionRef{Bucket: "bkt", Key: "big"}, MaxParts: 1001},
		{Object: admin.VersionRef{Bucket: "bkt", Key: "big"}, PartNumberMarker: -1},
	} {
		_, err := b.Attributes(ctx, in)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	}
}

// TestBrowserDeleteBypassGovernance: a console deletion asking to bypass
// the governance retention bypasses it for the versions the requester may
// (s3:BypassGovernanceRetention), as the S3 DeleteObjects; the others are
// deleted without, so that the object layer refuses the locked ones.
func TestBrowserDeleteBypassGovernance(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool {
		if in.Actions[0] == policy.ActionBypassGovernanceRetention {
			assert.Equal(t, []string{"true"}, in.Conditions["s3:x-amz-bypass-governance-retention"])
			return in.Object == "gov/a"
		}
		return true
	}))
	objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "bkt", Objects: []object.ObjectToDelete{{Key: "other/b", VersionID: "v2"}, {Key: "latest"}}}).
		Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "latest", DeleteMarker: true}},
			Errors: []object.DeleteError{{Key: "other/b", VersionID: "v2", Err: domain.ErrObjectLocked}}}, nil)
	objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "bkt", Objects: []object.ObjectToDelete{{Key: "gov/a", VersionID: "v1"}}, BypassGovernance: true}).
		Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "gov/a", VersionID: "v1"}}}, nil)
	res, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: req("bob"), Bucket: "bkt", BypassGovernance: true,
		Objects: []admin.ObjectRef{{Key: "gov/a", VersionID: "v1"}, {Key: "other/b", VersionID: "v2"}, {Key: "latest"}}})
	require.NoError(t, err)
	assert.Len(t, res.Deleted, 2)
	require.Len(t, res.Errors, 1)
	assert.ErrorIs(t, res.Errors[0].Err, domain.ErrObjectLocked)
}
