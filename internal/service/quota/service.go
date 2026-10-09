// Package quota enforces the hard quotas of the buckets (docs/02 S-BKT-09,
// docs/05 T-7.7). It is a decorator of the object layer (docs/12 §12.2
// rule 9): the writes that add data to a bucket (PutObject, CopyObject,
// UploadPart, UploadPartCopy, CompleteMultipartUpload) are checked
// against the quota of their bucket before any data is written, and
// refused with domain.ErrQuotaExceeded (S3 507 XNexoQuotaExceeded); every
// other operation passes through.
//
// The usage of a bucket is estimated as:
//
//	base (the scanner usage of the deployment, or a walk of the bucket, whichever is newer)
//	+ the bytes this node committed to the bucket after the base was measured
//	+ the bytes of the writes in progress on this node
//
// so that a quota is not exceeded between two scanner cycles. The base
// counts the whole deployment (the scanner and the walk read every node);
// the writes of the other nodes since the base are counted when the base
// is refreshed. A write whose estimate exceeds the quota is refused only
// on a walk of the bucket not older than the request (the concurrent
// refusals share it), so that a refusal is never due to a stale base nor
// to a delete on another node; a write that comes close to the quota
// (WithRefreshRatio) refreshes the base at most once per
// WithRefreshInterval (or sooner after a delete on this node), so that
// the writes of several nodes cannot pile up unnoticed near the limit. A
// write of unknown length (chunked, no Content-Length) streams under the
// remaining allowance and is aborted as soon as it exceeds it: nothing is
// committed.
package quota

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

// Service is the object layer decorated with the quota enforcement.
type Service interface {
	object.Service
	// BucketUsage returns the usage estimate the quota decisions of a
	// bucket rest on (refreshing the base when it is stale).
	BucketUsage(ctx context.Context, bucket string) (Usage, error)
}

// UsageSource provides the data usage computed by the scanner
// (scanner.Service).
type UsageSource interface {
	Usage(ctx context.Context) (scanner.Usage, error)
}

// Deps are the dependencies of the decorator. Objects (the decorated layer)
// and Buckets (the quotas) are required; Usage is optional (without it the
// base is a walk of the bucket).
type Deps struct {
	Objects object.Service
	Buckets object.BucketConfigSource
	Usage   UsageSource
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Base sources of Usage.
const (
	SourceScanner = "scanner"
	SourceWalk    = "walk"
)

// Usage is the usage estimate of a bucket, in bytes of plaintext.
type Usage struct {
	Bucket string `json:"bucket"`
	// Quota is the hard quota (0: none).
	Quota uint64 `json:"quota"`
	// Base is the usage measured at BaseTime by Source.
	Base     uint64    `json:"base"`
	BaseTime time.Time `json:"baseTime"`
	Source   string    `json:"source"`
	// Recent counts the bytes this node committed after BaseTime,
	// Reserved those of the writes in progress on this node.
	Recent   uint64 `json:"recent"`
	Reserved uint64 `json:"reserved"`
}
