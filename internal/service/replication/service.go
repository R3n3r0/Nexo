// Package replication replicates the versions of a bucket to buckets of
// other deployments (S-BKT-12, docs/03 §3.10): asynchronously, by the
// rules of the ReplicationConfiguration of the bucket, to replication
// targets (a remote S3 endpoint, its credentials and its bucket, sealed in
// the system store and named by an ARN the rules point to).
//
// A version a rule selects is written PENDING for each destination
// (object.BucketConfig.Replication). Its creation, a delete marker, a
// permanent deletion of a version or a change of its tags becomes a task
// of a persistent queue of the node that served the write; workers drain
// it in order per key, send the version with the id, the time and the
// ETag it has here (the destination stores it as a REPLICA, which it never
// replicates back: two buckets replicating to each other are active-active
// with the last writer winning, the versions ordered by their time), and
// record the outcome on the version (COMPLETED, or FAILED and retried with
// a backoff). A resync replicates again, from the leader, every version a
// destination does not have (existing objects, a target replaced, a queue
// lost with its drive).
package replication

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// Service manages the replication targets and runs the replication.
type Service interface {
	// Runner starts the workers of the queue of this node and, on the
	// leader, the resyncs.
	lifecycle.Runner

	// Notify turns an event of the object layer into a replication task
	// when the configuration of its bucket asks for it. It never fails
	// the operation that published the event: a task that cannot be
	// queued leaves the version PENDING, for a resync.
	Notify(ctx context.Context, e event.Event)

	// SetTarget creates a replication target of bucket (in.ARN empty) or
	// replaces one (its credentials may be left empty to keep them). The
	// remote bucket is checked first: reachable with the credentials and
	// with versioning enabled (domain.ErrInvalidArgument otherwise).
	SetTarget(ctx context.Context, bucket string, in TargetInput) (Target, error)
	// ListTargets returns the targets of bucket, sorted by ARN, without
	// their secret.
	ListTargets(ctx context.Context, bucket string) ([]Target, error)
	// RemoveTarget removes a target; one the replication configuration
	// still points to cannot be removed (domain.ErrInvalidArgument). A
	// missing target is domain.ErrNotFound.
	RemoveTarget(ctx context.Context, bucket, arn string) error
	// RemoveBucket forgets the targets and the resync of a deleted bucket.
	RemoveBucket(ctx context.Context, bucket string) error

	// ValidateConfig parses a ReplicationConfiguration document for
	// bucket, every destination being one of its targets, and returns the
	// canonical document to store.
	ValidateConfig(ctx context.Context, bucket string, doc []byte) ([]byte, error)

	// Status reports the replication of bucket as this node sees it: per
	// target, the tasks queued here, the counters since the start of the
	// process and the last error; and the last resync.
	Status(ctx context.Context, bucket string) (Status, error)
	// StartResync starts replicating again to target every version of
	// bucket its rules select and the destination does not have (the
	// versions recorded COMPLETED there are checked too). A resync of the
	// bucket in progress is replaced.
	StartResync(ctx context.Context, bucket, target string) (Resync, error)
}

// TargetInput are the settings of a replication target.
type TargetInput struct {
	// ARN names the target to replace; empty creates a new one.
	ARN string
	// Endpoint is the URL of the S3 API of the remote deployment
	// (http(s)://host[:port]).
	Endpoint string
	// Bucket is the remote bucket, Region its region (default
	// us-east-1).
	Bucket string
	Region string
	// AccessKey and SecretKey are the credentials of the replication at
	// the remote: s3:ReplicateObject, s3:ReplicateDelete and the object
	// actions on the remote bucket. An empty SecretKey on a replacement
	// keeps the stored one.
	AccessKey string
	SecretKey string
	// Label is a free description.
	Label string
}

// Target is a replication target, without its secret.
type Target struct {
	// ARN identifies the target in the rules:
	// arn:nexo:replication::<id>:<remote bucket>.
	ARN       string    `json:"arn"`
	Endpoint  string    `json:"endpoint"`
	Bucket    string    `json:"bucket"`
	Region    string    `json:"region,omitempty"`
	AccessKey string    `json:"accessKey"`
	Label     string    `json:"label,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Status is the replication of a bucket.
type Status struct {
	Targets []TargetStatus `json:"targets"`
	// Queued counts the tasks of the bucket in the queue of this node.
	Queued int `json:"queued"`
	// Resync is the last resync of the bucket (nil when none).
	Resync *Resync `json:"resync,omitempty"`
}

// TargetStatus is the replication to one target, since the start of the
// process of this node.
type TargetStatus struct {
	ARN string `json:"arn"`
	// Online reports that the last attempt succeeded (true before any).
	Online bool `json:"online"`
	// Queued counts the tasks for the target queued on this node.
	Queued int `json:"queued"`
	// Replicated and Bytes count the versions sent, Deletes the delete
	// markers and deletions, Failed the failed attempts.
	Replicated int64     `json:"replicated"`
	Bytes      int64     `json:"bytes"`
	Deletes    int64     `json:"deletes"`
	Failed     int64     `json:"failed"`
	LastError  string    `json:"lastError,omitempty"`
	LastFailed time.Time `json:"lastFailed,omitzero"`
	LastOK     time.Time `json:"lastOk,omitzero"`
}

// ResyncState is the state of a resync.
type ResyncState string

// Resync states.
const (
	ResyncRunning  ResyncState = "running"
	ResyncComplete ResyncState = "complete"
	ResyncFailed   ResyncState = "failed"
)

// Resync is the progress of a resync of a bucket to one target.
type Resync struct {
	ID         string      `json:"id"`
	Target     string      `json:"target"`
	State      ResyncState `json:"state"`
	StartedAt  time.Time   `json:"startedAt"`
	UpdatedAt  time.Time   `json:"updatedAt,omitzero"`
	FinishedAt time.Time   `json:"finishedAt,omitzero"`
	// Key and VersionID are where the walk stands.
	Key       string `json:"key,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	// Scanned counts the versions walked, Queued those queued for the
	// target.
	Scanned int64  `json:"scanned"`
	Queued  int64  `json:"queued"`
	Error   string `json:"error,omitempty"`
}

// Sealer seals the secrets of the targets in the system store
// (certs.Sealer).
type Sealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	Open(ctx context.Context, sealed, aad []byte) ([]byte, error)
}

// Leader reports whether this node runs the resyncs (cluster.Leader).
type Leader interface {
	IsLeader() bool
}

// RemoteConfig is what a Remote connects with.
type RemoteConfig struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// Dialer opens the clients of the targets.
type Dialer interface {
	Dial(ctx context.Context, cfg RemoteConfig) (Remote, error)
}

// Remote writes to the bucket of a target. Every write carries the
// replica headers (domain.HeaderReplica...): the version keeps the id and
// the time it has at the source.
type Remote interface {
	// CheckBucket fails unless the bucket exists, is reachable with the
	// credentials and has versioning enabled.
	CheckBucket(ctx context.Context) error
	// Head describes a version of the remote bucket (Exists false when it
	// does not have it).
	Head(ctx context.Context, key, versionID string) (RemoteVersion, error)
	// Put writes a version in one request.
	Put(ctx context.Context, v RemotePut, data []byte) error
	// CreateUpload, UploadPart, CompleteUpload and AbortUpload write a
	// version in parts.
	CreateUpload(ctx context.Context, v RemotePut) (string, error)
	UploadPart(ctx context.Context, key, uploadID string, number int, data []byte) (string, error)
	CompleteUpload(ctx context.Context, key, uploadID string, parts []RemotePart) error
	AbortUpload(ctx context.Context, key, uploadID string) error
	// DeleteMarker writes a delete marker with the id and the time it has
	// at the source.
	DeleteMarker(ctx context.Context, key, versionID string, modTime time.Time) error
	// DeleteVersion removes a version permanently.
	DeleteVersion(ctx context.Context, key, versionID string) error
	// PutTags replaces the tags of a version.
	PutTags(ctx context.Context, key, versionID string, tags map[string]string) error
}

// RemoteVersion describes a version of a remote bucket.
type RemoteVersion struct {
	Exists bool
	ETag   string
	Size   int64
}

// RemotePut is a version to write: its identity and its metadata.
type RemotePut struct {
	Key                string
	VersionID          string
	ModTime            time.Time
	ETag               string
	Size               int64
	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	Expires            time.Time
	UserMetadata       map[string]string
	Tags               map[string]string
	Retention          domain.ObjectRetention
	LegalHold          domain.LegalHoldStatus
	// Encryption asks the remote to encrypt the version like here (SSE-S3,
	// or SSE-KMS with the same key id).
	Encryption domain.EncryptionInfo
}

// RemotePart is a part of a completed remote upload.
type RemotePart struct {
	Number int
	ETag   string
}
