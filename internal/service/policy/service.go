// Package policy defines the policy engine: evaluation of IAM policies,
// bucket policies and session policies in the AWS format, plus the store of
// named IAM policies and their attachments to users and groups.
package policy

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the policy engine.
type Deps struct {
	// Store persists the named policies and their attachments (default: in memory).
	Store  PolicyStore
	Clock  clock.Clock
	Logger *slog.Logger
}

// Snapshot is the whole persisted state of the policy store.
type Snapshot struct {
	// Policies are the user-defined named policies (built-ins are not stored).
	Policies map[string]Named `json:"policies"`
	// Users and Groups map a principal to the names of its attached policies.
	Users  map[string][]string `json:"users"`
	Groups map[string][]string `json:"groups"`
}

// PolicyStore persists the policy snapshot. Implementations: in memory and,
// later, the system bucket (.nexo.sys/config/policies.json).
type PolicyStore interface {
	// Load returns the snapshot; an empty store yields empty maps.
	Load(ctx context.Context) (*Snapshot, error)
	// Save atomically replaces the snapshot.
	Save(ctx context.Context, s *Snapshot) error
}

// Action is a policy action name such as "s3:GetObject" or "admin:ServerInfo".
type Action string

// Common S3 and admin actions.
const (
	ActionAll                 Action = "s3:*"
	ActionListAllMyBuckets    Action = "s3:ListAllMyBuckets"
	ActionCreateBucket        Action = "s3:CreateBucket"
	ActionDeleteBucket        Action = "s3:DeleteBucket"
	ActionListBucket          Action = "s3:ListBucket"
	ActionListBucketVersions  Action = "s3:ListBucketVersions"
	ActionListMultipart       Action = "s3:ListBucketMultipartUploads"
	ActionGetBucketLocation   Action = "s3:GetBucketLocation"
	ActionGetBucketPolicy     Action = "s3:GetBucketPolicy"
	ActionPutBucketPolicy     Action = "s3:PutBucketPolicy"
	ActionDeleteBucketPolicy  Action = "s3:DeleteBucketPolicy"
	ActionGetBucketVersioning Action = "s3:GetBucketVersioning"
	ActionPutBucketVersioning Action = "s3:PutBucketVersioning"
	ActionGetObject           Action = "s3:GetObject"
	ActionGetObjectVersion    Action = "s3:GetObjectVersion"
	ActionPutObject           Action = "s3:PutObject"
	ActionDeleteObject        Action = "s3:DeleteObject"
	ActionDeleteObjectVersion Action = "s3:DeleteObjectVersion"
	ActionAbortMultipart      Action = "s3:AbortMultipartUpload"
	ActionListMultipartParts  Action = "s3:ListMultipartUploadParts"
	ActionGetObjectTagging    Action = "s3:GetObjectTagging"
	ActionPutObjectTagging    Action = "s3:PutObjectTagging"
	ActionDeleteObjectTagging Action = "s3:DeleteObjectTagging"

	// Admin API actions (docs/04 §4.4): one per endpoint family. Admin
	// actions are not bound to a resource; a statement matches them by name.
	ActionAdminAll             Action = "admin:*"
	ActionAdminServerInfo      Action = "admin:ServerInfo"
	ActionAdminDataUsage       Action = "admin:DataUsageInfo"
	ActionAdminHeal            Action = "admin:Heal"
	ActionAdminServiceRestart  Action = "admin:ServiceRestart"
	ActionAdminServiceStop     Action = "admin:ServiceStop"
	ActionAdminCreateUser      Action = "admin:CreateUser"
	ActionAdminListUsers       Action = "admin:ListUsers"
	ActionAdminGetUser         Action = "admin:GetUser"
	ActionAdminUpdateUser      Action = "admin:UpdateUser"
	ActionAdminDeleteUser      Action = "admin:DeleteUser"
	ActionAdminCreateGroup     Action = "admin:CreateGroup"
	ActionAdminListGroups      Action = "admin:ListGroups"
	ActionAdminGetGroup        Action = "admin:GetGroup"
	ActionAdminUpdateGroup     Action = "admin:UpdateGroup"
	ActionAdminDeleteGroup     Action = "admin:DeleteGroup"
	ActionAdminCreateAccessKey Action = "admin:CreateServiceAccount"
	ActionAdminListAccessKeys  Action = "admin:ListServiceAccounts"
	ActionAdminGetAccessKey    Action = "admin:GetServiceAccount"
	ActionAdminUpdateAccessKey Action = "admin:UpdateServiceAccount"
	ActionAdminDeleteAccessKey Action = "admin:DeleteServiceAccount"
	ActionAdminCreatePolicy    Action = "admin:CreatePolicy"
	ActionAdminListPolicies    Action = "admin:ListPolicies"
	ActionAdminGetPolicy       Action = "admin:GetPolicy"
	ActionAdminDeletePolicy    Action = "admin:DeletePolicy"
	ActionAdminAttachPolicy    Action = "admin:AttachPolicy"
	ActionAdminListBuckets     Action = "admin:ListBuckets"
	ActionAdminCreateBucket    Action = "admin:CreateBucket"
	ActionAdminGetBucket       Action = "admin:GetBucket"
	ActionAdminUpdateBucket    Action = "admin:UpdateBucket"
	ActionAdminDeleteBucket    Action = "admin:DeleteBucket"
	ActionAdminConfigRead      Action = "admin:ConfigRead"
	ActionAdminConfigWrite     Action = "admin:ConfigUpdate"
)

// Service is the policy engine and the IAM policy store.
type Service interface {
	// Evaluate decides whether the request described by args is allowed.
	// The decision combines the identity's IAM policies (directly attached
	// and via groups), the session policy and the bucket policy. Root is
	// always allowed; an explicit Deny always wins.
	Evaluate(ctx context.Context, args Args) (Decision, error)

	// Parse decodes and validates a JSON policy document.
	Parse(ctx context.Context, raw []byte) (*domain.Policy, error)
	// Validate checks a decoded policy (version, effects, actions, ARNs).
	Validate(ctx context.Context, p *domain.Policy) error

	// Named IAM policies.
	SetPolicy(ctx context.Context, name string, p *domain.Policy) error
	GetPolicy(ctx context.Context, name string) (*domain.Policy, error)
	DeletePolicy(ctx context.Context, name string) error
	ListPolicies(ctx context.Context) ([]Named, error)

	// Attach and Detach bind named policies to a user or a group.
	Attach(ctx context.Context, in AttachInput) error
	Detach(ctx context.Context, in AttachInput) error
}

// Args describe the request being authorized.
type Args struct {
	Identity *domain.Identity
	Action   Action
	Bucket   string
	// Object is empty for bucket-level actions.
	Object string
	// Conditions are the request condition values keyed by condition key
	// (e.g. "aws:SourceIp", "s3:prefix", "aws:SecureTransport").
	Conditions map[string][]string
	// IsOwner reports that the identity owns the bucket.
	IsOwner bool
	// BucketPolicy is the bucket policy to evaluate alongside IAM policies;
	// nil when the bucket has none.
	BucketPolicy *domain.Policy
	Now          time.Time
}

// Decision is the outcome of Evaluate.
type Decision struct {
	Allowed bool
	// Reason explains the decision for logs and the console.
	Reason string
	// MatchedStatement is the statement that decided, when any.
	MatchedStatement *domain.Statement
}

// Named is a stored IAM policy.
type Named struct {
	Name      string         `json:"name"`
	Policy    *domain.Policy `json:"policy"`
	UpdatedAt time.Time      `json:"updatedAt"`
	// BuiltIn marks the policies shipped with Nexo, which cannot be changed.
	BuiltIn bool `json:"builtIn,omitempty"`
}

// AttachInput are the parameters of Attach and Detach.
type AttachInput struct {
	PolicyNames []string
	// Exactly one of User and Group is set.
	User  string
	Group string
}

// Built-in policy names.
const (
	PolicyConsoleAdmin = "consoleAdmin"
	PolicyReadOnly     = "readonly"
	PolicyReadWrite    = "readwrite"
	PolicyWriteOnly    = "writeonly"
	PolicyDiagnostics  = "diagnostics"
)
