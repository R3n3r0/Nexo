package clitest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The download protection of the videos and their playback tokens
// (nexoctl media protection, token, revoke): the protections are
// validated and normalized as the server does; the tokens issued and the
// revocations are recorded (PlaybackTokens, PlaybackRevocations).

// memPlaybackState is the playback state of MemAdmin.
type memPlaybackState struct {
	protections map[string]domain.MediaProtection
	tokens      []PlaybackTokenIssued
	revocations []PlaybackRevocation
}

// PlaybackTokenIssued is a playback token issued by MemAdmin.
type PlaybackTokenIssued struct {
	Bucket, Key, VersionID string
	Request                nexoadmin.PlaybackTokenRequest
}

// PlaybackRevocation is a revocation received by MemAdmin: of a session
// or of a subject.
type PlaybackRevocation struct {
	Bucket, Session, Subject string
}

func (m *MemAdmin) playbackLocked() *memPlaybackState {
	if m.playback.protections == nil {
		m.playback.protections = map[string]domain.MediaProtection{}
	}
	return &m.playback
}

// PlaybackTokens returns the tokens issued.
func (m *MemAdmin) PlaybackTokens() []PlaybackTokenIssued {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]PlaybackTokenIssued(nil), m.playbackLocked().tokens...)
}

// PlaybackRevocations returns the revocations received.
func (m *MemAdmin) PlaybackRevocations() []PlaybackRevocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]PlaybackRevocation(nil), m.playbackLocked().revocations...)
}

// expectPlayback registers the protection and token methods on c.
func (s *memSession) expectPlayback(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().GetMediaProtection(x, x).RunAndReturn(s.getMediaProtection).Maybe()
	c.EXPECT().SetMediaProtection(x, x, x).RunAndReturn(s.setMediaProtection).Maybe()
	c.EXPECT().IssuePlaybackToken(x, x, x, x, x).RunAndReturn(s.issuePlaybackToken).Maybe()
	c.EXPECT().RevokePlaybackSession(x, x, x).RunAndReturn(s.revokePlaybackSession).Maybe()
	c.EXPECT().RevokePlaybackSubject(x, x, x).RunAndReturn(s.revokePlaybackSubject).Maybe()
	c.EXPECT().TracePlaybackLeak(x, x, x, x, x).RunAndReturn(s.tracePlaybackLeak).Maybe()
}

// tracePlaybackLeak answers with the tokens issued so far for the video
// whose fake code (the session number in binary, bit 0 first) agrees with
// the bits: given, or read from the votes (one per second: segment n is
// the second n, positive for 0).
func (s *memSession) tracePlaybackLeak(_ context.Context, bucket, key, _ string, req nexoadmin.PlaybackTraceRequest) (*nexoadmin.PlaybackTrace, error) {
	if err := s.begin("TracePlaybackLeak"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:TracePlaybackLeaks"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	bits := req.Bits
	if bits == "" {
		var b strings.Builder
		for _, v := range req.Votes {
			if v.S >= 0 {
				b.WriteByte('0')
			} else {
				b.WriteByte('1')
			}
		}
		bits = b.String()
	}
	if _, _, err := domain.ParseMediaTraceBits(bits); err != nil {
		return nil, err
	}
	out := &nexoadmin.PlaybackTrace{Bits: bits, Matches: []nexoadmin.PlaybackTraceMatch{}}
	for i, tok := range s.m.playbackLocked().tokens {
		if tok.Bucket != bucket || tok.Key != key {
			continue
		}
		code := uint64(i + 1)
		m := nexoadmin.PlaybackTraceMatch{SessionID: fmt.Sprintf("%032x", i+1), Subject: tok.Request.Subject, Watermark: tok.Request.Watermark, Key: tok.Key,
			IssuedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC), Code: fmt.Sprintf("%016x", code)}
		for j, c := range bits {
			if c == '?' {
				continue
			}
			m.Compared++
			if (c == '1') == (code>>uint(j)&1 == 1) {
				m.Agree++
			}
		}
		if m.Compared > 0 && m.Agree == m.Compared {
			out.Matches = append(out.Matches, m)
		}
	}
	return out, nil
}

func (s *memSession) getMediaProtection(_ context.Context, bucket string) (*nexoadmin.MediaProtection, error) {
	if err := s.begin("GetMediaProtection"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:GetBucketMedia"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	p := s.m.playbackLocked().protections[bucket]
	_ = p.Validate()
	out := cli.MediaProtectionFromDomain(p)
	return &out, nil
}

func (s *memSession) setMediaProtection(_ context.Context, bucket string, in nexoadmin.MediaProtection) (*nexoadmin.MediaProtection, error) {
	if err := s.begin("SetMediaProtection"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:SetBucketMedia"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	p := cli.MediaProtectionDomain(in)
	if err := p.Validate(); err != nil {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidConfiguration, err.Error())
	}
	s.m.playbackLocked().protections[bucket] = p.Clone()
	out := cli.MediaProtectionFromDomain(p)
	return &out, nil
}

func (s *memSession) issuePlaybackToken(_ context.Context, bucket, key, versionID string, req nexoadmin.PlaybackTokenRequest) (*nexoadmin.PlaybackToken, error) {
	if err := s.begin("IssuePlaybackToken"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("nexo:IssuePlaybackToken"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if req.Subject == "" {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidArgument, "playback token: the subject is required")
	}
	st := s.m.playbackLocked()
	st.tokens = append(st.tokens, PlaybackTokenIssued{Bucket: bucket, Key: key, VersionID: versionID, Request: req})
	n := len(st.tokens)
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl == 0 {
		ttl = domain.DefaultPlaybackTokenTTL
	}
	token := fmt.Sprintf("tok%d", n)
	playlist := domain.MediaMasterPath(bucket) + "?" + url.Values{"key": {key}, "pt": {token}}.Encode()
	return &nexoadmin.PlaybackToken{Token: token, SessionID: fmt.Sprintf("%032x", n),
		ExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(ttl),
		Playlist:  MemAdminEndpoint + playlist, Player: MemAdminEndpoint + domain.MediaPlayerPath + "#" + playlist}, nil
}

func (s *memSession) revokePlaybackSession(_ context.Context, bucket, session string) error {
	if err := s.begin("RevokePlaybackSession"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:RevokePlaybackTokens"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.m.playbackLocked()
	st.revocations = append(st.revocations, PlaybackRevocation{Bucket: bucket, Session: session})
	return nil
}

func (s *memSession) revokePlaybackSubject(_ context.Context, bucket, subject string) error {
	if err := s.begin("RevokePlaybackSubject"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:RevokePlaybackTokens"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.m.playbackLocked()
	st.revocations = append(st.revocations, PlaybackRevocation{Bucket: bucket, Subject: subject})
	return nil
}
