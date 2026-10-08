// Package admin is the facade used by the Admin API and the CLI for
// server-wide operations (information, data usage, healing, service
// control) and the home of the business rules of the console that span
// several domain services:
//
//   - Authorizer: the authorization service of the Admin API. It evaluates
//     the admin:* and s3:* actions of a request with the policy engine,
//     applies the self-service rule (a user always manages its own access
//     keys and share links, unless explicitly denied) and computes the
//     effective permissions shown by the permission-aware UI;
//   - BucketManager: bucket listing with the public flag, creation with its
//     initial settings, safe forced deletion and the anonymous access
//     presets per prefix;
//   - ObjectBrowser: the object browser operations (stat, versions,
//     folders, recursive delete, rename, download, preview and zip
//     archives), authorized object by object;
//   - ShareManager: managed share links, raw presigned URLs and POST policy
//     forms, signed on behalf of the caller without exposing its secret;
//   - IdentityManager and PolicyManager: the user, group and policy
//     operations of the console that combine several IAM calls, the policy
//     validator (with positions) and the simulator;
//   - ConfigManager: the dynamic configuration with its schema of known
//     subsystems, validated before it reaches config.Service.
package admin

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// Service exposes server administration.
type Service interface {
	// ServerInfo returns the server/cluster information.
	ServerInfo(ctx context.Context) (*domain.ServerInfo, error)
	// DataUsage returns the data usage per bucket.
	DataUsage(ctx context.Context) (*DataUsage, error)
	// Heal starts (or queries) a healing operation.
	Heal(ctx context.Context, in HealInput) (*HealResult, error)
	// ServiceAction restarts or stops the server through the injected
	// ActionHandler (domain.ErrNotImplemented without one).
	ServiceAction(ctx context.Context, action Action) error
}

// DataUsage is the data usage summary.
type DataUsage struct {
	LastUpdate    time.Time              `json:"lastUpdate"`
	ObjectsCount  uint64                 `json:"objectsCount"`
	VersionsCount uint64                 `json:"versionsCount"`
	TotalSize     uint64                 `json:"totalSize"`
	BucketsCount  uint64                 `json:"bucketsCount"`
	Buckets       map[string]BucketUsage `json:"buckets"`
}

// BucketUsage is the data usage of one bucket.
type BucketUsage struct {
	Size          uint64 `json:"size"`
	ObjectsCount  uint64 `json:"objectsCount"`
	VersionsCount uint64 `json:"versionsCount"`
	DeleteMarkers uint64 `json:"deleteMarkers"`
}

// HealInput are the parameters of Heal.
type HealInput struct {
	Bucket    string
	Prefix    string
	Recursive bool
	DryRun    bool
	// ID queries an existing healing operation instead of starting one.
	ID string
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

// Action is a service control action.
type Action string

// Service actions.
const (
	ActionRestart Action = "restart"
	ActionStop    Action = "stop"
)

// ActionHandler performs service actions; app provides it (a signal to
// the running process), the tests a mock.
type ActionHandler interface {
	Handle(ctx context.Context, action Action) error
}

// ActionHandlerFunc adapts a function to ActionHandler.
type ActionHandlerFunc func(ctx context.Context, action Action) error

// Handle implements ActionHandler.
func (f ActionHandlerFunc) Handle(ctx context.Context, action Action) error { return f(ctx, action) }

// --- authorization -------------------------------------------------------

// Requester is the authenticated caller of an Admin API request: its
// identity and the condition values of the request (aws:SourceIp,
// aws:SecureTransport, aws:UserAgent...) the policy engine evaluates with
// every action the request needs.
type Requester struct {
	Identity   domain.Identity
	Conditions map[string][]string
}

// Authorizer is the authorization service of the Admin API. Every route
// declares the actions it needs and the transport asks Authorize before
// calling the handler; the services that act on resources named in the
// request body (bulk deletes, renames, zip archives, share links...)
// authorize each resource with it too.
type Authorizer interface {
	// Authorize decides whether the requester may perform every action of
	// in.Actions on the bucket/object of in (none: the whole deployment).
	// The first denied action decides. With SelfService an implicit deny
	// is overridden (the service owning the resource enforces ownership:
	// own access keys, own share links); an explicit Deny never is. An
	// empty action list is domain.ErrInvalidArgument.
	Authorize(ctx context.Context, in AuthorizeInput) (Decision, error)
	// Permissions evaluates the catalog of console actions (admin:* and
	// s3:*) for the requester, or for in.User when set, on the bucket and
	// prefix of in (object actions are evaluated on "<prefix>*").
	Permissions(ctx context.Context, in PermissionsInput) (*Permissions, error)
}

// AuthorizeInput describes a request to authorize.
type AuthorizeInput struct {
	Requester Requester
	Actions   []policy.Action
	// Bucket and Object scope the actions; empty for deployment-wide
	// actions (admin:*, s3:ListAllMyBuckets).
	Bucket string
	Object string
	// SelfService marks the operations whose ownership rule is enforced by
	// the domain service (iam access keys, share links).
	SelfService bool
}

// Decision is the outcome of Authorize.
type Decision struct {
	Allowed bool
	// DeniedAction is the first action that was denied.
	DeniedAction policy.Action
	// Reason explains the decision (policy engine reason).
	Reason string
	// SelfService reports a request allowed by the self-service rule only.
	SelfService bool
}

// PermissionsInput are the parameters of Permissions.
type PermissionsInput struct {
	Requester Requester
	// User, when set, evaluates the permissions of that IAM user (resolved
	// through IAM, groups included) instead of the requester's.
	User string
	// Bucket scopes the s3:* actions; Prefix narrows the object actions.
	Bucket string
	Prefix string
}

// Permissions are the effective permissions of an identity.
type Permissions struct {
	// User is the user the permissions belong to (the effective user of
	// the requester when PermissionsInput.User is empty).
	User   string
	IsRoot bool
	Bucket string
	Prefix string
	// Allowed and Denied partition the catalog, each sorted.
	Allowed []policy.Action
	Denied  []policy.Action
}

// --- buckets -------------------------------------------------------------

// BucketManager is the console view of the buckets.
type BucketManager interface {
	// List returns the buckets visible to the requester, with their usage
	// and public flag: every bucket with s3:ListAllMyBuckets, otherwise
	// those it may list (s3:ListBucket).
	List(ctx context.Context, in ListBucketsInput) ([]BucketSummary, error)
	// Get returns a bucket with its usage and public flag.
	Get(ctx context.Context, name string) (*BucketSummary, error)
	// Create authorizes (s3:CreateBucket and the actions of the requested
	// settings), creates the bucket and applies versioning and quota; a
	// failure after the creation removes the new bucket.
	Create(ctx context.Context, in CreateBucketInput) (*BucketSummary, error)
	// Delete removes a bucket. With Force the bucket is emptied first
	// through the object layer (every version, delete marker and pending
	// multipart upload), so that object lock and retention are honored: a
	// protected version fails the deletion and the bucket is kept.
	Delete(ctx context.Context, in DeleteBucketInput) error
	// Access lists the anonymous access rules (presets per prefix) of the
	// bucket policy.
	Access(ctx context.Context, bucket string) (*BucketAccess, error)
	// SetAccess applies an anonymous access preset to a prefix ("" for
	// the whole bucket) and stores the resulting bucket policy, or deletes
	// it when no statement remains. PresetPrivate removes the rule.
	SetAccess(ctx context.Context, in SetAccessInput) (*BucketAccess, error)
}

// ListBucketsInput are the parameters of BucketManager.List.
type ListBucketsInput struct {
	Requester Requester
	// Prefix filters the bucket names.
	Prefix string
}

// BucketSummary is a bucket as shown by the console.
type BucketSummary struct {
	Info domain.BucketInfo
	// Public reports a bucket policy granting access to everyone.
	Public bool
}

// CreateBucketInput are the parameters of BucketManager.Create.
type CreateBucketInput struct {
	Requester Requester
	Name      string
	Region    string
	// Versioning enables versioning at creation (implied by ObjectLock).
	Versioning bool
	ObjectLock bool
	// QuotaBytes sets a hard quota; 0 means none.
	QuotaBytes uint64
}

// DeleteBucketInput are the parameters of BucketManager.Delete.
type DeleteBucketInput struct {
	Name  string
	Force bool
}

// BucketAccess is the anonymous access of a bucket.
type BucketAccess struct {
	Bucket string
	// Rules are the prefixes with anonymous access, sorted by prefix.
	Rules []policy.PublicAccessRule
	// Public reports that the bucket policy grants access to everyone.
	Public bool
	// HasPolicy reports that the bucket has a policy (possibly with
	// statements that are not presets).
	HasPolicy bool
}

// SetAccessInput are the parameters of BucketManager.SetAccess.
type SetAccessInput struct {
	Bucket string
	Prefix string
	Preset policy.Preset
}

// --- object browser ------------------------------------------------------

// ObjectBrowser implements the object browser of the console. The
// operations naming several objects authorize every object with the
// Authorizer and report per-object results.
type ObjectBrowser interface {
	// Stat returns the metadata of an object (version) with its tags and
	// first versions, each included only when the requester may read them.
	Stat(ctx context.Context, in StatInput) (*ObjectDetails, error)
	// Versions lists the versions and delete markers of one key, newest
	// first, resuming after Cursor.
	Versions(ctx context.Context, in VersionsInput) (*VersionList, error)
	// CreateFolder creates the empty "folder" object Prefix (which ends
	// with "/") after authorizing s3:PutObject.
	CreateFolder(ctx context.Context, in FolderInput) (domain.ObjectInfo, error)
	// Delete deletes explicit objects (versions) and, recursively, every
	// object under the given prefixes. It is synchronous and reports one
	// result per object.
	Delete(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsResult, error)
	// Rename moves an object, or every object under a prefix, within the
	// bucket: copy (multipart for objects over 5 GiB) then delete.
	Rename(ctx context.Context, in RenameInput) (*RenameResult, error)
	// Open opens an object for download or preview and decides how it is
	// served. Previews are inline only for content types that cannot run
	// scripts in the console origin (raster images, PDF, plain text, JSON,
	// audio, video); everything else (SVG, HTML, XML...) is an attachment.
	Open(ctx context.Context, in OpenInput) (*Download, error)
	// Zip streams a zip archive of explicit objects and of every object
	// under the given prefixes: inputs and the explicit objects are
	// checked first (errors are returned before anything is written), then
	// open is called with the archive name and the entries are written as
	// they are read, without buffering. Objects of the prefixes the
	// requester may not read are skipped and listed in a final
	// "nexo-zip-errors.txt" entry.
	Zip(ctx context.Context, in ZipInput, open ZipOpener) (*ZipResult, error)
}

// StatInput are the parameters of Stat.
type StatInput struct {
	Requester Requester
	Bucket    string
	Key       string
	VersionID string
}

// ObjectDetails is the result of Stat.
type ObjectDetails struct {
	Info domain.ObjectInfo
	// Tags is nil when the requester may not read them.
	Tags map[string]string
	// Versions are the first versions of the key (nil when the requester
	// may not list them); VersionsTruncated reports more.
	Versions          []domain.ObjectInfo
	VersionsTruncated bool
}

// VersionsInput are the parameters of Versions.
type VersionsInput struct {
	Bucket string
	Key    string
	// Cursor resumes a truncated listing (VersionList.NextCursor).
	Cursor string
	// Limit is 1 to 1000; 0 means 1000.
	Limit int
}

// VersionList is a page of versions of one key.
type VersionList struct {
	Versions   []domain.ObjectInfo
	NextCursor string
}

// FolderInput are the parameters of CreateFolder.
type FolderInput struct {
	Requester Requester
	Bucket    string
	// Prefix is the folder key; a trailing "/" is added when missing.
	Prefix string
}

// ObjectRef names an object or one of its versions.
type ObjectRef struct {
	Key       string
	VersionID string
}

// DeleteObjectsInput are the parameters of Delete.
type DeleteObjectsInput struct {
	Requester Requester
	Bucket    string
	Objects   []ObjectRef
	// Prefixes are deleted recursively ("" is the whole bucket).
	Prefixes []string
	// AllVersions deletes every version and delete marker under the
	// prefixes permanently, instead of the latest versions (which leaves
	// delete markers in versioned buckets).
	AllVersions bool
}

// DeletedObject is a successful deletion.
type DeletedObject struct {
	Key       string
	VersionID string
	// DeleteMarker reports that a delete marker was created (or removed).
	DeleteMarker          bool
	DeleteMarkerVersionID string
}

// ObjectError is a per-object failure; Err wraps a domain sentinel.
type ObjectError struct {
	Key       string
	VersionID string
	Err       error
}

// DeleteObjectsResult reports every object of a Delete.
type DeleteObjectsResult struct {
	Deleted []DeletedObject
	Errors  []ObjectError
}

// RenameInput are the parameters of Rename.
type RenameInput struct {
	Requester   Requester
	Bucket      string
	Source      string
	Destination string
	// Prefix renames every object under Source to Destination + the rest
	// of the key; Destination may not lie under Source.
	Prefix bool
	// Overwrite replaces existing destination objects; otherwise they are
	// reported as domain.ErrConflict.
	Overwrite bool
}

// RenamedObject is a successful move.
type RenamedObject struct {
	Source      string
	Destination string
}

// RenameError is a failed move; Err wraps a domain sentinel.
type RenameError struct {
	Source      string
	Destination string
	Err         error
}

// RenameResult reports every object of a Rename.
type RenameResult struct {
	Renamed []RenamedObject
	Errors  []RenameError
}

// OpenInput are the parameters of Open.
type OpenInput struct {
	Bucket    string
	Key       string
	VersionID string
	// Range is nil for the whole object.
	Range      *domain.Range
	Conditions domain.Conditions
	// Preview asks for inline rendering when the content type is safe.
	Preview bool
}

// Disposition values of a Download.
const (
	DispositionInline     = "inline"
	DispositionAttachment = "attachment"
)

// Download is an opened object and how to serve it. The caller must close
// Reader.
type Download struct {
	Reader *object.Reader
	// ContentType is the type to serve (normalized for previews).
	ContentType string
	// Disposition is DispositionInline or DispositionAttachment.
	Disposition string
	// FileName is the last segment of the key.
	FileName string
	// Partial reports a range response.
	Partial bool
	// Sandbox asks the transport to confine an inline document with a
	// sandboxing Content-Security-Policy (every inline type but PDF, which
	// the browsers render in their own viewer).
	Sandbox bool
}

// ZipInput are the parameters of Zip.
type ZipInput struct {
	Requester Requester
	Bucket    string
	// Base is the prefix the archive names are relative to (the folder
	// being browsed); keys outside it keep their full path.
	Base     string
	Keys     []string
	Prefixes []string
}

// ZipOpener is called once the inputs are validated: it returns the
// writer of the archive named filename (the transport sends the response
// headers there).
type ZipOpener func(filename string) (io.Writer, error)

// ZipResult reports a written archive.
type ZipResult struct {
	Files int
	Bytes int64
	// Skipped are the objects left out (access denied, vanished).
	Skipped []ObjectError
}

// --- shares --------------------------------------------------------------

// ShareManager exposes share links and presigned URLs to the console and
// the CLI. A requester always manages its own share links; managing those
// of other users requires admin:ManageShares (root has it implicitly).
// Signatures are computed on the server with the requester's credentials,
// which never leave it.
type ShareManager interface {
	// Create creates a managed share link owned by the requester.
	Create(ctx context.Context, in CreateShareInput) (*share.Share, error)
	// Get returns a share of the requester (any share with
	// admin:ManageShares).
	Get(ctx context.Context, req Requester, id string) (*share.Share, error)
	// List lists the active shares: the requester's own, or with
	// admin:ManageShares every share matching the filter.
	List(ctx context.Context, in ListSharesInput) ([]share.Share, error)
	// Revoke revokes one share.
	Revoke(ctx context.Context, req Requester, id string) error
	// RevokeAll revokes every share of owner ("" is the requester) and
	// returns how many were revoked.
	RevokeAll(ctx context.Context, req Requester, owner string) (int, error)
	// PresignShare signs the URL of key under a prefix share (or the
	// object of an object share) with the share's service account.
	PresignShare(ctx context.Context, req Requester, id, key string) (*SignedURL, error)
	// Presign returns a raw presigned URL (not revocable) signed with the
	// requester's credentials, after checking that it may perform the
	// operation now (it is checked again when the URL is used).
	Presign(ctx context.Context, in PresignInput) (*SignedURL, error)
	// PostPolicy builds and signs a POST policy form for browser uploads.
	PostPolicy(ctx context.Context, in PostPolicyInput) (*PostPolicyForm, error)
}

// CreateShareInput are the parameters of ShareManager.Create.
type CreateShareInput struct {
	Requester Requester
	Bucket    string
	Key       string
	Prefix    bool
	// Permission is share.PermissionDownload or share.PermissionUpload.
	Permission share.Permission
	// Expires is the validity (1 second to 7 days; 0 is 7 days).
	Expires            time.Duration
	ContentType        string
	ContentDisposition string
	VersionID          string
}

// ListSharesInput are the parameters of ShareManager.List.
type ListSharesInput struct {
	Requester Requester
	Owner     string
	Bucket    string
}

// SignedURL is a presigned URL.
type SignedURL struct {
	URL     string
	Method  string
	Expires time.Time
	// Headers are the signed headers the client must send unchanged.
	Headers map[string]string
}

// Presigned methods.
const (
	MethodGet    = "GET"
	MethodPut    = "PUT"
	MethodHead   = "HEAD"
	MethodDelete = "DELETE"
)

// PresignInput are the parameters of ShareManager.Presign.
type PresignInput struct {
	Requester Requester
	// Method is GET, PUT, HEAD or DELETE.
	Method    string
	Bucket    string
	Key       string
	VersionID string
	// Expires is the validity (1 second to 7 days), capped at the
	// expiration of the requester's credentials.
	Expires time.Duration
	// ContentType is a signed header of PUT URLs and the
	// response-content-type of GET URLs.
	ContentType string
	// ContentDisposition is the response-content-disposition of GET URLs.
	ContentDisposition string
	// Headers are additional signed headers (x-amz-meta-*, ...).
	Headers map[string]string
}

// PostPolicyInput are the parameters of ShareManager.PostPolicy.
type PostPolicyInput struct {
	Requester Requester
	Bucket    string
	// Key is the exact key; with KeyPrefix the browser may choose any key
	// under it (an empty Key then defaults to KeyPrefix + "${filename}").
	Key       string
	KeyPrefix string
	// Expires is the validity of the policy (1 second to 7 days).
	Expires time.Duration
	// MinSize and MaxSize bound the file size (content-length-range) when
	// MaxSize is positive.
	MinSize int64
	MaxSize int64
	// ContentType requires an exact Content-Type; ContentTypePrefix a
	// prefix (e.g. "image/").
	ContentType       string
	ContentTypePrefix string
	// SuccessActionStatus (200, 201 or 204) and SuccessActionRedirect.
	SuccessActionStatus   int
	SuccessActionRedirect string
	// Fields are additional exact-match fields (x-amz-meta-*, ...).
	Fields map[string]string
}

// PostPolicyForm is a signed POST policy form.
type PostPolicyForm struct {
	// URL is where the form is posted (the bucket URL).
	URL string
	// Fields are the form fields to send before the file.
	Fields     map[string]string
	Expiration time.Time
	// Conditions are the conditions of the signed policy.
	Conditions []PostCondition
}

// PostCondition is one condition of a POST policy.
type PostCondition struct {
	Operator string
	Key      string
	Value    string
	Min      int64
	Max      int64
}

// --- identities and policies --------------------------------------------

// IdentityManager implements the user and group operations of the console
// that combine several IAM calls. Each change is authorized with its own
// admin action before anything is applied.
type IdentityManager interface {
	// CreateUser creates a user and adds it to Groups
	// (admin:AddUserToGroup is required when Groups is set).
	CreateUser(ctx context.Context, in CreateUserInput) (*iam.User, error)
	// UpdateUser changes the secret (admin:CreateUser), the status
	// (admin:EnableUser / admin:DisableUser), the attached policies
	// (admin:AttachUserOrGroupPolicy) and the group memberships
	// (admin:AddUserToGroup / admin:RemoveUserFromGroup) of a user.
	UpdateUser(ctx context.Context, in UpdateUserInput) (*iam.User, error)
	// UpdateGroup changes the status (admin:EnableGroup /
	// admin:DisableGroup), the policies (admin:AttachUserOrGroupPolicy)
	// and the members (admin:AddUserToGroup / admin:RemoveUserFromGroup)
	// of a group.
	UpdateGroup(ctx context.Context, in UpdateGroupInput) (*iam.Group, error)
}

// CreateUserInput are the parameters of IdentityManager.CreateUser.
type CreateUserInput struct {
	Requester   Requester
	AccessKey   string
	SecretKey   string
	Status      domain.CredentialStatus
	PolicyNames []string
	Groups      []string
}

// UpdateUserInput are the parameters of UpdateUser; nil fields are left
// unchanged.
type UpdateUserInput struct {
	Requester   Requester
	AccessKey   string
	SecretKey   *string
	Status      *domain.CredentialStatus
	PolicyNames *[]string
	// Groups replaces the group memberships of the user.
	Groups *[]string
}

// UpdateGroupInput are the parameters of UpdateGroup; nil and empty
// fields are left unchanged.
type UpdateGroupInput struct {
	Requester     Requester
	Name          string
	Status        *domain.CredentialStatus
	PolicyNames   *[]string
	AddMembers    []string
	RemoveMembers []string
}

// PolicyManager implements the policy operations of the console that go
// beyond the policy engine contract: named records, documents validated
// from their JSON text (errors located by line and column) and the
// simulator.
type PolicyManager interface {
	// Get returns the record of a named policy (built-in or stored).
	Get(ctx context.Context, name string) (*policy.Named, error)
	// Save parses Document (errors wrap domain.ErrMalformedPolicy with a
	// *policy.ValidationError), stores it under Name and returns the
	// record. Create fails with domain.ErrConflict when the name exists.
	Save(ctx context.Context, in SavePolicyInput) (*policy.Named, error)
	// Validate checks a document without storing it: an IAM policy, or a
	// bucket policy of Bucket when set.
	Validate(ctx context.Context, in ValidatePolicyInput) (*ValidationReport, error)
	// Simulate evaluates a request for a user (the requester when empty)
	// with the same engine as the enforcement and returns the decision,
	// the deciding statement and every statement considered.
	Simulate(ctx context.Context, in SimulateInput) (*Simulation, error)
}

// SavePolicyInput are the parameters of PolicyManager.Save.
type SavePolicyInput struct {
	Name     string
	Document json.RawMessage
	// Create refuses to replace an existing policy.
	Create bool
}

// ValidatePolicyInput are the parameters of PolicyManager.Validate.
type ValidatePolicyInput struct {
	Document json.RawMessage
	// Bucket validates a bucket policy of that bucket.
	Bucket string
}

// ValidationReport is the result of Validate.
type ValidationReport struct {
	Valid    bool
	Problems []PolicyProblem
}

// PolicyProblem locates an error of a policy document.
type PolicyProblem struct {
	// Statement is 1-based, 0 for document-level errors.
	Statement int
	Sid       string
	Element   string
	// Line and Column are 1-based, 0 when unknown.
	Line    int
	Column  int
	Message string
}

// SimulateInput are the parameters of Simulate.
type SimulateInput struct {
	Requester Requester
	// User is the access key to simulate (user, service account); empty
	// means the requester.
	User   string
	Action policy.Action
	Bucket string
	Object string
	// Conditions are the request condition values (aws:SourceIp,
	// aws:SecureTransport, aws:CurrentTime...).
	Conditions map[string][]string
	// Now is the evaluation time (zero: the clock).
	Now time.Time
}

// Simulation is the result of Simulate.
type Simulation struct {
	// User is the simulated access key.
	User     string
	Decision policy.Decision
}

// --- configuration -------------------------------------------------------

// ConfigManager is the dynamic configuration with its schema: every
// subsystem and key written through it is validated against the schema
// (unknown subsystems and keys and malformed values are
// domain.ErrInvalidArgument).
type ConfigManager interface {
	// Schema describes the known subsystems, sorted by name.
	Schema(ctx context.Context) []SubsystemSchema
	// List returns the configured subsystems with their settings, sorted.
	List(ctx context.Context) ([]Subsystem, error)
	// Get returns the stored settings of a known subsystem.
	Get(ctx context.Context, name string) (config.KV, error)
	// Set validates and merges kv into a subsystem and returns its
	// settings.
	Set(ctx context.Context, name string, kv config.KV) (config.KV, error)
	// SetAll validates every subsystem first, then merges them.
	SetAll(ctx context.Context, all map[string]config.KV) error
	// Delete removes a subsystem, or only the given keys.
	Delete(ctx context.Context, name string, keys []string) error
	// Export serializes the whole configuration as JSON.
	Export(ctx context.Context) ([]byte, error)
	// Import validates and replaces the whole configuration.
	Import(ctx context.Context, data []byte) error
}

// Subsystem is the configuration of one subsystem.
type Subsystem struct {
	Name     string
	Settings config.KV
}

// KeyType is the type of a configuration value.
type KeyType string

// Configuration value types.
const (
	KeyString   KeyType = "string"
	KeyBool     KeyType = "bool"
	KeyInt      KeyType = "int"
	KeyDuration KeyType = "duration"
	KeyURL      KeyType = "url"
	KeyEnum     KeyType = "enum"
	// KeyList is a comma separated list of strings.
	KeyList KeyType = "list"
)

// SubsystemSchema describes a configuration subsystem.
type SubsystemSchema struct {
	Name        string
	Description string
	Keys        []KeySchema
}

// KeySchema describes a configuration key.
type KeySchema struct {
	Name        string
	Type        KeyType
	Description string
	// Default is the value used when the key is not set.
	Default string
	// Values are the accepted values of enum keys.
	Values []string
	// Sensitive marks secrets (tokens, keys).
	Sensitive bool
}
