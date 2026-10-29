package s3_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/media"
	mediamock "github.com/r3n3r0/nexo/internal/service/media/mock"
	"github.com/r3n3r0/nexo/internal/service/playback"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// sessionProtections is a fixed protection for every bucket.
type sessionProtections struct{ p *domain.MediaProtection }

func (f sessionProtections) GetMediaProtection(context.Context, string) (domain.MediaProtection, error) {
	return *f.p, nil
}

// sessionFixture serves the media routes with a real playback service and
// a mocked media service.
type sessionFixture struct {
	media    *mediamock.Service
	playback playback.Service
	prot     *domain.MediaProtection
	srv      *httptest.Server
	stopping chan struct{}
}

func newSessionFixture(t *testing.T, mut func(*domain.MediaProtection)) *sessionFixture {
	t.Helper()
	prot := &domain.MediaProtection{Enabled: true, Mode: domain.MediaPlaybackAdvanced, MaxSessions: -1}
	if mut != nil {
		mut(prot)
	}
	require.NoError(t, prot.Validate())
	pb, err := playback.New(playback.Deps{Protections: sessionProtections{prot}}, playback.WithKey([]byte("0123456789abcdef0123456789abcdef")))
	require.NoError(t, err)
	f := &sessionFixture{media: mediamock.NewService(t), playback: pb, prot: prot, stopping: make(chan struct{})}
	f.media.EXPECT().Protection(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, media.Source) (*domain.MediaProtection, error) {
		if !f.prot.Enabled {
			return nil, nil
		}
		p := *f.prot
		return &p, nil
	}).Maybe()
	routes, err := s3.NewMediaRoutes(s3.MediaDeps{Media: f.media, Policy: policymock.NewService(t), Signer: authmock.NewSigner(t),
		Creds: authmock.NewCredentialLookup(t), Playback: pb, Stopping: f.stopping, SessionAlive: 50 * time.Millisecond, Clock: clock.NewReal()})
	require.NoError(t, err)
	router := chi.NewRouter()
	routes.Register(router)
	f.srv = httptest.NewServer(router)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sessionFixture) token(t *testing.T) string {
	t.Helper()
	tok, err := f.playback.Issue(context.Background(), playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "s1", Watermark: "Mario"})
	require.NoError(t, err)
	return tok.Token
}

// player is the client side of a session: its ECDH key and the wrapping
// key it derives from the answer of the server.
type player struct {
	priv *ecdh.PrivateKey
	wrap cipher.AEAD
}

func newPlayer(t *testing.T) *player {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	return &player{priv: priv}
}

func (p *player) hello() []byte {
	data, _ := json.Marshal(map[string]any{"t": "hello", "v": 1, "pub": base64.RawURLEncoding.EncodeToString(p.priv.PublicKey().Bytes())})
	return data
}

type readyMsg struct {
	T, Pub, Salt, Ticket, Master, Session string
	Exp                                   int64
}

func (p *player) ready(t *testing.T, data []byte) readyMsg {
	t.Helper()
	var r readyMsg
	require.NoError(t, json.Unmarshal(data, &r), string(data))
	require.Equal(t, "ready", r.T, string(data))
	pub, err := base64.RawURLEncoding.DecodeString(r.Pub)
	require.NoError(t, err)
	server, err := ecdh.P256().NewPublicKey(pub)
	require.NoError(t, err)
	secret, err := p.priv.ECDH(server)
	require.NoError(t, err)
	salt, err := base64.RawURLEncoding.DecodeString(r.Salt)
	require.NoError(t, err)
	wrap, err := hkdf.Key(sha256.New, secret, salt, "nexo/media/session-wrap-v1\x00"+r.Session, 16)
	require.NoError(t, err)
	block, err := aes.NewCipher(wrap)
	require.NoError(t, err)
	p.wrap, err = cipher.NewGCM(block)
	require.NoError(t, err)
	return r
}

func (p *player) unwrap(t *testing.T, key, aad string) []byte {
	t.Helper()
	sealed, err := base64.RawURLEncoding.DecodeString(key)
	require.NoError(t, err)
	out, err := p.wrap.Open(nil, sealed[:12], sealed[12:], []byte(aad))
	require.NoError(t, err)
	return out
}

func keyAAD(rev, r string, i int) string {
	return "nexo/media/key\x00" + rev + "\x00" + r + "\x00" + string(rune('0'+i))
}

// expectKey mocks the media service for the key of period i of the
// rendition r of a session.
func (f *sessionFixture) expectKey(r string, i int, key []byte) {
	f.media.EXPECT().Describe(mock.Anything, mock.MatchedBy(func(s media.Source) bool { return s.Rendition == r && s.Session != "" })).
		Return(&media.Presentation{Segments: []time.Duration{6 * time.Second, 6 * time.Second}}, nil).Once()
	f.media.EXPECT().Key(mock.Anything, mock.MatchedBy(func(s media.Source) bool {
		return s.Rendition == r && s.Revision == "e1" && s.Session != "" && s.Viewer != ""
	}), i).Return(key, nil).Once()
}

// TestMediaSessionStandardRefused: the advanced playback refuses the
// tokens on the standard routes (the player learns it from a header),
// unless the older devices may play the standard way.
//
// covers: S-OBJ-18
func TestMediaSessionStandardRefused(t *testing.T) {
	f := newSessionFixture(t, nil)
	target := f.srv.URL + s3.MediaPrefix + "lms/master.m3u8?key=courses%2Fintro.mp4&pt=" + url.QueryEscape(f.token(t))
	resp, err := http.Get(target)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "session", resp.Header.Get("X-Nexo-Playback"))

	f.prot.LegacyDevices = domain.MediaLegacyStandard
	f.media.EXPECT().MasterPlaylist(mock.Anything, mock.Anything, mock.Anything).Return([]byte("#EXTM3U\n"), nil).Once()
	resp, err = http.Get(target)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the older devices play the standard way")
}

// TestMediaSessionWebSocket: a session over the WebSocket: the ready
// message, the keys wrapped for the session and bound to their
// presentation and period, the ticket authorizing the playlists (never
// the standard keys), and the channel closed on revocation and at the
// stop of the node.
//
// covers: S-OBJ-18
func TestMediaSessionWebSocket(t *testing.T) {
	f := newSessionFixture(t, nil)
	ctx := context.Background()
	tok := f.token(t)
	wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http") + s3.MediaPrefix + "lms/session?key=courses%2Fintro.mp4&pt=" + url.QueryEscape(tok)

	_, resp, err := websocket.Dial(ctx, strings.Replace(wsURL, "pt=", "pt=x", 1), nil)
	require.Error(t, err, "a bad token gets no channel")
	if resp != nil {
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	p := newPlayer(t)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, p.hello()))
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	ready := p.ready(t, data)
	assert.True(t, strings.HasPrefix(ready.Master, s3.MediaPrefix+"lms/master.m3u8?"), ready.Master)
	assert.Contains(t, ready.Master, "st=")

	key := []byte("fedcba9876543210")
	f.expectKey("720p", 1, key)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"t":"key","id":7,"rev":"e1","r":"720p","i":1}`)))
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)
	var m struct{ T, Key, Code string }
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "key", m.T, string(data))
	assert.Equal(t, key, p.unwrap(t, m.Key, keyAAD("e1", "720p", 1)))
	assert.NotContains(t, string(data), base64.RawURLEncoding.EncodeToString(key), "never in clear")

	// The ticket plays the presentation of the session, never a standard
	// key; the ticket alone opens nothing else.
	f.media.EXPECT().MasterPlaylist(mock.Anything, mock.MatchedBy(func(s media.Source) bool { return s.Session == ready.Session }), mock.Anything).
		Return([]byte("#EXTM3U\n"), nil).Once()
	resp, err = http.Get(f.srv.URL + ready.Master)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	stKey := strings.Replace(ready.Master, "master.m3u8?", "key?rev=e1&i=0&", 1)
	resp, err = http.Get(f.srv.URL + stKey)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the keys of a session go through its channel")

	// Revoked: the channel says so and closes.
	claims, err := f.playback.Verify(ctx, tok, "lms", "courses/intro.mp4", "")
	require.NoError(t, err)
	require.NoError(t, f.playback.Revoke(ctx, claims.SessionID))
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"t":"closed"`)
	assert.Contains(t, string(data), `"code":"denied"`)
	_, _, err = conn.Read(ctx)
	assert.Equal(t, websocket.StatusPolicyViolation, websocket.CloseStatus(err))

	// A node stopping closes the channels.
	conn2, _, err := websocket.Dial(ctx, strings.Replace(wsURL, url.QueryEscape(tok), url.QueryEscape(f.token(t)), 1), nil)
	require.NoError(t, err)
	defer func() { _ = conn2.CloseNow() }()
	require.NoError(t, conn2.Write(ctx, websocket.MessageText, newPlayer(t).hello()))
	_, _, err = conn2.Read(ctx)
	require.NoError(t, err)
	close(f.stopping)
	_, _, err = conn2.Read(ctx)
	assert.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
}

// TestMediaSessionPost: the same session without WebSocket (a proxy that
// carries none): the hello and the keys as POST requests.
//
// covers: S-OBJ-18
func TestMediaSessionPost(t *testing.T) {
	f := newSessionFixture(t, nil)
	base := f.srv.URL + s3.MediaPrefix + "lms/session"
	p := newPlayer(t)
	post := func(target string, body []byte) (int, []byte) {
		resp, err := http.Post(target, "text/plain", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, data
	}
	status, data := post(base+"?key=courses%2Fintro.mp4&pt="+url.QueryEscape(f.token(t)), p.hello())
	require.Equal(t, http.StatusOK, status, string(data))
	ready := p.ready(t, data)

	key := []byte("0011223344556677")
	f.expectKey("", 0, key)
	status, data = post(base+"/key?key=courses%2Fintro.mp4&st="+url.QueryEscape(ready.Ticket), []byte(`{"id":1,"rev":"e1","i":0}`))
	require.Equal(t, http.StatusOK, status, string(data))
	var m struct{ Key string }
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, key, p.unwrap(t, m.Key, keyAAD("e1", "", 0)))

	status, _ = post(base+"/key?key=courses%2Fintro.mp4&st=forged", []byte(`{"id":1,"rev":"e1","i":0}`))
	assert.Equal(t, http.StatusForbidden, status)
	status, _ = post(base+"?key=courses%2Fintro.mp4&pt="+url.QueryEscape(f.token(t)), []byte(`{"t":"hello","v":1,"pub":"bad"}`))
	assert.Equal(t, http.StatusBadRequest, status)
	f.prot.Enabled = false
	status, _ = post(base+"?key=courses%2Fintro.mp4&pt="+url.QueryEscape(f.token(t)), p.hello())
	assert.Equal(t, http.StatusNotFound, status, "a video that is not protected plays without a session")
}
