package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Defaults and limits of the object browser.
const (
	// maxListLimit is the largest page of a listing.
	maxListLimit = 1000
	// maxCopySize is the largest object CopyObject copies in one request;
	// larger objects are copied part by part.
	maxCopySize = 5 << 30
	// defaultCopyPartSize is the part size of the multipart copies.
	defaultCopyPartSize = 512 << 20
	// defaultZipMaxObjects bounds the number of entries of a zip archive.
	defaultZipMaxObjects = 10000
	// maxExplicitObjects bounds the objects named in one request.
	maxExplicitObjects = 10000
	// statVersions is the number of versions returned by Stat.
	statVersions = 10
	// folderContentType is the content type of the folder placeholders.
	folderContentType = "application/x-directory"
)

// ObjectBrowserDeps are the dependencies of the object browser.
type ObjectBrowserDeps struct {
	// Objects is the object layer (required).
	Objects object.Service
	// Authz authorizes every object of the bulk operations (required).
	Authz  Authorizer
	Clock  clock.Clock
	Logger *slog.Logger
}

// ObjectBrowserOption configures the object browser.
type ObjectBrowserOption func(*objectBrowser)

// WithZipMaxObjects bounds the entries of a zip archive (default 10000);
// the objects beyond the limit are reported in the errors entry.
func WithZipMaxObjects(n int) ObjectBrowserOption {
	return func(b *objectBrowser) {
		if n > 0 {
			b.zipMaxObjects = n
		}
	}
}

// WithCopyPartSize sets the part size of the multipart copies of the
// objects larger than 5 GiB (default 512 MiB, at least 5 MiB).
func WithCopyPartSize(size int64) ObjectBrowserOption {
	return func(b *objectBrowser) {
		if size >= 5<<20 {
			b.copyPartSize = size
		}
	}
}

type objectBrowser struct {
	deps          ObjectBrowserDeps
	zipMaxObjects int
	copyPartSize  int64
}

var _ ObjectBrowser = (*objectBrowser)(nil)

// NewObjectBrowser returns the object browser. Missing dependencies are a
// wiring error (domain.ErrInvalidArgument).
func NewObjectBrowser(deps ObjectBrowserDeps, opts ...ObjectBrowserOption) (ObjectBrowser, error) {
	if deps.Objects == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: object browser requires the object layer and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	b := &objectBrowser{deps: deps, zipMaxObjects: defaultZipMaxObjects, copyPartSize: defaultCopyPartSize}
	for _, o := range opts {
		o(b)
	}
	return b, nil
}

// listLimit validates a page size: 0 means the maximum.
func listLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return maxListLimit, nil
	case limit < 0 || limit > maxListLimit:
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", domain.ErrInvalidArgument, maxListLimit)
	default:
		return limit, nil
	}
}

// List implements ObjectBrowser.
func (b *objectBrowser) List(ctx context.Context, in ListObjectsInput) (*ObjectListing, error) {
	limit, err := listLimit(in.Limit)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateObjectPrefix(in.Prefix); err != nil {
		return nil, err
	}
	delimiter := DefaultDelimiter
	if in.Delimiter != nil {
		delimiter = *in.Delimiter
	}
	// The conditions of the S3 listing: s3:delimiter is the effective one.
	conds := ListingConditions(in.Prefix, delimiter, in.Limit)
	d, err := b.deps.Authz.Authorize(ctx, AuthorizeInput{Requester: in.Requester, Actions: []policy.Action{policy.ActionListBucket}, Bucket: in.Bucket, Conditions: conds})
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		return nil, denied(d)
	}
	res, err := b.deps.Objects.ListObjects(ctx, domain.ListObjectsInput{
		Bucket: in.Bucket, Prefix: in.Prefix, Delimiter: delimiter, ContinuationToken: in.Cursor, MaxKeys: limit,
	})
	if err != nil {
		return nil, err
	}
	out := &ObjectListing{Objects: make([]domain.ObjectInfo, 0, len(res.Objects)), Prefixes: res.CommonPrefixes}
	if out.Prefixes == nil {
		out.Prefixes = []string{}
	}
	for _, o := range res.Objects {
		// The placeholder of the folder being browsed is the folder itself.
		if in.Prefix != "" && o.Key == in.Prefix && strings.HasSuffix(in.Prefix, "/") {
			continue
		}
		out.Objects = append(out.Objects, o)
	}
	if res.IsTruncated {
		out.NextCursor = res.NextContinuationToken
	}
	return out, nil
}

// Stat implements ObjectBrowser.
func (b *objectBrowser) Stat(ctx context.Context, in StatInput) (*ObjectDetails, error) {
	info, err := b.deps.Objects.GetObjectInfo(ctx, in.Bucket, in.Key, object.GetObjectOptions{VersionID: in.VersionID})
	if err != nil {
		return nil, err
	}
	out := &ObjectDetails{Info: info}
	out.Info.Tags = nil
	tagAction := policy.ActionGetObjectTagging
	if in.VersionID != "" {
		tagAction = policy.ActionGetObjectVersionTagging
	}
	if b.allowed(ctx, in.Requester, in.Bucket, in.Key, ObjectConditions(ObjectRequest{VersionID: in.VersionID}), tagAction) {
		out.Tags = info.Tags
		if out.Tags == nil {
			out.Tags = map[string]string{}
		}
	}
	if b.allowed(ctx, in.Requester, in.Bucket, "", ListingConditions(in.Key, "", 0), policy.ActionListBucketVersions) {
		versions, err := b.Versions(ctx, VersionsInput{Bucket: in.Bucket, Key: in.Key, Limit: statVersions})
		if err != nil {
			return nil, err
		}
		out.Versions, out.VersionsTruncated = versions.Versions, versions.NextCursor != ""
	}
	return out, nil
}

// allowed reports whether the requester may perform action; errors are
// logged and count as a denial (the information is optional).
func (b *objectBrowser) allowed(ctx context.Context, req Requester, bucket, key string, conds map[string][]string, action policy.Action) bool {
	d, err := b.deps.Authz.Authorize(ctx, AuthorizeInput{Requester: req, Actions: []policy.Action{action}, Bucket: bucket, Object: key, Conditions: conds})
	if err != nil {
		b.deps.Logger.WarnContext(ctx, "admin: authorize", "action", action, "bucket", bucket, "key", key, "err", err)
		return false
	}
	return d.Allowed
}

// versionCursor is the decoded cursor of a version listing.
type versionCursor struct {
	Key       string `json:"k"`
	VersionID string `json:"v"`
}

func encodeVersionCursor(c versionCursor) string { return encodeJSONCursor(c) }

func decodeVersionCursor(s string) (versionCursor, error) {
	var c versionCursor
	if err := decodeJSONCursor(s, &c); err != nil {
		return versionCursor{}, err
	}
	return c, nil
}

// Versions implements ObjectBrowser.
func (b *objectBrowser) Versions(ctx context.Context, in VersionsInput) (*VersionList, error) {
	limit, err := listLimit(in.Limit)
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return nil, err
	}
	var cur versionCursor
	if in.Cursor != "" {
		if cur, err = decodeVersionCursor(in.Cursor); err != nil {
			return nil, err
		}
		if cur.Key != in.Key {
			return nil, fmt.Errorf("%w: the cursor belongs to another key", domain.ErrInvalidArgument)
		}
	}
	res, err := b.deps.Objects.ListObjectVersions(ctx, domain.ListObjectsInput{
		Bucket: in.Bucket, Prefix: in.Key, Marker: cur.Key, VersionIDMarker: cur.VersionID, MaxKeys: limit,
	})
	if err != nil {
		return nil, err
	}
	out := &VersionList{Versions: []domain.ObjectInfo{}}
	others := false
	for _, o := range res.Objects {
		if o.Key != in.Key {
			// Keys sharing the prefix sort after the key itself.
			others = true
			continue
		}
		out.Versions = append(out.Versions, o)
	}
	if res.IsTruncated && !others && res.NextMarker == in.Key && len(out.Versions) > 0 {
		out.NextCursor = encodeVersionCursor(versionCursor{Key: in.Key, VersionID: res.NextVersionIDMarker})
	}
	return out, nil
}

// CreateFolder implements ObjectBrowser.
func (b *objectBrowser) CreateFolder(ctx context.Context, in FolderInput) (domain.ObjectInfo, error) {
	prefix := in.Prefix
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if strings.Trim(prefix, "/") == "" {
		return domain.ObjectInfo{}, fmt.Errorf("%w: the folder name is required", domain.ErrInvalidObjectName)
	}
	if err := domain.ValidateObjectKey(prefix); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := authorizeObject(ctx, b.deps.Authz, in.Requester, in.Bucket, prefix, ObjectRequest{}, policy.ActionPutObject); err != nil {
		return domain.ObjectInfo{}, err
	}
	return b.deps.Objects.PutObject(ctx, object.PutObjectInput{
		Bucket: in.Bucket, Key: prefix, Reader: bytes.NewReader(nil), Size: 0, ContentType: folderContentType,
	})
}

// folderPrefix returns a prefix of the object browser as the folder it
// names: a non-empty prefix without a trailing slash is the folder of
// that name ("photos" is "photos/", never "photos2/" nor "photosx.txt"),
// as for the CLI (nexoctl rm -r alias/bucket/photos). "" is the whole
// bucket.
func folderPrefix(prefix string) string {
	if prefix == "" || strings.HasSuffix(prefix, "/") {
		return prefix
	}
	return prefix + "/"
}

// folderPrefixes applies folderPrefix to every prefix (a new slice).
func folderPrefixes(prefixes []string) []string {
	if prefixes == nil {
		return nil
	}
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = folderPrefix(p)
	}
	return out
}

// Delete implements ObjectBrowser.
func (b *objectBrowser) Delete(ctx context.Context, in DeleteObjectsInput) (*DeleteObjectsResult, error) {
	if len(in.Objects) == 0 && len(in.Prefixes) == 0 {
		return nil, fmt.Errorf("%w: no object or prefix to delete", domain.ErrInvalidArgument)
	}
	in.Prefixes = folderPrefixes(in.Prefixes)
	if len(in.Objects) > maxExplicitObjects {
		return nil, fmt.Errorf("%w: at most %d objects per request", domain.ErrInvalidArgument, maxExplicitObjects)
	}
	for _, p := range in.Prefixes {
		if err := domain.ValidateObjectPrefix(p); err != nil {
			return nil, err
		}
	}
	if err := b.authorizeWalks(ctx, in.Requester, in.Bucket, in.Prefixes, in.AllVersions); err != nil {
		return nil, err
	}
	d := &deleter{b: b, ctx: ctx, req: in.Requester, bucket: in.Bucket, out: &DeleteObjectsResult{Deleted: []DeletedObject{}, Errors: []ObjectError{}}}
	for _, ref := range in.Objects {
		if err := domain.ValidateObjectKey(ref.Key); err != nil {
			d.fail(ref, err)
			continue
		}
		if err := d.add(ref); err != nil {
			return nil, err
		}
	}
	for _, p := range in.Prefixes {
		var err error
		if in.AllVersions {
			err = b.walkVersions(ctx, in.Bucket, p, func(o domain.ObjectInfo) error {
				// Unversioned objects list without a version id: deleting
				// them is permanent already.
				return d.add(ObjectRef{Key: o.Key, VersionID: o.VersionID})
			})
		} else {
			err = b.walkObjects(ctx, in.Bucket, p, func(o domain.ObjectInfo) error {
				return d.add(ObjectRef{Key: o.Key})
			})
		}
		if err != nil {
			return nil, err
		}
	}
	if err := d.flush(); err != nil {
		return nil, err
	}
	b.deps.Logger.InfoContext(ctx, "admin: objects deleted", "bucket", in.Bucket, "deleted", len(d.out.Deleted), "failed", len(d.out.Errors))
	return d.out, nil
}

// deleter authorizes and batches the deletions of Delete.
type deleter struct {
	b       *objectBrowser
	ctx     context.Context
	req     Requester
	bucket  string
	pending []object.ObjectToDelete
	out     *DeleteObjectsResult
}

func (d *deleter) fail(ref ObjectRef, err error) {
	d.out.Errors = append(d.out.Errors, ObjectError{Key: ref.Key, VersionID: ref.VersionID, Err: err})
}

// add authorizes one deletion as the S3 DeleteObject of the same version
// (s3:versionid) and queues it, flushing full batches.
func (d *deleter) add(ref ObjectRef) error {
	action := policy.ActionDeleteObject
	if ref.VersionID != "" {
		action = policy.ActionDeleteObjectVersion
	}
	err := authorizeObject(d.ctx, d.b.deps.Authz, d.req, d.bucket, ref.Key, ObjectRequest{VersionID: ref.VersionID}, action)
	if errors.Is(err, domain.ErrAccessDenied) {
		d.fail(ref, err)
		return nil
	}
	if err != nil {
		return err
	}
	d.pending = append(d.pending, object.ObjectToDelete{Key: ref.Key, VersionID: ref.VersionID})
	if len(d.pending) >= deleteBatch {
		return d.flush()
	}
	return nil
}

// flush deletes the queued objects.
func (d *deleter) flush() error {
	if len(d.pending) == 0 {
		return nil
	}
	res, err := d.b.deps.Objects.DeleteObjects(d.ctx, object.DeleteObjectsInput{Bucket: d.bucket, Objects: d.pending})
	if err != nil {
		return err
	}
	d.pending = d.pending[:0]
	for _, o := range res.Deleted {
		d.out.Deleted = append(d.out.Deleted, DeletedObject(o))
	}
	for _, e := range res.Errors {
		d.out.Errors = append(d.out.Errors, ObjectError(e))
	}
	return nil
}

// authorizeWalk authorizes the listing behind the walk of a prefix:
// s3:ListBucket, or s3:ListBucketVersions for every version, with the
// s3:prefix condition, as the S3 API does for the same listing. A bulk
// operation on a prefix the requester may not list fails as a whole,
// before any output: its per-object errors would otherwise disclose the
// keys under the prefix.
func (b *objectBrowser) authorizeWalk(ctx context.Context, req Requester, bucket, prefix string, versions bool) error {
	action := policy.ActionListBucket
	if versions {
		action = policy.ActionListBucketVersions
	}
	d, err := b.deps.Authz.Authorize(ctx, AuthorizeInput{
		Requester: req, Actions: []policy.Action{action}, Bucket: bucket,
		Conditions: ListingConditions(prefix, "", 0),
	})
	if err != nil {
		return err
	}
	if !d.Allowed {
		return denied(d)
	}
	return nil
}

// authorizeWalks authorizes the walks of every prefix (see authorizeWalk).
func (b *objectBrowser) authorizeWalks(ctx context.Context, req Requester, bucket string, prefixes []string, versions bool) error {
	for _, p := range prefixes {
		if err := b.authorizeWalk(ctx, req, bucket, p, versions); err != nil {
			return err
		}
	}
	return nil
}

// walkObjects calls fn for the latest version of every object under
// prefix, page by page (fn may delete what it is given). The caller
// authorizes the listing first (authorizeWalk).
func (b *objectBrowser) walkObjects(ctx context.Context, bucket, prefix string, fn func(domain.ObjectInfo) error) error {
	token := ""
	for {
		res, err := b.deps.Objects.ListObjects(ctx, domain.ListObjectsInput{Bucket: bucket, Prefix: prefix, ContinuationToken: token, MaxKeys: maxListLimit})
		if err != nil {
			return err
		}
		for _, o := range res.Objects {
			if err := fn(o); err != nil {
				return err
			}
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return nil
		}
		token = res.NextContinuationToken
	}
}

// walkVersions calls fn for every version and delete marker under prefix.
func (b *objectBrowser) walkVersions(ctx context.Context, bucket, prefix string, fn func(domain.ObjectInfo) error) error {
	var marker, vmarker string
	for {
		res, err := b.deps.Objects.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: bucket, Prefix: prefix, Marker: marker, VersionIDMarker: vmarker, MaxKeys: maxListLimit})
		if err != nil {
			return err
		}
		for _, o := range res.Objects {
			if err := fn(o); err != nil {
				return err
			}
		}
		if !res.IsTruncated || res.NextMarker == "" {
			return nil
		}
		marker, vmarker = res.NextMarker, res.NextVersionIDMarker
	}
}

// Rename implements ObjectBrowser.
func (b *objectBrowser) Rename(ctx context.Context, in RenameInput) (*RenameResult, error) {
	if in.Source == "" || in.Destination == "" {
		return nil, fmt.Errorf("%w: source and destination are required", domain.ErrInvalidArgument)
	}
	if in.Source == in.Destination {
		return nil, fmt.Errorf("%w: source and destination are the same", domain.ErrInvalidArgument)
	}
	out := &RenameResult{Renamed: []RenamedObject{}, Errors: []RenameError{}}
	if !in.Prefix {
		if err := domain.ValidateObjectKey(in.Source); err != nil {
			return nil, err
		}
		if err := domain.ValidateObjectKey(in.Destination); err != nil {
			return nil, err
		}
		b.renameOne(ctx, in, in.Source, in.Destination, out)
		if len(out.Errors) == 1 && len(out.Renamed) == 0 {
			// A single move reports its failure as the error of the call.
			return nil, out.Errors[0].Err
		}
		return out, nil
	}
	in.Source, in.Destination = folderPrefix(in.Source), folderPrefix(in.Destination)
	if err := domain.ValidateObjectPrefix(in.Source); err != nil {
		return nil, err
	}
	if err := domain.ValidateObjectPrefix(in.Destination); err != nil {
		return nil, err
	}
	if strings.HasPrefix(in.Destination, in.Source) {
		return nil, fmt.Errorf("%w: the destination %q lies under the source %q", domain.ErrInvalidArgument, in.Destination, in.Source)
	}
	if err := b.authorizeWalk(ctx, in.Requester, in.Bucket, in.Source, false); err != nil {
		return nil, err
	}
	err := b.walkObjects(ctx, in.Bucket, in.Source, func(o domain.ObjectInfo) error {
		b.renameOne(ctx, in, o.Key, in.Destination+strings.TrimPrefix(o.Key, in.Source), out)
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	b.deps.Logger.InfoContext(ctx, "admin: objects renamed", "bucket", in.Bucket, "source", in.Source, "destination", in.Destination,
		"renamed", len(out.Renamed), "failed", len(out.Errors))
	return out, nil
}

// renameOne moves one object and records the outcome.
func (b *objectBrowser) renameOne(ctx context.Context, in RenameInput, src, dst string, out *RenameResult) {
	fail := func(err error) { out.Errors = append(out.Errors, RenameError{Source: src, Destination: dst, Err: err}) }
	if err := domain.ValidateObjectKey(dst); err != nil {
		fail(err)
		return
	}
	// The S3 requests a move stands for: GetObject and DeleteObject of the
	// source, CopyObject (PutObject with x-amz-copy-source) of the
	// destination.
	copySource := ObjectRequest{Headers: map[string]string{"x-amz-copy-source": copySourceOf(in.Bucket, src, "")}}
	for _, check := range []struct {
		key    string
		or     ObjectRequest
		action policy.Action
	}{{src, ObjectRequest{}, policy.ActionGetObject}, {dst, copySource, policy.ActionPutObject}, {src, ObjectRequest{}, policy.ActionDeleteObject}} {
		if err := authorizeObject(ctx, b.deps.Authz, in.Requester, in.Bucket, check.key, check.or, check.action); err != nil {
			fail(err)
			return
		}
	}
	if !in.Overwrite {
		_, err := b.deps.Objects.GetObjectInfo(ctx, in.Bucket, dst, object.GetObjectOptions{})
		switch {
		case err == nil:
			fail(fmt.Errorf("%w: %q already exists", domain.ErrConflict, dst))
			return
		case !errors.Is(err, domain.ErrObjectNotFound):
			fail(err)
			return
		}
	}
	info, err := b.deps.Objects.GetObjectInfo(ctx, in.Bucket, src, object.GetObjectOptions{})
	if err != nil {
		fail(err)
		return
	}
	if info.Size > maxCopySize {
		err = b.copyMultipart(ctx, in.Bucket, info, dst)
	} else {
		_, err = b.deps.Objects.CopyObject(ctx, object.CopyObjectInput{SrcBucket: in.Bucket, SrcKey: src, DstBucket: in.Bucket, DstKey: dst})
	}
	if err != nil {
		fail(err)
		return
	}
	if _, err := b.deps.Objects.DeleteObject(ctx, in.Bucket, src, object.DeleteObjectOptions{}); err != nil {
		fail(fmt.Errorf("copied but the source was not deleted: %w", err))
		return
	}
	out.Renamed = append(out.Renamed, RenamedObject{Source: src, Destination: dst})
}

// copyMultipart copies an object larger than 5 GiB part by part, keeping
// its metadata and tags; a failure aborts the upload.
func (b *objectBrowser) copyMultipart(ctx context.Context, bucket string, src domain.ObjectInfo, dst string) error {
	expires := ""
	if !src.Expires.IsZero() {
		expires = src.Expires.UTC().Format(http.TimeFormat)
	}
	up, err := b.deps.Objects.NewMultipartUpload(ctx, object.NewMultipartInput{
		Bucket: bucket, Key: dst, ContentType: src.ContentType, ContentEncoding: src.ContentEncoding,
		ContentDisposition: src.ContentDisposition, ContentLanguage: src.ContentLanguage, CacheControl: src.CacheControl,
		Expires: expires, UserMetadata: src.UserMetadata, Tags: src.Tags, StorageClass: src.StorageClass,
	})
	if err != nil {
		return err
	}
	var parts []domain.CompletePart
	for n, off := 1, int64(0); off < src.Size; n, off = n+1, off+b.copyPartSize {
		end := min(off+b.copyPartSize, src.Size) - 1
		part, err := b.deps.Objects.CopyObjectPart(ctx, object.CopyObjectPartInput{
			SrcBucket: bucket, SrcKey: src.Key, SrcVersionID: src.VersionID, SrcRange: &domain.Range{Start: off, End: end},
			DstBucket: bucket, DstKey: dst, UploadID: up.UploadID, PartNumber: n,
		})
		if err != nil {
			b.abort(ctx, bucket, dst, up.UploadID)
			return err
		}
		parts = append(parts, domain.CompletePart{Number: part.Number, ETag: part.ETag})
	}
	if _, err := b.deps.Objects.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: bucket, Key: dst, UploadID: up.UploadID, Parts: parts}); err != nil {
		b.abort(ctx, bucket, dst, up.UploadID)
		return err
	}
	return nil
}

func (b *objectBrowser) abort(ctx context.Context, bucket, key, uploadID string) {
	if err := b.deps.Objects.AbortMultipartUpload(context.WithoutCancel(ctx), bucket, key, uploadID); err != nil {
		b.deps.Logger.WarnContext(ctx, "admin: abort multipart copy", "bucket", bucket, "key", key, "upload", uploadID, "err", err)
	}
}

// RestoreVersion implements ObjectBrowser.
func (b *objectBrowser) RestoreVersion(ctx context.Context, in RestoreVersionInput) (domain.ObjectInfo, error) {
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if in.VersionID == "" {
		return domain.ObjectInfo{}, fmt.Errorf("%w: the version to restore is required", domain.ErrInvalidArgument)
	}
	// The S3 request it stands for: CopyObject of key?versionId onto key.
	if err := authorizeObject(ctx, b.deps.Authz, in.Requester, in.Bucket, in.Key, ObjectRequest{VersionID: in.VersionID}, policy.ActionGetObjectVersion); err != nil {
		return domain.ObjectInfo{}, err
	}
	copySource := ObjectRequest{Headers: map[string]string{"x-amz-copy-source": copySourceOf(in.Bucket, in.Key, in.VersionID)}}
	if err := authorizeObject(ctx, b.deps.Authz, in.Requester, in.Bucket, in.Key, copySource, policy.ActionPutObject); err != nil {
		return domain.ObjectInfo{}, err
	}
	src, err := b.deps.Objects.GetObjectInfo(ctx, in.Bucket, in.Key, object.GetObjectOptions{VersionID: in.VersionID})
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if src.IsDeleteMarker {
		return domain.ObjectInfo{}, fmt.Errorf("%w: a delete marker cannot be restored", domain.ErrMethodNotAllowed)
	}
	if src.Size > maxCopySize {
		if err := b.copyMultipart(ctx, in.Bucket, src, in.Key); err != nil {
			return domain.ObjectInfo{}, err
		}
		return b.deps.Objects.GetObjectInfo(ctx, in.Bucket, in.Key, object.GetObjectOptions{})
	}
	return b.deps.Objects.CopyObject(ctx, object.CopyObjectInput{SrcBucket: in.Bucket, SrcKey: in.Key, SrcVersionID: in.VersionID, DstBucket: in.Bucket, DstKey: in.Key})
}

// SetTags implements ObjectBrowser.
func (b *objectBrowser) SetTags(ctx context.Context, in SetTagsInput) (map[string]string, error) {
	tags := in.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	if _, err := b.deps.Objects.PutObjectTags(ctx, in.Bucket, in.Key, tags, object.TagOptions{VersionID: in.VersionID}); err != nil {
		return nil, err
	}
	return tags, nil
}

// Open implements ObjectBrowser.
func (b *objectBrowser) Open(ctx context.Context, in OpenInput) (*Download, error) {
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return nil, err
	}
	rd, err := b.deps.Objects.GetObject(ctx, in.Bucket, in.Key, object.GetObjectOptions{VersionID: in.VersionID, Range: in.Range, Conditions: in.Conditions})
	if err != nil {
		return nil, err
	}
	ct := rd.Info.ContentType
	if ct == "" {
		ct = DefaultContentType
	}
	out := &Download{
		Reader:      rd,
		ContentType: ct,
		Disposition: DispositionAttachment,
		FileName:    fileName(in.Key),
		Partial:     in.Range != nil && rd.Length > 0,
	}
	if in.Preview {
		if safe, sandbox, ok := previewType(ct); ok {
			out.ContentType, out.Disposition, out.Sandbox = safe, DispositionInline, sandbox
		} else {
			// Active content (SVG, HTML, XML, scripts...) never renders in
			// the console origin: it is downloaded as opaque bytes.
			out.ContentType = DefaultContentType
		}
	}
	return out, nil
}

// fileName is the last segment of a key.
func fileName(key string) string {
	name := path.Base(strings.TrimSuffix(key, "/"))
	if name == "." || name == "/" || name == "" {
		return "download"
	}
	return name
}

// previewType decides whether a content type may be rendered inline in
// the console origin: raster images, PDF, plain text, JSON, audio and
// video. It returns the normalized type to serve and whether the response
// must be sandboxed (every inline type but PDF).
func previewType(contentType string) (string, bool, bool) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", false, false
	}
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return mediaType, true, true
	case "application/pdf":
		return mediaType, false, true
	case "text/plain":
		return "text/plain; charset=utf-8", true, true
	case "application/json":
		return "application/json; charset=utf-8", true, true
	}
	if strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/") {
		return mediaType, true, true
	}
	return "", false, false
}
