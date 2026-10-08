package httpx

import (
	"net/http"
	"strings"
)

// Well-known x-amz-* headers.
const (
	HeaderAmzMetaPrefix     = "x-amz-meta-"
	HeaderAmzDate           = "x-amz-date"
	HeaderAmzContentSHA256  = "x-amz-content-sha256"
	HeaderAmzSecurityToken  = "x-amz-security-token"
	HeaderAmzVersionID      = "x-amz-version-id"
	HeaderAmzDeleteMarker   = "x-amz-delete-marker"
	HeaderAmzCopySource     = "x-amz-copy-source"
	HeaderAmzStorageClass   = "x-amz-storage-class"
	HeaderAmzTagging        = "x-amz-tagging"
	HeaderAmzBucketRegion   = "x-amz-bucket-region"
	HeaderAmzChecksumSHA256 = "x-amz-checksum-sha256"
	HeaderAmzDecodedLength  = "x-amz-decoded-content-length"
)

// AmzHeader returns the value of a case-insensitive x-amz-* header.
func AmzHeader(h http.Header, name string) string {
	return h.Get(name)
}

// HasAmzHeader reports whether the header is present (even if empty).
func HasAmzHeader(h http.Header, name string) bool {
	_, ok := h[http.CanonicalHeaderKey(name)]
	return ok
}

// UserMetadata extracts the x-amz-meta-* headers, returning a map keyed by
// the lowercase suffix after the prefix. Multiple values are joined with ",".
func UserMetadata(h http.Header) map[string]string {
	out := make(map[string]string)
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, HeaderAmzMetaPrefix) && len(lk) > len(HeaderAmzMetaPrefix) {
			out[lk[len(HeaderAmzMetaPrefix):]] = strings.Join(vs, ",")
		}
	}
	return out
}

// SetUserMetadata writes user metadata as x-amz-meta-* headers.
func SetUserMetadata(h http.Header, meta map[string]string) {
	for k, v := range meta {
		h.Set(HeaderAmzMetaPrefix+k, v)
	}
}

// AmzHeaders returns every x-amz-* header (lowercase names), useful for
// audit logs and signing.
func AmzHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for k, vs := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			out[lk] = strings.Join(vs, ",")
		}
	}
	return out
}
