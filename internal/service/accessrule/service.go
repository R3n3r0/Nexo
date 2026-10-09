// Package accessrule manages the access rules of the buckets (docs/10
// §10.1): rules with a priority, an effect, principals, actions, a key
// pattern (prefix, glob or RE2 regex, internal/service/match) and
// conditions, stored in the bucket metadata and evaluated by the policy
// engine as a resource-based source next to the bucket policy
// (Evaluator implements policy.AccessRules).
//
// # Rules
//
//   - principals: "*" (everyone, anonymous included), "user:<name>" (the
//     effective user of the credentials: the parent of a service account
//     or of an STS session), "group:<name>", "accesskey:<key>",
//     "distribution:<id>" (the requests served by a distribution, docs/10
//     §10.2). Names accept the "*" and "?" wildcards.
//   - actions: the classes read (s3:GetObject, s3:GetObjectVersion,
//     s3:GetObjectAttributes, s3:GetObjectVersionAttributes), list
//     (s3:ListBucket, s3:ListBucketVersions, s3:ListBucketMultipartUploads),
//     write (s3:PutObject, which covers the copies and the multipart
//     uploads, s3:AbortMultipartUpload, s3:ListMultipartUploadParts),
//     delete (s3:DeleteObject, s3:DeleteObjectVersion), tag (the
//     Get/Put/DeleteObject[Version]Tagging actions) and all (every class),
//     or explicit S3 actions ("s3:GetObject", "s3:Get*"). The actions of
//     object lock (retention, legal hold, governance bypass, the object
//     lock configuration) are never granted by an access rule: a rule
//     cannot override object lock.
//   - match: the pattern of the keys; omitted, the rule covers the whole
//     bucket. An object action matches the key of the object, a listing
//     the listing prefix (s3:prefix, "" for the whole bucket), any other
//     bucket action the empty key. Keys are normalized (match.Service) so
//     that an encoding never bypasses a rule.
//   - conditions (all must hold): sourceIp / notSourceIp (CIDRs or IPs,
//     IPv4 and IPv6, on the client behind the trusted proxies), referer
//     (patterns with "*" and "?", case-insensitive), host (the host the
//     request was addressed to, exact or "*.example.com"), secureTransport,
//     timeWindow (RFC 3339 bounds, both included), methods (the HTTP
//     method; for a request without one, such as an operation of the
//     console, the method of the S3 request the action stands for),
//     headers (exact values, compared in constant time, which may
//     reference secrets as ${secret:<name>}) and captureEquals (a named
//     group of a regex match equal to a value with policy variables such
//     as ${aws:username}). A condition that cannot be evaluated (a client
//     address that is not known, a variable or a secret that cannot be
//     resolved) never widens access: an allow rule does not apply, a deny
//     rule does.
//
// # Evaluation
//
// Deny rules come first, then allow rules, each in order of priority
// (lower first, then in the order of the document): the outcome names the
// deny rule of lowest priority that applies, otherwise the allow rule of
// lowest priority that applies. The policy engine combines it with the
// other sources (a deny in any source denies, an allow from IAM, the
// bucket policy or an access rule grants, the inline policies of derived
// credentials still limit them, root bypasses them).
//
// # Block public access
//
// At the cluster level (the "access" subsystem of the dynamic
// configuration: block_public, block_public_mode) and per bucket: while
// it is on, the principal "*" of the allow rules is ignored by the
// evaluation (the rule still applies to its other principals) and, in the
// "reject" mode, an allow rule for "*" is refused when the rules are
// saved (ErrPublicAccessBlocked). Deny rules for "*" are never affected.
//
// The rules of a bucket are compiled once per version of the document
// (its ETag) and cached by the Evaluator; a change saved on any node
// reaches the others with the invalidation of the bucket metadata
// (cluster.Coherence), without restart.
package accessrule

import (
	"context"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Effect is the effect of a rule.
type Effect string

// Rule effects.
const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Action classes of the rules.
const (
	ActionRead   = "read"
	ActionList   = "list"
	ActionWrite  = "write"
	ActionDelete = "delete"
	ActionTag    = "tag"
	ActionAll    = "all"
)

// Principal forms of the rules.
const (
	PrincipalEveryone           = "*"
	PrincipalUserPrefix         = "user:"
	PrincipalGroupPrefix        = "group:"
	PrincipalAccessKeyPrefix    = "accesskey:"
	PrincipalDistributionPrefix = "distribution:"
)

// Limits and defaults.
const (
	// DefaultMaxRules is the default maximum number of rules of a bucket
	// (WithMaxRules).
	DefaultMaxRules = 200
	// MaxPriority bounds the priority of a rule (0..MaxPriority).
	MaxPriority = 1_000_000
	// PresetPriority is the priority of the rules generated by the
	// presets: after the rules written by hand.
	PresetPriority = 1000
	// PresetRulePrefix starts the ids of the rules generated by the
	// presets, which ApplyPreset replaces.
	PresetRulePrefix = "preset-"
)

// Configuration of the cluster-level block public access: the subsystem
// and keys of the dynamic configuration.
const (
	ConfigSubsystem          = "access"
	ConfigKeyBlockPublic     = "block_public"
	ConfigKeyBlockPublicMode = "block_public_mode"
)

// Rule is an access rule.
type Rule struct {
	ID string `json:"id" yaml:"id"`
	// Priority orders the evaluation (lower first), 0..MaxPriority.
	Priority int    `json:"priority" yaml:"priority"`
	Effect   Effect `json:"effect" yaml:"effect"`
	// Principals are "*", "user:<name>", "group:<name>",
	// "accesskey:<key>" or "distribution:<id>".
	Principals []string `json:"principals" yaml:"principals"`
	// Actions are action classes (read, list, write, delete, tag, all) or
	// S3 actions ("s3:GetObject", "s3:Get*").
	Actions []string `json:"actions" yaml:"actions"`
	// Match is the pattern of the keys; nil covers the whole bucket.
	Match *match.Pattern `json:"match,omitempty" yaml:"match,omitempty"`
	// Conditions must all hold; nil for none.
	Conditions *Conditions `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	// Description is free text.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Conditions are the conditions of a rule.
type Conditions struct {
	// SourceIP are CIDRs or IPs one of which must contain the client.
	SourceIP []string `json:"sourceIp,omitempty" yaml:"sourceIp,omitempty"`
	// NotSourceIP are CIDRs or IPs none of which may contain the client.
	NotSourceIP []string `json:"notSourceIp,omitempty" yaml:"notSourceIp,omitempty"`
	// Referer are patterns ("*", "?") one of which the Referer must match.
	Referer []string `json:"referer,omitempty" yaml:"referer,omitempty"`
	// Host are the hosts one of which the request must be addressed to
	// (exact, or "*.example.com" for the subdomains).
	Host []string `json:"host,omitempty" yaml:"host,omitempty"`
	// SecureTransport requires (true) or excludes (false) TLS.
	SecureTransport *bool `json:"secureTransport,omitempty" yaml:"secureTransport,omitempty"`
	// TimeWindow bounds the time of the request.
	TimeWindow *TimeWindow `json:"timeWindow,omitempty" yaml:"timeWindow,omitempty"`
	// Methods are the HTTP methods one of which the request must use.
	Methods []string `json:"methods,omitempty" yaml:"methods,omitempty"`
	// Headers are request headers and the values they must have, which
	// may reference secrets (${secret:<name>}) and policy variables.
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// CaptureEquals are named groups of the regex of the rule and the
	// values they must capture, with policy variables (${aws:username}).
	CaptureEquals map[string]string `json:"captureEquals,omitempty" yaml:"captureEquals,omitempty"`
}

// TimeWindow bounds the time of a request: RFC 3339 times, both included;
// an empty bound is open.
type TimeWindow struct {
	From string `json:"from,omitempty" yaml:"from,omitempty"`
	To   string `json:"to,omitempty" yaml:"to,omitempty"`
}

// Document is the YAML or JSON form of the rules of a bucket (export,
// import, Parse).
type Document struct {
	Bucket string `json:"bucket,omitempty" yaml:"bucket,omitempty"`
	Rules  []Rule `json:"rules" yaml:"rules"`
}

// Config is the access rules configuration of a bucket.
type Config struct {
	Bucket string `json:"bucket"`
	// Rules are the rules as stored, in the order of the document.
	Rules []Rule `json:"rules"`
	// ETag identifies the version of the rules (If-Match of Put).
	ETag      string    `json:"etag"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	// Public reports an allow rule for everyone that is in effect (not
	// blocked).
	Public bool `json:"public"`
	// BlockPublic is the block public access in effect on the bucket
	// (cluster or bucket level).
	BlockPublic bool `json:"blockPublic"`
	// Warnings describe the rules that grant everyone write or delete
	// access, and those that public access blocking ignores.
	Warnings []string `json:"warnings,omitempty"`
}

// Service manages the access rules, the presets and the block public
// access.
type Service interface {
	// Get returns the rules of a bucket (an empty list when none).
	Get(ctx context.Context, bucket string) (*Config, error)
	// Put validates the rules and replaces those of a bucket atomically:
	// with in.IfMatch set, only when the current rules have that ETag
	// (domain.ErrPreconditionFailed otherwise). Every change is audited
	// with its diff. An invalid rule is ErrInvalidRule (a *RuleError
	// locating it), too many rules ErrTooManyRules, an allow rule for
	// everyone while public access is blocked in the reject mode
	// ErrPublicAccessBlocked.
	Put(ctx context.Context, in PutInput) (*Config, error)
	// Parse decodes a YAML or JSON document of rules (a Document, or a
	// bare list of rules) and validates the rules (not the block public
	// access, which depends on the bucket). Errors are ErrInvalidRule.
	Parse(ctx context.Context, doc []byte) (*Document, error)
	// Validate checks rules as Put does, without saving them.
	Validate(ctx context.Context, bucket string, rules []Rule) error
	// Test evaluates requests on keys of a bucket with every source of the
	// policy engine (IAM, bucket policy, access rules): for each key, the
	// rules that match, the outcome of the rules alone, the final
	// decision and the rule that determined it (docs/10 D-ACC-02,
	// nexoctl access test).
	Test(ctx context.Context, in TestInput) (*TestResult, error)
	// ApplyPreset replaces the rules generated by the presets (ids
	// starting with PresetRulePrefix) with those of a preset; the private
	// preset also removes every allow rule for everyone. public-write
	// needs in.Confirm (ErrConfirmationRequired).
	ApplyPreset(ctx context.Context, in PresetInput) (*Config, error)
	// PublicStatus reports whether a bucket is public through its access
	// rules, and with which preset.
	PublicStatus(ctx context.Context, bucket string) (*PublicStatus, error)
	// GetBlockPublic returns the block public access of a bucket, or of
	// the cluster when bucket is empty.
	GetBlockPublic(ctx context.Context, bucket string) (*BlockPublic, error)
	// SetBlockPublic turns the block public access of a bucket, or of the
	// cluster (in.Bucket empty, with its mode), on or off; audited.
	SetBlockPublic(ctx context.Context, in BlockPublicInput) (*BlockPublic, error)
	// PublicAccessBlocked reports whether the block public access is in
	// effect on a bucket (cluster or bucket level): the public behaviors
	// of the distributions refuse their requests then (docs/10 §10.2,
	// distribution.PublicAccessBlock).
	PublicAccessBlocked(ctx context.Context, bucket string) (bool, error)
}

// Evaluator evaluates the access rules of the buckets for the policy
// engine (it implements policy.AccessRules): the compiled rules are cached
// per bucket and version of the document.
type Evaluator interface {
	// EvaluateRules implements policy.AccessRules.
	EvaluateRules(ctx context.Context, in policy.RuleRequest) (policy.RuleOutcome, error)
}

// ConfigSource reads the access configuration of the buckets at every
// evaluation (bucket.MetadataStore implements it, from its cache).
type ConfigSource interface {
	AccessConfig(ctx context.Context, bucket string) (bucket.AccessConfig, error)
}

// SecretStore resolves the secrets referenced by the header conditions
// (${secret:<name>}). A missing secret is domain.ErrNotFound.
type SecretStore interface {
	Secret(ctx context.Context, name string) (string, error)
}

// Deps are the dependencies of the Service.
type Deps struct {
	// Buckets stores the rules and the block public access of the buckets
	// (required).
	Buckets bucket.Service
	// Policy decides the requests of Test with every source (required).
	Policy policy.Service
	// Matcher compiles the patterns (required).
	Matcher match.Service
	// Config holds the cluster-level block public access (the "access"
	// subsystem). Optional: without it the defaults of the options apply
	// and the cluster level cannot be changed.
	Config config.Service
	// Audit records every change. Optional (the changes are logged).
	Audit  audit.Service
	Clock  clock.Clock
	Logger *slog.Logger
}

// EvaluatorDeps are the dependencies of the Evaluator.
type EvaluatorDeps struct {
	// Source reads the access configuration of the buckets (required).
	Source ConfigSource
	// Matcher compiles the patterns and normalizes the keys (required).
	Matcher match.Service
	// Config holds the cluster-level block public access (optional).
	Config config.Service
	// Secrets resolves the ${secret:<name>} of the header conditions
	// (optional: without it such a condition cannot be evaluated).
	Secrets SecretStore
	Clock   clock.Clock
	Logger  *slog.Logger
}

// PutInput are the parameters of Put.
type PutInput struct {
	Bucket string
	Rules  []Rule
	// IfMatch, when set, is the ETag the current rules must have.
	IfMatch string
	// Actor names who changes the rules (audit).
	Actor string
}

// TestInput describes the requests of Test.
type TestInput struct {
	Bucket string
	// Keys are the keys to test; none tests the bucket itself (the empty
	// key). For a listing action a key is the listing prefix.
	Keys []string
	// Action is an S3 action or an action class (read: s3:GetObject,
	// list: s3:ListBucket, write: s3:PutObject, delete: s3:DeleteObject,
	// tag: s3:PutObjectTagging); default s3:GetObject.
	Action string
	// Identity is the caller; nil tests an anonymous request.
	Identity *domain.Identity
	// IP is the client address; "" leaves it unknown.
	IP string
	// Host is the host the request is addressed to.
	Host string
	// Method is the HTTP method; "" derives it from the action.
	Method string
	// Secure is the transport; nil leaves it unknown.
	Secure  *bool
	Referer string
	// Headers are request headers (name → value).
	Headers map[string]string
	// Distribution is the distribution serving the request.
	Distribution string
	// Now is the time of the request (default: the clock).
	Now time.Time
}

// Outcome is the outcome of the access rules alone.
type Outcome string

// Outcomes of the access rules.
const (
	OutcomeAllow Outcome = "allow"
	OutcomeDeny  Outcome = "deny"
	// OutcomeNone: no rule applies (the other sources decide).
	OutcomeNone Outcome = "none"
)

// TestResult is the result of Test.
type TestResult struct {
	Bucket string `json:"bucket"`
	Action string `json:"action"`
	// BlockPublic is the block public access in effect on the bucket.
	BlockPublic BlockPublic `json:"blockPublic"`
	Results     []KeyResult `json:"results"`
}

// KeyResult is the outcome of Test for one key.
type KeyResult struct {
	Key string `json:"key"`
	// NormalizedKey is the key the patterns were matched against.
	NormalizedKey string `json:"normalizedKey"`
	// Rules lists every rule with its outcome, in evaluation order.
	Rules []policy.EvaluatedRule `json:"rules"`
	// Matched are the ids of the rules that apply, in evaluation order.
	Matched []string `json:"matched"`
	// BlockedPublic are the ids of the rules for everyone that the block
	// public access ignores for this request.
	BlockedPublic []string `json:"blockedPublic,omitempty"`
	// RulesOutcome is the outcome of the access rules alone.
	RulesOutcome Outcome `json:"rulesOutcome"`
	// Allowed is the final decision of the policy engine.
	Allowed bool `json:"allowed"`
	// Determinant is the id of the access rule that determined the
	// decision; "" when another source (or the implicit deny) decided.
	Determinant string `json:"determinant,omitempty"`
	// Source and Reason explain the decision (policy.Decision).
	Source policy.Source `json:"source,omitempty"`
	Reason string        `json:"reason"`
}

// Preset is a simplified public access level (docs/10 §10.1 Scorciatoie).
type Preset string

// Presets.
const (
	// PresetPrivate removes every allow rule for everyone.
	PresetPrivate Preset = "private"
	// PresetPublicRead grants everyone read on the whole bucket.
	PresetPublicRead Preset = "public-read"
	// PresetPublicReadList grants everyone read and list.
	PresetPublicReadList Preset = "public-read-list"
	// PresetPublicPrefix grants everyone read on the keys of a pattern.
	PresetPublicPrefix Preset = "public-prefix"
	// PresetPublicWrite grants everyone write (explicit confirmation).
	PresetPublicWrite Preset = "public-write"
	// PresetCustom is reported by PublicStatus for allow rules for
	// everyone that no preset generated; it cannot be applied.
	PresetCustom Preset = "custom"
)

// PresetInput are the parameters of ApplyPreset.
type PresetInput struct {
	Bucket string
	Preset Preset
	// Match is the pattern of public-prefix (required) and of
	// public-write (optional: the whole bucket).
	Match *match.Pattern
	// Confirm acknowledges public-write.
	Confirm bool
	// IfMatch, when set, is the ETag the current rules must have.
	IfMatch string
	Actor   string
}

// PublicStatus is the public access of a bucket through its rules.
type PublicStatus struct {
	Bucket string `json:"bucket"`
	// Public reports an allow rule for everyone in effect.
	Public bool `json:"public"`
	// Preset is the preset the rules for everyone correspond to,
	// PresetCustom for other rules, PresetPrivate when there are none.
	Preset Preset `json:"preset"`
	// Rules are the ids of the allow rules for everyone.
	Rules []string `json:"rules,omitempty"`
	// Blocked reports that the block public access ignores them.
	Blocked bool `json:"blocked"`
}

// BlockPublicMode is what the block public access does with the allow
// rules for everyone.
type BlockPublicMode string

// Block public access modes.
const (
	// BlockModeReject refuses them when saved, and ignores those already
	// stored.
	BlockModeReject BlockPublicMode = "reject"
	// BlockModeIgnore accepts them but ignores them in the evaluation.
	BlockModeIgnore BlockPublicMode = "ignore"
)

// BlockPublic is a block public access setting.
type BlockPublic struct {
	// Bucket is "" for the cluster level.
	Bucket string `json:"bucket,omitempty"`
	// Enabled is the setting of this level.
	Enabled bool `json:"enabled"`
	// Mode is the mode of the deployment (a cluster-level setting).
	Mode BlockPublicMode `json:"mode"`
	// Cluster is the cluster-level setting.
	Cluster bool `json:"cluster"`
	// Effective reports the block in effect: the cluster or the bucket.
	Effective bool `json:"effective"`
}

// BlockPublicInput are the parameters of SetBlockPublic.
type BlockPublicInput struct {
	// Bucket is "" for the cluster level.
	Bucket  string
	Enabled bool
	// Mode sets the mode (cluster level only; "" keeps it).
	Mode  BlockPublicMode
	Actor string
}

// RuleError locates an invalid rule. It unwraps to the sentinel of the
// problem (ErrInvalidRule, ErrTooManyRules, ErrPublicAccessBlocked), all
// wrapping a domain sentinel.
type RuleError struct {
	// Index is the 0-based position of the rule, -1 for the whole list.
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	// Field is the element at fault ("match.pattern",
	// "conditions.sourceIp[1]"...).
	Field string `json:"field,omitempty"`
	// Position is the 1-based position of the problem in a pattern; 0
	// when not applicable.
	Position int    `json:"position,omitempty"`
	Msg      string `json:"message"`

	err error
}
