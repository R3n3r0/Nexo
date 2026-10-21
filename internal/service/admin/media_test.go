package admin_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
)

func newMediaManager(t *testing.T, jobs admin.MediaJobs) (admin.MediaManager, *bucketmock.Service) {
	t.Helper()
	buckets := bucketmock.NewService(t)
	mgr, err := admin.NewMediaManager(admin.MediaManagerDeps{Buckets: buckets, Jobs: jobs})
	require.NoError(t, err)
	return mgr, buckets
}

func TestNewMediaManagerRequiresDeps(t *testing.T) {
	_, err := admin.NewMediaManager(admin.MediaManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestMediaGetWithoutConfiguration(t *testing.T) {
	mgr, buckets := newMediaManager(t, nil)
	buckets.EXPECT().GetMedia(mock.Anything, "bkt").Return(domain.MediaConfig{}, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchMediaConfiguration", "none"))
	cfg, err := mgr.Get(context.Background(), "bkt")
	require.NoError(t, err)
	assert.NotNil(t, cfg.Rules)
	assert.Empty(t, cfg.Rules)
}

func TestMediaSetValidatesEachRule(t *testing.T) {
	mgr, buckets := newMediaManager(t, nil)
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	_, err := mgr.Set(context.Background(), "bkt", domain.MediaConfig{Rules: []domain.MediaRule{
		{ID: "ok", Poster: true},
		{ID: "bad rendition", Renditions: []string{"999p"}},
		{ID: "ok", Poster: true},
		{ID: "empty"},
	}})
	var cerr *admin.ConfigError
	require.ErrorAs(t, err, &cerr)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.Equal(t, "media", cerr.Kind)
	require.Len(t, cerr.Problems, 3)
	assert.Equal(t, 2, cerr.Problems[0].Rule)
	assert.Contains(t, cerr.Problems[0].Message, "id must be")
	assert.Equal(t, admin.RuleProblem{Rule: 3, ID: "ok", Code: "InvalidArgument", Message: "duplicate rule id (rule 1)"}, cerr.Problems[1])
	assert.Equal(t, 4, cerr.Problems[2].Rule)
	assert.Contains(t, cerr.Problems[2].Message, "nothing to produce")
}

// TestMediaSetImageRules: the problems of the image rules name their
// rule once, in the problem, not again in the message.
func TestMediaSetImageRules(t *testing.T) {
	mgr, buckets := newMediaManager(t, nil)
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	_, err := mgr.Set(context.Background(), "bkt", domain.MediaConfig{Rules: []domain.MediaRule{
		{ID: "gif", Images: &domain.MediaImages{Formats: []string{"gif"}}},
		{ID: "tiny", Images: &domain.MediaImages{Widths: []int{8}}},
		{ID: "mixed", Poster: true, Images: &domain.MediaImages{}},
	}})
	var cerr *admin.ConfigError
	require.ErrorAs(t, err, &cerr)
	require.Len(t, cerr.Problems, 3)
	assert.Equal(t, admin.RuleProblem{Rule: 1, ID: "gif", Code: "InvalidArgument",
		Message: `unknown image format "gif" (one of avif, webp, jpeg, png)`}, cerr.Problems[0])
	assert.Equal(t, "the image widths must be from 16 to 8192 pixels, not 8", cerr.Problems[1].Message)
	assert.Equal(t, "an image rule has no renditions, poster, thumbnails or subtitles", cerr.Problems[2].Message)
	assert.Contains(t, err.Error(), `rule 1 (gif): unknown image format "gif"`)

	stored := domain.MediaConfig{Rules: []domain.MediaRule{{ID: "pics", Suffixes: domain.DefaultImageSuffixes(),
		Images: &domain.MediaImages{Formats: []string{"webp", "jpeg"}, Widths: []int{320, 640}}}}}
	buckets.EXPECT().SetMedia(mock.Anything, "bkt", stored).Return(nil)
	buckets.EXPECT().GetMedia(mock.Anything, "bkt").Return(stored, nil)
	got, err := mgr.Set(context.Background(), "bkt", domain.MediaConfig{Rules: []domain.MediaRule{
		{ID: "pics", Images: &domain.MediaImages{Formats: []string{"jpeg", "webp"}, Widths: []int{640, 320}}},
	}})
	require.NoError(t, err)
	assert.Equal(t, &stored, got, "normalized")
}

func TestMediaSetStoresAndReturnsNormalized(t *testing.T) {
	mgr, buckets := newMediaManager(t, nil)
	in := domain.MediaConfig{Rules: []domain.MediaRule{{ID: "v", Renditions: []string{"480p", "720p"}}}}
	stored := domain.MediaConfig{Rules: []domain.MediaRule{{ID: "v", Suffixes: []string{".m4v", ".mov", ".mp4"}, Renditions: []string{"720p", "480p"}}}}
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	buckets.EXPECT().SetMedia(mock.Anything, "bkt", stored).Return(nil)
	buckets.EXPECT().GetMedia(mock.Anything, "bkt").Return(stored, nil)
	got, err := mgr.Set(context.Background(), "bkt", in)
	require.NoError(t, err)
	assert.Equal(t, &stored, got)
}

func TestMediaSetUnknownBucket(t *testing.T) {
	mgr, buckets := newMediaManager(t, nil)
	buckets.EXPECT().GetVersioning(mock.Anything, "nope").Return(domain.VersioningOff, domain.ErrBucketNotFound)
	_, err := mgr.Set(context.Background(), "nope", domain.MediaConfig{})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}

func TestMediaDelete(t *testing.T) {
	mgr, buckets := newMediaManager(t, nil)
	buckets.EXPECT().DeleteMedia(mock.Anything, "bkt").Return(nil)
	require.NoError(t, mgr.Delete(context.Background(), "bkt"))
}

func TestMediaReprocess(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newMediaManager(t, nil)
	assert.ErrorIs(t, mgr.Reprocess(ctx, "bkt", "v.mp4", ""), domain.ErrNotImplemented)

	jobs := adminmock.NewMediaJobs(t)
	mgr, _ = newMediaManager(t, jobs)
	assert.ErrorIs(t, mgr.Reprocess(ctx, "bkt", "", ""), domain.ErrInvalidArgument)
	jobs.EXPECT().Reprocess(mock.Anything, "bkt", "v.mp4", "v1").Return(nil)
	require.NoError(t, mgr.Reprocess(ctx, "bkt", "v.mp4", "v1"))
}
