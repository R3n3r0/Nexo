package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
	"github.com/r3n3r0/nexo/internal/mediaworker/worker"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// TestConsoleVideoStreaming: the console serves an MP4 video as HLS (the
// master playlist, the media playlist, the init segment and the media
// segments, linked by relative URIs); the playlists are pinned to the
// ETag, so the URIs of an overwritten video answer 412, and a user without
// s3:GetObject cannot stream it.
func TestConsoleVideoStreaming(t *testing.T) {
	e := newEnv(t)
	video, err := os.ReadFile(filepath.Join("..", "..", "internal", "service", "media", "testdata", "h264_aac.mp4"))
	require.NoError(t, err)
	e.mkBucket("videos")
	e.put("videos", "clips/intro.mp4", video)
	root := e.consoleLogin(e.rootCreds())

	get := func(c *console, u *url.URL) response {
		t.Helper()
		return doWith(t, c.client, newRequest(t, http.MethodGet, u.String(), nil))
	}
	master := mustParse(t, e.srv.ConsoleURL+"/api/v1/buckets/videos/objects/hls/master.m3u8?key=clips%2Fintro.mp4")
	media := playHLS(t, func(u *url.URL) response { return get(root, u) }, master)

	// A user without s3:GetObject on the bucket.
	frank := e.consoleLogin(e.createUser("frank", "frank-secret-1", "diagnostics"))
	assert.Equal(t, http.StatusForbidden, get(frank, master).StatusCode)

	// Not an MP4 video.
	e.put("videos", "notes.txt", []byte("not a video"))
	notVideo := mustParse(t, e.srv.ConsoleURL+"/api/v1/buckets/videos/objects/hls/master.m3u8?key=notes.txt")
	assert.Equal(t, http.StatusBadRequest, get(root, notVideo).StatusCode)

	// The overwritten video: the pinned URIs fail, a new master works.
	e.put("videos", "clips/intro.mp4", append(video[:len(video):len(video)], 0))
	assert.Equal(t, http.StatusPreconditionFailed, get(root, media).StatusCode)
	assert.Equal(t, http.StatusOK, get(root, master).StatusCode)
}

// TestVideoStreamShare: a stream share of a video is a presigned master
// playlist on the S3 listener whose playlists are signed by the share, so
// a plain HTTP client (a player) plays it without credentials; revoking
// the share revokes the whole stream, and the signature never covers
// another object.
func TestVideoStreamShare(t *testing.T) {
	e := newEnv(t)
	video, err := os.ReadFile(filepath.Join("..", "..", "internal", "service", "media", "testdata", "hevc_aac.mp4"))
	require.NoError(t, err)
	e.mkBucket("films")
	e.put("films", "trailer.mp4", video)
	e.put("films", "other.mp4", video)
	root := e.consoleLogin(e.rootCreds())

	resp := root.do(http.MethodPost, "/buckets/films/objects/share", `{"key":"trailer.mp4","stream":true,"expiresIn":3600}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	var sh struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		Stream bool   `json:"stream"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &sh))
	assert.True(t, sh.Stream)
	master := mustParse(t, sh.URL)
	assert.Equal(t, "/nexo/media/v1/films/master.m3u8", master.Path)
	assert.True(t, strings.HasPrefix(sh.URL, e.srv.S3URL+"/"), "the link points to the S3 API: %s", sh.URL)
	anonymous := func(u *url.URL) response { return do(t, newRequest(t, http.MethodGet, u.String(), nil)) }
	media := playHLS(t, anonymous, master)

	// Unsigned, or signed for another object, the stream is denied.
	unsigned := *master
	unsigned.RawQuery = "key=trailer.mp4"
	assert.Equal(t, http.StatusForbidden, anonymous(&unsigned).StatusCode)
	other := *master
	q := other.Query()
	q.Set("key", "other.mp4")
	other.RawQuery = q.Encode()
	assert.Equal(t, http.StatusForbidden, anonymous(&other).StatusCode, "the signature covers the query")

	// The stream options are checked.
	resp = root.do(http.MethodPost, "/buckets/films/objects/share", `{"key":"films/","prefix":true,"stream":true}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", resp.Body)

	// Revoked, the stream stops: the playlists and the segments.
	resp = root.do(http.MethodDelete, "/shares/"+sh.ID, "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, http.StatusForbidden, anonymous(master).StatusCode)
	assert.Equal(t, http.StatusForbidden, anonymous(media).StatusCode)
}

// TestImageVariants: the variants of an image the media worker wrote in
// the sidecar directory (written here as the worker would) are served by
// the console and, through an image share, by the S3 listener: the format
// the client accepts, the width it asks for, conditional requests, and
// only while they derive from the current content of the image.
func TestImageVariants(t *testing.T) {
	e := newEnv(t)
	root := e.consoleLogin(e.rootCreds())
	resp := root.do(http.MethodPost, "/buckets", `{"name":"pics","images":true}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	resp = root.do(http.MethodGet, "/buckets/pics/media", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), `"id":"images"`)

	etag := e.put("pics", "summer/beach.jpg", []byte("a photo"))
	dir := domain.MediaSidecarDir("summer/beach.jpg", "")
	var variants []domain.MediaImageOutput
	for _, f := range []string{"avif", "webp", "jpeg"} {
		for _, w := range []int{320, 0} {
			name := domain.MediaImageFile(w, f)
			e.put("pics", dir+name, []byte(name))
			pw := 1000
			if w > 0 {
				pw = w
			}
			variants = append(variants, domain.MediaImageOutput{Format: f, Width: w, PixelWidth: pw, PixelHeight: pw * 3 / 4, Size: int64(len(name))})
		}
	}
	status, err := json.Marshal(domain.MediaStatus{State: domain.MediaStateDone, Rule: "images", ETag: etag, Width: 1000, Height: 750,
		Images: variants, Updated: time.Now()})
	require.NoError(t, err)
	e.put("pics", dir+domain.MediaStatusFile, status)

	get := func(c *console, path string, hdr http.Header) response {
		t.Helper()
		return c.doRaw(http.MethodGet, path, nil, hdr)
	}
	const image = "/buckets/pics/objects/image?key=summer%2Fbeach.jpg"
	for _, tc := range []struct{ query, accept, body, contentType string }{
		{"", "image/avif,image/webp,*/*", "images/full.avif", "image/avif"},
		{"", "image/webp,*/*", "images/full.webp", "image/webp"},
		{"&w=200", "*/*", "images/320.jpg", "image/jpeg"},
		{"&w=600", "image/webp", "images/full.webp", "image/webp"},
		{"&format=jpeg&w=320", "image/avif", "images/320.jpg", "image/jpeg"},
	} {
		resp := get(root, image+tc.query, http.Header{"Accept": {tc.accept}})
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s %s: %s", tc.query, tc.accept, resp.Body)
		assert.Equal(t, tc.body, string(resp.Body), "%s %s", tc.query, tc.accept)
		assert.Equal(t, tc.contentType, resp.Header.Get("Content-Type"))
		if tc.accept == "*/*" {
			resp = get(root, image+tc.query, http.Header{"Accept": {tc.accept}, "If-None-Match": {resp.Header.Get("ETag")}})
			assert.Equal(t, http.StatusNotModified, resp.StatusCode, "the ETag names the variant")
		}
	}
	resp = get(root, image+"&format=png", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "a format the worker did not produce")
	resp = root.do(http.MethodGet, "/buckets/pics/objects/media/status?key=summer%2Fbeach.jpg", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), `"url":"/api/v1/buckets/pics/objects/image?format=webp\u0026key=summer%2Fbeach.jpg\u0026w=320"`)

	// A user without s3:GetObject.
	frank := e.consoleLogin(e.createUser("frank", "frank-secret-1", "diagnostics"))
	assert.Equal(t, http.StatusForbidden, get(frank, image, nil).StatusCode)

	// An image share: the S3 listener, anonymous, the key in the path.
	resp = root.do(http.MethodPost, "/buckets/pics/objects/share", `{"key":"summer/beach.jpg","image":true,"expiresIn":3600}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	var sh struct {
		ID    string `json:"id"`
		URL   string `json:"url"`
		Image bool   `json:"image"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &sh))
	assert.True(t, sh.Image)
	link := mustParse(t, sh.URL)
	assert.Equal(t, "/nexo/media/v1/pics/image/summer/beach.jpg", link.Path)
	anonymous := func(u *url.URL, accept string) response {
		req := newRequest(t, http.MethodGet, u.String(), nil)
		req.Header.Set("Accept", accept)
		return do(t, req)
	}
	resp = anonymous(link, "image/webp,*/*")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "images/full.webp", string(resp.Body))
	assert.Equal(t, "Accept", resp.Header.Get("Vary"))
	other := *link
	other.Path = "/nexo/media/v1/pics/image/summer/other.jpg"
	assert.Equal(t, http.StatusForbidden, anonymous(&other, "*/*").StatusCode, "the signature covers the key")

	// A new content: the variants no longer apply.
	e.put("pics", "summer/beach.jpg", []byte("another photo"))
	assert.Equal(t, http.StatusNotFound, get(root, image, nil).StatusCode)
	assert.Equal(t, http.StatusNotFound, anonymous(link, "*/*").StatusCode)

	// Revoked, the link is denied.
	resp = root.do(http.MethodDelete, "/shares/"+sh.ID, "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, http.StatusForbidden, anonymous(link, "*/*").StatusCode)
}

// playHLS plays an HLS presentation with get: the master playlist and its
// first media playlist (playMedia; the fixtures have 100 frames). It
// returns the URL of the media playlist.
func playHLS(t *testing.T, get func(u *url.URL) response, master *url.URL) *url.URL {
	t.Helper()
	resp := get(master)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "application/vnd.apple.mpegurl", resp.Header.Get("Content-Type"))
	media := master.ResolveReference(mustParse(t, uris(resp.Body)[0]))
	playMedia(t, get, media, 100)
	return media
}

// playMedia plays a media playlist: the init segment and every media
// segment, whose frames ffprobe counts when it is installed.
func playMedia(t *testing.T, get func(u *url.URL) response, media *url.URL, frames int) {
	t.Helper()
	resp := get(media)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	playlist := string(resp.Body)
	assert.Contains(t, playlist, "#EXT-X-ENDLIST")
	segments := uris(resp.Body)
	require.NotEmpty(t, segments)
	initURI := regexp.MustCompile(`#EXT-X-MAP:URI="([^"]+)"`).FindStringSubmatch(playlist)
	require.Len(t, initURI, 2)

	var stream bytes.Buffer
	for _, ref := range append([]string{initURI[1]}, segments...) {
		resp := get(media.ResolveReference(mustParse(t, ref)))
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", ref, resp.Body)
		stream.Write(resp.Body)
	}
	// The segments of a protected video are encrypted (their decoding is
	// covered by the tests of the media service).
	encrypted := strings.Contains(playlist, "#EXT-X-KEY:")
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil && !encrypted {
		file := filepath.Join(t.TempDir(), "stream.mp4")
		require.NoError(t, os.WriteFile(file, stream.Bytes(), 0o600))
		out, err := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
			"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", file).Output()
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(frames), strings.TrimSpace(string(out)), "every frame of the video decodes")
	}
}

// uris returns the URI lines of a playlist.
func uris(playlist []byte) []string {
	var out []string
	for _, line := range strings.Split(string(playlist), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// mediaWorkerPolicy is the policy of the media worker of
// docs/user/media-worker.md, on every bucket.
const mediaWorkerPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:GetObject", "s3:GetObjectVersion", "nexo:DownloadProtectedMedia"], "Resource": ["arn:aws:s3:::*/*"]},
    {"Effect": "Allow", "Action": ["s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion"], "Resource": ["arn:aws:s3:::*/.nexo-media/*"]},
    {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:ListBucketVersions"], "Resource": ["arn:aws:s3:::*"],
     "Condition": {"StringLike": {"s3:prefix": [".nexo-media/*"]}}}
  ]
}`

// TestMediaWorker: a media rule of a bucket sends the jobs of its videos to
// the media worker (the notify_media_worker target); the worker derives a
// rendition, a poster and the thumbnails through the S3 API, the master
// playlist then lists the rendition next to the source, and deleting the
// video removes its sidecar directory.
func TestMediaWorker(t *testing.T) {
	libPath := os.Getenv("NEXO_OPENH264")
	if libPath == "" {
		t.Skip("NEXO_OPENH264 is not set (the path of libopenh264 " + openh264.Version + ")")
	}
	e := newEnv(t)
	video, err := os.ReadFile(filepath.Join("..", "..", "internal", "mediaworker", "transcode", "testdata", "h264_aac_360p.mp4"))
	require.NoError(t, err)
	e.mkBucket("videos")
	root := e.consoleLogin(e.rootCreds())

	// The worker, with the least-privilege policy of the documentation:
	// it reads the videos and writes only under the sidecar prefix.
	e.setPolicy("media-worker", mediaWorkerPolicy)
	wc := e.createUser("media-worker", "media-worker-secret", "media-worker")
	lib, err := openh264.Open(libPath)
	require.NoError(t, err)
	store, err := worker.NewS3Store(worker.S3Config{Endpoint: e.srv.S3URL, Region: e.srv.Config.Region,
		AccessKey: wc.AccessKey, SecretKey: wc.SecretKey, HTTP: e.loopbackClient()})
	require.NoError(t, err)
	const token = "worker-token"
	w, err := worker.New(worker.Config{AuthToken: token, WorkDir: t.TempDir(), Backoff: 100 * time.Millisecond}, store, lib, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	ws := httptest.NewServer(w.Handler())
	t.Cleanup(ws.Close)
	ctx, cancel := context.WithCancel(e.ctx)
	stopped := make(chan struct{})
	go func() { w.Run(ctx); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })

	resp := root.do(http.MethodPut, "/config/notify_media_worker", fmt.Sprintf(`{"settings":{"enable":"on","endpoint":%q,"auth_token":%q}}`, ws.URL+"/jobs", token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	id := event.TargetID{ID: event.DefaultTargetID, Type: event.TypeMediaWorker}
	require.Eventually(t, func() bool {
		for _, n := range e.srv.Nodes {
			if n.App == nil || n.App.Events() == nil {
				return false
			}
			if _, err := n.App.Events().GetTarget(e.ctx, id); err != nil {
				return false
			}
		}
		return true
	}, 30*time.Second, 20*time.Millisecond, "every node loads the target")
	resp = root.do(http.MethodPut, "/buckets/videos/media", `{"rules":[{"id":"clips","prefix":"clips/","renditions":["240p","1080p"],"poster":true,"thumbnails":{"interval":"1s"}}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)

	e.put("videos", "clips/intro.mp4", video)
	e.put("videos", "other/skip.mp4", video) // no rule
	var status struct {
		State      string `json:"state"`
		Error      string `json:"error"`
		Current    bool   `json:"current"`
		Renditions []struct {
			Name string `json:"name"`
		} `json:"renditions"`
		Poster bool `json:"poster"`
	}
	require.Eventually(t, func() bool {
		resp := root.do(http.MethodGet, "/buckets/videos/objects/media/status?key=clips%2Fintro.mp4", "")
		if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &status) != nil {
			return false
		}
		require.NotEqual(t, "failed", status.State, status.Error)
		return status.State == "done"
	}, 2*time.Minute, 100*time.Millisecond, "the worker processes the video")
	assert.True(t, status.Current)
	assert.True(t, status.Poster)
	require.Len(t, status.Renditions, 1, "1080p is taller than the source")
	assert.Equal(t, "240p", status.Renditions[0].Name)

	// The master playlist lists the rendition, which plays.
	get := func(u *url.URL) response {
		return doWith(t, root.client, newRequest(t, http.MethodGet, u.String(), nil))
	}
	masterURL := mustParse(t, e.srv.ConsoleURL+"/api/v1/buckets/videos/objects/hls/master.m3u8?key=clips%2Fintro.mp4")
	master := get(masterURL)
	require.Equal(t, http.StatusOK, master.StatusCode)
	assert.Equal(t, 2, strings.Count(string(master.Body), "#EXT-X-STREAM-INF"), "%s", master.Body)
	m := regexp.MustCompile(`(?m)^(media\.m3u8\?[^\n]*r=240p[^\n]*)$`).FindStringSubmatch(string(master.Body))
	require.NotNil(t, m, "%s", master.Body)
	playMedia(t, get, masterURL.ResolveReference(mustParse(t, m[1])), 75)
	poster := get(mustParse(t, e.srv.ConsoleURL+"/api/v1/buckets/videos/objects/hls/poster.jpg?key=clips%2Fintro.mp4"))
	require.Equal(t, http.StatusOK, poster.StatusCode)
	assert.Equal(t, "image/jpeg", poster.Header.Get("Content-Type"))
	vtt := get(mustParse(t, e.srv.ConsoleURL+"/api/v1/buckets/videos/objects/hls/thumbnails.vtt?key=clips%2Fintro.mp4"))
	require.Equal(t, http.StatusOK, vtt.StatusCode)
	assert.True(t, strings.HasPrefix(string(vtt.Body), "WEBVTT"))

	// No job for a video no rule selects.
	resp = root.do(http.MethodGet, "/buckets/videos/objects/media/status?key=other%2Fskip.mp4", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(resp.Body), `"state":"none"`)

	// Deleting the video removes its outputs.
	_, err = e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("videos"), Key: aws.String("clips/intro.mp4")})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		out, err := e.root.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String("videos"), Prefix: aws.String(domain.MediaSidecarPrefix)})
		return err == nil && len(out.Contents) == 0
	}, time.Minute, 100*time.Millisecond, "the sidecar directory is removed")

	// A versioned bucket: a reprocessing replaces the outputs without
	// keeping their old versions, and deleting the version of the video
	// for good removes every version of its sidecar.
	e.mkBucket("archive")
	_, err = e.root.PutBucketVersioning(e.ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("archive"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	resp = root.do(http.MethodPut, "/buckets/archive/media", `{"rules":[{"id":"all","poster":true}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	put, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("archive"), Key: aws.String("talk.mp4"), Body: bytes.NewReader(video)})
	require.NoError(t, err)
	versionID := aws.ToString(put.VersionId)
	statusPath := "/buckets/archive/objects/media/status?key=talk.mp4&versionId=" + url.QueryEscape(versionID)
	waitDone := func(after time.Time) {
		t.Helper()
		require.Eventually(t, func() bool {
			var st struct {
				State   string    `json:"state"`
				Error   string    `json:"error"`
				Updated time.Time `json:"updated"`
			}
			resp := root.do(http.MethodGet, statusPath, "")
			if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &st) != nil {
				return false
			}
			require.NotEqual(t, "failed", st.State, st.Error)
			return st.State == "done" && st.Updated.After(after)
		}, 2*time.Minute, 100*time.Millisecond)
	}
	waitDone(time.Time{})
	sidecarVersions := func() int {
		out, err := e.root.ListObjectVersions(e.ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("archive"), Prefix: aws.String(domain.MediaSidecarPrefix)})
		require.NoError(t, err)
		return len(out.Versions) + len(out.DeleteMarkers)
	}
	require.Eventually(t, func() bool { return sidecarVersions() == 2 }, time.Minute, 100*time.Millisecond, "status.json and poster.jpg, one version each")
	before := time.Now()
	resp = root.do(http.MethodPost, "/buckets/archive/media/reprocess", fmt.Sprintf(`{"key":"talk.mp4","versionId":%q}`, versionID))
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "%s", resp.Body)
	waitDone(before)
	require.Eventually(t, func() bool { return sidecarVersions() == 2 }, time.Minute, 100*time.Millisecond, "the replaced versions are removed")

	// A delete marker keeps the version and its outputs; deleting the
	// version removes them.
	_, err = e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("archive"), Key: aws.String("talk.mp4")})
	require.NoError(t, err)
	assert.Equal(t, 2, sidecarVersions())
	_, err = e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("archive"), Key: aws.String("talk.mp4"), VersionId: aws.String(versionID)})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return sidecarVersions() == 0 }, time.Minute, 100*time.Millisecond, "every version of the sidecar is removed")

	// A bucket created with the HLS option: every rendition up to the
	// height of the video, a poster and thumbnails, with no rule written.
	resp = root.do(http.MethodPost, "/buckets", `{"name":"films","hls":true}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	e.put("films", "trailer.mp4", video)
	require.Eventually(t, func() bool {
		resp := root.do(http.MethodGet, "/buckets/films/objects/media/status?key=trailer.mp4", "")
		if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &status) != nil {
			return false
		}
		require.NotEqual(t, "failed", status.State, status.Error)
		return status.State == "done"
	}, 2*time.Minute, 100*time.Millisecond, "the worker processes the video of the HLS bucket")
	assert.True(t, status.Poster)
	names := make([]string, 0, len(status.Renditions))
	for _, r := range status.Renditions {
		names = append(names, r.Name)
	}
	assert.Equal(t, []string{"360p", "240p"}, names, "the presets up to the 360p source")
	master = get(mustParse(t, e.srv.ConsoleURL+"/api/v1/buckets/films/objects/hls/master.m3u8?key=trailer.mp4"))
	require.Equal(t, http.StatusOK, master.StatusCode)
	assert.Equal(t, 3, strings.Count(string(master.Body), "#EXT-X-STREAM-INF"), "the source and two renditions: %s", master.Body)

	// A bucket created with the images option: the worker derives every
	// format at every width up to the image's own, and the server serves the
	// best one the client accepts.
	resp = root.do(http.MethodPost, "/buckets", `{"name":"photos","images":true}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	photo := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := range 600 {
		for x := range 800 {
			photo.Set(x, y, color.RGBA{R: uint8(x * 255 / 800), G: uint8(y * 255 / 600), B: 128, A: 255}) // #nosec G115 -- bounded
		}
	}
	var photoJPEG bytes.Buffer
	require.NoError(t, jpeg.Encode(&photoJPEG, photo, &jpeg.Options{Quality: 90}))
	e.put("photos", "trips/sea.jpg", photoJPEG.Bytes())
	var imgStatus domain.MediaStatus
	require.Eventually(t, func() bool {
		resp := root.do(http.MethodGet, "/buckets/photos/objects/media/status?key=trips%2Fsea.jpg", "")
		if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &imgStatus) != nil {
			return false
		}
		require.NotEqual(t, domain.MediaStateFailed, imgStatus.State, imgStatus.Error)
		return imgStatus.State == domain.MediaStateDone
	}, 2*time.Minute, 100*time.Millisecond, "the worker processes the image")
	assert.Equal(t, 800, imgStatus.Width)
	assert.False(t, imgStatus.Alpha)
	perFormat := map[string][]int{}
	for _, v := range imgStatus.Images {
		perFormat[v.Format] = append(perFormat[v.Format], v.Width)
	}
	for _, f := range domain.MediaImageFormats() {
		assert.ElementsMatch(t, []int{0, 160, 320, 640}, perFormat[f], "%s: the widths below 800 and the own size", f)
	}
	for accept, want := range map[string]string{
		"image/avif,image/webp,*/*": "image/avif",
		"image/webp,*/*":            "image/webp",
		"*/*":                       "image/jpeg",
	} {
		r := root.doRaw(http.MethodGet, "/buckets/photos/objects/image?key=trips%2Fsea.jpg&w=300", nil, http.Header{"Accept": {accept}})
		require.Equal(t, http.StatusOK, r.StatusCode, "%s", r.Body)
		assert.Equal(t, want, r.Header.Get("Content-Type"), accept)
		if want == "image/jpeg" {
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(r.Body))
			require.NoError(t, err)
			assert.Equal(t, 320, cfg.Width, "the narrowest variant at least 300 wide")
		}
	}
}
