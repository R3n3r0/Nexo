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
}

// PoolInfo is an erasure pool.
type PoolInfo struct {
	Index        int `json:"index"`
	Sets         int `json:"sets"`
	DrivesPerSet int `json:"drivesPerSet"`
	Parity       int `json:"parity"`
}

// NodeInfo is a node of the deployment.
type NodeInfo struct {
	Endpoint      string     `json:"endpoint"`
	State         string     `json:"state"`
	Version       string     `json:"version"`
	Commit        string     `json:"commit"`
	UptimeSeconds int64      `json:"uptimeSeconds"`
	Disks         []DiskInfo `json:"disks"`
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
	Error    string `json:"error,omitempty"`
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
}

// HealRequest starts a healing operation.
type HealRequest struct {
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	DryRun    bool   `json:"dryRun,omitempty"`
}

// HealResult is the state of a healing operation.
type HealResult struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"startedAt"`
	Finished  bool      `json:"finished"`
	Scanned   uint64    `json:"scanned"`
	Healed    uint64    `json:"healed"`
	Failed    uint64    `json:"failed"`
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
	ID                 string    `json:"id"`
	Owner              string    `json:"owner"`
	Bucket             string    `json:"bucket"`
	Key                string    `json:"key"`
	Prefix             bool      `json:"prefix"`
	Permission         string    `json:"permission"`
	VersionID          string    `json:"versionId,omitempty"`
	ContentType        string    `json:"contentType,omitempty"`
	ContentDisposition string    `json:"contentDisposition,omitempty"`
	ExpiresAt          time.Time `json:"expiresAt"`
	CreatedAt          time.Time `json:"createdAt"`
	URL                string    `json:"url,omitempty"`
	AccessKey          string    `json:"accessKey"`
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
