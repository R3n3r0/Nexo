// Package pools runs the decommission of a server pool and the rebalance
// between pools (docs/03 §3.8) on top of object.PoolMover.
//
// A decommission drains a pool: no new object, version or upload goes to
// it, and a background job moves every version of every object it holds
// to the other pools, pass after pass until a pass finds nothing left and
// no multipart upload is in progress there. The pool can then be removed
// from the command line. A rebalance moves objects from the pools fuller
// than the average to the emptier ones until every pool is within a few
// percent of it.
//
// The requests (start, cancel) are recorded in a control document of the
// system store, which every node reads to know the draining pools; the
// jobs run on the leader only (cluster.Leader), which records their
// progress in documents of their own: a job resumes where it stopped
// after a restart or a change of leader, and a cancel is never overwritten
// by the progress of the job.
package pools

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Service manages the decommission and rebalance jobs.
type Service interface {
	// Runner reads the control document periodically, applies the
	// draining pools to the object layer and, on the leader, runs the
	// jobs.
	lifecycle.Runner
	// Status returns the pools and the state of their jobs.
	Status(ctx context.Context) (Status, error)
	// Decommission starts draining pool (0-based). The first pool, which
	// holds the system documents, cannot be decommissioned; neither can
	// the last pool that is not draining, nor a pool during a rebalance
	// (domain.ErrInvalidArgument). Starting it again restarts the job.
	Decommission(ctx context.Context, pool int) error
	// CancelDecommission stops the decommission of pool: the pool takes
	// new objects again; the objects already moved stay where they are.
	CancelDecommission(ctx context.Context, pool int) error
	// StartRebalance starts a rebalance (domain.ErrInvalidArgument with a
	// single pool or during a decommission).
	StartRebalance(ctx context.Context) error
	// StopRebalance stops the rebalance.
	StopRebalance(ctx context.Context) error
}

// JobState is the state of a job.
type JobState string

// Job states.
const (
	JobRunning  JobState = "running"
	JobComplete JobState = "complete"
	JobCanceled JobState = "canceled"
	JobFailed   JobState = "failed"
)

// Job is the progress of a decommission or of the rebalance.
type Job struct {
	State     JobState  `json:"state"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// FinishedAt is set once the job completed, failed or was canceled.
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// Pass counts the walks over the objects (a decommission walks until
	// a pass finds nothing left).
	Pass int `json:"pass"`
	// Pool, Bucket and Object are where the job stands: the last object
	// handled (Pool is the source pool of a rebalance).
	Pool   int    `json:"pool"`
	Bucket string `json:"bucket,omitempty"`
	Object string `json:"object,omitempty"`
	// Objects counts the objects moved, Versions their versions and Bytes
	// their stored size.
	Objects  int64 `json:"objects"`
	Versions int64 `json:"versions"`
	Bytes    int64 `json:"bytes"`
	// Failed counts the objects that could not be moved during the
	// current pass (they are retried by the next one); Seen the objects
	// the current pass found in the pool.
	Failed int64 `json:"failed"`
	Seen   int64 `json:"seen"`
	// NextPassAt delays the next pass of a decommission that left
	// objects behind.
	NextPassAt time.Time `json:"nextPassAt,omitzero"`
	// PendingUploads counts the multipart uploads still in progress in
	// a pool whose objects are all moved: the decommission waits for them
	// to complete or be aborted.
	PendingUploads int    `json:"pendingUploads,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Pool is a server pool and its decommission.
type Pool struct {
	// Index is the position of the pool in the command line (0-based).
	Index int
	// ID identifies the pool across restarts: its command-line arguments.
	ID    string
	Total uint64
	Free  uint64
	// Draining reports a pool that receives no new objects.
	Draining bool
	// Decommission is the decommission of the pool, nil when none was
	// started.
	Decommission *Job
}

// Status is the state of the pools.
type Status struct {
	Pools []Pool
	// Rebalance is the last rebalance, nil when none was started.
	Rebalance *Job
}
