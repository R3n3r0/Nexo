package nexoadmin

import (
	"context"
	"net/http"
	"time"
)

// Access rules (docs/10 §10.1): rules per bucket with a prefix, glob or RE2
// regex pattern of the keys, evaluated by the policy engine next to the
// bucket policy.

// Pattern types of KeyPattern.
const (
	PatternPrefix = "prefix"
	PatternGlob   = "glob"
	PatternRegex  = "regex"
)

// Effects and principal forms of the access rules.
const (
	EffectAllow       = "allow"
	EffectDeny        = "deny"
	PrincipalEveryone = "*"
)

// KeyPattern selects keys (access rules) or paths (behaviors).
type KeyPattern struct {
	// Type is PatternPrefix, PatternGlob or PatternRegex.
	Type            string `json:"type" yaml:"type"`
	Pattern         string `json:"pattern" yaml:"pattern"`
	CaseInsensitive bool   `json:"caseInsensitive,omitempty" yaml:"caseInsensitive,omitempty"`
}

// TimeWindow bounds the time of the requests (RFC 3339, both included).
type TimeWindow struct {
	From string `json:"from,omitempty" yaml:"from,omitempty"`
	To   string `json:"to,omitempty" yaml:"to,omitempty"`
}

// AccessRuleConditions are the conditions of an access rule; all must
// hold.
type AccessRuleConditions struct {
	SourceIP        []string          `json:"sourceIp,omitempty" yaml:"sourceIp,omitempty"`
	NotSourceIP     []string          `json:"notSourceIp,omitempty" yaml:"notSourceIp,omitempty"`
	Referer         []string          `json:"referer,omitempty" yaml:"referer,omitempty"`
	Host            []string          `json:"host,omitempty" yaml:"host,omitempty"`
	SecureTransport *bool             `json:"secureTransport,omitempty" yaml:"secureTransport,omitempty"`
	TimeWindow      *TimeWindow       `json:"timeWindow,omitempty" yaml:"timeWindow,omitempty"`
	Methods         []string          `json:"methods,omitempty" yaml:"methods,omitempty"`
	Headers         map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	CaptureEquals   map[string]string `json:"captureEquals,omitempty" yaml:"captureEquals,omitempty"`
}

// BucketAccessRule is an access rule of a bucket.
type BucketAccessRule struct {
	ID string `json:"id" yaml:"id"`
	// Priority orders the evaluation, lower first.
	Priority int `json:"priority" yaml:"priority"`
	// Effect is EffectAllow or EffectDeny.
	Effect string `json:"effect" yaml:"effect"`
	// Principals are "*", "user:<name>", "group:<name>",
	// "accesskey:<key>" or "distribution:<id>".
	Principals []string `json:"principals" yaml:"principals"`
	// Actions are classes (read, list, write, delete, tag, all) or S3
	// actions.
	Actions []string `json:"actions" yaml:"actions"`
	// Match is the pattern of the keys; nil covers the whole bucket.
	Match       *KeyPattern           `json:"match,omitempty" yaml:"match,omitempty"`
	Conditions  *AccessRuleConditions `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Description string                `json:"description,omitempty" yaml:"description,omitempty"`
}

// AccessRules are the access rules of a bucket.
type AccessRules struct {
	Bucket string             `json:"bucket"`
	Rules  []BucketAccessRule `json:"rules"`
	// ETag identifies the version of the rules (the ifMatch of
	// SetAccessRules).
	ETag      string     `json:"etag"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
	// Public reports an allow rule for everyone in effect.
	Public bool `json:"public"`
	// BlockPublic is the block public access in effect on the bucket.
	BlockPublic bool     `json:"blockPublic"`
	Warnings    []string `json:"warnings"`
}

// TestAccessRulesRequest describes the requests of TestAccessRules.
type TestAccessRulesRequest struct {
	Keys []string `json:"keys,omitempty"`
	// Action is a class (read, list, write, delete, tag) or an S3 action.
	Action string `json:"action,omitempty"`
	// Principal is "anonymous" (default), "user:<name>",
	// "accesskey:<key>" or "distribution:<id>".
	Principal    string            `json:"principal,omitempty"`
	IP           string            `json:"ip,omitempty"`
	Host         string            `json:"host,omitempty"`
	Method       string            `json:"method,omitempty"`
	Secure       *bool             `json:"secure,omitempty"`
	Referer      string            `json:"referer,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Distribution string            `json:"distribution,omitempty"`
	Time         *time.Time        `json:"time,omitempty"`
}

// EvaluatedAccessRule is the outcome of one rule for one key.
type EvaluatedAccessRule struct {
	ID       string            `json:"id"`
	Priority int               `json:"priority"`
	Effect   string            `json:"effect"`
	Captures map[string]string `json:"captures,omitempty"`
	Matched  bool              `json:"matched"`
	// Mismatch is what did not match (principal, action, key, condition,
	// blocked-public); Condition names the condition.
	Mismatch  string `json:"mismatch,omitempty"`
	Condition string `json:"condition,omitempty"`
}

// AccessRulesKeyResult is the outcome of the requests on one key.
type AccessRulesKeyResult struct {
	Key           string                `json:"key"`
	NormalizedKey string                `json:"normalizedKey"`
	Rules         []EvaluatedAccessRule `json:"rules"`
	Matched       []string              `json:"matched"`
	BlockedPublic []string              `json:"blockedPublic,omitempty"`
	// RulesOutcome is the outcome of the rules alone: allow, deny or none.
	RulesOutcome string `json:"rulesOutcome"`
	// Allowed is the final decision; Determinant the rule that decided.
	Allowed     bool   `json:"allowed"`
	Determinant string `json:"determinant,omitempty"`
	Source      string `json:"source,omitempty"`
	Reason      string `json:"reason"`
}

// AccessRulesTestResult is the outcome of TestAccessRules.
type AccessRulesTestResult struct {
	Bucket      string                 `json:"bucket"`
	Action      string                 `json:"action"`
	BlockPublic BlockPublic            `json:"blockPublic"`
	Results     []AccessRulesKeyResult `json:"results"`
}

// Presets of the access rules.
const (
	PresetPrivate        = "private"
	PresetPublicRead     = "public-read"
	PresetPublicReadList = "public-read-list"
	PresetPublicPrefix   = "public-prefix"
	PresetPublicWrite    = "public-write"
	PresetCustom         = "custom"
)

// AccessRulesPublicStatus is the public access of a bucket through its
// rules.
type AccessRulesPublicStatus struct {
	Bucket  string   `json:"bucket"`
	Public  bool     `json:"public"`
	Preset  string   `json:"preset"`
	Rules   []string `json:"rules,omitempty"`
	Blocked bool     `json:"blocked"`
}

// AccessPresetRequest applies a preset (ApplyAccessPreset).
type AccessPresetRequest struct {
	Preset string `json:"preset"`
	// Match is the pattern of public-prefix (and public-write).
	Match *KeyPattern `json:"match,omitempty"`
	// Confirm acknowledges public-write.
	Confirm bool `json:"confirm,omitempty"`
}

// Block public access modes.
const (
	BlockModeReject = "reject"
	BlockModeIgnore = "ignore"
)

// BlockPublic is a block public access setting.
type BlockPublic struct {
	// Bucket is "" for the cluster level.
	Bucket  string `json:"bucket,omitempty"`
	Enabled bool   `json:"enabled"`
	// Mode is BlockModeReject or BlockModeIgnore (cluster setting).
	Mode      string `json:"mode"`
	Cluster   bool   `json:"cluster"`
	Effective bool   `json:"effective"`
}

// BlockPublicRequest sets a block public access.
type BlockPublicRequest struct {
	Enabled bool `json:"enabled"`
	// Mode (cluster level only) is BlockModeReject or BlockModeIgnore; ""
	// keeps it.
	Mode string `json:"mode,omitempty"`
}

// AccessRuleError locates the problem of an access rule
// (APIError.AccessRuleErrors).
type AccessRuleError struct {
	// Rule is the 1-based index of the rule, 0 for the whole list.
	Rule     int    `json:"rule"`
	ID       string `json:"id,omitempty"`
	Field    string `json:"field,omitempty"`
	Position int    `json:"position,omitempty"`
	Message  string `json:"message"`
}

// Error codes of the access rules (APIError.Code).
const (
	CodeInvalidAccessRule    = "invalid_access_rule"
	CodeTooManyAccessRules   = "too_many_access_rules"
	CodePublicAccessBlocked  = "public_access_blocked"
	CodeConfirmationRequired = "confirmation_required"
	CodePreconditionFailed   = "precondition_failed"
)

// IsPreconditionFailed reports an error answered with 412: the document
// changed since its ETag was read.
func IsPreconditionFailed(err error) bool { return statusOf(err) == http.StatusPreconditionFailed }

// ifMatchHeader returns the If-Match header of an ETag ("" for none).
func ifMatchHeader(etag string) http.Header {
	if etag == "" {
		return nil
	}
	return http.Header{"If-Match": {`"` + etag + `"`}}
}

// GetAccessRules implements AdminClient.
func (c *client) GetAccessRules(ctx context.Context, bucket string) (*AccessRules, error) {
	var out AccessRules
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "access-rules")}, &out)
}

// SetAccessRules implements AdminClient.
func (c *client) SetAccessRules(ctx context.Context, bucket string, rules []BucketAccessRule, ifMatch string) (*AccessRules, error) {
	if rules == nil {
		rules = []BucketAccessRule{}
	}
	var out AccessRules
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", bucket, "access-rules"), header: ifMatchHeader(ifMatch),
		body: struct {
			Rules []BucketAccessRule `json:"rules"`
		}{rules}}, &out)
}

// TestAccessRules implements AdminClient.
func (c *client) TestAccessRules(ctx context.Context, bucket string, in TestAccessRulesRequest) (*AccessRulesTestResult, error) {
	var out AccessRulesTestResult
	return &out, c.do(ctx, request{method: http.MethodPost, path: pathOf("buckets", bucket, "access-rules", "test"), body: in}, &out)
}

// GetAccessRulesPublic implements AdminClient.
func (c *client) GetAccessRulesPublic(ctx context.Context, bucket string) (*AccessRulesPublicStatus, error) {
	var out AccessRulesPublicStatus
	return &out, c.do(ctx, request{method: http.MethodGet, path: pathOf("buckets", bucket, "access-rules", "public")}, &out)
}

// ApplyAccessPreset implements AdminClient.
func (c *client) ApplyAccessPreset(ctx context.Context, bucket string, in AccessPresetRequest, ifMatch string) (*AccessRules, error) {
	var out AccessRules
	return &out, c.do(ctx, request{method: http.MethodPut, path: pathOf("buckets", bucket, "access-rules", "public"), header: ifMatchHeader(ifMatch), body: in}, &out)
}

// blockPublicPath is the path of the block public access of a bucket, or
// of the cluster for "".
func blockPublicPath(bucket string) string {
	if bucket == "" {
		return "/access/block-public"
	}
	return pathOf("buckets", bucket, "block-public")
}

// GetBlockPublic implements AdminClient.
func (c *client) GetBlockPublic(ctx context.Context, bucket string) (*BlockPublic, error) {
	var out BlockPublic
	return &out, c.do(ctx, request{method: http.MethodGet, path: blockPublicPath(bucket)}, &out)
}

// SetBlockPublic implements AdminClient.
func (c *client) SetBlockPublic(ctx context.Context, bucket string, in BlockPublicRequest) (*BlockPublic, error) {
	var out BlockPublic
	return &out, c.do(ctx, request{method: http.MethodPut, path: blockPublicPath(bucket), body: in}, &out)
}
