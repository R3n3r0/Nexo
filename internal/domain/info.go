package domain

import "time"

// Drive and node states as reported by ServerInfo.
const (
	StateOnline  = "online"
	StateOffline = "offline"
	StateHealing = "healing"
	StateUnknown = "unknown"
)

// DiskInfo describes a drive and its usage.
type DiskInfo struct {
	Endpoint string
	Path     string
	// ID is the drive identifier from format.json.
	ID      string
	PoolIdx int
	SetIdx  int
	DiskIdx int
	State   string
	// Total, Free and Used are bytes.
	Total      uint64
	Free       uint64
	Used       uint64
	UsedInodes uint64
	FreeInodes uint64
	FSType     string
	RootDisk   bool
	Healing    bool
	// HealingProgress is the progress of the healing of the drive (a
	// replaced or wiped drive being repopulated), nil when not healing.
	HealingProgress *HealingProgress
	Scanning        bool
	// Error is set when the drive could not be queried.
	Error string
}

// HealingProgress is the progress of the healing of a drive, from its
// healing tracker.
type HealingProgress struct {
	Started    time.Time
	LastUpdate time.Time
	// Bucket and Object are the last item healed.
	Bucket string
	Object string
	// HealedBuckets counts the buckets completely healed.
	HealedBuckets int
	ObjectsHealed uint64
	ObjectsFailed uint64
	BytesDone     uint64
	BytesFailed   uint64
}

// NodeInfo describes a server node.
type NodeInfo struct {
	Endpoint string
	State    string
	Version  string
	Commit   string
	Uptime   time.Duration
	Disks    []DiskInfo
}

// PoolInfo summarizes an erasure pool.
type PoolInfo struct {
	Index        int
	Sets         int
	DrivesPerSet int
	Parity       int
}

// ServerInfo is the aggregated server/cluster information.
type ServerInfo struct {
	Version      string
	Commit       string
	BuildDate    string
	Mode         Mode
	Region       string
	DeploymentID string
	StartedAt    time.Time
	Uptime       time.Duration
	Pools        []PoolInfo
	Nodes        []NodeInfo
	// Buckets, Objects and Usage are the data usage totals when known.
	Buckets uint64
	Objects uint64
	Usage   uint64
}
