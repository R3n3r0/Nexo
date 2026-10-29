package playback_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/playback"
)

// unwrap opens a key wrapped for a session the way the player does: the
// wrapping key derived from the ECDH secret with HKDF-SHA256, AES-GCM.
func unwrap(t *testing.T, priv *ecdh.PrivateKey, s *playback.Session, wrapped []byte, aad string) ([]byte, error) {
	t.Helper()
	server, err := ecdh.P256().NewPublicKey(s.ServerKey)
	require.NoError(t, err)
	secret, err := priv.ECDH(server)
	require.NoError(t, err)
	wrap, err := hkdf.Key(sha256.New, secret, s.Salt, "nexo/media/session-wrap-v1\x00"+s.Claims.Session, 16)
	require.NoError(t, err)
	block, err := aes.NewCipher(wrap)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return aead.Open(nil, wrapped[:12], wrapped[12:], []byte(aad))
}

// TestPlayerSessions: a session opened with a token gives a ticket every node
// verifies; the keys it wraps open only with the ECDH secret of the
// player and their additional data; a ticket is no token and a token no
// ticket; revoking the token stops its sessions.
//
// covers: S-OBJ-18
func TestPlayerSessions(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	p := newProtection(t, func(p *domain.MediaProtection) { p.Mode = domain.MediaPlaybackAdvanced })
	docs := newDocs(t)
	node1, node2 := newService(t, p, docs, clk), newService(t, p, docs, clk)

	tok, err := node1.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "s1"})
	require.NoError(t, err)
	assert.Equal(t, playback.ModeSession, tok.Claims.Mode, "the token says the video plays in a session")
	plain, err := node1.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "public/intro.mp4", Subject: "s1"})
	require.NoError(t, err)
	assert.Empty(t, plain.Claims.Mode)

	claims, err := node1.Verify(ctx, tok.Token, "lms", "courses/intro.mp4", "")
	require.NoError(t, err)
	_, err = node1.OpenSession(ctx, claims, []byte("not a key"))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := node1.OpenSession(ctx, claims, priv.PublicKey().Bytes())
	require.NoError(t, err)
	assert.True(t, playback.IsTicket(s.Ticket))
	assert.False(t, playback.IsTicket(tok.Token))
	assert.Len(t, s.Claims.Session, 32)
	assert.Equal(t, claims.SessionID, s.Claims.SessionID)

	// Another node verifies the ticket and wraps for the session.
	tc, err := node2.VerifyTicket(ctx, s.Ticket, "lms", "courses/intro.mp4", "")
	require.NoError(t, err)
	assert.Equal(t, s.Claims.Session, tc.Session)
	segmentKey := []byte("0123456789abcdef")
	wrapped, err := node2.WrapKey(tc, segmentKey, "r=720p;i=3")
	require.NoError(t, err)
	got, err := unwrap(t, priv, s, wrapped, "r=720p;i=3")
	require.NoError(t, err)
	assert.Equal(t, segmentKey, got)
	_, err = unwrap(t, priv, s, wrapped, "r=720p;i=4")
	require.Error(t, err, "bound to its additional data")
	other, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, err = unwrap(t, other, s, wrapped, "r=720p;i=3")
	require.Error(t, err, "only the player that opened the session unwraps")

	// A token wraps nothing; a ticket is no token; a tampered ticket, one
	// of another video, or of another deployment is refused.
	_, err = node2.WrapKey(claims, segmentKey, "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = node2.Verify(ctx, s.Ticket, "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = node2.VerifyTicket(ctx, tok.Token, "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = node2.VerifyTicket(ctx, s.Ticket+"x", "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = node2.VerifyTicket(ctx, s.Ticket, "lms", "courses/other.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	foreign, err := playback.New(playback.Deps{Protections: protections{*p}, Clock: clk}, playback.WithKey([]byte("another deployment key, 32 bytes")))
	require.NoError(t, err)
	_, err = foreign.VerifyTicket(ctx, s.Ticket, "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)

	// Alive until revoked or expired.
	require.NoError(t, node2.Alive(ctx, tc, p))
	require.NoError(t, node1.Revoke(ctx, claims.SessionID))
	clk.Advance(playback.DefaultRefresh + time.Second)
	_, err = node2.VerifyTicket(ctx, s.Ticket, "lms", "courses/intro.mp4", "")
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	require.ErrorIs(t, node2.Alive(ctx, tc, p), domain.ErrAccessDenied)
}

// TestAliveEvicted: a stream a newer one of its viewer replaced is no
// longer alive.
//
// covers: S-OBJ-18
func TestAliveEvicted(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	p := newProtection(t, func(p *domain.MediaProtection) { p.MaxSessions = 1 })
	svc := newService(t, p, newDocs(t), clk)
	claims := func() *playback.Claims {
		tok, err := svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "s1"})
		require.NoError(t, err)
		return &tok.Claims
	}
	first, second := claims(), claims()
	require.NoError(t, svc.AuthorizeKey(ctx, first, p, 0, time.Minute))
	require.NoError(t, svc.Alive(ctx, first, p))
	require.NoError(t, svc.AuthorizeKey(ctx, second, p, 0, time.Minute))
	require.ErrorIs(t, svc.Alive(ctx, first, p), domain.ErrAccessDenied)
	require.NoError(t, svc.Alive(ctx, second, p))
	clk.Advance(2 * time.Hour)
	require.ErrorIs(t, svc.Alive(ctx, second, p), domain.ErrAccessDenied, "expired")
}

// traces records the trace records, failing on demand.
type traces struct {
	records []domain.MediaTrace
	err     error
}

func (f *traces) RecordTrace(_ context.Context, t domain.MediaTrace) error {
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, t)
	return nil
}

// TestIssueForensic: a token of a video marked by the forensic watermark
// is issued only with its trace record; the other videos need none.
//
// covers: S-OBJ-19
func TestIssueForensic(t *testing.T) {
	ctx := context.Background()
	p := newProtection(t, func(p *domain.MediaProtection) { p.Forensic = true })
	rec := &traces{}
	svc, err := playback.New(playback.Deps{Protections: protections{*p}, Traces: rec}, playback.WithKey(testKey))
	require.NoError(t, err)
	tok, err := svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "s1", Watermark: "Mario"})
	require.NoError(t, err)
	require.Len(t, rec.records, 1)
	assert.Equal(t, domain.MediaTrace{Bucket: "lms", Key: "courses/intro.mp4", SessionID: tok.Claims.SessionID, Subject: "s1",
		Watermark: "Mario", IssuedAt: tok.Claims.IssuedAt, ExpiresAt: tok.Claims.ExpiresAt}, rec.records[0])

	_, err = svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "public/intro.mp4", Subject: "s1"})
	require.NoError(t, err)
	assert.Len(t, rec.records, 1, "an unprotected video has no record")

	rec.err = domain.ErrWriteQuorum
	_, err = svc.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "s1"})
	require.ErrorIs(t, err, domain.ErrWriteQuorum, "no token without its record")
	none, err := playback.New(playback.Deps{Protections: protections{*p}}, playback.WithKey(testKey))
	require.NoError(t, err)
	_, err = none.Issue(ctx, playback.IssueRequest{Bucket: "lms", Key: "courses/intro.mp4", Subject: "s1"})
	require.ErrorIs(t, err, domain.ErrServiceUnavailable)
}
