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
	// MetadataLog describes the metadata log of a drive in log mode (nil
	// for a drive keeping xl.meta files).
	MetadataLog *MetadataLogInfo
	// Error is set when the drive could not be queried.
	Error string
}

// MetadataLogInfo describes the metadata log of a drive.
type MetadataLogInfo struct {
	Segments int
	// Bytes are the bytes of the segments, LiveBytes those still holding
	// the current metadata (the rest is garbage the compaction reclaims).
	Bytes     int64
	LiveBytes int64
	Objects   int64
	// IndexBytes estimates the memory of the index, IndexBudget is the
	// share of the RAM it may use.
	IndexBytes  uint64
	IndexBudget uint64
	SegmentSize int64
	// Counters since the drive opened.
	Flushes        uint64
	Records        uint64
	FlushSeconds   float64
	Compactions    uint64
	CompactedBytes uint64
	DamagedAreas   uint64
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
	// RetryAttempts counts the passes started over because objects failed
	// to heal (ObjectsFailed counts those of the current pass).
	RetryAttempts int
}

// NodeInfo describes a server node.
type NodeInfo struct {
	Endpoint string
	State    string
	Version  string
	Commit   string
	Uptime   time.Duration
	Disks    []DiskInfo
	// Local marks the node that answered (the first one of
	// ServerInfo.Nodes).
	Local bool
	// ClockSkew is the clock offset of the node measured by the node that
	// answered; nil for that node and for the unreachable ones.
	ClockSkew *ClockSkew
	// HealQueue are the counters of the background healing (MRF) of the
	// node; nil when the node has none (a single drive) or is unreachable.
	HealQueue *HealQueueStats
}

// ClockSkew is the clock offset of a node (docs/03 §3.11).
type ClockSkew struct {
	// Offset is the clock of the node minus the clock of the node
	// measuring it, corrected by half the round trip.
	Offset time.Duration
	RTT    time.Duration
	// Exceeded reports an offset above the warning threshold (2s).
	Exceeded bool
}

// HealQueueStats are the counters of the background healing queue of a
// node (MRF, docs/03 §3.7): the objects written or read without every
// drive, repaired in the background.
type HealQueueStats struct {
	// Pending counts the objects waiting to be healed or being healed.
	Pending int
	// Healed counts the objects repaired, Retried the attempts that
	// failed and were rescheduled, Dropped the objects refused (queue
	// full) or abandoned (too many attempts).
	Healed  uint64
	Retried uint64
	Dropped uint64
}

// Erasure set states as reported by ServerInfo: whether the drives that
// answer keep the read and write quorums of the set.
const (
	// SetStateOK: every drive is online and none is healing.
	SetStateOK = "ok"
	// SetStateDegraded: some drives are offline or healing, the write
	// quorum is kept.
	SetStateDegraded = "degraded"
	// SetStateReadOnly: only the read quorum is kept.
	SetStateReadOnly = "readonly"
	// SetStateUnavailable: not even the read quorum is kept.
	SetStateUnavailable = "unavailable"
)

// ErasureSetInfo is the state of an erasure set of a pool.
type ErasureSetInfo struct {
	Index int
	// Drives is the size of the set; Online counts the drives that answer
	// (the healing ones included), Offline the others, Healing the drives
	// being repopulated.
	Drives  int
	Online  int
	Offline int
	Healing int
	// State is one of the SetState* constants.
	State string
}

// PoolInfo summarizes an erasure pool.
type PoolInfo struct {
	Index        int
	Sets         int
	DrivesPerSet int
	Parity       int
	// ReadQuorum and WriteQuorum are the drives of a set needed to read
	// and to write with the default parity of the pool (docs/03 §3.3).
	ReadQuorum  int
	WriteQuorum int
	// ErasureSets are the states of the sets, from the drives of
	// ServerInfo.Nodes.
	ErasureSets []ErasureSetInfo
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
	// KMS is the state of the KMS of the server-side encryption (nil when
	// the server does not report it).
	KMS *KMSInfo
	// TLS is the state of the TLS of the listeners of the node that
	// answers (nil when the server does not report it).
	TLS *TLSInfo
	// Telemetry is the state of the distributed tracing (OpenTelemetry)
	// of the node that answers (nil when the server does not report it).
	Telemetry *TelemetryInfo
}

// TLSInfo is the state of the TLS of the listeners of a node (docs/07
// §7.3).
type TLSInfo struct {
	// Enabled is false when the listeners serve plain HTTP (no
	// certificates directory).
	Enabled bool
	// MinVersion is the oldest TLS version the listeners accept ("1.2",
	// "1.3").
	MinVersion string
	// InternodeVerify reports mutual TLS between the nodes.
	InternodeVerify bool
	// Certificates are the certificates of the certificates directory, the
	// default one (Name "") first, then those selected by SNI.
	Certificates []TLSCertificate
}

// TLSCertificate describes a certificate of the listeners, never its key.
type TLSCertificate struct {
	// Name is "" for the default certificate, the name of its
	// sub-directory for a certificate selected by SNI.
	Name string
	// Domains are the DNS names of the leaf certificate, IPs its
	// addresses.
	Domains     []string
	IPs         []string
	Subject     string
	Issuer      string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string
}

// TelemetryInfo is the state of the distributed tracing (OpenTelemetry,
// docs/07 §7.4).
type TelemetryInfo struct {
	// Enabled reports that the spans are exported to an OTLP collector.
	Enabled bool
	// Endpoint is the URL of the collector, without user info nor query.
	Endpoint string
	// Protocol is "http/protobuf" or "grpc".
	Protocol string
	// SampleRatio is the share (0-1) of the traces started by the node
	// that are recorded.
	SampleRatio float64
}

// KMSInfo is the state of the KMS of the server-side encryption.
type KMSInfo struct {
	// Configured is false without KMS (SSE-S3 and SSE-KMS refused).
	Configured bool
	// Backend is "builtin" (master key from the environment) or "kes".
	Backend    string
	DefaultKey string
	// Endpoints are the servers of an external KMS.
	Endpoints []string
	// Online reports that the KMS answers; Error why it does not.
	Online bool
	Error  string
	// Keys are the names of the keys of the KMS (the first ones), nil when
	// they cannot be listed.
	Keys []string
}
