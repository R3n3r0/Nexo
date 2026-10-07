// Package policy defines the policy engine: evaluation of IAM policies,
// bucket policies and session policies in the AWS format, plus the store of
// named IAM policies.
//
//go:generate mockery --name Service --output ./mock --outpkg mock --with-expecter
package policy

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

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

	ActionAdminAll         Action = "admin:*"
	ActionAdminServerInfo  Action = "admin:ServerInfo"
	ActionAdminCreateUser  Action = "admin:CreateUser"
	ActionAdminConfigRead  Action = "admin:ConfigRead"
	ActionAdminConfigWrite Action = "admin:ConfigUpdate"
	ActionAdminHeal        Action = "admin:Heal"
)

// Service is the policy engine and the IAM policy store.
type Service interface {
	// Evaluate decides whether the request described by args is allowed.
	// The decision combines the identity's IAM policies (directly attached
	// and via groups), the session policy and the bucket policy. Root is
	// always allowed; an explicit Deny always wins.
	Evaluate(ctx context.Context, args Args) (Decision, error)

	// Parse decodes and validates a JSON policy document.
	Parse(raw []byte) (*domain.Policy, error)
	// Validate checks a decoded policy (version, effects, actions, ARNs).
	Validate(p *domain.Policy) error

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
	Name      string
	Policy    *domain.Policy
	UpdatedAt time.Time
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
