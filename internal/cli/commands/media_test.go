package commands_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The scenarios of nexoctl media (the video rules of the buckets and the
// state of the media worker) and of the media sidecars hidden by the
// listings, against the in-memory store.

func TestScenarioMedia(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("videos")
	h.write("media.json", `{"rules": [{"id": "courses", "prefix": "courses/", "renditions": ["480p", "720p"], "poster": true}]}`)
	h.write("bad.json", `{"rules": [{"id": "x", "renditions": ["4k"]}]}`)
	h.write("empty.json", `{"rules": []}`)
	h.write("typo.json", `{"rules": [{"id": "x", "poster": true, "rendition": ["720p"]}]}`)
	s := h.session()
	s.run("media", "get", "mem/videos")
	s.run("media", "set", "mem/videos", "media.json")
	s.run("media", "get", "mem/videos")
	s.run("media", "get", "--json", "mem/videos")
	s.run("media", "rule", "add", "mem/videos", "--id", "clips", "--suffix", ".MP4,.webm", "--thumbnails", "5s")
	s.run("media", "rule", "add", "mem/videos", "--id", "courses", "--prefix", "courses/", "--rendition", "1080p", "--rendition", "360p", "--json")
	s.run("media", "get", "mem/videos")
	s.run("media", "rule", "rm", "mem/videos", "clips")
	s.run("media", "reprocess", "mem/videos/courses/intro.mp4")
	s.run("media", "reprocess", "--json", "--version-id", "v2", "mem/videos/courses/intro.mp4")
	s.run("media", "status", "mem/videos/courses/intro.mp4")
	updated := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	h.admin.SetMediaStatus("videos", "courses/intro.mp4", "", nexoadmin.MediaStatus{
		State: "done", Current: true, VersionID: "v2", Rule: "courses", Duration: 754.25, Width: 1920, Height: 1080,
		Renditions: []nexoadmin.MediaRenditionOutput{{Name: "1080p", Width: 1920, Height: 1080, Bandwidth: 5_128_000}, {Name: "360p", Width: 640, Height: 360, Bandwidth: 896_000}},
		Poster:     true, Thumbnails: &nexoadmin.MediaThumbnailsOutput{Interval: 10, Width: 160, Height: 90, Columns: 10, Count: 76}, Updated: &updated,
	})
	s.run("media", "status", "mem/videos/courses/intro.mp4")
	s.run("media", "status", "--json", "mem/videos/courses/intro.mp4")
	h.admin.SetMediaStatus("videos", "courses/intro.mp4", "v1", nexoadmin.MediaStatus{
		State: "failed", VersionID: "v1", Rule: "courses", Step: "renditions", Error: "ffmpeg: exit status 1", Updated: &updated,
	})
	s.run("media", "status", "--version-id", "v1", "mem/videos/courses/intro.mp4")
	h.admin.SetMediaStatus("videos", "old.mov", "", nexoadmin.MediaStatus{State: "done", VersionID: "null", Rule: "courses", Poster: true})
	s.run("media", "status", "mem/videos/old.mov")
	// Usage errors and failures.
	s.run("media", "set", "mem/videos", "bad.json")
	s.run("media", "set", "mem/videos", "empty.json")
	s.run("media", "set", "mem/videos", "typo.json")
	s.run("media", "set", "mem/videos/courses", "media.json")
	s.run("media", "rule", "add", "mem/videos", "--poster")
	s.run("media", "rule", "add", "mem/videos", "--id", "none")
	s.run("media", "rule", "add", "mem/videos", "--id", "fast", "--thumbnails", "100ms")
	s.run("media", "rule", "add", "mem/videos", "--id", "side", "--prefix", ".nexo-media/", "--poster")
	s.run("media", "rule", "rm", "mem/videos", "ghost")
	s.run("media", "reprocess", "mem/videos/notes.txt")
	s.run("media", "reprocess", "mem/videos")
	s.run("media", "status", "mem/videos/courses/")
	s.run("media", "rm", "mem/videos")
	s.run("media", "get", "mem/videos")
	golden(t, "scenario/media", s.b.String())

	assert.Equal(t, []clitest.MediaJob{
		{Bucket: "videos", Key: "courses/intro.mp4"}, {Bucket: "videos", Key: "courses/intro.mp4", VersionID: "v2"},
	}, h.admin.MediaReprocessed())
	assert.Empty(t, h.admin.MediaRules("videos").Rules)
}

func TestMediaRuleRemoveLastDeletes(t *testing.T) {
	h := newHarness(t)
	h.mustRun("media", "rule", "add", "mem/videos", "--id", "only", "--poster")
	require.Len(t, h.admin.MediaRules("videos").Rules, 1)
	h.mustRun("media", "rule", "rm", "mem/videos", "only")
	assert.Empty(t, h.admin.MediaRules("videos").Rules)
	assert.Equal(t, 1, h.admin.Calls("DeleteBucketMedia"))
}

func TestMediaAdminFailure(t *testing.T) {
	h := newHarness(t)
	h.admin.FailOn("GetBucketMedia", errors.New("boom"))
	r := h.run("media", "rule", "add", "mem/videos", "--id", "x", "--poster")
	assert.NotEqual(t, 0, r.code)
	assert.Contains(t, r.stderr, "boom")
	assert.Equal(t, 0, h.admin.Calls("SetBucketMedia"), "no write after a failed read")
}

func TestScenarioMediaSidecarsHidden(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("videos")
	h.mem.PutObject("videos", "intro.mp4", "video", clitest.ObjectSpec{})
	h.mem.PutObject("videos", "docs/readme.txt", "text", clitest.ObjectSpec{})
	h.mem.PutObject("videos", ".nexo-media/intro.mp4/null/status.json", "{}", clitest.ObjectSpec{})
	h.mem.PutObject("videos", ".nexo-media/intro.mp4/null/poster.jpg", "jpeg", clitest.ObjectSpec{})
	s := h.session()
	s.run("ls", "mem/videos")
	s.run("ls", "--media", "mem/videos")
	s.run("ls", "-r", "mem/videos")
	s.run("ls", "-r", "--media", "mem/videos")
	s.run("ls", "mem/videos/.nexo-media")
	s.run("ls", "-r", "mem/videos/.nexo-media/intro.mp4/")
	s.run("find", "mem/videos")
	s.run("find", "--media", "--name", "*.jpg", "mem/videos")
	s.run("find", "mem/videos/.nexo-media/")
	s.run("tree", "mem/videos")
	s.run("tree", "--media", "mem/videos")
	golden(t, "scenario/media_sidecars", s.b.String())
}
