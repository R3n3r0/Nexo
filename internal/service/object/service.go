// Package object defines the object layer: the logical S3 API over buckets,
// objects, versions and multipart uploads. Two implementations share the
// contract: the single-drive filesystem layer (NewFS, SNSD) and the
// erasure-coded layer (NewErasure, SNMD / distributed), both selected by the
// composition root from the deployment mode.
package object

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Deps are the dependencies of the object layer implementations. Store is
// used by NewFS, Sets/Parity/DeploymentID by NewErasure; the other fields are
// shared and optional (nil values are replaced by defaults).
type Deps struct {
	// Store is the single local drive of the filesystem layer.
	Store storage.StorageAPI
	// Sets lists the drives of every erasure set of the pool, in set and
	// disk order (topology.Pool.Sets()).
	Sets [][]storage.StorageAPI
	// Parity is the number of parity shards per erasure set.
	Parity int
	// DeploymentID seeds the hash that assigns objects to erasure sets.
	DeploymentID string
	// Buckets provides the bucket settings the layer enforces (versioning,
	// object lock). Nil means every bucket is unversioned.
	Buckets BucketConfigSource
	// Locks serializes access to buckets and objects (default: local locker).
	Locks lock.Locker
	// Events receives bucket notifications; nil disables them.
	Events event.Service
	// IDs generates version identifiers and temporary directory names
	// (default: time-ordered UUID v7). Upload ids embed one of its ids when
	// it is a UUID.
	IDs    topology.IDGenerator
	Clock  clock.Clock
	Logger *slog.Logger
}

// BucketConfig is the subset of bucket settings the object layer enforces.
type BucketConfig struct {
	Versioning domain.Versioning
	// ObjectLock reports that the bucket has object locking enabled.
	ObjectLock bool
	// QuotaBytes is the hard quota in bytes, 0 when unset.
	QuotaBytes uint64
}

// BucketConfigSource provides the bucket settings. The bucket service's
// metadata store implements it; a missing bucket yields the zero config.
type BucketConfigSource interface {
	BucketConfig(ctx context.Context, bucket string) (BucketConfig, error)
}

// Service is the object layer. ETags are returned unquoted (transports
// quote them): the hex MD5 of the content, or "<md5 of the part md5s>-N"
// for an object assembled from N parts. Writes are atomic: a reader sees
// the previous version or the new one, never a partial object.
type Service interface {
	// Buckets. Names follow the S3 (DNS) rules; the system volume
	// (domain.SystemBucket) is reserved. Buckets are listed by name, with
	// the creation time recorded at MakeBucket.
	MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error
	GetBucketInfo(ctx context.Context, bucket string) (domain.BucketInfo, error)
	ListBuckets(ctx context.Context) ([]domain.BucketInfo, error)
	DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error

	// Objects. Reads evaluate opts.Conditions with the RFC 9110 (7232)
	// precedence and fail with domain.ErrPreconditionFailed or
	// domain.ErrNotModified; GetObjectInfo then also returns the object
	// description, so that a 304 can carry its ETag and Last-Modified.
	PutObject(ctx context.Context, in PutObjectInput) (domain.ObjectInfo, error)
	GetObject(ctx context.Context, bucket, key string, opts GetObjectOptions) (*Reader, error)
	GetObjectInfo(ctx context.Context, bucket, key string, opts GetObjectOptions) (domain.ObjectInfo, error)
	DeleteObject(ctx context.Context, bucket, key string, opts DeleteObjectOptions) (DeletedObject, error)
	DeleteObjects(ctx context.Context, in DeleteObjectsInput) (DeleteObjectsResult, error)
	CopyObject(ctx context.Context, in CopyObjectInput) (domain.ObjectInfo, error)
	// ListObjects serves v1 (Marker/NextMarker) and v2 (opaque
	// ContinuationToken, StartAfter) listings; encoding-type is applied by
	// the transports.
	ListObjects(ctx context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error)
	ListObjectVersions(ctx context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error)

	// Multipart. Upload ids are URL-safe; parts are numbered 1-10000 and
	// every part but the last must reach the minimum part size (5 MiB).
	NewMultipartUpload(ctx context.Context, in NewMultipartInput) (domain.MultipartUpload, error)
	PutObjectPart(ctx context.Context, in PutObjectPartInput) (domain.Part, error)
	CopyObjectPart(ctx context.Context, in CopyObjectPartInput) (domain.Part, error)
	ListObjectParts(ctx context.Context, in domain.ListPartsInput) (domain.ListPartsResult, error)
	ListMultipartUploads(ctx context.Context, in domain.ListMultipartUploadsInput) (domain.ListMultipartUploadsResult, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
	CompleteMultipartUpload(ctx context.Context, in CompleteMultipartInput) (domain.ObjectInfo, error)

	// Tags.
	PutObjectTags(ctx context.Context, bucket, key string, tags map[string]string, opts TagOptions) (domain.ObjectInfo, error)
	GetObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (map[string]string, error)
	DeleteObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (domain.ObjectInfo, error)

	// CleanupStaleUploads aborts the multipart uploads of bucket (every
	// bucket when empty) initiated more than olderThan ago, and removes the
	// orphaned upload directories of the same age. It returns the number of
	// uploads removed. Scheduling belongs to the caller (scanner, ILM).
	CleanupStaleUploads(ctx context.Context, bucket string, olderThan time.Duration) (int, error)

	// DataUsage walks the buckets and returns them with their usage
	// counters (Objects, Versions, DeleteMarkers, Size) filled in, sorted by
	// name. An empty bucket selects every bucket; otherwise the result holds
	// that bucket only, or fails with domain.ErrBucketNotFound. The walk is
	// proportional to the number of objects: callers cache the result.
	DataUsage(ctx context.Context, bucket string) ([]domain.BucketInfo, error)

	// Health reports the storage health (quorum, online drives).
	Health(ctx context.Context, opts HealthOptions) HealthResult
}

// MakeBucketOptions tunes MakeBucket.
type MakeBucketOptions struct {
	Region string
	// LockEnabled creates the bucket with object locking (implies versioning).
	LockEnabled bool
	// Versioning sets the initial versioning state.
	Versioning domain.Versioning
	// ForceCreate skips the "already exists" check (bootstrap, healing).
	ForceCreate bool
}

// DeleteBucketOptions tunes DeleteBucket.
type DeleteBucketOptions struct {
	// Force removes a non-empty bucket.
	Force bool
}

// PutObjectInput are the parameters of PutObject.
type PutObjectInput struct {
	Bucket string
	Key    string
	// Reader provides the content. Size is -1 when unknown (chunked).
	Reader io.Reader
	Size   int64
	// MD5Hex and SHA256Hex are the client-declared checksums to verify.
	MD5Hex    string
	SHA256Hex string
	Checksum  domain.Checksum
	// ContentType defaults to application/octet-stream.
	ContentType string
	// ContentEncoding, ContentDisposition, ContentLanguage, CacheControl
	// and Expires (the raw header value) are the standard headers stored
	// with the object and returned on reads.
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	Expires            string
	// UserMetadata holds the x-amz-meta-* headers. Keys are
	// case-insensitive: they are stored lowercase, without the
	// x-amz-meta- prefix (stripped when present). For compatibility the
	// keys cache-control, content-disposition, content-encoding,
	// content-language and expires are stored as the standard header of
	// the same name unless the corresponding field above is set.
	UserMetadata map[string]string
	// Tags are validated as S3 object tags (at most 10).
	Tags         map[string]string
	StorageClass string
	// Conditions are the write preconditions (If-None-Match: * etc.).
	Conditions domain.Conditions
	// VersionID forces a version id (replication); normally empty.
	VersionID string
	// ModTime forces the modification time; zero means now.
	ModTime time.Time
	// LengthRange, when set, bounds the content length (the
	// content-length-range of a POST policy): a body outside [Min, Max]
	// fails with domain.ErrEntityTooSmall or domain.ErrEntityTooLarge and
	// nothing is committed.
	LengthRange *LengthRange
}

// LengthRange is an inclusive range of content lengths, in bytes.
type LengthRange struct {
	Min int64
	Max int64
}

// GetObjectOptions tunes GetObject and GetObjectInfo.
type GetObjectOptions struct {
	VersionID string
	// Range is nil for the whole object.
	Range      *domain.Range
	Conditions domain.Conditions
	// PartNumber selects one part of a multipart object (0 = all).
	PartNumber int
	// NoLock skips the read lock (internal callers).
	NoLock bool
}

// Reader is an object body with its metadata. Callers must Close it.
type Reader struct {
	io.ReadCloser
	Info domain.ObjectInfo
	// Offset and Length describe the served range.
	Offset int64
	Length int64
}

// DeleteObjectOptions tunes DeleteObject.
type DeleteObjectOptions struct {
	VersionID string
	// BypassGovernance allows deleting a governance-locked version.
	BypassGovernance bool
	// ModTime forces the delete marker time.
	ModTime time.Time
}

// ObjectToDelete identifies one object in DeleteObjects.
type ObjectToDelete struct {
	Key       string
	VersionID string
}

// DeleteObjectsInput are the parameters of DeleteObjects (at most 1000
// objects).
type DeleteObjectsInput struct {
	Bucket  string
	Objects []ObjectToDelete
	// Quiet leaves the successful deletions out of the result: only the
	// errors are reported (S3 quiet mode).
	Quiet            bool
	BypassGovernance bool
}

// DeletedObject describes a successfully deleted object or version.
type DeletedObject struct {
	Key       string
	VersionID string
	// DeleteMarker reports that a delete marker was created (or removed).
	DeleteMarker          bool
	DeleteMarkerVersionID string
}

// DeleteError is a per-object failure of DeleteObjects.
type DeleteError struct {
	Key       string
	VersionID string
	Err       error
}

// DeleteObjectsResult is the result of DeleteObjects: one entry per
// requested object, in request order, in either Deleted or Errors.
type DeleteObjectsResult struct {
	Deleted []DeletedObject
	Errors  []DeleteError
}

// CopyObjectInput are the parameters of CopyObject. A source larger than
// 5 GiB is refused (InvalidRequest): such objects are copied part by part.
type CopyObjectInput struct {
	SrcBucket    string
	SrcKey       string
	SrcVersionID string
	// SrcConditions apply to the source (x-amz-copy-source-if-*); every
	// failure is domain.ErrPreconditionFailed, never ErrNotModified.
	SrcConditions domain.Conditions
	DstBucket     string
	DstKey        string
	// ReplaceMetadata (metadata directive REPLACE) stores ContentType, the
	// standard headers and UserMetadata (same rules as PutObjectInput)
	// instead of copying those of the source.
	ReplaceMetadata    bool
	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	Expires            string
	UserMetadata       map[string]string
	// ReplaceTags uses Tags instead of copying them.
	ReplaceTags  bool
	Tags         map[string]string
	StorageClass string
}

// NewMultipartInput are the parameters of NewMultipartUpload. The metadata
// fields follow the rules of PutObjectInput.
type NewMultipartInput struct {
	Bucket             string
	Key                string
	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	Expires            string
	UserMetadata       map[string]string
	Tags               map[string]string
	StorageClass       string
	Checksum           domain.ChecksumAlgorithm
}

// PutObjectPartInput are the parameters of PutObjectPart.
type PutObjectPartInput struct {
	Bucket     string
	Key        string
	UploadID   string
	PartNumber int
	Reader     io.Reader
	Size       int64
	MD5Hex     string
	SHA256Hex  string
	Checksum   domain.Checksum
}

// CopyObjectPartInput are the parameters of CopyObjectPart.
type CopyObjectPartInput struct {
	SrcBucket    string
	SrcKey       string
	SrcVersionID string
	// SrcRange (x-amz-copy-source-range) must lie within the source: a
	// range ending beyond it fails with domain.ErrInvalidRange.
	SrcRange      *domain.Range
	SrcConditions domain.Conditions
	DstBucket     string
	DstKey        string
	UploadID      string
	PartNumber    int
}

// CompleteMultipartInput are the parameters of CompleteMultipartUpload.
type CompleteMultipartInput struct {
	Bucket     string
	Key        string
	UploadID   string
	Parts      []domain.CompletePart
	Conditions domain.Conditions
	Checksum   domain.Checksum
}

// TagOptions tunes the tag operations.
type TagOptions struct {
	VersionID string
}

// HealthOptions tunes Health.
type HealthOptions struct {
	// Maintenance checks whether one more drive/node can go down safely.
	Maintenance bool
}

// HealthResult is the storage health.
type HealthResult struct {
	Healthy bool
	// HealingDrives is the number of drives being healed.
	HealingDrives int
	// WriteQuorum and ReadQuorum are the minimum healthy drives per set.
	WriteQuorum int
	ReadQuorum  int
	// Message explains an unhealthy state.
	Message string
}
