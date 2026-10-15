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
//   - ObjectBrowser: the object browser operations (folder listing, stat,
//     versions, folders, recursive delete, rename, tags, download, preview
//     and zip archives), authorized object by object;
//   - UploadManager: the uploads proxied by the console (single request
//     and multipart), with their limits and the paging of the pending
//     uploads and parts;
//   - ShareManager: managed share links, raw presigned URLs and POST policy
//     forms, signed on behalf of the caller without exposing its secret;
//   - IdentityManager, AccessKeyManager and PolicyManager: the user, group,
//     access key and policy operations of the console that combine several
//     IAM calls or depend on the caller, the policy validator (with
//     positions) and the simulator;
//   - ConfigManager: the dynamic configuration with its schema of known
//     subsystems, validated before it reaches config.Service;
//   - LifecycleManager, NotificationManager and CORSManager: the lifecycle,
//     notification and CORS configurations of the buckets in the
//     structured form of the console, converted to and from the S3 XML
//     documents and validated rule by rule, and the notification targets
//     with their delivery state;
//   - AccessRuleManager, DistributionManager and CertificateManager: the
//     access rules of the buckets with their tester and block public
//     access, the distributions with their key groups, signatures, tests
//     and host routing, and the TLS certificates of their domains
//     (docs/10);
//   - CacheManager: the cache of the distributions (docs/11): reusable
//     policies, settings of the cluster, of the distributions, of their
//     domains and paths, invalidations with their state on every node,
//     warm-ups, purge webhooks, the inspector of a URL and the statistics.
//
// Every implementation is created by its New* constructor from a Deps
// struct of interfaces; the transport never applies these rules itself.
package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// Service exposes server administration.
type Service interface {
	// ServerInfo returns the server/cluster information.
	ServerInfo(ctx context.Context) (*domain.ServerInfo, error)
	// DataUsage returns the data usage per bucket: the usage computed by
	// the scanner, the buckets it has not met yet computed on demand (all
	// of them without scanner).
	DataUsage(ctx context.Context) (*DataUsage, error)
	// StartHeal starts a healing job (domain.ErrNotImplemented without
	// healing: the single-drive mode); an identical running job is
	// returned instead of a new one.
	StartHeal(ctx context.Context, in HealInput) (heal.Job, error)
	// HealStatus returns the state of a healing job (domain.ErrNotFound
	// when unknown).
	HealStatus(ctx context.Context, id string) (heal.Job, error)
	// ListHeals lists the running and recently finished healing jobs,
	// newest first.
	ListHeals(ctx context.Context) ([]heal.Job, error)
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
	// Prefixes breaks the bucket down by top-level prefix ("photos/", ""
	// for the root), when the scanner computed it.
	Prefixes map[string]PrefixUsage `json:"prefixes,omitempty"`
}

// PrefixUsage is the data usage of a top-level prefix of a bucket.
type PrefixUsage struct {
	Size          uint64 `json:"size"`
	ObjectsCount  uint64 `json:"objectsCount"`
	VersionsCount uint64 `json:"versionsCount"`
	DeleteMarkers uint64 `json:"deleteMarkers"`
}

// HealInput are the parameters of StartHeal: the whole deployment (format
// of the drives and buckets), a bucket, or with Recursive every object
// below the bucket(s) or Prefix (without Recursive, Prefix names the one
// object to heal).
type HealInput struct {
	Bucket    string
	Prefix    string
	Recursive bool
	DryRun    bool
	// Deep verifies the bitrot hash of every shard block.
	Deep bool
	// Remove deletes the dangling objects found (never implied).
	Remove bool
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

// --- pagination ----------------------------------------------------------

// MaxPageLimit is the largest page of the console lists, and the default.
const MaxPageLimit = 1000

// Page selects a page of a list. The lists are sorted by a stable key
// (name, access key, newest first for share links); the services select
// the page, the transports only carry Cursor and Limit.
type Page struct {
	// Cursor resumes a list after the last item of the previous page (its
	// Paged.NextCursor, an opaque value); "" starts from the first item. A
	// malformed cursor is domain.ErrInvalidArgument.
	Cursor string
	// Limit is 1 to MaxPageLimit; 0 means MaxPageLimit.
	Limit int
}

// Paged is a page of a list.
type Paged[T any] struct {
	Items []T
	// NextCursor resumes the list (Page.Cursor); "" on the last page.
	NextCursor string
}

// --- authorization -------------------------------------------------------

// Requester is the authenticated caller of an Admin API request: its
// identity and the condition values of the request (aws:SourceIp,
// aws:SecureTransport, aws:UserAgent...) the policy engine evaluates with
// every action the request needs.
type Requester struct {
	Identity   domain.Identity
	Conditions map[string][]string
}

// Client describes the client of an Admin API request; ClientConditions
// turns it into the condition values of its Requester.
type Client struct {
	// IP is the address of the client (behind the trusted proxies).
	IP string
	// Secure reports a request received over TLS, or through a trusted
	// proxy terminating it.
	Secure    bool
	UserAgent string
	Referer   string
}

// ObjectRequest describes, for the policy engine, the S3 request an object
// operation of the Admin API stands for. ObjectConditions gives it the
// condition values the S3 API computes for the same request, so that a
// policy decides alike through the console and through S3 (docs/12 §12.7):
// every object authorization of the transport and of the services goes
// through it.
type ObjectRequest struct {
	// VersionID is the version the operation addresses (s3:versionid).
	VersionID string
	// Headers are the x-amz-* headers (case-insensitive names) the
	// operation forwards to the object layer or signs into a presigned
	// URL, and only those: each is the condition value s3:<name>
	// (x-amz-meta-* and x-amz-security-token aside, as in the S3 API);
	// x-amz-tagging also gives s3:RequestObjectTag/<key> and
	// s3:RequestObjectTagKeys.
	Headers map[string]string
	// AuthType is how the S3 request authenticates: "" for a request
	// signed in its headers (what the console stands for),
	// domain.AuthTypeQuery for the use of a presigned URL or share link,
	// domain.AuthTypePost for a POST policy upload (s3:authType).
	AuthType domain.AuthType
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
	// own access keys, own share links); an explicit Deny never is.
	// Anonymous requesters are always denied. An empty action list is
	// domain.ErrInvalidArgument.
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
	// Conditions are request condition values specific to the operation
	// (s3:prefix, s3:delimiter, s3:max-keys...), merged over the
	// requester's.
	Conditions map[string][]string
	// SelfService marks the operations whose ownership rule is enforced by
	// the domain service (iam access keys, share links).
	SelfService bool
	// Partial also allows the actions that a statement grants on the
	// resource under conditions the request does not meet: a listing that
	// s3:prefix restricts to a home directory (docs/09 §9.3) makes the
	// bucket visible although its root cannot be listed. It decides what
	// the console shows (the buckets of the listing, the bucket page),
	// never an operation on data, which the S3 conditions of the
	// operation itself authorize. An explicit Deny that applies still
	// denies.
	Partial bool
}

// Decision is the outcome of Authorize.
type Decision struct {
	Allowed bool
	// DeniedAction is the first action that was denied.
	DeniedAction policy.Action
	// Reason explains the decision (policy engine reason).
	Reason string
	// SelfService reports a request allowed by the self-service rule only:
	// the requester may act on its own resources, not on those of others.
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
	// List returns the buckets visible to the requester, sorted by name,
	// one page at a time: every bucket with s3:ListAllMyBuckets, otherwise
	// those it may list at least in part (s3:ListBucket, conditions such
	// as the s3:prefix of a home directory aside: AuthorizeInput.Partial).
	// Each is summarized as Get does.
	List(ctx context.Context, in ListBucketsInput) (*Paged[BucketSummary], error)
	// Get returns a bucket as the requester may see it: with its usage,
	// settings, tags and public flag when it may list the whole bucket
	// (s3:ListBucket with no condition left unmet), otherwise only its
	// name, creation date and region (BucketSummary.Partial). The page of
	// a bucket opens for a requester that may list a part of it (a home
	// directory) without disclosing what S3 would not show it.
	Get(ctx context.Context, req Requester, name string) (*BucketSummary, error)
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
	// Tags returns the tags of a bucket. Unlike S3 (NoSuchTagSet), a
	// bucket without tags has an empty, non-nil set.
	Tags(ctx context.Context, name string) (map[string]string, error)
	// ObjectLock returns the object lock configuration of a bucket. Unlike
	// S3 (ObjectLockConfigurationNotFoundError), a bucket without one has
	// the zero configuration (disabled).
	ObjectLock(ctx context.Context, name string) (bucket.ObjectLockConfig, error)
}

// ListBucketsInput are the parameters of BucketManager.List.
type ListBucketsInput struct {
	Requester Requester
	// Prefix filters the bucket names.
	Prefix string
	Page   Page
}

// BucketSummary is a bucket as shown by the console.
type BucketSummary struct {
	Info domain.BucketInfo
	// Public reports a bucket policy granting access to everyone, or an
	// access rule in effect allowing everyone (docs/10 D-ACC-03).
	Public bool
	// Partial reports a summary restricted to the name, creation date and
	// region of the bucket (the other fields are zero): the requester may
	// list only a part of it.
	Partial bool
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
	// List is the folder view of a bucket: the objects and the common
	// prefixes ("folders") directly under Prefix, without the folder
	// placeholder object Prefix itself, resuming after Cursor. It
	// authorizes s3:ListBucket with the conditions s3:prefix, s3:delimiter
	// (the effective one) and s3:max-keys (when Limit is set).
	List(ctx context.Context, in ListObjectsInput) (*ObjectListing, error)
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
	// SetTags replaces the tags of an object (version); an empty set
	// removes them.
	SetTags(ctx context.Context, in SetTagsInput) (map[string]string, error)
	// RestoreVersion makes a previous version of an object the latest
	// again, as the S3 clients do: the version is copied onto its own key
	// (part by part over 5 GiB) with its content, metadata and tags, which
	// creates a new latest version and keeps the history. It authorizes
	// s3:GetObjectVersion on the version (with its s3:versionid) and
	// s3:PutObject on the key (with the x-amz-copy-source of the copy). A
	// delete marker cannot be restored (domain.ErrMethodNotAllowed).
	RestoreVersion(ctx context.Context, in RestoreVersionInput) (domain.ObjectInfo, error)
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

	// Retention returns the Object Lock retention of a version: the zero
	// retention when it has none (unlike S3,
	// NoSuchObjectLockConfiguration). A bucket without object locking is
	// domain.ErrInvalidArgument (S3 InvalidRequest). The route authorizes
	// s3:GetObjectRetention.
	Retention(ctx context.Context, ref VersionRef) (domain.ObjectRetention, error)
	// SetRetention sets the retention of a version, or removes it (the
	// zero retention), as the S3 PutObjectRetention: it authorizes
	// s3:PutObjectRetention and, with BypassGovernance,
	// s3:BypassGovernanceRetention (with the conditions of the S3 request
	// carrying x-amz-bypass-governance-retention), which a GOVERNANCE
	// retention needs to be shortened or removed; a COMPLIANCE retention
	// can only be extended, by anyone (domain.ErrObjectLocked).
	SetRetention(ctx context.Context, in SetRetentionInput) (domain.ObjectInfo, error)
	// LegalHold returns the legal hold of a version: domain.LegalHoldOff
	// when none was ever set. The route authorizes s3:GetObjectLegalHold.
	LegalHold(ctx context.Context, ref VersionRef) (domain.LegalHoldStatus, error)
	// SetLegalHold sets the legal hold of a version (ON or OFF); the
	// route authorizes s3:PutObjectLegalHold.
	SetLegalHold(ctx context.Context, in SetLegalHoldInput) (domain.ObjectInfo, error)
	// Attributes describes a version without reading its content (S3
	// GetObjectAttributes): its checksum and, for a multipart object, a
	// page of its parts. The route authorizes s3:GetObjectAttributes, or
	// s3:GetObjectVersionAttributes for a version.
	Attributes(ctx context.Context, in AttributesInput) (*object.ObjectAttributes, error)
}

// VersionRef names a version of an object ("" is the latest).
type VersionRef struct {
	Bucket    string
	Key       string
	VersionID string
}

// SetRetentionInput are the parameters of ObjectBrowser.SetRetention.
type SetRetentionInput struct {
	Requester Requester
	Object    VersionRef
	// Retention is the new retention: a mode (GOVERNANCE or COMPLIANCE,
	// case-insensitive) with a retain-until date in the future, or the
	// zero retention to remove it.
	Retention domain.ObjectRetention
	// BypassGovernance shortens or removes a GOVERNANCE retention, when
	// the requester may (s3:BypassGovernanceRetention).
	BypassGovernance bool
}

// SetLegalHoldInput are the parameters of ObjectBrowser.SetLegalHold.
type SetLegalHoldInput struct {
	Object VersionRef
	// Hold is ON or OFF (case-insensitive).
	Hold domain.LegalHoldStatus
}

// AttributesInput are the parameters of ObjectBrowser.Attributes.
type AttributesInput struct {
	Object VersionRef
	// MaxParts limits the parts returned (1 to 1000, 0 for 1000);
	// PartNumberMarker lists the parts after that number.
	MaxParts         int
	PartNumberMarker int
}

// Defaults of the object browser.
const (
	// DefaultDelimiter is the delimiter of the folder view: the one of
	// ObjectBrowser.List when the request names none.
	DefaultDelimiter = "/"
	// DefaultContentType is the type of the objects uploaded without one.
	DefaultContentType = "application/octet-stream"
)

// ListObjectsInput are the parameters of ObjectBrowser.List.
type ListObjectsInput struct {
	Requester Requester
	Bucket    string
	Prefix    string
	// Delimiter groups the keys into folders: nil means DefaultDelimiter
	// (the folder view), "" a flat recursive listing.
	Delimiter *string
	// Cursor resumes a truncated listing (ObjectListing.NextCursor).
	Cursor string
	// Limit is 1 to 1000; 0 means 1000.
	Limit int
}

// ObjectListing is a page of the folder view.
type ObjectListing struct {
	Objects  []domain.ObjectInfo
	Prefixes []string
	// NextCursor is set when the listing is truncated.
	NextCursor string
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
	// Prefixes are folders deleted recursively: a prefix without a
	// trailing slash names the folder of that name ("photos" is
	// "photos/", not "photos2/"); "" is the whole bucket.
	Prefixes []string
	// AllVersions deletes every version and delete marker under the
	// prefixes permanently, instead of the latest versions (which leaves
	// delete markers in versioned buckets).
	AllVersions bool
	// BypassGovernance deletes the versions a GOVERNANCE retention
	// protects, as the S3 x-amz-bypass-governance-retention header: for
	// each version the requester may bypass (s3:BypassGovernanceRetention
	// on the version), the others failing with domain.ErrObjectLocked. A
	// COMPLIANCE retention or a legal hold always protects the version.
	BypassGovernance bool
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

// RestoreVersionInput are the parameters of RestoreVersion.
type RestoreVersionInput struct {
	Requester Requester
	Bucket    string
	Key       string
	// VersionID is the version to restore (required).
	VersionID string
}

// RenameInput are the parameters of Rename.
type RenameInput struct {
	Requester   Requester
	Bucket      string
	Source      string
	Destination string
	// Prefix renames every object of the folder Source to the folder
	// Destination + the rest of the key; both name folders (a missing
	// trailing slash is added: "photos" is "photos/", not "photos2/") and
	// Destination may not lie under Source.
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

// SetTagsInput are the parameters of SetTags.
type SetTagsInput struct {
	Bucket    string
	Key       string
	VersionID string
	Tags      map[string]string
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
	// Base is the folder the archive names are relative to (the folder
	// being browsed); keys outside it keep their full path.
	Base string
	Keys []string
	// Prefixes are folders archived recursively; like Base, a prefix
	// without a trailing slash names the folder of that name ("photos" is
	// "photos/", not "photos2/").
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

// --- bucket configurations ----------------------------------------------

// RuleProblem locates an invalid rule of a bucket configuration: a
// lifecycle rule, a notification subscription or a CORS rule (see
// ConfigError).
type RuleProblem struct {
	// Rule is the 1-based index of the rule, 0 for a problem of the whole
	// configuration.
	Rule int
	// ID is the id of the rule, when it has one.
	ID string
	// Code is the S3 error code of the problem (InvalidArgument,
	// InvalidRequest, MalformedXML, NotImplemented...).
	Code    string
	Message string
}

// LifecycleManager edits the lifecycle configurations of the buckets
// (D-BKT-03, S-BKT-07) in the structured form of the console. The
// configurations are stored as the S3 LifecycleConfiguration documents of
// the bucket service, the very documents of the S3 API: what one API
// writes the other reads. Every rule is validated on its own with the
// rules of the S3 API (ilm.Service.Parse), so that a rejected configuration
// reports every invalid rule. The routes authorize
// s3:GetLifecycleConfiguration and s3:PutLifecycleConfiguration.
type LifecycleManager interface {
	// Get returns the lifecycle rules of a bucket; a bucket without
	// configuration has none (unlike S3, NoSuchLifecycleConfiguration).
	Get(ctx context.Context, bucket string) (*LifecycleConfig, error)
	// Set validates every rule (a *ConfigError wrapping
	// domain.ErrInvalidArgument lists the invalid ones; nothing is stored
	// then), stores the configuration and returns it as stored: the rules
	// without ID get one. No rule removes the configuration.
	Set(ctx context.Context, bucket string, cfg LifecycleConfig) (*LifecycleConfig, error)
	// Delete removes the configuration of a bucket.
	Delete(ctx context.Context, bucket string) error
	// Apply applies the lifecycle of a bucket at once, as the scanner
	// does over its cycle (ilm.Service.ApplyNow): the expired versions are
	// removed (those Object Lock protects only get a delete marker), the
	// incomplete uploads aborted. A bucket without configuration is left
	// as is (zero result). The route also authorizes admin:ApplyLifecycle.
	Apply(ctx context.Context, bucket string) (ilm.ApplyResult, error)
}

// LifecycleConfig is a lifecycle configuration in the form of the console.
type LifecycleConfig struct {
	Rules []LifecycleRule
}

// LifecycleRule is one lifecycle rule (S3 Rule). The optional numbers are
// pointers: a number sent as 0 is validated as such (S3 rejects a zero
// Days), never taken for an absent one.
type LifecycleRule struct {
	// ID identifies the rule (at most 255 characters); one is generated
	// from the content of a rule without.
	ID      string
	Enabled bool
	// Filter selects the objects of the rule; the zero filter selects the
	// whole bucket.
	Filter                LifecycleFilter
	Expiration            *LifecycleExpiration
	NoncurrentExpiration  *LifecycleNoncurrentExpiration
	AbortIncompleteUpload *LifecycleAbortUpload
	// Transitions and NoncurrentTransitions move versions to the storage
	// class of a remote tier (P2): refused (NotImplemented) without one.
	Transitions           []LifecycleTransition
	NoncurrentTransitions []LifecycleNoncurrentTransition
}

// LifecycleFilter selects the objects of a rule: every criterion set must
// match.
type LifecycleFilter struct {
	Prefix string
	Tags   map[string]string
	// SizeGreaterThan and SizeLessThan bound the object size in bytes
	// (exclusive).
	SizeGreaterThan *int64
	SizeLessThan    *int64
}

// LifecycleExpiration expires the current versions Days after their
// creation or on Date, or (ExpiredObjectDeleteMarker) removes the delete
// markers left without versions: exactly one of them.
type LifecycleExpiration struct {
	Days *int
	// Date is an ISO 8601 date at midnight UTC ("2030-01-01" or
	// "2030-01-01T00:00:00Z"); Get returns the date only.
	Date                      string
	ExpiredObjectDeleteMarker bool
}

// LifecycleNoncurrentExpiration removes the noncurrent versions
// NoncurrentDays after they became noncurrent, keeping the
// NewerNoncurrentVersions newest ones.
type LifecycleNoncurrentExpiration struct {
	NoncurrentDays          *int
	NewerNoncurrentVersions *int
}

// LifecycleAbortUpload aborts the multipart uploads DaysAfterInitiation
// days after they started.
type LifecycleAbortUpload struct {
	DaysAfterInitiation *int
}

// LifecycleTransition moves the current versions to StorageClass Days
// after their creation or on Date.
type LifecycleTransition struct {
	Days         *int
	Date         string
	StorageClass string
}

// LifecycleNoncurrentTransition moves the noncurrent versions to
// StorageClass.
type LifecycleNoncurrentTransition struct {
	NoncurrentDays          *int
	NewerNoncurrentVersions *int
	StorageClass            string
}

// NotificationManager edits the event subscriptions of the buckets
// (D-BKT-04, S-BKT-10) in the form of the console, stored as the S3
// NotificationConfiguration documents of the bucket service (those of the
// S3 API), and exposes the notification targets of the deployment (the
// notify_* subsystems of the configuration) with their delivery state.
// The bucket routes authorize s3:GetBucketNotification and
// s3:PutBucketNotification, the target routes admin:ConfigUpdate.
type NotificationManager interface {
	// Get returns the subscriptions of a bucket (none without
	// configuration).
	Get(ctx context.Context, bucket string) (*NotificationConfig, error)
	// Set validates every subscription (known event names, prefix and
	// suffix filters, a destination ARN naming a configured target: a
	// *ConfigError wrapping domain.ErrInvalidArgument lists the invalid
	// ones), stores the configuration and returns it as stored: the
	// subscriptions without ID get one. No subscription removes them all.
	Set(ctx context.Context, bucket string, cfg NotificationConfig) (*NotificationConfig, error)
	// Delete removes every subscription of a bucket.
	Delete(ctx context.Context, bucket string) error
	// Targets lists the configured targets, sorted by id and type, with
	// the delivery state this node sees: online or offline (the last
	// delivery failed) and the events pending in its queue.
	Targets(ctx context.Context) ([]NotifyTarget, error)
	// TestTarget sends a test event (s3:TestEvent) to a target at once and
	// reports the outcome. target is "<id>:<type>" (the default target of a
	// subsystem has the id "_") or the ARN of the target; an unknown target
	// is domain.ErrNotFound.
	TestTarget(ctx context.Context, target string) (*TargetTest, error)
}

// NotificationConfig are the event subscriptions of a bucket.
type NotificationConfig struct {
	Subscriptions []Subscription
}

// Subscription routes the events of a bucket to a target.
type Subscription struct {
	// ID identifies the subscription (at most 255 characters); one is
	// generated from the content of a subscription without.
	ID string
	// ARN names the target: arn:nexo:sqs:<region>:<id>:<type>
	// (NotifyTarget.ARN).
	ARN string
	// Events are event names or families ("s3:ObjectCreated:*").
	Events []string
	// Prefix and Suffix filter the object keys.
	Prefix string
	Suffix string
}

// NotifyTarget is a notification target with its delivery state.
type NotifyTarget struct {
	// ID is the id of the target ("_" for the default target of its
	// subsystem), Type its type (webhook, kafka, nats, amqp, redis,
	// postgresql).
	ID   string
	Type string
	// ARN is what the subscriptions name.
	ARN     string
	Enabled bool
	// Endpoint describes the destination without its credentials (the
	// webhook URL, the brokers and topic...).
	Endpoint string
	// Online is false from a failed delivery to the next successful one.
	Online bool
	// Pending counts the events waiting in the queue of the target.
	Pending int
	// LastError is the error of the last failed delivery.
	LastError string
}

// TargetTest is the outcome of NotificationManager.TestTarget.
type TargetTest struct {
	// Target is "<id>:<type>".
	Target    string
	Delivered bool
	// Error is the reason of a failed delivery.
	Error string
}

// CORSManager edits the CORS configurations of the buckets (S-BKT-06) in
// the form of the console, stored as the S3 CORSConfiguration documents of
// the bucket service (those of the S3 API) and validated rule by rule
// (domain.CORSConfiguration.Validate). The routes authorize
// s3:GetBucketCORS and s3:PutBucketCORS.
type CORSManager interface {
	// Get returns the CORS rules of a bucket (none without configuration,
	// unlike S3: NoSuchCORSConfiguration).
	Get(ctx context.Context, bucket string) (*CORSConfig, error)
	// Set validates every rule (a *ConfigError wrapping
	// domain.ErrInvalidArgument lists the invalid ones), stores the
	// configuration and returns it as stored. No rule removes the
	// configuration.
	Set(ctx context.Context, bucket string, cfg CORSConfig) (*CORSConfig, error)
	// Delete removes the configuration of a bucket.
	Delete(ctx context.Context, bucket string) error
}

// CORSConfig is the CORS configuration of a bucket, its rules evaluated
// in order.
type CORSConfig struct {
	Rules []domain.CORSRule
}

// --- proxied uploads -----------------------------------------------------

// UploadManager implements the uploads the console proxies to the object
// layer (the console never holds the secret key of the user): objects
// stored in one request and multipart uploads part by part, with the
// limits of their bodies and the paging of the pending uploads and of the
// parts. Each route authorizes the S3 action of its operation
// (s3:PutObject, s3:ListBucketMultipartUploads,
// s3:ListMultipartUploadParts, s3:AbortMultipartUpload) on the object or
// the bucket before calling it.
type UploadManager interface {
	// Upload stores an object in one request. A Size over
	// UploadLimits.MaxUploadSize is domain.ErrEntityTooLarge before
	// anything is read; a body of unknown size (-1), or longer than
	// announced, fails with domain.ErrEntityTooLarge once it exceeds the
	// limit and nothing is committed. An object without a content type is
	// stored as DefaultContentType.
	Upload(ctx context.Context, in object.PutObjectInput) (domain.ObjectInfo, error)
	// Create starts a multipart upload (an object without a content type
	// is stored as DefaultContentType) and returns it with the limits its
	// parts must respect.
	Create(ctx context.Context, in object.NewMultipartInput) (*NewUpload, error)
	// List lists the pending multipart uploads of a bucket under Prefix,
	// sorted by key and upload ID, one page at a time.
	List(ctx context.Context, in ListUploadsInput) (*Paged[domain.MultipartUpload], error)
	// Parts lists the uploaded parts of an upload by number, one page at a
	// time.
	Parts(ctx context.Context, in ListPartsInput) (*Paged[domain.Part], error)
	// UploadPart stores one part, streaming. The number is 1 to
	// UploadLimits.MaxParts and the size is required, or
	// domain.ErrInvalidArgument; a part over UploadLimits.MaxPartSize is
	// domain.ErrEntityTooLarge, before anything is read or once the body
	// exceeds it.
	UploadPart(ctx context.Context, in object.PutObjectPartInput) (domain.Part, error)
	// Complete assembles the parts into the object.
	Complete(ctx context.Context, in object.CompleteMultipartInput) (domain.ObjectInfo, error)
	// Abort discards an upload and its parts.
	Abort(ctx context.Context, in UploadRef) error
}

// Limits of the proxied uploads.
const (
	// DefaultMaxUploadSize is the default limit of an object stored in one
	// request: larger files go multipart.
	DefaultMaxUploadSize = 64 << 20
	// DefaultMaxPartSize is the default limit of a proxied part.
	DefaultMaxPartSize = 64 << 20
	// MinPartSize is the S3 minimum size of every part but the last.
	MinPartSize = 5 << 20
	// MaxUploadParts is the S3 maximum number of parts of an upload.
	MaxUploadParts = 10000
	// RecommendedPartSize is the part size suggested to the console (at
	// most the part limit).
	RecommendedPartSize = 16 << 20
)

// UploadLimits are the limits of the proxied uploads.
type UploadLimits struct {
	// MaxUploadSize bounds an object stored in one request.
	MaxUploadSize int64
	// PartSize is the recommended part size.
	PartSize int64
	// MinPartSize applies to every part but the last.
	MinPartSize int64
	MaxPartSize int64
	MaxParts    int
}

// NewUpload is a started multipart upload with the limits of its parts.
type NewUpload struct {
	Upload domain.MultipartUpload
	Limits UploadLimits
}

// UploadRef names a multipart upload.
type UploadRef struct {
	Bucket   string
	Key      string
	UploadID string
}

// ListUploadsInput are the parameters of UploadManager.List.
type ListUploadsInput struct {
	Bucket string
	// Prefix filters the keys.
	Prefix string
	Page   Page
}

// ListPartsInput are the parameters of UploadManager.Parts.
type ListPartsInput struct {
	Upload UploadRef
	Page   Page
}

// --- shares --------------------------------------------------------------

// ShareManager exposes share links and presigned URLs to the console and
// the CLI. A requester always manages its own share links (unless
// explicitly denied); managing those of other users requires
// admin:ManageShares (root has it implicitly). Signatures are computed on
// the server with the requester's credentials, which never leave it.
type ShareManager interface {
	// Create creates a managed share link owned by the requester.
	Create(ctx context.Context, in CreateShareInput) (*share.Share, error)
	// Get returns a share of the requester (any share with
	// admin:ManageShares).
	Get(ctx context.Context, req Requester, id string) (*share.Share, error)
	// List lists the active shares, newest first, one page at a time: the
	// requester's own, or with admin:ManageShares every share matching the
	// filter.
	List(ctx context.Context, in ListSharesInput) (*Paged[share.Share], error)
	// Links hands out a prefix download share one object at a time, as
	// `nexoctl share download --recursive` does: a page of the objects
	// under the shared prefix, each with a URL signed by the share (so
	// revoking the share revokes them all) valid until the share expires.
	// The requester must see the share (Get) and may list the prefix
	// (s3:ListBucket with s3:prefix); other shares are
	// domain.ErrInvalidArgument.
	Links(ctx context.Context, in ShareLinksInput) (*ShareLinks, error)
	// Revoke revokes one share.
	Revoke(ctx context.Context, req Requester, id string) error
	// RevokeAll revokes every share of owner ("" is the requester) and
	// returns how many were revoked.
	RevokeAll(ctx context.Context, req Requester, owner string) (int, error)
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
	// Permission is share.PermissionDownload (the default when empty) or
	// share.PermissionUpload.
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
	Page      Page
}

// ShareLinksInput are the parameters of ShareManager.Links.
type ShareLinksInput struct {
	Requester Requester
	ID        string
	Page      Page
}

// ShareLinks is a page of the per-object links of a prefix share.
type ShareLinks struct {
	Share *share.Share
	Items []ShareLink
	// NextCursor resumes the listing (Page.Cursor); "" on the last page.
	NextCursor string
}

// ShareLink is the link of one object of a prefix share.
type ShareLink struct {
	Key  string
	Size int64
	URL  string
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
	// Expires is the validity (1 second to 7 days; 0 is 7 days), capped at
	// the expiration of the requester's credentials.
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
	// Expires is the validity of the policy (1 second to 7 days; 0 is one
	// hour).
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
	// ListUsers lists the users sorted by access key, one page at a time;
	// the route authorizes admin:ListUsers.
	ListUsers(ctx context.Context, page Page) (*Paged[iam.User], error)
	// ListGroups lists the groups sorted by name, one page at a time; the
	// route authorizes admin:ListGroups.
	ListGroups(ctx context.Context, page Page) (*Paged[iam.Group], error)
	// CreateUser creates a user (admin:CreateUser) and adds it to Groups
	// (admin:AddUserToGroup); attaching policies requires
	// admin:AttachUserOrGroupPolicy.
	CreateUser(ctx context.Context, in CreateUserInput) (*iam.User, error)
	// UpdateUser changes the secret (admin:CreateUser), the status
	// (admin:EnableUser / admin:DisableUser), the attached policies
	// (admin:AttachUserOrGroupPolicy) and the group memberships
	// (admin:AddUserToGroup / admin:RemoveUserFromGroup) of a user.
	UpdateUser(ctx context.Context, in UpdateUserInput) (*iam.User, error)
	// DeleteUser deletes a user (admin:DeleteUser) with its access keys
	// and STS credentials, then removes the records of its managed share
	// links (docs/09 §9.1): their service accounts went with the user, so
	// they no longer work and must not be listed as active, nor be
	// inherited by a user later created with the same name. Root cannot
	// be deleted and a missing user is domain.ErrUserNotFound; in both
	// cases nothing is changed.
	DeleteUser(ctx context.Context, req Requester, user string) error
	// CreateGroup creates a group with its members (admin:AddUserToGroup)
	// and policies (admin:AttachUserOrGroupPolicy).
	CreateGroup(ctx context.Context, in CreateGroupInput) (*iam.Group, error)
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

// CreateGroupInput are the parameters of IdentityManager.CreateGroup.
type CreateGroupInput struct {
	Requester   Requester
	Name        string
	Members     []string
	PolicyNames []string
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

// AccessKeyManager manages service accounts on behalf of a requester. A
// requester always manages its own keys (self-service, unless explicitly
// denied); the keys of other users need the admin action granted by a
// policy (root has every action). Derived credentials (service accounts,
// STS) manage no key, as iam.Service enforces.
type AccessKeyManager interface {
	// List lists the keys of parent ("" is the requester's own keys, or
	// every key for an administrator), sorted by access key, one page at
	// a time.
	List(ctx context.Context, req Requester, parent string, page Page) (*Paged[iam.AccessKey], error)
	// Get returns a key without its secret.
	Get(ctx context.Context, req Requester, accessKey string) (*iam.AccessKey, error)
	// Create creates a key; the result carries the secret, returned only
	// here.
	Create(ctx context.Context, req Requester, in iam.CreateAccessKeyInput) (*iam.AccessKey, error)
	// Update changes a key.
	Update(ctx context.Context, req Requester, accessKey string, in iam.UpdateAccessKeyInput) (*iam.AccessKey, error)
	// Delete removes a key.
	Delete(ctx context.Context, req Requester, accessKey string) error
}

// PolicyManager implements the policy operations of the console that go
// beyond the policy engine contract: named records, documents validated
// from their JSON text (errors located by line and column) and the
// simulator.
type PolicyManager interface {
	// List lists the named policies (built-in and stored) sorted by name,
	// one page at a time; the route authorizes admin:ListUserPolicies.
	List(ctx context.Context, page Page) (*Paged[policy.Named], error)
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
	// SourceIP and SecureTransport describe the client of the simulated
	// request (aws:SourceIp, aws:SecureTransport), with the definition of
	// the condition keys the S3 and Admin APIs use
	// (policy.RequestConditions); "" and nil leave the key unset.
	SourceIP        string
	SecureTransport *bool
	// Conditions are the other condition values (aws:Referer,
	// s3:prefix...); the client keys above take precedence.
	Conditions map[string][]string
	// Now is the evaluation time, which gives aws:CurrentTime and
	// aws:EpochTime (zero: the clock).
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
// domain.ErrInvalidArgument). Sensitive values (tokens, keys) are masked
// in the values it returns, except in Export, and a masked value written
// back leaves the stored secret unchanged.
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

// --- cluster export/import ---------------------------------------------------

// ClusterManager exports and imports the state of a deployment besides its
// objects (S-OPS-15, docs/08 §8.2): the IAM state (users, groups, access
// keys, STS credentials and their policy attachments), the named
// policies, the dynamic configuration and the settings of every bucket
// (versioning, object lock, policy, lifecycle, encryption, quota, tags,
// CORS, notifications), in one versioned zip archive. With `nexoctl
// mirror` for the objects it migrates a deployment to another mode
// (SNSD → cluster) or restores a backup. The IAM document of the archive
// is encrypted with a key derived from the root secret key: the target
// must share the root credentials of the source. The archive also holds
// the secrets of the configuration: it is as sensitive as the root
// credentials.
type ClusterManager interface {
	// Export returns the archive.
	Export(ctx context.Context) ([]byte, error)
	// Import validates the whole archive (format, version, policies,
	// configuration, bucket names), then, unless opts.DryRun, applies it:
	// configuration, named policies, IAM, bucket settings (the missing
	// buckets are created). Malformed archives are
	// domain.ErrInvalidArgument.
	Import(ctx context.Context, archive []byte, opts ImportOptions) (ImportSummary, error)
}

// ImportMode is what an import does with the state the archive does not
// hold.
type ImportMode string

// Import modes.
const (
	// ImportMerge (the default) adds the archived state and replaces its
	// homonyms; the rest of the deployment is kept.
	ImportMerge ImportMode = "merge"
	// ImportReplace also replaces the whole configuration and removes the
	// users, groups and named policies the archive does not hold, and the
	// settings of the archived buckets it does not set. Buckets are never
	// removed.
	ImportReplace ImportMode = "replace"
)

// ImportOptions tune ClusterManager.Import.
type ImportOptions struct {
	Mode ImportMode
	// DryRun validates the archive and reports what the import would do,
	// without changing anything.
	DryRun bool
}

// ImportSummary is the outcome of an import (or of its dry run): the
// counts of the archived state and what the import creates and removes.
type ImportSummary struct {
	Mode             ImportMode
	DryRun           bool
	Users            int
	Groups           int
	Policies         int
	ConfigSubsystems int
	Buckets          int
	// BucketsCreated are the archived buckets this deployment did not
	// have.
	BucketsCreated []string
	// Removed are the entries a replace import removes ("user:<name>",
	// "group:<name>", "policy:<name>").
	Removed []string
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
	// Applied reports a key the server applies, on the next request
	// (settings.Applied); the others are validated and stored but have no
	// effect yet, and the console and nexoctl say so.
	Applied bool
}

// --- KMS -------------------------------------------------------------------

// KMSManager is the key management of the Admin API (/kms): the state of
// the KMS of the server-side encryption (docs/07 §7.2) and its keys.
// Without a configured KMS the key operations fail with
// domain.ErrKMSNotConfigured (501 not_implemented); an invalid key name is
// domain.ErrInvalidArgument, an existing one domain.ErrKMSKeyExists (409).
type KMSManager interface {
	// Status reports the state of the KMS (its backend, default key and
	// reachability).
	Status(ctx context.Context) (kms.Status, error)
	// ListKeys lists the keys whose name starts with prefix.
	ListKeys(ctx context.Context, prefix string) ([]kms.KeyInfo, error)
	// CreateKey creates a named key and returns it.
	CreateKey(ctx context.Context, name string) (kms.KeyInfo, error)
	// Reseal re-seals the data keys of the SSE-S3 and SSE-KMS objects of
	// bucket (every bucket when empty) under the current master key of the
	// KMS, after a rotation of the master key (docs/07 §7.2): once done,
	// the previous master keys can be removed. It needs the object layer
	// (domain.ErrNotImplemented without).
	Reseal(ctx context.Context, bucket string) (ResealReport, error)
}

// ResealReport is the outcome of KMSManager.Reseal.
type ResealReport struct {
	// Buckets counts the buckets walked.
	Buckets int
	// Versions counts the SSE-S3 and SSE-KMS versions met, Uploads the
	// pending multipart uploads of such objects; Resealed counts the
	// versions and uploads re-sealed, Failed those that could not be
	// (FirstError tells why).
	Versions   int
	Uploads    int
	Resealed   int
	Failed     int
	FirstError string
}

// --- access rules ------------------------------------------------------------

// AccessRuleManager is the access rules of the buckets in the Admin API
// (docs/10 §10.1 and §10.5: /buckets/{bucket}/access-rules, the presets,
// the tester and the block public access): accessrule.Service with the
// changes attributed to the requester (audit) and the principal of the
// tester resolved through IAM. The routes authorize
// admin:ManageAccessRules on the bucket (on the deployment for the
// cluster-level block public access); Test authorizes itself.
type AccessRuleManager interface {
	// Get returns the rules of a bucket with their ETag (an empty list
	// when none; domain.ErrBucketNotFound).
	Get(ctx context.Context, bucket string) (*accessrule.Config, error)
	// Put validates the rules and replaces those of the bucket atomically:
	// with IfMatch, only when the current rules have that ETag
	// (domain.ErrPreconditionFailed otherwise). A rejected rule is an
	// *accessrule.RuleError locating it (index, field, position in the
	// pattern).
	Put(ctx context.Context, in PutAccessRulesInput) (*accessrule.Config, error)
	// Test evaluates requests of a principal on keys of the bucket with
	// every source of the policy engine (accessrule.Service.Test: the
	// matching rules, the outcome of the rules and the final decision
	// with the determining rule). It authorizes admin:ManageAccessRules
	// on the bucket and, to evaluate the requests of another identity
	// than the requester's own, admin:SimulatePolicy.
	Test(ctx context.Context, in TestAccessRulesInput) (*accessrule.TestResult, error)
	// Public reports whether the bucket is public through its rules, and
	// with which preset.
	Public(ctx context.Context, bucket string) (*accessrule.PublicStatus, error)
	// ApplyPreset replaces the rules generated by the presets with those
	// of a preset (accessrule.Service.ApplyPreset); public-write needs
	// Confirm (accessrule.ErrConfirmationRequired).
	ApplyPreset(ctx context.Context, in AccessPresetInput) (*accessrule.Config, error)
	// BlockPublic returns the block public access of a bucket, or of the
	// cluster when bucket is "".
	BlockPublic(ctx context.Context, bucket string) (*accessrule.BlockPublic, error)
	// SetBlockPublic turns the block public access of a bucket, or of the
	// cluster (Bucket "", with its mode), on or off.
	SetBlockPublic(ctx context.Context, in SetBlockPublicInput) (*accessrule.BlockPublic, error)
}

// PrincipalAnonymous names the anonymous requester in
// TestAccessRulesInput.Principal (as "" and "*" do).
const PrincipalAnonymous = "anonymous"

// PutAccessRulesInput are the parameters of AccessRuleManager.Put.
type PutAccessRulesInput struct {
	Requester Requester
	Bucket    string
	Rules     []accessrule.Rule
	// IfMatch, when set, is the ETag the current rules must have.
	IfMatch string
}

// TestAccessRulesInput describes the requests of AccessRuleManager.Test.
type TestAccessRulesInput struct {
	Requester Requester
	Bucket    string
	// Keys are the keys to test (the listing prefixes of a listing
	// action); none tests the bucket itself.
	Keys []string
	// Action is an S3 action or an action class (read, list, write,
	// delete, tag); default read.
	Action string
	// Principal makes the requests: "", PrincipalAnonymous or "*" for an
	// anonymous request; "user:<name>", "accesskey:<key>" or a bare
	// access key for the identity of those credentials (groups and
	// policies resolved by IAM); "distribution:<id>" for an anonymous
	// request served by that distribution.
	Principal string
	// IP is the client address, Host the host the request is addressed
	// to, Method the HTTP method ("" derives it from the action), Secure
	// the transport (nil: unknown), Referer and Headers the request
	// headers.
	IP      string
	Host    string
	Method  string
	Secure  *bool
	Referer string
	Headers map[string]string
	// Distribution is the distribution serving the request (set by a
	// "distribution:<id>" principal too).
	Distribution string
	// Time is the time of the request (default: now).
	Time time.Time
}

// AccessPresetInput are the parameters of AccessRuleManager.ApplyPreset.
type AccessPresetInput struct {
	Requester Requester
	Bucket    string
	Preset    accessrule.Preset
	// Match is the pattern of public-prefix (required) and of
	// public-write (optional).
	Match *match.Pattern
	// Confirm acknowledges public-write.
	Confirm bool
	// IfMatch, when set, is the ETag the current rules must have.
	IfMatch string
}

// SetBlockPublicInput are the parameters of
// AccessRuleManager.SetBlockPublic.
type SetBlockPublicInput struct {
	Requester Requester
	// Bucket is "" for the cluster level.
	Bucket  string
	Enabled bool
	// Mode sets the mode of the deployment (cluster level only; "" keeps
	// it).
	Mode accessrule.BlockPublicMode
}

// --- distributions -----------------------------------------------------------

// DistributionManager is the distributions in the Admin API (docs/10 §10.2
// and §10.5: /distributions, /keygroups, /routing/resolve):
// distribution.Service with the URL of a test or of a signature completed
// from the aliases of the distribution, the private key of a signature
// matched against the key group of the behavior, and the creation of a key
// group with its first key pair. The routes authorize
// admin:ManageDistributions (admin:SignDistributionURL for the
// signatures); Create and Update also authorize admin:ManageDistributions
// on the origin bucket, so that a policy restricting the action to some
// buckets (Resource) restricts the buckets an operator may expose.
type DistributionManager interface {
	// List returns every distribution, sorted by id.
	List(ctx context.Context) ([]distribution.Distribution, error)
	// Get returns a distribution with its ETag
	// (distribution.ErrDistributionNotFound).
	Get(ctx context.Context, id string) (distribution.Distribution, error)
	// Create stores a new distribution (domain.ErrConflict when the id or
	// an alias is taken: distribution.ErrAliasConflict).
	Create(ctx context.Context, in SaveDistributionInput) (distribution.Distribution, error)
	// Update replaces a distribution; a non-empty IfMatch must equal its
	// ETag (domain.ErrPreconditionFailed).
	Update(ctx context.Context, in SaveDistributionInput) (distribution.Distribution, error)
	// Delete removes a distribution; the requester must be allowed to
	// manage the distributions of its origin bucket (a user denied on a
	// bucket cannot free the aliases of its distribution for another one).
	Delete(ctx context.Context, req Requester, id string) error
	// Test reports how the distribution would serve a URL: the behavior,
	// the access, the object, the redirection or refusal and the response
	// headers. A URL whose host the distribution does not serve is
	// domain.ErrInvalidArgument.
	Test(ctx context.Context, in TestDistributionInput) (distribution.TestResult, error)
	// RotateOriginSecret generates a new origin secret, returned in clear
	// once, keeping the most recent previous ones active; the requester
	// must be allowed on the origin bucket.
	RotateOriginSecret(ctx context.Context, req Requester, in distribution.RotateSecretInput) (*RotatedOriginSecret, error)
	// Sign signs a URL, or the cookies of a resource, of a signed behavior
	// of the distribution with a private key of its key group (the server
	// never stores the private keys: the caller provides it).
	Sign(ctx context.Context, in SignDistributionInput) (*SignedDistributionRequest, error)
	// Resolve reports what serves a host (diagnostics of the routing
	// table of the answering node).
	Resolve(ctx context.Context, host string) (distribution.Route, error)

	// ListKeyGroups returns every key group, sorted by id.
	ListKeyGroups(ctx context.Context) ([]distribution.KeyGroup, error)
	// GetKeyGroup returns a key group (distribution.ErrKeyGroupNotFound).
	GetKeyGroup(ctx context.Context, id string) (distribution.KeyGroup, error)
	// CreateKeyGroup creates a key group and, with Key, generates its first
	// key pair (the group is removed when the generation fails).
	CreateKeyGroup(ctx context.Context, in CreateKeyGroupInput) (*CreatedKeyGroup, error)
	// DeleteKeyGroup removes a key group no distribution references
	// (domain.ErrConflict otherwise).
	DeleteKeyGroup(ctx context.Context, req Requester, id string) error
	// AddKey adds a public key provided by the user to a key group, or
	// generates a key pair without one. The keys of a group sign for
	// every distribution that uses it: the requester must be allowed on
	// the origin bucket of each of them.
	AddKey(ctx context.Context, in AddKeyInput) (*AddedKey, error)
	// DeleteKey removes a key from its group (authorized like AddKey).
	DeleteKey(ctx context.Context, req Requester, group, keyID string) error
}

// SaveDistributionInput are the parameters of DistributionManager.Create
// and Update.
type SaveDistributionInput struct {
	Requester    Requester
	Distribution distribution.Distribution
	// IfMatch (Update) is the ETag the stored distribution must have; ""
	// replaces it unconditionally.
	IfMatch string
}

// TestDistributionInput are the parameters of DistributionManager.Test.
type TestDistributionInput struct {
	ID string
	// URL is the URL to test; a path ("/assets/app.js") is completed with
	// https and the first exact alias of the distribution.
	URL string
	// Method defaults to GET.
	Method string
	// ClientIP is the address of the viewer (restrictions).
	ClientIP string
}

// RotatedOriginSecret is the outcome of
// DistributionManager.RotateOriginSecret.
type RotatedOriginSecret struct {
	// Secret is the new secret in clear: the only time it is shown.
	Secret string
	// Header is the header the proxies must send it in.
	Header       string
	Distribution distribution.Distribution
}

// SignDistributionInput are the parameters of DistributionManager.Sign.
type SignDistributionInput struct {
	ID string
	// URL is the resource to sign: an absolute URL whose host the
	// distribution serves, or a path completed with Scheme and Host.
	URL string
	// Scheme (default https) and Host (default: the first exact alias)
	// complete a path.
	Scheme string
	Host   string
	// PrivateKeyPEM is the private key (PKCS #8, or PKCS #1 for RSA) of a
	// key of the key group of the behavior serving the URL.
	PrivateKeyPEM string
	// KeyPairID names the public key of the private key; "" finds it by
	// matching the private key against the keys of the group.
	KeyPairID string
	// Expires is when the signature expires, or ExpiresIn how long from
	// now (one of them is required).
	Expires   time.Time
	ExpiresIn time.Duration
	// NotBefore, IPRange and Resource (a URL with "*" and "?" wildcards)
	// make a custom policy.
	NotBefore time.Time
	IPRange   string
	Resource  string
	// Cookies signs cookies instead of the URL.
	Cookies bool
}

// SignedDistributionRequest is the outcome of DistributionManager.Sign.
type SignedDistributionRequest struct {
	// URL is the signed URL (the URL itself, unsigned, with Cookies).
	URL string
	// Cookies are the signed cookies (with SignDistributionInput.Cookies).
	Cookies []distribution.SignedCookie
	// KeyGroup and KeyPairID name the key that signed.
	KeyGroup  string
	KeyPairID string
	Expires   time.Time
}

// CreateKeyGroupInput are the parameters of
// DistributionManager.CreateKeyGroup.
type CreateKeyGroupInput struct {
	ID      string
	Comment string
	// Key generates the first key pair of the group; nil creates an empty
	// group.
	Key *GenerateKeySpec
}

// GenerateKeySpec describes a key pair to generate.
type GenerateKeySpec struct {
	// Algorithm is ed25519 (default) or rsa.
	Algorithm distribution.KeyAlgorithm
	// Bits is the size of an RSA key (2048 by default, or 4096).
	Bits    int
	Comment string
}

// CreatedKeyGroup is the outcome of DistributionManager.CreateKeyGroup.
type CreatedKeyGroup struct {
	KeyGroup distribution.KeyGroup
	// Key is the generated key pair, its private key returned once (nil
	// without CreateKeyGroupInput.Key).
	Key *distribution.GeneratedKey
}

// AddKeyInput are the parameters of DistributionManager.AddKey.
type AddKeyInput struct {
	Requester Requester
	KeyGroup  string
	// PublicKeyPEM adds a public key provided by the user (PKIX "PUBLIC
	// KEY", Ed25519 or RSA); without it a key pair is generated with
	// Algorithm and Bits.
	PublicKeyPEM string
	Algorithm    distribution.KeyAlgorithm
	Bits         int
	Comment      string
}

// AddedKey is the outcome of DistributionManager.AddKey.
type AddedKey struct {
	PublicKey distribution.PublicKey
	// PrivateKeyPEM is the private key of a generated key pair, returned
	// once; "" for a public key provided by the user.
	PrivateKeyPEM string
}

// --- certificates ------------------------------------------------------------

// CertificateManager is the TLS certificates of the domains of the
// distributions in the Admin API (docs/10 §10.2, /certificates): the
// certificates with the distributions using them, the state of the ACME
// certificates, the upload of a PEM certificate and the deletion of an
// unused one. The routes authorize admin:ManageCertificates.
type CertificateManager interface {
	// List returns the stored certificates (sorted by id) and the ACME
	// certificates of the distributions, obtained or pending.
	List(ctx context.Context) (*CertificateList, error)
	// Get returns a certificate (certs.ErrCertificateNotFound).
	Get(ctx context.Context, id string) (*CertificateInfo, error)
	// Upload validates and stores a certificate, replacing the one of the
	// same id (certs.ErrInvalidCertificate).
	Upload(ctx context.Context, in certs.UploadInput) (*CertificateInfo, error)
	// Delete removes a certificate no distribution uses
	// ("upload:<id>": domain.ErrConflict).
	Delete(ctx context.Context, id string) error
}

// CertificateInfo is a certificate with the distributions using it.
type CertificateInfo struct {
	certs.Certificate
	// UsedBy are the ids of the distributions whose TLS names the
	// certificate ("upload:<id>", or "auto" for the ACME certificate of
	// their aliases), sorted.
	UsedBy []string
}

// CertificateList is the outcome of CertificateManager.List.
type CertificateList struct {
	Items []CertificateInfo
	ACME  ACMEStatus
}

// ACMEStatus is the state of the ACME certificates.
type ACMEStatus struct {
	// Directory is the ACME directory, Challenge the challenge type
	// (http-01, tls-alpn-01).
	Directory string
	Challenge string
	// Managed are the certificates ACME obtains and renews: one per
	// enabled distribution with tls.certificate "auto".
	Managed []ManagedCertificate
}

// ACME states of a ManagedCertificate.
const (
	// ACMEIssued: the certificate was obtained.
	ACMEIssued = "issued"
	// ACMEPending: not obtained yet (the leader orders it; a failed order
	// is retried).
	ACMEPending = "pending"
)

// ManagedCertificate is the ACME certificate of distributions.
type ManagedCertificate struct {
	// ID is the id of the certificate.
	ID      string
	Domains []string
	// Distributions are the distributions it serves.
	Distributions []string
	// State is ACMEIssued or ACMEPending.
	State string
	// Certificate is the stored certificate (State ACMEIssued): its
	// status, expiration and the last failure of a renewal.
	Certificate *certs.Certificate
}

// --- cache -------------------------------------------------------------------

// CacheManager is the cache of the distributions in the Admin API
// (docs/11 §11.11: /cache, /distributions/{id}/cache,
// /distributions/{id}/invalidations): cache.Manager and cache.Operations
// with the changes attributed to the requester, the URLs of the
// invalidations and of the inspector completed from the domains of their
// distribution, an invalidation found by its id alone and the
// distributions checked before their history and statistics are read.
// The routes authorize admin:ManageCache (configuration, webhooks,
// inspector, statistics) and admin:InvalidateCache (invalidations and
// warm-ups).
type CacheManager interface {
	// ListPolicies returns the built-in and defined policies, by name.
	ListPolicies(ctx context.Context) ([]cache.Policy, error)
	// GetPolicy returns a policy (cache.ErrPolicyNotFound).
	GetPolicy(ctx context.Context, name string) (cache.Policy, error)
	// CreatePolicy stores a new policy (domain.ErrConflict when the name is
	// taken or built-in).
	CreatePolicy(ctx context.Context, p cache.Policy) (cache.Policy, error)
	// UpdatePolicy replaces a defined policy; a non-empty ifMatch must equal
	// its ETag (domain.ErrPreconditionFailed).
	UpdatePolicy(ctx context.Context, p cache.Policy, ifMatch string) (cache.Policy, error)
	// DeletePolicy removes a defined policy no level references
	// (cache.ErrPolicyInUse).
	DeletePolicy(ctx context.Context, name string) error
	// PreviewPolicy validates a policy and computes the Cache-Control sent
	// to the clients, without storing it (D-CCH-03).
	PreviewPolicy(ctx context.Context, p cache.Policy) (cache.PolicyPreview, error)

	// GetSettings and UpdateSettings read and replace the settings of the
	// cluster (the global level).
	GetSettings(ctx context.Context) (cache.GlobalConfig, error)
	UpdateSettings(ctx context.Context, g cache.GlobalConfig, ifMatch string) (cache.GlobalConfig, error)

	// GetDistribution returns the cache settings of a distribution (its
	// defaults, the overrides of its domains and paths, its webhooks), an
	// empty document when it has none
	// (distribution.ErrDistributionNotFound for an unknown distribution).
	GetDistribution(ctx context.Context, id string) (cache.DistributionConfig, error)
	// UpdateDistribution replaces them; a non-empty ifMatch must equal
	// their ETag. The webhooks given with masked secrets keep the stored
	// ones.
	UpdateDistribution(ctx context.Context, cfg cache.DistributionConfig, ifMatch string) (cache.DistributionConfig, error)
	// Effective returns the effective policy of a path of a domain of a
	// distribution, with the level each setting comes from (TCC-01).
	Effective(ctx context.Context, in cache.EffectiveInput) (cache.EffectivePolicy, error)

	// Invalidate applies a manual invalidation on every node, attributed
	// to the requester. Paths may be absolute URLs on a domain of the
	// distribution: their host sets the domain of the invalidation.
	Invalidate(ctx context.Context, in InvalidateCacheInput) (cache.Invalidation, error)
	// GetInvalidation returns an invalidation of a distribution ("" for the
	// global purges) with its state on every node
	// (cache.ErrInvalidationNotFound).
	GetInvalidation(ctx context.Context, distribution, id string) (cache.Invalidation, error)
	// FindInvalidation returns an invalidation by its id alone, global or
	// of any distribution.
	FindInvalidation(ctx context.Context, id string) (cache.Invalidation, error)
	// ListInvalidations returns the history of a distribution ("" for the
	// global purges), the newest first.
	ListInvalidations(ctx context.Context, distribution string) ([]cache.Invalidation, error)

	// Warm starts a warm-up of a domain of a distribution, attributed to
	// the requester, and returns at once.
	Warm(ctx context.Context, in WarmCacheInput) (cache.WarmJob, error)
	// GetWarmJob returns a warm-up with its progress on every node
	// (cache.ErrWarmJobNotFound).
	GetWarmJob(ctx context.Context, distribution, id string) (cache.WarmJob, error)

	// Inspect reports how the cache serves a URL: the key, the effective
	// policy with the level of each setting and the entry on every node
	// (D-CCH-05).
	Inspect(ctx context.Context, in InspectCacheInput) (cache.InspectResult, error)
	// Stats returns the statistics of the cluster, of a distribution or of
	// one of its domains.
	Stats(ctx context.Context, q cache.StatsQuery) (cache.StatsReport, error)

	// Purge webhooks of a distribution (secrets masked).
	ListWebhooks(ctx context.Context, distribution string) ([]cache.Webhook, error)
	AddWebhook(ctx context.Context, distribution string, w cache.Webhook) (cache.Webhook, error)
	DeleteWebhook(ctx context.Context, distribution, id string) error
	// TestWebhook calls a webhook once, outside of its queue.
	TestWebhook(ctx context.Context, distribution, id string) (cache.WebhookTestResult, error)
}

// InvalidateCacheInput are the parameters of CacheManager.Invalidate.
type InvalidateCacheInput struct {
	Requester Requester
	// Request is the invalidation; its Distribution is "" for a global
	// purge (All only) and its Actor is set from the requester.
	Request cache.InvalidationRequest
}

// WarmCacheInput are the parameters of CacheManager.Warm.
type WarmCacheInput struct {
	Requester Requester
	// Request is the warm-up; its Actor is set from the requester.
	Request cache.WarmRequest
}

// InspectCacheInput are the parameters of CacheManager.Inspect.
type InspectCacheInput struct {
	// URL is an absolute http(s) URL, or a path of Distribution completed
	// with https and its first exact alias.
	URL string
	// Distribution, when set, is the distribution the URL must be served
	// by.
	Distribution string
	// Header are the request headers of the key (Accept-Encoding, the
	// headers and cookies of the cache key, Authorization).
	Header http.Header
}

// --- diagnostics (S-OPS-11) ------------------------------------------------

// ProfileType is a runtime profile of the server (pprof).
type ProfileType string

// Profile types.
const (
	// ProfileCPU samples the CPU for the duration of the profile.
	ProfileCPU ProfileType = "cpu"
	// ProfileHeap is the live heap at the end of the request.
	ProfileHeap ProfileType = "heap"
	// ProfileGoroutine are the stacks of every goroutine.
	ProfileGoroutine ProfileType = "goroutine"
	// ProfileMutex and ProfileBlock sample the contended mutexes and the
	// blocking events for the duration of the profile (their sampling is
	// off otherwise).
	ProfileMutex ProfileType = "mutex"
	ProfileBlock ProfileType = "block"
)

// Limits of the profiles: the sampled ones (cpu, mutex, block) last
// between MinProfileDuration and MaxProfileDuration, DefaultProfileDuration
// when unset.
const (
	MinProfileDuration     = time.Second
	DefaultProfileDuration = 10 * time.Second
	MaxProfileDuration     = 2 * time.Minute
)

// ProfileInput are the parameters of Diagnostics.Profile.
type ProfileInput struct {
	Type ProfileType
	// Duration of a sampled profile (0: DefaultProfileDuration); ignored
	// by the heap and goroutine snapshots.
	Duration time.Duration
}

// Profile is a profile of the node that served the request, in the pprof
// format (gzipped protocol buffer, `go tool pprof`).
type Profile struct {
	Type     ProfileType
	Node     string
	Duration time.Duration
	Data     []byte
}

// SpeedtestKind selects what a speed test measures.
type SpeedtestKind string

// Speed tests.
const (
	// SpeedtestDrive writes then reads files on every local drive of the
	// node (its temporary area), the drives in parallel.
	SpeedtestDrive SpeedtestKind = "drive"
	// SpeedtestObject uploads then downloads objects through the object
	// layer (erasure coding, quorum, internode RPC in a cluster) in a
	// temporary bucket, removed afterwards.
	SpeedtestObject SpeedtestKind = "object"
	// SpeedtestNet uploads files to a drive of every other node of a
	// cluster over the internode RPC, then reads one of them back
	// repeatedly (served from the page cache of the peer): the network
	// between the nodes. Not available out of a cluster.
	SpeedtestNet SpeedtestKind = "net"
)

// Limits of the speed tests (SpeedtestInput).
const (
	DefaultSpeedtestSize        = 4 << 20
	MaxSpeedtestSize            = 64 << 20
	DefaultSpeedtestDuration    = 5 * time.Second
	MaxSpeedtestDuration        = time.Minute
	DefaultSpeedtestConcurrency = 8
	MaxSpeedtestConcurrency     = 64
	// MaxSpeedtestBytes bounds the data a test writes per target (drive,
	// node or the object test as a whole) whatever its duration.
	MaxSpeedtestBytes = 1 << 30
)

// SpeedtestInput are the parameters of Diagnostics.Speedtest.
type SpeedtestInput struct {
	Kind SpeedtestKind
	// Size of each file or object (0: DefaultSpeedtestSize; at most
	// MaxSpeedtestSize).
	Size int64
	// Duration of each phase, write then read (0:
	// DefaultSpeedtestDuration; at most MaxSpeedtestDuration).
	Duration time.Duration
	// Concurrency of the object test and of the reads of the net test
	// (0: DefaultSpeedtestConcurrency; at most MaxSpeedtestConcurrency).
	Concurrency int
}

// SpeedtestResult is the outcome of a speed test.
type SpeedtestResult struct {
	Kind        SpeedtestKind     `json:"kind"`
	Node        string            `json:"node"`
	Size        int64             `json:"size"`
	Duration    time.Duration     `json:"duration"`
	Concurrency int               `json:"concurrency"`
	Targets     []SpeedtestTarget `json:"targets"`
}

// SpeedtestTarget is the measure of one target: a drive, a peer node or
// the object layer.
type SpeedtestTarget struct {
	Target string           `json:"target"`
	Write  SpeedtestMeasure `json:"write"`
	Read   SpeedtestMeasure `json:"read"`
	Error  string           `json:"error,omitempty"`
}

// SpeedtestMeasure is the throughput of a phase.
type SpeedtestMeasure struct {
	Bytes      int64         `json:"bytes"`
	Operations int64         `json:"operations"`
	Elapsed    time.Duration `json:"elapsed"`
	// BytesPerSecond and OperationsPerSecond are computed over Elapsed.
	BytesPerSecond      float64 `json:"bytesPerSecond"`
	OperationsPerSecond float64 `json:"operationsPerSecond"`
	// LatencyP50 and LatencyP99 are the latencies of the operations.
	LatencyP50 time.Duration `json:"latencyP50"`
	LatencyP99 time.Duration `json:"latencyP99"`
}

// Diagnostics profiles the server and measures its drives, its object
// layer and the network between its nodes (S-OPS-11). Both run on the
// node that serves the request, one at a time per kind
// (domain.ErrConflict while one is running); the inputs out of their
// limits are domain.ErrInvalidArgument. The Admin API exposes them to the
// identities allowed admin:Profiling and admin:Speedtest (root, the
// diagnostics policy).
type Diagnostics interface {
	// Profile collects a runtime profile.
	Profile(ctx context.Context, in ProfileInput) (*Profile, error)
	// Speedtest runs a speed test and removes what it wrote
	// (domain.ErrNotImplemented: SpeedtestNet out of a cluster).
	Speedtest(ctx context.Context, in SpeedtestInput) (*SpeedtestResult, error)
}
