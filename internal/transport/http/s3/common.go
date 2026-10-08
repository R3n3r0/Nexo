package s3

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Limits of request bodies.
const (
	maxXMLBody    = 1 << 20
	maxPolicyBody = 20 << 10
)

// s3TimeFormat is the timestamp format of S3 XML documents.
const s3TimeFormat = "2006-01-02T15:04:05.000Z"

// defaultOwner is the single owner of every bucket and object.
var defaultOwner = Owner{ID: "nexo", DisplayName: "nexo"}

func ownerOf(r *http.Request) Owner {
	if id, ok := middleware.IdentityFromContext(r.Context()); ok && id != nil && !id.IsAnonymous {
		if user := id.EffectiveUser(); user != "" {
			return Owner{ID: user, DisplayName: user}
		}
	}
	return defaultOwner
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

func versionIDOrNull(v string) string {
	if v == "" {
		return "null"
	}
	return v
}

func queryInt(q url.Values, name string, def int) (int, error) {
	s := q.Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Argument "+name+" must be a non-negative integer")
	}
	return n, nil
}

// parseConditions reads the conditional headers with the given prefix
// ("If-" for reads and writes, "x-amz-copy-source-if-" for copies).
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

// parseRange reads the Range header; an absent header yields nil.
func parseRange(h http.Header) (*domain.Range, error) {
	v := h.Get("Range")
	if v == "" {
		return nil, nil
	}
	return domain.ParseRange(v)
}

// standardMetadataHeaders are stored with the object alongside x-amz-meta-*.
var standardMetadataHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// userMetadata collects x-amz-meta-* and the standard metadata headers.
func userMetadata(h http.Header) map[string]string {
	meta := httpx.UserMetadata(h)
	for _, name := range standardMetadataHeaders {
		if v := h.Get(name); v != "" {
			meta[strings.ToLower(name)] = v
		}
	}
	return meta
}

// parseTaggingHeader decodes the x-amz-tagging header (query encoded).
func parseTaggingHeader(h http.Header) (map[string]string, error) {
	v := h.Get(httpx.HeaderAmzTagging)
	if v == "" {
		return nil, nil
	}
	values, err := url.ParseQuery(v)
	if err != nil {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The x-amz-tagging header is not a valid query string")
	}
	tags := make(map[string]string, len(values))
	for k, vs := range values {
		if len(vs) > 0 {
			tags[k] = vs[0]
		}
	}
	return tags, nil
}

// contentMD5Hex converts the base64 Content-MD5 header to hex.
func contentMD5Hex(h http.Header) (string, error) {
	v := h.Get("Content-MD5")
	if v == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(raw) != 16 {
		return "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidDigest", "The Content-MD5 you specified is not valid.")
	}
	return hex.EncodeToString(raw), nil
}

// checksumFromHeaders reads the x-amz-checksum-* header or the algorithm
// announced for a trailing checksum.
func checksumFromHeaders(h http.Header) (domain.Checksum, error) {
	var found domain.Checksum
	for _, algo := range []domain.ChecksumAlgorithm{domain.ChecksumCRC32, domain.ChecksumCRC32C, domain.ChecksumSHA1, domain.ChecksumSHA256} {
		if v := h.Get("x-amz-checksum-" + strings.ToLower(string(algo))); v != "" {
			if !found.IsZero() {
				return domain.Checksum{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Expecting a single x-amz-checksum- header")
			}
			found = domain.Checksum{Algorithm: algo, Value: v}
		}
	}
	if found.IsZero() {
		if algo := h.Get("x-amz-sdk-checksum-algorithm"); algo != "" && h.Get("x-amz-trailer") != "" {
			found = domain.Checksum{Algorithm: domain.ChecksumAlgorithm(strings.ToUpper(algo))}
		}
	}
	return found, nil
}

// writeObjectHeaders sets the response headers describing an object.
func writeObjectHeaders(w http.ResponseWriter, info domain.ObjectInfo) {
	h := w.Header()
	if info.ContentType != "" {
		h.Set("Content-Type", info.ContentType)
	}
	if info.ETag != "" {
		h.Set("ETag", quoteETag(info.ETag))
	}
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	if info.VersionID != "" {
		h.Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	if info.IsDeleteMarker {
		h.Set(httpx.HeaderAmzDeleteMarker, "true")
	}
	set := func(name, v string) {
		if v != "" {
			h.Set(name, v)
		}
	}
	set("Content-Encoding", info.ContentEncoding)
	set("Cache-Control", info.CacheControl)
	set("Content-Disposition", info.ContentDisposition)
	set("Content-Language", info.ContentLanguage)
	if !info.Expires.IsZero() {
		h.Set("Expires", info.Expires.UTC().Format(http.TimeFormat))
	}
	if info.StorageClass != "" && info.StorageClass != domain.StorageClassStandard {
		h.Set(httpx.HeaderAmzStorageClass, info.StorageClass)
	}
	httpx.SetUserMetadata(h, info.UserMetadata)
	if len(info.Tags) > 0 {
		h.Set("x-amz-tagging-count", strconv.Itoa(len(info.Tags)))
	}
	if info.Parts > 1 {
		h.Set("x-amz-mp-parts-count", strconv.Itoa(info.Parts))
	}
	if !info.Checksum.IsZero() && info.Checksum.Value != "" {
		h.Set("x-amz-checksum-"+strings.ToLower(string(info.Checksum.Algorithm)), info.Checksum.Value)
	}
	h.Set("Accept-Ranges", "bytes")
}

// applyResponseOverrides honors the response-* query parameters of GetObject.
func applyResponseOverrides(w http.ResponseWriter, q url.Values) {
	for param, header := range map[string]string{
		"response-content-type": "Content-Type", "response-content-language": "Content-Language",
		"response-expires": "Expires", "response-cache-control": "Cache-Control",
		"response-content-disposition": "Content-Disposition", "response-content-encoding": "Content-Encoding",
	} {
		if v := q.Get(param); v != "" {
			w.Header().Set(header, v)
		}
	}
}

// parseCopySource decodes x-amz-copy-source: "/bucket/key?versionId=v" (the
// leading slash is optional, the key is URL encoded).
func parseCopySource(v string) (bucket, key, versionID string, err error) {
	v = strings.TrimPrefix(v, "/")
	if i := strings.Index(v, "?"); i >= 0 {
		q, qerr := url.ParseQuery(v[i+1:])
		if qerr != nil {
			return "", "", "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Copy Source has an invalid query string")
		}
		versionID = q.Get("versionId")
		v = v[:i]
	}
	decoded, uerr := url.PathUnescape(v)
	if uerr != nil {
		return "", "", "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Copy Source must be URL encoded")
	}
	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", "", domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	return bucket, key, versionID, nil
}

// tagsToXML and tagsFromXML convert between maps and Tagging documents.
func tagsToXML(tags map[string]string) Tagging {
	var doc Tagging
	doc.TagSet.Tags = make([]Tag, 0, len(tags))
	for k, v := range tags {
		doc.TagSet.Tags = append(doc.TagSet.Tags, Tag{Key: k, Value: v})
	}
	sortTags(doc.TagSet.Tags)
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

func sortTags(tags []Tag) {
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && tags[j].Key < tags[j-1].Key; j-- {
			tags[j], tags[j-1] = tags[j-1], tags[j]
		}
	}
}

// requestConditions builds the policy condition context of a request.
func requestConditions(r *http.Request) map[string][]string {
	conds := map[string][]string{}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		conds["aws:SourceIp"] = []string{host}
	} else if r.RemoteAddr != "" {
		conds["aws:SourceIp"] = []string{r.RemoteAddr}
	}
	conds["aws:SecureTransport"] = []string{strconv.FormatBool(r.TLS != nil)}
	if ua := r.UserAgent(); ua != "" {
		conds["aws:UserAgent"] = []string{ua}
	}
	if ref := r.Referer(); ref != "" {
		conds["aws:Referer"] = []string{ref}
	}
	q := r.URL.Query()
	for param, key := range map[string]string{"prefix": "s3:prefix", "delimiter": "s3:delimiter", "max-keys": "s3:max-keys", "versionId": "s3:versionid"} {
		if v := q.Get(param); v != "" {
			conds[key] = []string{v}
		}
	}
	if v := r.Header.Get("x-amz-acl"); v != "" {
		conds["s3:x-amz-acl"] = []string{v}
	}
	switch {
	case auth.IsPresigned(q):
		conds["s3:authType"] = []string{"REST-QUERY-STRING"}
	case auth.IsSigned(r):
		conds["s3:authType"] = []string{"REST-HEADER"}
	}
	return conds
}

func toLower(s string) string { return strings.ToLower(s) }

func toUpper(s string) string { return strings.ToUpper(s) }

// authorizer evaluates the policy engine for the S3 handlers.
type authorizer struct {
	policy  policy.Service
	buckets bucket.Service
	enc     httpx.Encoder
}

// authorize checks that the caller may perform action on the resource. It
// writes the error response and returns false when access is denied. With
// no policy engine configured every request is allowed (tests).
func (a *authorizer) authorize(w http.ResponseWriter, r *http.Request, action policy.Action, bucketName, object string) bool {
	err := a.check(r, action, bucketName, object)
	if err != nil {
		a.enc.WriteS3Error(w, r, err)
		return false
	}
	return true
}

// allowed reports the decision without writing a response (batch operations).
func (a *authorizer) allowed(r *http.Request, action policy.Action, bucketName, object string) bool {
	return a.check(r, action, bucketName, object) == nil
}

// check returns nil when the request is allowed.
func (a *authorizer) check(r *http.Request, action policy.Action, bucketName, object string) error {
	if a.policy == nil {
		return nil
	}
	id, ok := middleware.IdentityFromContext(r.Context())
	if !ok || id == nil {
		id = domain.Anonymous()
	}
	args := policy.Args{Identity: id, Action: action, Bucket: bucketName, Object: object, Conditions: requestConditions(r)}
	if bucketName != "" && a.buckets != nil && !id.IsRoot {
		raw, err := a.buckets.GetPolicy(r.Context(), bucketName)
		switch {
		case err == nil:
			var p domain.Policy
			if jerr := json.Unmarshal(raw, &p); jerr == nil {
				args.BucketPolicy = &p
			}
		case errors.Is(err, domain.ErrPolicyNotFound), errors.Is(err, domain.ErrBucketNotFound):
		default:
			return err
		}
	}
	decision, err := a.policy.Evaluate(r.Context(), args)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fmt.Errorf("%w: %s", domain.ErrAccessDenied, decision.Reason)
	}
	return nil
}

// objectInput assembles the PutObject input from the request.
func objectInput(r *http.Request, bucketName, key string) (object.PutObjectInput, error) {
	md5Hex, err := contentMD5Hex(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	tags, err := parseTaggingHeader(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	checksum, err := checksumFromHeaders(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	size := r.ContentLength
	if r.Body == nil || r.Body == http.NoBody {
		size = 0
	}
	return object.PutObjectInput{
		Bucket:       bucketName,
		Key:          key,
		Reader:       r.Body,
		Size:         size,
		MD5Hex:       md5Hex,
		Checksum:     checksum,
		ContentType:  r.Header.Get("Content-Type"),
		UserMetadata: userMetadata(r.Header),
		Tags:         tags,
		StorageClass: r.Header.Get(httpx.HeaderAmzStorageClass),
		Conditions:   parseConditions(r.Header, "If-"),
	}, nil
}
