package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
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

// Form fields that need not appear in the policy conditions.
var policyExemptFields = map[string]bool{
	"policy": true, "x-amz-signature": true, "file": true, "bucket": true,
}

// policyDocument is the JSON shape of a POST policy.
type policyDocument struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

func policyError(detail string) error {
	return domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "Invalid according to Policy: "+detail)
}

// VerifyPostPolicy implements Verifier.
func (v *verifier) VerifyPostPolicy(ctx context.Context, form PostForm) (*domain.Identity, *PostPolicy, error) {
	fields := make(map[string]string, len(form.Fields))
	for k, val := range form.Fields {
		fields[strings.ToLower(k)] = val
	}
	policyB64 := fields["policy"]
	if policyB64 == "" {
		return nil, nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Bucket POST must contain a field named 'policy'.")
	}
	if fields["x-amz-algorithm"] != algorithm {
		return nil, nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "x-amz-algorithm must be AWS4-HMAC-SHA256.")
	}
	scope, err := parseCredential(fields["x-amz-credential"])
	if err != nil {
		return nil, nil, err
	}
	reqTime, err := parseAmzDate(fields["x-amz-date"])
	if err != nil {
		return nil, nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "x-amz-date must be in the ISO8601 Long Format.")
	}
	signature := fields["x-amz-signature"]
	if len(signature) != 64 || !isHex(signature) {
		return nil, nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "x-amz-signature must be 64 hex characters.")
	}
	if err := v.checkScope(scope, reqTime); err != nil {
		return nil, nil, err
	}
	creds, identity, err := v.lookup(ctx, scope.accessKey, fields["x-amz-security-token"])
	if err != nil {
		return nil, nil, err
	}
	if !equalSignatures(signatureHex(signingKey(creds.SecretKey, scope), policyB64), signature) {
		return nil, nil, fmt.Errorf("%w: post policy signature", domain.ErrSignatureMismatch)
	}

	raw, err := base64.StdEncoding.DecodeString(policyB64)
	if err != nil {
		return nil, nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "The policy must be base64 encoded JSON.")
	}
	policy, err := parsePostPolicy(raw)
	if err != nil {
		return nil, nil, err
	}
	if !v.deps.Clock.Now().Before(policy.Expiration) {
		return nil, nil, domain.NewS3Error(domain.ErrRequestExpired, "AccessDenied", "Invalid according to Policy: Policy expired.")
	}
	if err := checkPostConditions(policy, form, fields); err != nil {
		return nil, nil, err
	}
	return &identity, policy, nil
}

// parsePostPolicy decodes the JSON policy document.
func parsePostPolicy(raw []byte) (*PostPolicy, error) {
	var doc policyDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "The policy document is not valid JSON: "+err.Error())
	}
	exp, err := time.Parse(time.RFC3339Nano, doc.Expiration)
	if err != nil {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "The policy expiration must be an ISO 8601 timestamp.")
	}
	p := &PostPolicy{Expiration: exp}
	for _, rawCond := range doc.Conditions {
		cond, err := parsePostCondition(rawCond)
		if err != nil {
			return nil, err
		}
		p.Conditions = append(p.Conditions, cond)
	}
	return p, nil
}

// parsePostCondition decodes {"key": "value"}, ["op", "$key", "value"] and
// ["content-length-range", min, max].
func parsePostCondition(raw json.RawMessage) (PostCondition, error) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil || len(m) != 1 {
			return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "Conditions must be single key/value objects or arrays.")
		}
		for k, v := range m {
			return PostCondition{Operator: ConditionEq, Key: strings.ToLower(k), Value: v}, nil
		}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 3 {
		return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "Array conditions must have three elements.")
	}
	var op string
	if err := json.Unmarshal(arr[0], &op); err != nil {
		return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "The condition operator must be a string.")
	}
	switch strings.ToLower(op) {
	case ConditionLengthRange:
		var lo, hi int64
		if err := json.Unmarshal(arr[1], &lo); err != nil {
			return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "content-length-range bounds must be integers.")
		}
		if err := json.Unmarshal(arr[2], &hi); err != nil {
			return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "content-length-range bounds must be integers.")
		}
		if lo < 0 || hi < lo {
			return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "content-length-range bounds are inconsistent.")
		}
		return PostCondition{Operator: ConditionLengthRange, Min: lo, Max: hi}, nil
	case ConditionEq, ConditionStartsWith:
		var key, value string
		if err := json.Unmarshal(arr[1], &key); err != nil || !strings.HasPrefix(key, "$") {
			return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "The condition key must start with '$'.")
		}
		if err := json.Unmarshal(arr[2], &value); err != nil {
			return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "The condition value must be a string.")
		}
		return PostCondition{Operator: strings.ToLower(op), Key: strings.ToLower(strings.TrimPrefix(key, "$")), Value: value}, nil
	default:
		return PostCondition{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidPolicyDocument", "Unknown condition operator "+op+".")
	}
}

// checkPostConditions matches the form against the policy: every condition
// must hold and every form field (but the exempt ones) must be covered.
func checkPostConditions(policy *PostPolicy, form PostForm, fields map[string]string) error {
	covered := map[string]bool{}
	for _, c := range policy.Conditions {
		switch c.Operator {
		case ConditionLengthRange:
			continue
		}
		covered[c.Key] = true
		actual, ok := fields[c.Key]
		if c.Key == "bucket" {
			actual, ok = form.Bucket, true
		}
		if !ok {
			return policyError("Condition check failed: field " + c.Key + " is missing")
		}
		if c.Key == "key" {
			actual = strings.ReplaceAll(actual, "${filename}", form.FileName)
		}
		switch c.Operator {
		case ConditionEq:
			if actual != c.Value {
				return policyError(fmt.Sprintf("Condition failed: [\"eq\", \"$%s\", %q]", c.Key, c.Value))
			}
		case ConditionStartsWith:
			if !strings.HasPrefix(actual, c.Value) {
				return policyError(fmt.Sprintf("Condition failed: [\"starts-with\", \"$%s\", %q]", c.Key, c.Value))
			}
		}
	}
	for name := range fields {
		if policyExemptFields[name] || strings.HasPrefix(name, "x-ignore-") || covered[name] {
			continue
		}
		return policyError("Extra input fields: " + name)
	}
	return nil
}

// LengthRange returns the content-length-range bounds of the policy, if any.
func (p *PostPolicy) LengthRange() (minSize, maxSize int64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	for _, c := range p.Conditions {
		if c.Operator == ConditionLengthRange {
			return c.Min, c.Max, true
		}
	}
	return 0, 0, false
}

// CheckLength validates a file size against the content-length-range.
func (p *PostPolicy) CheckLength(size int64) error {
	lo, hi, ok := p.LengthRange()
	if !ok {
		return nil
	}
	if size < lo {
		return domain.NewS3Error(domain.ErrEntityTooSmall, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed size: "+strconv.FormatInt(lo, 10))
	}
	if size > hi {
		return domain.NewS3Error(domain.ErrEntityTooLarge, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size: "+strconv.FormatInt(hi, 10))
	}
	return nil
}
