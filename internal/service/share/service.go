// Package share manages revocable share links (docs/09 §9.1 "Revoking shared
// links"). A presigned SigV4 URL cannot be revoked on its own, so
// every managed link is signed by a dedicated service account "share-<id>",
// child of the user who shares, whose inline policy allows only the shared
// action (s3:GetObject or s3:PutObject) on the shared object or prefix and
// whose expiration is the link's. Revoking the link deletes the service
// account: every URL it signed stops working immediately, and permissions
// are still evaluated at use time with the parent's policies (disabling the
// user, or removing its policies, disables its links too).
//
// Raw, non-revocable presigned URLs remain available through auth.Signer.
package share

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Deps are the dependencies of the share service.
type Deps struct {
	// IAM creates and deletes the service accounts signing the links
	// (required).
	IAM iam.Service
	// Signer presigns the URLs (required).
	Signer auth.Signer
	// Store persists the share records in iam.NamespaceShares (default:
	// iam.NewMemoryStore()).
	Store iam.Store
	// Policy, when set, checks at creation that the owner may perform the
	// shared action on the shared resource. It is required to share with
	// derived credentials (service accounts, STS), whose inline policy
	// does not carry over to the link.
	Policy policy.Service
	Clock  clock.Clock
	Logger *slog.Logger
}

// Service manages the share links. Root (not its derived credentials) sees
// and manages every share; any other caller only the shares it owns, i.e.
// whose Owner is its effective user. Shares of others are reported as
// domain.ErrShareNotFound. Expired shares are not listed and are deleted,
// with their service accounts, by PurgeExpired (and opportunistically by
// Create).
type Service interface {
	// Create creates a share link: the service account, the presigned URL
	// (with the public host of WithServerURL) and the persisted record.
	Create(ctx context.Context, in CreateInput) (*Share, error)
	// Get returns a share visible to the caller.
	Get(ctx context.Context, caller domain.Identity, id string) (*Share, error)
	// List returns the active shares visible to the caller that match the
	// filter, newest first. A non-root caller naming another owner is
	// domain.ErrAccessDenied.
	List(ctx context.Context, caller domain.Identity, filter ListFilter) ([]Share, error)
	// Presign signs a URL for key with the share's service account and the
	// remaining validity: any key under the prefix of a prefix share, or
	// the shared object ("" or its key) of an object share. It is how a
	// prefix share yields one URL per object, all revoked together.
	Presign(ctx context.Context, caller domain.Identity, id, key string) (*url.URL, error)
	// Revoke deletes the share and its service account: the link stops
	// working immediately.
	Revoke(ctx context.Context, caller domain.Identity, id string) error
	// RevokeAll revokes every share of owner (empty: the caller's
	// effective user) and returns how many were revoked. Only root may
	// name another owner.
	RevokeAll(ctx context.Context, caller domain.Identity, owner string) (int, error)
	// RevokeIssuedBy revokes the shares created with accessKey (a service
	// account or temporary credentials of their owner) and the share whose
	// own service account is accessKey, and returns how many were revoked.
	// It is the cleanup of a disabled access key, run on behalf of the
	// system: there is no caller to authorize.
	RevokeIssuedBy(ctx context.Context, accessKey string) (int, error)
	// PurgeExpired deletes the expired shares and their service accounts
	// and returns how many were deleted.
	PurgeExpired(ctx context.Context) (int, error)
}

// Permission is what a share link allows.
type Permission string

// Share permissions.
const (
	// PermissionDownload allows s3:GetObject (and, for prefix shares,
	// listing the prefix).
	PermissionDownload Permission = "download"
	// PermissionUpload allows s3:PutObject.
	PermissionUpload Permission = "upload"
)

// CreateInput are the parameters of Create.
type CreateInput struct {
	// Owner is the authenticated identity sharing the resource; the link
	// belongs to its effective user.
	Owner domain.Identity
	// Bucket is the shared bucket.
	Bucket string
	// Key is the object key or, with Prefix, the key prefix ("" shares the
	// whole bucket). Keys and prefixes containing "*", "?" or "${" cannot
	// be shared (they would be policy wildcards): use a raw presigned URL.
	Key    string
	Prefix bool
	// Permission is download or upload.
	Permission Permission
	// Expires is the validity, from 1 second to 7 days; zero means 7 days.
	// It is capped at the expiration of the owner's STS session.
	Expires time.Duration
	// ContentDisposition sets the Content-Disposition of downloads
	// (response-content-disposition).
	ContentDisposition string
	// ContentType sets the Content-Type of downloads
	// (response-content-type); for uploads it is a signed header the
	// uploader must send.
	ContentType string
	// VersionID pins an object download to a version.
	VersionID string
	// Stream shares a video as an HLS stream: the link is the master
	// playlist of the object (domain.MediaMasterPath), which the share
	// signs like a download. Object download shares only, without
	// overrides.
	Stream bool
}

// ListFilter selects shares; empty fields match everything.
type ListFilter struct {
	Owner  string
	Bucket string
}

// Share is a managed share link.
type Share struct {
	ID string
	// Owner is the user the link belongs to (the parent of its service
	// account).
	Owner      string
	Bucket     string
	Key        string
	Prefix     bool
	Permission Permission
	VersionID  string
	// ContentType and ContentDisposition are the overrides of CreateInput.
	ContentType        string
	ContentDisposition string
	// Stream is set on the HLS stream shares (CreateInput.Stream).
	Stream    bool
	Expires   time.Time
	CreatedAt time.Time
	// URL is the presigned URL: GET or PUT of the object for object
	// shares, a ListObjectsV2 of the prefix for prefix download shares,
	// empty for prefix upload shares (use Presign).
	URL string
	// AccessKey is the service account signing the link ("share-<id>").
	AccessKey string
}
