package nexoadmin

import (
	"encoding/json"
	"time"
)

// The types of this file mirror the schemas of api/openapi.yaml; the
// contract tests check both against the server.

// Session is a console session (GET /auth/session).
type Session struct {
	ID        string          `json:"id"`
	AccessKey string          `json:"accessKey"`
	Identity  SessionIdentity `json:"identity"`
	IssuedAt  time.Time       `json:"issuedAt"`
	ExpiresAt time.Time       `json:"expiresAt"`
	CSRFToken string          `json:"csrfToken"`
}

// SessionIdentity is the principal behind a session.
type SessionIdentity struct {
	AccessKey   string   `json:"accessKey"`
	IsRoot      bool     `json:"isRoot"`
	Groups      []string `json:"groups"`
	PolicyNames []string `json:"policyNames"`
}

// Permissions are the effective permissions of the caller (or of User).
type Permissions struct {
	User    string   `json:"user"`
	IsRoot  bool     `json:"isRoot"`
	Bucket  string   `json:"bucket,omitempty"`
	Prefix  string   `json:"prefix,omitempty"`
	Allowed []string `json:"allowed"`
	Denied  []string `json:"denied"`
}

// PermissionsQuery selects the permissions to compute.
type PermissionsQuery struct {
	// User evaluates another user (requires admin:GetUser).
	User   string
	Bucket string
	Prefix string
}

// ServerInfo is the server and cluster information.
type ServerInfo struct {
	Version       string     `json:"version"`
	Commit        string     `json:"commit"`
	BuildDate     string     `json:"buildDate"`
	Mode          string     `json:"mode"`
	Region        string     `json:"region"`
	DeploymentID  string     `json:"deploymentId"`
	StartedAt     time.Time  `json:"startedAt"`
	UptimeSeconds int64      `json:"uptimeSeconds"`
	Buckets       uint64     `json:"buckets"`
	Objects       uint64     `json:"objects"`
	Usage         uint64     `json:"usage"`
	Pools         []PoolInfo `json:"pools"`
	Nodes         []NodeInfo `json:"nodes"`
	// TLS is the TLS of the listeners of the answering node (nil from a
	// server that does not report it).
	TLS *TLSInfo `json:"tls,omitempty"`
	// Telemetry is the distributed tracing of the answering node (nil
	// from a server that does not report it).
	Telemetry *TelemetryInfo `json:"telemetry,omitempty"`
}

// TLSInfo is the TLS of the listeners of a node.
type TLSInfo struct {
	// Enabled is false when the listeners serve plain HTTP.
	Enabled bool `json:"enabled"`
	// MinVersion is the oldest TLS version accepted ("1.2", "1.3").
	MinVersion string `json:"minVersion,omitempty"`
	// InternodeVerify reports mutual TLS between the nodes.
	InternodeVerify bool `json:"internodeVerify"`
	// Certificates are those of the certificates directory, the default
	// one first.
	Certificates []TLSCertificate `json:"certificates"`
}

// TLSCertificate is a certificate of the listeners (never its key).
type TLSCertificate struct {
	// Name is "" for the default certificate, the sub-directory of a
	// certificate selected by SNI.
	Name        string    `json:"name"`
	Domains     []string  `json:"domains,omitempty"`
	IPs         []string  `json:"ips,omitempty"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
}

// TelemetryInfo is the distributed tracing (OpenTelemetry) of a node.
type TelemetryInfo struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the URL of the OTLP collector, without user info.
	Endpoint string `json:"endpoint,omitempty"`
	// Protocol is http/protobuf or grpc.
	Protocol    string  `json:"protocol,omitempty"`
	SampleRatio float64 `json:"sampleRatio"`
}

// PoolInfo is an erasure pool.
type PoolInfo struct {
	Index        int `json:"index"`
	Sets         int `json:"sets"`
	DrivesPerSet int `json:"drivesPerSet"`
	Parity       int `json:"parity"`
	// ReadQuorum and WriteQuorum are the drives of a set needed to read
	// and to write.
	ReadQuorum  int              `json:"readQuorum"`
	WriteQuorum int              `json:"writeQuorum"`
	ErasureSets []ErasureSetInfo `json:"erasureSets"`
}

// Erasure set states.
const (
	SetStateOK          = "ok"
	SetStateDegraded    = "degraded"
	SetStateReadOnly    = "readonly"
	SetStateUnavailable = "unavailable"
)

// ErasureSetInfo is the state of an erasure set: ok (every drive online),
// degraded (drives offline or healing, the write quorum is kept),
// readonly (only the read quorum is kept) or unavailable.
type ErasureSetInfo struct {
	Index int `json:"index"`
	// Drives is the size of the set; Online counts the drives that
	// answer, the healing ones included.
	Drives  int    `json:"drives"`
	Online  int    `json:"online"`
	Offline int    `json:"offline"`
	Healing int    `json:"healing"`
	State   string `json:"state"`
}

// NodeInfo is a node of the deployment.
type NodeInfo struct {
	Endpoint      string     `json:"endpoint"`
	State         string     `json:"state"`
	Version       string     `json:"version"`
	Commit        string     `json:"commit"`
	UptimeSeconds int64      `json:"uptimeSeconds"`
	Disks         []DiskInfo `json:"disks"`
	// Local marks the node that answered (listed first).
	Local bool `json:"local"`
	// ClockSkew is the clock offset of the node measured by the node that
	// answered; nil for that node and for the unreachable ones.
	ClockSkew *ClockSkew `json:"clockSkew,omitempty"`
	// HealQueue is the background healing of the node; nil on a single
	// drive and for the unreachable nodes.
	HealQueue *HealQueueStats `json:"healQueue,omitempty"`
}

// ClockSkew is the clock offset of a node.
type ClockSkew struct {
	// OffsetMs is the clock of the node minus the clock of the node that
	// answered, in milliseconds.
	OffsetMs int64 `json:"offsetMs"`
	RTTMs    int64 `json:"rttMs"`
	// Exceeded reports an offset above the warning threshold (2s).
	Exceeded bool `json:"exceeded"`
}

// HealQueueStats are the counters of the background healing (MRF) of a
// node.
type HealQueueStats struct {
	// Pending counts the objects waiting to be healed or being healed.
	Pending int    `json:"pending"`
	Healed  uint64 `json:"healed"`
	Retried uint64 `json:"retried"`
	// Dropped counts the objects refused (queue full) or abandoned.
	Dropped uint64 `json:"dropped"`
}

// DiskInfo is a drive of a node.
type DiskInfo struct {
	Endpoint string `json:"endpoint"`
	Path     string `json:"path"`
	ID       string `json:"id"`
	PoolIdx  int    `json:"poolIdx"`
	SetIdx   int    `json:"setIdx"`
	DiskIdx  int    `json:"diskIdx"`
	State    string `json:"state"`
	Total    uint64 `json:"total"`
	Free     uint64 `json:"free"`
	Used     uint64 `json:"used"`
	Healing  bool   `json:"healing"`
	// HealingProgress is the progress of the drive healing, when healing.
	HealingProgress *HealingProgress `json:"healingProgress,omitempty"`
	// MetadataLog describes the metadata log of a drive in log mode.
	MetadataLog *MetadataLog `json:"metadataLog,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// MetadataLog is the metadata log of a drive in DiskInfo.
type MetadataLog struct {
	Segments int `json:"segments"`
	// Bytes of the segments; LiveBytes still hold current metadata (the
	// rest is garbage the compaction reclaims).
	Bytes       int64  `json:"bytes"`
	LiveBytes   int64  `json:"liveBytes"`
	Objects     int64  `json:"objects"`
	IndexBytes  uint64 `json:"indexBytes"`
	IndexBudget uint64 `json:"indexBudget"`
	SegmentSize int64  `json:"segmentSize"`
	// Counters since the drive opened.
	Flushes        uint64  `json:"flushes"`
	Records        uint64  `json:"records"`
	FlushSeconds   float64 `json:"flushSeconds"`
	Compactions    uint64  `json:"compactions"`
	CompactedBytes uint64  `json:"compactedBytes"`
	DamagedAreas   uint64  `json:"damagedAreas"`
}

// MetadataLogCompaction is the result of CompactMetadataLog: the
// compaction of each drive in log mode of the answering node.
type MetadataLogCompaction struct {
	Drives []MetadataLogDriveCompaction `json:"drives"`
}

// MetadataLogDriveCompaction is the compaction of one drive.
type MetadataLogDriveCompaction struct {
	Endpoint string `json:"endpoint"`
	// Segments rewritten and bytes of garbage reclaimed.
	Segments       int    `json:"segments"`
	ReclaimedBytes int64  `json:"reclaimedBytes"`
	Error          string `json:"error,omitempty"`
}

// HealingProgress is the progress of the healing of a replaced or wiped
// drive.
type HealingProgress struct {
	Started       time.Time `json:"started"`
	LastUpdate    time.Time `json:"lastUpdate"`
	Bucket        string    `json:"bucket,omitempty"`
	Object        string    `json:"object,omitempty"`
	HealedBuckets int       `json:"healedBuckets"`
	ObjectsHealed uint64    `json:"objectsHealed"`
	ObjectsFailed uint64    `json:"objectsFailed"`
	BytesDone     uint64    `json:"bytesDone"`
	BytesFailed   uint64    `json:"bytesFailed"`
	// RetryAttempts counts the passes started over because objects failed
	// to heal (ObjectsFailed counts those of the current pass).
	RetryAttempts int `json:"retryAttempts,omitempty"`
}

// DataUsage is the data usage, total and per bucket.
type DataUsage struct {
	LastUpdate    time.Time              `json:"lastUpdate"`
	ObjectsCount  uint64                 `json:"objectsCount"`
	VersionsCount uint64                 `json:"versionsCount"`
	TotalSize     uint64                 `json:"totalSize"`
	BucketsCount  uint64                 `json:"bucketsCount"`
	Buckets       map[string]BucketUsage `json:"buckets"`
}

// BucketUsage is the usage of one bucket.
type BucketUsage struct {
	Size          uint64 `json:"size"`
	ObjectsCount  uint64 `json:"objectsCount"`
	VersionsCount uint64 `json:"versionsCount"`
	DeleteMarkers uint64 `json:"deleteMarkers"`
	// Prefixes breaks the bucket down by top-level prefix, when the
	// scanner computed it.
	Prefixes map[string]PrefixUsage `json:"prefixes,omitempty"`
}

// PrefixUsage is the usage of a top-level prefix of a bucket.
type PrefixUsage struct {
	Size          uint64 `json:"size"`
	ObjectsCount  uint64 `json:"objectsCount"`
	VersionsCount uint64 `json:"versionsCount"`
	DeleteMarkers uint64 `json:"deleteMarkers"`
}

// HealRequest starts a healing job: the whole deployment, a bucket, or
// with Recursive every object below the bucket(s) or Prefix (without
// Recursive, Prefix names one object).
type HealRequest struct {
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	DryRun    bool   `json:"dryRun,omitempty"`
	// Deep verifies the bitrot hash of every shard block.
	Deep bool `json:"deep,omitempty"`
	// Remove deletes the dangling objects found.
	Remove bool `json:"remove,omitempty"`
}

// Heal job states.
const (
	HealRunning  = "running"
	HealDone     = "done"
	HealFailed   = "failed"
	HealCanceled = "canceled"
)

// HealJob is the state of a healing job.
type HealJob struct {
	ID string `json:"id"`
	// State is HealRunning, HealDone, HealFailed or HealCanceled.
	State   string      `json:"state"`
	Request HealRequest `json:"request"`
	// Finished reports a job that is no longer running.
	Finished   bool       `json:"finished"`
	Node       string     `json:"node,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// Current is the item being healed (bucket/object).
	Current string `json:"current,omitempty"`
	// Scanned counts the items examined, Healed those repaired on at
	// least one drive (or that would be, with DryRun), Failed those that
	// could not be healed.
	Scanned  uint64 `json:"scanned"`
	Healed   uint64 `json:"healed"`
	Failed   uint64 `json:"failed"`
	Dangling uint64 `json:"dangling"`
	Removed  uint64 `json:"removed"`
	Bytes    uint64 `json:"bytes"`
	// Drives sums up the job drive by drive.
	Drives []HealJobDrive `json:"drives"`
	// Items are the items that needed healing, were dangling or failed.
	Items          []HealItem `json:"items"`
	ItemsTruncated bool       `json:"itemsTruncated,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// HealJobDrive is the summary of a job on one drive.
type HealJobDrive struct {
	Endpoint string `json:"endpoint"`
	Pool     int    `json:"pool"`
	Set      int    `json:"set"`
	Disk     int    `json:"disk"`
	// State is "online" or "offline".
	State   string `json:"state"`
	Missing uint64 `json:"missing"`
	Corrupt uint64 `json:"corrupt"`
	Offline uint64 `json:"offline"`
	Healed  uint64 `json:"healed"`
}

// HealItem is an item of a job that needed healing.
type HealItem struct {
	// Kind is "format", "bucket" or "object".
	Kind     string          `json:"kind"`
	Bucket   string          `json:"bucket,omitempty"`
	Object   string          `json:"object,omitempty"`
	Healed   int             `json:"healed"`
	Dangling bool            `json:"dangling,omitempty"`
	Removed  bool            `json:"removed,omitempty"`
	Error    string          `json:"error,omitempty"`
	Drives   []HealItemDrive `json:"drives"`
}

// HealItemDrive is the state of an item on one drive before and after
// the healing: "ok", "missing", "corrupt" or "offline".
type HealItemDrive struct {
	Endpoint string `json:"endpoint"`
	Pool     int    `json:"pool"`
	Set      int    `json:"set"`
	Disk     int    `json:"disk"`
	Before   string `json:"before"`
	After    string `json:"after"`
}

// HealJobList is the list of the healing jobs, newest first.
type HealJobList struct {
	Items      []HealJob `json:"items"`
	NextCursor string    `json:"nextCursor"`
}

// ListOptions page a list endpoint.
type ListOptions struct {
	// Cursor is the NextCursor of the previous page.
	Cursor string
	// Limit is 1 to 1000; 0 means the server default (1000).
	Limit int
}

// Bucket is a bucket with its settings and usage.
type Bucket struct {
	Name              string            `json:"name"`
	Created           time.Time         `json:"created"`
	Region            string            `json:"region"`
	Versioning        string            `json:"versioning"`
	ObjectLockEnabled bool              `json:"objectLockEnabled"`
	Objects           uint64            `json:"objects"`
	Versions          uint64            `json:"versions"`
	DeleteMarkers     uint64            `json:"deleteMarkers"`
	Size              uint64            `json:"size"`
	QuotaBytes        uint64            `json:"quotaBytes"`
	Public            bool              `json:"public"`
	Tags              map[string]string `json:"tags"`
	// Partial reports a summary restricted to the name, creation date and
	// region: the caller may list only a part of the bucket.
	Partial bool `json:"partial,omitempty"`
}

// BucketList is a page of buckets.
type BucketList struct {
	Items      []Bucket `json:"items"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// ListBucketsOptions filter and page the buckets.
type ListBucketsOptions struct {
	ListOptions
	Prefix string
}

// CreateBucketRequest creates a bucket.
type CreateBucketRequest struct {
	Name       string `json:"name"`
	Region     string `json:"region,omitempty"`
	Versioning bool   `json:"versioning,omitempty"`
	ObjectLock bool   `json:"objectLock,omitempty"`
	QuotaBytes uint64 `json:"quotaBytes,omitempty"`
	// HLS sets the media rule "hls": every rendition, a poster and
	// thumbnails for every video of the bucket (needs a media worker).
	HLS bool `json:"hls,omitempty"`
	// Images sets the media rule "images": every format (AVIF, WebP,
	// JPEG, PNG) at every width for every image of the bucket (needs a
	// media worker).
	Images bool `json:"images,omitempty"`
}

// BucketQuota is the quota of a bucket; a zero Size disables it.
type BucketQuota struct {
	Type string `json:"type"`
	Size uint64 `json:"size"`
}

// BucketEncryption is the default encryption of a bucket.
type BucketEncryption struct {
	// Algorithm is "AES256" (SSE-S3) or "aws:kms" (SSE-KMS).
	Algorithm string `json:"algorithm"`
	KMSKeyID  string `json:"kmsKeyId,omitempty"`
}

// AccessRule is the anonymous access of one prefix.
type AccessRule struct {
	Prefix string `json:"prefix"`
	// Preset is private, download, upload, public or custom.
	Preset string `json:"preset"`
}

// BucketAccess is the anonymous access of a bucket.
type BucketAccess struct {
	Bucket    string       `json:"bucket"`
	Rules     []AccessRule `json:"rules"`
	Public    bool         `json:"public"`
	HasPolicy bool         `json:"hasPolicy"`
}

// ObjectEntry is an object (version).
type ObjectEntry struct {
	Key            string    `json:"key"`
	VersionID      string    `json:"versionId,omitempty"`
	Size           int64     `json:"size"`
	ETag           string    `json:"etag"`
	LastModified   time.Time `json:"lastModified"`
	ContentType    string    `json:"contentType,omitempty"`
	IsDeleteMarker bool      `json:"isDeleteMarker"`
	IsLatest       bool      `json:"isLatest"`
	StorageClass   string    `json:"storageClass,omitempty"`
}

// ObjectList is a page of the folder view of a bucket.
type ObjectList struct {
	Objects     []ObjectEntry `json:"objects"`
	Prefixes    []string      `json:"prefixes"`
	IsTruncated bool          `json:"isTruncated"`
	NextCursor  string        `json:"nextCursor,omitempty"`
}

// ListObjectsOptions select a listing.
type ListObjectsOptions struct {
	ListOptions
	Prefix string
	// Recursive lists every key under Prefix (no delimiter) instead of the
	// folder view.
	Recursive bool
}

// ObjectDetails are the metadata of an object.
type ObjectDetails struct {
	ObjectEntry
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentLanguage    string            `json:"contentLanguage,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	Expires            *time.Time        `json:"expires,omitempty"`
	UserMetadata       map[string]string `json:"userMetadata"`
	Parts              int               `json:"parts,omitempty"`
	ChecksumAlgorithm  string            `json:"checksumAlgorithm,omitempty"`
	Checksum           string            `json:"checksum,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Versions           []ObjectEntry     `json:"versions,omitempty"`
	VersionsTruncated  bool              `json:"versionsTruncated,omitempty"`
}

// ObjectRef names an object or one of its versions.
type ObjectRef struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

// DeleteObjectsRequest deletes objects and prefixes.
type DeleteObjectsRequest struct {
	Objects []ObjectRef `json:"objects,omitempty"`
	// Prefixes are folders deleted recursively: "photos" is "photos/",
	// never "photos2/" ("" is the whole bucket).
	Prefixes    []string `json:"prefixes,omitempty"`
	AllVersions bool     `json:"allVersions,omitempty"`
	// BypassGovernance deletes the versions a GOVERNANCE retention
	// protects, where the caller has s3:BypassGovernanceRetention.
	BypassGovernance bool `json:"bypassGovernance,omitempty"`
}

// DeletedObject is a successful deletion.
type DeletedObject struct {
	Key                   string `json:"key"`
	VersionID             string `json:"versionId,omitempty"`
	DeleteMarker          bool   `json:"deleteMarker"`
	DeleteMarkerVersionID string `json:"deleteMarkerVersionId,omitempty"`
}

// ObjectError is a per-object failure.
type ObjectError struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// DeleteObjectsResult reports every object of a deletion.
type DeleteObjectsResult struct {
	Deleted []DeletedObject `json:"deleted"`
	Errors  []ObjectError   `json:"errors"`
}

// Share is a managed share link.
type Share struct {
	ID                 string `json:"id"`
	Owner              string `json:"owner"`
	Bucket             string `json:"bucket"`
	Key                string `json:"key"`
	Prefix             bool   `json:"prefix"`
	Permission         string `json:"permission"`
	VersionID          string `json:"versionId,omitempty"`
	ContentType        string `json:"contentType,omitempty"`
	ContentDisposition string `json:"contentDisposition,omitempty"`
	// Stream marks the HLS stream shares of a video: URL is the master
	// playlist.
	Stream bool `json:"stream,omitempty"`
	// Image marks the image shares: URL is the image route of the object
	// (the variants the media worker derived).
	Image     bool      `json:"image,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	URL       string    `json:"url,omitempty"`
	AccessKey string    `json:"accessKey"`
}

// ShareList is a page of share links.
type ShareList struct {
	Items      []Share `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

// ListSharesOptions filter and page the share links.
type ListSharesOptions struct {
	ListOptions
	Owner  string
	Bucket string
}

// Share permissions.
const (
	PermissionDownload = "download"
	PermissionUpload   = "upload"
)

// CreateShareRequest creates a managed share link.
type CreateShareRequest struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Prefix bool   `json:"prefix,omitempty"`
	// Permission is PermissionDownload (default) or PermissionUpload.
	Permission string `json:"permission,omitempty"`
	// ExpiresIn is the validity in seconds (at most 7 days).
	ExpiresIn          int64  `json:"expiresIn,omitempty"`
	ContentType        string `json:"contentType,omitempty"`
	ContentDisposition string `json:"contentDisposition,omitempty"`
	VersionID          string `json:"versionId,omitempty"`
	// Stream shares a video as an HLS stream: the URL of the share is its
	// master playlist (object download shares only).
	Stream bool `json:"stream,omitempty"`
	// Image shares the variants of an image the media worker derived: the
	// URL of the share serves the image at its own size in the most
	// compact format the client accepts (object download shares only).
	Image bool `json:"image,omitempty"`
}

// PresignRequest asks for a raw presigned URL.
type PresignRequest struct {
	// Method is GET (default), PUT, HEAD or DELETE.
	Method             string            `json:"method,omitempty"`
	Bucket             string            `json:"bucket"`
	Key                string            `json:"key"`
	VersionID          string            `json:"versionId,omitempty"`
	ExpiresIn          int64             `json:"expiresIn,omitempty"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
}

// PresignedURL is a raw presigned URL.
type PresignedURL struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Headers   map[string]string `json:"headers"`
}

// PostPolicyRequest asks for a POST policy form.
type PostPolicyRequest struct {
	Bucket                string            `json:"bucket"`
	Key                   string            `json:"key,omitempty"`
	KeyPrefix             string            `json:"keyPrefix,omitempty"`
	ExpiresIn             int64             `json:"expiresIn,omitempty"`
	MinSize               int64             `json:"minSize,omitempty"`
	MaxSize               int64             `json:"maxSize,omitempty"`
	ContentType           string            `json:"contentType,omitempty"`
	ContentTypePrefix     string            `json:"contentTypePrefix,omitempty"`
	SuccessActionStatus   int               `json:"successActionStatus,omitempty"`
	SuccessActionRedirect string            `json:"successActionRedirect,omitempty"`
	Fields                map[string]string `json:"fields,omitempty"`
}

// PostCondition is a condition of a POST policy.
type PostCondition struct {
	Operator string `json:"operator"`
	Key      string `json:"key,omitempty"`
	Value    string `json:"value,omitempty"`
	Min      int64  `json:"min,omitempty"`
	Max      int64  `json:"max,omitempty"`
}

// PostPolicy is a signed POST policy form.
type PostPolicy struct {
	URL        string            `json:"url"`
	Fields     map[string]string `json:"fields"`
	ExpiresAt  time.Time         `json:"expiresAt"`
	Conditions []PostCondition   `json:"conditions"`
}

// User is an IAM user.
type User struct {
	AccessKey   string    `json:"accessKey"`
	Status      string    `json:"status"`
	PolicyNames []string  `json:"policyNames"`
	Groups      []string  `json:"groups"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// UserList is a page of users.
type UserList struct {
	Items      []User `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Credential statuses.
const (
	StatusEnabled  = "enabled"
	StatusDisabled = "disabled"
)

// CreateUserRequest creates a user.
type CreateUserRequest struct {
	AccessKey   string   `json:"accessKey"`
	SecretKey   string   `json:"secretKey"`
	PolicyNames []string `json:"policyNames,omitempty"`
	Groups      []string `json:"groups,omitempty"`
	Status      string   `json:"status,omitempty"`
}

// UpdateUserRequest changes a user; nil fields are left unchanged.
type UpdateUserRequest struct {
	SecretKey   *string   `json:"secretKey,omitempty"`
	Status      *string   `json:"status,omitempty"`
	PolicyNames *[]string `json:"policyNames,omitempty"`
	Groups      *[]string `json:"groups,omitempty"`
}

// Group is an IAM group.
type Group struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Members     []string  `json:"members"`
	PolicyNames []string  `json:"policyNames"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// GroupList is a page of groups.
type GroupList struct {
	Items      []Group `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

// CreateGroupRequest creates a group.
type CreateGroupRequest struct {
	Name        string   `json:"name"`
	Members     []string `json:"members,omitempty"`
	PolicyNames []string `json:"policyNames,omitempty"`
}

// UpdateGroupRequest changes a group; nil and empty fields are left
// unchanged.
type UpdateGroupRequest struct {
	Status        *string   `json:"status,omitempty"`
	PolicyNames   *[]string `json:"policyNames,omitempty"`
	AddMembers    []string  `json:"addMembers,omitempty"`
	RemoveMembers []string  `json:"removeMembers,omitempty"`
}

// AccessKey is a service account.
type AccessKey struct {
	AccessKey   string          `json:"accessKey"`
	ParentUser  string          `json:"parentUser"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Status      string          `json:"status"`
	Expiration  *time.Time      `json:"expiration,omitempty"`
	Policy      json.RawMessage `json:"policy,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

// CreatedAccessKey is an access key just created, with its secret (shown
// only once).
type CreatedAccessKey struct {
	AccessKey
	SecretKey string `json:"secretKey"`
}

// AccessKeyList is a page of access keys.
type AccessKeyList struct {
	Items      []AccessKey `json:"items"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// ListAccessKeysOptions select and page the access keys.
type ListAccessKeysOptions struct {
	ListOptions
	// User lists the keys of another user (the caller's own by default).
	User string
}

// CreateAccessKeyRequest creates an access key.
type CreateAccessKeyRequest struct {
	ParentUser  string          `json:"parentUser,omitempty"`
	AccessKey   string          `json:"accessKey,omitempty"`
	SecretKey   string          `json:"secretKey,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Expiration  *time.Time      `json:"expiration,omitempty"`
	Policy      json.RawMessage `json:"policy,omitempty"`
}

// UpdateAccessKeyRequest changes an access key; nil fields are left
// unchanged.
type UpdateAccessKeyRequest struct {
	SecretKey       *string         `json:"secretKey,omitempty"`
	Name            *string         `json:"name,omitempty"`
	Description     *string         `json:"description,omitempty"`
	Status          *string         `json:"status,omitempty"`
	Expiration      *time.Time      `json:"expiration,omitempty"`
	ClearExpiration bool            `json:"clearExpiration,omitempty"`
	Policy          json.RawMessage `json:"policy,omitempty"`
	ClearPolicy     bool            `json:"clearPolicy,omitempty"`
}

// Policy is a named IAM policy.
type Policy struct {
	Name      string          `json:"name"`
	Document  json.RawMessage `json:"document"`
	BuiltIn   bool            `json:"builtIn"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// PolicyList is a page of policies.
type PolicyList struct {
	Items      []Policy `json:"items"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// AttachRequest attaches or detaches policies; exactly one of User and
// Group is set.
type AttachRequest struct {
	PolicyNames []string `json:"policyNames"`
	User        string   `json:"user,omitempty"`
	Group       string   `json:"group,omitempty"`
}

// PolicyEntities are the users and groups of a policy.
type PolicyEntities struct {
	Policy string   `json:"policy"`
	Users  []string `json:"users"`
	Groups []string `json:"groups"`
}

// SimulateRequest describes a request to simulate.
type SimulateRequest struct {
	User            string              `json:"user,omitempty"`
	Action          string              `json:"action"`
	Bucket          string              `json:"bucket,omitempty"`
	Object          string              `json:"object,omitempty"`
	SourceIP        string              `json:"sourceIp,omitempty"`
	SecureTransport *bool               `json:"secureTransport,omitempty"`
	Time            *time.Time          `json:"time,omitempty"`
	Conditions      map[string][]string `json:"conditions,omitempty"`
}

// EvaluatedStatement is a statement considered by the simulator.
type EvaluatedStatement struct {
	Source     string          `json:"source"`
	PolicyName string          `json:"policyName,omitempty"`
	Group      string          `json:"group,omitempty"`
	Index      int             `json:"index"`
	Sid        string          `json:"sid,omitempty"`
	Effect     string          `json:"effect"`
	Matched    bool            `json:"matched"`
	Mismatch   string          `json:"mismatch,omitempty"`
	Statement  json.RawMessage `json:"statement,omitempty"`
}

// Simulation is the result of a simulation.
type Simulation struct {
	User       string               `json:"user"`
	Action     string               `json:"action"`
	Bucket     string               `json:"bucket,omitempty"`
	Object     string               `json:"object,omitempty"`
	Allowed    bool                 `json:"allowed"`
	Decision   string               `json:"decision"`
	Reason     string               `json:"reason"`
	Source     string               `json:"source,omitempty"`
	PolicyName string               `json:"policyName,omitempty"`
	Group      string               `json:"group,omitempty"`
	Statement  *EvaluatedStatement  `json:"statement,omitempty"`
	Evaluated  []EvaluatedStatement `json:"evaluated"`
}

// PolicyError locates an error of a policy document.
type PolicyError struct {
	Statement int    `json:"statement"`
	Sid       string `json:"sid,omitempty"`
	Element   string `json:"element,omitempty"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	Message   string `json:"message"`
}

// ValidationReport is the result of a validation.
type ValidationReport struct {
	Valid  bool          `json:"valid"`
	Errors []PolicyError `json:"errors"`
}

// ConfigSubsystem is the configuration of a subsystem (sensitive values
// masked).
type ConfigSubsystem struct {
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings"`
}

// ConfigKey describes a configuration key.
type ConfigKey struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Default     string   `json:"default,omitempty"`
	Values      []string `json:"values,omitempty"`
	Sensitive   bool     `json:"sensitive"`
	// Applied reports a key the server applies; the others are stored but
	// have no effect yet.
	Applied bool `json:"applied"`
}

// ConfigSubsystemSchema describes a subsystem.
type ConfigSubsystemSchema struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Keys        []ConfigKey `json:"keys"`
}

// ImportClusterOptions tune ImportCluster.
type ImportClusterOptions struct {
	// Mode is "merge" (default: what the archive does not hold is kept) or
	// "replace" (the configuration is replaced, the users, groups and named
	// policies the archive does not hold are removed).
	Mode string
	// DryRun validates the archive and reports what the import would do.
	DryRun bool
}

// ClusterImportResult is the outcome of ImportCluster.
type ClusterImportResult struct {
	Mode             string   `json:"mode"`
	DryRun           bool     `json:"dryRun"`
	Users            int      `json:"users"`
	Groups           int      `json:"groups"`
	Policies         int      `json:"policies"`
	ConfigSubsystems int      `json:"configSubsystems"`
	Buckets          int      `json:"buckets"`
	BucketsCreated   []string `json:"bucketsCreated"`
	Removed          []string `json:"removed"`
}

// PoolJob is the progress of a decommission or of the rebalance.
type PoolJob struct {
	// State is running, complete, canceled or failed.
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"startedAt"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Pass       int        `json:"pass"`
	// Pool is the source pool of a rebalance (1-based).
	Pool           int    `json:"pool,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	Object         string `json:"object,omitempty"`
	Objects        int64  `json:"objects"`
	Versions       int64  `json:"versions"`
	Bytes          int64  `json:"bytes"`
	Failed         int64  `json:"failed"`
	PendingUploads int    `json:"pendingUploads"`
	Error          string `json:"error,omitempty"`
}

// PoolStatus is a server pool and its decommission.
type PoolStatus struct {
	// Index is the 1-based position of the pool in the command line.
	Index        int      `json:"index"`
	ID           string   `json:"id"`
	Total        uint64   `json:"total"`
	Free         uint64   `json:"free"`
	Draining     bool     `json:"draining"`
	Decommission *PoolJob `json:"decommission,omitempty"`
}

// Pools is the answer of GET /pools.
type Pools struct {
	Pools     []PoolStatus `json:"pools"`
	Rebalance *PoolJob     `json:"rebalance,omitempty"`
}

// Lock is a namespace lock held across the lock servers.
type Lock struct {
	Resource string `json:"resource"`
	UID      string `json:"uid"`
	Owner    string `json:"owner"`
	Source   string `json:"source,omitempty"`
	// Type is "write" or "read".
	Type      string    `json:"type"`
	Quorum    int       `json:"quorum"`
	Since     time.Time `json:"since"`
	Refreshed time.Time `json:"refreshed"`
	Servers   int       `json:"servers"`
	Total     int       `json:"total"`
}
