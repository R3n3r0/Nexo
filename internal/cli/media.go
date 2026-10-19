package cli

import (
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The video rules of the media commands travel in the JSON form of the
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
		out.Rules = append(out.Rules, ar)
	}
	return out
}
