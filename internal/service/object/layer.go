package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
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
)

// Limits shared with S3.
const (
	maxTags           = 10
	maxTagKeyLength   = 128
	maxTagValueLength = 256
	maxListKeys       = 1000
	maxPartNumber     = 10000
)

// layer holds the S3 logic shared by the filesystem and erasure
// implementations. Every object lives in exactly one store (erasure set),
// chosen by pick; buckets exist in every store.
type layer struct {
	deps Deps
	opts options
	sets []store
	pick func(key string) store
	name string
}

func newLayer(deps Deps, opts []Option, sets []store, name string) *layer {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Locks == nil {
		deps.Locks = lock.NewLocal(lock.Deps{})
	}
	if deps.IDs == nil {
		deps.IDs = topology.NewUUIDGenerator()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	l := &layer{deps: deps, opts: o, sets: sets, name: name}
	l.pick = func(string) store { return sets[0] }
	return l
}

func (l *layer) now() time.Time { return l.deps.Clock.Now().UTC() }

func (l *layer) newID() string { return l.deps.IDs.NewID() }

// --- locking -----------------------------------------------------------------

func (l *layer) lock(ctx context.Context, bucket, key string, write bool) (lock.LockContext, error) {
	ns := l.deps.Locks.NewNSLock(ctx, bucket, key)
	var (
		lc  lock.LockContext
		err error
	)
	if write {
		lc, err = ns.GetLock(ctx, l.opts.lockTimeout)
	} else {
		lc, err = ns.GetRLock(ctx, l.opts.lockTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: lock %s: %w", l.name, lock.Resource(bucket, key), err)
	}
	return lc, nil
}

// --- bucket configuration -------------------------------------------------

func (l *layer) bucketConfig(ctx context.Context, bucket string) (BucketConfig, error) {
	if l.deps.Buckets == nil {
		return BucketConfig{}, nil
	}
	cfg, err := l.deps.Buckets.BucketConfig(ctx, bucket)
	if err != nil {
		return BucketConfig{}, fmt.Errorf("%s: bucket config %s: %w", l.name, bucket, err)
	}
	return cfg, nil
}

// --- buckets ----------------------------------------------------------------

func validBucket(name string) error {
	if err := domain.ValidateBucketName(name); err != nil {
		return err
	}
	if domain.IsSystemBucket(name) {
		return fmt.Errorf("%w: %q is reserved", domain.ErrInvalidBucketName, name)
	}
	return nil
}

// MakeBucket implements Service.
func (l *layer) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	lc, err := l.lock(ctx, bucket, "", true)
	if err != nil {
		return err
	}
	defer lc.Unlock()
	errs := parallel(len(l.sets), func(i int) error { return l.sets[i].MakeVol(ctx, bucket) })
	for _, err := range errs {
		if err != nil && (!errors.Is(err, domain.ErrBucketAlreadyExists) || !opts.ForceCreate) {
			return fmt.Errorf("%s: make bucket %s: %w", l.name, bucket, err)
		}
	}
	l.publish(ctx, event.BucketCreated, bucket, domain.ObjectInfo{Bucket: bucket})
	return nil
}

// GetBucketInfo implements Service.
func (l *layer) GetBucketInfo(ctx context.Context, bucket string) (domain.BucketInfo, error) {
	if err := validBucket(bucket); err != nil {
		return domain.BucketInfo{}, err
	}
	vi, err := l.sets[0].StatVol(ctx, bucket)
	if err != nil {
		return domain.BucketInfo{}, fmt.Errorf("%s: stat bucket %s: %w", l.name, bucket, err)
	}
	return l.bucketInfo(ctx, vi), nil
}

func (l *layer) bucketInfo(ctx context.Context, vi storage.VolInfo) domain.BucketInfo {
	info := domain.BucketInfo{Bucket: domain.Bucket{Name: vi.Name, Created: vi.Created.UTC(), Region: l.opts.region}}
	if cfg, err := l.bucketConfig(ctx, vi.Name); err == nil {
		info.Versioning = cfg.Versioning
		info.ObjectLockEnabled = cfg.ObjectLock
		info.QuotaBytes = cfg.QuotaBytes
	}
	return info
}

// ListBuckets implements Service.
func (l *layer) ListBuckets(ctx context.Context) ([]domain.BucketInfo, error) {
	vols, err := l.sets[0].ListVols(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: list buckets: %w", l.name, err)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	out := make([]domain.BucketInfo, 0, len(vols))
	for _, v := range vols {
		if domain.IsSystemBucket(v.Name) || domain.ValidateBucketName(v.Name) != nil {
			continue
		}
		out = append(out, l.bucketInfo(ctx, v))
	}
	return out, nil
}

// DeleteBucket implements Service.
func (l *layer) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	lc, err := l.lock(ctx, bucket, "", true)
	if err != nil {
		return err
	}
	defer lc.Unlock()
	if _, err := l.sets[0].StatVol(ctx, bucket); err != nil {
		return fmt.Errorf("%s: delete bucket %s: %w", l.name, bucket, err)
	}
	if !opts.Force {
		for _, st := range l.sets {
			empty := true
			err := st.Walk(ctx, storage.WalkDirOptions{Bucket: bucket, Recursive: true, Limit: 1}, func(e storage.WalkEntry) error {
				if !e.IsDir {
					empty = false
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("%s: delete bucket %s: %w", l.name, bucket, err)
			}
			if !empty {
				return fmt.Errorf("%w: %s", domain.ErrBucketNotEmpty, bucket)
			}
		}
		uploads, err := l.uploadsOf(ctx, bucket)
		if err != nil {
			return err
		}
		if len(uploads) > 0 {
			return fmt.Errorf("%w: %s has %d multipart upload(s) in progress", domain.ErrBucketNotEmpty, bucket, len(uploads))
		}
	}
	errs := parallel(len(l.sets), func(i int) error { return l.sets[i].DeleteVol(ctx, bucket, opts.Force) })
	for _, err := range errs {
		if err != nil {
			return fmt.Errorf("%s: delete bucket %s: %w", l.name, bucket, err)
		}
	}
	if opts.Force {
		l.abortUploadsOf(ctx, bucket)
	}
	l.publish(ctx, event.BucketRemoved, bucket, domain.ObjectInfo{Bucket: bucket})
	return nil
}

// --- metadata helpers -----------------------------------------------------

// objectInfo converts the persisted version into the public description.
func objectInfo(bucket, key string, fi storage.FileInfo) domain.ObjectInfo {
	info := domain.ObjectInfo{
		Bucket:         bucket,
		Key:            key,
		VersionID:      fi.VersionID,
		Size:           fi.Size,
		ModTime:        fi.ModTime.UTC(),
		IsDeleteMarker: fi.Deleted,
		IsLatest:       fi.IsLatest,
		UserMetadata:   map[string]string{},
		Parts:          len(fi.Parts),
		Inlined:        fi.DataDir == "",
	}
	if len(fi.Parts) == 1 {
		info.Parts = 0
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
			if t, err := time.Parse(time.RFC1123, v); err == nil {
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
		case strings.HasPrefix(k, metaChecksumPrefix):
			info.Checksum = domain.Checksum{Algorithm: domain.ChecksumAlgorithm(strings.ToUpper(strings.TrimPrefix(k, metaChecksumPrefix))), Value: v}
		}
	}
	if info.StorageClass == "" && !fi.Deleted {
		info.StorageClass = domain.StorageClassStandard
	}
	return info
}

// buildMetadata assembles the persisted metadata of a new version.
func buildMetadata(contentType string, user, tags map[string]string, storageClass string, checksum domain.Checksum) map[string]string {
	meta := make(map[string]string, len(user)+4)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	meta[metaContentType] = contentType
	for k, v := range user {
		lk := strings.ToLower(k)
		switch lk {
		case metaContentEncoding, metaCacheControl, metaContentDisposition, metaContentLanguage, metaExpires:
			meta[lk] = v
		default:
			meta[metaUserPrefix+strings.TrimPrefix(lk, metaUserPrefix)] = v
		}
	}
	if len(tags) > 0 {
		meta[metaTagging] = encodeTags(tags)
	}
	if storageClass != "" && storageClass != domain.StorageClassStandard {
		meta[metaStorageClass] = storageClass
	}
	if !checksum.IsZero() {
		meta[metaChecksumPrefix+strings.ToLower(string(checksum.Algorithm))] = checksum.Value
	}
	return meta
}

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

func validateTags(tags map[string]string) error {
	if len(tags) > maxTags {
		return domain.NewS3Error(domain.ErrInvalidArgument, "BadRequest", "Object tags cannot be greater than 10")
	}
	for k, v := range tags {
		if k == "" || len(k) > maxTagKeyLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagKey you have provided is invalid")
		}
		if len(v) > maxTagValueLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagValue you have provided is invalid")
		}
	}
	return nil
}

// checkReadConditions evaluates the conditional headers of a read.
func checkReadConditions(info domain.ObjectInfo, c domain.Conditions) error {
	if c.IsZero() {
		return nil
	}
	modTime := info.ModTime.Truncate(time.Second)
	if c.IfMatch != "" && !etagMatches(c.IfMatch, info.ETag) {
		return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
	}
	if !c.IfUnmodifiedSince.IsZero() && c.IfMatch == "" && modTime.After(c.IfUnmodifiedSince) {
		return fmt.Errorf("%w: If-Unmodified-Since", domain.ErrPreconditionFailed)
	}
	if c.IfNoneMatch != "" && etagMatches(c.IfNoneMatch, info.ETag) {
		return fmt.Errorf("%w: If-None-Match", domain.ErrNotModified)
	}
	if !c.IfModifiedSince.IsZero() && c.IfNoneMatch == "" && !modTime.After(c.IfModifiedSince) {
		return fmt.Errorf("%w: If-Modified-Since", domain.ErrNotModified)
	}
	return nil
}

// checkWriteConditions evaluates the conditional headers of a write against
// the current version (exists reports whether there is one).
func checkWriteConditions(exists bool, info domain.ObjectInfo, c domain.Conditions) error {
	if c.IfNoneMatch != "" && exists && etagMatches(c.IfNoneMatch, info.ETag) {
		return fmt.Errorf("%w: If-None-Match", domain.ErrPreconditionFailed)
	}
	if c.IfMatch != "" && (!exists || !etagMatches(c.IfMatch, info.ETag)) {
		return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
	}
	return nil
}

// etagMatches implements the ETag comparison of conditional headers: "*"
// matches anything, otherwise a comma-separated list of (quoted) tags.
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.Trim(strings.TrimSpace(candidate), `"`)
		candidate = strings.TrimPrefix(candidate, "W/")
		if strings.EqualFold(strings.Trim(candidate, `"`), strings.Trim(etag, `"`)) {
			return true
		}
	}
	return false
}

// --- hashing -----------------------------------------------------------------

// hashReader computes the ETag (MD5), the optional SHA-256 and the optional
// additional checksum of a stream while it is being stored.
type hashReader struct {
	r     io.Reader
	md5   hash.Hash
	sha   hash.Hash
	extra hash.Hash
	algo  domain.ChecksumAlgorithm
	n     int64
}

func newHashReader(r io.Reader, wantSHA bool, algo domain.ChecksumAlgorithm) (*hashReader, error) {
	h := &hashReader{r: r, md5: md5.New(), algo: algo}
	if wantSHA {
		h.sha = sha256.New()
	}
	switch algo {
	case domain.ChecksumNone:
	case domain.ChecksumCRC32:
		h.extra = crc32.New(crc32.IEEETable)
	case domain.ChecksumCRC32C:
		h.extra = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case domain.ChecksumSHA1:
		h.extra = sha1.New()
	case domain.ChecksumSHA256:
		h.extra = sha256.New()
	default:
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Unsupported checksum algorithm "+string(algo))
	}
	return h, nil
}

func (h *hashReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.n += int64(n)
		_, _ = h.md5.Write(p[:n])
		if h.sha != nil {
			_, _ = h.sha.Write(p[:n])
		}
		if h.extra != nil {
			_, _ = h.extra.Write(p[:n])
		}
	}
	return n, err
}

func (h *hashReader) etag() string { return hex.EncodeToString(h.md5.Sum(nil)) }

// verify compares the computed digests with the client-declared ones and
// returns the checksum to persist.
func (h *hashReader) verify(md5Hex, sha256Hex string, declared domain.Checksum) (domain.Checksum, error) {
	if md5Hex != "" && !strings.EqualFold(md5Hex, h.etag()) {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrChecksumMismatch, "BadDigest", "The Content-MD5 you specified did not match what we received.")
	}
	if h.sha != nil && sha256Hex != "" && !strings.EqualFold(sha256Hex, hex.EncodeToString(h.sha.Sum(nil))) {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
	}
	if h.extra == nil {
		return domain.Checksum{}, nil
	}
	got := domain.Checksum{Algorithm: h.algo, Value: base64.StdEncoding.EncodeToString(h.extra.Sum(nil))}
	if declared.Value != "" && declared.Value != got.Value {
		return domain.Checksum{}, domain.NewS3Error(domain.ErrChecksumMismatch, "BadDigest",
			fmt.Sprintf("The %s you specified did not match the calculated checksum.", h.algo))
	}
	return got, nil
}

// --- object writes -----------------------------------------------------------

// versionFor decides the version id of a new write from the bucket state.
func (l *layer) versionFor(cfg BucketConfig, forced string) (versionID string, versioned bool) {
	if forced != "" && forced != "null" {
		return forced, true
	}
	switch cfg.Versioning {
	case domain.VersioningEnabled:
		return l.newID(), true
	case domain.VersioningSuspended:
		return "", true
	default:
		return "", false
	}
}

// writeInput is what a single-stream write needs beyond the body.
type writeInput struct {
	size       int64
	md5Hex     string
	sha256Hex  string
	checksum   domain.Checksum
	meta       map[string]string
	conditions domain.Conditions
	versionID  string
	modTime    time.Time
	eventName  event.Name
	skipEvent  bool
	withinLock bool
}

// writeObject stores one stream as a new version of bucket/key.
func (l *layer) writeObject(ctx context.Context, bucket, key string, body io.Reader, in writeInput) (domain.ObjectInfo, error) {
	if err := validBucket(bucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := domain.ValidateObjectKey(key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	if !in.withinLock {
		lc, err := l.lock(ctx, bucket, key, true)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		defer lc.Unlock()
	}
	st := l.pick(key)
	if _, err := st.StatVol(ctx, bucket); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: put %s/%s: %w", l.name, bucket, key, err)
	}
	cfg, err := l.bucketConfig(ctx, bucket)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if !in.conditions.IsZero() {
		cur, err := st.ReadVersion(ctx, bucket, key, "", false)
		exists := err == nil && !cur.Deleted
		if err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
			return domain.ObjectInfo{}, fmt.Errorf("%s: put %s/%s: %w", l.name, bucket, key, err)
		}
		if err := checkWriteConditions(exists, objectInfo(bucket, key, cur), in.conditions); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	if cfg.QuotaBytes > 0 && in.size > int64(cfg.QuotaBytes) {
		return domain.ObjectInfo{}, fmt.Errorf("%w: object of %d bytes exceeds the bucket quota of %d bytes", domain.ErrQuotaExceeded, in.size, cfg.QuotaBytes)
	}

	hr, err := newHashReader(body, in.sha256Hex != "", in.checksum.Algorithm)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	versionID, versioned := l.versionFor(cfg, in.versionID)
	modTime := in.modTime
	if modTime.IsZero() {
		modTime = l.now()
	}
	fi := storage.FileInfo{
		Volume:    bucket,
		Name:      key,
		VersionID: versionID,
		ModTime:   modTime,
		Metadata:  in.meta,
		Versioned: versioned,
		Erasure:   st.ErasureInfo(),
	}
	tmpDir := ""
	inline := in.size >= 0 && in.size <= l.opts.inlineLimit
	if inline {
		data, err := io.ReadAll(io.LimitReader(hr, in.size+1))
		if err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%s: read body: %w", l.name, mapBodyErr(err))
		}
		if int64(len(data)) != in.size {
			return domain.ObjectInfo{}, incompleteBody(in.size, int64(len(data)))
		}
		fi.Data = data
		fi.Size = in.size
	} else {
		tmpDir = storage.TmpDir + "/" + l.newID()
		fi.DataDir = uuid.NewString()
		n, err := st.WriteData(ctx, tmpDir, 1, in.size, hr)
		if err != nil {
			_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
			return domain.ObjectInfo{}, fmt.Errorf("%s: write %s/%s: %w", l.name, bucket, key, mapBodyErr(err))
		}
		if in.size >= 0 && n != in.size {
			_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
			return domain.ObjectInfo{}, incompleteBody(in.size, n)
		}
		fi.Size = n
	}
	checksum, err := hr.verify(in.md5Hex, in.sha256Hex, in.checksum)
	if err != nil {
		if tmpDir != "" {
			_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
		}
		return domain.ObjectInfo{}, err
	}
	if fi.Metadata == nil {
		fi.Metadata = map[string]string{}
	}
	fi.Metadata[metaETag] = hr.etag()
	if !checksum.IsZero() {
		fi.Metadata[metaChecksumPrefix+strings.ToLower(string(checksum.Algorithm))] = checksum.Value
	}
	fi.Parts = []storage.PartInfo{{Number: 1, ETag: hr.etag(), Size: fi.Size, ActualSize: fi.Size, ModTime: modTime}}
	if err := l.commitVersion(ctx, st, bucket, key, fi, tmpDir); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: commit %s/%s: %w", l.name, bucket, key, err)
	}
	fi.IsLatest = true
	info := objectInfo(bucket, key, fi)
	if !in.skipEvent {
		l.publish(ctx, in.eventName, bucket, info)
	}
	return info, nil
}

// commitVersion makes a version visible and purges the data it replaces.
func (l *layer) commitVersion(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, tmpDir string) error {
	old, err := st.CommitData(ctx, tmpDir, fi, bucket, key)
	if err != nil {
		if tmpDir != "" {
			_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
		}
		return err
	}
	if old != "" && old != fi.DataDir {
		if err := st.DeletePath(ctx, bucket, storage.ObjectDataDir(key, old), true); err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
			l.deps.Logger.WarnContext(ctx, "object: cannot purge replaced data", "bucket", bucket, "key", key, "data_dir", old, "err", err)
		}
	}
	return nil
}

func incompleteBody(want, got int64) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody",
		fmt.Sprintf("You did not provide the number of bytes specified by the Content-Length HTTP header: expected %d, received %d", want, got))
}

// mapBodyErr keeps client body failures distinguishable from storage ones.
func mapBodyErr(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody", "The request body ended before the declared length.")
	}
	return err
}

// PutObject implements Service.
func (l *layer) PutObject(ctx context.Context, in PutObjectInput) (domain.ObjectInfo, error) {
	if err := validateTags(in.Tags); err != nil {
		return domain.ObjectInfo{}, err
	}
	return l.writeObject(ctx, in.Bucket, in.Key, in.Reader, writeInput{
		size:       in.Size,
		md5Hex:     in.MD5Hex,
		sha256Hex:  in.SHA256Hex,
		checksum:   in.Checksum,
		meta:       buildMetadata(in.ContentType, in.UserMetadata, in.Tags, in.StorageClass, domain.Checksum{}),
		conditions: in.Conditions,
		versionID:  in.VersionID,
		modTime:    in.ModTime,
		eventName:  event.ObjectCreatedPut,
	})
}

// --- object reads ------------------------------------------------------------

// readVersion resolves the version a read refers to, mapping delete
// markers to the S3 semantics (latest marker → not found, explicit marker →
// method not allowed).
func (l *layer) readVersion(ctx context.Context, st store, bucket, key, versionID string, readData bool) (storage.FileInfo, error) {
	if versionID == "null" {
		versionID = ""
	}
	fi, err := st.ReadVersion(ctx, bucket, key, versionID, readData)
	if err != nil {
		return storage.FileInfo{}, err
	}
	if fi.Deleted {
		if versionID == "" {
			return fi, domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.").WithResource(bucket + "/" + key)
		}
		return fi, fmt.Errorf("%w: version %s of %s/%s is a delete marker", domain.ErrMethodNotAllowed, versionID, bucket, key)
	}
	return fi, nil
}

// GetObjectInfo implements Service.
func (l *layer) GetObjectInfo(ctx context.Context, bucket, key string, opts GetObjectOptions) (domain.ObjectInfo, error) {
	if err := validBucket(bucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := domain.ValidateObjectKey(key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if !opts.NoLock {
		lc, err := l.lock(ctx, bucket, key, false)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		defer lc.Unlock()
	}
	fi, err := l.readVersion(ctx, l.pick(key), bucket, key, opts.VersionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: head %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	if err := checkReadConditions(info, opts.Conditions); err != nil {
		return info, err
	}
	return info, nil
}

// GetObject implements Service.
func (l *layer) GetObject(ctx context.Context, bucket, key string, opts GetObjectOptions) (*Reader, error) {
	if err := validBucket(bucket); err != nil {
		return nil, err
	}
	if err := domain.ValidateObjectKey(key); err != nil {
		return nil, err
	}
	unlock := func() {}
	if !opts.NoLock {
		lc, err := l.lock(ctx, bucket, key, false)
		if err != nil {
			return nil, err
		}
		unlock = lc.Unlock
	}
	st := l.pick(key)
	fi, err := l.readVersion(ctx, st, bucket, key, opts.VersionID, true)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("%s: get %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	if err := checkReadConditions(info, opts.Conditions); err != nil {
		unlock()
		return nil, err
	}
	offset, length, err := resolveRange(fi, opts)
	if err != nil {
		unlock()
		return nil, err
	}
	rc, err := l.openData(ctx, st, bucket, key, fi, offset, length)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("%s: read %s/%s: %w", l.name, bucket, key, err)
	}
	l.publish(ctx, event.ObjectAccessedGet, bucket, info)
	return &Reader{ReadCloser: &unlockOnClose{ReadCloser: rc, unlock: unlock}, Info: info, Offset: offset, Length: length}, nil
}

// resolveRange turns the range or part number of a read into offset/length.
func resolveRange(fi storage.FileInfo, opts GetObjectOptions) (int64, int64, error) {
	if opts.PartNumber > 0 {
		var start int64
		for _, p := range fi.Parts {
			if p.Number == opts.PartNumber {
				return start, p.Size, nil
			}
			start += p.Size
		}
		return 0, 0, fmt.Errorf("%w: part number %d", domain.ErrInvalidRange, opts.PartNumber)
	}
	if opts.Range == nil {
		return 0, fi.Size, nil
	}
	return opts.Range.Resolve(fi.Size)
}

// openData returns a reader over [offset, offset+length) of a version.
func (l *layer) openData(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, offset, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if fi.DataDir == "" {
		if offset+length > int64(len(fi.Data)) {
			return nil, fmt.Errorf("%w: inline data of %d bytes, want [%d,%d)", domain.ErrFaultyDisk, len(fi.Data), offset, offset+length)
		}
		return io.NopCloser(bytes.NewReader(fi.Data[offset : offset+length])), nil
	}
	return &partsReader{ctx: ctx, st: st, bucket: bucket, key: key, fi: fi, offset: offset, remaining: length}, nil
}

// partsReader streams a byte range across the parts of a version, opening
// one part at a time.
type partsReader struct {
	ctx       context.Context
	st        store
	bucket    string
	key       string
	fi        storage.FileInfo
	offset    int64
	remaining int64
	cur       io.ReadCloser
}

func (r *partsReader) Read(p []byte) (int, error) {
	for {
		if r.remaining <= 0 {
			return 0, io.EOF
		}
		if r.cur == nil {
			if err := r.openNext(); err != nil {
				return 0, err
			}
		}
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.cur.Read(p)
		r.offset += int64(n)
		r.remaining -= int64(n)
		if errors.Is(err, io.EOF) {
			_ = r.cur.Close()
			r.cur = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("object: read %s/%s: %w", r.bucket, r.key, err)
		}
		return n, nil
	}
}

func (r *partsReader) openNext() error {
	var start int64
	for _, part := range r.fi.Parts {
		if part.Size == 0 {
			continue
		}
		if r.offset >= start && r.offset < start+part.Size {
			partOff := r.offset - start
			partLen := part.Size - partOff
			if partLen > r.remaining {
				partLen = r.remaining
			}
			rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, part, partOff, partLen)
			if err != nil {
				return fmt.Errorf("object: open part %d of %s/%s: %w", part.Number, r.bucket, r.key, err)
			}
			r.cur = rc
			return nil
		}
		start += part.Size
	}
	return fmt.Errorf("%w: offset %d beyond the parts of %s/%s", domain.ErrInvalidRange, r.offset, r.bucket, r.key)
}

func (r *partsReader) Close() error {
	if r.cur != nil {
		err := r.cur.Close()
		r.cur = nil
		return err
	}
	return nil
}

// unlockOnClose releases the read lock when the body is closed.
type unlockOnClose struct {
	io.ReadCloser
	unlock func()
	done   bool
}

func (u *unlockOnClose) Close() error {
	err := u.ReadCloser.Close()
	if !u.done {
		u.done = true
		u.unlock()
	}
	return err
}

// --- deletes -------------------------------------------------------------------

// DeleteObject implements Service.
func (l *layer) DeleteObject(ctx context.Context, bucket, key string, opts DeleteObjectOptions) (DeletedObject, error) {
	if err := validBucket(bucket); err != nil {
		return DeletedObject{}, err
	}
	if err := domain.ValidateObjectKey(key); err != nil {
		return DeletedObject{}, err
	}
	lc, err := l.lock(ctx, bucket, key, true)
	if err != nil {
		return DeletedObject{}, err
	}
	defer lc.Unlock()
	st := l.pick(key)
	if _, err := st.StatVol(ctx, bucket); err != nil {
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
	}
	cfg, err := l.bucketConfig(ctx, bucket)
	if err != nil {
		return DeletedObject{}, err
	}
	modTime := opts.ModTime
	if modTime.IsZero() {
		modTime = l.now()
	}
	versionID := opts.VersionID
	if versionID == "null" {
		versionID = ""
	}
	if opts.VersionID != "" {
		return l.deleteVersion(ctx, st, bucket, key, versionID)
	}
	switch cfg.Versioning {
	case domain.VersioningEnabled:
		marker := storage.FileInfo{VersionID: l.newID(), ModTime: modTime, MarkDeleted: true, Versioned: true}
		if err := st.DeleteVersion(ctx, bucket, key, marker); err != nil {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		del := DeletedObject{Key: key, DeleteMarker: true, DeleteMarkerVersionID: marker.VersionID}
		l.publish(ctx, event.ObjectRemovedDeleteMarkerCreated, bucket, domain.ObjectInfo{Bucket: bucket, Key: key, VersionID: marker.VersionID, IsDeleteMarker: true})
		return del, nil
	case domain.VersioningSuspended:
		marker := storage.FileInfo{VersionID: "", ModTime: modTime, MarkDeleted: true, Versioned: true}
		if err := st.DeleteVersion(ctx, bucket, key, marker); err != nil {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		del := DeletedObject{Key: key, DeleteMarker: true, DeleteMarkerVersionID: "null"}
		l.publish(ctx, event.ObjectRemovedDeleteMarkerCreated, bucket, domain.ObjectInfo{Bucket: bucket, Key: key, IsDeleteMarker: true})
		return del, nil
	default:
		cur, err := st.ReadVersion(ctx, bucket, key, "", false)
		if err != nil {
			if errors.Is(err, domain.ErrObjectNotFound) {
				return DeletedObject{Key: key}, nil // S3 deletes are idempotent
			}
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		if err := st.DeleteVersion(ctx, bucket, key, storage.FileInfo{VersionID: cur.VersionID}); err != nil && !errors.Is(err, domain.ErrNoSuchVersion) && !errors.Is(err, domain.ErrObjectNotFound) {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		l.publish(ctx, event.ObjectRemovedDelete, bucket, objectInfo(bucket, key, cur))
		return DeletedObject{Key: key}, nil
	}
}

// deleteVersion permanently removes one version or delete marker.
func (l *layer) deleteVersion(ctx context.Context, st store, bucket, key, versionID string) (DeletedObject, error) {
	cur, err := st.ReadVersion(ctx, bucket, key, versionID, false)
	if err != nil {
		if versionID == "" && errors.Is(err, domain.ErrObjectNotFound) {
			return DeletedObject{}, fmt.Errorf("%w: null version of %s/%s", domain.ErrNoSuchVersion, bucket, key)
		}
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
	}
	if cur.VersionID != versionID {
		return DeletedObject{}, fmt.Errorf("%w: null version of %s/%s", domain.ErrNoSuchVersion, bucket, key)
	}
	if err := st.DeleteVersion(ctx, bucket, key, storage.FileInfo{VersionID: versionID}); err != nil {
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
	}
	reported := versionID
	if reported == "" {
		reported = "null"
	}
	del := DeletedObject{Key: key, VersionID: reported, DeleteMarker: cur.Deleted}
	if cur.Deleted {
		del.DeleteMarkerVersionID = reported
	}
	l.publish(ctx, event.ObjectRemovedDelete, bucket, objectInfo(bucket, key, cur))
	return del, nil
}

// DeleteObjects implements Service.
func (l *layer) DeleteObjects(ctx context.Context, in DeleteObjectsInput) (DeleteObjectsResult, error) {
	if err := validBucket(in.Bucket); err != nil {
		return DeleteObjectsResult{}, err
	}
	if len(in.Objects) == 0 {
		return DeleteObjectsResult{}, fmt.Errorf("%w: no objects to delete", domain.ErrMalformedXML)
	}
	if len(in.Objects) > maxListKeys {
		return DeleteObjectsResult{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: too many keys (max 1000)")
	}
	if _, err := l.sets[0].StatVol(ctx, in.Bucket); err != nil {
		return DeleteObjectsResult{}, fmt.Errorf("%s: delete objects in %s: %w", l.name, in.Bucket, err)
	}
	res := DeleteObjectsResult{}
	for _, o := range in.Objects {
		del, err := l.DeleteObject(ctx, in.Bucket, o.Key, DeleteObjectOptions{VersionID: o.VersionID, BypassGovernance: in.BypassGovernance})
		if err != nil {
			res.Errors = append(res.Errors, DeleteError{Key: o.Key, VersionID: o.VersionID, Err: err})
			continue
		}
		res.Deleted = append(res.Deleted, del)
	}
	return res, nil
}

// --- copy ------------------------------------------------------------------------

// CopyObject implements Service.
func (l *layer) CopyObject(ctx context.Context, in CopyObjectInput) (domain.ObjectInfo, error) {
	for _, b := range []string{in.SrcBucket, in.DstBucket} {
		if err := validBucket(b); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	for _, k := range []string{in.SrcKey, in.DstKey} {
		if err := domain.ValidateObjectKey(k); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	if err := validateTags(in.Tags); err != nil {
		return domain.ObjectInfo{}, err
	}
	same := in.SrcBucket == in.DstBucket && in.SrcKey == in.DstKey
	if same && !in.ReplaceMetadata && !in.ReplaceTags {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
	}
	lc, err := l.lock(ctx, in.DstBucket, in.DstKey, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer lc.Unlock()
	if _, err := l.pick(in.DstKey).StatVol(ctx, in.DstBucket); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: copy to %s/%s: %w", l.name, in.DstBucket, in.DstKey, err)
	}
	// The destination lock protects a self-copy; other sources take a read lock.
	src, err := l.GetObject(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, Conditions: in.SrcConditions, NoLock: same})
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer func() { _ = src.Close() }()

	meta := buildMetadata(src.Info.ContentType, src.Info.UserMetadata, nil, "", domain.Checksum{})
	for k, v := range src.Info.UserMetadata {
		meta[metaUserPrefix+k] = v
	}
	copyStd(meta, src.Info)
	if in.ReplaceMetadata {
		meta = buildMetadata(in.ContentType, in.UserMetadata, nil, "", domain.Checksum{})
	}
	tags := src.Info.Tags
	if in.ReplaceTags {
		tags = in.Tags
	}
	if len(tags) > 0 {
		meta[metaTagging] = encodeTags(tags)
	}
	storageClass := in.StorageClass
	if storageClass == "" {
		storageClass = src.Info.StorageClass
	}
	if storageClass != "" && storageClass != domain.StorageClassStandard {
		meta[metaStorageClass] = storageClass
	}
	return l.writeObject(ctx, in.DstBucket, in.DstKey, src, writeInput{
		size:       src.Length,
		meta:       meta,
		eventName:  event.ObjectCreatedCopy,
		withinLock: true,
	})
}

// copyStd copies the standard HTTP metadata of info into meta.
func copyStd(meta map[string]string, info domain.ObjectInfo) {
	set := func(k, v string) {
		if v != "" {
			meta[k] = v
		}
	}
	set(metaContentEncoding, info.ContentEncoding)
	set(metaCacheControl, info.CacheControl)
	set(metaContentDisposition, info.ContentDisposition)
	set(metaContentLanguage, info.ContentLanguage)
	if !info.Expires.IsZero() {
		meta[metaExpires] = info.Expires.UTC().Format(time.RFC1123)
	}
}

// --- tags ------------------------------------------------------------------------

// PutObjectTags implements Service.
func (l *layer) PutObjectTags(ctx context.Context, bucket, key string, tags map[string]string, opts TagOptions) (domain.ObjectInfo, error) {
	if err := validateTags(tags); err != nil {
		return domain.ObjectInfo{}, err
	}
	return l.updateTags(ctx, bucket, key, opts.VersionID, func(meta map[string]string) {
		if len(tags) == 0 {
			delete(meta, metaTagging)
			return
		}
		meta[metaTagging] = encodeTags(tags)
	}, event.ObjectCreatedPutTagging)
}

// DeleteObjectTags implements Service.
func (l *layer) DeleteObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (domain.ObjectInfo, error) {
	return l.updateTags(ctx, bucket, key, opts.VersionID, func(meta map[string]string) { delete(meta, metaTagging) }, event.ObjectCreatedDeleteTagging)
}

// GetObjectTags implements Service.
func (l *layer) GetObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (map[string]string, error) {
	info, err := l.GetObjectInfo(ctx, bucket, key, GetObjectOptions{VersionID: opts.VersionID})
	if err != nil {
		return nil, err
	}
	if info.Tags == nil {
		return map[string]string{}, nil
	}
	return info.Tags, nil
}

func (l *layer) updateTags(ctx context.Context, bucket, key, versionID string, mutate func(map[string]string), name event.Name) (domain.ObjectInfo, error) {
	if err := validBucket(bucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := domain.ValidateObjectKey(key); err != nil {
		return domain.ObjectInfo{}, err
	}
	lc, err := l.lock(ctx, bucket, key, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer lc.Unlock()
	st := l.pick(key)
	fi, err := l.readVersion(ctx, st, bucket, key, versionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: tag %s/%s: %w", l.name, bucket, key, err)
	}
	if fi.Metadata == nil {
		fi.Metadata = map[string]string{}
	}
	mutate(fi.Metadata)
	if err := st.UpdateMetadata(ctx, bucket, key, fi); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: tag %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	l.publish(ctx, name, bucket, info)
	return info, nil
}

// --- events ------------------------------------------------------------------------

// publish sends a notification; failures are logged, never returned.
func (l *layer) publish(ctx context.Context, name event.Name, bucket string, info domain.ObjectInfo) {
	if l.deps.Events == nil {
		return
	}
	now := l.now()
	e := event.NewEvent(name, l.opts.region, now)
	e.S3.Bucket = event.Bucket{Name: bucket, ARN: "arn:aws:s3:::" + bucket}
	e.S3.Object = event.Object{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		UserMetadata: info.UserMetadata,
		VersionID:    info.VersionID,
		Sequencer:    strconv.FormatInt(now.UnixNano(), 16),
	}
	if err := l.deps.Events.Publish(ctx, e); err != nil {
		l.deps.Logger.DebugContext(ctx, "object: event not published", "event", name, "bucket", bucket, "key", info.Key, "err", err)
	}
}
