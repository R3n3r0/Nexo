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
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Deps are the dependencies of the bucket service.
type Deps struct {
	Objects object.Service
	Store   MetadataStore
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Metadata is the persisted configuration of a bucket
// (.nexo.sys/buckets/<bucket>/metadata.json).
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

// MetadataStore persists the bucket metadata documents. A bucket without a
// document yields the zero Metadata (unversioned, no configuration).
type MetadataStore interface {
	object.BucketConfigSource
	Load(ctx context.Context, bucket string) (*Metadata, error)
	Save(ctx context.Context, bucket string, m *Metadata) error
	Delete(ctx context.Context, bucket string) error
}

// Service manages buckets and their metadata.
type Service interface {
	// Create, Get, List and Delete delegate to object.Service and keep the
	// bucket metadata store in sync.
	Create(ctx context.Context, in CreateInput) (*domain.Bucket, error)
	Get(ctx context.Context, name string) (*domain.BucketInfo, error)
	List(ctx context.Context, in ListInput) ([]domain.BucketInfo, error)
	Delete(ctx context.Context, name string, opts DeleteOptions) error

	GetVersioning(ctx context.Context, name string) (domain.Versioning, error)
	SetVersioning(ctx context.Context, name string, v domain.Versioning) error

	// GetPolicy returns the bucket policy document as raw JSON, or
	// domain.ErrPolicyNotFound when none is set.
	GetPolicy(ctx context.Context, name string) (json.RawMessage, error)
	SetPolicy(ctx context.Context, name string, policy json.RawMessage) error
	DeletePolicy(ctx context.Context, name string) error

	// Lifecycle, CORS and Notification configurations are stored as their
	// canonical XML documents; parsing and evaluation live in the ilm and
	// event packages. A missing configuration returns domain.ErrNotFound-like
	// sentinels: ErrPolicyNotFound is reused for "no configuration".
	GetLifecycle(ctx context.Context, name string) ([]byte, error)
	SetLifecycle(ctx context.Context, name string, xmlDoc []byte) error
	DeleteLifecycle(ctx context.Context, name string) error

	GetObjectLock(ctx context.Context, name string) (ObjectLockConfig, error)
	SetObjectLock(ctx context.Context, name string, cfg ObjectLockConfig) error

	GetEncryption(ctx context.Context, name string) (EncryptionConfig, error)
	SetEncryption(ctx context.Context, name string, cfg EncryptionConfig) error
	DeleteEncryption(ctx context.Context, name string) error

	GetQuota(ctx context.Context, name string) (Quota, error)
	SetQuota(ctx context.Context, name string, q Quota) error

	GetTags(ctx context.Context, name string) (map[string]string, error)
	SetTags(ctx context.Context, name string, tags map[string]string) error
	DeleteTags(ctx context.Context, name string) error

	GetCORS(ctx context.Context, name string) ([]byte, error)
	SetCORS(ctx context.Context, name string, xmlDoc []byte) error
	DeleteCORS(ctx context.Context, name string) error

	GetNotification(ctx context.Context, name string) ([]byte, error)
	SetNotification(ctx context.Context, name string, xmlDoc []byte) error
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
	// WithUsage includes object counts and sizes (slower).
	WithUsage bool
}

// DeleteOptions tunes Delete.
type DeleteOptions struct {
	// Force deletes the bucket even when it is not empty.
	Force bool
}

// RetentionMode is the object lock retention mode.
type RetentionMode string

// Retention modes.
const (
	RetentionGovernance RetentionMode = "GOVERNANCE"
	RetentionCompliance RetentionMode = "COMPLIANCE"
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
