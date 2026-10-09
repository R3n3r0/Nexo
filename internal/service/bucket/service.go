// Package bucket manages bucket metadata: versioning, policy, lifecycle,
// object lock, encryption, quota, tags, CORS and notification configuration.
// Bucket creation, deletion and listing delegate to the object layer; the
// metadata documents are persisted by a MetadataStore, which also serves the
// object layer as its BucketConfigSource.
package bucket

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Deps are the dependencies of the bucket service.
type Deps struct {
	Objects object.Service
	Store   MetadataStore
	// Policy parses and validates bucket policies and decides whether they
	// are public.
	Policy policy.Service
	// Locks serializes the read-modify-write cycles of a bucket's
	// configuration (default: a local locker; pass the deployment's
	// locker so that every node serializes on the same locks).
	Locks lock.Locker
	// KMS validates the default encryption of the buckets (SetEncryption):
	// SSE-S3 and SSE-KMS need a configured KMS, and SSE-KMS an existing
	// key. Nil refuses every default encryption (no KMS).
	KMS    kms.Service
	Clock  clock.Clock
	Logger *slog.Logger
}

// Metadata is the persisted configuration of a bucket
// (.nexo.sys/buckets/<bucket>/.metadata.json). Policy is the raw JSON
// document; Lifecycle, CORS and Notification are the raw XML (or JSON)
// documents, parsed and evaluated by their own packages.
type Metadata struct {
	Name         string            `json:"name"`
	Created      time.Time         `json:"created"`
	Region       string            `json:"region,omitempty"`
	Versioning   domain.Versioning `json:"versioning"`
	ObjectLock   ObjectLockConfig  `json:"objectLock"`
	Policy       json.RawMessage   `json:"policy,omitempty"`
	Lifecycle    []byte            `json:"lifecycle,omitempty"`
	Encryption   EncryptionConfig  `json:"encryption"`
	Quota        Quota             `json:"quota"`
	Tags         map[string]string `json:"tags,omitempty"`
	CORS         []byte            `json:"cors,omitempty"`
	Notification []byte            `json:"notification,omitempty"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

// MetadataStore persists the bucket metadata documents behind an
// in-memory cache. A bucket without a document yields the zero Metadata
// (unversioned, no configuration).
type MetadataStore interface {
	object.BucketConfigSource
	// Load returns a copy of the bucket metadata, from the cache when
	// present.
	Load(ctx context.Context, bucket string) (*Metadata, error)
	// Save writes the document atomically (on a majority of the drives)
	// and refreshes the cache.
	Save(ctx context.Context, bucket string, m *Metadata) error
	// Delete removes the document and its cache entry.
	Delete(ctx context.Context, bucket string) error
	// Invalidate drops the cache entry of bucket (every bucket when
	// empty) so that the next Load reads the drives: the hook of the peer
	// notifications sent when another node changes a bucket.
	Invalidate(ctx context.Context, bucket string)
}

// Service manages buckets and their metadata.
type Service interface {
	// Create, Get, List and Delete delegate to object.Service and keep the
	// bucket metadata store in sync.
	Create(ctx context.Context, in CreateInput) (*domain.Bucket, error)
	Get(ctx context.Context, name string) (*domain.BucketInfo, error)
	List(ctx context.Context, in ListInput) ([]domain.BucketInfo, error)
	// ListPage returns a page of the buckets in name order (ListBuckets
	// with max-buckets and continuation-token); an invalid token or page
	// size fails with domain.ErrInvalidArgument.
	ListPage(ctx context.Context, in ListPageInput) (ListPageResult, error)
	Delete(ctx context.Context, name string, opts DeleteOptions) error

	GetVersioning(ctx context.Context, name string) (domain.Versioning, error)
	SetVersioning(ctx context.Context, name string, v domain.Versioning) error

	// GetPolicy returns the bucket policy document as raw JSON, or
	// domain.ErrPolicyNotFound when none is set.
	GetPolicy(ctx context.Context, name string) (json.RawMessage, error)
	// SetPolicy validates the document with the policy engine's bucket
	// policy rules (policy.Service.ParseBucketPolicy: S3 MalformedPolicy
	// errors wrapping domain.ErrInvalidArgument) and stores it as given.
	SetPolicy(ctx context.Context, name string, policy json.RawMessage) error
	DeletePolicy(ctx context.Context, name string) error
	// IsPublic reports whether the bucket policy grants access to everyone
	// (S3 GetBucketPolicyStatus). A bucket without a policy returns
	// domain.ErrPolicyNotFound (S3 NoSuchBucketPolicy).
	IsPublic(ctx context.Context, name string) (bool, error)

	// Lifecycle, CORS and Notification configurations are stored as their
	// canonical XML documents; parsing and evaluation live in the ilm and
	// event packages. A missing configuration returns domain.ErrNotFound-like
	// sentinels: ErrPolicyNotFound is reused for "no configuration".
	GetLifecycle(ctx context.Context, name string) ([]byte, error)
	SetLifecycle(ctx context.Context, name string, xmlDoc []byte) error
	DeleteLifecycle(ctx context.Context, name string) error

	// GetObjectLock returns the object lock configuration of a bucket
	// created with object locking (S3 ObjectLockConfigurationNotFoundError
	// otherwise).
	GetObjectLock(ctx context.Context, name string) (ObjectLockConfig, error)
	// SetObjectLock sets the object lock configuration and default
	// retention of a bucket. Object locking is enabled at the creation of
	// a bucket (CreateInput.ObjectLock) or later on a bucket whose
	// versioning is Enabled (S3 InvalidBucketState otherwise), and stays
	// enabled.
	SetObjectLock(ctx context.Context, name string, cfg ObjectLockConfig) error

	// GetEncryption returns the default encryption of a bucket (S3
	// ServerSideEncryptionConfigurationNotFoundError when none).
	GetEncryption(ctx context.Context, name string) (EncryptionConfig, error)
	// SetEncryption sets the default encryption of a bucket (S-BKT-11),
	// applied by the object layer to the writes that ask for none. It
	// needs a configured KMS (domain.ErrKMSNotConfigured, S3
	// NotImplemented) and, for aws:kms with a key id, an existing key
	// (domain.ErrKMSKeyNotFound, S3 KMS.NotFoundException).
	SetEncryption(ctx context.Context, name string, cfg EncryptionConfig) error
	DeleteEncryption(ctx context.Context, name string) error

	GetQuota(ctx context.Context, name string) (Quota, error)
	// SetQuota stores the quota and returns it normalized: a positive size
	// without a type is a hard quota, a zero size clears the quota.
	SetQuota(ctx context.Context, name string, q Quota) (Quota, error)

	// GetTags returns the bucket tags (S3 NoSuchTagSet when none).
	GetTags(ctx context.Context, name string) (map[string]string, error)
	// SetTags replaces the bucket tags, validated with the S3 rules: at
	// most 50 tags, keys of 1-128 and values of 0-256 Unicode characters,
	// no key in the reserved "aws:" namespace (S3 InvalidTag, BadRequest).
	SetTags(ctx context.Context, name string, tags map[string]string) error
	DeleteTags(ctx context.Context, name string) error

	// GetCORS returns the CORS configuration as stored (S3
	// NoSuchCORSConfiguration when none).
	GetCORS(ctx context.Context, name string) ([]byte, error)
	// SetCORS validates a CORSConfiguration document (domain
	// CORSConfiguration.Validate: 1-100 rules, ...) and stores it.
	SetCORS(ctx context.Context, name string, xmlDoc []byte) error
	DeleteCORS(ctx context.Context, name string) error
	// GetCORSConfig returns the parsed CORS configuration of a bucket,
	// evaluated by the S3 API for the cross-origin requests (S3
	// NoSuchCORSConfiguration, wrapping domain.ErrPolicyNotFound, when
	// none).
	GetCORSConfig(ctx context.Context, name string) (domain.CORSConfiguration, error)

	GetNotification(ctx context.Context, name string) ([]byte, error)
	SetNotification(ctx context.Context, name string, xmlDoc []byte) error

	// Invalidate drops the cached metadata of a bucket (every bucket when
	// name is empty); see MetadataStore.Invalidate.
	Invalidate(ctx context.Context, name string)
}

// CreateInput are the parameters of Create.
type CreateInput struct {
	Name   string
	Region string
	// ObjectLock enables object locking (and versioning) at creation.
	ObjectLock bool
}

// ListInput are the parameters of List.
type ListInput struct {
	// Prefix filters bucket names.
	Prefix string
	// WithUsage fills in the usage counters (objects, versions, delete
	// markers, size) with a walk of every bucket: slower.
	WithUsage bool
}

// MaxListBuckets is the largest page of ListPage (max-buckets of S3).
const MaxListBuckets = 10000

// ListPageInput are the parameters of ListPage.
type ListPageInput struct {
	// Prefix filters bucket names.
	Prefix string
	// MaxBuckets bounds the page, 1..MaxListBuckets; 0 returns every
	// bucket after the token.
	MaxBuckets int
	// ContinuationToken resumes the listing after the page that returned
	// it (opaque).
	ContinuationToken string
}

// ListPageResult is a page of ListPage.
type ListPageResult struct {
	Buckets []domain.BucketInfo
	// ContinuationToken resumes the listing; empty on the last page.
	ContinuationToken string
}

// DeleteOptions tunes Delete.
type DeleteOptions struct {
	// Force deletes the bucket even when it is not empty.
	Force bool
}

// RetentionMode is the object lock retention mode.
type RetentionMode = domain.RetentionMode

// Retention modes.
const (
	RetentionGovernance = domain.RetentionGovernance
	RetentionCompliance = domain.RetentionCompliance
)

// ObjectLockConfig is the bucket object lock configuration.
type ObjectLockConfig struct {
	Enabled bool
	// Mode, Days and Years define the default retention; zero means none.
	Mode  RetentionMode
	Days  int
	Years int
}

// EncryptionConfig is the default bucket encryption.
type EncryptionConfig struct {
	// Algorithm is "AES256" (SSE-S3) or "aws:kms" (SSE-KMS).
	Algorithm string
	// KMSKeyID is the key for SSE-KMS.
	KMSKeyID string
}

// QuotaType is the quota enforcement type.
type QuotaType string

// Quota types.
const (
	// QuotaHard rejects writes beyond the limit.
	QuotaHard QuotaType = "hard"
)

// Quota is the bucket storage quota; Size 0 disables it.
type Quota struct {
	Type QuotaType
	// Size is the limit in bytes.
	Size uint64
}
