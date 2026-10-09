package s3

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	objectpkg "github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// S3 actions without a constant in the policy package.
const (
	actionGetBucketTagging           policy.Action = "s3:GetBucketTagging"
	actionPutBucketTagging           policy.Action = "s3:PutBucketTagging"
	actionGetObjectVersionTagging    policy.Action = "s3:GetObjectVersionTagging"
	actionPutObjectVersionTagging    policy.Action = "s3:PutObjectVersionTagging"
	actionDeleteObjectVersionTagging policy.Action = "s3:DeleteObjectVersionTagging"
	actionGetBucketACL               policy.Action = "s3:GetBucketAcl"
	actionPutBucketACL               policy.Action = "s3:PutBucketAcl"
	actionGetObjectACL               policy.Action = "s3:GetObjectAcl"
	actionGetObjectVersionACL        policy.Action = "s3:GetObjectVersionAcl"
	actionPutObjectACL               policy.Action = "s3:PutObjectAcl"
	actionPutObjectVersionACL        policy.Action = "s3:PutObjectVersionAcl"
	actionGetBucketCORS              policy.Action = "s3:GetBucketCORS"
	actionPutBucketCORS              policy.Action = "s3:PutBucketCORS"
	actionGetLifecycle               policy.Action = "s3:GetLifecycleConfiguration"
	actionPutLifecycle               policy.Action = "s3:PutLifecycleConfiguration"
	actionGetEncryption              policy.Action = "s3:GetEncryptionConfiguration"
	actionPutEncryption              policy.Action = "s3:PutEncryptionConfiguration"
	actionGetBucketObjectLock        policy.Action = "s3:GetBucketObjectLockConfiguration"
	actionPutBucketObjectLock        policy.Action = "s3:PutBucketObjectLockConfiguration"
	actionGetBucketNotification      policy.Action = "s3:GetBucketNotification"
	actionPutBucketNotification      policy.Action = "s3:PutBucketNotification"
	// actionForceDeleteBucket is required on top of s3:DeleteBucket to
	// delete a non-empty bucket (force-delete header).
	actionForceDeleteBucket policy.Action = "s3:ForceDeleteBucket"

	// Object Lock and GetObjectAttributes.
	actionGetObjectRetention         policy.Action = "s3:GetObjectRetention"
	actionPutObjectRetention         policy.Action = "s3:PutObjectRetention"
	actionGetObjectLegalHold         policy.Action = "s3:GetObjectLegalHold"
	actionPutObjectLegalHold         policy.Action = "s3:PutObjectLegalHold"
	actionBypassGovernanceRetention  policy.Action = "s3:BypassGovernanceRetention"
	actionGetObjectAttributes        policy.Action = "s3:GetObjectAttributes"
	actionGetObjectVersionAttributes policy.Action = "s3:GetObjectVersionAttributes"
)

// keyExistingObjectTagPrefix is the condition key of the tags of the
// object a request addresses (s3:ExistingObjectTag/<key>).
const keyExistingObjectTagPrefix = "s3:ExistingObjectTag/"

// existingTagActions are the actions S3 evaluates with the
// s3:ExistingObjectTag/<key> condition keys: the tags of the addressed
// version are looked up before deciding them.
func existingTagActions() map[policy.Action]bool {
	return map[policy.Action]bool{
		policy.ActionGetObject: true, policy.ActionGetObjectVersion: true,
		policy.ActionGetObjectTagging: true, actionGetObjectVersionTagging: true,
		policy.ActionPutObjectTagging: true, actionPutObjectVersionTagging: true,
		policy.ActionDeleteObjectTagging: true, actionDeleteObjectVersionTagging: true,
		actionGetObjectACL: true, actionGetObjectVersionACL: true, actionPutObjectACL: true, actionPutObjectVersionACL: true,
		actionGetObjectRetention: true, actionPutObjectRetention: true,
		actionGetObjectLegalHold: true, actionPutObjectLegalHold: true, actionBypassGovernanceRetention: true,
		actionGetObjectAttributes: true, actionGetObjectVersionAttributes: true,
	}
}

// presignedPrefix prefixes the names of the operations reached through a
// presigned URL (PresignedGetObject...): they share the action of the
// plain operation.
const presignedPrefix = "Presigned"

// ActionOf returns the policy action that authorizes an S3 operation (by
// name, as in Operation.Name). versioned selects the version-specific
// action of the operations addressing an explicit version (versionId):
// GetObjectVersion, DeleteObjectVersion, *ObjectVersionTagging. Operations
// that need a second action (CopyObject and UploadPartCopy read the
// source with s3:GetObject, a forced DeleteBucket needs
// s3:ForceDeleteBucket) authorize it separately. Unknown operations map to
// "", which no policy grants.
func ActionOf(operation string, versioned bool) policy.Action {
	operation = strings.TrimPrefix(operation, presignedPrefix)
	pick := func(plain, version policy.Action) policy.Action {
		if versioned {
			return version
		}
		return plain
	}
	switch operation {
	case "ListBuckets":
		return policy.ActionListAllMyBuckets
	case "CreateBucket":
		return policy.ActionCreateBucket
	case "DeleteBucket":
		return policy.ActionDeleteBucket
	case "HeadBucket", "ListObjects", "ListObjectsV2":
		return policy.ActionListBucket
	case "ListObjectVersions":
		return policy.ActionListBucketVersions
	case "ListMultipartUploads":
		return policy.ActionListMultipart
	case "GetBucketLocation":
		return policy.ActionGetBucketLocation
	case "GetBucketPolicy":
		return policy.ActionGetBucketPolicy
	case "PutBucketPolicy":
		return policy.ActionPutBucketPolicy
	case "DeleteBucketPolicy":
		return policy.ActionDeleteBucketPolicy
	case "GetBucketPolicyStatus":
		return policy.ActionGetBucketPolicyStatus
	case "GetBucketVersioning":
		return policy.ActionGetBucketVersioning
	case "PutBucketVersioning":
		return policy.ActionPutBucketVersioning
	case "GetBucketTagging":
		return actionGetBucketTagging
	case "PutBucketTagging", "DeleteBucketTagging":
		return actionPutBucketTagging
	case "GetBucketCors":
		return actionGetBucketCORS
	case "PutBucketCors", "DeleteBucketCors":
		return actionPutBucketCORS
	case "GetBucketLifecycle":
		return actionGetLifecycle
	case "PutBucketLifecycle", "DeleteBucketLifecycle":
		return actionPutLifecycle
	case "GetBucketEncryption":
		return actionGetEncryption
	case "PutBucketEncryption", "DeleteBucketEncryption":
		return actionPutEncryption
	case "GetObjectLockConfiguration":
		return actionGetBucketObjectLock
	case "PutObjectLockConfiguration":
		return actionPutBucketObjectLock
	case "GetBucketNotification":
		return actionGetBucketNotification
	case "PutBucketNotification":
		return actionPutBucketNotification
	case "GetBucketAcl":
		return actionGetBucketACL
	case "PutBucketAcl":
		return actionPutBucketACL
	case "GetObject", "HeadObject":
		return pick(policy.ActionGetObject, policy.ActionGetObjectVersion)
	case "PutObject", "CopyObject", "PostObject", "CreateMultipartUpload", "UploadPart", "UploadPartCopy", "CompleteMultipartUpload":
		return policy.ActionPutObject
	case "DeleteObject", "DeleteObjects":
		return pick(policy.ActionDeleteObject, policy.ActionDeleteObjectVersion)
	case "AbortMultipartUpload":
		return policy.ActionAbortMultipart
	case "ListParts":
		return policy.ActionListMultipartParts
	case "GetObjectTagging":
		return pick(policy.ActionGetObjectTagging, actionGetObjectVersionTagging)
	case "PutObjectTagging":
		return pick(policy.ActionPutObjectTagging, actionPutObjectVersionTagging)
	case "DeleteObjectTagging":
		return pick(policy.ActionDeleteObjectTagging, actionDeleteObjectVersionTagging)
	case "GetObjectAcl":
		return pick(actionGetObjectACL, actionGetObjectVersionACL)
	case "PutObjectAcl":
		return pick(actionPutObjectACL, actionPutObjectVersionACL)
	case "GetObjectRetention":
		return actionGetObjectRetention
	case "PutObjectRetention":
		return actionPutObjectRetention
	case "GetObjectLegalHold":
		return actionGetObjectLegalHold
	case "PutObjectLegalHold":
		return actionPutObjectLegalHold
	case "GetObjectAttributes":
		return pick(actionGetObjectAttributes, actionGetObjectVersionAttributes)
	default:
		return ""
	}
}

// authorizer evaluates the policy engine for the S3 handlers. The bucket
// policy is resolved by the engine (policy.Deps.Buckets): one source and
// one parser of the stored documents.
type authorizer struct {
	policy policy.Service
	enc    httpx.Encoder
	clock  clock.Clock
	// objects, when set, provides the tags of the addressed objects for
	// the s3:ExistingObjectTag/<key> condition keys.
	objects objectpkg.Service
}

// authorize checks the action of the operation selected by the
// Dispatcher (ActionOf, versioned when the request names a versionId) on
// the resource. It writes the error response and returns false when
// access is denied.
func (a *authorizer) authorize(w http.ResponseWriter, r *http.Request, bucketName, object string) bool {
	op, _ := OperationFromContext(r.Context())
	return a.authorizeAction(w, r, ActionOf(op.Name, r.URL.Query().Get("versionId") != ""), bucketName, object)
}

// authorizeAction checks an explicit action on the version the request
// names (versionId); see authorize.
func (a *authorizer) authorizeAction(w http.ResponseWriter, r *http.Request, action policy.Action, bucketName, object string) bool {
	return a.authorizeVersion(w, r, action, bucketName, object, r.URL.Query().Get("versionId"), nil)
}

// authorizeVersion checks action on one version of object (the source of
// a copy names its own), with extra condition values (the tags of a
// PutObjectTagging body); see authorize.
func (a *authorizer) authorizeVersion(w http.ResponseWriter, r *http.Request, action policy.Action, bucketName, object, versionID string, extra map[string][]string) bool {
	if err := a.check(r, action, bucketName, object, versionID, extra); err != nil {
		a.enc.WriteS3Error(w, r, err)
		return false
	}
	return true
}

// check decides action on one version of object without writing a
// response. For the actions S3 evaluates with the
// s3:ExistingObjectTag/<key> condition keys, the tags of the version are
// looked up when a statement that could apply to the request uses them
// (probed with the simulator, which costs no I/O): the decision is then
// taken again with their values.
func (a *authorizer) check(r *http.Request, action policy.Action, bucketName, object, versionID string, extra map[string][]string) error {
	return a.decide(r, bucketName, extra, func(ctx context.Context, args policy.Args) (policy.Decision, error) {
		decision, err := a.policy.Evaluate(ctx, args)
		if err != nil || !a.tagsMayMatter(ctx, args) {
			return decision, err
		}
		tags := a.existingTags(ctx, bucketName, object, versionID)
		if len(tags) == 0 {
			return decision, nil
		}
		conds := make(map[string][]string, len(args.Conditions)+len(tags))
		for k, v := range args.Conditions {
			conds[k] = v
		}
		for k, v := range tags {
			conds[k] = v
		}
		args.Conditions = conds
		return a.policy.Evaluate(ctx, args)
	})(action, object)
}

// tagsMayMatter reports whether the decision of args may depend on the
// tags of the object: an action evaluated with s3:ExistingObjectTag/<key>,
// by an identity the policies restrict, with a statement using such a key
// that applies or fails on its conditions only.
func (a *authorizer) tagsMayMatter(ctx context.Context, args policy.Args) bool {
	if a.objects == nil || args.Object == "" || !existingTagActions()[args.Action] {
		return false
	}
	if id := args.Identity; id != nil && id.IsRoot && id.SessionPolicy == nil {
		return false
	}
	probe, err := a.policy.Simulate(ctx, args)
	if err != nil {
		return true
	}
	for _, ev := range probe.Evaluated {
		if ev.Statement == nil || (ev.Mismatch != policy.MismatchNone && ev.Mismatch != policy.MismatchCondition) {
			continue
		}
		for _, keys := range ev.Statement.Condition {
			for key := range keys {
				if strings.HasPrefix(strings.ToLower(key), strings.ToLower(keyExistingObjectTagPrefix)) {
					return true
				}
			}
		}
	}
	return false
}

// existingTags returns the s3:ExistingObjectTag/<key> values of a version
// (none for a missing object). The tags are read without the namespace
// lock: the decision is taken before the operation, as S3 does.
func (a *authorizer) existingTags(ctx context.Context, bucketName, object, versionID string) map[string][]string {
	info, err := a.objects.GetObjectInfo(ctx, bucketName, object, objectpkg.GetObjectOptions{VersionID: versionID, NoLock: true})
	if err != nil || len(info.Tags) == 0 {
		return nil
	}
	out := make(map[string][]string, len(info.Tags))
	for k, v := range info.Tags {
		out[keyExistingObjectTagPrefix+k] = []string{v}
	}
	return out
}

// allowed reports whether the request may perform action on the version
// of object (no response is written).
func (a *authorizer) allowed(r *http.Request, action policy.Action, bucketName, object, versionID string) bool {
	return a.check(r, action, bucketName, object, versionID, nil) == nil
}

// bypassGovernance reports whether a request may bypass the GOVERNANCE
// retention of object: it asks for it (x-amz-bypass-governance-retention:
// true) and is allowed s3:BypassGovernanceRetention.
func (a *authorizer) bypassGovernance(r *http.Request, bucketName, object, versionID string) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get(httpx.HeaderAmzBypassGovernance)), "true") {
		return false
	}
	return a.allowed(r, actionBypassGovernanceRetention, bucketName, object, versionID)
}

// checker decides one action on one object of the prepared bucket.
type checker func(action policy.Action, object string) error

// prepare computes what the decisions on a bucket share (identity,
// condition values) once, for the batch operations.
func (a *authorizer) prepare(r *http.Request, bucketName string) checker {
	return a.decide(r, bucketName, nil, func(ctx context.Context, args policy.Args) (policy.Decision, error) {
		return a.policy.Evaluate(ctx, args)
	})
}

// decide is prepare with extra condition values and the evaluation
// function.
func (a *authorizer) decide(r *http.Request, bucketName string, extra map[string][]string,
	evaluate func(ctx context.Context, args policy.Args) (policy.Decision, error),
) checker {
	ctx := r.Context()
	id, ok := middleware.IdentityFromContext(ctx)
	if !ok {
		id = domain.Anonymous()
	}
	now := a.clock.Now()
	conds := requestConditions(r, now)
	for k, v := range extra {
		conds[k] = v
	}
	return func(action policy.Action, object string) error {
		if action == "" {
			return fmt.Errorf("%w: operation without a policy action", domain.ErrAccessDenied)
		}
		decision, err := evaluate(ctx, policy.Args{
			Identity: id, Action: action, Bucket: bucketName, Object: object,
			Conditions: conds, Now: now,
		})
		if err != nil {
			return err
		}
		if !decision.Allowed {
			return fmt.Errorf("%w: %s", domain.ErrAccessDenied, decision.Reason)
		}
		return nil
	}
}

// requestConditions builds the condition values of a request with the
// shared definition of the condition keys (policy.RequestConditions): the
// client, the time, the listing and version parameters, the x-amz-*
// headers, the requested tags, the authentication type and the signature
// age. The Admin API describes the S3 requests of the console with the
// same definition, so a policy decides alike through both. The client
// (aws:SourceIp, aws:SecureTransport) is the one behind the trusted
// proxies (middleware.NewForwarded, mounted on both listeners), or the
// peer of the connection without the middleware.
func requestConditions(r *http.Request, now time.Time) map[string][]string {
	client := middleware.ClientOf(r)
	q := r.URL.Query()
	params := map[string]string{}
	for _, name := range []string{"prefix", "delimiter", "max-keys", "versionId"} {
		if q.Has(name) {
			params[name] = q.Get(name)
		}
	}
	authType, signedAt := authInfo(r, q)
	return policy.RequestConditions(policy.Request{
		Client:   &policy.RequestClient{IP: client.IP, Secure: client.Secure, UserAgent: r.UserAgent(), Referer: r.Referer()},
		Now:      now,
		Params:   params,
		Headers:  r.Header,
		AuthType: authType,
		SignedAt: signedAt,
		// The keys of the access rules (docs/10 §10.1): the host the
		// client addressed (forwarded by the trusted proxies), the method
		// and the headers.
		Host:        client.Host,
		Method:      r.Method,
		HTTPHeaders: r.Header,
	})
}

// authInfo returns the s3:authType of a request and the time it was
// signed at (zero for anonymous requests).
func authInfo(r *http.Request, q url.Values) (domain.AuthType, time.Time) {
	if op, ok := OperationFromContext(r.Context()); ok && op.Name == "PostObject" {
		return domain.AuthTypePost, time.Time{}
	}
	switch {
	case auth.IsPresigned(q):
		return domain.AuthTypeQuery, parseAmzDate(q.Get("X-Amz-Date"))
	case auth.IsSigned(r):
		return domain.AuthTypeHeader, parseAmzDate(r.Header.Get(httpx.HeaderAmzDate))
	default:
		return "", time.Time{}
	}
}

// parseAmzDate parses an ISO 8601 basic timestamp (zero when invalid).
func parseAmzDate(s string) time.Time {
	t, err := time.Parse("20060102T150405Z", s)
	if err != nil {
		return time.Time{}
	}
	return t
}
