package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// POST policy condition operators.
const (
	ConditionEq          = "eq"
	ConditionStartsWith  = "starts-with"
	ConditionLengthRange = "content-length-range"
)

// POST form field names (lowercase).
const (
	fieldPolicy        = "policy"
	fieldAlgorithm     = "x-amz-algorithm"
	fieldCredential    = "x-amz-credential" // #nosec G101 -- a POST policy form field name, not a credential
	fieldDate          = "x-amz-date"
	fieldSignature     = "x-amz-signature"
	fieldSecurityToken = "x-amz-security-token" // #nosec G101 -- a POST policy form field name, not a credential
	fieldBucket        = "bucket"
	fieldKey           = "key"
	fieldFile          = "file"
	fieldContentType   = "content-type"
	fieldV2AccessKey   = "awsaccesskeyid"
	fieldV2Signature   = "signature"
	ignorePrefix       = "x-ignore-"
	filenameVariable   = "${filename}"
	// maxPolicyConditions bounds the conditions of a policy document.
	maxPolicyConditions = 256
)

// isPolicyExempt reports whether a form field need not appear in the
// policy conditions: the policy and its signature, the file and the
// x-ignore-* fields (as AWS). The bucket comes from the request target.
func isPolicyExempt(name string) bool {
	switch name {
	case fieldPolicy, fieldSignature, fieldFile, fieldBucket:
		return true
	}
	return strings.HasPrefix(name, ignorePrefix)
}

// isExactMatchOnly reports the fields whose conditions must be exact
// matches.
func isExactMatchOnly(name string) bool {
	switch name {
	case fieldBucket, fieldAlgorithm, fieldCredential, fieldDate, fieldSecurityToken:
		return true
	}
	return false
}

// policyDocument is the JSON shape of a POST policy.
type policyDocument struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

// VerifyPostPolicy implements Verifier.
func (v *verifier) VerifyPostPolicy(ctx context.Context, form PostForm) (*domain.Identity, *PostPolicy, error) {
	fields := make(map[string]string, len(form.Fields))
	for k, val := range form.Fields {
		fields[strings.ToLower(k)] = val
	}
	if fields[fieldAlgorithm] == "" && fields[fieldV2AccessKey] != "" && fields[fieldV2Signature] != "" {
		return nil, nil, errUnsupported()
	}
	policyB64 := fields[fieldPolicy]
	if policyB64 == "" {
		return nil, nil, errPostArgument("Bucket POST must contain a field named 'policy'.")
	}
	if fields[fieldAlgorithm] != algorithm {
		return nil, nil, errPostArgument("The x-amz-algorithm field must be AWS4-HMAC-SHA256.")
	}
	scope, err := parseCredential(fields[fieldCredential])
	if err != nil {
		return nil, nil, errPostArgument("Invalid x-amz-credential field: " + err.Error() + ".")
	}
	reqTime, err := parseAmzDate(fields[fieldDate])
	if err != nil {
		return nil, nil, errPostArgument("The x-amz-date field must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\".")
	}
	signature := fields[fieldSignature]
	if !isSignature(signature) {
		return nil, nil, errPostArgument("The x-amz-signature field must be 64 hex characters.")
	}
	// No clock skew check: a form is signed once and used until the
	// policy expiration.
	if err := v.checkScope(ctx, scope, reqTime); err != nil {
		return nil, nil, errPostArgument("Invalid x-amz-credential field: " + err.Error() + ".")
	}
	creds, identity, err := v.lookup(ctx, scope.accessKey, fields[fieldSecurityToken])
	if err != nil {
		return nil, nil, err
	}
	if !equalSignatures(signatureHex(v.keys.get(creds.SecretKey, scope), policyB64), signature) {
		v.deps.Logger.DebugContext(ctx, "auth: post policy signature mismatch", "access_key", scope.accessKey, "bucket", form.Bucket)
		return nil, nil, errSignature("post policy")
	}

	raw, err := base64.StdEncoding.DecodeString(policyB64)
	if err != nil {
		return nil, nil, errPolicyDocument("Invalid Policy: the policy must be base64-encoded JSON.")
	}
	policy, err := parsePostPolicy(raw)
	if err != nil {
		return nil, nil, err
	}
	now := v.deps.Clock.Now()
	if !now.Before(policy.Expiration) {
		return nil, nil, errExpired("Invalid according to Policy: Policy expired.")
	}
	if err := checkPostConditions(policy, form, fields); err != nil {
		return nil, nil, err
	}
	identity.AuthType = domain.AuthTypePost
	identity.SignatureAge = signatureAge(now, reqTime)
	return &identity, policy, nil
}

// parsePostPolicy decodes the JSON policy document.
func parsePostPolicy(raw []byte) (*PostPolicy, error) {
	var doc policyDocument
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, errPolicyDocument("Invalid Policy: the policy document is not valid JSON: " + err.Error() + ".")
	}
	if doc.Expiration == "" {
		return nil, errPolicyDocument("Invalid Policy: the policy must contain an expiration.")
	}
	exp, err := time.Parse(time.RFC3339Nano, doc.Expiration)
	if err != nil {
		return nil, errPolicyDocument("Invalid Policy: the expiration must be an ISO 8601 timestamp.")
	}
	if len(doc.Conditions) > maxPolicyConditions {
		return nil, errPolicyDocument("Invalid Policy: too many conditions.")
	}
	p := &PostPolicy{Expiration: exp, Conditions: make([]PostCondition, 0, len(doc.Conditions))}
	for _, rawCond := range doc.Conditions {
		cond, err := parsePostCondition(rawCond)
		if err != nil {
			return nil, err
		}
		p.Conditions = append(p.Conditions, cond)
	}
	return p, nil
}

// parsePostCondition decodes {"field": "value"}, ["eq"|"starts-with",
// "$field", "value"] and ["content-length-range", min, max].
func parsePostCondition(raw json.RawMessage) (PostCondition, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil || len(m) != 1 {
			return PostCondition{}, errPolicyDocument("Invalid Policy: object conditions must have exactly one string field.")
		}
		for k, val := range m {
			return PostCondition{Operator: ConditionEq, Key: strings.ToLower(strings.TrimPrefix(k, "$")), Value: val}, nil
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 3 {
		return PostCondition{}, errPolicyDocument("Invalid Policy: array conditions must have three elements.")
	}
	var op string
	if err := json.Unmarshal(arr[0], &op); err != nil {
		return PostCondition{}, errPolicyDocument("Invalid Policy: the condition operator must be a string.")
	}
	switch op = strings.ToLower(op); op {
	case ConditionLengthRange:
		lo, err1 := parseRangeBound(arr[1])
		hi, err2 := parseRangeBound(arr[2])
		if err1 != nil || err2 != nil {
			return PostCondition{}, errPolicyDocument("Invalid Policy: content-length-range bounds must be non-negative integers.")
		}
		if hi < lo {
			return PostCondition{}, errPolicyDocument("Invalid Policy: content-length-range minimum exceeds the maximum.")
		}
		return PostCondition{Operator: ConditionLengthRange, Min: lo, Max: hi}, nil
	case ConditionEq, ConditionStartsWith:
		var key, value string
		if err := json.Unmarshal(arr[1], &key); err != nil || !strings.HasPrefix(key, "$") || len(key) < 2 {
			return PostCondition{}, errPolicyDocument("Invalid Policy: the condition field must be a string starting with '$'.")
		}
		if err := json.Unmarshal(arr[2], &value); err != nil {
			return PostCondition{}, errPolicyDocument("Invalid Policy: the condition value must be a string.")
		}
		key = strings.ToLower(key[1:])
		if op == ConditionStartsWith && isExactMatchOnly(key) {
			return PostCondition{}, errPolicyDocument("Invalid Policy: the " + key + " field only supports exact matches.")
		}
		return PostCondition{Operator: op, Key: key, Value: value}, nil
	default:
		return PostCondition{}, errPolicyDocument("Invalid Policy: unknown condition operator " + op + ".")
	}
}

// parseRangeBound decodes a content-length-range bound: a non-negative
// integer, possibly quoted.
func parseRangeBound(raw json.RawMessage) (int64, error) {
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0, err
	}
	switch t := v.(type) {
	case json.Number:
		n = t
	case string:
		n = json.Number(strings.TrimSpace(t))
	default:
		return 0, fmt.Errorf("unsupported bound %s", raw)
	}
	i, err := n.Int64()
	if err != nil || i < 0 {
		return 0, fmt.Errorf("invalid bound %s", raw)
	}
	return i, nil
}

// checkPostConditions matches the form against the policy: every condition
// must hold and every form field (but the exempt ones) must be covered by
// a condition.
func checkPostConditions(policy *PostPolicy, form PostForm, fields map[string]string) error {
	if b, ok := fields[fieldBucket]; ok && b != form.Bucket {
		return errPolicy("the bucket field does not match the request bucket")
	}
	values := make(map[string]string, len(fields)+1)
	for k, val := range fields {
		values[k] = val
	}
	values[fieldBucket] = form.Bucket
	// The very key the upload is stored under (PostForm.ObjectKey, which
	// the POST handler uses too).
	values[fieldKey] = form.ObjectKey()

	covered := map[string]bool{}
	for _, c := range policy.Conditions {
		if c.Operator == ConditionLengthRange {
			continue
		}
		covered[c.Key] = true
		if !c.matches(values[c.Key]) {
			return errPolicy(fmt.Sprintf("Policy Condition failed: [%q, \"$%s\", %q]", c.Operator, c.Key, c.Value))
		}
	}
	for name := range fields {
		if isPolicyExempt(name) || covered[name] {
			continue
		}
		return errPolicy("Extra input fields: " + name)
	}
	return nil
}

// matches reports whether a field value satisfies an eq or starts-with
// condition. A starts-with on Content-Type applies to every value of a
// comma-separated list, as AWS does.
func (c PostCondition) matches(value string) bool {
	switch c.Operator {
	case ConditionEq:
		return value == c.Value
	case ConditionStartsWith:
		if c.Key == fieldContentType && strings.Contains(value, ",") {
			for _, part := range strings.Split(value, ",") {
				if !strings.HasPrefix(strings.TrimSpace(part), c.Value) {
					return false
				}
			}
			return true
		}
		return strings.HasPrefix(value, c.Value)
	default:
		return true
	}
}

// Check validates the value of a form field against the eq and starts-with
// conditions of the policy on that field (case-insensitive name). A field
// that no condition mentions is rejected unless exempt (policy,
// x-amz-signature, file, bucket, x-ignore-*). The error wraps
// domain.ErrAccessDenied.
func (p *PostPolicy) Check(field, value string) error {
	name := strings.ToLower(field)
	if p == nil {
		return errPolicy("no policy")
	}
	covered := false
	for _, c := range p.Conditions {
		if c.Operator == ConditionLengthRange || c.Key != name {
			continue
		}
		covered = true
		if !c.matches(value) {
			return errPolicy(fmt.Sprintf("Policy Condition failed: [%q, \"$%s\", %q]", c.Operator, c.Key, c.Value))
		}
	}
	if !covered && !isPolicyExempt(name) {
		return errPolicy("Extra input fields: " + name)
	}
	return nil
}

// ObjectKey returns the key of the uploaded object: the key field
// (matched case-insensitively) with "${filename}" replaced by the name of
// the submitted file. VerifyPostPolicy checks the policy conditions on
// this value, and the POST handler stores the object under it, so the
// verified key and the written key cannot diverge.
func (f PostForm) ObjectKey() string {
	key, ok := f.Fields[fieldKey]
	if !ok {
		for name, value := range f.Fields {
			if strings.EqualFold(name, fieldKey) {
				key = value
				break
			}
		}
	}
	return strings.ReplaceAll(key, filenameVariable, f.FileName)
}

// ContentLengthRange returns the bounds of the file size allowed by the
// content-length-range conditions (their intersection when several), and
// false when the policy sets none.
func (p *PostPolicy) ContentLengthRange() (minSize, maxSize int64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	for _, c := range p.Conditions {
		if c.Operator != ConditionLengthRange {
			continue
		}
		if !ok {
			minSize, maxSize, ok = c.Min, c.Max, true
			continue
		}
		minSize = max(minSize, c.Min)
		maxSize = min(maxSize, c.Max)
	}
	return minSize, maxSize, ok
}

// LengthRange is ContentLengthRange.
func (p *PostPolicy) LengthRange() (minSize, maxSize int64, ok bool) {
	return p.ContentLengthRange()
}
