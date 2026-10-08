// Package policy is the policy engine: parsing and validation of AWS-format
// policy documents, evaluation of IAM policies, group policies, inline
// (session) policies and bucket policies, the policy simulator, the
// simplified anonymous-access presets and the store of named IAM policies
// with their attachments to users and groups.
//
// # Evaluation
//
// Evaluate follows the AWS logic (docs/09 §9.3):
//
//  1. root credentials are always allowed, like MinIO: root bypasses IAM
//     and bucket policies, explicit Deny statements included. When root
//     signs with derived credentials carrying an inline policy (a service
//     account or an STS session of root) only that inline policy limits it;
//  2. an explicit Deny in any applicable statement (identity, group,
//     inline or bucket policy) denies;
//  3. anonymous requests are allowed only by bucket policy statements whose
//     principal matches everyone ("*", {"AWS": "*"}, or a NotPrincipal
//     that does not exclude everyone);
//  4. otherwise the request is allowed when an identity policy (attached to
//     the user directly or via a group) or the bucket policy allows it; for
//     identities with an inline policy (service accounts, STS) the inline
//     policy must allow it too: the effective permissions are the
//     intersection of the parent's and the inline ones;
//  5. everything else is an implicit deny.
//
// Admin actions (admin:*) are evaluated with the same logic. Statements
// match actions case-insensitively and resources case-sensitively, with
// the "*" and "?" wildcards. Policy variables (${aws:username},
// ${aws:userid}, ${aws:principaltype}, ${s3:prefix}, any request condition
// key, ${*}, ${?}, ${$} and the ${key, 'default'} form) are expanded in
// Resource, NotResource and condition values; the expanded text is always
// literal. A variable that cannot be resolved never widens a statement:
// it matches nothing in Resource and positive conditions and everything in
// NotResource and negated conditions.
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
	// Store persists the named policies and their attachments (default:
	// NewMemoryStore()).
	Store PolicyStore
	// Attachments, when set, is the source of truth of the policies
	// attached to users and groups (iam.Service implements it): the engine
	// reads attachments from it and delegates Attach and Detach to it
	// instead of using the attachment methods of Store.
	Attachments Attachments
	// Buckets resolves the bucket policy when Args.BucketPolicy is nil.
	// Optional: without it only Args.BucketPolicy is evaluated.
	Buckets BucketPolicyProvider
	Clock   clock.Clock
	Logger  *slog.Logger
}

// Attachments maintains the named policies attached to users and groups
// outside the PolicyStore (the IAM service). Policy names are not checked
// by the implementation: the engine checks them before Attach.
type Attachments interface {
	// Attached returns the policies attached to a user or a group, sorted;
	// nil when none or when the entity does not exist.
	Attached(ctx context.Context, e Entity) ([]string, error)
	// ListAttached returns every mapping of the given kind (entity name →
	// sorted policy names).
	ListAttached(ctx context.Context, kind EntityKind) (map[string][]string, error)
	// Attach adds policies to the user or the group of in.
	Attach(ctx context.Context, in AttachInput) error
	// Detach removes policies from the user or the group of in.
	Detach(ctx context.Context, in AttachInput) error
}

// BucketPolicyProvider returns the bucket policy document of a bucket, as
// stored (raw JSON): the engine parses it with ParseBucketPolicy, so that
// there is one source and one parser of bucket policies. The composition
// root adapts the bucket metadata store to it. A bucket without a policy
// returns domain.ErrPolicyNotFound (or an empty document); a missing
// bucket returns domain.ErrBucketNotFound. Both mean "no bucket policy".
type BucketPolicyProvider interface {
	GetBucketPolicy(ctx context.Context, bucket string) ([]byte, error)
}

// EntityKind is the kind of principal a named policy is attached to.
type EntityKind string

// Entity kinds.
const (
	EntityUser  EntityKind = "user"
	EntityGroup EntityKind = "group"
)

// Entity is a user or a group policies are attached to.
type Entity struct {
	Kind EntityKind
	Name string
}

// PolicyStore persists the named IAM policies and their attachments.
// Built-in policies are never stored. Implementations: NewMemoryStore (tests,
// bootstrap) and NewFileStore (one JSON document per policy in a directory).
// Policies returned by Get and List are shared with the store and must not
// be modified by the caller.
type PolicyStore interface {
	// Get returns the named policy or domain.ErrPolicyNotFound.
	Get(ctx context.Context, name string) (Named, error)
	// Set creates or replaces the policy named p.Name.
	Set(ctx context.Context, p Named) error
	// Delete removes a named policy; domain.ErrPolicyNotFound when missing.
	Delete(ctx context.Context, name string) error
	// List returns every stored policy, sorted by name.
	List(ctx context.Context) ([]Named, error)

	// Attached returns the names of the policies attached to an entity,
	// sorted; nil when none.
	Attached(ctx context.Context, e Entity) ([]string, error)
	// SetAttached replaces the policies attached to an entity; an empty
	// list removes the mapping.
	SetAttached(ctx context.Context, e Entity, names []string) error
	// ListAttached returns every mapping of the given kind (entity name →
	// sorted policy names).
	ListAttached(ctx context.Context, kind EntityKind) (map[string][]string, error)
}

// Action is a policy action name such as "s3:GetObject" or "admin:ServerInfo".
type Action string

// Common S3 and admin actions.
const (
	ActionAll                   Action = "s3:*"
	ActionListAllMyBuckets      Action = "s3:ListAllMyBuckets"
	ActionCreateBucket          Action = "s3:CreateBucket"
	ActionDeleteBucket          Action = "s3:DeleteBucket"
	ActionListBucket            Action = "s3:ListBucket"
	ActionListBucketVersions    Action = "s3:ListBucketVersions"
	ActionListMultipart         Action = "s3:ListBucketMultipartUploads"
	ActionGetBucketLocation     Action = "s3:GetBucketLocation"
	ActionGetBucketPolicy       Action = "s3:GetBucketPolicy"
	ActionPutBucketPolicy       Action = "s3:PutBucketPolicy"
	ActionDeleteBucketPolicy    Action = "s3:DeleteBucketPolicy"
	ActionGetBucketPolicyStatus Action = "s3:GetBucketPolicyStatus"
	ActionGetBucketVersioning   Action = "s3:GetBucketVersioning"
	ActionPutBucketVersioning   Action = "s3:PutBucketVersioning"
	ActionGetObject             Action = "s3:GetObject"
	ActionGetObjectVersion      Action = "s3:GetObjectVersion"
	ActionPutObject             Action = "s3:PutObject"
	ActionDeleteObject          Action = "s3:DeleteObject"
	ActionDeleteObjectVersion   Action = "s3:DeleteObjectVersion"
	ActionAbortMultipart        Action = "s3:AbortMultipartUpload"
	ActionListMultipartParts    Action = "s3:ListMultipartUploadParts"
	ActionGetObjectTagging      Action = "s3:GetObjectTagging"
	ActionPutObjectTagging      Action = "s3:PutObjectTagging"
	ActionDeleteObjectTagging   Action = "s3:DeleteObjectTagging"

	// Admin API actions (docs/04 §4.4): one per endpoint family. Admin
	// actions are bound to a resource only when the request names a bucket;
	// a statement without Resource matches them by name.
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
	// ActionAdminSimulatePolicy authorizes the policy simulator.
	ActionAdminSimulatePolicy Action = "admin:SimulatePolicy"
	// ActionAdminPrometheus authorizes the scrape of the metrics
	// endpoints with a bearer token.
	ActionAdminPrometheus Action = "admin:Prometheus"

	// Admin API actions named after the MinIO admin policy actions, used by
	// the Admin API routes of M3 (docs/04 §4.4): user and group status,
	// group membership, policy attachment, service accounts, bucket quota,
	// traces and the management of the share links of other users.
	ActionAdminEnableUser              Action = "admin:EnableUser"
	ActionAdminDisableUser             Action = "admin:DisableUser"
	ActionAdminListUserPolicies        Action = "admin:ListUserPolicies"
	ActionAdminAttachUserOrGroupPolicy Action = "admin:AttachUserOrGroupPolicy"
	ActionAdminAddUserToGroup          Action = "admin:AddUserToGroup"
	ActionAdminRemoveUserFromGroup     Action = "admin:RemoveUserFromGroup"
	ActionAdminEnableGroup             Action = "admin:EnableGroup"
	ActionAdminDisableGroup            Action = "admin:DisableGroup"
	ActionAdminRemoveServiceAccount    Action = "admin:RemoveServiceAccount"
	ActionAdminGetBucketQuota          Action = "admin:GetBucketQuota"
	ActionAdminSetBucketQuota          Action = "admin:SetBucketQuota"
	ActionAdminServerTrace             Action = "admin:ServerTrace"
	ActionAdminConsoleLog              Action = "admin:ConsoleLog"
	ActionAdminManageShares            Action = "admin:ManageShares"

	// Bucket and object configuration actions of the S3 API, shared by
	// the S3 and the Admin API transports.
	ActionGetBucketTagging           Action = "s3:GetBucketTagging"
	ActionPutBucketTagging           Action = "s3:PutBucketTagging"
	ActionGetEncryption              Action = "s3:GetEncryptionConfiguration"
	ActionPutEncryption              Action = "s3:PutEncryptionConfiguration"
	ActionGetBucketObjectLock        Action = "s3:GetBucketObjectLockConfiguration"
	ActionPutBucketObjectLock        Action = "s3:PutBucketObjectLockConfiguration"
	ActionGetObjectVersionTagging    Action = "s3:GetObjectVersionTagging"
	ActionPutObjectVersionTagging    Action = "s3:PutObjectVersionTagging"
	ActionDeleteObjectVersionTagging Action = "s3:DeleteObjectVersionTagging"
	ActionGetLifecycle               Action = "s3:GetLifecycleConfiguration"
	ActionPutLifecycle               Action = "s3:PutLifecycleConfiguration"
	ActionGetBucketNotification      Action = "s3:GetBucketNotification"
	ActionPutBucketNotification      Action = "s3:PutBucketNotification"
	ActionGetReplication             Action = "s3:GetReplicationConfiguration"
	ActionPutReplication             Action = "s3:PutReplicationConfiguration"
	// ActionForceDeleteBucket is required on top of s3:DeleteBucket to
	// delete a non-empty bucket.
	ActionForceDeleteBucket Action = "s3:ForceDeleteBucket"
)

// Service is the policy engine and the IAM policy store.
type Service interface {
	// Evaluate decides whether the request described by args is allowed
	// (see the package documentation for the algorithm). An empty action
	// is domain.ErrInvalidArgument; a failure of the store or of the
	// bucket policy provider is returned as is and the caller must deny.
	Evaluate(ctx context.Context, args Args) (Decision, error)
	// Simulate is Evaluate for the policy simulator (docs/09 S-IAM-04b):
	// the same decision, plus Decision.Evaluated listing every statement
	// considered and its outcome.
	Simulate(ctx context.Context, args Args) (Decision, error)

	// Parse decodes and validates a JSON policy document. Errors wrap
	// domain.ErrMalformedPolicy (with a *ValidationError locating the
	// problem: line and column for JSON errors, statement index and
	// element otherwise) or domain.ErrPolicyTooLarge (over 20 KiB), and
	// carry a *domain.S3Error with the S3 code.
	Parse(ctx context.Context, raw []byte) (*domain.Policy, error)
	// Validate checks a decoded policy: version, statements, effects,
	// actions, resources, principals, condition operators and values.
	Validate(ctx context.Context, p *domain.Policy) error
	// ParseBucketPolicy is Parse plus the bucket policy rules: every
	// statement names a Principal or NotPrincipal, uses S3 actions only and
	// targets bucket or its objects.
	ParseBucketPolicy(ctx context.Context, bucket string, raw []byte) (*domain.Policy, error)
	// ValidateBucketPolicy is Validate plus the bucket policy rules.
	ValidateBucketPolicy(ctx context.Context, bucket string, p *domain.Policy) error
	// IsPublic reports whether the bucket policy p grants access to
	// everyone, with the semantics of the S3 PolicyStatus: an Allow
	// statement whose principal is "*" (or that uses NotPrincipal) and whose
	// conditions do not pin the requester to fixed sources (aws:SourceIp
	// ranges other than 0.0.0.0/0 and ::/0, aws:SourceVpc(e), aws:SourceArn,
	// aws:SourceAccount, aws:SourceOwner, aws:Principal{OrgID,Account,Arn},
	// aws:userid, aws:username). A nil policy is not public.
	IsPublic(ctx context.Context, p *domain.Policy) bool

	// GeneratePublicPolicy returns the bucket policy granting the preset
	// to anonymous users on prefix ("" for the whole bucket); nil for
	// PresetPrivate.
	GeneratePublicPolicy(ctx context.Context, bucket, prefix string, preset Preset) (*domain.Policy, error)
	// ApplyPublicPreset returns current (which is not modified) with the
	// anonymous rules of prefix replaced by those of preset; the other
	// statements are kept. The result is nil when no statement remains,
	// meaning the bucket policy should be deleted.
	ApplyPublicPreset(ctx context.Context, current *domain.Policy, bucket, prefix string, preset Preset) (*domain.Policy, error)
	// DetectPreset reports the anonymous access that the bucket policy p
	// actually grants on prefix: one of the presets, or PresetCustom when
	// anonymous statements cover the prefix without matching a preset.
	DetectPreset(ctx context.Context, p *domain.Policy, bucket, prefix string) Preset
	// ListPublicAccess lists the prefixes the bucket policy grants
	// anonymous access to, with their preset, sorted by prefix.
	ListPublicAccess(ctx context.Context, p *domain.Policy, bucket string) []PublicAccessRule

	// SetPolicy creates or replaces a named IAM policy. Built-in names are
	// read-only (domain.ErrInvalidArgument) and identity policies may not
	// name a Principal.
	SetPolicy(ctx context.Context, name string, p *domain.Policy) error
	// GetPolicy returns a copy of a named policy (built-in or stored), or
	// domain.ErrPolicyNotFound.
	GetPolicy(ctx context.Context, name string) (*domain.Policy, error)
	// DeletePolicy detaches a stored policy from every user and group and
	// deletes it. Built-ins cannot be deleted (domain.ErrInvalidArgument).
	DeletePolicy(ctx context.Context, name string) error
	// ListPolicies returns the built-in policies first, then the stored
	// ones, each group sorted by name.
	ListPolicies(ctx context.Context) ([]Named, error)
	// ListBuiltin returns the built-in policies (readonly, readwrite,
	// writeonly, diagnostics, consoleAdmin), sorted by name.
	ListBuiltin(ctx context.Context) []Named

	// Attach and Detach bind named policies to a user or a group (in the
	// store, or through Deps.Attachments when set). Attach fails with
	// domain.ErrPolicyNotFound for unknown names; Detach is idempotent.
	Attach(ctx context.Context, in AttachInput) error
	Detach(ctx context.Context, in AttachInput) error
	// Entities lists the users and groups a policy is attached to (admin
	// policy entities).
	Entities(ctx context.Context, name string) (PolicyEntities, error)
}

// Args describe the request being authorized.
type Args struct {
	Identity *domain.Identity
	Action   Action
	Bucket   string
	// Object is empty for bucket-level actions.
	Object string
	// Conditions are the request condition values keyed by condition key
	// (e.g. "aws:SourceIp", "s3:prefix", "aws:SecureTransport"). Keys are
	// case-insensitive. The engine adds aws:CurrentTime, aws:EpochTime,
	// aws:username, aws:userid, aws:principaltype, s3:authType,
	// s3:signatureAge and jwt:* (STS claims) when they are missing.
	Conditions map[string][]string
	// IsOwner reports that the identity owns the bucket. Informational:
	// Nexo has no bucket ownership, permissions come from policies only.
	IsOwner bool
	// BucketPolicy is the bucket policy to evaluate alongside IAM
	// policies. When nil, the engine asks Deps.Buckets (if configured).
	BucketPolicy *domain.Policy
	// Now is the evaluation time (default: the engine clock).
	Now time.Time
}

// Source is where a statement comes from.
type Source string

// Statement sources.
const (
	// SourceRoot marks the decision taken for root credentials.
	SourceRoot Source = "root"
	// SourceIAM is a named policy attached to the user (or listed in
	// Identity.PolicyNames).
	SourceIAM Source = "iam"
	// SourceGroup is a named policy attached to a group of the user.
	SourceGroup Source = "group"
	// SourceInline is the inline policy of a service account or STS session.
	SourceInline Source = "inline"
	// SourceBucket is the bucket policy.
	SourceBucket Source = "bucket"
)

// Decision is the outcome of Evaluate and Simulate.
type Decision struct {
	Allowed bool
	// Reason explains the decision for logs and the console.
	Reason string
	// Source is the origin of the deciding statement; SourceRoot for root,
	// SourceInline when the inline policy denies by not allowing, empty
	// for an implicit deny.
	Source Source
	// PolicyName is the named policy holding the deciding statement
	// (iam and group sources).
	PolicyName string
	// Group is the group through which PolicyName applies (group source).
	Group string
	// MatchedStatement is the statement that decided, nil for root and
	// implicit denies.
	MatchedStatement *domain.Statement
	// Sid is the Sid of MatchedStatement.
	Sid string
	// Index is the 0-based position of MatchedStatement in its document;
	// meaningful only when MatchedStatement is set.
	Index int
	// Evaluated lists every statement considered, in evaluation order
	// (iam, group, inline, bucket). Only Simulate fills it.
	Evaluated []EvaluatedStatement
}

// Mismatch names the first element of a statement that did not match.
type Mismatch string

// Statement elements reported by EvaluatedStatement.Mismatch.
const (
	MismatchNone      Mismatch = ""
	MismatchPrincipal Mismatch = "principal"
	MismatchAction    Mismatch = "action"
	MismatchResource  Mismatch = "resource"
	MismatchCondition Mismatch = "condition"
)

// EvaluatedStatement is the outcome of one statement in the simulator.
type EvaluatedStatement struct {
	Source     Source
	PolicyName string
	Group      string
	// Index is the 0-based position of the statement in its document.
	Index  int
	Sid    string
	Effect domain.Effect
	// Matched reports that principal, action, resource and conditions all
	// match: the statement applies to the request.
	Matched bool
	// Mismatch is the first element that did not match (empty when Matched).
	Mismatch  Mismatch
	Statement *domain.Statement
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

// PolicyEntities are the users and groups a policy is attached to.
type PolicyEntities struct {
	Policy string
	Users  []string
	Groups []string
}

// Preset is a simplified anonymous access level of a bucket or prefix
// (docs/09 §9.3, `nexoctl anonymous set`).
type Preset string

// Anonymous access presets.
const (
	// PresetPrivate grants nothing to anonymous users.
	PresetPrivate Preset = "private"
	// PresetDownload grants listing (s3:ListBucket restricted to the
	// prefix) and reading (s3:GetObject).
	PresetDownload Preset = "download"
	// PresetUpload grants writing (s3:PutObject and the multipart
	// actions) without reading.
	PresetUpload Preset = "upload"
	// PresetPublic is download plus upload plus s3:DeleteObject.
	PresetPublic Preset = "public"
	// PresetCustom is reported by DetectPreset when anonymous statements
	// cover the prefix without matching a preset; it cannot be applied.
	PresetCustom Preset = "custom"
)

// PublicAccessRule is the anonymous access level of one prefix.
type PublicAccessRule struct {
	// Prefix is "" for the whole bucket.
	Prefix string
	Preset Preset
}

// Built-in policy names.
const (
	PolicyConsoleAdmin = "consoleAdmin"
	PolicyReadOnly     = "readonly"
	PolicyReadWrite    = "readwrite"
	PolicyWriteOnly    = "writeonly"
	PolicyDiagnostics  = "diagnostics"
)
