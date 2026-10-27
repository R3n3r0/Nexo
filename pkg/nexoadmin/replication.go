package nexoadmin

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// ReplicationRule is one rule of the replication of a bucket.
type ReplicationRule struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
	// Prefix and Tags select the keys (none: every key).
	Prefix string            `json:"prefix,omitempty"`
	Tags   map[string]string `json:"tags,omitempty"`
	// Target is the ARN of a replication target of the bucket.
	Target string `json:"target"`
	// DeleteMarkers, Deletes and Existing replicate the delete markers,
	// the permanent deletions of versions and, on a resync, the versions
	// written before the rule.
	DeleteMarkers bool `json:"deleteMarkers"`
	Deletes       bool `json:"deletes"`
	Existing      bool `json:"existing"`
}

// ReplicationConfiguration is the replication of a bucket (no rule: none).
type ReplicationConfiguration struct {
	Rules []ReplicationRule `json:"rules"`
}

// ReplicationTarget is a remote bucket a bucket replicates to. SecretKey
// is sent, never returned.
type ReplicationTarget struct {
	// ARN names the target in the rules; empty creates a target.
	ARN       string    `json:"arn,omitempty"`
	Endpoint  string    `json:"endpoint"`
	Bucket    string    `json:"bucket"`
	Region    string    `json:"region,omitempty"`
	AccessKey string    `json:"accessKey"`
	SecretKey string    `json:"secretKey,omitempty"`
	Label     string    `json:"label,omitempty"`
	Created   time.Time `json:"created,omitzero"`
	Updated   time.Time `json:"updated,omitzero"`
}

// ReplicationResync is the progress of a resync of a bucket to a target.
type ReplicationResync struct {
	ID     string `json:"id,omitempty"`
	Target string `json:"target"`
	// State is running, complete or failed.
	State      string    `json:"state,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	UpdatedAt  time.Time `json:"updatedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Scanned    int64     `json:"scanned"`
	Queued     int64     `json:"queued"`
	Error      string    `json:"error,omitempty"`
}

// ReplicationTargetStatus is the replication to one target as the node
// that answered sees it, since its start.
type ReplicationTargetStatus struct {
	ARN        string    `json:"arn"`
	Online     bool      `json:"online"`
	Queued     int       `json:"queued"`
	Replicated int64     `json:"replicated"`
	Bytes      int64     `json:"bytes"`
	Deletes    int64     `json:"deletes"`
	Failed     int64     `json:"failed"`
	LastError  string    `json:"lastError,omitempty"`
	LastFailed time.Time `json:"lastFailed,omitzero"`
	LastOK     time.Time `json:"lastOk,omitzero"`
}

// ReplicationStatus is the replication of a bucket.
type ReplicationStatus struct {
	Targets []ReplicationTargetStatus `json:"targets"`
	Queued  int                       `json:"queued"`
	Resync  *ReplicationResync        `json:"resync,omitempty"`
}

type replicationTargets struct {
	Targets []ReplicationTarget `json:"targets"`
}

// GetBucketReplication implements AdminClient.
func (c *client) GetBucketReplication(ctx context.Context, bucket string) (*ReplicationConfiguration, error) {
	var out ReplicationConfiguration
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "replication")}, &out)
}

// SetBucketReplication implements AdminClient.
func (c *client) SetBucketReplication(ctx context.Context, bucket string, cfg ReplicationConfiguration) (*ReplicationConfiguration, error) {
	if cfg.Rules == nil {
		cfg.Rules = []ReplicationRule{}
	}
	var out ReplicationConfiguration
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", bucket, "replication"), body: cfg}, &out)
}

// DeleteBucketReplication implements AdminClient.
func (c *client) DeleteBucketReplication(ctx context.Context, bucket string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("buckets", bucket, "replication")}, nil)
}

// ListReplicationTargets implements AdminClient.
func (c *client) ListReplicationTargets(ctx context.Context, bucket string) ([]ReplicationTarget, error) {
	var out replicationTargets
	return out.Targets, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "replication", "targets")}, &out)
}

// SetReplicationTarget implements AdminClient.
func (c *client) SetReplicationTarget(ctx context.Context, bucket string, t ReplicationTarget) (*ReplicationTarget, error) {
	var out ReplicationTarget
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("buckets", bucket, "replication", "targets"), body: t}, &out)
}

// RemoveReplicationTarget implements AdminClient.
func (c *client) RemoveReplicationTarget(ctx context.Context, bucket, arn string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("buckets", bucket, "replication", "targets"), query: url.Values{"arn": {arn}}}, nil)
}

// GetReplicationStatus implements AdminClient.
func (c *client) GetReplicationStatus(ctx context.Context, bucket string) (*ReplicationStatus, error) {
	var out ReplicationStatus
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "replication", "status")}, &out)
}

// StartReplicationResync implements AdminClient.
func (c *client) StartReplicationResync(ctx context.Context, bucket, target string) (*ReplicationResync, error) {
	var out ReplicationResync
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("buckets", bucket, "replication", "resync"), body: ReplicationResync{Target: target}}, &out)
}
