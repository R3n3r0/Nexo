// Package tier manages the remote tiers of the lifecycle (S-STO-12,
// docs/03 §3.12): S3 buckets of other deployments or of a cloud provider
// (an endpoint, a bucket, a prefix and credentials sealed in the system
// store) that the Transition actions of the lifecycle move the data of
// versions to. It implements object.TierStore: the object layer writes the
// stored bytes of a version there as one object, named
// <prefix><deployment id>/<id>, and reads ranges of it back.
//
// The objects no version refers to any more (the version was deleted or
// replaced, a transition failed after the write) are removed by the sweep
// the leader runs periodically: an object of the tier older than the grace
// period whose version, looked up under its lock, does not name it. The
// objects of other deployments in the same bucket are never touched. A
// tier holding objects of this deployment cannot be removed.
package tier

import (
	"context"
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Service manages the tiers and keeps the data of the transitioned
// versions.
type Service interface {
	// Runner runs the sweep of the orphaned objects on the leader.
	lifecycle.Runner
	// TierStore is what the object layer transitions to and reads from.
	object.TierStore

	// Add creates a tier. The remote bucket must be reachable with the
	// credentials (domain.ErrInvalidArgument otherwise); a tier of the
	// same name is domain.ErrConflict.
	Add(ctx context.Context, in Input) (Tier, error)
	// Edit replaces the credentials (and the region and the remote storage
	// class, when set) of a tier, checked first; its endpoint, bucket and
	// prefix never change. A missing tier is domain.ErrNotFound.
	Edit(ctx context.Context, name string, in Input) (Tier, error)
	// List returns the tiers, sorted by name, without their secret.
	List(ctx context.Context) ([]Tier, error)
	// Remove removes a tier that holds no object of this deployment
	// (domain.ErrConflict otherwise, or when the tier cannot be checked).
	Remove(ctx context.Context, name string) error
	// Exists reports whether a tier exists: the transitions of the
	// lifecycle may name it.
	Exists(ctx context.Context, name string) (bool, error)
	// Sweep removes now the orphaned objects of every tier (see the
	// package documentation).
	Sweep(ctx context.Context) (SweepResult, error)
}

// Input are the settings of a tier.
type Input struct {
	// Name is the storage class the lifecycle transitions name: upper
	// case letters, digits, '-' and '_' (STANDARD and REDUCED_REDUNDANCY
	// are reserved).
	Name string
	// Endpoint is the URL of the S3 API (http(s)://host[:port]).
	Endpoint string
	// Bucket is the remote bucket, Prefix the prefix of the objects in it
	// (empty, or ending with '/'), Region its region (default us-east-1).
	Bucket string
	Prefix string
	Region string
	// StorageClass is the storage class of the objects written in the
	// remote bucket (empty: its default). It must be readable at once
	// (not an archive class that needs a restore).
	StorageClass string
	// AccessKey and SecretKey are the credentials at the remote: read,
	// write, list and delete in the bucket.
	AccessKey string
	SecretKey string
}

// Tier is a tier, without its secret.
type Tier struct {
	Name         string    `json:"name"`
	Endpoint     string    `json:"endpoint"`
	Bucket       string    `json:"bucket"`
	Prefix       string    `json:"prefix,omitempty"`
	Region       string    `json:"region,omitempty"`
	StorageClass string    `json:"storageClass,omitempty"`
	AccessKey    string    `json:"accessKey"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

// SweepResult reports a sweep.
type SweepResult struct {
	// Scanned counts the objects of the tiers examined, Removed the
	// orphaned ones removed, Failed the objects or tiers that could not
	// be checked or removed.
	Scanned int `json:"scanned"`
	Removed int `json:"removed"`
	Failed  int `json:"failed"`
}

// Objects tells which tier object a version refers to (object.Service).
type Objects interface {
	TierLocation(ctx context.Context, bucket, key, versionID string) (object.TierLocation, error)
}

// Sealer seals the secrets of the tiers in the system store
// (certs.Sealer).
type Sealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	Open(ctx context.Context, sealed, aad []byte) ([]byte, error)
}

// Leader reports whether this node runs the sweep (cluster.Leader).
type Leader interface {
	IsLeader() bool
}

// RemoteConfig is what a Remote connects with.
type RemoteConfig struct {
	Endpoint     string
	Bucket       string
	Region       string
	StorageClass string
	AccessKey    string
	SecretKey    string
}

// Dialer opens the clients of the tiers.
type Dialer interface {
	Dial(ctx context.Context, cfg RemoteConfig) (Remote, error)
}

// Remote is the bucket of a tier.
type Remote interface {
	// CheckBucket fails unless the bucket exists and is reachable with
	// the credentials.
	CheckBucket(ctx context.Context) error
	// Put writes the size bytes of r as the object name, with metadata
	// (x-amz-meta-*), and checks its size there.
	Put(ctx context.Context, name string, r io.Reader, size int64, meta map[string]string) error
	// Head describes an object (domain.ErrObjectNotFound when missing).
	Head(ctx context.Context, name string) (RemoteObject, error)
	// Open streams [offset, offset+length) of an object; a shorter stream
	// fails with io.ErrUnexpectedEOF.
	Open(ctx context.Context, name string, offset, length int64) (io.ReadCloser, error)
	// Delete removes an object (a missing one is not an error).
	Delete(ctx context.Context, name string) error
	// List returns a page of the objects under prefix after the
	// continuation token ("" first) and the token of the next page (""
	// at the end). Metadata is not filled.
	List(ctx context.Context, prefix, token string) ([]RemoteObject, string, error)
}

// RemoteObject is an object of a tier.
type RemoteObject struct {
	Name     string
	Size     int64
	ModTime  time.Time
	Metadata map[string]string
}
