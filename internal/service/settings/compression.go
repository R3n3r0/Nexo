package settings

import (
	"mime"
	"strings"
)

// Compression are the settings of the transparent compression (S-OBJ-14):
// when enabled, a new version is compressed when its key ends with one of
// Extensions or its content type matches one of MimeTypes ("text/*"
// matches every text type); with both lists empty, every version is. The
// formats already compressed (archives, images, audio, video) never are.
type Compression struct {
	Enabled    bool
	Extensions []string
	MimeTypes  []string
}

// Selects reports whether a version of key with contentType is to be
// compressed.
func (c Compression) Selects(key, contentType string) bool {
	if !c.Enabled {
		return false
	}
	key = strings.ToLower(key)
	ct := strings.ToLower(contentType)
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	if incompressible(key, ct) {
		return false
	}
	if len(c.Extensions) == 0 && len(c.MimeTypes) == 0 {
		return true
	}
	for _, ext := range c.Extensions {
		if ext = strings.ToLower(strings.TrimSpace(ext)); ext != "" && strings.HasSuffix(key, ext) {
			return true
		}
	}
	for _, pattern := range c.MimeTypes {
		if matchMime(strings.ToLower(strings.TrimSpace(pattern)), ct) {
			return true
		}
	}
	return false
}

// matchMime matches a content type against a pattern: a type, or a
// family ("text/*").
func matchMime(pattern, ct string) bool {
	if pattern == "" || ct == "" {
		return false
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(ct, prefix)
	}
	return pattern == ct
}

// incompressible reports the formats that are compressed already.
func incompressible(key, ct string) bool {
	for _, ext := range []string{
		".gz", ".tgz", ".bz2", ".xz", ".zst", ".lz4", ".br", ".zip", ".7z", ".rar", ".s2", ".snappy",
		".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".heic", ".mp3", ".aac", ".ogg", ".opus", ".flac",
		".mp4", ".m4a", ".m4v", ".mov", ".mkv", ".webm", ".avi",
	} {
		if strings.HasSuffix(key, ext) {
			return true
		}
	}
	for _, family := range []string{"image/", "audio/", "video/"} {
		if strings.HasPrefix(ct, family) {
			return true
		}
	}
	switch ct {
	case "application/zip", "application/gzip", "application/x-gzip", "application/x-bzip2", "application/x-xz",
		"application/zstd", "application/x-7z-compressed", "application/x-rar-compressed", "application/x-compress":
		return true
	}
	return false
}
