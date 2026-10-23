package nexoadmin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// --- session and server -------------------------------------------------------

// MyPermissions implements AdminClient.
func (c *client) MyPermissions(ctx context.Context, q PermissionsQuery) (*Permissions, error) {
	query := url.Values{}
	if q.User != "" {
		query.Set("user", q.User)
	}
	if q.Bucket != "" {
		query.Set("bucket", q.Bucket)
	}
	if q.Prefix != "" {
		query.Set("prefix", q.Prefix)
	}
	var out Permissions
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/me/permissions", query: query}, &out)
}

// ServerInfo implements AdminClient.
func (c *client) ServerInfo(ctx context.Context) (*ServerInfo, error) {
	var out ServerInfo
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/info"}, &out)
}

// DataUsage implements AdminClient.
func (c *client) DataUsage(ctx context.Context) (*DataUsage, error) {
	var out DataUsage
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/usage"}, &out)
}

// Heal implements AdminClient.
func (c *client) Heal(ctx context.Context, in HealRequest) (*HealJob, error) {
	var out HealJob
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/heal", body: in}, &out)
}

// HealStatus implements AdminClient.
func (c *client) HealStatus(ctx context.Context, id string) (*HealJob, error) {
	var out HealJob
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("heal", id)}, &out)
}

// ListHeals implements AdminClient.
func (c *client) ListHeals(ctx context.Context) (*HealJobList, error) {
	var out HealJobList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/heal"}, &out)
}

// CompactMetadataLog implements AdminClient.
func (c *client) CompactMetadataLog(ctx context.Context) (*MetadataLogCompaction, error) {
	var out MetadataLogCompaction
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/storage/metadata-log/compact"}, &out)
}

// RestartService implements AdminClient.
func (c *client) RestartService(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/service/restart"}, nil)
}

// StopService implements AdminClient.
func (c *client) StopService(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/service/stop"}, nil)
}

// --- buckets -----------------------------------------------------------------

// ListBuckets implements AdminClient.
func (c *client) ListBuckets(ctx context.Context, opts ListBucketsOptions) (*BucketList, error) {
	q := listQuery(opts.ListOptions, nil)
	if opts.Prefix != "" {
		q.Set("prefix", opts.Prefix)
	}
	var out BucketList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/buckets", query: q}, &out)
}

// CreateBucket implements AdminClient.
func (c *client) CreateBucket(ctx context.Context, in CreateBucketRequest) (*Bucket, error) {
	var out Bucket
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/buckets", body: in}, &out)
}

// GetBucket implements AdminClient.
func (c *client) GetBucket(ctx context.Context, name string) (*Bucket, error) {
	var out Bucket
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", name)}, &out)
}

// DeleteBucket implements AdminClient.
func (c *client) DeleteBucket(ctx context.Context, name string, force bool) error {
	var q url.Values
	if force {
		q = url.Values{"force": {"true"}}
	}
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("buckets", name), query: q}, nil)
}

type versioningBody struct {
	Status string `json:"status"`
}

// GetBucketVersioning implements AdminClient.
func (c *client) GetBucketVersioning(ctx context.Context, name string) (string, error) {
	var out versioningBody
	err := c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", name, "versioning")}, &out)
	return out.Status, err
}

// SetBucketVersioning implements AdminClient.
func (c *client) SetBucketVersioning(ctx context.Context, name, status string) error {
	return c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", name, "versioning"), body: versioningBody{Status: status}}, nil)
}

// GetBucketPolicy implements AdminClient.
func (c *client) GetBucketPolicy(ctx context.Context, name string) (json.RawMessage, error) {
	data, _, err := c.send(ctx, request{method: http.MethodGet, path: pathOf("buckets", name, "policy")})
	return json.RawMessage(data), err
}

// SetBucketPolicy implements AdminClient.
func (c *client) SetBucketPolicy(ctx context.Context, name string, document []byte) error {
	return c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", name, "policy"), raw: document, contentType: "application/json"}, nil)
}

// DeleteBucketPolicy implements AdminClient.
func (c *client) DeleteBucketPolicy(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("buckets", name, "policy")}, nil)
}

// GetBucketQuota implements AdminClient.
func (c *client) GetBucketQuota(ctx context.Context, name string) (*BucketQuota, error) {
	var out BucketQuota
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", name, "quota")}, &out)
}

// SetBucketQuota implements AdminClient.
func (c *client) SetBucketQuota(ctx context.Context, name string, quota BucketQuota) (*BucketQuota, error) {
	var out BucketQuota
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", name, "quota"), body: quota}, &out)
}

// GetBucketEncryption implements AdminClient.
func (c *client) GetBucketEncryption(ctx context.Context, name string) (*BucketEncryption, error) {
	var out BucketEncryption
	err := c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", name, "encryption")}, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code != "bucket_not_found" {
		// No default encryption (S3 ServerSideEncryptionConfigurationNotFoundError).
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetBucketEncryption implements AdminClient.
func (c *client) SetBucketEncryption(ctx context.Context, name string, enc BucketEncryption) (*BucketEncryption, error) {
	var out BucketEncryption
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", name, "encryption"), body: enc}, &out)
}

// DeleteBucketEncryption implements AdminClient.
func (c *client) DeleteBucketEncryption(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("buckets", name, "encryption")}, nil)
}

type tagsBody struct {
	Tags map[string]string `json:"tags"`
}

// GetBucketTags implements AdminClient.
func (c *client) GetBucketTags(ctx context.Context, name string) (map[string]string, error) {
	var out tagsBody
	err := c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", name, "tags")}, &out)
	return out.Tags, err
}

// SetBucketTags implements AdminClient.
func (c *client) SetBucketTags(ctx context.Context, name string, tags map[string]string) error {
	if tags == nil {
		tags = map[string]string{}
	}
	return c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", name, "tags"), body: tagsBody{Tags: tags}}, nil)
}

// GetBucketAccess implements AdminClient.
func (c *client) GetBucketAccess(ctx context.Context, name string) (*BucketAccess, error) {
	var out BucketAccess
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", name, "access")}, &out)
}

// SetBucketAccess implements AdminClient.
func (c *client) SetBucketAccess(ctx context.Context, name, prefix, preset string) (*BucketAccess, error) {
	var out BucketAccess
	body := struct {
		Prefix string `json:"prefix"`
		Preset string `json:"preset"`
	}{prefix, preset}
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", name, "access"), body: body}, &out)
}

// --- objects -----------------------------------------------------------------

// ListObjects implements AdminClient.
func (c *client) ListObjects(ctx context.Context, bucket string, opts ListObjectsOptions) (*ObjectList, error) {
	q := listQuery(opts.ListOptions, nil)
	if opts.Prefix != "" {
		q.Set("prefix", opts.Prefix)
	}
	if opts.Recursive {
		q.Set("delimiter", "")
	}
	var out ObjectList
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "objects"), query: q}, &out)
}

// StatObject implements AdminClient.
func (c *client) StatObject(ctx context.Context, bucket, key, versionID string) (*ObjectDetails, error) {
	q := url.Values{"key": {key}}
	if versionID != "" {
		q.Set("versionId", versionID)
	}
	var out ObjectDetails
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "objects", "stat"), query: q}, &out)
}

// DeleteObjects implements AdminClient.
func (c *client) DeleteObjects(ctx context.Context, bucket string, in DeleteObjectsRequest) (*DeleteObjectsResult, error) {
	var out DeleteObjectsResult
	return &out, c.do(ctx, request{method: http.MethodDelete, path: pathOf("buckets", bucket, "objects"), body: in}, &out)
}

// --- shares --------------------------------------------------------------------

// CreateShare implements AdminClient.
func (c *client) CreateShare(ctx context.Context, in CreateShareRequest) (*Share, error) {
	var out Share
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/shares", body: in}, &out)
}

// ListShares implements AdminClient.
func (c *client) ListShares(ctx context.Context, opts ListSharesOptions) (*ShareList, error) {
	q := listQuery(opts.ListOptions, nil)
	if opts.Owner != "" {
		q.Set("owner", opts.Owner)
	}
	if opts.Bucket != "" {
		q.Set("bucket", opts.Bucket)
	}
	var out ShareList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/shares", query: q}, &out)
}

// GetShare implements AdminClient.
func (c *client) GetShare(ctx context.Context, id string) (*Share, error) {
	var out Share
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("shares", id)}, &out)
}

// RevokeShare implements AdminClient.
func (c *client) RevokeShare(ctx context.Context, id string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("shares", id)}, nil)
}

// RevokeAllShares implements AdminClient.
func (c *client) RevokeAllShares(ctx context.Context, owner string) (int, error) {
	var q url.Values
	if owner != "" {
		q = url.Values{"owner": {owner}}
	}
	var out struct {
		Revoked int `json:"revoked"`
	}
	err := c.do(ctx, request{method: http.MethodDelete, path: "/shares", query: q}, &out)
	return out.Revoked, err
}

// Presign implements AdminClient.
func (c *client) Presign(ctx context.Context, in PresignRequest) (*PresignedURL, error) {
	var out PresignedURL
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/presign", body: in}, &out)
}

// PostPolicy implements AdminClient.
func (c *client) PostPolicy(ctx context.Context, in PostPolicyRequest) (*PostPolicy, error) {
	var out PostPolicy
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/post-policy", body: in}, &out)
}

// --- users and groups ----------------------------------------------------------

// ListUsers implements AdminClient.
func (c *client) ListUsers(ctx context.Context, opts ListOptions) (*UserList, error) {
	var out UserList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/users", query: listQuery(opts, nil)}, &out)
}

// CreateUser implements AdminClient.
func (c *client) CreateUser(ctx context.Context, in CreateUserRequest) (*User, error) {
	var out User
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/users", body: in}, &out)
}

// GetUser implements AdminClient.
func (c *client) GetUser(ctx context.Context, accessKey string) (*User, error) {
	var out User
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("users", accessKey)}, &out)
}

// UpdateUser implements AdminClient.
func (c *client) UpdateUser(ctx context.Context, accessKey string, in UpdateUserRequest) (*User, error) {
	var out User
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("users", accessKey), body: in}, &out)
}

// DeleteUser implements AdminClient.
func (c *client) DeleteUser(ctx context.Context, accessKey string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("users", accessKey)}, nil)
}

// ListGroups implements AdminClient.
func (c *client) ListGroups(ctx context.Context, opts ListOptions) (*GroupList, error) {
	var out GroupList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/groups", query: listQuery(opts, nil)}, &out)
}

// CreateGroup implements AdminClient.
func (c *client) CreateGroup(ctx context.Context, in CreateGroupRequest) (*Group, error) {
	var out Group
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/groups", body: in}, &out)
}

// GetGroup implements AdminClient.
func (c *client) GetGroup(ctx context.Context, name string) (*Group, error) {
	var out Group
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("groups", name)}, &out)
}

// UpdateGroup implements AdminClient.
func (c *client) UpdateGroup(ctx context.Context, name string, in UpdateGroupRequest) (*Group, error) {
	var out Group
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("groups", name), body: in}, &out)
}

// DeleteGroup implements AdminClient.
func (c *client) DeleteGroup(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("groups", name)}, nil)
}

// --- access keys -----------------------------------------------------------------

// ListAccessKeys implements AdminClient.
func (c *client) ListAccessKeys(ctx context.Context, opts ListAccessKeysOptions) (*AccessKeyList, error) {
	q := listQuery(opts.ListOptions, nil)
	if opts.User != "" {
		q.Set("user", opts.User)
	}
	var out AccessKeyList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/access-keys", query: q}, &out)
}

// CreateAccessKey implements AdminClient.
func (c *client) CreateAccessKey(ctx context.Context, in CreateAccessKeyRequest) (*CreatedAccessKey, error) {
	var out CreatedAccessKey
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/access-keys", body: in}, &out)
}

// GetAccessKey implements AdminClient.
func (c *client) GetAccessKey(ctx context.Context, accessKey string) (*AccessKey, error) {
	var out AccessKey
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("access-keys", accessKey)}, &out)
}

// UpdateAccessKey implements AdminClient.
func (c *client) UpdateAccessKey(ctx context.Context, accessKey string, in UpdateAccessKeyRequest) (*AccessKey, error) {
	var out AccessKey
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("access-keys", accessKey), body: in}, &out)
}

// DeleteAccessKey implements AdminClient.
func (c *client) DeleteAccessKey(ctx context.Context, accessKey string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("access-keys", accessKey)}, nil)
}

// --- policies --------------------------------------------------------------------

// documentBody sends a policy document as its JSON text.
type documentBody struct {
	Name     string `json:"name,omitempty"`
	Document string `json:"document"`
	Bucket   string `json:"bucket,omitempty"`
}

// ListPolicies implements AdminClient.
func (c *client) ListPolicies(ctx context.Context, opts ListOptions) (*PolicyList, error) {
	var out PolicyList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/policies", query: listQuery(opts, nil)}, &out)
}

// CreatePolicy implements AdminClient.
func (c *client) CreatePolicy(ctx context.Context, name string, document []byte) (*Policy, error) {
	var out Policy
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/policies", body: documentBody{Name: name, Document: string(document)}}, &out)
}

// GetPolicy implements AdminClient.
func (c *client) GetPolicy(ctx context.Context, name string) (*Policy, error) {
	var out Policy
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("policies", name)}, &out)
}

// SavePolicy implements AdminClient.
func (c *client) SavePolicy(ctx context.Context, name string, document []byte) (*Policy, error) {
	var out Policy
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("policies", name), body: documentBody{Document: string(document)}}, &out)
}

// DeletePolicy implements AdminClient.
func (c *client) DeletePolicy(ctx context.Context, name string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("policies", name)}, nil)
}

// AttachPolicy implements AdminClient.
func (c *client) AttachPolicy(ctx context.Context, in AttachRequest) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/policies/attach", body: in}, nil)
}

// DetachPolicy implements AdminClient.
func (c *client) DetachPolicy(ctx context.Context, in AttachRequest) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/policies/detach", body: in}, nil)
}

// PolicyEntities implements AdminClient.
func (c *client) PolicyEntities(ctx context.Context, name string) (*PolicyEntities, error) {
	var out PolicyEntities
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("policies", name, "entities")}, &out)
}

// SimulatePolicy implements AdminClient.
func (c *client) SimulatePolicy(ctx context.Context, in SimulateRequest) (*Simulation, error) {
	var out Simulation
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/policies/simulate", body: in}, &out)
}

// ValidatePolicy implements AdminClient.
func (c *client) ValidatePolicy(ctx context.Context, document []byte, bucket string) (*ValidationReport, error) {
	var out ValidationReport
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/policies/validate", body: documentBody{Document: string(document), Bucket: bucket}}, &out)
}

// --- configuration ---------------------------------------------------------------

// ListConfig implements AdminClient.
func (c *client) ListConfig(ctx context.Context) ([]ConfigSubsystem, error) {
	var out []ConfigSubsystem
	return out, c.do(ctx, request{method: http.MethodGet, path: "/config"}, &out)
}

// ConfigSchema implements AdminClient.
func (c *client) ConfigSchema(ctx context.Context) ([]ConfigSubsystemSchema, error) {
	var out []ConfigSubsystemSchema
	return out, c.do(ctx, request{method: http.MethodGet, path: "/config/schema"}, &out)
}

// GetConfig implements AdminClient.
func (c *client) GetConfig(ctx context.Context, subsystem string) (*ConfigSubsystem, error) {
	var out ConfigSubsystem
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("config", subsystem)}, &out)
}

// SetConfig implements AdminClient.
func (c *client) SetConfig(ctx context.Context, subsystem string, settings map[string]string) (*ConfigSubsystem, error) {
	var out ConfigSubsystem
	body := struct {
		Settings map[string]string `json:"settings"`
	}{settings}
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("config", subsystem), body: body}, &out)
}

// MergeConfig implements AdminClient.
func (c *client) MergeConfig(ctx context.Context, all map[string]map[string]string) error {
	return c.do(ctx, request{method: http.MethodPut, path: "/config", body: all}, nil)
}

// DeleteConfig implements AdminClient.
func (c *client) DeleteConfig(ctx context.Context, subsystem string, keys ...string) error {
	var q url.Values
	if len(keys) > 0 {
		q = url.Values{"keys": {strings.Join(keys, ",")}}
	}
	return c.do(ctx, request{method: http.MethodDelete, path: pathOf("config", subsystem), query: q}, nil)
}

// ExportConfig implements AdminClient.
func (c *client) ExportConfig(ctx context.Context) ([]byte, error) {
	data, _, err := c.send(ctx, request{method: http.MethodGet, path: "/config/export"})
	return data, err
}

// ImportConfig implements AdminClient.
func (c *client) ImportConfig(ctx context.Context, data []byte) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/config/import", raw: data, contentType: "application/json"}, nil)
}

// ExportCluster implements AdminClient.
func (c *client) ExportCluster(ctx context.Context) ([]byte, error) {
	data, _, err := c.send(ctx, request{method: http.MethodGet, path: "/cluster/export"})
	return data, err
}

// ImportCluster implements AdminClient.
func (c *client) ImportCluster(ctx context.Context, archive []byte, opts ImportClusterOptions) (*ClusterImportResult, error) {
	q := url.Values{}
	if opts.Mode != "" {
		q.Set("mode", opts.Mode)
	}
	if opts.DryRun {
		q.Set("dryRun", "true")
	}
	var out ClusterImportResult
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/cluster/import", query: q, raw: archive, contentType: "application/zip"}, &out)
}

// ListLocks implements AdminClient.
func (c *client) ListLocks(ctx context.Context) ([]Lock, error) {
	var out []Lock
	return out, c.do(ctx, request{method: http.MethodGet, path: "/locks"}, &out)
}

// ListPools implements AdminClient.
func (c *client) ListPools(ctx context.Context) (*Pools, error) {
	var out Pools
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/pools"}, &out)
}

// DecommissionPool implements AdminClient.
func (c *client) DecommissionPool(ctx context.Context, pool int) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/pools/" + strconv.Itoa(pool) + "/decommission"}, nil)
}

// CancelDecommission implements AdminClient.
func (c *client) CancelDecommission(ctx context.Context, pool int) error {
	return c.do(ctx, request{method: http.MethodDelete, path: "/pools/" + strconv.Itoa(pool) + "/decommission"}, nil)
}

// StartRebalance implements AdminClient.
func (c *client) StartRebalance(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/rebalance"}, nil)
}

// StopRebalance implements AdminClient.
func (c *client) StopRebalance(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodDelete, path: "/rebalance"}, nil)
}

// PlanPools implements AdminClient.
func (c *client) PlanPools(ctx context.Context, in PoolPlanInput) (*PoolPlan, error) {
	var out PoolPlan
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/pools/plan", body: in}, &out)
}

// ForceUnlock implements AdminClient.
func (c *client) ForceUnlock(ctx context.Context, resources ...string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: "/locks", query: url.Values{"resource": resources}}, nil)
}
