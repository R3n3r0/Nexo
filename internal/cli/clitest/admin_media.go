package clitest

import (
	"context"
	"net/http"
	"slices"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The video rules of the buckets and the state of the media worker
// (nexoctl media): the rules are validated and normalized as the server
// does; the states are set by the tests (SetMediaStatus) and the
// reprocessing requests recorded (MediaReprocessed).

// memMediaState is the media state of MemAdmin.
type memMediaState struct {
	// rules are the video rules, by bucket.
	rules map[string]nexoadmin.MediaConfiguration
	// status are the processing states, by bucket, key and version.
	status map[memMediaKey]nexoadmin.MediaStatus
	// reprocessed are the jobs queued by ReprocessMedia.
	reprocessed []MediaJob
}

// memMediaKey names a version of a video ("" is the latest).
type memMediaKey struct{ bucket, key, versionID string }

// MediaJob is a reprocessing request received by MemAdmin.
type MediaJob struct {
	Bucket, Key, VersionID string
}

// mediaLocked returns the state, initialized on first use (lock held).
func (s *memSession) mediaLocked() *memMediaState {
	return s.m.mediaLocked()
}

func (m *MemAdmin) mediaLocked() *memMediaState {
	if m.media.rules == nil {
		m.media.rules, m.media.status = map[string]nexoadmin.MediaConfiguration{}, map[memMediaKey]nexoadmin.MediaStatus{}
	}
	return &m.media
}

// SetMediaStatus sets the processing state of a version of a video
// returned by GetMediaStatus (versionID "" for the latest version).
func (m *MemAdmin) SetMediaStatus(bucket, key, versionID string, st nexoadmin.MediaStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mediaLocked().status[memMediaKey{bucket, key, versionID}] = st
}

// MediaRules returns the video rules of a bucket.
func (m *MemAdmin) MediaRules(bucket string) nexoadmin.MediaConfiguration {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.mediaLocked().rules[bucket]
	cfg.Rules = slices.Clone(cfg.Rules)
	return cfg
}

// MediaReprocessed returns the reprocessing requests received.
func (m *MemAdmin) MediaReprocessed() []MediaJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.mediaLocked().reprocessed)
}

// expectMedia registers the media methods of the store on c.
func (s *memSession) expectMedia(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().GetBucketMedia(x, x).RunAndReturn(s.getBucketMedia).Maybe()
	c.EXPECT().SetBucketMedia(x, x, x).RunAndReturn(s.setBucketMedia).Maybe()
	c.EXPECT().DeleteBucketMedia(x, x).RunAndReturn(s.deleteBucketMedia).Maybe()
	c.EXPECT().ReprocessMedia(x, x, x, x).RunAndReturn(s.reprocessMedia).Maybe()
	c.EXPECT().GetMediaStatus(x, x, x, x).RunAndReturn(s.getMediaStatus).Maybe()
}

func (s *memSession) getBucketMedia(_ context.Context, bucket string) (*nexoadmin.MediaConfiguration, error) {
	if err := s.begin("GetBucketMedia"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:GetBucketMedia"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	cfg := s.mediaLocked().rules[bucket]
	out := cli.MediaFromDomain(cli.MediaDomain(cfg))
	return &out, nil
}

func (s *memSession) setBucketMedia(_ context.Context, bucket string, cfg nexoadmin.MediaConfiguration) (*nexoadmin.MediaConfiguration, error) {
	if err := s.begin("SetBucketMedia"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:SetBucketMedia"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d := cli.MediaDomain(cfg)
	if err := d.Validate(); err != nil {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidConfiguration, err.Error())
	}
	stored := cli.MediaFromDomain(d)
	if len(stored.Rules) == 0 {
		delete(s.mediaLocked().rules, bucket)
	} else {
		s.mediaLocked().rules[bucket] = stored
	}
	out := cli.MediaFromDomain(d)
	return &out, nil
}

func (s *memSession) deleteBucketMedia(_ context.Context, bucket string) error {
	if err := s.begin("DeleteBucketMedia"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:SetBucketMedia"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	delete(s.mediaLocked().rules, bucket)
	return nil
}

func (s *memSession) reprocessMedia(_ context.Context, bucket, key, versionID string) error {
	if err := s.begin("ReprocessMedia"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:SetBucketMedia"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	if key == "" {
		return invalid("key is required")
	}
	if _, ok := cli.MediaDomain(s.mediaLocked().rules[bucket]).Match(key); !ok {
		return invalid("no media rule of bucket %s selects %s", bucket, key)
	}
	s.mediaLocked().reprocessed = append(s.mediaLocked().reprocessed, MediaJob{Bucket: bucket, Key: key, VersionID: versionID})
	return nil
}

func (s *memSession) getMediaStatus(_ context.Context, bucket, key, versionID string) (*nexoadmin.MediaStatus, error) {
	if err := s.begin("GetMediaStatus"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("s3:GetObject"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st, ok := s.mediaLocked().status[memMediaKey{bucket, key, versionID}]
	if !ok {
		st = nexoadmin.MediaStatus{State: "none"}
	}
	st.Renditions = slices.Clone(st.Renditions)
	st.Images = slices.Clone(st.Images)
	if st.Thumbnails != nil {
		t := *st.Thumbnails
		st.Thumbnails = &t
	}
	return &st, nil
}
