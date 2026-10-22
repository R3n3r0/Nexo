package object

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Metadata keys persisted in xl.meta. HTTP headers map to them one to one.
const (
	metaContentType        = "content-type"
	metaContentEncoding    = "content-encoding"
	metaCacheControl       = "cache-control"
	metaContentDisposition = "content-disposition"
	metaContentLanguage    = "content-language"
	metaExpires            = "expires"
	metaETag               = "etag"
	metaStorageClass       = "x-amz-storage-class"
	metaTagging            = "x-amz-tagging"
	metaUserPrefix         = "x-amz-meta-"
	metaChecksumPrefix     = "x-amz-checksum-"
	// metaChecksumType is the checksum type (FULL_OBJECT or COMPOSITE)
	// of the additional checksum of the version.
	metaChecksumType = "x-amz-checksum-type"
	// metaUpdated is the stamp (Unix nanoseconds, increasing) of the last
	// in-place update of the metadata of a version (tags): when the
	// drives disagree on the metadata with no majority, the newest wins.
	// It is internal: never exposed, never copied.
	metaUpdated = "x-nexo-internal-meta-updated"
	// metaActualSize is the plaintext size of an encrypted version (its
	// stored size is the one of the ciphertext). It is internal.
	metaActualSize = "x-nexo-internal-actual-size"
	// metaUploadID is the multipart upload a version was completed from:
	// a retried completion of that upload (its response lost) answers
	// with the version instead of NoSuchUpload. It is internal.
	metaUploadID = "x-nexo-internal-upload-id"
	// metaReplicationStatus is the replication state of the version
	// (x-amz-replication-status, S-BKT-12); metaReplicationTargetPrefix
	// followed by the ARN of a target its state at that destination. They
	// are never copied.
	metaReplicationStatus       = "x-amz-replication-status"
	metaReplicationTargetPrefix = "x-nexo-internal-replication-"
)

// metaStamp returns the in-place update stamp of a version's metadata, 0
// when it was never updated.
func metaStamp(meta map[string]string) int64 {
	v, err := strconv.ParseInt(meta[metaUpdated], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// defaultContentType is stored when a write declares no Content-Type.
const defaultContentType = "application/octet-stream"

// httpTimeLayouts are the date formats accepted for the Expires header
// (RFC 9110 §5.6.7: IMF-fixdate first, then the obsolete formats).
func httpTimeLayouts() []string {
	return []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC}
}

func parseHTTPTime(v string) (time.Time, bool) {
	for _, layout := range httpTimeLayouts() {
		if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// formatHTTPTime renders t as an IMF-fixdate.
func formatHTTPTime(t time.Time) string { return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") }

// httpMeta is the client-provided metadata of a new version: the standard
// headers and the user metadata.
type httpMeta struct {
	contentType        string
	contentEncoding    string
	contentDisposition string
	contentLanguage    string
	cacheControl       string
	expires            string
	user               map[string]string
}

// standard returns the standard header fields of m keyed by metadata key.
func (m *httpMeta) standard() map[string]*string {
	return map[string]*string{
		metaContentEncoding:    &m.contentEncoding,
		metaContentDisposition: &m.contentDisposition,
		metaContentLanguage:    &m.contentLanguage,
		metaCacheControl:       &m.cacheControl,
		metaExpires:            &m.expires,
	}
}

// build assembles the persisted metadata of a new version. User metadata
// keys are lowercased and stripped of the x-amz-meta- prefix; the keys
// named after a standard header fill that header when its field is empty.
func (m httpMeta) build(tags map[string]string, storageClass string) map[string]string {
	std := m.standard()
	meta := make(map[string]string, len(m.user)+len(std)+3)
	user := make(map[string]string, len(m.user))
	for k, v := range m.user {
		lk := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(k)), metaUserPrefix)
		if lk == "" {
			continue
		}
		if field, ok := std[lk]; ok {
			if *field == "" {
				*field = v
			}
			continue
		}
		user[lk] = v
	}
	for k, v := range user {
		meta[metaUserPrefix+k] = v
	}
	for k, field := range std {
		if *field != "" {
			meta[k] = *field
		}
	}
	meta[metaContentType] = m.contentType
	if m.contentType == "" {
		meta[metaContentType] = defaultContentType
	}
	if len(tags) > 0 {
		meta[metaTagging] = encodeTags(tags)
	}
	if storageClass != "" && storageClass != domain.StorageClassStandard {
		meta[metaStorageClass] = storageClass
	}
	return meta
}

// copyableMeta returns the metadata a COPY directive carries over from the
// source version: content type, standard headers and user metadata. The
// ETag, checksums, tags and storage class are recomputed or chosen by the
// copy itself.
func copyableMeta(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		switch {
		case k == metaContentType, k == metaContentEncoding, k == metaCacheControl,
			k == metaContentDisposition, k == metaContentLanguage, k == metaExpires,
			strings.HasPrefix(k, metaUserPrefix):
			out[k] = v
		}
	}
	if out[metaContentType] == "" {
		out[metaContentType] = defaultContentType
	}
	return out
}

// isMultipartETag reports the "<md5>-<parts>" ETag of a completed upload.
func isMultipartETag(etag string) bool {
	i := strings.LastIndexByte(etag, '-')
	if i < 0 {
		return false
	}
	_, err := strconv.Atoi(etag[i+1:])
	return err == nil
}

// objectInfo converts the persisted version into the public description.
func objectInfo(bucket, key string, fi storage.FileInfo) domain.ObjectInfo {
	info := domain.ObjectInfo{
		Bucket:         bucket,
		Key:            key,
		VersionID:      fi.VersionID,
		Size:           actualSize(fi),
		ModTime:        fi.ModTime.UTC(),
		IsDeleteMarker: fi.Deleted,
		IsLatest:       fi.IsLatest,
		UserMetadata:   map[string]string{},
		Inlined:        fi.DataDir == "" && !fi.Deleted,
	}
	if info.VersionID == "" && fi.Versioned {
		// The null version of a versioned (or suspended) bucket is
		// reported as "null", like S3 (x-amz-version-id: null); only
		// unversioned buckets have no version id at all. A null version
		// written before versioning was enabled has no versioned flag:
		// labelNull reports it from the bucket state.
		info.VersionID = nullVersion
	}
	for k, v := range fi.Metadata {
		switch {
		case k == metaContentType:
			info.ContentType = v
		case k == metaContentEncoding:
			info.ContentEncoding = v
		case k == metaCacheControl:
			info.CacheControl = v
		case k == metaContentDisposition:
			info.ContentDisposition = v
		case k == metaContentLanguage:
			info.ContentLanguage = v
		case k == metaExpires:
			if t, ok := parseHTTPTime(v); ok {
				info.Expires = t
			}
		case k == metaETag:
			info.ETag = v
		case k == metaStorageClass:
			info.StorageClass = v
		case k == metaTagging:
			info.Tags = decodeTags(v)
		case strings.HasPrefix(k, metaUserPrefix):
			info.UserMetadata[strings.TrimPrefix(k, metaUserPrefix)] = v
		case k == metaChecksumType:
			// Read with the checksum, below.
		case strings.HasPrefix(k, metaChecksumPrefix):
			info.Checksum = domain.Checksum{Algorithm: domain.ChecksumAlgorithm(strings.ToUpper(strings.TrimPrefix(k, metaChecksumPrefix))), Value: v}
		case strings.HasPrefix(k, metaSealedChecksumPrefix):
			// Sealed (encrypted version, openChecksums not applied): the
			// algorithm only.
			if info.Checksum.Value == "" {
				info.Checksum = domain.Checksum{Algorithm: domain.ChecksumAlgorithm(strings.ToUpper(strings.TrimPrefix(k, metaSealedChecksumPrefix)))}
			}
		case k == metaLockMode:
			info.Retention.Mode = domain.RetentionMode(v)
		case k == metaLockRetainUntil:
			if t, ok := parseRetainUntil(v); ok {
				info.Retention.RetainUntil = t
			}
		case k == metaLockLegalHold:
			info.LegalHold = domain.LegalHoldStatus(v)
		case k == metaReplicationStatus:
			info.ReplicationStatus = domain.ReplicationStatus(v)
		case strings.HasPrefix(k, metaReplicationTargetPrefix):
			if info.ReplicationTargets == nil {
				info.ReplicationTargets = map[string]domain.ReplicationStatus{}
			}
			info.ReplicationTargets[strings.TrimPrefix(k, metaReplicationTargetPrefix)] = domain.ReplicationStatus(v)
		}
	}
	if !info.Checksum.IsZero() {
		info.Checksum.Type = checksumTypeOf(info.Checksum.Value, fi.Metadata[metaChecksumType])
	}
	info.Encryption = crypto.InfoOf(fi.Metadata)
	if isMultipartETag(info.ETag) {
		info.Parts = len(fi.Parts)
	}
	if info.StorageClass == "" && !fi.Deleted {
		info.StorageClass = domain.StorageClassStandard
	}
	return info
}

// nullLabel returns the version id the null version of bucket is
// reported with: "null" when the bucket has a versioning state (enabled
// or suspended), "" when it never had versioning. When the settings of the
// bucket cannot be read, the label stays "" (logged): the read is served
// anyway, and the callers that address versions treat "" as the null
// version too.
func (l *layer) nullLabel(ctx context.Context, bucket string) string {
	cfg, err := l.bucketConfig(ctx, bucket)
	if err != nil {
		l.deps.Logger.DebugContext(ctx, "object: cannot read the versioning of the bucket", "bucket", bucket, "err", err)
		return ""
	}
	if cfg.Versioning.Versioned() {
		return nullVersion
	}
	return ""
}

// labelNull reports a version without id as "null" when its bucket has a
// versioning state (see nullLabel): a version written before versioning
// was enabled is the null version of the bucket, addressed as "null" by
// every caller (S3, the lifecycle, the console), like the null version
// written while versioning is suspended. objectInfo alone reports it
// without id, since the version does not record the later change.
func (l *layer) labelNull(ctx context.Context, bucket string, info *domain.ObjectInfo) {
	if info.VersionID == "" {
		info.VersionID = l.nullLabel(ctx, bucket)
	}
}

// labelNulls applies labelNull to the versions of a listing of bucket,
// reading the bucket state once and only when a version needs it.
func (l *layer) labelNulls(ctx context.Context, bucket string, infos []domain.ObjectInfo) {
	resolved, label := false, ""
	for i := range infos {
		if infos[i].VersionID != "" {
			continue
		}
		if !resolved {
			label, resolved = l.nullLabel(ctx, bucket), true
		}
		infos[i].VersionID = label
	}
}

// --- tags --------------------------------------------------------------------

func encodeTags(tags map[string]string) string {
	v := url.Values{}
	for k, val := range tags {
		v.Set(k, val)
	}
	return v.Encode()
}

func decodeTags(s string) map[string]string {
	v, err := url.ParseQuery(s)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(v))
	for k, vals := range v {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}

// validateTags applies the S3 object tagging rules: at most 10 tags, keys
// of 1-128 and values of 0-256 Unicode characters, valid UTF-8, no key in
// the reserved "aws:" namespace.
func validateTags(tags map[string]string) error {
	if len(tags) > maxTags {
		return domain.NewS3Error(domain.ErrInvalidArgument, "BadRequest", "Object tags cannot be greater than 10")
	}
	for k, v := range tags {
		if k == "" || !utf8.ValidString(k) || utf8.RuneCountInString(k) > maxTagKeyLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagKey you have provided is invalid")
		}
		if strings.HasPrefix(strings.ToLower(k), "aws:") {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "Your TagKey cannot be prefixed with aws:")
		}
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxTagValueLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagValue you have provided is invalid")
		}
	}
	return nil
}

// --- conditions -------------------------------------------------------------

// checkReadConditions evaluates the conditional headers of a read with the
// precedence of RFC 9110 §13.2.2 (formerly RFC 7232 §6): If-Match, then
// If-Unmodified-Since when If-Match is absent, then If-None-Match, then
// If-Modified-Since when If-None-Match is absent. Dates compare at second
// precision, the precision of HTTP dates.
func checkReadConditions(info domain.ObjectInfo, c domain.Conditions) error {
	if c.IsZero() {
		return nil
	}
	modTime := info.ModTime.Truncate(time.Second)
	if c.IfMatch != "" {
		if !etagMatches(c.IfMatch, info.ETag) {
			return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
		}
	} else if !c.IfUnmodifiedSince.IsZero() && modTime.After(c.IfUnmodifiedSince) {
		return fmt.Errorf("%w: If-Unmodified-Since", domain.ErrPreconditionFailed)
	}
	if c.IfNoneMatch != "" {
		if etagMatches(c.IfNoneMatch, info.ETag) {
			return fmt.Errorf("%w: If-None-Match", domain.ErrNotModified)
		}
	} else if !c.IfModifiedSince.IsZero() && !modTime.After(c.IfModifiedSince) {
		return fmt.Errorf("%w: If-Modified-Since", domain.ErrNotModified)
	}
	return nil
}

// checkSourceConditions evaluates the x-amz-copy-source-if-* headers of a
// copy: every failure, including the "not modified" ones, is a failed
// precondition (S3 answers 412 to a copy, never 304).
func checkSourceConditions(info domain.ObjectInfo, c domain.Conditions) error {
	err := checkReadConditions(info, c)
	if errors.Is(err, domain.ErrNotModified) {
		return fmt.Errorf("%w: copy source: %v", domain.ErrPreconditionFailed, err)
	}
	return err
}

// checkWriteConditions evaluates the conditional headers of a write against
// the current version (exists reports whether there is one): If-None-Match
// "*" requires a missing object, If-Match an existing one with that ETag.
// As in S3, If-Match on a missing object (or a delete marker) answers
// NoSuchKey, not PreconditionFailed.
func checkWriteConditions(exists bool, info domain.ObjectInfo, c domain.Conditions) error {
	if c.IfNoneMatch != "" && exists && etagMatches(c.IfNoneMatch, info.ETag) {
		return fmt.Errorf("%w: If-None-Match", domain.ErrPreconditionFailed)
	}
	if c.IfMatch != "" {
		if !exists {
			return domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")
		}
		if !etagMatches(c.IfMatch, info.ETag) {
			return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
		}
	}
	return nil
}

// etagMatches implements the ETag comparison of conditional headers: "*"
// matches anything, otherwise a comma-separated list of (quoted, possibly
// weak) tags.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	want := strings.Trim(etag, `"`)
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if strings.EqualFold(strings.Trim(candidate, `"`), want) {
			return true
		}
	}
	return false
}
