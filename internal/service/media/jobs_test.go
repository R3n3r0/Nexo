package media_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
	"github.com/r3n3r0/nexo/internal/service/media"
)

var jobTime = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

type jobsFixture struct {
	jobs      media.Jobs
	events    *eventmock.Service
	buckets   *bucketmock.MetadataStore
	published []event.Event
}

func newJobs(t *testing.T) *jobsFixture {
	t.Helper()
	f := &jobsFixture{events: eventmock.NewService(t), buckets: bucketmock.NewMetadataStore(t)}
	var err error
	f.jobs, err = media.NewJobs(media.JobsDeps{Events: f.events, Buckets: f.buckets, Clock: clock.NewFake(jobTime)})
	require.NoError(t, err)
	f.events.EXPECT().Publish(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, e event.Event) error {
		f.published = append(f.published, e)
		return nil
	}).Maybe()
	rules := domain.MediaConfig{Rules: []domain.MediaRule{{ID: "v", Prefix: "videos/", Suffixes: []string{".mp4"}, Poster: true}}}
	f.buckets.EXPECT().Load(tmock.Anything, "bkt").Return(&bucket.Metadata{Name: "bkt", Media: &rules}, nil).Maybe()
	f.buckets.EXPECT().Load(tmock.Anything, "plain").Return(&bucket.Metadata{Name: "plain"}, nil).Maybe()
	return f
}

func objectEvent(name event.Name, bkt, key, version string) event.Event {
	e := event.NewEvent(name, "", jobTime)
	e.S3.Bucket.Name = bkt
	e.S3.Object = event.Object{Key: key, VersionID: version, ETag: "e1", Size: 42}
	return e
}

func TestNewJobsRequiresDeps(t *testing.T) {
	_, err := media.NewJobs(media.JobsDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestJobsOfObjectEvents(t *testing.T) {
	rule := domain.MediaRule{ID: "v", Prefix: "videos/", Suffixes: []string{".mp4"}, Poster: true}
	tests := []struct {
		name string
		e    event.Event
		want *domain.MediaJob
	}{
		{"created", objectEvent(event.ObjectCreatedPut, "bkt", "videos/a.mp4", "v1"), &domain.MediaJob{
			Action: domain.MediaJobProcess, Bucket: "bkt", Key: "videos/a.mp4", VersionID: "v1", ETag: "e1", Size: 42,
			Sidecar: ".nexo-media/videos/a.mp4/v1/", Rule: &rule, Time: jobTime}},
		{"multipart", objectEvent(event.ObjectCreatedCompleteMultipartUpload, "bkt", "videos/b.MP4", ""), &domain.MediaJob{
			Action: domain.MediaJobProcess, Bucket: "bkt", Key: "videos/b.MP4", ETag: "e1", Size: 42,
			Sidecar: ".nexo-media/videos/b.MP4/null/", Rule: &rule, Time: jobTime}},
		{"version removed", objectEvent(event.ObjectRemovedDelete, "bkt", "videos/a.mp4", "v1"), &domain.MediaJob{
			Action: domain.MediaJobDelete, Bucket: "bkt", Key: "videos/a.mp4", VersionID: "v1",
			Sidecar: ".nexo-media/videos/a.mp4/v1/", Time: jobTime}},
		{"expired", objectEvent(event.LifecycleExpirationDelete, "bkt", "videos/a.mp4", ""), &domain.MediaJob{
			Action: domain.MediaJobDelete, Bucket: "bkt", Key: "videos/a.mp4", Sidecar: ".nexo-media/videos/a.mp4/null/", Time: jobTime}},
		{"delete marker", objectEvent(event.ObjectRemovedDeleteMarkerCreated, "bkt", "videos/a.mp4", "dm"), nil},
		{"read", objectEvent(event.ObjectAccessedGet, "bkt", "videos/a.mp4", ""), nil},
		{"not selected", objectEvent(event.ObjectCreatedPut, "bkt", "docs/a.mp4", ""), nil},
		{"wrong suffix", objectEvent(event.ObjectCreatedPut, "bkt", "videos/a.mkv", ""), nil},
		{"sidecar", objectEvent(event.ObjectCreatedPut, "bkt", ".nexo-media/videos/a.mp4/null/poster.jpg", ""), nil},
		{"no rules", objectEvent(event.ObjectCreatedPut, "plain", "videos/a.mp4", ""), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newJobs(t)
			require.NoError(t, f.jobs.Publish(context.Background(), tt.e))
			require.NotEmpty(t, f.published)
			assert.Equal(t, tt.e, f.published[0], "the event itself first")
			if tt.want == nil {
				assert.Len(t, f.published, 1, "no job")
				return
			}
			require.Len(t, f.published, 2)
			job := f.published[1]
			assert.Equal(t, event.MediaJob, job.EventName)
			assert.Equal(t, tt.e.S3.Bucket.Name, job.S3.Bucket.Name)
			assert.Equal(t, tt.e.S3.Object.Key, job.S3.Object.Key)
			assert.Equal(t, tt.want, job.Media)
		})
	}
}

func TestJobsReprocess(t *testing.T) {
	ctx := context.Background()
	f := newJobs(t)
	err := f.jobs.Reprocess(ctx, "bkt", "docs/a.mp4", "")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "no rule selects the key")

	require.NoError(t, f.jobs.Reprocess(ctx, "bkt", "videos/a.mp4", ""))
	require.NoError(t, f.jobs.Reprocess(ctx, "bkt", "videos/a.mp4", "v7"))
	require.Len(t, f.published, 2)
	current := f.published[0].Media
	require.NotNil(t, current)
	assert.Equal(t, domain.MediaJobProcess, current.Action)
	assert.Empty(t, current.VersionID, "the worker resolves the current version")
	assert.Empty(t, current.Sidecar)
	require.NotNil(t, current.Rule)
	assert.Equal(t, "v", current.Rule.ID)
	version := f.published[1].Media
	assert.Equal(t, "v7", version.VersionID)
	assert.Equal(t, ".nexo-media/videos/a.mp4/v7/", version.Sidecar)
}
