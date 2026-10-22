// Package ilm implements the bucket lifecycle (S-BKT-07, docs/05 T-7.3):
// the LifecycleConfiguration documents of S3 (parsing and validation),
// their evaluation against the versions of a key and the in-progress
// multipart uploads with an injected clock (Expiration, expired object
// delete markers, NoncurrentVersionExpiration, AbortIncompleteMultipartUpload,
// Transition and NoncurrentVersionTransition to the remote tiers of
// S-STO-12), and their application: by the background scanner, which hands
// every version to the service (scanner.Lifecycle), or on demand
// (ApplyNow).
//
// An expiration due wins over a transition. A transition moves the data of
// a version to the tier its storage class names (object.TransitionObject);
// the versions stored inline, empty or already in a tier stay where they
// are.
//
// Versions protected by Object Lock (retention or legal hold) are never
// deleted: an expiration only puts a delete marker over them. The
// removals go through the object layer with a lifecycle request in the
// context, so that their notifications are s3:LifecycleExpiration events.
package ilm

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

// Service evaluates and applies the lifecycle configurations.
type Service interface {
	// Lifecycle is the hook of the scanner: Apply evaluates a version
	// (the first of a key evaluates every version of the key) and applies
	// the actions due; AbortUploads applies the rules to the multipart
	// uploads of a bucket.
	scanner.Lifecycle

	// Parse decodes and validates a LifecycleConfiguration document: S3
	// MalformedXML, InvalidArgument or InvalidRequest errors (wrapping
	// domain.ErrMalformedXML / ErrInvalidArgument), InvalidStorageClass
	// for a transition to a storage class without tier.
	Parse(ctx context.Context, doc []byte) (*Configuration, error)
	// Validate parses doc and returns its canonical form, the document to
	// store (rules without ID get one).
	Validate(ctx context.Context, doc []byte) ([]byte, error)
	// Evaluate returns the actions due now on the versions of one key and
	// on the multipart uploads of in. Versions locked by Object Lock are
	// never deleted.
	Evaluate(ctx context.Context, in EvaluateInput) ([]Action, error)
	// Expiration returns when the lifecycle of bucket expires a version
	// of key ("" is the latest), for the x-amz-expiration header of
	// PUT, GET and HEAD: the zero Expiry when no rule expires it (no
	// configuration, a noncurrent version, a delete marker).
	Expiration(ctx context.Context, bucket, key, versionID string) (Expiry, error)
	// ApplyNow applies the lifecycle of bucket (every bucket with a
	// configuration when empty) at once: every key and every multipart
	// upload. It is what the scanner does over its cycle, for the tests
	// and the Admin API.
	ApplyNow(ctx context.Context, bucket string) (ApplyResult, error)
}

// BucketSettings provides the lifecycle document and the versioning of a
// bucket (bucket.MetadataStore implements it).
type BucketSettings interface {
	Load(ctx context.Context, bucket string) (*bucket.Metadata, error)
}

// Tiers lists the remote tiers the transitions may name (tier.Service,
// through an adapter).
type Tiers interface {
	TierNames(ctx context.Context) ([]string, error)
}

// Deps are the dependencies of the lifecycle service.
type Deps struct {
	// Objects lists, reads, deletes and transitions the versions and the
	// uploads (required).
	Objects object.Service
	// Buckets provides the configurations (required).
	Buckets BucketSettings
	// Tiers provides the tiers of the transitions (nil: only those of
	// WithStorageClasses).
	Tiers  Tiers
	Clock  clock.Clock
	Logger *slog.Logger
}

// EvaluateInput are the parameters of Evaluate.
type EvaluateInput struct {
	Bucket     string
	Config     *Configuration
	Versioning domain.Versioning
	// Versions are the versions of one key, newest first (the current
	// version, or delete marker, first). Their Retention and LegalHold
	// tell the versions protected by Object Lock.
	Versions []domain.ObjectInfo
	// Uploads are the in-progress multipart uploads to evaluate.
	Uploads []domain.MultipartUpload
}

// ActionType is what an action does.
type ActionType string

// Action types.
const (
	// ActionDeleteVersion permanently removes a version or a delete
	// marker (the object itself in an unversioned bucket).
	ActionDeleteVersion ActionType = "DeleteVersion"
	// ActionDeleteMarker puts a delete marker over the current version
	// of a versioned bucket.
	ActionDeleteMarker ActionType = "DeleteMarker"
	// ActionAbortUpload aborts an incomplete multipart upload.
	ActionAbortUpload ActionType = "AbortUpload"
	// ActionTransition moves the data of a version to the tier
	// Action.StorageClass names.
	ActionTransition ActionType = "Transition"
)

// Action is an action due on a version or an upload.
type Action struct {
	Type ActionType
	// RuleID is the rule that makes the action due (the earliest due).
	RuleID string
	Bucket string
	Key    string
	// VersionID and ModTime identify the version ("" in an unversioned
	// bucket; "" and "null" both designate the null version), UploadID
	// the upload. The action applies only while the version still has
	// ModTime: a version written after the evaluation is never removed.
	VersionID string
	ModTime   time.Time
	UploadID  string
	// Due is when the action became due.
	Due time.Time
	// StorageClass is the tier of a transition.
	StorageClass string
}

// Expiry is the expiration of a version: the zero value when none.
type Expiry struct {
	Date   time.Time
	RuleID string
}

// ApplyResult reports an ApplyNow.
type ApplyResult struct {
	// Buckets counts the buckets with a lifecycle configuration, Versions
	// the versions evaluated.
	Buckets  int `json:"buckets"`
	Versions int `json:"versions"`
	// Deleted counts the versions and delete markers removed,
	// DeleteMarkers the delete markers created, UploadsAborted the
	// uploads aborted.
	Deleted        int `json:"deleted"`
	DeleteMarkers  int `json:"deleteMarkers"`
	UploadsAborted int `json:"uploadsAborted"`
	// Locked counts the expirations skipped because Object Lock protects
	// the version.
	Locked int `json:"locked"`
	// Transitioned counts the versions moved to a remote tier.
	Transitioned int `json:"transitioned"`
}
