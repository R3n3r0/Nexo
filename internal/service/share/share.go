package share

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

const (
	// MaxExpiry is the longest validity of a share link (SigV4 presigned
	// URLs are valid for at most 7 days).
	MaxExpiry = 7 * 24 * time.Hour
	// accessKeyPrefix prefixes the access key of the share service
	// accounts.
	accessKeyPrefix = "share-"
	// idLen is the length of share IDs: 12 base32 characters (60 bits), so
	// that "share-<id>" fits the 20 characters of an access key.
	idLen      = 12
	idAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
	// maxDescriptionLen is the IAM limit on access key descriptions.
	maxDescriptionLen = 256
)

// record is the persisted form of a share.
type record struct {
	ID                 string     `json:"id"`
	Owner              string     `json:"owner"`
	Bucket             string     `json:"bucket"`
	Key                string     `json:"key"`
	Prefix             bool       `json:"prefix,omitempty"`
	Permission         Permission `json:"permission"`
	VersionID          string     `json:"versionId,omitempty"`
	ContentType        string     `json:"contentType,omitempty"`
	ContentDisposition string     `json:"contentDisposition,omitempty"`
	Expires            time.Time  `json:"expires"`
	CreatedAt          time.Time  `json:"createdAt"`
	URL                string     `json:"url,omitempty"`
	AccessKey          string     `json:"accessKey"`
}

func (r *record) toShare() *Share {
	return &Share{
		ID: r.ID, Owner: r.Owner, Bucket: r.Bucket, Key: r.Key, Prefix: r.Prefix,
		Permission: r.Permission, VersionID: r.VersionID, ContentType: r.ContentType,
		ContentDisposition: r.ContentDisposition, Expires: r.Expires, CreatedAt: r.CreatedAt,
		URL: r.URL, AccessKey: r.AccessKey,
	}
}

// service is the share link implementation.
type service struct {
	deps Deps
	opts options
}

var _ Service = (*service)(nil)

// New returns the share service. Deps.IAM and Deps.Signer are required:
// without them every call fails with domain.ErrInvalidArgument.
func New(deps Deps, opts ...Option) Service {
	if deps.Store == nil {
		deps.Store = iam.NewMemoryStore()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &service{deps: deps, opts: o}
}

func (s *service) now() time.Time { return s.deps.Clock.Now().UTC() }

func (s *service) checkDeps() error {
	if s.deps.IAM == nil || s.deps.Signer == nil {
		return fmt.Errorf("%w: share: the IAM and Signer dependencies are required", domain.ErrInvalidArgument)
	}
	return s.opts.serverURLErr
}

// systemActor is the identity the service uses towards IAM: the service
// enforces its own ownership rules and creates the share service accounts
// as children of the owner, which only root may do on someone's behalf.
func systemActor() domain.Identity { return domain.Identity{IsRoot: true} }

// isAdmin reports whether caller manages every share: root itself, not
// its derived credentials.
func isAdmin(caller domain.Identity) bool { return caller.IsRoot && caller.Parent == "" }

// visible reports whether caller may see a share of owner.
func visible(caller domain.Identity, owner string) bool {
	return isAdmin(caller) || (!caller.IsAnonymous && caller.EffectiveUser() != "" && caller.EffectiveUser() == owner)
}

func invalidArg(format string, args ...any) error {
	return fmt.Errorf("%w: share: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

func errNotFound(id string) error {
	return fmt.Errorf("%w: %q", domain.ErrShareNotFound, id)
}

// --- creation ---------------------------------------------------------------

// Create implements Service.
func (s *service) Create(ctx context.Context, in CreateInput) (*Share, error) {
	if err := s.checkDeps(); err != nil {
		return nil, err
	}
	owner := in.Owner
	if owner.IsAnonymous || owner.AccessKey == "" {
		return nil, fmt.Errorf("%w: share: anonymous callers cannot share", domain.ErrAccessDenied)
	}
	if err := validateTarget(in); err != nil {
		return nil, err
	}
	now := s.now().Truncate(time.Second)
	validity, err := s.validity(in, now)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, in, now); err != nil {
		return nil, err
	}
	if _, err := s.PurgeExpired(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "share: purge of expired shares failed", "error", err)
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	rec := &record{
		ID: id, Owner: owner.EffectiveUser(), Bucket: in.Bucket, Key: in.Key, Prefix: in.Prefix,
		Permission: in.Permission, VersionID: in.VersionID, ContentType: in.ContentType,
		ContentDisposition: in.ContentDisposition, Expires: now.Add(validity), CreatedAt: now,
		AccessKey: accessKeyPrefix + id,
	}
	key, err := s.deps.IAM.CreateAccessKey(ctx, iam.CreateAccessKeyInput{
		Caller:      systemActor(),
		ParentUser:  rec.Owner,
		AccessKey:   rec.AccessKey,
		Name:        rec.AccessKey,
		Description: describe(rec),
		Expiration:  rec.Expires,
		Policy:      inlinePolicy(rec),
	})
	if err != nil {
		return nil, fmt.Errorf("share: create service account: %w", err)
	}
	creds := domain.Credentials{AccessKey: key.AccessKey, SecretKey: key.SecretKey, Expiration: rec.Expires}
	if u, err := s.presign(ctx, rec, creds, rec.Key, now, true); err != nil {
		s.rollback(ctx, rec)
		return nil, err
	} else if u != nil {
		rec.URL = u.String()
	}
	if err := s.save(ctx, rec); err != nil {
		s.rollback(ctx, rec)
		return nil, err
	}
	s.deps.Logger.InfoContext(ctx, "share: link created", "id", rec.ID, "owner", rec.Owner, "bucket", rec.Bucket,
		"key", rec.Key, "prefix", rec.Prefix, "permission", rec.Permission, "expires", rec.Expires)
	return rec.toShare(), nil
}

// validateTarget checks bucket, key, permission and options.
func validateTarget(in CreateInput) error {
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return fmt.Errorf("share: %w", err)
	}
	if domain.IsReservedBucket(in.Bucket) {
		return fmt.Errorf("%w: share: a reserved bucket cannot be shared", domain.ErrAccessDenied)
	}
	if in.Prefix {
		if err := domain.ValidateObjectPrefix(in.Key); err != nil {
			return fmt.Errorf("share: %w", err)
		}
	} else if err := domain.ValidateObjectKey(in.Key); err != nil {
		return fmt.Errorf("share: %w", err)
	}
	if strings.ContainsAny(in.Key, "*?") || strings.Contains(in.Key, "${") {
		return invalidArg("keys containing '*', '?' or '${' cannot be shared with a managed link; use a presigned URL")
	}
	switch in.Permission {
	case PermissionDownload:
	case PermissionUpload:
		if in.VersionID != "" || in.ContentDisposition != "" {
			return invalidArg("versionId and contentDisposition apply to downloads only")
		}
	default:
		return invalidArg("permission must be %q or %q", PermissionDownload, PermissionUpload)
	}
	if in.Prefix && (in.VersionID != "" || in.ContentDisposition != "") {
		return invalidArg("versionId and contentDisposition apply to object shares only")
	}
	for _, v := range []string{in.ContentType, in.ContentDisposition, in.VersionID} {
		if len(v) > 1024 || !utf8.ValidString(v) || strings.ContainsAny(v, "\r\n\x00") {
			return invalidArg("invalid contentType, contentDisposition or versionId")
		}
	}
	return nil
}

// validity returns the validity of the link: Expires (default 7 days),
// capped at the expiration of the owner's STS session.
func (s *service) validity(in CreateInput, now time.Time) (time.Duration, error) {
	d := in.Expires
	if d == 0 {
		d = MaxExpiry
	}
	if d < time.Second || d > MaxExpiry {
		return 0, invalidArg("expiry must be between 1 second and %s", MaxExpiry)
	}
	d = d.Truncate(time.Second)
	if exp, ok := claimTime(in.Owner.Claims, "exp"); ok {
		left := exp.Sub(now)
		if left < time.Second {
			return 0, fmt.Errorf("%w: share: the session of the owner has expired", domain.ErrAccessDenied)
		}
		if left < d {
			d = left.Truncate(time.Second)
		}
	}
	return d, nil
}

// claimTime reads a numeric date claim.
func claimTime(claims map[string]any, name string) (time.Time, bool) {
	switch v := claims[name].(type) {
	case int64:
		return time.Unix(v, 0), true
	case int:
		return time.Unix(int64(v), 0), true
	case float64:
		return time.Unix(int64(v), 0), true
	case json.Number:
		n, err := v.Int64()
		return time.Unix(n, 0), err == nil
	}
	return time.Time{}, false
}

// sharedAction is the action a share grants.
func sharedAction(p Permission) policy.Action {
	if p == PermissionUpload {
		return policy.ActionPutObject
	}
	return policy.ActionGetObject
}

// authorize checks with the policy engine that the owner may perform the
// shared action, which derived credentials must prove: their inline policy
// does not carry over to the link.
func (s *service) authorize(ctx context.Context, in CreateInput, now time.Time) error {
	owner := in.Owner
	derived := owner.Parent != ""
	if s.deps.Policy == nil {
		if derived {
			return fmt.Errorf("%w: share: derived credentials cannot share without a policy engine", domain.ErrAccessDenied)
		}
		return nil
	}
	if isAdmin(owner) {
		return nil
	}
	object := in.Key
	if in.Prefix {
		object += "*"
	}
	d, err := s.deps.Policy.Evaluate(ctx, policy.Args{Identity: &owner, Action: sharedAction(in.Permission), Bucket: in.Bucket, Object: object, Now: now})
	if err != nil {
		return fmt.Errorf("share: authorize: %w", err)
	}
	if !d.Allowed {
		return fmt.Errorf("%w: share: %s on %s/%s is not allowed: %s", domain.ErrAccessDenied, sharedAction(in.Permission), in.Bucket, object, d.Reason)
	}
	return nil
}

// inlinePolicy is the policy of the share service account: the shared
// action on the exact object, or on the objects under the prefix (plus
// listing the prefix for downloads).
func inlinePolicy(r *record) *domain.Policy {
	resource := "arn:aws:s3:::" + r.Bucket + "/" + r.Key
	if r.Prefix {
		resource += "*"
	}
	actions := domain.StringSet{string(sharedAction(r.Permission))}
	if r.Permission == PermissionDownload && r.VersionID != "" {
		actions = append(actions, string(policy.ActionGetObjectVersion))
	}
	p := &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{
		SID: "Share", Effect: domain.Allow, Action: actions, Resource: domain.StringSet{resource},
	}}}
	if r.Prefix && r.Permission == PermissionDownload {
		p.Statement = append(p.Statement, domain.Statement{
			SID: "ShareList", Effect: domain.Allow,
			Action:    domain.StringSet{string(policy.ActionListBucket)},
			Resource:  domain.StringSet{"arn:aws:s3:::" + r.Bucket},
			Condition: domain.Condition{"StringLike": {"s3:prefix": domain.StringSet{r.Key + "*"}}},
		})
	}
	return p
}

// describe is the description of the share service account.
func describe(r *record) string {
	d := fmt.Sprintf("share link %s (%s %s/%s", r.ID, r.Permission, r.Bucket, r.Key)
	if r.Prefix {
		d += "*"
	}
	d += ")"
	if utf8.RuneCountInString(d) > maxDescriptionLen {
		d = string([]rune(d)[:maxDescriptionLen-3]) + "..."
	}
	return d
}

func newID() (string, error) {
	out := make([]byte, idLen)
	size := big.NewInt(int64(len(idAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", fmt.Errorf("share: random id: %w", err)
		}
		out[i] = idAlphabet[v.Int64()]
	}
	return string(out), nil
}

// presign signs the URL of key for the share. For prefix shares, an empty
// key with initial set yields the ListObjectsV2 URL of a download share and
// nothing for an upload share.
func (s *service) presign(ctx context.Context, r *record, creds domain.Credentials, key string, now time.Time, initial bool) (*url.URL, error) {
	left := r.Expires.Sub(now).Truncate(time.Second)
	if left < time.Second {
		return nil, fmt.Errorf("%w: share %q has expired", domain.ErrRequestExpired, r.ID)
	}
	u := *s.opts.serverURL
	q := url.Values{}
	in := auth.PresignInput{Method: http.MethodGet, Expires: left, Creds: creds, Region: s.opts.region, Now: now}
	switch {
	case r.Prefix && initial:
		if r.Permission == PermissionUpload {
			return nil, nil
		}
		u.Path += "/" + r.Bucket
		q.Set("list-type", "2")
		q.Set("prefix", r.Key)
	default:
		u.Path += "/" + r.Bucket + "/" + key
		if r.Permission == PermissionUpload {
			in.Method = http.MethodPut
			if r.ContentType != "" {
				in.Headers = http.Header{"Content-Type": {r.ContentType}}
			}
			break
		}
		if r.VersionID != "" {
			q.Set("versionId", r.VersionID)
		}
		if r.ContentDisposition != "" {
			q.Set("response-content-disposition", r.ContentDisposition)
		}
		if r.ContentType != "" {
			q.Set("response-content-type", r.ContentType)
		}
	}
	u.RawPath = ""
	u.RawQuery = q.Encode()
	in.URL = &u
	signed, err := s.deps.Signer.Presign(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("share: presign: %w", err)
	}
	return signed, nil
}

// rollback removes what a failed Create left behind. It runs even when the
// client went away: an orphan service account has no record, so the Share
// API could never revoke it.
func (s *service) rollback(ctx context.Context, r *record) {
	ctx = context.WithoutCancel(ctx)
	if err := s.deleteAccount(ctx, r); err != nil {
		s.deps.Logger.ErrorContext(ctx, "share: rollback: delete service account", "accessKey", r.AccessKey, "error", err)
	}
}

// --- records ----------------------------------------------------------------

func (s *service) save(ctx context.Context, r *record) error {
	raw, err := json.Marshal(r) // #nosec G117 -- AccessKey is the public id of the share's service account, its secret is not in the record
	if err != nil {
		return fmt.Errorf("share: encode %s: %w", r.ID, err)
	}
	if err := s.deps.Store.Put(ctx, iam.NamespaceShares, r.ID, raw); err != nil {
		return fmt.Errorf("share: save %s: %w", r.ID, err)
	}
	return nil
}

// load returns the record of id; a missing record is ErrShareNotFound.
func (s *service) load(ctx context.Context, id string) (*record, error) {
	if !validID(id) {
		return nil, errNotFound(id)
	}
	raw, err := s.deps.Store.Get(ctx, iam.NamespaceShares, id)
	if errors.Is(err, iam.ErrEntryNotFound) {
		return nil, errNotFound(id)
	}
	if err != nil {
		return nil, fmt.Errorf("share: load %s: %w", id, err)
	}
	return decodeRecord(id, raw)
}

// all returns every record, newest first.
func (s *service) all(ctx context.Context) ([]*record, error) {
	entries, err := s.deps.Store.List(ctx, iam.NamespaceShares)
	if err != nil {
		return nil, fmt.Errorf("share: list: %w", err)
	}
	out := make([]*record, 0, len(entries))
	for id, raw := range entries {
		r, err := decodeRecord(id, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func decodeRecord(id string, raw []byte) (*record, error) {
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%w: share: record %s: %w", domain.ErrInvalidFormat, id, err)
	}
	if r.ID != id || r.AccessKey == "" {
		return nil, fmt.Errorf("%w: share: record %s is inconsistent", domain.ErrInvalidFormat, id)
	}
	return &r, nil
}

func validID(id string) bool {
	if len(id) != idLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !strings.ContainsRune(idAlphabet, rune(id[i])) {
			return false
		}
	}
	return true
}

// deleteAccount deletes the service account of a share; a missing one is
// not an error.
func (s *service) deleteAccount(ctx context.Context, r *record) error {
	err := s.deps.IAM.DeleteAccessKey(ctx, systemActor(), r.AccessKey)
	if err != nil && !errors.Is(err, domain.ErrAccessKeyNotFound) && !errors.Is(err, domain.ErrUserNotFound) {
		return fmt.Errorf("share: delete service account %s: %w", r.AccessKey, err)
	}
	return nil
}

// remove deletes the service account, then the record of a share.
func (s *service) remove(ctx context.Context, r *record) error {
	if err := s.deleteAccount(ctx, r); err != nil {
		return err
	}
	if err := s.deps.Store.Delete(ctx, iam.NamespaceShares, r.ID); err != nil {
		return fmt.Errorf("share: delete %s: %w", r.ID, err)
	}
	return nil
}

// --- queries and revocation ---------------------------------------------------

// visibleRecord loads a share the caller may see.
func (s *service) visibleRecord(ctx context.Context, caller domain.Identity, id string) (*record, error) {
	r, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !visible(caller, r.Owner) {
		return nil, errNotFound(id)
	}
	return r, nil
}

// Get implements Service.
func (s *service) Get(ctx context.Context, caller domain.Identity, id string) (*Share, error) {
	r, err := s.visibleRecord(ctx, caller, id)
	if err != nil {
		return nil, err
	}
	return r.toShare(), nil
}

// ownerScope resolves the owner a listing or a mass revocation applies to:
// any (empty) or the named owner for root, the caller's effective user
// otherwise.
func ownerScope(caller domain.Identity, owner string) (string, error) {
	if isAdmin(caller) {
		return owner, nil
	}
	self := caller.EffectiveUser()
	if caller.IsAnonymous || self == "" {
		return "", fmt.Errorf("%w: share: anonymous callers have no shares", domain.ErrAccessDenied)
	}
	if owner != "" && owner != self {
		return "", fmt.Errorf("%w: share: only root may manage the shares of other users", domain.ErrAccessDenied)
	}
	return self, nil
}

// List implements Service.
func (s *service) List(ctx context.Context, caller domain.Identity, filter ListFilter) ([]Share, error) {
	owner, err := ownerScope(caller, filter.Owner)
	if err != nil {
		return nil, err
	}
	records, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := []Share{}
	for _, r := range records {
		if (owner != "" && r.Owner != owner) || (filter.Bucket != "" && r.Bucket != filter.Bucket) || !now.Before(r.Expires) {
			continue
		}
		out = append(out, *r.toShare())
	}
	return out, nil
}

// Presign implements Service.
func (s *service) Presign(ctx context.Context, caller domain.Identity, id, key string) (*url.URL, error) {
	if err := s.checkDeps(); err != nil {
		return nil, err
	}
	r, err := s.visibleRecord(ctx, caller, id)
	if err != nil {
		return nil, err
	}
	switch {
	case !r.Prefix && key == "":
		key = r.Key
	case !r.Prefix && key != r.Key:
		return nil, invalidArg("share %s covers %q only", r.ID, r.Key)
	case r.Prefix:
		if err := domain.ValidateObjectKey(key); err != nil {
			return nil, fmt.Errorf("share: %w", err)
		}
		if !strings.HasPrefix(key, r.Key) {
			return nil, invalidArg("key %q is not under the shared prefix %q", key, r.Key)
		}
	}
	now := s.now().Truncate(time.Second)
	if !now.Before(r.Expires) {
		return nil, errNotFound(id)
	}
	creds, _, err := s.deps.IAM.LookupCredentials(ctx, r.AccessKey)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) {
			return nil, errNotFound(id)
		}
		return nil, fmt.Errorf("share: credentials of %s: %w", r.ID, err)
	}
	return s.presign(ctx, r, creds, key, now, false)
}

// Revoke implements Service.
func (s *service) Revoke(ctx context.Context, caller domain.Identity, id string) error {
	if err := s.checkDeps(); err != nil {
		return err
	}
	r, err := s.visibleRecord(ctx, caller, id)
	if err != nil {
		return err
	}
	if err := s.remove(ctx, r); err != nil {
		return err
	}
	s.deps.Logger.InfoContext(ctx, "share: link revoked", "id", r.ID, "owner", r.Owner)
	return nil
}

// RevokeAll implements Service.
func (s *service) RevokeAll(ctx context.Context, caller domain.Identity, owner string) (int, error) {
	if err := s.checkDeps(); err != nil {
		return 0, err
	}
	if owner == "" {
		owner = caller.EffectiveUser()
	}
	scope, err := ownerScope(caller, owner)
	if err != nil {
		return 0, err
	}
	if scope == "" {
		return 0, invalidArg("owner is required")
	}
	return s.removeWhere(ctx, func(r *record) bool { return r.Owner == scope })
}

// PurgeExpired implements Service.
func (s *service) PurgeExpired(ctx context.Context) (int, error) {
	if err := s.checkDeps(); err != nil {
		return 0, err
	}
	now := s.now()
	return s.removeWhere(ctx, func(r *record) bool { return !now.Before(r.Expires) })
}

// removeWhere removes the shares selected by match and returns how many
// were removed; it goes on after a failure and reports every error.
func (s *service) removeWhere(ctx context.Context, match func(*record) bool) (int, error) {
	records, err := s.all(ctx)
	if err != nil {
		return 0, err
	}
	var (
		n    int
		errs []error
	)
	for _, r := range records {
		if !match(r) {
			continue
		}
		if err := s.remove(ctx, r); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if n > 0 {
		s.deps.Logger.InfoContext(ctx, "share: links removed", "count", n)
	}
	return n, errors.Join(errs...)
}
