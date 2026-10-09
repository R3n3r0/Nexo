// Package heal is the healing service of the erasure-coded deployments
// (docs/03 §3.7): it repairs objects and buckets through the object
// layer's Healer and reports the state of every drive before and after,
// drains the MRF queue (the writes that missed a drive and the reads that
// met damaged data), formats the empty drives found in an existing
// deployment and heals them completely with a checkpoint that survives
// restarts (healing.bin), and runs the healing jobs of the Admin API
// (nexoctl admin heal).
//
// The MRF queue (Queue, NewQueue) is a separate service so that the
// composition root can give it to the object layer as its
// object.HealNotifier before the layer exists, and the layer to the
// healing service (New) afterwards: the queue only records, the service
// heals.
//
// Every node heals what it owns: its MRF entries and its local drives
// (format and drive healing); a job runs on the node that received the
// request and its progress is persisted on the drives of the first
// erasure set, so that any node answers about it.
package heal

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Options tunes a healing operation.
type Options struct {
	// DryRun reports what would be healed without writing anything.
	DryRun bool `json:"dryRun,omitempty"`
	// Deep verifies the bitrot hash of every shard block (reads all the
	// data); otherwise shard files are checked for presence and size.
	Deep bool `json:"deep,omitempty"`
	// Remove deletes the dangling versions found: present on too few
	// drives to ever be read or rebuilt. It is never implied.
	Remove bool `json:"remove,omitempty"`
}

// ObjectResult is the outcome of healing one object (one version, or
// every version with HealPrefix and the jobs).
type ObjectResult struct {
	Bucket string `json:"bucket"`
	Object string `json:"object"`
	// VersionIDs are the versions examined.
	VersionIDs []string `json:"versionIds,omitempty"`
	// Drives is the state of every drive of the object's set before and
	// after the operation (the worst across the versions).
	Drives []object.HealDrive `json:"drives,omitempty"`
	// Healed counts the drive copies rebuilt (or that would be, with
	// DryRun).
	Healed int `json:"healed"`
	// Dangling counts the dangling versions found, Removed those deleted.
	Dangling int `json:"dangling,omitempty"`
	Removed  int `json:"removed,omitempty"`
	// Bytes is the logical size of the versions examined.
	Bytes int64 `json:"bytes"`
}

// BucketResult is the outcome of healing one bucket (its volume and
// record on every drive of every set).
type BucketResult struct {
	Bucket string             `json:"bucket"`
	Drives []object.HealDrive `json:"drives,omitempty"`
	Healed int                `json:"healed"`
}

// FormatResult is the outcome of HealFormat: the state of the format.json
// of every drive of the deployment before and after.
type FormatResult struct {
	Drives []object.HealDrive `json:"drives,omitempty"`
	// Healed lists the endpoints of the drives formatted and marked for
	// healing (or that would be, with DryRun).
	Healed []string `json:"healed,omitempty"`
}

// DiskResult is the outcome of HealDisk.
type DiskResult struct {
	Endpoint string `json:"endpoint"`
	// Resumed reports that the healing continued from a checkpoint.
	Resumed bool `json:"resumed"`
	// Buckets is the number of buckets healed on the drive.
	Buckets int `json:"buckets"`
	// Objects, Healed and Failed count the objects examined, repaired and
	// failed during this run; Bytes their logical size.
	Objects uint64 `json:"objects"`
	Healed  uint64 `json:"healed"`
	Failed  uint64 `json:"failed"`
	Bytes   uint64 `json:"bytes"`
	// Started is when the drive healing began (the first run).
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// Summary sums up a HealPrefix walk.
type Summary struct {
	Buckets  uint64 `json:"buckets"`
	Objects  uint64 `json:"objects"`
	Healed   uint64 `json:"healed"`
	Failed   uint64 `json:"failed"`
	Dangling uint64 `json:"dangling"`
	Removed  uint64 `json:"removed"`
	Bytes    uint64 `json:"bytes"`
}

// ObjectFunc receives the outcome of every object of a HealPrefix walk,
// with the error of a failed one. It runs on the walking goroutine.
type ObjectFunc func(res ObjectResult, err error)

// JobRequest are the parameters of a healing job.
type JobRequest struct {
	// Bucket is the bucket to heal; empty heals the format of the drives
	// and every bucket.
	Bucket string `json:"bucket,omitempty"`
	// Prefix narrows a recursive job to the objects under it; without
	// Recursive it names the one object to heal.
	Prefix string `json:"prefix,omitempty"`
	// Recursive heals every object below the bucket(s) or prefix.
	Recursive bool `json:"recursive,omitempty"`
	Options
}

// JobState is the state of a healing job.
type JobState string

// Job states.
const (
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobFailed   JobState = "failed"
	JobCanceled JobState = "canceled"
)

// Job is the state of a healing job (GET /heal/{id}).
type Job struct {
	ID      string     `json:"id"`
	Request JobRequest `json:"request"`
	State   JobState   `json:"state"`
	// Node is the node running the job.
	Node       string    `json:"node,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	// Current is the item being healed ("bucket/object").
	Current string `json:"current,omitempty"`
	// Scanned counts the items examined (format, buckets, objects),
	// Healed those repaired on at least one drive (or that would be, with
	// DryRun), Failed those that could not be healed.
	Scanned  uint64 `json:"scanned"`
	Healed   uint64 `json:"healed"`
	Failed   uint64 `json:"failed"`
	Dangling uint64 `json:"dangling"`
	Removed  uint64 `json:"removed"`
	Bytes    uint64 `json:"bytes"`
	// Drives sums up the job drive by drive.
	Drives []JobDrive `json:"drives,omitempty"`
	// Items are the items that needed healing, were dangling or failed
	// (at most the job's item limit; ItemsTruncated reports the others).
	Items          []JobItem `json:"items,omitempty"`
	ItemsTruncated bool      `json:"itemsTruncated,omitempty"`
	// Error is the failure of a failed job.
	Error string `json:"error,omitempty"`
}

// JobDrive is the summary of a job on one drive.
type JobDrive struct {
	Endpoint string `json:"endpoint"`
	Pool     int    `json:"pool"`
	Set      int    `json:"set"`
	Disk     int    `json:"disk"`
	// State is the last state seen: "online" or "offline".
	State string `json:"state"`
	// Missing, Corrupt and Offline count the items found in that state on
	// the drive; Healed the copies rebuilt on it.
	Missing uint64 `json:"missing"`
	Corrupt uint64 `json:"corrupt"`
	Offline uint64 `json:"offline"`
	Healed  uint64 `json:"healed"`
}

// Kinds of job items.
const (
	ItemFormat = "format"
	ItemBucket = "bucket"
	ItemObject = "object"
)

// JobItem is an item of a job that needed healing, was dangling or failed.
type JobItem struct {
	Kind   string             `json:"kind"`
	Bucket string             `json:"bucket,omitempty"`
	Object string             `json:"object,omitempty"`
	Drives []object.HealDrive `json:"drives,omitempty"`
	Healed int                `json:"healed"`
	// Dangling reports a dangling version (Removed: deleted).
	Dangling bool   `json:"dangling,omitempty"`
	Removed  bool   `json:"removed,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Service heals the deployment. Start resumes the drive healings left
// unfinished, loads nothing else (the MRF queue loads itself) and starts
// the MRF workers and the drive monitor (format healing and drive
// healing of the local drives); Stop cancels the running jobs and waits
// for the workers.
type Service interface {
	lifecycle.Runner

	// HealObject heals one version of bucket/object ("" is the latest;
	// every version with HealPrefix). It fails with
	// domain.ErrObjectNotFound when no drive holds it.
	HealObject(ctx context.Context, bucket, object, versionID string, opts Options) (ObjectResult, error)
	// HealBucket creates the bucket and its record on the drives that
	// miss them. It fails with domain.ErrBucketNotFound when no drive
	// holds it.
	HealBucket(ctx context.Context, bucket string, opts Options) (BucketResult, error)
	// HealPrefix heals the buckets and every version of every object under
	// prefix of bucket (every bucket when bucket is empty), walking the
	// union of the drives so that objects missing from a quorum of drives
	// are found too, and reports each object to fn (optional).
	HealPrefix(ctx context.Context, bucket, prefix string, opts Options, fn ObjectFunc) (Summary, error)
	// HealFormat gives the online local drives without a format.json (new
	// or wiped drives in an existing deployment) the document of their
	// position and a healing tracker (healing.bin), so that HealDisk
	// repopulates them. Remote drives are reported, their node heals them.
	HealFormat(ctx context.Context, opts Options) (FormatResult, error)
	// HealPools creates the buckets of the deployment on the pools that
	// miss them and returns how many it created: a pool added by an
	// expansion (docs/03 §3.8) starts empty, and a new object placed on it
	// would not find its bucket (MakeBucket creates the later buckets on
	// every pool). The candidates are the volumes of every online drive,
	// each listed within a bound so that an unresponsive node does not
	// hold the caller; HealBucket only creates what is missing and
	// refuses the remnants of a rolled-back creation that no set holds.
	// Nothing is done for a single pool. The error is the first failed
	// bucket (the others are still tried): the healing jobs and the
	// scanner repair them later.
	HealPools(ctx context.Context) (int, error)
	// HealDisk heals every bucket, object and system file of the set of
	// the local drive endpoint onto it, checkpointing its progress (last
	// bucket and object, counters) in its healing tracker and resuming
	// from it; the tracker is removed once the drive is healed. A drive
	// without tracker gets a new one.
	HealDisk(ctx context.Context, endpoint string) (DiskResult, error)

	// StartJob starts a healing job in the background and returns its
	// initial state; an identical job already running is returned instead
	// of starting another one.
	StartJob(ctx context.Context, in JobRequest) (Job, error)
	// Job returns the state of a job (domain.ErrNotFound when unknown).
	Job(ctx context.Context, id string) (Job, error)
	// Jobs lists the running and recently finished jobs, newest first.
	Jobs(ctx context.Context) ([]Job, error)
}

// Entry is an object queued for healing.
type Entry struct {
	Bucket    string `json:"bucket"`
	Object    string `json:"object"`
	VersionID string `json:"versionId,omitempty"`
	// Attempts counts the failed heals so far.
	Attempts int `json:"attempts,omitempty"`
	// Waits counts the heals that found a drive of the set offline (the
	// entry waits for it to come back, with backoff, never abandoned).
	Waits int `json:"waits,omitempty"`
	// Queued is when the entry was first queued; NotBefore when it may be
	// tried again (backoff).
	Queued    time.Time `json:"queued"`
	NotBefore time.Time `json:"notBefore,omitempty"`
}

// QueueStats are the counters of the MRF queue.
type QueueStats struct {
	// Pending counts the entries waiting (Leased those being healed).
	Pending int `json:"pending"`
	Leased  int `json:"leased"`
	// Queued counts the notifications accepted, Merged those merged into
	// an entry already queued, Dropped those refused (queue full) or
	// abandoned (too many attempts).
	Queued  uint64 `json:"queued"`
	Merged  uint64 `json:"merged"`
	Dropped uint64 `json:"dropped"`
	// Healed and Retried count the outcomes reported with Done.
	Healed  uint64 `json:"healed"`
	Retried uint64 `json:"retried"`
}

// Queue is the MRF (most recently failed) queue: the object layer reports
// the objects that need healing through NotifyHeal (never blocking), the
// queue keeps them in memory, bounded and deduplicated, and persists them
// periodically on the local drives (.nexo.sys/healing/mrf.bin) so that
// they survive a restart; the healing service leases them with Next and
// reports the outcome with Done, which drops a healed entry and
// reschedules a failed one with exponential backoff. Start loads the
// persisted entries and starts the periodic persistence, Stop persists
// the queue a last time.
type Queue interface {
	object.HealNotifier
	lifecycle.Runner
	// Next waits for an entry that is due, leases it and returns it; it
	// fails with the context error when ctx ends.
	Next(ctx context.Context) (Entry, error)
	// Done ends the lease of e: retry false drops it (healed, or nothing
	// to heal), retry true queues it again with backoff (until the
	// attempts limit, when it is dropped and logged). A notification
	// received for e during the lease queues it again.
	Done(ctx context.Context, e Entry, retry bool)
	// Release ends the lease of e without a failed attempt. With wait
	// false the healing was interrupted (the service is stopping): the
	// entry is due again at once, unchanged, and persisted with the
	// queue. With wait true the heal found a drive of the set offline: the
	// entry is due again after a backoff that grows with its waits, up to
	// the maximum, and it is never abandoned for that reason.
	Release(ctx context.Context, e Entry, wait bool)
	// Stats returns the counters of the queue.
	Stats(ctx context.Context) QueueStats
}
