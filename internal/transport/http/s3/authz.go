package s3

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
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
)

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
}

// authorize checks the action of the operation selected by the
// Dispatcher (ActionOf, versioned when the request names a versionId) on
// the resource. It writes the error response and returns false when
// access is denied.
func (a *authorizer) authorize(w http.ResponseWriter, r *http.Request, bucketName, object string) bool {
	op, _ := OperationFromContext(r.Context())
	return a.authorizeAction(w, r, ActionOf(op.Name, r.URL.Query().Get("versionId") != ""), bucketName, object)
}

// authorizeAction checks an explicit action; see authorize.
func (a *authorizer) authorizeAction(w http.ResponseWriter, r *http.Request, action policy.Action, bucketName, object string) bool {
	if err := a.prepare(r, bucketName)(action, object); err != nil {
		a.enc.WriteS3Error(w, r, err)
		return false
	}
	return true
}

// checker decides one action on one object of the prepared bucket.
type checker func(action policy.Action, object string) error

// prepare computes what the decisions on a bucket share (identity,
// condition values) once, for the batch operations.
func (a *authorizer) prepare(r *http.Request, bucketName string) checker {
	ctx := r.Context()
	id, ok := middleware.IdentityFromContext(ctx)
	if !ok {
		id = domain.Anonymous()
	}
	now := a.clock.Now()
	conds := requestConditions(r, now)
	return func(action policy.Action, object string) error {
		if action == "" {
			return fmt.Errorf("%w: operation without a policy action", domain.ErrAccessDenied)
		}
		decision, err := a.policy.Evaluate(ctx, policy.Args{
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
