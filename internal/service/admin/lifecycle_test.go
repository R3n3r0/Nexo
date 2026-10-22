package admin_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	ilmmock "github.com/r3n3r0/nexo/internal/service/ilm/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
)

// lifecycleEnv is a lifecycle manager over a mocked bucket service and
// the real lifecycle parser (its dependencies mocked: parsing never uses
// them), so that the conversion is checked against the S3 rules.
type lifecycleEnv struct {
	buckets *bucketmock.Service
	ilm     ilm.Service
	mgr     admin.LifecycleManager
}

func newLifecycleEnv(t *testing.T, lifecycle ilm.Service) *lifecycleEnv {
	t.Helper()
	e := &lifecycleEnv{buckets: bucketmock.NewService(t), ilm: lifecycle}
	if e.ilm == nil {
		parser, err := ilm.New(ilm.Deps{Objects: objectmock.NewService(t), Buckets: ilmmock.NewBucketSettings(t)}, ilm.WithStorageClasses("WARM"))
		require.NoError(t, err)
		e.ilm = parser
	}
	mgr, err := admin.NewLifecycleManager(admin.LifecycleManagerDeps{Buckets: e.buckets, Lifecycle: e.ilm})
	require.NoError(t, err)
	e.mgr = mgr
	return e
}

func intp(n int) *int       { return &n }
func int64p(n int64) *int64 { return &n }

func TestNewLifecycleManagerRequiresDeps(t *testing.T) {
	_, err := admin.NewLifecycleManager(admin.LifecycleManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestLifecycleRoundTrip: the console form becomes the S3 document the
// bucket service stores, and comes back from it unchanged (IDs generated
// for the rules without).
func TestLifecycleRoundTrip(t *testing.T) {
	ctx := context.Background()
	e := newLifecycleEnv(t, nil)
	cfg := admin.LifecycleConfig{Rules: []admin.LifecycleRule{
		{ID: "logs", Enabled: true, Filter: admin.LifecycleFilter{Prefix: "logs/"}, Expiration: &admin.LifecycleExpiration{Days: intp(30)}},
		{ID: "tagged", Enabled: false, Filter: admin.LifecycleFilter{Prefix: "tmp/", Tags: map[string]string{"b": "2", "a": "1"}, SizeGreaterThan: int64p(10), SizeLessThan: int64p(1000)},
			Expiration: &admin.LifecycleExpiration{Date: "2030-01-01"}},
		{Enabled: true, NoncurrentExpiration: &admin.LifecycleNoncurrentExpiration{NoncurrentDays: intp(7), NewerNoncurrentVersions: intp(2)},
			AbortIncompleteUpload: &admin.LifecycleAbortUpload{DaysAfterInitiation: intp(3)}},
		{ID: "markers", Enabled: true, Filter: admin.LifecycleFilter{SizeLessThan: int64p(5)}, Expiration: &admin.LifecycleExpiration{ExpiredObjectDeleteMarker: true}},
		{ID: "tier", Enabled: true, Filter: admin.LifecycleFilter{Tags: map[string]string{"k": "v"}},
			Transitions:           []admin.LifecycleTransition{{Days: intp(0), StorageClass: "WARM"}, {Date: "2031-06-01T00:00:00Z", StorageClass: "WARM"}},
			NoncurrentTransitions: []admin.LifecycleNoncurrentTransition{{NoncurrentDays: intp(1), StorageClass: "WARM"}}},
	}}
	var stored []byte
	e.buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningEnabled, nil)
	e.buckets.EXPECT().SetLifecycle(mock.Anything, "bkt", mock.Anything).RunAndReturn(func(_ context.Context, _ string, doc []byte) error {
		stored = doc
		return nil
	})
	got, err := e.mgr.Set(ctx, "bkt", cfg)
	require.NoError(t, err)
	require.Len(t, got.Rules, 5)
	assert.NotEmpty(t, got.Rules[2].ID, "a rule without ID gets one")
	cfg.Rules[2].ID = got.Rules[2].ID
	cfg.Rules[4].Transitions[1].Date = "2031-06-01"
	assert.Equal(t, cfg, *got)

	// The stored document is the canonical S3 one, read back by Get.
	parsed, err := e.ilm.Parse(ctx, stored)
	require.NoError(t, err)
	require.Len(t, parsed.Rules, 5)
	assert.Equal(t, 30, parsed.Rules[0].Expiration.Days)
	e.buckets.EXPECT().GetLifecycle(mock.Anything, "bkt").Return(stored, nil)
	again, err := e.mgr.Get(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

// TestLifecycleValidationPerRule: every invalid rule is reported with
// its index, ID and S3 code; nothing is stored.
func TestLifecycleValidationPerRule(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		rules []admin.LifecycleRule
		want  []admin.RuleProblem
	}{
		{
			name: "zero days and two expirations",
			rules: []admin.LifecycleRule{
				{ID: "ok", Enabled: true, Expiration: &admin.LifecycleExpiration{Days: intp(1)}},
				{ID: "zero", Enabled: true, Expiration: &admin.LifecycleExpiration{Days: intp(0)}},
				{ID: "both", Enabled: true, Expiration: &admin.LifecycleExpiration{Days: intp(1), Date: "2030-01-01"}},
			},
			want: []admin.RuleProblem{
				{Rule: 2, ID: "zero", Code: "InvalidArgument", Message: "'Days' for Expiration action must be a positive integer"},
				{Rule: 3, ID: "both", Code: "InvalidRequest", Message: "Expiration must have exactly one of Days, Date or ExpiredObjectDeleteMarker"},
			},
		},
		{
			name: "no action, duplicate id, date not at midnight",
			rules: []admin.LifecycleRule{
				{ID: "a", Enabled: true},
				{ID: "a", Enabled: true, Expiration: &admin.LifecycleExpiration{Days: intp(1)}},
				{ID: "c", Enabled: true, Expiration: &admin.LifecycleExpiration{Date: "2030-01-01T10:00:00Z"}},
			},
			want: []admin.RuleProblem{
				{Rule: 1, ID: "a", Code: "InvalidRequest", Message: "At least one action needs to be specified in a rule"},
				{Rule: 2, ID: "a", Code: "InvalidArgument", Message: `the rule ID "a" is already used by rule 1`},
				{Rule: 3, ID: "c", Code: "InvalidArgument", Message: "'Date' must be at midnight GMT"},
			},
		},
		{
			name: "transition without tier, tags with abort",
			rules: []admin.LifecycleRule{
				{ID: "cold", Enabled: true, Transitions: []admin.LifecycleTransition{{Days: intp(1), StorageClass: "GLACIER"}}},
				{ID: "abort", Enabled: true, Filter: admin.LifecycleFilter{Tags: map[string]string{"k": "v"}}, AbortIncompleteUpload: &admin.LifecycleAbortUpload{DaysAfterInitiation: intp(1)}},
			},
			want: []admin.RuleProblem{
				{Rule: 1, ID: "cold", Code: "InvalidStorageClass", Message: `The storage class "GLACIER" of the transition names no remote tier`},
				{Rule: 2, ID: "abort", Code: "InvalidRequest", Message: "AbortIncompleteMultipartUpload cannot be specified with Tags or object size filters"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newLifecycleEnv(t, nil)
			e.buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
			_, err := e.mgr.Set(ctx, "bkt", admin.LifecycleConfig{Rules: tt.rules})
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
			problems, ok := admin.RuleProblemsOf(err)
			require.True(t, ok)
			assert.Equal(t, tt.want, problems)
		})
	}
}

func TestLifecycleSetEmptyDeletesAndMissingBucket(t *testing.T) {
	ctx := context.Background()
	e := newLifecycleEnv(t, nil)
	e.buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	e.buckets.EXPECT().DeleteLifecycle(mock.Anything, "bkt").Return(nil)
	got, err := e.mgr.Set(ctx, "bkt", admin.LifecycleConfig{})
	require.NoError(t, err)
	assert.Empty(t, got.Rules)
	assert.NotNil(t, got.Rules)

	e.buckets.EXPECT().GetVersioning(mock.Anything, "nope").Return(domain.VersioningOff, domain.ErrBucketNotFound)
	_, err = e.mgr.Set(ctx, "nope", admin.LifecycleConfig{Rules: []admin.LifecycleRule{{Enabled: true, Expiration: &admin.LifecycleExpiration{Days: intp(0)}}}})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound, "a missing bucket is reported before the rules")
}

func TestLifecycleGetWithoutConfiguration(t *testing.T) {
	ctx := context.Background()
	e := newLifecycleEnv(t, nil)
	e.buckets.EXPECT().GetLifecycle(mock.Anything, "bkt").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchLifecycleConfiguration", "none"))
	got, err := e.mgr.Get(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, &admin.LifecycleConfig{Rules: []admin.LifecycleRule{}}, got)

	e.buckets.EXPECT().GetLifecycle(mock.Anything, "nope").Return(nil, domain.ErrBucketNotFound)
	_, err = e.mgr.Get(ctx, "nope")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}

func TestLifecycleApply(t *testing.T) {
	ctx := context.Background()
	lifecycle := ilmmock.NewService(t)
	e := newLifecycleEnv(t, lifecycle)
	e.buckets.EXPECT().GetLifecycle(mock.Anything, "bkt").Return([]byte("<LifecycleConfiguration/>"), nil)
	lifecycle.EXPECT().ApplyNow(mock.Anything, "bkt").Return(ilm.ApplyResult{Buckets: 1, Versions: 3, Deleted: 2, Locked: 1}, nil)
	res, err := e.mgr.Apply(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, ilm.ApplyResult{Buckets: 1, Versions: 3, Deleted: 2, Locked: 1}, res)

	// Without configuration nothing is applied; "" never means every
	// bucket.
	e.buckets.EXPECT().GetLifecycle(mock.Anything, "plain").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchLifecycleConfiguration", "none"))
	res, err = e.mgr.Apply(ctx, "plain")
	require.NoError(t, err)
	assert.Zero(t, res)
	_, err = e.mgr.Apply(ctx, "")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
