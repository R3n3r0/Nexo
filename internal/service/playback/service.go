// Package playback issues and checks the playback tokens of the videos
// (docs/proposals/video-protection.md): a token is a capability to stream
// one video, signed by the deployment, that an application hands to a
// viewer without credentials. The keys of a protected video go to a token
// only for a bounded number of simultaneous streams of its viewer and at
// the pace of playback; a token, or every token of a viewer, can be
// revoked.
package playback

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service issues and checks the playback tokens.
type Service interface {
	// Issue signs a playback token of a video. The TTL defaults to
	// domain.DefaultPlaybackTokenTTL and is bounded by the token TTL of
	// the protection of the bucket (domain.MaxPlaybackTokenTTL for a video
	// it does not protect): domain.ErrInvalidArgument beyond it, or for a
	// missing subject.
	Issue(ctx context.Context, req IssueRequest) (*Token, error)
	// Verify checks a token for a video: signature, expiry, revocation,
	// and the object it names (its version when the token names one).
	// Every failure is domain.ErrAccessDenied.
	Verify(ctx context.Context, token, bucket, key, versionID string) (*Claims, error)
	// AuthorizeKey decides whether the stream of a token gets the key of
	// a period of a protected video, periodLength being the duration of a
	// key period: domain.ErrAccessDenied when a newer stream of the viewer
	// replaced it (MaxSessions), domain.ErrTooManyRequests when it asks for
	// keys faster than PaceFactor times the playback.
	AuthorizeKey(ctx context.Context, c *Claims, p *domain.MediaProtection, period int, periodLength time.Duration) error
	// Revoke revokes the token of a session id until it expires.
	Revoke(ctx context.Context, sessionID string) error
	// RevokeSubject revokes every token of the videos of a bucket issued
	// to a viewer so far.
	RevokeSubject(ctx context.Context, bucket, subject string) error
}

// IssueRequest asks for a playback token.
type IssueRequest struct {
	Bucket string
	Key    string
	// VersionID pins the token to a version; empty follows the current
	// one.
	VersionID string
	// Subject identifies the viewer for the application (an account id):
	// the simultaneous streams are counted by subject.
	Subject string
	// Watermark is the text the player draws over the video (a name, an
	// e-mail); empty draws the subject.
	Watermark string
	// TTL is the validity of the token (0: the default).
	TTL time.Duration
}

// Token is a signed playback token.
type Token struct {
	// Token is the value of the pt query parameter of the media routes.
	Token  string
	Claims Claims
}

// Claims are the payload of a playback token (readable by the player:
// they hold nothing secret).
type Claims struct {
	Version   int    `json:"v"`
	Bucket    string `json:"b"`
	Key       string `json:"k"`
	VersionID string `json:"vid,omitempty"`
	Subject   string `json:"sub"`
	Watermark string `json:"wm,omitempty"`
	// SessionID identifies the token (its stream) for the revocations and
	// the simultaneous streams.
	SessionID string `json:"sid"`
	// IssuedAt and ExpiresAt are Unix times, in seconds.
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}
