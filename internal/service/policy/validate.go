package policy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// s3Actions lists the S3 actions a policy may name (AWS IAM actions of the
// s3 service plus the MinIO extensions). Unknown S3 actions are rejected,
// as S3 does for bucket policies, so that a typo never silently disables a
// statement; wildcard patterns must match at least one of them.
func s3Actions() []string {
	return []string{
		"AbortMultipartUpload", "BypassGovernanceRetention", "CreateBucket", "DeleteBucket",
		"DeleteBucketCORS", "DeleteBucketEncryption", "DeleteBucketLifecycle", "DeleteBucketOwnershipControls",
		"DeleteBucketPolicy", "DeleteBucketReplication", "DeleteBucketTagging", "DeleteBucketWebsite",
		"DeleteObject", "DeleteObjectTagging", "DeleteObjectVersion", "DeleteObjectVersionTagging",
		"ForceDeleteBucket", "GetAccelerateConfiguration", "GetAnalyticsConfiguration", "GetBucketAcl",
		"GetBucketCORS", "GetBucketLocation", "GetBucketLogging", "GetBucketNotification",
		"GetBucketObjectLockConfiguration", "GetBucketOwnershipControls", "GetBucketPolicy",
		"GetBucketPolicyStatus", "GetBucketPublicAccessBlock", "GetBucketRequestPayment", "GetBucketTagging",
		"GetBucketVersioning", "GetBucketWebsite", "GetEncryptionConfiguration",
		"GetIntelligentTieringConfiguration", "GetInventoryConfiguration", "GetLifecycleConfiguration",
		"GetMetricsConfiguration", "GetObject", "GetObjectAcl", "GetObjectAttributes", "GetObjectLegalHold",
		"GetObjectRetention", "GetObjectTagging", "GetObjectTorrent", "GetObjectVersion", "GetObjectVersionAcl",
		"GetObjectVersionAttributes", "GetObjectVersionForReplication", "GetObjectVersionTagging",
		"GetObjectVersionTorrent", "GetReplicationConfiguration", "ListAllMyBuckets", "ListBucket",
		"ListBucketMultipartUploads", "ListBucketVersions", "ListMultipartUploadParts", "ListenBucketNotification",
		"ListenNotification", "ObjectOwnerOverrideToBucketOwner", "PutAccelerateConfiguration",
		"PutAnalyticsConfiguration", "PutBucketAcl", "PutBucketCORS", "PutBucketLogging", "PutBucketNotification",
		"PutBucketObjectLockConfiguration", "PutBucketOwnershipControls", "PutBucketPolicy",
		"PutBucketPublicAccessBlock", "PutBucketRequestPayment", "PutBucketTagging", "PutBucketVersioning",
		"PutBucketWebsite", "PutEncryptionConfiguration", "PutIntelligentTieringConfiguration",
		"PutInventoryConfiguration", "PutLifecycleConfiguration", "PutMetricsConfiguration", "PutObject",
		"PutObjectAcl", "PutObjectLegalHold", "PutObjectRetention", "PutObjectTagging", "PutObjectVersionAcl",
		"PutObjectVersionTagging", "PutReplicationConfiguration", "ReplicateDelete", "ReplicateObject",
		"ReplicateTags", "ResetBucketReplicationState", "RestoreObject",
	}
}

// validAction checks one Action/NotAction entry: "*" or
// "<service>:<name>" with service s3, admin, sts or kms. S3 names must be
// known actions or wildcard patterns matching one.
func validAction(a string) error {
	if a == "*" {
		return nil
	}
	service, name, ok := strings.Cut(a, ":")
	if !ok || name == "" {
		return fmt.Errorf("invalid action %q (expected <service>:<action>)", a)
	}
	for _, r := range name {
		if !isActionRune(r) {
			return fmt.Errorf("invalid action %q", a)
		}
	}
	switch strings.ToLower(service) {
	case "admin", "sts", "kms":
		return nil
	case "s3":
	default:
		return fmt.Errorf("invalid action %q: unknown service %q (s3, admin, sts, kms)", a, service)
	}
	for _, known := range s3Actions() {
		if matchWildcard(name, known, true) {
			return nil
		}
	}
	return fmt.Errorf("unknown S3 action %q", a)
}

// isActionRune reports whether r may appear in an action name pattern.
func isActionRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("*?-_", r)
	}
}

// isS3Action reports whether an action pattern may match S3 actions.
func isS3Action(a string) bool {
	return a == "*" || strings.HasPrefix(strings.ToLower(a), "s3:")
}

// validResource checks one Resource/NotResource entry: "*" or an S3 ARN
// (arn:aws:s3:::bucket[/key]) whose variables are well formed.
func validResource(r string) error {
	if r == "*" {
		return nil
	}
	rest, ok := strings.CutPrefix(r, "arn:aws:s3:::")
	if !ok || rest == "" {
		return fmt.Errorf("invalid resource %q (expected arn:aws:s3:::bucket[/key])", r)
	}
	if bad, ok := checkVariables(r); !ok {
		return fmt.Errorf("invalid policy variable %q in resource %q", bad, r)
	}
	return nil
}

// validPrincipal checks a Principal/NotPrincipal element.
func validPrincipal(p *domain.Principal) error {
	if len(p.AWS)+len(p.Service)+len(p.Federated) == 0 {
		return fmt.Errorf("must name at least one principal")
	}
	for _, set := range []domain.StringSet{p.AWS, p.Service, p.Federated} {
		for _, v := range set {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("principals must not be empty")
			}
		}
	}
	return nil
}

// validCondition checks the operators, keys and values of a Condition
// element; it returns the element path and the problem.
func validCondition(cond domain.Condition) (string, error) {
	for _, op := range sortedKeys(cond) {
		element := "Condition." + op
		parsed, ok := parseOperator(op)
		if !ok {
			return element, fmt.Errorf("unknown condition operator %q", op)
		}
		keys := cond[op]
		if len(keys) == 0 {
			return element, fmt.Errorf("operator %s has no condition keys", op)
		}
		for _, key := range sortedKeys(keys) {
			if strings.TrimSpace(key) == "" || strings.ContainsAny(key, " \t\n${}") {
				return element, fmt.Errorf("invalid condition key %q", key)
			}
			values := keys[key]
			if len(values) == 0 {
				return element + "." + key, fmt.Errorf("must have at least one value")
			}
			for _, v := range values {
				if bad, ok := checkVariables(v); !ok {
					return element + "." + key, fmt.Errorf("invalid policy variable %q", bad)
				}
				if !checkConditionValue(parsed.base, v) {
					return element + "." + key, fmt.Errorf("invalid value %q for %s", v, parsed.base)
				}
			}
		}
	}
	return "", nil
}

// validatePolicy checks a decoded document. offsets, when known, locate the
// statements in the JSON text (see Parse).
func validatePolicy(p *domain.Policy, maxSize int) error {
	if p == nil {
		return malformed("policy document is empty")
	}
	if data, err := json.Marshal(p); err == nil && len(data) > maxSize {
		return &ValidationError{Msg: fmt.Sprintf("policy document is %d bytes, the limit is %d", len(data), maxSize), tooLarge: true}
	}
	if p.Version != domain.PolicyVersion {
		return &ValidationError{Element: "Version", Msg: fmt.Sprintf("must be %q", domain.PolicyVersion)}
	}
	if len(p.Statement) == 0 {
		return &ValidationError{Element: "Statement", Msg: "the policy must contain at least one statement"}
	}
	sids := map[string]bool{}
	for i := range p.Statement {
		if err := validateStatement(i, &p.Statement[i], sids); err != nil {
			return err
		}
	}
	return nil
}

func validateStatement(i int, st *domain.Statement, sids map[string]bool) error {
	if st.SID != "" {
		if sids[st.SID] {
			return stmtError(i, st.SID, "Sid", "duplicate Sid %q", st.SID)
		}
		sids[st.SID] = true
	}
	if !st.Effect.IsValid() {
		return stmtError(i, st.SID, "Effect", "must be %q or %q", domain.Allow, domain.Deny)
	}
	if (len(st.Action) == 0) == (len(st.NotAction) == 0) {
		return stmtError(i, st.SID, "Action", "exactly one of Action and NotAction is required")
	}
	if st.Principal != nil && st.NotPrincipal != nil {
		return stmtError(i, st.SID, "Principal", "Principal and NotPrincipal are mutually exclusive")
	}
	for name, p := range map[string]*domain.Principal{"Principal": st.Principal, "NotPrincipal": st.NotPrincipal} {
		if p == nil {
			continue
		}
		if err := validPrincipal(p); err != nil {
			return stmtError(i, st.SID, name, "%s", err.Error())
		}
	}
	needsResource := len(st.NotAction) > 0
	for name, set := range map[string]domain.StringSet{"Action": st.Action, "NotAction": st.NotAction} {
		for _, a := range set {
			if err := validAction(a); err != nil {
				return stmtError(i, st.SID, name, "%s", err.Error())
			}
			if isS3Action(a) {
				needsResource = true
			}
		}
	}
	if len(st.Resource) > 0 && len(st.NotResource) > 0 {
		return stmtError(i, st.SID, "Resource", "Resource and NotResource are mutually exclusive")
	}
	if needsResource && len(st.Resource) == 0 && len(st.NotResource) == 0 {
		return stmtError(i, st.SID, "Resource", "Resource or NotResource is required for S3 actions")
	}
	for name, set := range map[string]domain.StringSet{"Resource": st.Resource, "NotResource": st.NotResource} {
		for _, r := range set {
			if err := validResource(r); err != nil {
				return stmtError(i, st.SID, name, "%s", err.Error())
			}
		}
	}
	if element, err := validCondition(st.Condition); err != nil {
		return stmtError(i, st.SID, element, "%s", err.Error())
	}
	return nil
}

// validateBucketRules checks the rules specific to bucket policies: every
// statement names a principal, uses S3 actions only and targets the bucket
// or its objects.
func validateBucketRules(bucket string, p *domain.Policy) error {
	if bucket == "" || strings.ContainsAny(bucket, "/*?$") {
		return fmt.Errorf("%w: policy: invalid bucket name %q", domain.ErrInvalidArgument, bucket)
	}
	arn := "arn:aws:s3:::" + bucket
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Principal == nil && st.NotPrincipal == nil {
			return stmtError(i, st.SID, "Principal", "bucket policy statements must name a Principal or NotPrincipal")
		}
		for name, set := range map[string]domain.StringSet{"Action": st.Action, "NotAction": st.NotAction} {
			for _, a := range set {
				if !isS3Action(a) {
					return stmtError(i, st.SID, name, "action %q is not an S3 action", a)
				}
			}
		}
		for name, set := range map[string]domain.StringSet{"Resource": st.Resource, "NotResource": st.NotResource} {
			for _, r := range set {
				if r != arn && !strings.HasPrefix(r, arn+"/") {
					return stmtError(i, st.SID, name, "resource %q is outside bucket %s", r, bucket)
				}
			}
		}
	}
	return nil
}

// validateIdentityRules checks the rules specific to identity (IAM and
// inline) policies: no Principal.
func validateIdentityRules(p *domain.Policy) error {
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Principal != nil || st.NotPrincipal != nil {
			return stmtError(i, st.SID, "Principal", "identity policies must not name a Principal")
		}
	}
	return nil
}

// validPolicyName reports whether name is 1 to 128 characters of
// [A-Za-z0-9+=,.@_-], the IAM policy name alphabet, and not "." or "..".
func validPolicyName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("+=,.@_-", r):
		default:
			return false
		}
	}
	return true
}
