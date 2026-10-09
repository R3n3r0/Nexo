package s3

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Limits of request documents and metadata.
const (
	// maxXMLBody bounds the XML documents of requests.
	maxXMLBody = 1 << 20
	// maxCompleteBody bounds CompleteMultipartUpload: 10000 parts with
	// their checksums exceed 1 MiB.
	maxCompleteBody = 4 << 20
	// maxPolicyBody is the S3 limit of a bucket policy (20 KiB).
	maxPolicyBody = 20 << 10
	// maxUserMetadata is the S3 limit of the user metadata (2 KiB of keys
	// and values).
	maxUserMetadata = 2 << 10
	// maxListKeys caps max-keys, max-uploads and max-parts.
	maxListKeys = 1000
	// maxDeleteObjects is the S3 limit of a DeleteObjects request.
	maxDeleteObjects = 1000
	// minPartNumber and maxPartNumber bound the part numbers.
	minPartNumber = 1
	maxPartNumber = 10000
)

// s3TimeFormat is the timestamp format of S3 XML documents.
const s3TimeFormat = "2006-01-02T15:04:05.000Z"

// Owner reported for anonymous requests.
const (
	defaultOwnerID          = "nexo"
	defaultOwnerDisplayName = "nexo"
)

// Metadata directives of CopyObject (x-amz-metadata-directive and
// x-amz-tagging-directive).
const (
	directiveCopy    = "COPY"
	directiveReplace = "REPLACE"
)

// Headers of the force-delete extension of DeleteBucket.
const (
	headerForceDelete       = "x-nexo-force-delete"
	headerForceDeleteLegacy = "x-amz-force-delete"
)

// standard header names stored with an object.
const (
	headerCacheControl       = "Cache-Control"
	headerContentDisposition = "Content-Disposition"
	headerContentEncoding    = "Content-Encoding"
	headerContentLanguage    = "Content-Language"
	headerContentType        = "Content-Type"
	headerContentMD5         = "Content-MD5"
	headerExpires            = "Expires"
)

// awsChunkedEncoding is the content coding of the streaming SigV4 payloads.
const awsChunkedEncoding = "aws-chunked"

// defaultContentType is reported for objects stored without a type.
const defaultContentType = "application/octet-stream"

// defaultOwner is the owner reported when the request has no named identity.
func defaultOwner() Owner {
	return Owner{ID: defaultOwnerID, DisplayName: defaultOwnerDisplayName}
}

// identityOf returns the identity of the request (anonymous when no
// authentication middleware ran).
func identityOf(r *http.Request) *domain.Identity {
	if id, ok := middleware.IdentityFromContext(r.Context()); ok {
		return id
	}
	return domain.Anonymous()
}

// requireSignedCopySource is a defense in depth against copy-source
// injection through presigned URLs: a presigned request copies only when
// its signature covers x-amz-copy-source, otherwise whoever holds an
// upload link could read anything the signer can read. The verifier
// already rejects unsigned x-amz-* headers; this check keeps the copy
// handlers safe should the request reach them by another path.
func requireSignedCopySource(r *http.Request) error {
	id := identityOf(r)
	if id.AuthType != domain.AuthTypeQuery || slices.Contains(id.SignedHeaders, strings.ToLower(httpx.HeaderAmzCopySource)) {
		return nil
	}
	return domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "There were headers present in the request which were not signed")
}

func ownerOf(r *http.Request) Owner {
	if id := identityOf(r); !id.IsAnonymous {
		if user := id.EffectiveUser(); user != "" {
			return Owner{ID: user, DisplayName: user}
		}
	}
	return defaultOwner()
}

func s3Time(t time.Time) string { return t.UTC().Format(s3TimeFormat) }

func quoteETag(etag string) string {
	if etag == "" {
		return ""
	}
	if strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

// trimETag removes the quotes (and a weak prefix) of a client ETag.
func trimETag(etag string) string {
	etag = strings.TrimSpace(etag)
	etag = strings.TrimPrefix(etag, "W/")
	return strings.Trim(etag, `"`)
}

func versionIDOrNull(v string) string {
	if v == "" {
		return "null"
	}
	return v
}

func storageClassOrStandard(sc string) string {
	if sc == "" {
		return domain.StorageClassStandard
	}
	return sc
}

func invalidArgument(message string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", message)
}

func invalidRequest(message string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", message)
}

func notImplemented(message string) error {
	return domain.NewS3Error(domain.ErrNotImplemented, "NotImplemented", message)
}

// queryInt reads a non-negative integer query parameter.
func queryInt(q url.Values, name string, def int) (int, error) {
	s := q.Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, invalidArgument("Argument " + name + " must be an integer between 0 and 2147483647")
	}
	return n, nil
}

// queryLimit reads max-keys/max-uploads/max-parts, capped at 1000 (the
// default when absent). The value is the one echoed in the response; the
// services take serviceLimit of it.
func queryLimit(q url.Values, name string) (int, error) {
	n, err := queryInt(q, name, maxListKeys)
	if err != nil {
		return 0, err
	}
	return min(n, maxListKeys), nil
}

// serviceLimit converts a requested limit to the listing input of the
// services, where zero means the default: an explicit 0 asks for no
// entries (domain.ListLimitNone).
func serviceLimit(n int) int {
	if n == 0 {
		return domain.ListLimitNone
	}
	return n
}

// parseConditions reads the conditional headers with the given prefix
// ("If-" for reads and writes, "x-amz-copy-source-if-" for copies).
// Unparsable dates are ignored, as HTTP requires.
func parseConditions(h http.Header, prefix string) domain.Conditions {
	c := domain.Conditions{
		IfMatch:     h.Get(prefix + "Match"),
		IfNoneMatch: h.Get(prefix + "None-Match"),
	}
	if v := h.Get(prefix + "Modified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			c.IfModifiedSince = t
		}
	}
	if v := h.Get(prefix + "Unmodified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			c.IfUnmodifiedSince = t
		}
	}
	return c
}

// parseRange reads the Range header. As in S3 (and RFC 9110), a Range the
// server does not understand (bad syntax, several ranges, other units) is
// ignored and the whole object is served; an unsatisfiable range is
// reported by the object layer.
func parseRange(h http.Header) *domain.Range {
	v := h.Get("Range")
	if v == "" {
		return nil
	}
	rg, err := domain.ParseRange(v)
	if err != nil {
		return nil
	}
	return rg
}

// parseCopyRange reads x-amz-copy-source-range: a closed "bytes=first-last"
// range.
func parseCopyRange(h http.Header) (*domain.Range, error) {
	v := h.Get(httpx.HeaderAmzCopySourceRange)
	if v == "" {
		return nil, nil
	}
	rg, err := domain.ParseRange(v)
	if err != nil || rg.IsSuffix || rg.End < 0 {
		return nil, invalidArgument("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy")
	}
	return rg, nil
}

// objectMetadata are the metadata of a write: the content type, the
// standard headers and the user metadata (x-amz-meta-*).
type objectMetadata struct {
	contentType        string
	contentEncoding    string
	contentDisposition string
	contentLanguage    string
	cacheControl       string
	expires            string
	user               map[string]string
}

// metadataFromHeaders reads the metadata of a write from its headers. The
// aws-chunked coding of streaming uploads is not part of the object.
func metadataFromHeaders(h http.Header) (objectMetadata, error) {
	m := objectMetadata{
		contentType:        h.Get(headerContentType),
		contentEncoding:    stripAWSChunked(h.Get(headerContentEncoding)),
		contentDisposition: h.Get(headerContentDisposition),
		contentLanguage:    h.Get(headerContentLanguage),
		cacheControl:       h.Get(headerCacheControl),
		expires:            h.Get(headerExpires),
		user:               httpx.UserMetadata(h),
	}
	return m, checkMetadataSize(m.user)
}

// stripAWSChunked removes aws-chunked from a Content-Encoding list; a list
// without it is kept verbatim (S3 returns the stored value as sent).
func stripAWSChunked(v string) string {
	if v == "" {
		return ""
	}
	var kept []string
	stripped := false
	for _, coding := range strings.Split(v, ",") {
		coding = strings.TrimSpace(coding)
		switch {
		case strings.EqualFold(coding, awsChunkedEncoding):
			stripped = true
		case coding != "":
			kept = append(kept, coding)
		}
	}
	if !stripped {
		return v
	}
	return strings.Join(kept, ", ")
}

// checkMetadataSize enforces the 2 KiB S3 limit of the user metadata.
func checkMetadataSize(meta map[string]string) error {
	size := 0
	for k, v := range meta {
		size += len(k) + len(v)
	}
	if size > maxUserMetadata {
		return domain.NewS3Error(domain.ErrInvalidArgument, "MetadataTooLarge",
			"Your metadata headers exceed the maximum allowed metadata size of "+strconv.Itoa(maxUserMetadata)+" bytes.")
	}
	return nil
}

// parseTaggingHeader decodes the x-amz-tagging header (query encoded).
func parseTaggingHeader(h http.Header) (map[string]string, error) {
	v := h.Get(httpx.HeaderAmzTagging)
	if v == "" {
		return nil, nil
	}
	values, err := url.ParseQuery(v)
	if err != nil {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The x-amz-tagging header must be a URL-encoded query string")
	}
	tags := make(map[string]string, len(values))
	for k, vs := range values {
		if len(vs) > 1 {
			return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "Cannot provide multiple Tags with the same key")
		}
		tags[k] = vs[0]
	}
	return tags, nil
}

// decodeContentMD5 decodes the base64 Content-MD5 header ("" when absent).
func decodeContentMD5(h http.Header) ([]byte, error) {
	v := h.Get(headerContentMD5)
	if v == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(raw) != 16 {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidDigest", "The Content-MD5 you specified is not valid.")
	}
	return raw, nil
}

// contentMD5Hex converts the Content-MD5 header to hex for the services.
func contentMD5Hex(h http.Header) (string, error) {
	raw, err := decodeContentMD5(h)
	if err != nil || raw == nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// unsignedPayloadSHA256 returns the hex x-amz-content-sha256 of a request
// whose payload hash was not verified by the authentication (no SigV4
// signature): the object layer then verifies it. Signed requests have
// their payload verified while it streams (auth.Verifier).
func unsignedPayloadSHA256(r *http.Request) string {
	if auth.IsSigned(r) || auth.IsPresigned(r.URL.Query()) {
		return ""
	}
	v := strings.ToLower(r.Header.Get(httpx.HeaderAmzContentSHA256))
	if len(v) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(v); err != nil {
		return ""
	}
	return v
}

// contentLength returns the length of a write body: the declared
// Content-Length, -1 for a streamed body (Transfer-Encoding: chunked,
// HTTP/2 without length). A body of unknown length on HTTP/1 without
// chunked coding is refused (411 MissingContentLength), as is an aws-chunked
// body the authentication did not decode (it would be stored with its
// framing).
func contentLength(r *http.Request) (int64, error) {
	if decoded := r.Header.Get(httpx.HeaderAmzDecodedLength); decoded != "" {
		n, err := strconv.ParseInt(decoded, 10, 64)
		if err != nil || n < 0 || n != r.ContentLength {
			return 0, invalidRequest("aws-chunked payloads require a signed request (AWS4-HMAC-SHA256)")
		}
		return n, nil
	}
	switch {
	case r.ContentLength > 0:
		return r.ContentLength, nil
	case r.ContentLength == 0 && (r.Header.Get("Content-Length") != "" || r.ProtoMajor >= 2):
		return 0, nil
	case r.ContentLength < 0 && (isChunked(r) || r.ProtoMajor >= 2):
		return -1, nil
	default:
		return 0, domain.NewS3Error(domain.ErrInvalidArgument, "MissingContentLength", "You must provide the Content-Length HTTP header.")
	}
}

func isChunked(r *http.Request) bool {
	for _, te := range r.TransferEncoding {
		if strings.EqualFold(te, "chunked") {
			return true
		}
	}
	return false
}

// checkStorageClass accepts the storage classes Nexo stores.
func checkStorageClass(sc string) error {
	switch sc {
	case "", domain.StorageClassStandard, domain.StorageClassReducedRedundancy:
		return nil
	default:
		return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidStorageClass", "The storage class you specified is not valid")
	}
}

// checkUnsupportedHeaders refuses the request headers that ask for a
// feature Nexo does not provide (ACL grants): failing loudly beats storing
// data without the protection the client asked for. The private canned
// ACLs are accepted: access is governed by policies. The server-side
// encryption headers are read by sseFromHeaders.
func checkUnsupportedHeaders(h http.Header) error {
	switch acl := h.Get(httpx.HeaderAmzACL); acl {
	case "", "private", "bucket-owner-full-control":
	default:
		return notImplemented("Canned ACL " + acl + " is not supported: use bucket policies or anonymous access rules")
	}
	for name := range h {
		lname := strings.ToLower(name)
		switch {
		case strings.HasPrefix(lname, httpx.HeaderAmzGrantPrefix):
			return notImplemented("ACL grants are not supported: use bucket policies")
		}
	}
	return nil
}

// writeObjectHeaders sets the response headers describing an object.
// Checksums are returned only when the client asks for them
// (x-amz-checksum-mode: ENABLED) on a whole-object or whole-part read
// (whole).
func writeObjectHeaders(w http.ResponseWriter, r *http.Request, info domain.ObjectInfo, whole bool) {
	h := w.Header()
	set := func(name, v string) {
		if v != "" {
			h.Set(name, v)
		}
	}
	// Without a stored type net/http would sniff one from the body.
	contentType := info.ContentType
	if contentType == "" {
		contentType = defaultContentType
	}
	h.Set(headerContentType, contentType)
	set("ETag", quoteETag(info.ETag))
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	set(httpx.HeaderAmzVersionID, info.VersionID)
	if info.IsDeleteMarker {
		h.Set(httpx.HeaderAmzDeleteMarker, "true")
	}
	writeSSEHeaders(w, info.Encryption)
	set(headerContentEncoding, info.ContentEncoding)
	set(headerCacheControl, info.CacheControl)
	set(headerContentDisposition, info.ContentDisposition)
	set(headerContentLanguage, info.ContentLanguage)
	if !info.Expires.IsZero() {
		h.Set(headerExpires, info.Expires.UTC().Format(http.TimeFormat))
	}
	if info.StorageClass != "" && info.StorageClass != domain.StorageClassStandard {
		h.Set(httpx.HeaderAmzStorageClass, info.StorageClass)
	}
	httpx.SetUserMetadata(h, info.UserMetadata)
	if len(info.Tags) > 0 {
		h.Set(httpx.HeaderAmzTaggingCount, strconv.Itoa(len(info.Tags)))
	}
	if whole && info.Checksum.Value != "" && strings.EqualFold(r.Header.Get(httpx.HeaderAmzChecksumMode), "ENABLED") {
		writeChecksum(w, info.Checksum)
	}
	h.Set("Accept-Ranges", "bytes")
}

// responseOverride pairs a response-* query parameter with its header.
type responseOverride struct{ param, header string }

func responseOverrides() []responseOverride {
	return []responseOverride{
		{"response-content-type", headerContentType},
		{"response-content-language", headerContentLanguage},
		{"response-expires", headerExpires},
		{"response-cache-control", headerCacheControl},
		{"response-content-disposition", headerContentDisposition},
		{"response-content-encoding", headerContentEncoding},
	}
}

// checkResponseOverrides refuses the response-* parameters on anonymous
// requests: S3 honors them only on signed (header or presigned) requests.
func checkResponseOverrides(r *http.Request) error {
	if !identityOf(r).IsAnonymous {
		return nil
	}
	q := r.URL.Query()
	for _, o := range responseOverrides() {
		if q.Has(o.param) {
			return invalidRequest("Request specific response headers cannot be used for anonymous GET requests.")
		}
	}
	return nil
}

// applyResponseOverrides honors the response-* query parameters.
func applyResponseOverrides(w http.ResponseWriter, q url.Values) {
	for _, o := range responseOverrides() {
		if v := q.Get(o.param); v != "" {
			w.Header().Set(o.header, v)
		}
	}
}

// parseCopySource decodes x-amz-copy-source: "/bucket/key?versionId=v"
// (the leading slash is optional, bucket and key are URL encoded).
func parseCopySource(v string) (bucket, key, versionID string, err error) {
	v = strings.TrimPrefix(v, "/")
	if i := strings.Index(v, "?"); i >= 0 {
		q, qerr := url.ParseQuery(v[i+1:])
		if qerr != nil {
			return "", "", "", invalidArgument("Copy Source has an invalid query string")
		}
		versionID = q.Get("versionId")
		v = v[:i]
	}
	decoded, uerr := url.PathUnescape(v)
	if uerr != nil {
		return "", "", "", invalidArgument("Copy Source must be URL encoded")
	}
	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", "", invalidArgument("Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	if len(key) > domain.MaxObjectKeyLength {
		return "", "", "", domain.NewS3Error(domain.ErrInvalidObjectName, "KeyTooLongError", "Your key is too long.")
	}
	return bucket, key, versionID, nil
}

// directive reads a COPY/REPLACE directive header (COPY when absent).
func directive(h http.Header, name string) (string, error) {
	switch v := strings.ToUpper(strings.TrimSpace(h.Get(name))); v {
	case "", directiveCopy:
		return directiveCopy, nil
	case directiveReplace:
		return directiveReplace, nil
	default:
		return "", invalidArgument("Unknown " + name + " value " + v)
	}
}

// tagsToXML and tagsFromXML convert between maps and Tagging documents.
func tagsToXML(tags map[string]string) Tagging {
	var doc Tagging
	doc.TagSet.Tags = make([]Tag, 0, len(tags))
	for k, v := range tags {
		doc.TagSet.Tags = append(doc.TagSet.Tags, Tag{Key: k, Value: v})
	}
	sort.Slice(doc.TagSet.Tags, func(i, j int) bool { return doc.TagSet.Tags[i].Key < doc.TagSet.Tags[j].Key })
	return doc
}

func tagsFromXML(set TagSet) (map[string]string, error) {
	tags := make(map[string]string, len(set.Tags))
	for _, t := range set.Tags {
		if _, dup := tags[t.Key]; dup {
			return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "Cannot provide multiple Tags with the same key")
		}
		tags[t.Key] = t.Value
	}
	return tags, nil
}

// requestScheme returns the scheme the client used, for the URLs the
// responses name (no authorization depends on it): TLS to this server or
// to a trusted proxy (middleware.NewForwarded), else the X-Forwarded-Proto
// header.
func requestScheme(r *http.Request) string {
	if middleware.ClientOf(r).Secure {
		return "https"
	}
	if p := strings.ToLower(r.Header.Get("X-Forwarded-Proto")); p == "https" || p == "http" {
		return p
	}
	return "http"
}

// objectURL returns the URL of an object as addressed by the request
// (path-style or virtual-host).
func objectURL(r *http.Request, bucketName, key string) string {
	u := url.URL{Scheme: requestScheme(r), Host: r.Host, Path: "/" + bucketName + "/" + key}
	if t, ok := targetFromContext(r.Context()); ok && t.virtualHost {
		u.Path = "/" + key
	}
	return u.String()
}
