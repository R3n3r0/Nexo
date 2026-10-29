package playback_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/playback"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// protections protects the videos under courses/ of every bucket.
type protections struct{ p domain.MediaProtection }

func (f protections) GetMediaProtection(context.Context, string) (domain.MediaProtection, error) {
	return f.p, nil
}

func newProtection(t testing.TB, mut func(*domain.MediaProtection)) *domain.MediaProtection {
	t.Helper()
	p := &domain.MediaProtection{Enabled: true, Prefix: "courses/", TokenTTLSeconds: 3600}
	if mut != nil {
		mut(p)
	}
	require.NoError(t, p.Validate())
	return p
}

func newDocs(t *testing.T) sysstore.Store {
	t.Helper()
	drives := make([]storage.StorageAPI, 4)
	for i := range drives {
		drives[i] = testutil.NewLocalDriveIndex(t, "", i)
	}
	docs, err := sysstore.New(sysstore.Deps{Drives: drives, Locks: lock.NewLocal(lock.Deps{})})
	require.NoError(t, err)
	return docs
}

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newService(t testing.TB, p *domain.MediaProtection, docs sysstore.Store, clk clock.Clock) playback.Service {
	t.Helper()
	svc, err := playback.New(playback.Deps{Protections: protections{*p}, Store: docs, Clock: clk}, playback.WithKey(testKey))
	require.NoError(t, err)
	return svc
}

func TestNewRequiresKey(t *testing.T) {
	_, err := playback.New(playback.Deps{Protections: protections{}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = playback.New(playback.Deps{}, playback.WithKey(testKey))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestIssueVerify: a token plays its video until it expires; a token of
// another video, another version, another deployment or a tampered one
// does not; the claims are readable by the player.
//
// covers: S-OBJ-17
func TestIssueVerify(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	svc := newService(t, newProtection(t, nil), nil, clk)

	tok, err := svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "student-1842", Watermark: "mario@example.com"})
	require.NoError(t, err)
	assert.Equal(t, clk.Now().Add(time.Hour).Unix(), tok.Claims.ExpiresAt, "the default TTL bounded by the protection")
	assert.Len(t, tok.Claims.SessionID, 32)

	payload, _, ok := strings.Cut(tok.Token, ".")
	require.True(t, ok)
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	assert.Equal(t, "mario@example.com", claims["wm"])

	c, err := svc.Verify(ctx, tok.Token, "lms", "courses/intro.mp4", "")
	require.NoError(t, err)
	assert.Equal(t, "student-1842", c.Subject)

	for name, tc := range map[string]struct{ token, bucket, key string }{
		"other key":    {tok.Token, "lms", "courses/other.mp4"},
		"other bucket": {tok.Token, "other", "courses/intro.mp4"},
		"tampered":     {strings.Replace(tok.Token, ".", "x.", 1), "lms", "courses/intro.mp4"},
		"garbage":      {"abc", "lms", "courses/intro.mp4"},
	} {
		_, err := svc.Verify(ctx, tc.token, tc.bucket, tc.key, "")
		require.ErrorIs(t, err, domain.ErrAccessDenied, name)
	}
	other, err := playback.New(playback.Deps{Protections: protections{}, Clock: clk}, playback.WithKey([]byte("another deployment key 32 bytes!")))
	require.NoError(t, err)
	_, err = other.Verify(ctx, tok.Token, "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied, "another deployment")

	pinned, err := svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", VersionID: "v1", Subject: "s"})
	require.NoError(t, err)
	_, err = svc.Verify(ctx, pinned.Token, "lms", "courses/intro.mp4", "v2")
	require.ErrorIs(t, err, domain.ErrAccessDenied, "another version")
	_, err = svc.Verify(ctx, pinned.Token, "lms", "courses/intro.mp4", "v1")
	require.NoError(t, err)

	clk.Advance(time.Hour)
	_, err = svc.Verify(ctx, tok.Token, "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied, "expired")
}

// TestIssueValidation: the subject is required, the TTL bounded by the
// protection of the video (24 h for an unprotected one).
//
// covers: S-OBJ-17
func TestIssueValidation(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, newProtection(t, nil), nil, clock.NewFake(time.Now()))
	for name, req := range map[string]playback.IssueRequest{
		"no subject":    {Bucket: "lms", Key: "courses/a.mp4"},
		"control chars": {Bucket: "lms", Key: "courses/a.mp4", Subject: "a\nb"},
		"long TTL":      {Bucket: "lms", Key: "courses/a.mp4", Subject: "s", TTL: 2 * time.Hour},
		"short TTL":     {Bucket: "lms", Key: "courses/a.mp4", Subject: "s", TTL: time.Second},
		"long mark":     {Bucket: "lms", Key: "courses/a.mp4", Subject: "s", Watermark: strings.Repeat("x", 201)},
	} {
		_, err := svc.Issue(ctx, req)
		require.ErrorIs(t, err, domain.ErrInvalidArgument, name)
	}
	_, err := svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Subject: "s"})
	require.ErrorIs(t, err, domain.ErrInvalidObjectName, "no key")
	_, err = svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "public/a.mp4", Subject: "s", TTL: 20 * time.Hour})
	require.NoError(t, err, "an unprotected video takes up to 24 h")
}

// TestRevocation: a revoked token, or every token of a revoked viewer,
// stops playing on every node (after the refresh of the other node); a
// token issued after the revocation of its viewer plays.
//
// covers: S-OBJ-17
func TestRevocation(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	docs := newDocs(t)
	p := newProtection(t, nil)
	node1, node2 := newService(t, p, docs, clk), newService(t, p, docs, clk)
	issue := func(subject string) *playback.Token {
		tok, err := node1.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/a.mp4", Subject: subject})
		require.NoError(t, err)
		return tok
	}
	verify := func(svc playback.Service, tok *playback.Token) error {
		_, err := svc.Verify(ctx, tok.Token, "lms", "courses/a.mp4", "")
		return err
	}
	a, b := issue("alice"), issue("bob")
	require.NoError(t, verify(node2, a), "node2 caches the revocations")

	require.NoError(t, node1.Revoke(ctx, a.Claims.SessionID))
	require.ErrorIs(t, verify(node1, a), domain.ErrAccessDenied)
	clk.Advance(playback.DefaultRefresh)
	require.ErrorIs(t, verify(node2, a), domain.ErrAccessDenied, "node2 after its refresh")
	require.NoError(t, verify(node2, b))

	clk.Advance(time.Second)
	require.NoError(t, node2.RevokeSubject(ctx, "lms", "bob"))
	require.ErrorIs(t, verify(node2, b), domain.ErrAccessDenied)
	clk.Advance(time.Second)
	require.NoError(t, verify(node2, issue("bob")), "a token issued afterwards")

	require.ErrorIs(t, node1.Revoke(ctx, "../x"), domain.ErrInvalidArgument)
}

// TestSessions: a new stream of a viewer beyond MaxSessions replaces the
// oldest one, on every node; the streams of other viewers and buckets are
// not counted; -1 lifts the limit.
//
// covers: S-OBJ-17
func TestSessions(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	docs := newDocs(t)
	p := newProtection(t, func(p *domain.MediaProtection) { p.MaxSessions = 1 })
	node1, node2 := newService(t, p, docs, clk), newService(t, p, docs, clk)
	claims := func(bucket, subject string) *playback.Claims {
		tok, err := node1.Issue(ctx, playback.IssueRequest{Bucket: bucket, Key: "courses/a.mp4", Subject: subject})
		require.NoError(t, err)
		return &tok.Claims
	}
	key := func(svc playback.Service, c *playback.Claims) error {
		return svc.AuthorizeKey(ctx, c, p, 0, time.Minute)
	}
	first := claims("lms", "alice")
	require.NoError(t, key(node1, first))
	require.NoError(t, key(node1, first), "again")
	require.NoError(t, key(node1, claims("lms", "bob")), "another viewer")
	require.NoError(t, key(node1, claims("other", "alice")), "another bucket")

	second := claims("lms", "alice")
	require.NoError(t, key(node2, second), "the newest stream wins")
	require.ErrorIs(t, key(node2, first), domain.ErrAccessDenied)
	clk.Advance(playback.DefaultRefresh)
	require.ErrorIs(t, key(node1, first), domain.ErrAccessDenied, "on the other node")
	require.NoError(t, key(node1, second))

	unlimited := newProtection(t, func(p *domain.MediaProtection) { p.MaxSessions = -1 })
	for range 3 {
		require.NoError(t, node1.AuthorizeKey(ctx, claims("lms", "carol"), unlimited, 0, time.Minute))
	}
}

// TestPace: a stream gets the burst of periods at once, then about
// PaceFactor periods per period of playback; the periods it already got
// are free (seeking back).
//
// covers: S-OBJ-17
func TestPace(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	p := newProtection(t, func(p *domain.MediaProtection) { p.PaceFactor = 2 })
	svc := newService(t, p, nil, clk)
	tok, err := svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/a.mp4", Subject: "s"})
	require.NoError(t, err)
	c := &tok.Claims
	key := func(period int) error { return svc.AuthorizeKey(ctx, c, p, period, time.Minute) }

	for period := range playback.DefaultBurst {
		require.NoError(t, key(period))
	}
	require.ErrorIs(t, key(3), domain.ErrTooManyRequests, "a downloader stops after the burst")
	require.NoError(t, key(0), "a period already fetched")
	clk.Advance(30 * time.Second)
	require.NoError(t, key(3), "half a period played: one more at pace 2")
	require.ErrorIs(t, key(4), domain.ErrTooManyRequests)
	clk.Advance(30 * time.Second)
	require.NoError(t, key(10), "a seek forward")
	require.ErrorIs(t, key(11), domain.ErrTooManyRequests)
}
