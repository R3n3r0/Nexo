package integration_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// gcm is AES-128-GCM with key.
func gcm(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return aead
}

// TestVideoProtectionAdvanced: the advanced playback of a protected video
// through a real server, as the Nexo player plays it. The playback token
// alone opens no stream; the player opens a session over the WebSocket
// (ECDH P-256, HKDF-SHA256), receives the keys wrapped for it and
// decrypts every segment, sealed for the session (AES-128-GCM bound to
// its number). With the forensic watermark, no stream is served before
// the media worker derived the marked renditions, and the trace of a
// leaked copy finds the token among its records.
//
// covers: S-OBJ-18, S-OBJ-19
func TestVideoProtectionAdvanced(t *testing.T) {
	e := newEnv(t)
	video, err := os.ReadFile(filepath.Join("..", "..", "internal", "service", "media", "testdata", "h264_aac.mp4"))
	require.NoError(t, err)
	e.mkBucket("lms")
	e.put("lms", "courses/intro.mp4", video)

	admin := func(c creds) nexoadmin.AdminClient {
		cl, err := nexoadmin.New(nexoadmin.Config{Endpoint: e.srv.ConsoleURL, AccessKey: c.AccessKey, SecretKey: c.SecretKey, Region: e.srv.Config.Region})
		require.NoError(t, err)
		return cl
	}
	root := admin(e.rootCreds())
	_, err = root.SetMediaProtection(e.ctx, "lms", nexoadmin.MediaProtection{Enabled: true, Prefix: "courses/", Mode: "advanced"})
	require.NoError(t, err)
	tok, err := root.IssuePlaybackToken(e.ctx, "lms", "courses/intro.mp4", "", nexoadmin.PlaybackTokenRequest{Subject: "student-7"})
	require.NoError(t, err)
	master := mustParse(t, tok.Playlist)
	anonymous := func(u *url.URL) response { return do(t, newRequest(t, http.MethodGet, u.String(), nil)) }

	// The token alone: refused, and the player learns why.
	resp := anonymous(master)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "session", resp.Header.Get("X-Nexo-Playback"))

	// The session.
	sessionURL := *master
	sessionURL.Scheme = strings.Replace(master.Scheme, "http", "ws", 1)
	sessionURL.Path = strings.TrimSuffix(master.Path, "master.m3u8") + "session"
	conn, _, err := websocket.Dial(e.ctx, sessionURL.String(), nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	hello, err := json.Marshal(map[string]any{"t": "hello", "v": 1, "pub": base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())})
	require.NoError(t, err)
	require.NoError(t, conn.Write(e.ctx, websocket.MessageText, hello))
	_, data, err := conn.Read(e.ctx)
	require.NoError(t, err)
	var ready struct {
		T, Pub, Salt, Session, Master string
		Period                        int
	}
	require.NoError(t, json.Unmarshal(data, &ready), "%s", data)
	require.Equal(t, "ready", ready.T, "%s", data)
	pub, err := base64.RawURLEncoding.DecodeString(ready.Pub)
	require.NoError(t, err)
	serverKey, err := ecdh.P256().NewPublicKey(pub)
	require.NoError(t, err)
	secret, err := priv.ECDH(serverKey)
	require.NoError(t, err)
	salt, err := base64.RawURLEncoding.DecodeString(ready.Salt)
	require.NoError(t, err)
	wrapKey, err := hkdf.Key(sha256.New, secret, salt, "nexo/media/session-wrap-v1\x00"+ready.Session, 16)
	require.NoError(t, err)
	wrap := gcm(t, wrapKey)
	require.Positive(t, ready.Period)

	keys := map[string][]byte{}
	keyOf := func(rev, r string, i int) []byte {
		id := fmt.Sprintf("%s|%s|%d", rev, r, i)
		if k, ok := keys[id]; ok {
			return k
		}
		req, err := json.Marshal(map[string]any{"t": "key", "id": len(keys) + 1, "rev": rev, "r": r, "i": i})
		require.NoError(t, err)
		require.NoError(t, conn.Write(e.ctx, websocket.MessageText, req))
		_, data, err := conn.Read(e.ctx)
		require.NoError(t, err)
		var m struct{ T, Key string }
		require.NoError(t, json.Unmarshal(data, &m))
		require.Equal(t, "key", m.T, "%s", data)
		sealed, err := base64.RawURLEncoding.DecodeString(m.Key)
		require.NoError(t, err)
		k, err := wrap.Open(nil, sealed[:12], sealed[12:], []byte("nexo/media/key\x00"+rev+"\x00"+r+"\x00"+strconv.Itoa(i)))
		require.NoError(t, err, "the key is wrapped for this session")
		keys[id] = k
		return k
	}

	// The ticket plays the presentation of the session: its playlists name
	// no key, its segments are sealed.
	sessionMaster := master.ResolveReference(mustParse(t, ready.Master))
	resp = anonymous(sessionMaster)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	media := sessionMaster.ResolveReference(mustParse(t, uris(resp.Body)[0]))
	resp = anonymous(media)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.NotContains(t, string(resp.Body), "#EXT-X-KEY")
	segments := uris(resp.Body)
	require.NotEmpty(t, segments)
	for _, ref := range segments {
		u := media.ResolveReference(mustParse(t, ref))
		q := u.Query()
		n, err := strconv.Atoi(q.Get("n"))
		require.NoError(t, err)
		resp := anonymous(u)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		plain, err := gcm(t, keyOf(q.Get("rev"), q.Get("r"), n/ready.Period)).Open(nil, resp.Body[:12], resp.Body[12:], []byte(strconv.Itoa(n)))
		require.NoError(t, err, "segment %d decrypts with the key of the session", n)
		assert.Equal(t, "moof", string(plain[4:8]), "segment %d is fMP4", n)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// The forensic watermark: no marked rendition yet, no stream.
	_, err = root.SetMediaProtection(e.ctx, "lms", nexoadmin.MediaProtection{Enabled: true, Prefix: "courses/", Mode: "advanced", Forensic: true})
	require.NoError(t, err)
	marked, err := root.IssuePlaybackToken(e.ctx, "lms", "courses/intro.mp4", "", nexoadmin.PlaybackTokenRequest{Subject: "student-9", Watermark: "Anna Bianchi"})
	require.NoError(t, err)
	conn, _, err = websocket.Dial(e.ctx, strings.Replace(sessionURL.String(), url.QueryEscape(tok.Token), url.QueryEscape(marked.Token), 1), nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, conn.Write(e.ctx, websocket.MessageText, hello))
	_, data, err = conn.Read(e.ctx)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &ready))
	resp = anonymous(master.ResolveReference(mustParse(t, ready.Master)))
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "never streamed unmarked: %s", resp.Body)

	// The trace: every token issued under the forensic watermark has a
	// record, found by the bits of its code (one bit here, so either value
	// names it); the earlier token has none.
	var found []string
	for _, bits := range []string{"0", "1"} {
		res, err := root.TracePlaybackLeak(e.ctx, "lms", "courses/intro.mp4", "", nexoadmin.PlaybackTraceRequest{Bits: bits, Limit: 100})
		require.NoError(t, err)
		for _, m := range res.Matches {
			found = append(found, m.Subject+"/"+m.Watermark)
		}
	}
	assert.ElementsMatch(t, []string{"student-9/Anna Bianchi"}, found)

	// The trace needs admin:TracePlaybackLeaks.
	e.setPolicy("lms-player", `{"Version": "2012-10-17", "Statement": [
	  {"Effect": "Allow", "Action": ["nexo:IssuePlaybackToken"], "Resource": ["arn:aws:s3:::lms/courses/*"]}]}`)
	app := admin(e.createUser("lms-app", "lms-app-secret-1", "lms-player"))
	_, err = app.TracePlaybackLeak(e.ctx, "lms", "courses/intro.mp4", "", nexoadmin.PlaybackTraceRequest{Bits: "0"})
	require.Error(t, err)
}
