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
//     subsystems, validated before it reaches config.Service.
//
// Every implementation is created by its New* constructor from a Deps
// struct of interfaces; the transport never applies these rules itself.
package admin

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/iam"
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
	// Public reports a bucket policy granting access to everyone.
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
