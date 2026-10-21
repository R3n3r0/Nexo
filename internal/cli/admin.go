package cli

import (
	"context"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// AdminClient is the Admin API surface used by the nexoctl admin and share
// commands (docs/12 §12.5): a subset of nexoadmin.AdminClient (its live
// streams behind the EventStream interface, so that the tests replace
// them). The implementation of NewAdminFactory
// wraps every failure of the server in an *AdminError, which wraps the
// matching sentinel of domain (domain.ErrUserNotFound,
// domain.ErrAccessDenied, ...) so that the commands map them to stable
// exit codes (ExitCode).
type AdminClient interface {
	// Server.
	ServerInfo(ctx context.Context) (*nexoadmin.ServerInfo, error)
	DataUsage(ctx context.Context) (*nexoadmin.DataUsage, error)
	Heal(ctx context.Context, in nexoadmin.HealRequest) (*nexoadmin.HealJob, error)
	HealStatus(ctx context.Context, id string) (*nexoadmin.HealJob, error)
	ListHeals(ctx context.Context) (*nexoadmin.HealJobList, error)
	// CompactMetadataLog compacts the metadata log of the drives of the
	// node that serves the request.
	CompactMetadataLog(ctx context.Context) (*nexoadmin.MetadataLogCompaction, error)
	RestartService(ctx context.Context) error
	StopService(ctx context.Context) error
	// Diagnostics of the node that serves the request (S-OPS-11).
	Profile(ctx context.Context, in nexoadmin.ProfileRequest) (*nexoadmin.Profile, error)
	Speedtest(ctx context.Context, kind string, in nexoadmin.SpeedtestRequest) (*nexoadmin.SpeedtestResult, error)

	// Observability of the deployment, every node of a cluster included
	// (C-ADM-08, docs/07 §7.4): the live trace of the requests and the live
	// logs (streams read until closed or the server ends them), the search
	// of the audit log and the series of the metrics of the console.
	Trace(ctx context.Context, opts nexoadmin.TraceOptions) (EventStream[nexoadmin.TraceEvent], error)
	Logs(ctx context.Context, opts nexoadmin.LogOptions) (EventStream[nexoadmin.LogEntry], error)
	SearchAudit(ctx context.Context, q nexoadmin.AuditQuery) (*nexoadmin.AuditList, error)
	ListMetrics(ctx context.Context) (*nexoadmin.MetricList, error)
	QueryMetric(ctx context.Context, q nexoadmin.MetricQuery) (*nexoadmin.MetricSeries, error)

	// Users and groups.
	ListUsers(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.UserList, error)
	CreateUser(ctx context.Context, in nexoadmin.CreateUserRequest) (*nexoadmin.User, error)
	GetUser(ctx context.Context, accessKey string) (*nexoadmin.User, error)
	UpdateUser(ctx context.Context, accessKey string, in nexoadmin.UpdateUserRequest) (*nexoadmin.User, error)
	DeleteUser(ctx context.Context, accessKey string) error
	ListGroups(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.GroupList, error)
	CreateGroup(ctx context.Context, in nexoadmin.CreateGroupRequest) (*nexoadmin.Group, error)
	GetGroup(ctx context.Context, name string) (*nexoadmin.Group, error)
	UpdateGroup(ctx context.Context, name string, in nexoadmin.UpdateGroupRequest) (*nexoadmin.Group, error)
	DeleteGroup(ctx context.Context, name string) error

	// Access keys (service accounts).
	ListAccessKeys(ctx context.Context, opts nexoadmin.ListAccessKeysOptions) (*nexoadmin.AccessKeyList, error)
	CreateAccessKey(ctx context.Context, in nexoadmin.CreateAccessKeyRequest) (*nexoadmin.CreatedAccessKey, error)
	GetAccessKey(ctx context.Context, accessKey string) (*nexoadmin.AccessKey, error)
	UpdateAccessKey(ctx context.Context, accessKey string, in nexoadmin.UpdateAccessKeyRequest) (*nexoadmin.AccessKey, error)
	DeleteAccessKey(ctx context.Context, accessKey string) error

	// Policies: the documents travel as their JSON text, so that the
	// errors of a malformed document carry a line and a column.
	ListPolicies(ctx context.Context, opts nexoadmin.ListOptions) (*nexoadmin.PolicyList, error)
	CreatePolicy(ctx context.Context, name string, document []byte) (*nexoadmin.Policy, error)
	GetPolicy(ctx context.Context, name string) (*nexoadmin.Policy, error)
	SavePolicy(ctx context.Context, name string, document []byte) (*nexoadmin.Policy, error)
	DeletePolicy(ctx context.Context, name string) error
	AttachPolicy(ctx context.Context, in nexoadmin.AttachRequest) error
	DetachPolicy(ctx context.Context, in nexoadmin.AttachRequest) error
	PolicyEntities(ctx context.Context, name string) (*nexoadmin.PolicyEntities, error)
	SimulatePolicy(ctx context.Context, in nexoadmin.SimulateRequest) (*nexoadmin.Simulation, error)
	ValidatePolicy(ctx context.Context, document []byte, bucket string) (*nexoadmin.ValidationReport, error)

	// Configuration.
	ListConfig(ctx context.Context) ([]nexoadmin.ConfigSubsystem, error)
	ConfigSchema(ctx context.Context) ([]nexoadmin.ConfigSubsystemSchema, error)
	GetConfig(ctx context.Context, subsystem string) (*nexoadmin.ConfigSubsystem, error)
	SetConfig(ctx context.Context, subsystem string, settings map[string]string) (*nexoadmin.ConfigSubsystem, error)
	DeleteConfig(ctx context.Context, subsystem string, keys ...string) error
	ExportConfig(ctx context.Context) ([]byte, error)
	ImportConfig(ctx context.Context, data []byte) error

	// Cluster state (S-OPS-15, C-ADM-13): IAM, policies, configuration and
	// bucket settings in one zip archive.
	ExportCluster(ctx context.Context) ([]byte, error)
	ImportCluster(ctx context.Context, archive []byte, opts nexoadmin.ImportClusterOptions) (*nexoadmin.ClusterImportResult, error)

	// Namespace locks (docs/03 §3.6: admin lock list/clear).
	ListLocks(ctx context.Context) ([]nexoadmin.Lock, error)
	ForceUnlock(ctx context.Context, resources ...string) error

	// GetBucket returns the summary of a bucket: usage, settings, tags
	// (quota info reports the usage against the quota).
	GetBucket(ctx context.Context, bucket string) (*nexoadmin.Bucket, error)
	// Bucket settings of the Admin API (C-OBJ-09: quota, encrypt).
	GetBucketQuota(ctx context.Context, bucket string) (*nexoadmin.BucketQuota, error)
	SetBucketQuota(ctx context.Context, bucket string, quota nexoadmin.BucketQuota) (*nexoadmin.BucketQuota, error)
	// GetBucketEncryption returns nil when the bucket has no default
	// encryption.
	GetBucketEncryption(ctx context.Context, bucket string) (*nexoadmin.BucketEncryption, error)
	SetBucketEncryption(ctx context.Context, bucket string, enc nexoadmin.BucketEncryption) (*nexoadmin.BucketEncryption, error)
	DeleteBucketEncryption(ctx context.Context, bucket string) error
	// ApplyBucketLifecycle applies the lifecycle rules of a bucket at once
	// (ilm apply: s3:PutLifecycleConfiguration and admin:ApplyLifecycle).
	ApplyBucketLifecycle(ctx context.Context, bucket string) (*nexoadmin.LifecycleApplyResult, error)
	// Video rules of the buckets and the state of the media worker
	// (nexoctl media): GetBucketMedia returns no rule without
	// configuration, SetBucketMedia stores the rules normalized,
	// ReprocessMedia queues a job of the worker for a video and
	// GetMediaStatus returns its processing state (state "none" when the
	// worker never processed it); versionID "" is the latest version.
	GetBucketMedia(ctx context.Context, bucket string) (*nexoadmin.MediaConfiguration, error)
	SetBucketMedia(ctx context.Context, bucket string, cfg nexoadmin.MediaConfiguration) (*nexoadmin.MediaConfiguration, error)
	DeleteBucketMedia(ctx context.Context, bucket string) error
	ReprocessMedia(ctx context.Context, bucket, key, versionID string) error
	GetMediaStatus(ctx context.Context, bucket, key, versionID string) (*nexoadmin.MediaStatus, error)
	// Subtitle tracks of a video (nexoctl media subtitles): ListSubtitles
	// returns those that apply to its content, ExportSubtitles the
	// document of a track (format vtt or srt), ImportSubtitles stores a
	// WebVTT or SubRip document as a manual track, DeleteSubtitles removes
	// a track.
	ListSubtitles(ctx context.Context, bucket, key, versionID string) ([]nexoadmin.SubtitleTrack, error)
	ExportSubtitles(ctx context.Context, bucket, key, versionID, lang, format string) ([]byte, error)
	ImportSubtitles(ctx context.Context, bucket, key, versionID, lang string, doc []byte) (*nexoadmin.SubtitleTrack, error)
	DeleteSubtitles(ctx context.Context, bucket, key, versionID, lang string) error

	// Notification targets (admin notify targets ls|test): their delivery
	// state on the answering node and a test event; target is
	// "<id>:<type>" or the ARN of the target.
	ListNotifyTargets(ctx context.Context) ([]nexoadmin.NotifyTarget, error)
	TestNotifyTarget(ctx context.Context, target string) (*nexoadmin.NotifyTargetTest, error)

	// KMS of the server-side encryption (admin kms status|key ls|key
	// create|reseal).
	KMSStatus(ctx context.Context) (*nexoadmin.KMSStatus, error)
	ListKMSKeys(ctx context.Context, prefix string) ([]nexoadmin.KMSKey, error)
	CreateKMSKey(ctx context.Context, id string) (*nexoadmin.KMSKey, error)
	ResealKMSKeys(ctx context.Context, bucket string) (*nexoadmin.KMSResealResult, error)

	// Access rules of the buckets (docs/10 §10.1: nexoctl access). The
	// changes are a read-modify-write guarded by the ETag of the rules
	// (ifMatch): a concurrent change fails with
	// domain.ErrPreconditionFailed.
	GetAccessRules(ctx context.Context, bucket string) (*nexoadmin.AccessRules, error)
	SetAccessRules(ctx context.Context, bucket string, rules []nexoadmin.BucketAccessRule, ifMatch string) (*nexoadmin.AccessRules, error)
	TestAccessRules(ctx context.Context, bucket string, in nexoadmin.TestAccessRulesRequest) (*nexoadmin.AccessRulesTestResult, error)
	GetAccessRulesPublic(ctx context.Context, bucket string) (*nexoadmin.AccessRulesPublicStatus, error)
	ApplyAccessPreset(ctx context.Context, bucket string, in nexoadmin.AccessPresetRequest, ifMatch string) (*nexoadmin.AccessRules, error)
	// GetBlockPublic and SetBlockPublic: bucket "" is the cluster.
	GetBlockPublic(ctx context.Context, bucket string) (*nexoadmin.BlockPublic, error)
	SetBlockPublic(ctx context.Context, bucket string, in nexoadmin.BlockPublicRequest) (*nexoadmin.BlockPublic, error)

	// Distributions, key groups, routing and certificates (docs/10 §10.2:
	// nexoctl dist). UpdateDistribution replaces the whole document,
	// guarded by its ETag (ifMatch).
	ListDistributions(ctx context.Context) ([]nexoadmin.Distribution, error)
	CreateDistribution(ctx context.Context, d nexoadmin.Distribution) (*nexoadmin.Distribution, error)
	GetDistribution(ctx context.Context, id string) (*nexoadmin.Distribution, error)
	UpdateDistribution(ctx context.Context, d nexoadmin.Distribution, ifMatch string) (*nexoadmin.Distribution, error)
	DeleteDistribution(ctx context.Context, id string) error
	TestDistribution(ctx context.Context, id string, in nexoadmin.DistributionTestRequest) (*nexoadmin.DistributionTestResult, error)
	RotateOriginSecret(ctx context.Context, id string, in nexoadmin.RotateOriginSecretRequest) (*nexoadmin.RotatedOriginSecret, error)
	SignDistributionURL(ctx context.Context, id string, in nexoadmin.SignRequest) (*nexoadmin.SignedURL, error)
	ResolveHost(ctx context.Context, host string) (*nexoadmin.Route, error)
	ListKeyGroups(ctx context.Context) ([]nexoadmin.KeyGroup, error)
	CreateKeyGroup(ctx context.Context, in nexoadmin.CreateKeyGroupRequest) (*nexoadmin.CreatedKeyGroup, error)
	GetKeyGroup(ctx context.Context, id string) (*nexoadmin.KeyGroup, error)
	DeleteKeyGroup(ctx context.Context, id string) error
	AddKeyGroupKey(ctx context.Context, group string, in nexoadmin.AddKeyRequest) (*nexoadmin.AddedKey, error)
	DeleteKeyGroupKey(ctx context.Context, group, keyID string) error
	ListCertificates(ctx context.Context) (*nexoadmin.CertificateList, error)
	UploadCertificate(ctx context.Context, in nexoadmin.UploadCertificateRequest) (*nexoadmin.Certificate, error)
	DeleteCertificate(ctx context.Context, id string) error
	ListDNSProviders(ctx context.Context) ([]nexoadmin.DNSProvider, error)
	GetDNSProvider(ctx context.Context, name string) (*nexoadmin.DNSProvider, error)
	CreateDNSProvider(ctx context.Context, in nexoadmin.DNSProviderRequest) (*nexoadmin.DNSProvider, error)
	UpdateDNSProvider(ctx context.Context, name string, in nexoadmin.DNSProviderRequest) (*nexoadmin.DNSProvider, error)
	DeleteDNSProvider(ctx context.Context, name string) error
	TestDNSProvider(ctx context.Context, name, domain string) (*nexoadmin.DNSProviderTestResult, error)

	// Cache of the distributions (docs/11: nexoctl cache, dist cache, dist
	// invalidate). The settings are replaced whole, guarded by their ETag
	// (ifMatch); the invalidations of distribution "" are the global
	// purges, and GetCacheInvalidation with distribution "" finds one by
	// its id alone.
	ListCachePolicies(ctx context.Context) ([]nexoadmin.CachePolicy, error)
	GetCachePolicy(ctx context.Context, name string) (*nexoadmin.CachePolicy, error)
	CreateCachePolicy(ctx context.Context, p nexoadmin.CachePolicy) (*nexoadmin.CachePolicy, error)
	UpdateCachePolicy(ctx context.Context, p nexoadmin.CachePolicy, ifMatch string) (*nexoadmin.CachePolicy, error)
	DeleteCachePolicy(ctx context.Context, name string) error
	PreviewCachePolicy(ctx context.Context, p nexoadmin.CachePolicy) (*nexoadmin.CachePolicyPreview, error)
	GetCacheSettings(ctx context.Context) (*nexoadmin.CacheGlobalSettings, error)
	SetCacheSettings(ctx context.Context, s nexoadmin.CacheGlobalSettings, ifMatch string) (*nexoadmin.CacheGlobalSettings, error)
	GetDistributionCache(ctx context.Context, id string) (*nexoadmin.DistributionCache, error)
	SetDistributionCache(ctx context.Context, cfg nexoadmin.DistributionCache, ifMatch string) (*nexoadmin.DistributionCache, error)
	EffectiveCachePolicy(ctx context.Context, id, host, path string) (*nexoadmin.CacheEffectivePolicy, error)
	InvalidateCache(ctx context.Context, distribution string, in nexoadmin.CacheInvalidationRequest) (*nexoadmin.CacheInvalidation, error)
	GetCacheInvalidation(ctx context.Context, distribution, id string) (*nexoadmin.CacheInvalidation, error)
	ListCacheInvalidations(ctx context.Context, distribution string) ([]nexoadmin.CacheInvalidation, error)
	WarmCache(ctx context.Context, distribution string, in nexoadmin.CacheWarmRequest) (*nexoadmin.CacheWarmJob, error)
	GetCacheWarmJob(ctx context.Context, distribution, id string) (*nexoadmin.CacheWarmJob, error)
	InspectCache(ctx context.Context, in nexoadmin.CacheInspectRequest) (*nexoadmin.CacheInspection, error)
	CacheStats(ctx context.Context, q nexoadmin.CacheStatsQuery) (*nexoadmin.CacheStats, error)
	ListCacheWebhooks(ctx context.Context, distribution string) ([]nexoadmin.CacheWebhook, error)
	AddCacheWebhook(ctx context.Context, distribution string, w nexoadmin.CacheWebhook) (*nexoadmin.CacheWebhook, error)
	DeleteCacheWebhook(ctx context.Context, distribution, id string) error
	TestCacheWebhook(ctx context.Context, distribution, id string) (*nexoadmin.CacheWebhookTestResult, error)

	// Managed share links (docs/09 §9.1, S-OBJ-08c).
	CreateShare(ctx context.Context, in nexoadmin.CreateShareRequest) (*nexoadmin.Share, error)
	ListShares(ctx context.Context, opts nexoadmin.ListSharesOptions) (*nexoadmin.ShareList, error)
	GetShare(ctx context.Context, id string) (*nexoadmin.Share, error)
	RevokeShare(ctx context.Context, id string) error
	RevokeAllShares(ctx context.Context, owner string) (int, error)
}

// EventStream is a live stream of the Admin API (admin trace, admin logs).
// Next waits for the next item and returns io.EOF when the server ended the
// stream; Dropped counts the items the server dropped because the client
// did not keep up; Close ends the stream (Next then fails). A stream is not
// safe for concurrent calls of Next.
type EventStream[T any] interface {
	Next() (*T, error)
	// Dropped is a pure accessor.
	Dropped() uint64
	Close() error
}

// AdminFactory builds the Admin API client of an alias: the commands
// receive it in their dependencies so that the tests can substitute the
// client.
type AdminFactory interface {
	Client(ctx context.Context, a Alias, opts ClientOptions) (AdminClient, error)
}
