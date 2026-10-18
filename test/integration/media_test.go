package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// playHLS plays an HLS presentation with get: the master playlist, the
// media playlist, the init segment and every media segment, whose frames
// ffprobe counts when it is installed (the fixtures have 100). It returns
// the URL of the media playlist.
func playHLS(t *testing.T, get func(u *url.URL) response, master *url.URL) *url.URL {
	t.Helper()
	resp := get(master)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "application/vnd.apple.mpegurl", resp.Header.Get("Content-Type"))
	media := master.ResolveReference(mustParse(t, uris(resp.Body)[0]))

	resp = get(media)
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
	if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
		file := filepath.Join(t.TempDir(), "stream.mp4")
		require.NoError(t, os.WriteFile(file, stream.Bytes(), 0o600))
		out, err := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
			"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", file).Output()
		require.NoError(t, err)
		assert.Equal(t, "100", strings.TrimSpace(string(out)), "every frame of the video decodes")
	}
	return media
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
