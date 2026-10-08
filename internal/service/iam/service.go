// Package iam manages identities: the root user, users, groups, service
// accounts (access keys with inline policies and expiry) and STS temporary
// credentials, plus the policies attached to users and groups.
//
// New is the implementation: it keeps the whole state in a thread-safe
// in-memory cache, loaded lazily from a Store and written through on every
// change. NewMemoryStore and NewFileStore are the Store implementations; the
// file store encrypts every entry with AES-256-GCM under a key derived with
// Argon2id from the root secret key.
//
// Credential resolution (LookupCredentials, which also makes the service an
// auth.CredentialLookup) follows these rules:
//   - root: installed by Bootstrap, never persisted, always enabled; the
//     identity has IsRoot set;
//   - users: a disabled user is domain.ErrAccessDenied; the identity carries
//     the enabled groups of the user and the policies attached to the user
//     and to those groups;
//   - service accounts: Identity.Parent is the owning user and
//     Identity.SessionPolicy the inline policy; a disabled or expired key, or
//     a disabled parent, is domain.ErrAccessDenied. Keys of root have IsRoot
//     set too: the policy engine then limits them with the inline policy
//     only;
//   - STS: the session token (an HS256 JWT signed with a key derived from
//     the root credentials) is verified and returned in
//     Credentials.SessionToken, which the request must carry; its claims
//     are in Identity.Claims and its session policy in
//     Identity.SessionPolicy.
//
// Unknown access keys are domain.ErrInvalidAccessKey.
package iam

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Deps are the dependencies of the IAM service.
type Deps struct {
	// Store persists users, groups, service accounts and STS credentials
	// (default: NewMemoryStore()).
	Store  Store
	Clock  clock.Clock
	Logger *slog.Logger
}

// Service is the identity store.
type Service interface {
	// Attachments are the policies attached to users and groups: the IAM
	// service is their source of truth and the policy engine reads them
	// (policy.Deps.Attachments). Attached returns nil for unknown
	// entities; Attach and Detach return domain.ErrUserNotFound or
	// domain.ErrGroupNotFound for them, domain.ErrInvalidArgument for root
	// or an empty list. Policy names are not checked against the policy
	// store: the engine checks them, unknown names grant nothing.
	policy.Attachments

	// Bootstrap installs the root credentials. It is called at startup and
	// is idempotent. Root is never persisted; its secret key also derives
	// the signing key of the STS session tokens and the key of Export.
	Bootstrap(ctx context.Context, root domain.Credentials) error

	// Users. Access keys are 3 to 20 characters (letters, digits and
	// "-_.@+"), secret keys 8 to 40. The root user cannot be changed
	// (domain.ErrInvalidArgument).
	CreateUser(ctx context.Context, in CreateUserInput) (*User, error)
	GetUser(ctx context.Context, accessKey string) (*User, error)
	// ListUsers returns the users sorted by access key; root is not listed.
	ListUsers(ctx context.Context) ([]User, error)
	// DeleteUser removes the user, its group memberships, its service
	// accounts and its STS credentials.
	DeleteUser(ctx context.Context, accessKey string) error
	SetUserSecret(ctx context.Context, accessKey, secretKey string) error
	// SetUserStatus enables or disables the user. A disabled user, and
	// every credential derived from it, is rejected immediately.
	SetUserStatus(ctx context.Context, accessKey string, status domain.CredentialStatus) error
	// SetUserPolicy replaces the policies attached directly to the user.
	SetUserPolicy(ctx context.Context, accessKey string, policyNames []string) error

	// Groups. Members must be existing users. A disabled group does not
	// grant its policies to its members.
	CreateGroup(ctx context.Context, in CreateGroupInput) (*Group, error)
	GetGroup(ctx context.Context, name string) (*Group, error)
	// ListGroups returns the groups sorted by name.
	ListGroups(ctx context.Context) ([]Group, error)
	DeleteGroup(ctx context.Context, name string) error
	AddGroupMembers(ctx context.Context, name string, members []string) error
	RemoveGroupMembers(ctx context.Context, name string, members []string) error
	SetGroupStatus(ctx context.Context, name string, status domain.CredentialStatus) error
	// SetGroupPolicy replaces the policies attached to the group.
	SetGroupPolicy(ctx context.Context, name string, policyNames []string) error

	// Access keys (service accounts). Every call carries the authenticated
	// caller and the service enforces the ownership rule: root manages
	// every key, a user only the keys whose ParentUser is its own access
	// key, derived credentials (service accounts, STS) and anonymous
	// callers none. Violations return domain.ErrAccessDenied. Unknown keys
	// return domain.ErrAccessKeyNotFound. The secret key is returned by
	// CreateAccessKey only.
	CreateAccessKey(ctx context.Context, in CreateAccessKeyInput) (*AccessKey, error)
	GetAccessKey(ctx context.Context, caller domain.Identity, accessKey string) (*AccessKey, error)
	// ListAccessKeys lists the keys owned by parent, sorted by access key.
	// An empty parent means the caller itself, or every user when the
	// caller is root.
	ListAccessKeys(ctx context.Context, caller domain.Identity, parent string) ([]AccessKey, error)
	UpdateAccessKey(ctx context.Context, caller domain.Identity, accessKey string, in UpdateAccessKeyInput) (*AccessKey, error)
	DeleteAccessKey(ctx context.Context, caller domain.Identity, accessKey string) error

	// LookupCredentials resolves any access key (root, user, service
	// account, STS) to its credentials and identity (see the package
	// documentation). Unknown keys return domain.ErrInvalidAccessKey;
	// disabled or expired credentials domain.ErrAccessDenied.
	LookupCredentials(ctx context.Context, accessKey string) (domain.Credentials, domain.Identity, error)

	// AssumeRole issues temporary credentials for the caller, which must
	// be root or an enabled user (derived and anonymous callers are
	// domain.ErrAccessDenied). They are persisted until they expire;
	// expired ones are purged.
	AssumeRole(ctx context.Context, in AssumeRoleInput) (*domain.Credentials, error)

	// Invalidate drops the in-memory cache: the next call reloads the
	// store. Peers call it when another node changed the IAM state. It
	// does no I/O.
	Invalidate(ctx context.Context)
	// Export returns the whole IAM state (users, groups, service accounts,
	// STS credentials, attachments; not root) as a JSON document encrypted
	// with a key derived from the root secret key.
	Export(ctx context.Context) ([]byte, error)
	// Import merges a document produced by Export with the same root
	// secret key: entries with the same key are replaced, expired STS
	// credentials are skipped. A wrong key returns ErrWrongKey, a damaged
	// document domain.ErrInvalidFormat.
	Import(ctx context.Context, data []byte) error
}

// Namespace partitions the entries of a Store.
type Namespace string

// Store namespaces.
const (
	NamespaceUsers           Namespace = "users"
	NamespaceGroups          Namespace = "groups"
	NamespaceServiceAccounts Namespace = "serviceaccounts"
	NamespaceSTS             Namespace = "sts"
	// NamespaceShares holds the managed share links (package share).
	NamespaceShares Namespace = "shares"
	// NamespaceSessions holds the revoked console sessions until they
	// expire (package session).
	NamespaceSessions Namespace = "sessions"
)

// Store is a key-value store partitioned by namespace. Values are opaque
// documents (JSON). Put and Delete are atomic: a reader sees the old or the
// new value, never a partial one. Namespaces are lowercase names
// ([a-z0-9-], 1 to 64 characters); keys are non-empty strings of at most
// 1024 bytes without NUL bytes, otherwise domain.ErrInvalidArgument.
type Store interface {
	// Get returns the value of key; a missing key returns an error
	// wrapping ErrEntryNotFound. A damaged entry is domain.ErrInvalidFormat
	// and an entry encrypted with another key ErrWrongKey.
	Get(ctx context.Context, ns Namespace, key string) ([]byte, error)
	// Put creates or replaces the value of key.
	Put(ctx context.Context, ns Namespace, key string, value []byte) error
	// Delete removes key; deleting a missing key is not an error.
	Delete(ctx context.Context, ns Namespace, key string) error
	// List returns every entry of the namespace, keyed by key.
	List(ctx context.Context, ns Namespace) (map[string][]byte, error)
}

// KDFParams are the Argon2id parameters deriving the encryption keys from
// the root secret key. The zero value means the defaults (1 pass, 64 MiB,
// 4 threads; RFC 9106 §4).
type KDFParams struct {
	// Time is the number of passes over the memory.
	Time uint32 `json:"time"`
	// MemoryKiB is the memory size in KiB.
	MemoryKiB uint32 `json:"memoryKiB"`
	// Threads is the degree of parallelism.
	Threads uint8 `json:"threads"`
}

// User is an IAM user.
type User struct {
	AccessKey string
	Status    domain.CredentialStatus
	// PolicyNames are the policies attached directly to the user.
	PolicyNames []string
	// Groups are the groups the user is a member of, sorted.
	Groups    []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateUserInput are the parameters of CreateUser.
type CreateUserInput struct {
	AccessKey string
	SecretKey string
	// Status defaults to domain.StatusEnabled.
	Status      domain.CredentialStatus
	PolicyNames []string
}

// Group is an IAM group.
type Group struct {
	Name   string
	Status domain.CredentialStatus
	// Members are the access keys of the member users, sorted.
	Members     []string
	PolicyNames []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateGroupInput are the parameters of CreateGroup. Group names are 1 to
// 128 printable characters without ",".
type CreateGroupInput struct {
	Name        string
	Members     []string
	PolicyNames []string
}

// AccessKey is a service account derived from a parent user.
type AccessKey struct {
	AccessKey string
	// SecretKey is only populated by CreateAccessKey.
	SecretKey   string
	ParentUser  string
	Name        string
	Description string
	Status      domain.CredentialStatus
	// Expiration is zero for non-expiring keys.
	Expiration time.Time
	// Policy is the inline session policy; nil means "inherit the parent".
	Policy    *domain.Policy
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateAccessKeyInput are the parameters of CreateAccessKey.
type CreateAccessKeyInput struct {
	// Caller is the authenticated identity creating the key.
	Caller domain.Identity
	// ParentUser owns the key; empty means the caller. Only root may name
	// another user (or itself, for keys of root).
	ParentUser string
	// AccessKey and SecretKey are generated when empty.
	AccessKey string
	SecretKey string
	// Name is at most 64 characters, Description at most 256.
	Name        string
	Description string
	// Expiration is zero for a key that never expires; otherwise it must
	// be in the future.
	Expiration time.Time
	// Policy is the optional inline policy restricting the key.
	Policy *domain.Policy
}

// UpdateAccessKeyInput are the parameters of UpdateAccessKey; nil fields
// are left unchanged.
type UpdateAccessKeyInput struct {
	SecretKey   *string
	Name        *string
	Description *string
	Status      *domain.CredentialStatus
	// Expiration replaces the expiration; a zero time removes it.
	Expiration *time.Time
	Policy     *domain.Policy
	// ClearPolicy removes the inline policy.
	ClearPolicy bool
}

// AssumeRoleInput are the parameters of AssumeRole.
type AssumeRoleInput struct {
	// Caller is the authenticated identity requesting the role.
	Caller domain.Identity
	// Policy is the optional session policy restricting the credentials.
	Policy *domain.Policy
	// Duration is the validity, from 15 minutes (900 s) to 12 hours
	// (43200 s); zero means one hour.
	Duration time.Duration
	// RoleSessionName is informational (2 to 64 characters when set).
	RoleSessionName string
}
