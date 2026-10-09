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

	HeaderAmzChecksumPrefix        = "x-amz-checksum-"
	HeaderAmzChecksumAlgorithm     = "x-amz-checksum-algorithm"
	HeaderAmzChecksumMode          = "x-amz-checksum-mode"
	HeaderAmzSDKChecksumAlgorithm  = "x-amz-sdk-checksum-algorithm"
	HeaderAmzTrailer               = "x-amz-trailer"
	HeaderAmzACL                   = "x-amz-acl"
	HeaderAmzGrantPrefix           = "x-amz-grant-"
	HeaderAmzSSE                   = "x-amz-server-side-encryption"
	HeaderAmzSSECustomerAlgorithm  = "x-amz-server-side-encryption-customer-algorithm"
	HeaderAmzSSEKMSKeyID           = "x-amz-server-side-encryption-aws-kms-key-id"
	HeaderAmzObjectLockMode        = "x-amz-object-lock-mode"
	HeaderAmzObjectLockRetainUntil = "x-amz-object-lock-retain-until-date"
	HeaderAmzObjectLockLegalHold   = "x-amz-object-lock-legal-hold"
	HeaderAmzBucketObjectLock      = "x-amz-bucket-object-lock-enabled"
	HeaderAmzBypassGovernance      = "x-amz-bypass-governance-retention"
	HeaderAmzMetadataDirective     = "x-amz-metadata-directive"
	HeaderAmzTaggingDirective      = "x-amz-tagging-directive"
	HeaderAmzTaggingCount          = "x-amz-tagging-count"
	HeaderAmzMPPartsCount          = "x-amz-mp-parts-count"
	HeaderAmzCopySourceRange       = "x-amz-copy-source-range"
	HeaderAmzCopySourceVersionID   = "x-amz-copy-source-version-id"
	HeaderAmzCopySourceIfPrefix    = "x-amz-copy-source-if-"
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

// SetUserMetadata writes user metadata as x-amz-meta-* headers. The names
// are sent as S3 sends them, lowercase and not canonicalized
// ("x-amz-meta-foo", not "X-Amz-Meta-Foo"): the SDKs derive the metadata
// keys from the header names as received.
func SetUserMetadata(h http.Header, meta map[string]string) {
	for k, v := range meta {
		name := HeaderAmzMetaPrefix + strings.ToLower(k)
		h.Del(name)
		h[name] = []string{v}
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
