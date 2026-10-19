package admin

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// defaultPostPolicyExpiry is the validity of a POST policy without an
// explicit one.
const defaultPostPolicyExpiry = time.Hour

// ShareManagerDeps are the dependencies of the share manager.
type ShareManagerDeps struct {
	// Shares manages the share links (required).
	Shares share.Service
	// Signer presigns the raw URLs and POST policies (required).
	Signer auth.Signer
	// Creds resolves the secret of the requester, which never leaves the
	// server (required).
	Creds auth.CredentialLookup
	// Authz authorizes the shared operations (required).
	Authz Authorizer
	// Objects lists the objects of a prefix share (required).
	Objects object.Service
	Clock   clock.Clock
	Logger  *slog.Logger
}

// ShareManagerOption configures the share manager.
type ShareManagerOption func(*shareManager)

// WithServerURL sets the public URL of the S3 API the presigned URLs and
// POST forms point to (default "http://localhost:9000"); see
// share.WithServerURL for the accepted form.
func WithServerURL(raw string) ShareManagerOption {
	return func(m *shareManager) { m.serverURL = strings.TrimSuffix(strings.TrimSpace(raw), "/") }
}

// WithRegion sets the signing region (default: the signer's).
func WithRegion(region string) ShareManagerOption {
	return func(m *shareManager) { m.region = region }
}

type shareManager struct {
	deps      ShareManagerDeps
	serverURL string
	region    string
}

var _ ShareManager = (*shareManager)(nil)

// NewShareManager returns the share manager. Missing dependencies and an
// invalid server URL are a wiring error (domain.ErrInvalidArgument).
func NewShareManager(deps ShareManagerDeps, opts ...ShareManagerOption) (ShareManager, error) {
	if deps.Shares == nil || deps.Signer == nil || deps.Creds == nil || deps.Authz == nil || deps.Objects == nil {
		return nil, fmt.Errorf("%w: share manager requires the share, signer, credential, authorizer and object services", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &shareManager{deps: deps, serverURL: "http://localhost:9000"}
	for _, o := range opts {
		o(m)
	}
	u, err := url.Parse(m.serverURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, fmt.Errorf("%w: share manager: server URL %q must be http(s)://host[:port][/path]", domain.ErrInvalidArgument, m.serverURL)
	}
	return m, nil
}

// Create implements ShareManager.
func (m *shareManager) Create(ctx context.Context, in CreateShareInput) (*share.Share, error) {
	if in.Permission == "" {
		in.Permission = share.PermissionDownload
	}
	action := policy.ActionGetObject
	switch {
	case in.Permission == share.PermissionUpload:
		action = policy.ActionPutObject
	case in.VersionID != "":
		action = policy.ActionGetObjectVersion
	}
	object := in.Key
	if in.Prefix {
		object += "*"
	}
	if in.Bucket == "" {
		return nil, fmt.Errorf("%w: bucket is required", domain.ErrInvalidArgument)
	}
	if object != "" {
		// The link is used as a presigned URL of the requested version.
		or := ObjectRequest{VersionID: in.VersionID, AuthType: domain.AuthTypeQuery}
		if err := authorizeObject(ctx, m.deps.Authz, in.Requester, in.Bucket, object, or, action); err != nil {
			return nil, err
		}
	}
	return m.deps.Shares.Create(ctx, share.CreateInput{
		Owner: in.Requester.Identity, Bucket: in.Bucket, Key: in.Key, Prefix: in.Prefix, Permission: in.Permission,
		Expires: in.Expires, ContentDisposition: in.ContentDisposition, ContentType: in.ContentType, VersionID: in.VersionID,
		Stream: in.Stream, Image: in.Image,
	})
}

// caller returns the identity to present to the share service: the
// system actor for requesters allowed admin:ManageShares, the requester
// itself (own shares only) otherwise. An explicit deny of
// admin:ManageShares denies even the own shares.
func (m *shareManager) caller(ctx context.Context, req Requester) (domain.Identity, error) {
	d, err := m.deps.Authz.Authorize(ctx, AuthorizeInput{Requester: req, Actions: []policy.Action{policy.ActionAdminManageShares}, SelfService: true})
	if err != nil {
		return domain.Identity{}, err
	}
	if !d.Allowed {
		return domain.Identity{}, denied(d)
	}
	return elevate(req, d), nil
}

// Get implements ShareManager.
func (m *shareManager) Get(ctx context.Context, req Requester, id string) (*share.Share, error) {
	caller, err := m.caller(ctx, req)
	if err != nil {
		return nil, err
	}
	return m.deps.Shares.Get(ctx, caller, id)
}

// List implements ShareManager.
func (m *shareManager) List(ctx context.Context, in ListSharesInput) (*Paged[share.Share], error) {
	if _, err := pageLimit(in.Page); err != nil {
		return nil, err
	}
	caller, err := m.caller(ctx, in.Requester)
	if err != nil {
		return nil, err
	}
	shares, err := m.deps.Shares.List(ctx, caller, share.ListFilter{Owner: in.Owner, Bucket: in.Bucket})
	if err != nil {
		return nil, err
	}
	return pageOf(shares, shareOrder, in.Page)
}

// shareOrder is the sort key of the share links: newest first, then by id.
func shareOrder(s share.Share) string {
	return fmt.Sprintf("%020d/%s", math.MaxInt64-s.CreatedAt.UnixNano(), s.ID)
}

// Links implements ShareManager.
func (m *shareManager) Links(ctx context.Context, in ShareLinksInput) (*ShareLinks, error) {
	limit, err := pageLimit(in.Page)
	if err != nil {
		return nil, err
	}
	caller, err := m.caller(ctx, in.Requester)
	if err != nil {
		return nil, err
	}
	sh, err := m.deps.Shares.Get(ctx, caller, in.ID)
	if err != nil {
		return nil, err
	}
	if !sh.Prefix || sh.Permission != share.PermissionDownload {
		return nil, fmt.Errorf("%w: share %s is not a prefix download share", domain.ErrInvalidArgument, sh.ID)
	}
	// Enumerating the prefix is a listing of the requester: the share
	// does not let it see more than it may list itself.
	d, err := m.deps.Authz.Authorize(ctx, AuthorizeInput{
		Requester: in.Requester, Actions: []policy.Action{policy.ActionListBucket}, Bucket: sh.Bucket,
		Conditions: ListingConditions(sh.Key, "", limit),
	})
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		return nil, denied(d)
	}
	res, err := m.deps.Objects.ListObjects(ctx, domain.ListObjectsInput{
		Bucket: sh.Bucket, Prefix: sh.Key, ContinuationToken: in.Page.Cursor, MaxKeys: limit,
	})
	if err != nil {
		return nil, err
	}
	out := &ShareLinks{Share: sh, Items: make([]ShareLink, 0, len(res.Objects))}
	for _, o := range res.Objects {
		if strings.HasSuffix(o.Key, "/") {
			continue // folder placeholder
		}
		u, err := m.deps.Shares.Presign(ctx, caller, sh.ID, o.Key)
		if err != nil {
			return nil, fmt.Errorf("admin: link of %s: %w", o.Key, err)
		}
		out.Items = append(out.Items, ShareLink{Key: o.Key, Size: o.Size, URL: u.String()})
	}
	if res.IsTruncated {
		out.NextCursor = res.NextContinuationToken
	}
	return out, nil
}

// Revoke implements ShareManager.
func (m *shareManager) Revoke(ctx context.Context, req Requester, id string) error {
	caller, err := m.caller(ctx, req)
	if err != nil {
		return err
	}
	return m.deps.Shares.Revoke(ctx, caller, id)
}

// RevokeAll implements ShareManager.
func (m *shareManager) RevokeAll(ctx context.Context, req Requester, owner string) (int, error) {
	caller, err := m.caller(ctx, req)
	if err != nil {
		return 0, err
	}
	if owner == "" {
		owner = req.Identity.EffectiveUser()
	}
	return m.deps.Shares.RevokeAll(ctx, caller, owner)
}

// presignAction is the action a presigned method performs.
func presignAction(method string, versioned bool) (policy.Action, error) {
	switch method {
	case MethodGet, MethodHead:
		if versioned {
			return policy.ActionGetObjectVersion, nil
		}
		return policy.ActionGetObject, nil
	case MethodPut:
		return policy.ActionPutObject, nil
	case MethodDelete:
		if versioned {
			return policy.ActionDeleteObjectVersion, nil
		}
		return policy.ActionDeleteObject, nil
	default:
		return "", fmt.Errorf("%w: method must be GET, PUT, HEAD or DELETE, not %q", domain.ErrInvalidArgument, method)
	}
}

// validity checks a requested validity (0 means def) against the 1 s to
// 7 days range of SigV4.
func validity(d, def time.Duration) (time.Duration, error) {
	if d == 0 {
		d = def
	}
	if d < time.Second || d > share.MaxExpiry {
		return 0, fmt.Errorf("%w: expiry must be between 1 second and %s", domain.ErrInvalidArgument, share.MaxExpiry)
	}
	return d.Truncate(time.Second), nil
}

// credentials returns the signing credentials of the requester and caps
// the validity at their expiration.
func (m *shareManager) credentials(ctx context.Context, req Requester, d time.Duration) (domain.Credentials, time.Duration, error) {
	id := req.Identity
	if id.IsAnonymous || id.AccessKey == "" {
		return domain.Credentials{}, 0, fmt.Errorf("%w: anonymous requesters cannot sign", domain.ErrAccessDenied)
	}
	creds, _, err := m.deps.Creds.LookupCredentials(ctx, id.AccessKey)
	if err != nil {
		return domain.Credentials{}, 0, fmt.Errorf("admin: signing credentials: %w", err)
	}
	if !creds.Expiration.IsZero() {
		left := creds.Expiration.Sub(m.deps.Clock.Now()).Truncate(time.Second)
		if left < time.Second {
			return domain.Credentials{}, 0, fmt.Errorf("%w: the credentials of the requester have expired", domain.ErrAccessDenied)
		}
		d = min(d, left)
	}
	return creds, d, nil
}

// Presign implements ShareManager.
func (m *shareManager) Presign(ctx context.Context, in PresignInput) (*SignedURL, error) {
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		method = MethodGet
	}
	action, err := presignAction(method, in.VersionID != "")
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return nil, err
	}
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return nil, err
	}
	expires, err := validity(in.Expires, share.MaxExpiry)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	signed := map[string]string{}
	for name, value := range in.Headers {
		lower := strings.ToLower(strings.TrimSpace(name))
		switch {
		case lower == "" || strings.ContainsAny(lower, " \t\r\n:"):
			return nil, fmt.Errorf("%w: invalid header name %q", domain.ErrInvalidArgument, name)
		case lower == "host" || lower == "authorization" || strings.HasPrefix(lower, "x-amz-security-token"):
			return nil, fmt.Errorf("%w: header %s cannot be presigned", domain.ErrInvalidArgument, name)
		}
		headers.Set(lower, value)
		signed[lower] = value
	}
	// The URL is checked as the S3 API will evaluate its use: the version,
	// the signed x-amz-* headers and the query string authentication.
	or := ObjectRequest{VersionID: in.VersionID, Headers: signed, AuthType: domain.AuthTypeQuery}
	if err := authorizeObject(ctx, m.deps.Authz, in.Requester, in.Bucket, in.Key, or, action); err != nil {
		return nil, err
	}
	creds, expires, err := m.credentials(ctx, in.Requester, expires)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if in.VersionID != "" {
		q.Set("versionId", in.VersionID)
	}
	switch method {
	case MethodPut:
		if in.ContentType != "" {
			headers.Set("content-type", in.ContentType)
			signed["content-type"] = in.ContentType
		}
	case MethodGet:
		if in.ContentType != "" {
			q.Set("response-content-type", in.ContentType)
		}
		if in.ContentDisposition != "" {
			q.Set("response-content-disposition", in.ContentDisposition)
		}
	}
	u, err := m.objectURL(in.Bucket, in.Key)
	if err != nil {
		return nil, err
	}
	u.RawQuery = q.Encode()
	now := m.deps.Clock.Now().UTC().Truncate(time.Second)
	out, err := m.deps.Signer.Presign(ctx, auth.PresignInput{Method: method, URL: u, Headers: headers, Expires: expires, Creds: creds, Region: m.region, Now: now})
	if err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: raw presigned URL", "method", method, "bucket", in.Bucket, "key", in.Key,
		"by", in.Requester.Identity.AccessKey, "expires", expires)
	return &SignedURL{URL: out.String(), Method: method, Expires: now.Add(expires), Headers: signed}, nil
}

// objectURL is the path-style URL of a bucket or object on the public S3
// endpoint.
func (m *shareManager) objectURL(bucket, key string) (*url.URL, error) {
	u, err := url.Parse(m.serverURL)
	if err != nil {
		return nil, fmt.Errorf("%w: server URL: %w", domain.ErrInvalidArgument, err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = ""
	return u, nil
}

// PostPolicy implements ShareManager.
func (m *shareManager) PostPolicy(ctx context.Context, in PostPolicyInput) (*PostPolicyForm, error) {
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return nil, err
	}
	if in.Key == "" && in.KeyPrefix == "" {
		return nil, fmt.Errorf("%w: key or keyPrefix is required", domain.ErrInvalidArgument)
	}
	if err := domain.ValidateObjectPrefix(in.KeyPrefix); err != nil {
		return nil, err
	}
	if in.MinSize < 0 || in.MaxSize < 0 || (in.MaxSize > 0 && in.MinSize > in.MaxSize) {
		return nil, fmt.Errorf("%w: invalid size range [%d, %d]", domain.ErrInvalidArgument, in.MinSize, in.MaxSize)
	}
	switch in.SuccessActionStatus {
	case 0, http.StatusOK, http.StatusCreated, http.StatusNoContent:
	default:
		return nil, fmt.Errorf("%w: successActionStatus must be 200, 201 or 204", domain.ErrInvalidArgument)
	}
	expires, err := validity(in.Expires, defaultPostPolicyExpiry)
	if err != nil {
		return nil, err
	}
	resource := in.Key
	if in.KeyPrefix != "" {
		resource = in.KeyPrefix + "*"
	}
	if err := authorizeObject(ctx, m.deps.Authz, in.Requester, in.Bucket, resource, ObjectRequest{AuthType: domain.AuthTypePost}, policy.ActionPutObject); err != nil {
		return nil, err
	}
	creds, expires, err := m.credentials(ctx, in.Requester, expires)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	for k, v := range in.Fields {
		fields[k] = v
	}
	if in.ContentType != "" {
		fields["Content-Type"] = in.ContentType
	}
	if in.SuccessActionStatus != 0 {
		fields["success_action_status"] = strconv.Itoa(in.SuccessActionStatus)
	}
	if in.SuccessActionRedirect != "" {
		fields["success_action_redirect"] = in.SuccessActionRedirect
	}
	var conds []auth.PostCondition
	if in.ContentTypePrefix != "" {
		conds = append(conds, auth.PostCondition{Operator: auth.ConditionStartsWith, Key: "content-type", Value: in.ContentTypePrefix})
	}
	if in.MaxSize > 0 {
		conds = append(conds, auth.PostCondition{Operator: auth.ConditionLengthRange, Min: in.MinSize, Max: in.MaxSize})
	}
	form, err := m.deps.Signer.PresignPostPolicy(ctx, auth.PostPolicyInput{
		Bucket: in.Bucket, Key: in.Key, KeyPrefix: in.KeyPrefix, Fields: fields, Conditions: conds,
		Expires: expires, Creds: creds, Region: m.region, Now: m.deps.Clock.Now().UTC().Truncate(time.Second),
	})
	if err != nil {
		return nil, err
	}
	u, err := m.objectURL(in.Bucket, "")
	if err != nil {
		return nil, err
	}
	out := &PostPolicyForm{URL: u.String(), Fields: form.Fields, Expiration: form.Expiration, Conditions: make([]PostCondition, 0, len(form.Policy.Conditions))}
	for _, c := range form.Policy.Conditions {
		out.Conditions = append(out.Conditions, PostCondition(c))
	}
	return out, nil
}
