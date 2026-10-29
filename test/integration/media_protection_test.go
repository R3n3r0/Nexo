package integration_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/playback"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestVideoProtection: the full flow of a protected video. The bucket owner
// enables the protection; an application key allowed only the playback
// tokens issues one for a viewer; a plain HTTP client (a player) plays the
// encrypted presentation with it, keys included; a newer stream of the
// same viewer replaces it (maxSessions 1); revoking the viewer stops the
// stream; the original cannot be downloaded by a user allowed s3:GetObject,
// while an unprotected video can.
//
// covers: S-OBJ-17
func TestVideoProtection(t *testing.T) {
	e := newEnv(t)
	video, err := os.ReadFile(filepath.Join("..", "..", "internal", "service", "media", "testdata", "h264_aac.mp4"))
	require.NoError(t, err)
	e.mkBucket("lms")
	e.put("lms", "courses/intro.mp4", video)
	e.put("lms", "public/teaser.mp4", video)

	admin := func(c creds) nexoadmin.AdminClient {
		cl, err := nexoadmin.New(nexoadmin.Config{Endpoint: e.srv.ConsoleURL, AccessKey: c.AccessKey, SecretKey: c.SecretKey, Region: e.srv.Config.Region})
		require.NoError(t, err)
		return cl
	}
	root := admin(e.rootCreds())
	p, err := root.SetMediaProtection(e.ctx, "lms", nexoadmin.MediaProtection{Enabled: true, Prefix: "courses/", MaxSessions: 1, Watermark: true})
	require.NoError(t, err)
	assert.Equal(t, 7200, p.TokenTTLSeconds)

	e.setPolicy("lms-player", `{"Version": "2012-10-17", "Statement": [
	  {"Effect": "Allow", "Action": ["nexo:IssuePlaybackToken"], "Resource": ["arn:aws:s3:::lms/courses/*"]},
	  {"Effect": "Allow", "Action": ["admin:RevokePlaybackTokens"], "Resource": ["arn:aws:s3:::lms"]}]}`)
	app := admin(e.createUser("lms-app", "lms-app-secret-1", "lms-player"))
	issue := func() *nexoadmin.PlaybackToken {
		t.Helper()
		tok, err := app.IssuePlaybackToken(e.ctx, "lms", "courses/intro.mp4", "", nexoadmin.PlaybackTokenRequest{Subject: "student-7", Watermark: "Mario Rossi"})
		require.NoError(t, err)
		return tok
	}
	_, err = app.IssuePlaybackToken(e.ctx, "lms", "other/x.mp4", "", nexoadmin.PlaybackTokenRequest{Subject: "s"})
	require.Error(t, err, "the application key issues tokens of its videos only")

	anonymous := func(u *url.URL) response { return do(t, newRequest(t, http.MethodGet, u.String(), nil)) }
	first := issue()
	master := mustParse(t, first.Playlist)
	assert.Equal(t, "/nexo/media/v1/lms/master.m3u8", master.Path)
	media := playHLS(t, anonymous, master)
	keyURI := func(media *url.URL) *url.URL {
		resp := anonymous(media)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		m := regexp.MustCompile(`#EXT-X-KEY:METHOD=AES-128,URI="([^"]+)"`).FindStringSubmatch(string(resp.Body))
		require.Len(t, m, 2, "the playlist of a protected video names its keys")
		return media.ResolveReference(mustParse(t, m[1]))
	}
	key1 := keyURI(media)
	resp := anonymous(key1)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Len(t, resp.Body, 16)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	// Without the token the stream is refused; a newer stream of the same
	// viewer replaces the first one.
	unsigned := *master
	unsigned.RawQuery = url.Values{"key": {"courses/intro.mp4"}}.Encode()
	assert.Equal(t, http.StatusForbidden, anonymous(&unsigned).StatusCode)
	second := mustParse(t, issue().Playlist)
	media2 := master.ResolveReference(mustParse(t, uris(anonymous(second).Body)[0]))
	assert.Equal(t, http.StatusOK, anonymous(keyURI(media2)).StatusCode)
	assert.Equal(t, http.StatusForbidden, anonymous(key1).StatusCode, "replaced by the newer stream")

	// Revoked, the viewer's streams stop: on every node within the
	// refresh of the revocations (playback.DefaultRefresh).
	require.NoError(t, app.RevokePlaybackSubject(e.ctx, "lms", "student-7"))
	assert.Eventually(t, func() bool { return anonymous(second).StatusCode == http.StatusForbidden },
		3*playback.DefaultRefresh/2, 250*time.Millisecond)

	// The original: refused to a user allowed s3:GetObject, served for an
	// unprotected video and to root.
	reader := e.createUser("reader", "reader-secret-1", "readonly")
	assert.Equal(t, http.StatusForbidden, e.signedDo(http.MethodGet, "/lms/courses/intro.mp4", reader, nil, nil).StatusCode)
	assert.Equal(t, http.StatusOK, e.signedDo(http.MethodGet, "/lms/public/teaser.mp4", reader, nil, nil).StatusCode)
	assert.Equal(t, http.StatusOK, e.signedDo(http.MethodGet, "/lms/courses/intro.mp4", e.rootCreds(), nil, nil).StatusCode)
}
