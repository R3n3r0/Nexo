package domain

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Defaults and bounds of MediaProtection.
const (
	DefaultPlaybackTokenTTL = 2 * time.Hour
	MaxPlaybackTokenTTL     = 24 * time.Hour
	DefaultMediaSessions    = 2
	maxMediaSessions        = 100
	DefaultMediaPaceFactor  = 2
	maxMediaPaceFactor      = 20
	maxMediaOrigins         = 32
)

// MediaProtection is the download protection of the videos of a bucket
// (docs/proposals/video-protection.md): the HLS segments of a protected
// video are encrypted (AES-128), their keys are served only to a playback
// token of the video (or to someone allowed to read it), at the pace of
// playback and to a bounded number of simultaneous streams per viewer, and
// the original cannot be downloaded without nexo:DownloadProtectedMedia.
type MediaProtection struct {
	Enabled bool `json:"enabled"`
	// Prefix limits the protection to the keys under it (empty: the whole
	// bucket).
	Prefix string `json:"prefix,omitempty"`
	// Suffixes of the protected videos, matched without case (default
	// DefaultMediaSuffixes).
	Suffixes []string `json:"suffixes,omitempty"`
	// TokenTTLSeconds is the longest validity of a playback token
	// (default DefaultPlaybackTokenTTL, at most MaxPlaybackTokenTTL).
	TokenTTLSeconds int `json:"tokenTtlSeconds,omitempty"`
	// MaxSessions is the number of simultaneous streams of a viewer
	// (default DefaultMediaSessions; -1 for no limit).
	MaxSessions int `json:"maxSessions,omitempty"`
	// PaceFactor bounds the keys a stream fetches to this many times the
	// pace of playback, plus a burst (default DefaultMediaPaceFactor).
	PaceFactor int `json:"paceFactor,omitempty"`
	// NoWatermark disables the watermark of the viewer in the player.
	NoWatermark bool `json:"noWatermark,omitempty"`
	// AllowedOrigins are the web origins ("https://learn.example.com")
	// allowed to embed the player and fetch the keys; empty allows any.
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
	// ShareLinks lets the stream share links (managed shares, presigned
	// playlists) play the protected videos, without watermark nor limits.
	ShareLinks bool `json:"shareLinks,omitempty"`
}

// Validate checks the protection and fills its defaults.
func (p *MediaProtection) Validate() error {
	if len(p.Prefix) > 1024 {
		return fmt.Errorf("%w: media protection: the prefix is longer than 1024 bytes", ErrInvalidArgument)
	}
	if IsMediaSidecar(p.Prefix) {
		return fmt.Errorf("%w: media protection: the prefix %s is reserved", ErrInvalidArgument, MediaSidecarPrefix)
	}
	if len(p.Suffixes) == 0 {
		p.Suffixes = DefaultMediaSuffixes()
	}
	for i, s := range p.Suffixes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || len(s) > 32 {
			return fmt.Errorf("%w: media protection: invalid suffix %q", ErrInvalidArgument, p.Suffixes[i])
		}
		p.Suffixes[i] = s
	}
	switch {
	case p.TokenTTLSeconds == 0:
		p.TokenTTLSeconds = int(DefaultPlaybackTokenTTL / time.Second)
	case p.TokenTTLSeconds < 60 || p.TokenTTLSeconds > int(MaxPlaybackTokenTTL/time.Second):
		return fmt.Errorf("%w: media protection: the token TTL must be 60 to %d seconds", ErrInvalidArgument, int(MaxPlaybackTokenTTL/time.Second))
	}
	switch {
	case p.MaxSessions == 0:
		p.MaxSessions = DefaultMediaSessions
	case p.MaxSessions < -1 || p.MaxSessions > maxMediaSessions:
		return fmt.Errorf("%w: media protection: maxSessions must be 1 to %d, or -1 for no limit", ErrInvalidArgument, maxMediaSessions)
	}
	switch {
	case p.PaceFactor == 0:
		p.PaceFactor = DefaultMediaPaceFactor
	case p.PaceFactor < 1 || p.PaceFactor > maxMediaPaceFactor:
		return fmt.Errorf("%w: media protection: paceFactor must be 1 to %d", ErrInvalidArgument, maxMediaPaceFactor)
	}
	if len(p.AllowedOrigins) > maxMediaOrigins {
		return fmt.Errorf("%w: media protection: at most %d allowed origins", ErrInvalidArgument, maxMediaOrigins)
	}
	for i, o := range p.AllowedOrigins {
		norm, err := normalizeOrigin(o)
		if err != nil {
			return err
		}
		p.AllowedOrigins[i] = norm
	}
	return nil
}

// normalizeOrigin checks a web origin (scheme://host[:port], no path) and
// lowers it.
func normalizeOrigin(o string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(o))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("%w: media protection: %q is not an origin (https://host[:port])", ErrInvalidArgument, o)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), nil
}

// TokenTTL is the longest validity of a playback token.
func (p *MediaProtection) TokenTTL() time.Duration {
	if p.TokenTTLSeconds <= 0 {
		return DefaultPlaybackTokenTTL
	}
	return time.Duration(p.TokenTTLSeconds) * time.Second
}

// Protects reports whether key is a protected video: a key under the
// prefix with a protected suffix, or a rendition the media worker derived
// from one (the renditions are videos too; the poster, the thumbnails and
// the subtitles are not protected).
func (p *MediaProtection) Protects(key string) bool {
	if p == nil || !p.Enabled {
		return false
	}
	if source, file, ok := MediaSidecarSource(key); ok {
		return strings.HasPrefix(file, MediaRenditionsDir) && p.Protects(source)
	}
	if !strings.HasPrefix(key, p.Prefix) {
		return false
	}
	suffixes := p.Suffixes
	if len(suffixes) == 0 {
		suffixes = DefaultMediaSuffixes()
	}
	lower := strings.ToLower(key)
	for _, s := range suffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

// AllowsOrigin reports whether a request from origin may embed the player
// and fetch the keys: no origin (a native player) or one of
// AllowedOrigins, any when none is set.
func (p *MediaProtection) AllowsOrigin(origin string) bool {
	if origin == "" || len(p.AllowedOrigins) == 0 {
		return true
	}
	origin = strings.ToLower(origin)
	for _, o := range p.AllowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// Clone returns a deep copy of p.
func (p MediaProtection) Clone() MediaProtection {
	p.Suffixes = append([]string(nil), p.Suffixes...)
	p.AllowedOrigins = append([]string(nil), p.AllowedOrigins...)
	return p
}
