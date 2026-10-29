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

// TestScenarioMediaImages: the image rules (mb --with-images, rule add
// --images) and the variants of an image in media status.
func TestScenarioMediaImages(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("photos")
	s := h.session()
	s.run("mb", "--with-images", "mem/pictures")
	s.run("mb", "--with-hls", "--with-images", "mem/library")
	s.run("mb", "--ignore-existing", "--with-images", "--json", "mem/library")
	s.run("media", "get", "mem/library")
	s.run("media", "rule", "add", "mem/photos", "--images")
	s.run("media", "rule", "add", "mem/photos", "--images", "--id", "thumbs", "--prefix", "gallery/", "--format", "JPG,webp", "--width", "640", "--width", "160", "--json")
	s.run("media", "get", "mem/photos")
	updated := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	h.admin.SetMediaStatus("photos", "gallery/beach.jpg", "", nexoadmin.MediaStatus{
		State: "done", Current: true, VersionID: "null", Rule: "thumbs", Width: 4000, Height: 3000, Updated: &updated,
		Images: []nexoadmin.MediaImageOutput{
			{Format: "webp", Width: 640, PixelWidth: 640, PixelHeight: 480, Size: 41_000, ContentType: "image/webp", URL: "/api/v1/buckets/photos/objects/image?format=webp&key=gallery%2Fbeach.jpg&w=640"},
			{Format: "webp", Width: 0, PixelWidth: 4000, PixelHeight: 3000, Size: 1_200_000, ContentType: "image/webp", URL: "/api/v1/buckets/photos/objects/image?format=webp&key=gallery%2Fbeach.jpg"},
			{Format: "webp", Width: 160, PixelWidth: 160, PixelHeight: 120, Size: 4_000, ContentType: "image/webp", URL: "/api/v1/buckets/photos/objects/image?format=webp&key=gallery%2Fbeach.jpg&w=160"},
			{Format: "jpeg", Width: 160, PixelWidth: 160, PixelHeight: 120, Size: 6_000, ContentType: "image/jpeg", URL: "/api/v1/buckets/photos/objects/image?format=jpeg&key=gallery%2Fbeach.jpg&w=160"},
			{Format: "jpeg", Width: 0, PixelWidth: 4000, PixelHeight: 3000, Size: 2_500_000, ContentType: "image/jpeg", URL: "/api/v1/buckets/photos/objects/image?format=jpeg&key=gallery%2Fbeach.jpg"},
		},
	})
	s.run("media", "status", "mem/photos/gallery/beach.jpg")
	s.run("media", "status", "--json", "mem/photos/gallery/beach.jpg")
	s.run("media", "status", "mem/photos/gallery/new.png")
	// Usage errors.
	s.run("media", "rule", "add", "mem/photos", "--images", "--format", "gif")
	s.run("media", "rule", "add", "mem/photos", "--images", "--width", "8")
	s.run("media", "rule", "add", "mem/photos", "--images", "--thumbnails", "10s")
	s.run("media", "rule", "add", "mem/photos", "--id", "x", "--format", "webp")
	s.run("media", "rule", "add", "mem/photos", "--images", "--hls")
	golden(t, "scenario/media_images", s.b.String())

	rules := h.admin.MediaRules("library").Rules
	require.Len(t, rules, 2)
	assert.Equal(t, "hls", rules[0].ID)
	assert.Equal(t, "images", rules[1].ID)
}

// TestScenarioMediaSubtitles: the subtitle rules (rule add --subtitles)
// and the tracks of a video (media subtitles).
func TestScenarioMediaSubtitles(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("videos")
	h.write("it.srt", "1\n00:00:01,000 --> 00:00:02,500\nCiao a tutti\n\n2\n00:00:03,000 --> 00:00:04,000\n<script>x</script>Benvenuti\n")
	h.write("bad.vtt", "WEBVTT\n\n00:02.000 --> 00:01.000\nx\n")
	updated := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	require.NoError(t, h.admin.SetSubtitles("videos", "talks/a.mp4", "", nexoadmin.SubtitleTrack{Lang: "en", Auto: true, Engine: "whisper-1", Updated: &updated},
		"WEBVTT\n\n00:00.000 --> 00:01.000\nHello\n"))
	require.NoError(t, h.admin.SetSubtitles("videos", "talks/a.mp4", "", nexoadmin.SubtitleTrack{Lang: "de", Auto: true, From: "en", Updated: &updated},
		"WEBVTT\n\n00:00.000 --> 00:01.000\nHallo\n"))
	h.admin.SetMediaStatus("videos", "talks/a.mp4", "", nexoadmin.MediaStatus{State: "done", Current: true, VersionID: "null", Rule: "talks",
		Language: "en", Subtitles: []string{"de", "en"}, Updated: &updated})
	s := h.session()
	s.run("media", "rule", "add", "mem/videos", "--id", "talks", "--prefix", "talks/", "--subtitles", "--translate", "de,PT-br")
	s.run("media", "rule", "add", "mem/videos", "--id", "talks", "--prefix", "talks/", "--subtitles=en", "--json")
	s.run("media", "status", "mem/videos/talks/a.mp4")
	s.run("media", "subtitles", "ls", "mem/videos/talks/a.mp4")
	s.run("media", "subtitles", "put", "mem/videos/talks/a.mp4", "IT", "it.srt")
	s.run("media", "subtitles", "ls", "--json", "mem/videos/talks/a.mp4")
	s.run("media", "subtitles", "get", "mem/videos/talks/a.mp4", "it")
	s.run("media", "subtitles", "get", "--format", "srt", "mem/videos/talks/a.mp4", "it")
	s.run("media", "subtitles", "rm", "mem/videos/talks/a.mp4", "de")
	s.run("media", "subtitles", "ls", "mem/videos/talks/a.mp4")
	// Usage errors and failures.
	s.run("media", "rule", "add", "mem/videos", "--id", "x", "--subtitles=klingon!")
	s.run("media", "rule", "add", "mem/videos", "--images", "--subtitles")
	s.run("media", "subtitles", "put", "mem/videos/talks/a.mp4", "it", "bad.vtt")
	s.run("media", "subtitles", "put", "mem/videos/talks/a.mp4", "??", "it.srt")
	s.run("media", "subtitles", "get", "--format", "ass", "mem/videos/talks/a.mp4", "it")
	s.run("media", "subtitles", "get", "mem/videos/talks/a.mp4", "fr")
	s.run("media", "subtitles", "rm", "mem/videos/talks/a.mp4", "fr")
	s.run("media", "subtitles", "ls", "mem/videos/talks/b.mp4")
	golden(t, "scenario/media_subtitles", s.b.String())
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

// TestScenarioMediaProtection: the download protection of the videos
// (media protection get/set), the playback tokens (media token) and their
// revocation (media revoke).
//
// covers: S-OBJ-17
func TestScenarioMediaProtection(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("courses")
	s := h.session()
	s.run("media", "protection", "get", "mem/courses")
	s.run("media", "protection", "set", "mem/courses", "--enable", "--prefix", "lessons/", "--max-sessions", "1",
		"--origin", "https://Learn.Example.com", "--token-ttl", "4h")
	s.run("media", "protection", "get", "mem/courses")
	s.run("media", "protection", "set", "mem/courses", "--max-sessions", "unlimited", "--watermark=false", "--share-links")
	s.run("media", "protection", "get", "--json", "mem/courses")
	s.run("media", "token", "mem/courses/lessons/intro.mp4", "--subject", "student-1842", "--watermark", "mario.rossi@example.com")
	s.run("media", "token", "--json", "--ttl", "30m", "mem/courses/lessons/intro.mp4", "--subject", "student-7")
	s.run("media", "revoke", "mem/courses", "--session", "00000000000000000000000000000001")
	s.run("media", "revoke", "mem/courses", "--subject", "student-7")
	// Usage errors.
	s.run("media", "protection", "set", "mem/courses", "--enable", "--disable")
	s.run("media", "protection", "set", "mem/courses", "--max-sessions", "0")
	s.run("media", "protection", "set", "mem/courses", "--origin", "https://a.example/path")
	s.run("media", "protection", "set", "mem/courses", "--pace", "50")
	s.run("media", "token", "mem/courses/lessons/intro.mp4")
	s.run("media", "token", "mem/courses", "--subject", "s")
	s.run("media", "revoke", "mem/courses")
	s.run("media", "protection", "set", "mem/courses", "--disable")
	s.run("media", "protection", "get", "mem/courses")
	golden(t, "scenario/media_protection", s.b.String())

	tokens := h.admin.PlaybackTokens()
	require.Len(t, tokens, 2)
	assert.Equal(t, nexoadmin.PlaybackTokenRequest{Subject: "student-7", TTLSeconds: 1800}, tokens[1].Request)
	assert.Equal(t, []clitest.PlaybackRevocation{
		{Bucket: "courses", Session: "00000000000000000000000000000001"}, {Bucket: "courses", Subject: "student-7"},
	}, h.admin.PlaybackRevocations())
}
