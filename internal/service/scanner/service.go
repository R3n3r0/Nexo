// Package scanner is the background scanner (docs/03 §3.7): a continuous
// cycle over every bucket and object of the deployment that computes the
// data usage (per bucket and per top-level prefix: objects, versions,
// delete markers, bytes) saved in .nexo.sys/usage.json and served to the
// Admin API, heals a sample of the objects (one every N, with a deep
// bitrot verification every M cycles), aborts the stale multipart uploads
// and hands every version to the lifecycle hook (ILM, M7).
//
// The scanner throttles itself: after each page of objects it sleeps in
// proportion to the work just done (the speed setting picks the factor),
// and it pauses while the API load reported by a LoadSampler is high. It
// runs on the leader only (cluster.Leader): on a single node always.
package scanner

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
)

// Counts are the usage counters of a bucket or prefix: Objects counts the
// live objects (latest versions that are not delete markers), Versions
// the noncurrent versions, DeleteMarkers the delete markers and Size the
// logical bytes of every version.
type Counts struct {
	Objects       uint64 `json:"objects"`
	Versions      uint64 `json:"versions"`
	DeleteMarkers uint64 `json:"deleteMarkers"`
	Size          uint64 `json:"size"`
}

// BucketUsage is the usage of one bucket.
type BucketUsage struct {
	Counts
	// Prefixes breaks the bucket down by top-level prefix ("photos/");
	// the objects at the root of the bucket are under "". At most the
	// scanner's prefix limit are kept: PrefixesTruncated reports the
	// others (still counted in the bucket totals).
	Prefixes          map[string]Counts `json:"prefixes,omitempty"`
	PrefixesTruncated bool              `json:"prefixesTruncated,omitempty"`
}

// Usage is the data usage computed by a scanner cycle (usage.json).
type Usage struct {
	// LastUpdate is when the cycle that computed it ended.
	LastUpdate time.Time              `json:"lastUpdate"`
	Cycle      uint64                 `json:"cycle"`
	Buckets    map[string]BucketUsage `json:"buckets"`
}

// CycleStats describes one scanner cycle.
type CycleStats struct {
	Cycle    uint64    `json:"cycle"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	// Deep reports a cycle whose heal sample verified the bitrot hashes.
	Deep bool `json:"deep"`
	// Buckets and Objects count what was scanned (Objects counts keys,
	// Versions every version and delete marker).
	Buckets  uint64 `json:"buckets"`
	Objects  uint64 `json:"objects"`
	Versions uint64 `json:"versions"`
	// HealChecked counts the sampled objects, HealRepaired those found
	// damaged and repaired, HealFailed those that could not be healed.
	HealChecked  uint64 `json:"healChecked"`
	HealRepaired uint64 `json:"healRepaired"`
	HealFailed   uint64 `json:"healFailed"`
	// LifecycleActions counts the versions the lifecycle hook acted on.
	LifecycleActions uint64 `json:"lifecycleActions"`
	// UploadsCleaned counts the multipart uploads aborted: the stale
	// ones and those the lifecycle rules expire.
	UploadsCleaned int `json:"uploadsCleaned"`
	// Slept is the time spent throttling, Paused the time waiting for the
	// API load to drop.
	Slept  time.Duration `json:"slept"`
	Paused time.Duration `json:"paused"`
}

// Status is the state of the scanner on this node.
type Status struct {
	// Active reports that this node runs the scanner (the leader).
	Active bool `json:"active"`
	// Scanning reports a cycle in progress.
	Scanning bool `json:"scanning"`
	// Cycle is the number of the last cycle started.
	Cycle uint64 `json:"cycle"`
	// Last describes the last completed cycle of this node (zero before).
	Last CycleStats `json:"last"`
}

// Service is the scanner. Start registers it with the leader election (or
// starts it at once without one); Stop ends the running cycle and waits
// for it.
type Service interface {
	lifecycle.Runner
	// Usage returns the data usage of the last completed cycle: the one of
	// this node when it scans, the newest usage.json of the drives
	// otherwise. It fails with domain.ErrNotFound before the first cycle.
	Usage(ctx context.Context) (Usage, error)
	// Scan runs one complete cycle now and returns its statistics; cycles
	// never overlap (Scan waits for the running one).
	Scan(ctx context.Context) (CycleStats, error)
	// Status reports the state of the scanner on this node.
	Status(ctx context.Context) Status
}

// LoadSampler reports the load of the API, from 0 (idle) to 1 (saturated):
// the scanner pauses while it is at or above its threshold.
type LoadSampler interface {
	Load(ctx context.Context) float64
}

// LoadSamplerFunc adapts a function to LoadSampler.
type LoadSamplerFunc func(ctx context.Context) float64

// Load implements LoadSampler.
func (f LoadSamplerFunc) Load(ctx context.Context) float64 { return f(ctx) }

// LifecycleAction is what the lifecycle hook did to a version.
type LifecycleAction int

// Lifecycle actions.
const (
	// LifecycleNone leaves the version as it is.
	LifecycleNone LifecycleAction = iota
	// LifecycleDeleted removed the version (expiration): the usage leaves
	// it out.
	LifecycleDeleted
	// LifecycleTransitioned moved the version to another tier: it is still
	// counted.
	LifecycleTransitioned
)

// Lifecycle applies the lifecycle rules of the buckets (ILM, M7) to the
// versions the scanner meets, latest first for every key, and to the
// incomplete multipart uploads of every bucket. The default does nothing;
// the ilm service implements it.
type Lifecycle interface {
	Apply(ctx context.Context, obj domain.ObjectInfo) (LifecycleAction, error)
	// AbortUploads aborts the incomplete multipart uploads of bucket that
	// its rules expire (AbortIncompleteMultipartUpload) and returns how
	// many; the scanner calls it once per bucket and cycle, after the
	// versions.
	AbortUploads(ctx context.Context, bucket string) (int, error)
}
