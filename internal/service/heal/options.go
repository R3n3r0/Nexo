package heal

import (
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Paths of the healing state inside the system volume.
const (
	// MRFFile is the persisted MRF queue of a node, on its local drives.
	MRFFile = "healing/mrf.bin"
	// JobsDir holds the state of the healing jobs (<id>.json), on the
	// drives of the first erasure set of the first pool.
	JobsDir = "healing/jobs"
)

// Deps are the dependencies of the healing service.
type Deps struct {
	// Healer repairs the buckets and objects: the erasure object layer
	// (required).
	Healer object.Healer
	// Pools are the drives of the deployment as given to
	// object.NewServerPools (required): the drives of every erasure set of
	// every pool, local and remote (nil for a drive never reached). The
	// local ones (Endpoint().IsLocal) are formatted and healed by this
	// node.
	Pools []object.PoolSpec
	// Queue is the MRF queue the workers drain; nil runs no MRF worker.
	Queue Queue
	// NodeID names this node in the jobs (default "local").
	NodeID string
	// IDs generates the job ids (default: UUID).
	IDs    topology.IDGenerator
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the healing service.
type Option func(*options)

type options struct {
	mrfWorkers      int
	mrfRemove       bool
	monitorInterval time.Duration
	checkpointEvery int
	checkpointAfter time.Duration
	jobRetention    time.Duration
	jobItems        int
	maxJobs         int
	persistAfter    time.Duration
}

func defaultOptions() options {
	return options{
		mrfWorkers:      2,
		monitorInterval: 30 * time.Second,
		checkpointEvery: 100,
		checkpointAfter: 5 * time.Second,
		jobRetention:    time.Hour,
		jobItems:        1000,
		maxJobs:         4,
		persistAfter:    time.Second,
	}
}

// WithMRFWorkers sets how many objects of the MRF queue are healed
// concurrently (default 2; 0 runs no worker).
func WithMRFWorkers(n int) Option {
	return func(o *options) {
		if n >= 0 {
			o.mrfWorkers = n
		}
	}
}

// WithMRFRemoveDangling lets the MRF workers delete the dangling versions
// they find (the leftovers of a delete that missed a drive). Off by
// default: dangling versions are only removed on explicit request.
func WithMRFRemoveDangling(remove bool) Option {
	return func(o *options) { o.mrfRemove = remove }
}

// WithMonitorInterval sets how often the local drives are checked for a
// missing format.json or an unfinished healing (default 30s; 0 disables
// the monitor, HealFormat and HealDisk are then only run on demand).
func WithMonitorInterval(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.monitorInterval = d
		}
	}
}

// WithCheckpoint sets how often HealDisk saves its progress in the
// healing tracker: every n objects or after d, whichever comes first
// (default 100 objects, 5s).
func WithCheckpoint(n int, d time.Duration) Option {
	return func(o *options) {
		if n > 0 {
			o.checkpointEvery = n
		}
		if d > 0 {
			o.checkpointAfter = d
		}
	}
}

// WithJobRetention sets how long a finished job stays visible (default
// 1h).
func WithJobRetention(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.jobRetention = d
		}
	}
}

// WithJobItems bounds the items a job reports (default 1000).
func WithJobItems(n int) Option {
	return func(o *options) {
		if n >= 0 {
			o.jobItems = n
		}
	}
}

// WithMaxJobs bounds the jobs running at once (default 4): more fail
// with domain.ErrTooManyRequests.
func WithMaxJobs(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxJobs = n
		}
	}
}

// QueueDeps are the dependencies of the MRF queue.
type QueueDeps struct {
	// Drives are the local drives where the queue is persisted (MRFFile on
	// every online one); none disables the persistence.
	Drives []storage.StorageAPI
	Clock  clock.Clock
	Logger *slog.Logger
}

// QueueOption configures the MRF queue.
type QueueOption func(*queueOptions)

type queueOptions struct {
	size            int
	persistInterval time.Duration
	minBackoff      time.Duration
	maxBackoff      time.Duration
	maxAttempts     int
}

func defaultQueueOptions() queueOptions {
	return queueOptions{
		size:            10000,
		persistInterval: 10 * time.Second,
		minBackoff:      time.Second,
		maxBackoff:      5 * time.Minute,
		maxAttempts:     20,
	}
}

// WithQueueSize bounds the entries of the queue (default 10000): further
// notifications are dropped (and counted) until room is made; the scanner
// finds those objects later.
func WithQueueSize(n int) QueueOption {
	return func(o *queueOptions) {
		if n > 0 {
			o.size = n
		}
	}
}

// WithPersistInterval sets how often a changed queue is persisted
// (default 10s).
func WithPersistInterval(d time.Duration) QueueOption {
	return func(o *queueOptions) {
		if d > 0 {
			o.persistInterval = d
		}
	}
}

// WithBackoff sets the delay before the first retry of a failed entry and
// its upper bound; the delay doubles at every attempt (default 1s, 5m).
func WithBackoff(minDelay, maxDelay time.Duration) QueueOption {
	return func(o *queueOptions) {
		if minDelay > 0 {
			o.minBackoff = minDelay
		}
		if maxDelay >= o.minBackoff {
			o.maxBackoff = maxDelay
		}
	}
}

// WithMaxAttempts sets after how many failed heals an entry is abandoned
// (default 20; 0 never abandons it).
func WithMaxAttempts(n int) QueueOption {
	return func(o *queueOptions) {
		if n >= 0 {
			o.maxAttempts = n
		}
	}
}
