package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// BucketManagerDeps are the dependencies of the bucket manager.
type BucketManagerDeps struct {
	// Buckets manages the bucket metadata (required).
	Buckets bucket.Service
	// Objects empties buckets on forced deletion and reports usage
	// (required).
	Objects object.Service
	// Policy parses bucket policies and computes the anonymous presets
	// (required).
	Policy policy.Service
	// Authz authorizes the requested settings and filters the listing
	// (required).
	Authz Authorizer
	// AccessRules reports the buckets public through their access rules
	// (optional: without it only the bucket policy makes a bucket public).
	AccessRules accessrule.Service
	Logger      *slog.Logger
}

// BucketManagerOption configures the bucket manager.
type BucketManagerOption func(*bucketManager)

type bucketManager struct {
	deps BucketManagerDeps
}

var _ BucketManager = (*bucketManager)(nil)

// NewBucketManager returns the console view of the buckets. Missing
// dependencies are a wiring error (domain.ErrInvalidArgument).
func NewBucketManager(deps BucketManagerDeps, opts ...BucketManagerOption) (BucketManager, error) {
	if deps.Buckets == nil || deps.Objects == nil || deps.Policy == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: bucket manager requires the bucket, object, policy and authorizer services", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &bucketManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// List implements BucketManager. The buckets are visited by name after
// the cursor and authorized one by one until the page is full.
func (m *bucketManager) List(ctx context.Context, in ListBucketsInput) (*Paged[BucketSummary], error) {
	limit, err := pageLimit(in.Page)
	if err != nil {
		return nil, err
	}
	after, err := decodeCursor(in.Page.Cursor)
	if err != nil {
		return nil, err
	}
	all, err := m.deps.Authz.Authorize(ctx, AuthorizeInput{Requester: in.Requester, Actions: []policy.Action{policy.ActionListAllMyBuckets}})
	if err != nil {
		return nil, err
	}
	infos, err := m.deps.Buckets.List(ctx, bucket.ListInput{Prefix: in.Prefix, WithUsage: true})
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	out := &Paged[BucketSummary]{Items: make([]BucketSummary, 0, min(len(infos), limit))}
	for _, info := range infos {
		if info.Name <= after {
			continue
		}
		whole, err := m.listable(ctx, in.Requester, info.Name, false)
		if err != nil {
			return nil, err
		}
		if !whole && !all.Allowed {
			// A bucket is visible when some part of it may be listed: a
			// home directory restricted by s3:prefix counts (Partial).
			visible, err := m.listable(ctx, in.Requester, info.Name, true)
			if err != nil {
				return nil, err
			}
			if !visible {
				continue
			}
		}
		if len(out.Items) == limit {
			out.NextCursor = encodeCursor(out.Items[limit-1].Info.Name)
			break
		}
		if !whole {
			out.Items = append(out.Items, partialSummary(info))
			continue
		}
		out.Items = append(out.Items, BucketSummary{Info: info, Public: m.public(ctx, info.Name)})
	}
	return out, nil
}

// listable reports whether the requester may list the bucket: as a whole,
// or with partial at least a part of it (AuthorizeInput.Partial).
func (m *bucketManager) listable(ctx context.Context, req Requester, name string, partial bool) (bool, error) {
	d, err := m.deps.Authz.Authorize(ctx, AuthorizeInput{Requester: req, Actions: []policy.Action{policy.ActionListBucket}, Bucket: name, Partial: partial})
	if err != nil {
		return false, err
	}
	return d.Allowed, nil
}

// partialSummary is the summary of a bucket the requester may list only in
// part: its name, creation date and region, as s3:ListAllMyBuckets shows.
func partialSummary(info domain.BucketInfo) BucketSummary {
	return BucketSummary{Info: domain.BucketInfo{Bucket: info.Bucket}, Partial: true}
}

// Get implements BucketManager.
func (m *bucketManager) Get(ctx context.Context, req Requester, name string) (*BucketSummary, error) {
	whole, err := m.listable(ctx, req, name, false)
	if err != nil {
		return nil, err
	}
	if !whole {
		info, err := m.deps.Buckets.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		s := partialSummary(*info)
		return &s, nil
	}
	return m.summary(ctx, name)
}

// summary returns the full summary of a bucket: usage, settings, tags and
// public flag.
func (m *bucketManager) summary(ctx context.Context, name string) (*BucketSummary, error) {
	info, err := m.deps.Buckets.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	usage, err := m.deps.Objects.DataUsage(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("admin: usage of %s: %w", name, err)
	}
	if len(usage) == 1 {
		info.Objects, info.Versions, info.DeleteMarkers, info.Size = usage[0].Objects, usage[0].Versions, usage[0].DeleteMarkers, usage[0].Size
	}
	return &BucketSummary{Info: *info, Public: m.public(ctx, name)}, nil
}

// public reports whether the bucket policy grants access to everyone, or
// an access rule in effect allows everyone; a failure is logged and
// reported as not public.
func (m *bucketManager) public(ctx context.Context, name string) bool {
	public, err := m.deps.Buckets.IsPublic(ctx, name)
	if err != nil && !errors.Is(err, domain.ErrPolicyNotFound) {
		m.deps.Logger.WarnContext(ctx, "admin: bucket policy status", "bucket", name, "err", err)
	}
	if public || m.deps.AccessRules == nil {
		return public
	}
	st, err := m.deps.AccessRules.PublicStatus(ctx, name)
	if err != nil {
		m.deps.Logger.WarnContext(ctx, "admin: access rules public status", "bucket", name, "err", err)
		return false
	}
	return st.Public
}

// Create implements BucketManager.
func (m *bucketManager) Create(ctx context.Context, in CreateBucketInput) (*BucketSummary, error) {
	if err := domain.ValidateBucketName(in.Name); err != nil {
		return nil, err
	}
	actions := []policy.Action{policy.ActionCreateBucket}
	if in.Versioning && !in.ObjectLock {
		actions = append(actions, policy.ActionPutBucketVersioning)
	}
	if in.ObjectLock {
		actions = append(actions, policy.ActionPutBucketObjectLock)
	}
	if in.QuotaBytes > 0 {
		actions = append(actions, policy.ActionAdminSetBucketQuota)
	}
	if in.HLS {
		actions = append(actions, policy.ActionAdminSetBucketMedia)
	}
	if err := authorizeAll(ctx, m.deps.Authz, in.Requester, in.Name, "", actions...); err != nil {
		return nil, err
	}
	if _, err := m.deps.Buckets.Create(ctx, bucket.CreateInput{Name: in.Name, Region: in.Region, ObjectLock: in.ObjectLock}); err != nil {
		return nil, err
	}
	if err := m.applySettings(ctx, in); err != nil {
		// Without its settings the bucket is not what was asked for: undo
		// the creation, even when the client went away.
		if derr := m.deps.Buckets.Delete(context.WithoutCancel(ctx), in.Name, bucket.DeleteOptions{}); derr != nil {
			m.deps.Logger.WarnContext(ctx, "admin: cannot roll back bucket creation", "bucket", in.Name, "err", derr)
		}
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: bucket created", "bucket", in.Name, "by", in.Requester.Identity.AccessKey)
	// The creator sees the settings it has just applied to the empty
	// bucket.
	return m.summary(ctx, in.Name)
}

// applySettings applies the settings requested at creation.
func (m *bucketManager) applySettings(ctx context.Context, in CreateBucketInput) error {
	if in.Versioning && !in.ObjectLock {
		if err := m.deps.Buckets.SetVersioning(ctx, in.Name, domain.VersioningEnabled); err != nil {
			return fmt.Errorf("admin: enable versioning of %s: %w", in.Name, err)
		}
	}
	if in.QuotaBytes > 0 {
		if _, err := m.deps.Buckets.SetQuota(ctx, in.Name, bucket.Quota{Type: bucket.QuotaHard, Size: in.QuotaBytes}); err != nil {
			return fmt.Errorf("admin: quota of %s: %w", in.Name, err)
		}
	}
	if in.HLS {
		if err := m.deps.Buckets.SetMedia(ctx, in.Name, domain.MediaConfig{Rules: []domain.MediaRule{domain.HLSMediaRule()}}); err != nil {
			return fmt.Errorf("admin: media rule of %s: %w", in.Name, err)
		}
	}
	return nil
}

// Delete implements BucketManager.
func (m *bucketManager) Delete(ctx context.Context, in DeleteBucketInput) error {
	if in.Force {
		if err := m.empty(ctx, in.Name); err != nil {
			return err
		}
	}
	if err := m.deps.Buckets.Delete(ctx, in.Name, bucket.DeleteOptions{}); err != nil {
		return err
	}
	m.deps.Logger.InfoContext(ctx, "admin: bucket deleted", "bucket", in.Name, "force", in.Force)
	return nil
}

// deleteBatch is the number of objects removed per DeleteObjects call.
const deleteBatch = 1000

// empty deletes every version, delete marker and pending multipart upload
// of a bucket through the object layer, so that object lock is honored.
// Versions that cannot be deleted are skipped and reported at the end:
// the bucket is then not empty and is kept.
func (m *bucketManager) empty(ctx context.Context, name string) error {
	var (
		failed   int
		firstErr error
		marker   string
		vmarker  string
	)
	for {
		res, err := m.deps.Objects.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: name, Marker: marker, VersionIDMarker: vmarker, MaxKeys: deleteBatch})
		if err != nil {
			return fmt.Errorf("admin: empty bucket %s: %w", name, err)
		}
		refs := make([]object.ObjectToDelete, 0, len(res.Objects))
		for _, o := range res.Objects {
			// Every version goes for good: one listed without id is the
			// null version (a delete without version id would add a
			// delete marker to a versioned bucket).
			vid := o.VersionID
			if vid == "" {
				vid = domain.NullVersionID
			}
			refs = append(refs, object.ObjectToDelete{Key: o.Key, VersionID: vid})
		}
		if len(refs) > 0 {
			out, err := m.deps.Objects.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: name, Objects: refs, Quiet: true})
			if err != nil {
				return fmt.Errorf("admin: empty bucket %s: %w", name, err)
			}
			for _, e := range out.Errors {
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("%s (version %q): %w", e.Key, e.VersionID, e.Err)
				}
			}
		}
		if !res.IsTruncated {
			break
		}
		marker, vmarker = res.NextMarker, res.NextVersionIDMarker
	}
	if err := m.abortUploads(ctx, name); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%w: bucket %s: %d versions could not be deleted, first: %w", domain.ErrBucketNotEmpty, name, failed, firstErr)
	}
	return nil
}

// abortUploads aborts every pending multipart upload of a bucket.
func (m *bucketManager) abortUploads(ctx context.Context, name string) error {
	var keyMarker, uploadMarker string
	for {
		res, err := m.deps.Objects.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: name, KeyMarker: keyMarker, UploadIDMarker: uploadMarker})
		if err != nil {
			return fmt.Errorf("admin: uploads of %s: %w", name, err)
		}
		for _, u := range res.Uploads {
			if err := m.deps.Objects.AbortMultipartUpload(ctx, name, u.Key, u.UploadID); err != nil && !errors.Is(err, domain.ErrNoSuchUpload) {
				return fmt.Errorf("admin: abort upload %s of %s/%s: %w", u.UploadID, name, u.Key, err)
			}
		}
		if !res.IsTruncated {
			return nil
		}
		keyMarker, uploadMarker = res.NextKeyMarker, res.NextUploadIDMarker
	}
}

// Access implements BucketManager.
func (m *bucketManager) Access(ctx context.Context, name string) (*BucketAccess, error) {
	current, err := m.bucketPolicy(ctx, name)
	if err != nil {
		return nil, err
	}
	out := &BucketAccess{Bucket: name, Rules: []policy.PublicAccessRule{}}
	if current == nil {
		return out, nil
	}
	out.HasPolicy = true
	out.Public = m.deps.Policy.IsPublic(ctx, current)
	if rules := m.deps.Policy.ListPublicAccess(ctx, current, name); rules != nil {
		out.Rules = rules
	}
	return out, nil
}

// bucketPolicy returns the parsed bucket policy, nil when none is set.
func (m *bucketManager) bucketPolicy(ctx context.Context, name string) (*domain.Policy, error) {
	raw, err := m.deps.Buckets.GetPolicy(ctx, name)
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return m.deps.Policy.ParseBucketPolicy(ctx, name, raw)
}

// SetAccess implements BucketManager.
func (m *bucketManager) SetAccess(ctx context.Context, in SetAccessInput) (*BucketAccess, error) {
	switch in.Preset {
	case policy.PresetPrivate, policy.PresetDownload, policy.PresetUpload, policy.PresetPublic:
	default:
		return nil, fmt.Errorf("%w: preset must be private, download, upload or public, not %q", domain.ErrInvalidArgument, in.Preset)
	}
	if err := domain.ValidateObjectPrefix(in.Prefix); err != nil {
		return nil, err
	}
	current, err := m.bucketPolicy(ctx, in.Bucket)
	if err != nil {
		return nil, err
	}
	next, err := m.deps.Policy.ApplyPublicPreset(ctx, current, in.Bucket, in.Prefix, in.Preset)
	if err != nil {
		return nil, err
	}
	switch {
	case next == nil && current != nil:
		if err := m.deps.Buckets.DeletePolicy(ctx, in.Bucket); err != nil && !errors.Is(err, domain.ErrPolicyNotFound) {
			return nil, err
		}
	case next != nil:
		raw, err := json.Marshal(next)
		if err != nil {
			return nil, fmt.Errorf("admin: encode bucket policy: %w", err)
		}
		if err := m.deps.Buckets.SetPolicy(ctx, in.Bucket, raw); err != nil {
			return nil, err
		}
	}
	m.deps.Logger.InfoContext(ctx, "admin: anonymous access changed", "bucket", in.Bucket, "prefix", in.Prefix, "preset", in.Preset)
	return m.Access(ctx, in.Bucket)
}

// Tags implements BucketManager.
func (m *bucketManager) Tags(ctx context.Context, name string) (map[string]string, error) {
	tags, err := m.deps.Buckets.GetTags(ctx, name)
	switch {
	case errors.Is(err, domain.ErrPolicyNotFound):
		return map[string]string{}, nil
	case err != nil:
		return nil, err
	case tags == nil:
		return map[string]string{}, nil
	}
	return tags, nil
}

// ObjectLock implements BucketManager.
func (m *bucketManager) ObjectLock(ctx context.Context, name string) (bucket.ObjectLockConfig, error) {
	cfg, err := m.deps.Buckets.GetObjectLock(ctx, name)
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return bucket.ObjectLockConfig{}, nil
	}
	return cfg, err
}
