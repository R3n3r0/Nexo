// Package nexoadmin is the Go client of the Nexo Admin API (/api/v1,
// documented by api/openapi.yaml). Requests are signed with AWS Signature
// Version 4 (service "s3", the region of the server), like the S3 API, so
// the same access keys work for both; the console session flow of the web
// UI is not used.
//
//	c, err := nexoadmin.New(nexoadmin.Config{
//		Endpoint:  "http://localhost:9001",
//		AccessKey: "admin",
//		SecretKey: "password",
//	})
//	info, err := c.ServerInfo(ctx)
//
// Failures of the server are *APIError values carrying the HTTP status and
// the stable problem code (access_denied, user_not_found...).
package nexoadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AdminClient is the Admin API. Every method fails with an *APIError when
// the server answers with an error.
type AdminClient interface {
	// Session and permissions.
	MyPermissions(ctx context.Context, q PermissionsQuery) (*Permissions, error)

	// Server.
	ServerInfo(ctx context.Context) (*ServerInfo, error)
	DataUsage(ctx context.Context) (*DataUsage, error)
	// Heal starts a healing job; HealStatus and ListHeals follow it.
	Heal(ctx context.Context, in HealRequest) (*HealJob, error)
	HealStatus(ctx context.Context, id string) (*HealJob, error)
	ListHeals(ctx context.Context) (*HealJobList, error)
	RestartService(ctx context.Context) error
	StopService(ctx context.Context) error
	// Profile collects a runtime profile of the node that serves the
	// request; Speedtest runs a speed test (SpeedtestDrive,
	// SpeedtestObject, SpeedtestNet) on it.
	Profile(ctx context.Context, in ProfileRequest) (*Profile, error)
	Speedtest(ctx context.Context, kind string, in SpeedtestRequest) (*SpeedtestResult, error)

	// Monitoring. Trace and Logs open live streams (read them with Next
	// until io.EOF, Close them when done): the requests served and the
	// server logs of every node of the deployment. SearchAudit searches
	// the recent audit records; ListMetrics and QueryMetric read the
	// series kept by the nodes for the charts of the console.
	Trace(ctx context.Context, opts TraceOptions) (*Stream[TraceEvent], error)
	Logs(ctx context.Context, opts LogOptions) (*Stream[LogEntry], error)
	SearchAudit(ctx context.Context, q AuditQuery) (*AuditList, error)
	ListMetrics(ctx context.Context) (*MetricList, error)
	QueryMetric(ctx context.Context, q MetricQuery) (*MetricSeries, error)

	// Buckets.
	ListBuckets(ctx context.Context, opts ListBucketsOptions) (*BucketList, error)
	CreateBucket(ctx context.Context, in CreateBucketRequest) (*Bucket, error)
	GetBucket(ctx context.Context, name string) (*Bucket, error)
	DeleteBucket(ctx context.Context, name string, force bool) error
	GetBucketVersioning(ctx context.Context, name string) (string, error)
	SetBucketVersioning(ctx context.Context, name, status string) error
	GetBucketPolicy(ctx context.Context, name string) (json.RawMessage, error)
	SetBucketPolicy(ctx context.Context, name string, document []byte) error
	DeleteBucketPolicy(ctx context.Context, name string) error
	GetBucketQuota(ctx context.Context, name string) (*BucketQuota, error)
	SetBucketQuota(ctx context.Context, name string, quota BucketQuota) (*BucketQuota, error)
	// GetBucketEncryption returns the default encryption of a bucket, nil
	// when it has none.
	GetBucketEncryption(ctx context.Context, name string) (*BucketEncryption, error)
	SetBucketEncryption(ctx context.Context, name string, enc BucketEncryption) (*BucketEncryption, error)
	DeleteBucketEncryption(ctx context.Context, name string) error
	GetBucketTags(ctx context.Context, name string) (map[string]string, error)
	SetBucketTags(ctx context.Context, name string, tags map[string]string) error
	GetBucketAccess(ctx context.Context, name string) (*BucketAccess, error)
	// SetBucketAccess applies an anonymous preset (private, download,
	// upload, public) to a prefix ("" for the whole bucket).
	SetBucketAccess(ctx context.Context, name, prefix, preset string) (*BucketAccess, error)

	// Bucket configurations in structured form, stored as the S3
	// documents of the S3 API. A rejected configuration is an *APIError
	// with Code invalid_configuration listing every invalid rule in
	// RuleErrors; no rule removes the configuration.
	GetBucketLifecycle(ctx context.Context, bucket string) (*LifecycleConfiguration, error)
	SetBucketLifecycle(ctx context.Context, bucket string, cfg LifecycleConfiguration) (*LifecycleConfiguration, error)
	DeleteBucketLifecycle(ctx context.Context, bucket string) error
	// ApplyBucketLifecycle applies the lifecycle rules of a bucket at once
	// (s3:PutLifecycleConfiguration and admin:ApplyLifecycle).
	ApplyBucketLifecycle(ctx context.Context, bucket string) (*LifecycleApplyResult, error)
	GetBucketEvents(ctx context.Context, bucket string) (*BucketNotifications, error)
	SetBucketEvents(ctx context.Context, bucket string, cfg BucketNotifications) (*BucketNotifications, error)
	DeleteBucketEvents(ctx context.Context, bucket string) error
	GetBucketCORS(ctx context.Context, bucket string) (*CORSConfiguration, error)
	SetBucketCORS(ctx context.Context, bucket string, cfg CORSConfiguration) (*CORSConfiguration, error)
	DeleteBucketCORS(ctx context.Context, bucket string) error

	// Notification targets (admin:ConfigUpdate): their delivery state on
	// the answering node and a test event. target is "<id>:<type>" or the
	// ARN of the target.
	ListNotifyTargets(ctx context.Context) ([]NotifyTarget, error)
	TestNotifyTarget(ctx context.Context, target string) (*NotifyTargetTest, error)

	// Objects (the S3 API is the way to transfer data; these are the
	// management operations of the console).
	ListObjects(ctx context.Context, bucket string, opts ListObjectsOptions) (*ObjectList, error)
	StatObject(ctx context.Context, bucket, key, versionID string) (*ObjectDetails, error)
	DeleteObjects(ctx context.Context, bucket string, in DeleteObjectsRequest) (*DeleteObjectsResult, error)
	// Object Lock of a version ("" is the latest) and its attributes.
	GetObjectRetention(ctx context.Context, bucket, key, versionID string) (*ObjectRetention, error)
	SetObjectRetention(ctx context.Context, bucket, key, versionID string, in SetObjectRetentionRequest) (*ObjectRetention, error)
	GetObjectLegalHold(ctx context.Context, bucket, key, versionID string) (*ObjectLegalHold, error)
	// SetObjectLegalHold sets the legal hold (ON or OFF).
	SetObjectLegalHold(ctx context.Context, bucket, key, versionID, status string) (*ObjectLegalHold, error)
	GetObjectAttributes(ctx context.Context, bucket, key string, opts ObjectAttributesOptions) (*ObjectAttributes, error)

	// Share links, presigned URLs and POST policies.
	CreateShare(ctx context.Context, in CreateShareRequest) (*Share, error)
	ListShares(ctx context.Context, opts ListSharesOptions) (*ShareList, error)
	GetShare(ctx context.Context, id string) (*Share, error)
	RevokeShare(ctx context.Context, id string) error
	// RevokeAllShares revokes every share of owner ("" is the caller) and
	// returns how many were revoked.
	RevokeAllShares(ctx context.Context, owner string) (int, error)
	Presign(ctx context.Context, in PresignRequest) (*PresignedURL, error)
	PostPolicy(ctx context.Context, in PostPolicyRequest) (*PostPolicy, error)

	// Users and groups.
	ListUsers(ctx context.Context, opts ListOptions) (*UserList, error)
	CreateUser(ctx context.Context, in CreateUserRequest) (*User, error)
	GetUser(ctx context.Context, accessKey string) (*User, error)
	UpdateUser(ctx context.Context, accessKey string, in UpdateUserRequest) (*User, error)
	DeleteUser(ctx context.Context, accessKey string) error
	ListGroups(ctx context.Context, opts ListOptions) (*GroupList, error)
	CreateGroup(ctx context.Context, in CreateGroupRequest) (*Group, error)
	GetGroup(ctx context.Context, name string) (*Group, error)
	UpdateGroup(ctx context.Context, name string, in UpdateGroupRequest) (*Group, error)
	DeleteGroup(ctx context.Context, name string) error

	// Access keys (service accounts).
	ListAccessKeys(ctx context.Context, opts ListAccessKeysOptions) (*AccessKeyList, error)
	CreateAccessKey(ctx context.Context, in CreateAccessKeyRequest) (*CreatedAccessKey, error)
	GetAccessKey(ctx context.Context, accessKey string) (*AccessKey, error)
	UpdateAccessKey(ctx context.Context, accessKey string, in UpdateAccessKeyRequest) (*AccessKey, error)
	DeleteAccessKey(ctx context.Context, accessKey string) error

	// Policies. The documents are sent as their JSON text, so that errors
	// are located by line and column in that text (APIError.Errors).
	ListPolicies(ctx context.Context, opts ListOptions) (*PolicyList, error)
	CreatePolicy(ctx context.Context, name string, document []byte) (*Policy, error)
	GetPolicy(ctx context.Context, name string) (*Policy, error)
	SavePolicy(ctx context.Context, name string, document []byte) (*Policy, error)
	DeletePolicy(ctx context.Context, name string) error
	AttachPolicy(ctx context.Context, in AttachRequest) error
	DetachPolicy(ctx context.Context, in AttachRequest) error
	PolicyEntities(ctx context.Context, name string) (*PolicyEntities, error)
	SimulatePolicy(ctx context.Context, in SimulateRequest) (*Simulation, error)
	// ValidatePolicy validates an IAM policy, or a bucket policy of bucket
	// when set, without storing it.
	ValidatePolicy(ctx context.Context, document []byte, bucket string) (*ValidationReport, error)

	// Configuration.
	ListConfig(ctx context.Context) ([]ConfigSubsystem, error)
	ConfigSchema(ctx context.Context) ([]ConfigSubsystemSchema, error)
	GetConfig(ctx context.Context, subsystem string) (*ConfigSubsystem, error)
	SetConfig(ctx context.Context, subsystem string, settings map[string]string) (*ConfigSubsystem, error)
	MergeConfig(ctx context.Context, all map[string]map[string]string) error
	// DeleteConfig removes a subsystem, or only the given keys.
	DeleteConfig(ctx context.Context, subsystem string, keys ...string) error
	ExportConfig(ctx context.Context) ([]byte, error)
	ImportConfig(ctx context.Context, data []byte) error

	// Cluster state (admin cluster export/import): IAM, policies,
	// configuration and bucket settings in one zip archive.
	ExportCluster(ctx context.Context) ([]byte, error)
	ImportCluster(ctx context.Context, archive []byte, opts ImportClusterOptions) (*ClusterImportResult, error)

	// Namespace locks (admin lock list/clear).
	ListLocks(ctx context.Context) ([]Lock, error)
	ForceUnlock(ctx context.Context, resources ...string) error

	// KMS of the server-side encryption (admin:KMSKeyStatus,
	// admin:KMSCreateKey): without KMS configured the server answers 501.
	KMSStatus(ctx context.Context) (*KMSStatus, error)
	// ListKMSKeys lists the keys whose name starts with prefix ("" for
	// all).
	ListKMSKeys(ctx context.Context, prefix string) ([]KMSKey, error)
	// CreateKMSKey creates a key usable as SSE-KMS key id and as default
	// key of a bucket encryption; an existing key is a conflict.
	CreateKMSKey(ctx context.Context, id string) (*KMSKey, error)
	// ResealKMSKeys re-seals the data keys of the SSE-S3 and SSE-KMS objects
	// of bucket (every bucket when empty) under the current master key of
	// the KMS, after a rotation (admin:KMSCreateKey).
	ResealKMSKeys(ctx context.Context, bucket string) (*KMSResealResult, error)

	// Access rules of the buckets (admin:ManageAccessRules, docs/10
	// §10.1). SetAccessRules replaces every rule atomically; a non-empty
	// ifMatch is the ETag the current rules must have (IsPreconditionFailed
	// otherwise). An invalid rule is an *APIError with Code
	// invalid_access_rule locating it in AccessRuleErrors.
	GetAccessRules(ctx context.Context, bucket string) (*AccessRules, error)
	SetAccessRules(ctx context.Context, bucket string, rules []BucketAccessRule, ifMatch string) (*AccessRules, error)
	// TestAccessRules evaluates requests on keys with every source of the
	// policy engine (admin:SimulatePolicy too, for another identity).
	TestAccessRules(ctx context.Context, bucket string, in TestAccessRulesRequest) (*AccessRulesTestResult, error)
	GetAccessRulesPublic(ctx context.Context, bucket string) (*AccessRulesPublicStatus, error)
	ApplyAccessPreset(ctx context.Context, bucket string, in AccessPresetRequest, ifMatch string) (*AccessRules, error)
	// GetBlockPublic and SetBlockPublic read and set the block public
	// access of a bucket, or of the cluster when bucket is "".
	GetBlockPublic(ctx context.Context, bucket string) (*BlockPublic, error)
	SetBlockPublic(ctx context.Context, bucket string, in BlockPublicRequest) (*BlockPublic, error)

	// Distributions, their key groups and the host routing
	// (admin:ManageDistributions; admin:SignDistributionURL for the
	// signatures, docs/10 §10.2). UpdateDistribution replaces the whole
	// document; a non-empty ifMatch is the ETag it must have.
	ListDistributions(ctx context.Context) ([]Distribution, error)
	CreateDistribution(ctx context.Context, d Distribution) (*Distribution, error)
	GetDistribution(ctx context.Context, id string) (*Distribution, error)
	UpdateDistribution(ctx context.Context, d Distribution, ifMatch string) (*Distribution, error)
	DeleteDistribution(ctx context.Context, id string) error
	TestDistribution(ctx context.Context, id string, in DistributionTestRequest) (*DistributionTestResult, error)
	RotateOriginSecret(ctx context.Context, id string, in RotateOriginSecretRequest) (*RotatedOriginSecret, error)
	// SignDistributionURL signs a URL or cookies with a private key of the
	// key group of the signed behavior (never stored by the server).
	SignDistributionURL(ctx context.Context, id string, in SignRequest) (*SignedURL, error)
	ResolveHost(ctx context.Context, host string) (*Route, error)
	ListKeyGroups(ctx context.Context) ([]KeyGroup, error)
	// CreateKeyGroup creates a key group; with GenerateKey the private key
	// of its first key pair is returned once.
	CreateKeyGroup(ctx context.Context, in CreateKeyGroupRequest) (*CreatedKeyGroup, error)
	GetKeyGroup(ctx context.Context, id string) (*KeyGroup, error)
	DeleteKeyGroup(ctx context.Context, id string) error
	AddKeyGroupKey(ctx context.Context, group string, in AddKeyRequest) (*AddedKey, error)
	DeleteKeyGroupKey(ctx context.Context, group, keyID string) error

	// Certificates of the domains (admin:ManageCertificates).
	ListCertificates(ctx context.Context) (*CertificateList, error)
	UploadCertificate(ctx context.Context, in UploadCertificateRequest) (*Certificate, error)
	GetCertificate(ctx context.Context, id string) (*Certificate, error)
	DeleteCertificate(ctx context.Context, id string) error

	// Cache of the distributions (docs/11). The configuration
	// (admin:ManageCache): reusable policies, settings of the cluster and
	// of a distribution (replaced whole; a non-empty ifMatch is the ETag
	// they must have), the effective policy of a path of a domain, the
	// purge webhooks, the inspector and the statistics.
	ListCachePolicies(ctx context.Context) ([]CachePolicy, error)
	GetCachePolicy(ctx context.Context, name string) (*CachePolicy, error)
	CreateCachePolicy(ctx context.Context, p CachePolicy) (*CachePolicy, error)
	UpdateCachePolicy(ctx context.Context, p CachePolicy, ifMatch string) (*CachePolicy, error)
	DeleteCachePolicy(ctx context.Context, name string) error
	// PreviewCachePolicy validates a policy and computes the Cache-Control
	// it sends, without storing it.
	PreviewCachePolicy(ctx context.Context, p CachePolicy) (*CachePolicyPreview, error)
	GetCacheSettings(ctx context.Context) (*CacheGlobalSettings, error)
	SetCacheSettings(ctx context.Context, s CacheGlobalSettings, ifMatch string) (*CacheGlobalSettings, error)
	GetDistributionCache(ctx context.Context, id string) (*DistributionCache, error)
	SetDistributionCache(ctx context.Context, cfg DistributionCache, ifMatch string) (*DistributionCache, error)
	// EffectiveCachePolicy returns the policy of a path ("" for /) of a
	// domain ("" for the level of the distribution).
	EffectiveCachePolicy(ctx context.Context, id, host, path string) (*CacheEffectivePolicy, error)
	InspectCache(ctx context.Context, in CacheInspectRequest) (*CacheInspection, error)
	CacheStats(ctx context.Context, q CacheStatsQuery) (*CacheStats, error)
	ListCacheWebhooks(ctx context.Context, distribution string) ([]CacheWebhook, error)
	AddCacheWebhook(ctx context.Context, distribution string, w CacheWebhook) (*CacheWebhook, error)
	DeleteCacheWebhook(ctx context.Context, distribution, id string) error
	TestCacheWebhook(ctx context.Context, distribution, id string) (*CacheWebhookTestResult, error)
	// The operations on the cached content (admin:InvalidateCache):
	// invalidations of a distribution, or global purges when distribution
	// is "" (All only); GetCacheInvalidation with distribution "" finds an
	// invalidation by its id alone. The warm-ups of a domain.
	InvalidateCache(ctx context.Context, distribution string, in CacheInvalidationRequest) (*CacheInvalidation, error)
	GetCacheInvalidation(ctx context.Context, distribution, id string) (*CacheInvalidation, error)
	ListCacheInvalidations(ctx context.Context, distribution string) ([]CacheInvalidation, error)
	WarmCache(ctx context.Context, distribution string, in CacheWarmRequest) (*CacheWarmJob, error)
	GetCacheWarmJob(ctx context.Context, distribution, id string) (*CacheWarmJob, error)
}

// DefaultRegion is the signing region when Config.Region is empty.
const DefaultRegion = "us-east-1"

// Config configures a client.
type Config struct {
	// Endpoint is the base URL of the console listener
	// (http(s)://host:port); "/api/v1" is appended.
	Endpoint string
	// AccessKey and SecretKey sign the requests; SessionToken is set for
	// temporary (STS) credentials.
	AccessKey    string
	SecretKey    string
	SessionToken string
	// Region is the region of the server (default "us-east-1").
	Region string
	// HTTPClient sends the requests (default: a client with a 60 s
	// timeout).
	HTTPClient *http.Client
	// UserAgent is sent with every request.
	UserAgent string
	// Now is the signing clock (default time.Now).
	Now func() time.Time
}

type client struct {
	base   *url.URL
	cfg    Config
	signer *signer
}

var _ AdminClient = (*client)(nil)

// New returns a client. The endpoint must be an absolute http(s) URL and
// the credentials are required.
func New(cfg Config) (AdminClient, error) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(cfg.Endpoint), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("nexoadmin: endpoint %q must be http(s)://host[:port]", cfg.Endpoint)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("nexoadmin: access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "nexoadmin-go"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + apiPrefix
	return &client{base: u, cfg: cfg, signer: newSigner(cfg)}, nil
}

// apiPrefix is the base path of the Admin API.
const apiPrefix = "/api/v1"
