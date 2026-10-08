package cli

import (
	"context"
	"io"
	"iter"
	"net/http"
	"time"
)

// S3Client is the S3 surface used by the nexoctl commands (docs/12 §12.5).
// The default implementation (NewS3Factory) is built on aws-sdk-go-v2;
// internal/cli/s3mem is the deterministic in-memory implementation used by
// the unit tests of the commands. Errors wrap the sentinels of domain
// (domain.ErrBucketNotFound, domain.ErrObjectNotFound,
// domain.ErrAccessDenied, ...) so that the commands map them to stable
// exit codes (ExitCode).
type S3Client interface {
	// ListBuckets returns the buckets sorted by name.
	ListBuckets(ctx context.Context) ([]BucketInfo, error)
	// BucketExists reports whether the bucket exists (HeadBucket).
	BucketExists(ctx context.Context, bucket string) (bool, error)
	// MakeBucket creates a bucket.
	MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error
	// RemoveBucket deletes an empty bucket.
	RemoveBucket(ctx context.Context, bucket string) error

	// List iterates over the objects of a bucket in key order. Without
	// Recursive, the keys below the next "/" after the prefix are folded
	// into a common prefix (ObjectInfo.Prefix). With Versions, every
	// version and delete marker is returned, the latest first for each key.
	// The iteration stops at the first error, which is yielded last.
	List(ctx context.Context, bucket string, opts ListOptions) iter.Seq2[ObjectInfo, error]
	// Stat returns the metadata of an object (HeadObject).
	Stat(ctx context.Context, bucket, key string, opts StatOptions) (*ObjectInfo, error)
	// Get opens an object (or a range of it); the caller closes the reader.
	Get(ctx context.Context, bucket, key string, opts GetOptions) (io.ReadCloser, *ObjectInfo, error)
	// Put uploads body as bucket/key. Objects larger than one part (or of
	// unknown size, PutOptions.Size < 0) are sent as a multipart upload
	// whose parts are uploaded in parallel; when the upload fails or ctx is
	// canceled the multipart upload is aborted and the error is an
	// *AbortedUploadError.
	Put(ctx context.Context, bucket, key string, body io.Reader, opts PutOptions) (*PutResult, error)
	// Copy copies an object server-side (CopyObject, or UploadPartCopy for
	// objects larger than MaxCopySize).
	Copy(ctx context.Context, src, dst ObjectRef, opts CopyOptions) (*PutResult, error)
	// Remove deletes objects or object versions in batches (DeleteObjects).
	// The per-object outcome is in the results; the error reports a failure
	// of the whole request.
	Remove(ctx context.Context, bucket string, objects []ObjectVersion) ([]RemoveResult, error)

	// ListUploads iterates over the multipart uploads in progress under
	// prefix.
	ListUploads(ctx context.Context, bucket, prefix string) iter.Seq2[UploadInfo, error]
	// AbortUpload aborts a multipart upload.
	AbortUpload(ctx context.Context, bucket, key, uploadID string) error

	// Presign returns a presigned URL signed locally with the credentials
	// of the alias (raw, non-revocable: docs/09 §9.1).
	Presign(ctx context.Context, in PresignInput) (*PresignedRequest, error)
	// PostPolicy returns the form fields of a browser upload (POST policy,
	// docs/09 §9.2), signed locally with the credentials of the alias.
	PostPolicy(ctx context.Context, in PostPolicyInput) (*PostPolicyForm, error)

	// GetBucketPolicy returns the bucket policy document; a bucket without
	// policy is an error wrapping domain.ErrPolicyNotFound.
	GetBucketPolicy(ctx context.Context, bucket string) (string, error)
	// PutBucketPolicy replaces the bucket policy.
	PutBucketPolicy(ctx context.Context, bucket, policy string) error
	// DeleteBucketPolicy removes the bucket policy.
	DeleteBucketPolicy(ctx context.Context, bucket string) error

	// GetVersioning returns the versioning state of a bucket.
	GetVersioning(ctx context.Context, bucket string) (Versioning, error)
	// SetVersioning enables or suspends versioning.
	SetVersioning(ctx context.Context, bucket string, v Versioning) error

	// GetTags returns the tags of a bucket (empty ref.Key) or of an object
	// version; no tags is an empty map.
	GetTags(ctx context.Context, ref ObjectRef) (map[string]string, error)
	// PutTags replaces the tags of a bucket or object.
	PutTags(ctx context.Context, ref ObjectRef, tags map[string]string) error
	// DeleteTags removes the tags of a bucket or object.
	DeleteTags(ctx context.Context, ref ObjectRef) error
}

// S3Factory builds the S3 client of an alias: the commands receive it in
// their dependencies so that the tests can substitute the client.
type S3Factory interface {
	Client(ctx context.Context, a Alias, opts ClientOptions) (S3Client, error)
}

// HTTPDoer sends HTTP requests (*http.Client implements it); the context
// travels in the request.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClientOptions tune one client of S3Factory.
type ClientOptions struct {
	// Debug, when not nil, receives a dump of every HTTP request and
	// response (headers only), with the credentials redacted.
	Debug io.Writer
}

// S3 limits (multipart uploads, presigned URLs).
const (
	// MinPartSize is the smallest part of a multipart upload but the last.
	MinPartSize int64 = 5 << 20
	// MaxPartSize is the largest part of a multipart upload.
	MaxPartSize int64 = 5 << 30
	// MaxParts is the maximum number of parts of a multipart upload.
	MaxParts = 10000
	// MaxObjectSize is the largest object S3 stores.
	MaxObjectSize int64 = 5 << 40
	// DefaultPartSize is the part size of uploads that need no larger
	// parts.
	DefaultPartSize int64 = 16 << 20
	// MaxCopySize is the largest object CopyObject copies in one request.
	MaxCopySize int64 = 5 << 30
	// DefaultParallel is the default number of parts uploaded at once.
	DefaultParallel = 4
	// MaxPresignExpiry is the longest validity of a presigned URL.
	MaxPresignExpiry = 7 * 24 * time.Hour
	// DefaultRegion is the region of the aliases that set none.
	DefaultRegion = "us-east-1"
)

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name    string
	Created time.Time
}

// MakeBucketOptions tune MakeBucket.
type MakeBucketOptions struct {
	// Region is the location constraint ("" uses the alias region).
	Region string
}

// ListOptions select the objects of List.
type ListOptions struct {
	Prefix    string
	Recursive bool
	Versions  bool
}

// ObjectInfo describes an object, an object version, a delete marker or a
// common prefix of a non-recursive listing.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
	VersionID    string
	IsLatest     bool
	DeleteMarker bool
	// Prefix reports a common prefix ("directory"): only Key is set.
	Prefix bool
	// Metadata holds the user metadata (x-amz-meta-*, without the prefix,
	// lower-case keys) returned by Stat and Get.
	Metadata     map[string]string
	StorageClass string
}

// StatOptions tune Stat.
type StatOptions struct {
	VersionID string
}

// ByteRange is a range of an object: Length 0 reads up to the end.
type ByteRange struct {
	Offset int64
	Length int64
}

// GetOptions tune Get.
type GetOptions struct {
	VersionID string
	Range     *ByteRange
}

// PutOptions tune Put.
type PutOptions struct {
	// Size is the length of the body, -1 when unknown (streams): the body
	// is then uploaded in parts of growing size (StreamPartSize).
	Size        int64
	ContentType string
	Metadata    map[string]string
	Tags        map[string]string
	// PartSize forces the part size (0: computed with PartSize).
	PartSize int64
	// Parallel is the number of parts uploaded at once (0:
	// DefaultParallel).
	Parallel int
	// Progress, when not nil, is called with the number of bytes of each
	// part (or of the whole object) once uploaded. It may be called from
	// several goroutines.
	Progress func(n int64)
}

// PutResult describes an uploaded or copied object.
type PutResult struct {
	ETag      string
	VersionID string
	Size      int64
	// Parts is the number of parts of a multipart upload (0: single PUT).
	Parts int
}

// ObjectRef names a bucket, an object or an object version.
type ObjectRef struct {
	Bucket    string
	Key       string
	VersionID string
}

// CopyOptions tune Copy.
type CopyOptions struct {
	// Metadata, when not nil, replaces the user metadata of the copy
	// (REPLACE directive); otherwise the metadata of the source is kept.
	Metadata map[string]string
	// ContentType replaces the content type of the copy (with the
	// metadata).
	ContentType string
	// Tags, when not nil, replaces the tags of the copy.
	Tags map[string]string
}

// ObjectVersion names an object (empty VersionID: the latest version) for
// Remove.
type ObjectVersion struct {
	Key       string
	VersionID string
}

// RemoveResult is the outcome of the removal of one object.
type RemoveResult struct {
	Key       string
	VersionID string
	// DeleteMarker reports that the removal created (or removed) a delete
	// marker, whose version is DeleteMarkerVersionID.
	DeleteMarker          bool
	DeleteMarkerVersionID string
	// Err is the failure of this object, nil on success.
	Err error
}

// UploadInfo describes a multipart upload in progress.
type UploadInfo struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// PresignInput describes a presigned request.
type PresignInput struct {
	// Method is GET, HEAD, PUT or DELETE.
	Method    string
	Bucket    string
	Key       string
	VersionID string
	Expiry    time.Duration
	// Headers are signed: the client must send them unchanged (PUT:
	// Content-Type, Cache-Control, Content-Disposition, Content-Encoding,
	// Content-Language, x-amz-meta-*).
	Headers map[string]string
	// ResponseContentDisposition and ResponseContentType override the
	// headers of a GET response.
	ResponseContentDisposition string
	ResponseContentType        string
}

// PresignedRequest is a presigned URL and the headers to send with it.
type PresignedRequest struct {
	Method    string
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

// PostPolicyInput describes a browser upload (POST policy).
type PostPolicyInput struct {
	Bucket string
	// Key is the exact key of the upload; when empty the key must start
	// with KeyPrefix and the form proposes KeyPrefix + "${filename}".
	Key       string
	KeyPrefix string
	Expiry    time.Duration
	// MinSize and MaxSize add a content-length-range condition when
	// MaxSize > 0.
	MinSize int64
	MaxSize int64
	// ContentType, when set, is required as the Content-Type field.
	ContentType string
}

// PostPolicyForm is the URL and the fields of a browser upload: the file is
// sent as the last field, named "file".
type PostPolicyForm struct {
	URL       string
	Fields    map[string]string
	ExpiresAt time.Time
}

// Versioning is the versioning state of a bucket.
type Versioning string

// Versioning states.
const (
	// VersioningOff is a bucket that never had versioning.
	VersioningOff       Versioning = ""
	VersioningEnabled   Versioning = "Enabled"
	VersioningSuspended Versioning = "Suspended"
)
