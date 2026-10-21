// Package object defines the object layer: the logical S3 API over buckets,
// objects, versions and multipart uploads. Two implementations share the
// contract: the single-drive filesystem layer (NewFS, SNSD) and the
// erasure-coded layer (NewServerPools, SNMD / distributed: server pools of
// erasure sets of drives, docs/03), both selected by the composition root
// from the deployment mode. NewErasure is the single-pool shorthand of
// NewServerPools.
package object

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/erasure"
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
	// Heal receives the damaged objects found by the erasure layer (see
	// PoolsDeps.Heal); nil means no healing.
	Heal HealNotifier
	// Codecs builds the erasure codecs of NewErasure (required there, see
	// PoolsDeps.Codecs).
	Codecs erasure.Factory
	// Crypto seals the object keys of the encrypted objects (see
	// PoolsDeps.Crypto); nil refuses server-side encryption.
	Crypto crypto.Service
	// IDs generates version identifiers and temporary directory names
	// (default: time-ordered UUID v7). Upload ids embed one of its ids when
	// it is a UUID.
	IDs    topology.IDGenerator
	Clock  clock.Clock
	Logger *slog.Logger
}

// Healer repairs the buckets and objects of the erasure layer (docs/03
// §3.7): the layers of NewServerPools and NewErasure implement it (the
// single-drive layer has nothing to rebuild). The healing service drives
// it from its queue (heal-on-read, MRF), the scanner, drive replacements
// and the Admin API. Healing an object takes its write lock.
type Healer interface {
	// HealBucket creates the bucket (volume and record) on the online
	// drives that miss it, in every set.
	HealBucket(ctx context.Context, bucket string, opts HealOptions) (HealResult, error)
	// HealObject rebuilds one version of bucket/key ("" is the latest,
	// "null" the null version; opts.AllVersions heals every version) on
	// the online drives of its set that miss it, hold an outdated or
	// corrupt copy, or lack shard files. A version that a quorum of drives
	// miss and too few drives hold to be rebuilt is dangling: it is
	// reported, and removed with opts.Remove. A key no drive holds fails
	// with domain.ErrObjectNotFound.
	HealObject(ctx context.Context, bucket, key, versionID string, opts HealOptions) (HealResult, error)
}

// PoolMover moves objects between the server pools of the erasure layer
// (docs/03 §3.8): the decommission of a pool and the rebalance between
// pools. The layer of NewServerPools implements it.
//
// A move never loses data: every version of the key is copied to the
// target pool, read back and compared with the source, and only then
// removed from the source, under the write lock of the key (writes,
// deletes and reads of the key wait); a failure leaves the source as it
// was, and copying again is harmless.
type PoolMover interface {
	// PoolUsage returns the capacity of every pool.
	PoolUsage(ctx context.Context) []PoolUsage
	// SetPoolDraining excludes a pool from the placement of new objects,
	// new versions and new multipart uploads (draining), or makes it
	// eligible again. Its objects stay readable and deletable.
	SetPoolDraining(pool int, draining bool)
	// PoolKeys returns, in key order, up to max keys of bucket after
	// `after` with at least one version in pool.
	PoolKeys(ctx context.Context, pool int, bucket, after string, max int) ([]string, error)
	// MoveObject moves the versions of bucket/key held by in.From to
	// another pool. A key without versions in in.From is not an error
	// (Versions is 0).
	MoveObject(ctx context.Context, in MoveObjectInput) (MoveObjectResult, error)
	// PoolUploads counts the multipart uploads in progress in pool.
	PoolUploads(ctx context.Context, pool int) (int, error)
}

// PoolUsage is the capacity of the online drives of a pool.
type PoolUsage struct {
	Index    int
	Total    uint64
	Free     uint64
	Draining bool
}

// MoveObjectInput are the parameters of PoolMover.MoveObject.
type MoveObjectInput struct {
	Bucket string
	Key    string
	// From is the pool the versions leave.
	From int
	// Targets restricts the pools a key goes to when no other pool holds
	// versions of it yet (nil: every pool but From that is not draining);
	// a key with versions in another pool joins them there.
	Targets []int
}

// MoveObjectResult reports a move.
type MoveObjectResult struct {
	// To is the pool the versions went to (-1 when nothing moved).
	To int
	// Versions counts the versions moved (delete markers included) and
	// Bytes their stored size.
	Versions int
	Bytes    int64
}

// HealOptions tunes the healing operations.
type HealOptions struct {
	// Deep verifies the bitrot hash of every shard block (reads all the
	// data); otherwise shard files are only checked for presence and size.
	Deep bool
	// DryRun reports what would be healed without writing anything.
	DryRun bool
	// Remove deletes the dangling versions found.
	Remove bool
	// AllVersions heals every version of the key (HealObject).
	AllVersions bool
}

// HealResult reports a healing operation.
type HealResult struct {
	Bucket string
	Key    string
	// VersionIDs are the versions examined (HealObject).
	VersionIDs []string
	// Drives is the number of drives involved and Offline those that
	// could not be reached (they are healed when they come back).
	Drives  int
	Offline int
	// Healed counts the drive copies rebuilt (or that would be, with
	// DryRun), across the versions examined.
	Healed int
	// Dangling counts the dangling versions found; Removed those deleted.
	Dangling int
	Removed  int
	// DriveStates reports, for every drive involved (the drives of the
	// object's set, of every set for HealBucket), the state of its copy
	// before and after the operation; across several versions the worst
	// state is kept. With DryRun, After is Before.
	DriveStates []HealDrive
	// Bytes is the logical size of the versions examined (HealObject).
	Bytes int64
	// Orphans counts the data directories that no version references,
	// removed by HealObject with AllVersions once older than
	// OrphanDataDirGrace.
	Orphans int
}

// HealDriveState is the state of the copy of a bucket or of an object
// version on one drive.
type HealDriveState string

// Drive states of a healing operation, from the best to the worst.
const (
	// HealDriveOK is a complete, intact copy.
	HealDriveOK HealDriveState = "ok"
	// HealDriveMissing is a copy that is absent or outdated (the drive
	// missed a write, or lacks shard files).
	HealDriveMissing HealDriveState = "missing"
	// HealDriveCorrupt is a copy that is present but damaged: bitrot,
	// truncated shard, unreadable or diverging metadata.
	HealDriveCorrupt HealDriveState = "corrupt"
	// HealDriveOffline is a drive that could not be reached.
	HealDriveOffline HealDriveState = "offline"
)

// HealDrive is the outcome of a healing operation on one drive.
type HealDrive struct {
	// Endpoint is the drive endpoint ("" for a drive never connected).
	Endpoint string `json:"endpoint"`
	// Pool, Set and Disk are the 0-based position of the drive.
	Pool   int            `json:"pool"`
	Set    int            `json:"set"`
	Disk   int            `json:"disk"`
	Before HealDriveState `json:"before"`
	After  HealDriveState `json:"after"`
}

// PoolsDeps are the dependencies of the erasure object layer
// (NewServerPools). The drives are storage.StorageAPI values, local or
// remote: the layer does not know which.
type PoolsDeps struct {
	// Pools lists the server pools in command line order (at least one).
	Pools []PoolSpec
	// DeploymentID seeds the placement of objects on erasure sets and the
	// bitrot key of the shards (format.json).
	DeploymentID string
	// Locks serializes access to buckets and objects: the local locker in
	// SNMD, the distributed one (dsync) in a cluster (default: local).
	Locks lock.Locker
	// Heal receives the objects found damaged by reads (heal-on-read) and
	// the writes that missed some drive (MRF). Nil means no healing.
	Heal HealNotifier
	// Buckets provides the bucket settings (versioning, object lock,
	// quota). Nil means every bucket is unversioned.
	Buckets BucketConfigSource
	// Events receives bucket notifications; nil disables them.
	Events event.Service
	// Codecs builds the erasure codecs (required): erasure.NewFactory
	// keyed with the same DeploymentID (erasure.WithDeploymentID), whose
	// bitrot hashes the shards carry.
	Codecs erasure.Factory
	// Crypto seals and unseals the object keys of the encrypted objects
	// (server-side encryption, S-OBJ-13): the data is encrypted before
	// the erasure coding. Nil refuses every encryption request (S3
	// NotImplemented) and every read of an encrypted object.
	Crypto crypto.Service
	// IDs generates version identifiers and temporary directory names
	// (default: time-ordered UUID v7).
	IDs    topology.IDGenerator
	Clock  clock.Clock
	Logger *slog.Logger
}

// PoolSpec describes one server pool.
type PoolSpec struct {
	// Sets lists the drives of every erasure set of the pool, in set and
	// disk order (topology.Pool.Sets()); every set has the same size
	// (2..16 drives). A nil drive is an offline one.
	Sets [][]storage.StorageAPI
	// Parity is the parity of the STANDARD storage class (0 selects the
	// default of the set size, docs/03 §3.3); WithStorageClass overrides it.
	Parity int
	// Decommissioning excludes the pool from the placement of new objects
	// (its objects stay readable and writable).
	Decommissioning bool
}

// BucketConfig is the subset of bucket settings the object layer enforces.
type BucketConfig struct {
	Versioning domain.Versioning
	// ObjectLock reports that the bucket has object locking enabled.
	ObjectLock bool
	// QuotaBytes is the hard quota in bytes, 0 when unset.
	QuotaBytes uint64
	// DefaultRetention is the default retention of the versions written
	// to an object lock bucket (zero when none).
	DefaultRetention domain.DefaultRetention
	// Encryption is the default encryption of the bucket (S-BKT-11),
	// applied to the writes that ask for none (zero when none).
	Encryption domain.BucketEncryption
}

// BucketConfigSource provides the bucket settings. The bucket service's
// metadata store implements it; a missing bucket yields the zero config.
type BucketConfigSource interface {
	BucketConfig(ctx context.Context, bucket string) (BucketConfig, error)
}

// HealNotifier receives the objects the erasure layer found damaged: a
// read that met missing, outdated or corrupt shards or metadata (served
// anyway from the healthy ones: heal-on-read), or a write committed with
// quorum but not on every drive (MRF, most recently failed). The healing
// service queues them; the call must not block the request. The default
// is a no-op.
type HealNotifier interface {
	NotifyHeal(ctx context.Context, bucket, object, versionID string)
}

// Service is the object layer. ETags are returned unquoted (transports
// quote them): the hex MD5 of the content, or "<md5 of the part md5s>-N"
// for an object assembled from N parts (an opaque value of the same form
// for the SSE-C and SSE-KMS objects). Writes are atomic: a reader sees
// the previous version or the new one, never a partial object.
//
// Server-side encryption (S-OBJ-13): a write encrypts the new version with
// the encryption it requests (SSE fields of the inputs) or, without one,
// the default encryption of the bucket (BucketConfig.Encryption); sizes,
// ranges, checksums and listings are those of the plaintext. Reading an
// SSE-C version (GetObject, GetObjectInfo, the source of a copy) needs its
// key: without it the read fails with S3 InvalidRequest, with another key
// with S3 AccessDenied; an SSE-C key sent for another version is S3
// InvalidRequest. SSE-S3 and SSE-KMS need a KMS (domain.ErrKMSNotConfigured
// otherwise).
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

	// Object Lock (docs/02 S-BKT-08, S-OBJ-12). The retention and the
	// legal hold of a version are set when it is written (the request's
	// or, for the retention, the bucket default) and changed in place;
	// both need a bucket created with object locking (S3 InvalidRequest
	// otherwise). A version under an active retention or a legal hold
	// cannot be deleted (DeleteObject, DeleteObjects, DeleteBucket with
	// Force, the healing of dangling versions): COMPLIANCE never, not even
	// for root, GOVERNANCE only with opts.BypassGovernance; such failures
	// wrap domain.ErrObjectLocked (S3 AccessDenied). A retention may always
	// be extended; shortening or removing an active one is allowed in
	// GOVERNANCE mode with opts.BypassGovernance only. The callers decide
	// whether a request may bypass the governance mode
	// (s3:BypassGovernanceRetention).
	//
	// GetObjectRetention and GetObjectLegalHold fail with an S3
	// NoSuchObjectLockConfiguration error (wrapping
	// domain.ErrPolicyNotFound) when the version has none.
	GetObjectRetention(ctx context.Context, bucket, key string, opts LockOptions) (domain.ObjectRetention, error)
	// PutObjectRetention sets the retention of a version; the zero
	// retention removes it. A new retention must end in the future.
	PutObjectRetention(ctx context.Context, bucket, key string, retention domain.ObjectRetention, opts LockOptions) (domain.ObjectInfo, error)
	GetObjectLegalHold(ctx context.Context, bucket, key string, opts LockOptions) (domain.LegalHoldStatus, error)
	PutObjectLegalHold(ctx context.Context, bucket, key string, hold domain.LegalHoldStatus, opts LockOptions) (domain.ObjectInfo, error)

	// GetObjectAttributes describes a version without reading its content
	// (docs/02 S-OBJ-15): the object description and, for a multipart
	// object, a page of its parts with their sizes and checksums.
	GetObjectAttributes(ctx context.Context, bucket, key string, opts AttributesOptions) (ObjectAttributes, error)

	// ResealKeys re-seals the data keys of the SSE-S3 and SSE-KMS versions
	// of bucket under the current master key of the KMS (master key
	// rotation, docs/07 §7.2), version by version under its write lock:
	// the object keys and the data are unchanged, only the sealed data key
	// in the metadata is rewritten. Once every bucket is re-sealed, the
	// previous master keys can be removed (NEXO_KMS_SECRET_KEY_PREVIOUS).
	// The versions that cannot be re-sealed are counted, not fatal.
	ResealKeys(ctx context.Context, bucket string) (ResealResult, error)

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
	// Retention and LegalHold are the Object Lock settings of the new
	// version (x-amz-object-lock-*): the bucket must have object locking,
	// the retention must end in the future, and the request must carry a
	// Content-MD5 or an additional checksum (as S3 requires). Without a
	// Retention the bucket default retention applies.
	Retention domain.ObjectRetention
	LegalHold domain.LegalHoldStatus
	// SSE is the server-side encryption of the new version (zero: the
	// default encryption of the bucket, if any).
	SSE domain.SSERequest
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
	// SSECustomerKey is the SSE-C key of the request.
	SSECustomerKey domain.SSECustomerKey
	// ChecksumMode asks GetObjectInfo for the value of the additional
	// checksum (x-amz-checksum-mode: ENABLED): the checksum of an
	// encrypted version is sealed and its value is opened with the object
	// key, which costs a KMS call for SSE-S3 and SSE-KMS. Without it, the
	// checksum of such a version reports its algorithm only. GetObject
	// always reports it.
	ChecksumMode bool
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
	// IfModTime, when set, makes the delete conditional on the version it
	// targets still having this modification time: the version named by
	// VersionID, or without one the latest version (the one a delete
	// marker would cover, or an unversioned bucket would lose). The check
	// runs under the object write lock, with the commit times monotonic
	// per key: a version replaced or written meanwhile, or a missing one,
	// fails the delete with domain.ErrPreconditionFailed and nothing
	// changes. The lifecycle deletes this way the versions it evaluated,
	// so that a write acknowledged after the evaluation is never lost.
	IfModTime time.Time
	// IfMatch makes the delete conditional on the ETag of the version it
	// targets (the version named by VersionID, else the latest), "*"
	// matching any ETag (S3 conditional deletes): a missing version, a
	// delete marker or another ETag fails the delete with
	// domain.ErrPreconditionFailed and nothing changes. The check runs
	// under the object write lock.
	IfMatch string
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
	// ChecksumAlgorithm is the additional checksum of the copy
	// (x-amz-checksum-algorithm); empty keeps the algorithm of the source.
	// The checksum is computed over the copied content.
	ChecksumAlgorithm domain.ChecksumAlgorithm
	// Retention and LegalHold are the Object Lock settings of the copy,
	// as for PutObjectInput (the source's are never copied).
	Retention domain.ObjectRetention
	LegalHold domain.LegalHoldStatus
	// SrcSSECustomerKey is the SSE-C key of the source
	// (x-amz-copy-source-server-side-encryption-customer-*).
	SrcSSECustomerKey domain.SSECustomerKey
	// SSE is the encryption of the copy, as for PutObjectInput: the
	// encryption of the source is never copied, so that a copy re-keys an
	// object (SSE-C to SSE-S3, a new SSE-C key...).
	SSE domain.SSERequest
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
	// ChecksumType is how the checksum of the completed object is
	// computed: COMPOSITE (the default, every algorithm but CRC64NVME) or
	// FULL_OBJECT (the CRC algorithms, the default of CRC64NVME).
	ChecksumType domain.ChecksumType
	// Retention and LegalHold are the Object Lock settings of the
	// completed object, as for PutObjectInput.
	Retention domain.ObjectRetention
	LegalHold domain.LegalHoldStatus
	// SSE is the encryption of the completed object, as for
	// PutObjectInput: every part is encrypted with it, and the parts of an
	// SSE-C upload need its key (PutObjectPartInput.SSECustomerKey).
	SSE domain.SSERequest
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
	// SSECustomerKey is the SSE-C key of the upload.
	SSECustomerKey domain.SSECustomerKey
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
	// SrcSSECustomerKey is the SSE-C key of the source, SSECustomerKey
	// the one of the upload.
	SrcSSECustomerKey domain.SSECustomerKey
	SSECustomerKey    domain.SSECustomerKey
}

// CompleteMultipartInput are the parameters of CompleteMultipartUpload.
type CompleteMultipartInput struct {
	Bucket     string
	Key        string
	UploadID   string
	Parts      []domain.CompletePart
	Conditions domain.Conditions
	// Checksum, when set, is the checksum the client computed for the
	// object: verified against the one of the upload (BadDigest).
	Checksum domain.Checksum
	// ChecksumType, when set, must be the checksum type of the upload.
	ChecksumType domain.ChecksumType
	// ObjectSize, when set (x-amz-mp-object-size), is the size the client
	// expects for the object: the sum of the listed parts (plaintext)
	// must match it, or the completion fails with InvalidRequest before
	// anything is committed.
	ObjectSize *int64
	// SSECustomerKey is the SSE-C key of an SSE-C upload: the checksums of
	// its parts are sealed under it, so a completion that computes or
	// verifies a checksum needs it (as in S3); it is optional otherwise.
	SSECustomerKey domain.SSECustomerKey
}

// TagOptions tunes the tag operations.
type TagOptions struct {
	VersionID string
}

// LockOptions tunes the Object Lock operations.
type LockOptions struct {
	// VersionID selects the version ("" is the latest).
	VersionID string
	// BypassGovernance allows shortening or removing an active GOVERNANCE
	// retention (PutObjectRetention).
	BypassGovernance bool
}

// AttributesOptions tunes GetObjectAttributes.
type AttributesOptions struct {
	VersionID string
	// MaxParts limits the parts returned, as MaxKeys does (0 for the
	// default of 1000, domain.ListLimitNone for none).
	MaxParts int
	// PartNumberMarker lists the parts after this part number.
	PartNumberMarker int
	// SSECustomerKey is the key of an SSE-C version, which is described
	// to the holder of its key only (as HEAD).
	SSECustomerKey domain.SSECustomerKey
}

// ResealResult reports a ResealKeys run.
type ResealResult struct {
	// Versions counts the SSE-S3 and SSE-KMS versions met, Uploads the
	// pending multipart uploads of such objects (their description holds
	// the sealed key); Resealed counts the versions and uploads re-sealed
	// and Failed those that could not be (FirstError tells why).
	Versions   int
	Uploads    int
	Resealed   int
	Failed     int
	FirstError string
}

// ObjectAttributes is the result of GetObjectAttributes.
type ObjectAttributes struct {
	Info domain.ObjectInfo
	// PartsCount is the number of parts of a multipart object, 0 for an
	// object written in a single request.
	PartsCount int
	// Parts is the page of parts selected by AttributesOptions.
	Parts                []domain.ObjectPart
	IsTruncated          bool
	NextPartNumberMarker int
}

// HealthOptions tunes Health.
type HealthOptions struct {
	// Maintenance checks whether one more node (all its drives) can go
	// down without losing the write quorum of any set.
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
