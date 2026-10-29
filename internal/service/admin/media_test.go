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
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/media"
	mediamock "github.com/r3n3r0/nexo/internal/service/media/mock"
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

// TestMediaTrace: the votes of a leaked copy are grouped by segment, then
// by bit position (segment n is bit n mod 64), and read as '0' (variant
// A), '1' (variant B) or '?'; the bits name the trace records.
//
// covers: S-OBJ-19
func TestMediaTrace(t *testing.T) {
	ctx := context.Background()
	medias := mediamock.NewService(t)
	mgr, err := admin.NewMediaManager(admin.MediaManagerDeps{Buckets: bucketmock.NewService(t), Media: medias})
	require.NoError(t, err)

	_, err = mgr.Trace(ctx, admin.TraceInput{Bucket: "bkt", Key: "a.mp4"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = mgr.Trace(ctx, admin.TraceInput{Bucket: "bkt", Key: "a.mp4", Bits: "01", Votes: []domain.MediaTraceVote{{T: 1}}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	match := []domain.MediaTraceMatch{{MediaTrace: domain.MediaTrace{SessionID: "ab", Subject: "s"}, Agree: 3, Compared: 3}}
	medias.EXPECT().Trace(mock.Anything, "bkt", "01", 10).Return(match, nil).Once()
	res, err := mgr.Trace(ctx, admin.TraceInput{Bucket: "bkt", Key: "a.mp4", Bits: "01"})
	require.NoError(t, err)
	assert.Equal(t, &admin.TraceResult{Bits: "01", Matches: match}, res)

	src := media.Source{Bucket: "bkt", Key: "a.mp4"}
	st := &media.OutputStatus{Current: true, MediaStatus: domain.MediaStatus{Renditions: []domain.MediaRenditionOutput{
		{Name: "1080p"}, {Name: "720p", Forensic: true},
	}}}
	medias.EXPECT().Status(mock.Anything, src).Return(st, nil)
	withRendition := src
	withRendition.Rendition = "720p"
	medias.EXPECT().Describe(mock.Anything, withRendition).Return(&media.Presentation{
		Segments: []time.Duration{6 * time.Second, 6 * time.Second, 6 * time.Second, 6 * time.Second},
	}, nil)
	// Segment 0: A; segment 1: B (the boundary picture at 6s is of
	// segment 1); segment 2: undecided; segment 3: A, but by one vote
	// out of the video (24s).
	votes := []domain.MediaTraceVote{
		{T: 0.5, Score: 0.9}, {T: 3, Score: 0.6},
		{T: 6, Score: -0.8}, {T: 9, Score: -0.7},
		{T: 12.5, Score: 0.3}, {T: 15, Score: -0.3},
		{T: 18.2, Score: 0.7}, {T: 24, Score: -5},
	}
	medias.EXPECT().Trace(mock.Anything, "bkt", "01?0", 5).Return(nil, nil).Once()
	res, err = mgr.Trace(ctx, admin.TraceInput{Bucket: "bkt", Key: "a.mp4", Votes: votes, Limit: 5})
	require.NoError(t, err)
	assert.Equal(t, "01?0", res.Bits)

	// Votes telling nothing.
	_, err = mgr.Trace(ctx, admin.TraceInput{Bucket: "bkt", Key: "a.mp4", Votes: []domain.MediaTraceVote{{T: 1, Score: 0.1}}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestMediaTraceWithoutForensicRenditions: votes are read against the
// forensic renditions only.
func TestMediaTraceWithoutForensicRenditions(t *testing.T) {
	medias := mediamock.NewService(t)
	mgr, err := admin.NewMediaManager(admin.MediaManagerDeps{Buckets: bucketmock.NewService(t), Media: medias})
	require.NoError(t, err)
	medias.EXPECT().Status(mock.Anything, mock.Anything).Return(&media.OutputStatus{Current: true}, nil)
	_, err = mgr.Trace(context.Background(), admin.TraceInput{Bucket: "bkt", Key: "a.mp4", Votes: []domain.MediaTraceVote{{T: 1, Score: 1}}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.Contains(t, err.Error(), "no rendition with the forensic watermark")

	mgr, err = admin.NewMediaManager(admin.MediaManagerDeps{Buckets: bucketmock.NewService(t)})
	require.NoError(t, err)
	_, err = mgr.Trace(context.Background(), admin.TraceInput{Bucket: "bkt", Key: "a.mp4", Bits: "1"})
	require.ErrorIs(t, err, domain.ErrNotImplemented)
}
