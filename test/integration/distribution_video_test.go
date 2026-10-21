package integration_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// TestDistributionVideo: a behavior streaming the videos serves the HLS
// presentation of a video below its URL on the domain of the
// distribution: a player follows the playlists and plays every frame; a
// bucket policy denying s3:GetObject keeps the original from being
// downloaded while the video still streams; a signed behavior streams
// with a custom policy whose resource covers the URL of the video.
func TestDistributionVideo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("films")
	video, err := os.ReadFile(filepath.Join("..", "..", "internal", "service", "media", "testdata", "h264_aac.mp4"))
	require.NoError(t, err)
	e.put("films", "videos/intro.mp4", video)
	e.put("films", "private/talk.mp4", video)
	_, err = e.root.PutBucketPolicy(e.ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("films"), Policy: aws.String(`{
		"Version":"2012-10-17",
		"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::films/videos/*"}]}`)})
	require.NoError(t, err)

	dists := e.srv.Nodes[0].App.Distributions()
	_, err = dists.CreateKeyGroup(e.ctx, "players", "")
	require.NoError(t, err)
	key, err := dists.GenerateKey(e.ctx, distribution.GenerateKeyInput{KeyGroup: "players"})
	require.NoError(t, err)
	_, err = dists.Create(e.ctx, distribution.Distribution{
		ID: "tv", Enabled: true, Aliases: []string{"tv.site.test"}, Mode: distribution.ModeWebsite,
		Origin:          distribution.Origin{Bucket: "films"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic},
		Behaviors: []distribution.Behavior{
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/videos/"},
				Video: &distribution.Video{HLS: true}},
			{PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/private/"},
				Video: &distribution.Video{HLS: true}, Access: distribution.AccessSigned,
				Signing: &distribution.Signing{KeyGroup: "players"}},
		},
	})
	require.NoError(t, err)
	e.cacheSynced()
	node := e.nodeURLs()[0]
	get := func(u *url.URL) response {
		t.Helper()
		return hostRequest(t, http.MethodGet, node, "tv.site.test", u.RequestURI(), nil)
	}

	// The video plays, every frame of it.
	master := mustParse(t, "http://tv.site.test/videos/intro.mp4/master.m3u8")
	resp := get(master)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "public, no-cache", resp.Header.Get("Cache-Control"))
	media := playHLS(t, get, master)
	assert.Equal(t, "/videos/intro.mp4/media.m3u8", media.Path)
	seg := get(media)
	assert.Equal(t, "public, max-age=86400, immutable", seg.Header.Get("Cache-Control"))

	// The bucket policy denies the download of the original.
	orig := get(mustParse(t, "http://tv.site.test/videos/intro.mp4"))
	assert.Equal(t, http.StatusForbidden, orig.StatusCode, "%s", orig.Body)

	// A signed behavior: refused without a signature; with a custom
	// policy covering the URL of the video, the playlists carry the
	// signature to the files they name.
	assert.Equal(t, http.StatusForbidden, get(mustParse(t, "http://tv.site.test/private/talk.mp4/master.m3u8")).StatusCode)
	signer := e.srv.App.DistributionSigner()
	signed, err := signer.SignURL(e.ctx, distribution.SignInput{
		URL: "http://tv.site.test/private/talk.mp4/master.m3u8", Resource: "http://tv.site.test/private/talk.mp4/*",
		KeyPairID: key.PublicKey.ID, PrivateKeyPEM: key.PrivateKeyPEM, Expires: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	playHLS(t, get, mustParse(t, signed))
	// The signature covers that video only.
	other := mustParse(t, signed)
	other.Path = "/private/other.mp4/master.m3u8"
	assert.Equal(t, http.StatusForbidden, get(other).StatusCode)
}
