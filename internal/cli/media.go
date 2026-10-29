package cli

import (
	"slices"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The video and image rules of the media commands travel in the JSON form of the
// Admin API ({"rules": [{"id": ..., "prefix": ..., "renditions": [...]}]}),
// the one of "media get" and of the console.

// ParseMedia parses the video rules of a bucket (JSON) and validates them
// as the server does (domain.MediaConfig): the result is normalized like
// the configuration the server stores (default suffixes, renditions
// tallest first, thumbnail interval).
func ParseMedia(data []byte) (nexoadmin.MediaConfiguration, error) {
	var cfg nexoadmin.MediaConfiguration
	if err := decodeStrictJSON(data, &cfg); err != nil {
		return nexoadmin.MediaConfiguration{}, UsageError("invalid media JSON: %v", err)
	}
	if len(cfg.Rules) == 0 {
		return nexoadmin.MediaConfiguration{}, UsageError("the media configuration has no rule (use \"media rm\" to remove it)")
	}
	return NormalizeMedia(cfg)
}

// NormalizeMedia validates video rules as the server does and returns them
// normalized; an invalid configuration is a usage error.
func NormalizeMedia(cfg nexoadmin.MediaConfiguration) (nexoadmin.MediaConfiguration, error) {
	d := MediaDomain(cfg)
	if err := d.Validate(); err != nil {
		return nexoadmin.MediaConfiguration{}, UsageError("invalid media configuration: %s",
			strings.TrimPrefix(err.Error(), domain.ErrInvalidArgument.Error()+": "))
	}
	return MediaFromDomain(d), nil
}

// MediaDomain converts video rules to the domain type (shared with the
// server validation); the result shares nothing with cfg.
func MediaDomain(cfg nexoadmin.MediaConfiguration) domain.MediaConfig {
	out := domain.MediaConfig{Rules: make([]domain.MediaRule, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		dr := domain.MediaRule{ID: r.ID, Prefix: r.Prefix, Suffixes: r.Suffixes, Renditions: r.Renditions, Poster: r.Poster}
		if r.Thumbnails != nil {
			dr.Thumbnails = &domain.MediaThumbnails{Interval: r.Thumbnails.Interval}
		}
		if r.Images != nil {
			dr.Images = &domain.MediaImages{Formats: r.Images.Formats, Widths: r.Images.Widths}
		}
		if r.Subtitles != nil {
			dr.Subtitles = &domain.MediaSubtitles{Source: r.Subtitles.Source, Languages: r.Subtitles.Languages}
		}
		out.Rules = append(out.Rules, dr.Clone())
	}
	return out
}

// MediaFromDomain converts video rules of the domain to the Admin API
// form.
func MediaFromDomain(cfg domain.MediaConfig) nexoadmin.MediaConfiguration {
	out := nexoadmin.MediaConfiguration{Rules: make([]nexoadmin.MediaRule, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		r = r.Clone()
		ar := nexoadmin.MediaRule{ID: r.ID, Prefix: r.Prefix, Suffixes: r.Suffixes, Renditions: r.Renditions, Poster: r.Poster}
		if r.Thumbnails != nil {
			ar.Thumbnails = &nexoadmin.MediaThumbnails{Interval: r.Thumbnails.Interval}
		}
		if r.Images != nil {
			ar.Images = &nexoadmin.MediaImages{Formats: r.Images.Formats, Widths: r.Images.Widths}
		}
		if r.Subtitles != nil {
			ar.Subtitles = &nexoadmin.MediaSubtitles{Source: r.Subtitles.Source, Languages: r.Subtitles.Languages}
		}
		out.Rules = append(out.Rules, ar)
	}
	return out
}

// MediaProtectionDomain converts a protection of the Admin API to the
// domain form (Watermark is the inverse of NoWatermark).
func MediaProtectionDomain(p nexoadmin.MediaProtection) domain.MediaProtection {
	return domain.MediaProtection{Enabled: p.Enabled, Prefix: p.Prefix, Suffixes: slices.Clone(p.Suffixes),
		TokenTTLSeconds: p.TokenTTLSeconds, MaxSessions: p.MaxSessions, PaceFactor: p.PaceFactor, NoWatermark: !p.Watermark,
		AllowedOrigins: slices.Clone(p.AllowedOrigins), ShareLinks: p.ShareLinks,
		Mode: p.Mode, LegacyDevices: p.LegacyDevices, Forensic: p.Forensic}
}

// MediaProtectionFromDomain converts a domain protection to the form of
// the Admin API.
func MediaProtectionFromDomain(p domain.MediaProtection) nexoadmin.MediaProtection {
	origins := slices.Clone(p.AllowedOrigins)
	if origins == nil {
		origins = []string{}
	}
	return nexoadmin.MediaProtection{Enabled: p.Enabled, Prefix: p.Prefix, Suffixes: slices.Clone(p.Suffixes),
		TokenTTLSeconds: p.TokenTTLSeconds, MaxSessions: p.MaxSessions, PaceFactor: p.PaceFactor, Watermark: !p.NoWatermark,
		AllowedOrigins: origins, ShareLinks: p.ShareLinks,
		Mode: p.Mode, LegacyDevices: p.LegacyDevices, Forensic: p.Forensic}
}
