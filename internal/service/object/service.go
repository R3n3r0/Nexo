// Package object defines the object layer: the logical S3 API over buckets,
// objects, versions and multipart uploads. Two implementations share the
// contract: the single-drive filesystem layer (SNSD) and the erasure-coded
// layer (SNMD / distributed).
//
//go:generate mockery --name Service --output ./mock --outpkg mock --with-expecter
package object

import (
	"context"
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service is the object layer.
type Service interface {
	// Buckets.
	MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error
	GetBucketInfo(ctx context.Context, bucket string) (domain.BucketInfo, error)
	ListBuckets(ctx context.Context) ([]domain.BucketInfo, error)
	DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error

	// Objects.
	PutObject(ctx context.Context, in PutObjectInput) (domain.ObjectInfo, error)
	GetObject(ctx context.Context, bucket, key string, opts GetObjectOptions) (*Reader, error)
	GetObjectInfo(ctx context.Context, bucket, key string, opts GetObjectOptions) (domain.ObjectInfo, error)
	DeleteObject(ctx context.Context, bucket, key string, opts DeleteObjectOptions) (DeletedObject, error)
	DeleteObjects(ctx context.Context, in DeleteObjectsInput) (DeleteObjectsResult, error)
	CopyObject(ctx context.Context, in CopyObjectInput) (domain.ObjectInfo, error)
	ListObjects(ctx context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error)
	ListObjectVersions(ctx context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error)

	// Multipart.
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
	// ContentType and UserMetadata are stored with the object.
	ContentType  string
	UserMetadata map[string]string
	Tags         map[string]string
	StorageClass string
	// Conditions are the write preconditions (If-None-Match: * etc.).
	Conditions domain.Conditions
	// VersionID forces a version id (replication); normally empty.
	VersionID string
	// ModTime forces the modification time; zero means now.
	ModTime time.Time
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

// DeleteObjectsInput are the parameters of DeleteObjects.
type DeleteObjectsInput struct {
	Bucket  string
	Objects []ObjectToDelete
	// Quiet suppresses successful entries in the response.
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

// DeleteObjectsResult is the result of DeleteObjects.
type DeleteObjectsResult struct {
	Deleted []DeletedObject
	Errors  []DeleteError
}

// CopyObjectInput are the parameters of CopyObject.
type CopyObjectInput struct {
	SrcBucket    string
	SrcKey       string
	SrcVersionID string
	// SrcConditions apply to the source (x-amz-copy-source-if-*).
	SrcConditions domain.Conditions
	DstBucket     string
	DstKey        string
	// ReplaceMetadata uses UserMetadata/ContentType instead of copying them.
	ReplaceMetadata bool
	ContentType     string
	UserMetadata    map[string]string
	// ReplaceTags uses Tags instead of copying them.
	ReplaceTags  bool
	Tags         map[string]string
	StorageClass string
}

// NewMultipartInput are the parameters of NewMultipartUpload.
type NewMultipartInput struct {
	Bucket       string
	Key          string
	ContentType  string
	UserMetadata map[string]string
	Tags         map[string]string
	StorageClass string
	Checksum     domain.ChecksumAlgorithm
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
	SrcBucket     string
	SrcKey        string
	SrcVersionID  string
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
