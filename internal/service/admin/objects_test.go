package admin_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func newBrowser(t *testing.T, authz admin.Authorizer, opts ...admin.ObjectBrowserOption) (admin.ObjectBrowser, *objectmock.Service) {
	objects := objectmock.NewService(t)
	b, err := admin.NewObjectBrowser(admin.ObjectBrowserDeps{Objects: objects, Authz: authz}, opts...)
	require.NoError(t, err)
	return b, objects
}

func TestBrowserList(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowAll(t))
	slash := "/"
	objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "docs/", Delimiter: "/", ContinuationToken: "c1", MaxKeys: 1000}).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "docs/"}, {Key: "docs/a.txt"}}, CommonPrefixes: []string{"docs/x/"}, IsTruncated: true, NextContinuationToken: "c2"}, nil)
	got, err := b.List(ctx, admin.ListObjectsInput{Requester: req("bob"), Bucket: "bkt", Prefix: "docs/", Delimiter: &slash, Cursor: "c1"})
	require.NoError(t, err)
	require.Len(t, got.Objects, 1, "the folder placeholder is not part of its content")
	assert.Equal(t, "docs/a.txt", got.Objects[0].Key)
	assert.Equal(t, "c2", got.NextCursor)

	_, err = b.List(ctx, admin.ListObjectsInput{Requester: req("bob"), Bucket: "bkt", Limit: 1001})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = b.List(ctx, admin.ListObjectsInput{Requester: req("bob"), Bucket: "bkt", Prefix: "a/../"})
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
}

// TestBrowserListDelimiterAndAuthorization: the service owns the default
// of the folder view ("/" when the request names no delimiter) and
// authorizes s3:ListBucket with the effective conditions.
func TestBrowserListDelimiterAndAuthorization(t *testing.T) {
	ctx := context.Background()
	empty, comma := "", ","
	tests := []struct {
		name      string
		delimiter *string
		limit     int
		want      string
		conds     map[string][]string
	}{
		{name: "default folder view", want: "/", conds: map[string][]string{"s3:prefix": {"p/"}, "s3:delimiter": {"/"}}},
		// The S3 request of a flat listing names no delimiter.
		{name: "flat listing", delimiter: &empty, want: "", conds: map[string][]string{"s3:prefix": {"p/"}}},
		{name: "other delimiter and limit", delimiter: &comma, limit: 5, want: ",", conds: map[string][]string{"s3:prefix": {"p/"}, "s3:delimiter": {","}, "s3:max-keys": {"5"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []admin.AuthorizeInput
			b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return true }))
			max := 1000
			if tt.limit != 0 {
				max = tt.limit
			}
			objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "p/", Delimiter: tt.want, MaxKeys: max}).Return(domain.ListObjectsResult{}, nil)
			_, err := b.List(ctx, admin.ListObjectsInput{Requester: req("bob"), Bucket: "bkt", Prefix: "p/", Delimiter: tt.delimiter, Limit: tt.limit})
			require.NoError(t, err)
			require.Len(t, asked, 1)
			assert.Equal(t, []policy.Action{policy.ActionListBucket}, asked[0].Actions)
			assert.Equal(t, "bkt", asked[0].Bucket)
			assert.Equal(t, tt.conds, asked[0].Conditions)
		})
	}
	t.Run("denied lists nothing", func(t *testing.T) {
		b, _ := newBrowser(t, allowing(t, func(admin.AuthorizeInput) bool { return false }))
		_, err := b.List(ctx, admin.ListObjectsInput{Requester: req("bob"), Bucket: "bkt", Prefix: "private/"})
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})
}

func TestBrowserStat(t *testing.T) {
	ctx := context.Background()
	t.Run("tags and versions when allowed", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "k", object.GetObjectOptions{}).Return(domain.ObjectInfo{Key: "k", Tags: map[string]string{"a": "1"}}, nil)
		objects.EXPECT().ListObjectVersions(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "k", MaxKeys: 10}).
			Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "k", VersionID: "v2"}, {Key: "k", VersionID: "v1"}, {Key: "k2"}}}, nil)
		d, err := b.Stat(ctx, admin.StatInput{Requester: req("bob"), Bucket: "bkt", Key: "k"})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1"}, d.Tags)
		assert.Nil(t, d.Info.Tags)
		require.Len(t, d.Versions, 2)
		assert.False(t, d.VersionsTruncated)
	})
	t.Run("nothing beyond the metadata when denied", func(t *testing.T) {
		b, objects := newBrowser(t, allowing(t, func(admin.AuthorizeInput) bool { return false }))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "k", object.GetObjectOptions{VersionID: "v1"}).Return(domain.ObjectInfo{Key: "k"}, nil)
		d, err := b.Stat(ctx, admin.StatInput{Requester: req("bob"), Bucket: "bkt", Key: "k", VersionID: "v1"})
		require.NoError(t, err)
		assert.Nil(t, d.Tags)
		assert.Nil(t, d.Versions)
	})
}

func TestBrowserVersionsCursor(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowAll(t))
	objects.EXPECT().ListObjectVersions(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "k", MaxKeys: 2}).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "k", VersionID: "v3"}, {Key: "k", VersionID: "v2"}}, IsTruncated: true, NextMarker: "k", NextVersionIDMarker: "v2"}, nil)
	page, err := b.Versions(ctx, admin.VersionsInput{Bucket: "bkt", Key: "k", Limit: 2})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	objects.EXPECT().ListObjectVersions(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "k", Marker: "k", VersionIDMarker: "v2", MaxKeys: 2}).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "k", VersionID: "v1"}, {Key: "kz", VersionID: "x"}}, IsTruncated: true, NextMarker: "kz"}, nil)
	page, err = b.Versions(ctx, admin.VersionsInput{Bucket: "bkt", Key: "k", Limit: 2, Cursor: page.NextCursor})
	require.NoError(t, err)
	require.Len(t, page.Versions, 1)
	assert.Empty(t, page.NextCursor, "another key ends the versions of k")

	_, err = b.Versions(ctx, admin.VersionsInput{Bucket: "bkt", Key: "other", Cursor: "eyJrIjoiayIsInYiOiJ2MiJ9"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "a cursor of another key")
	_, err = b.Versions(ctx, admin.VersionsInput{Bucket: "bkt", Key: "k", Cursor: "%%%"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestBrowserCreateFolder(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool {
		return in.Actions[0] == policy.ActionPutObject && in.Object == "docs/2026/"
	}))
	objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		return in.Key == "docs/2026/" && in.Size == 0 && in.ContentType == "application/x-directory"
	})).Return(domain.ObjectInfo{Key: "docs/2026/"}, nil)
	got, err := b.CreateFolder(ctx, admin.FolderInput{Requester: req("bob"), Bucket: "bkt", Prefix: "docs/2026"})
	require.NoError(t, err)
	assert.Equal(t, "docs/2026/", got.Key)
	_, err = b.CreateFolder(ctx, admin.FolderInput{Requester: req("bob"), Bucket: "bkt", Prefix: "other/"})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = b.CreateFolder(ctx, admin.FolderInput{Requester: req("bob"), Bucket: "bkt", Prefix: "/"})
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
}

func TestBrowserDelete(t *testing.T) {
	ctx := context.Background()
	// bob may list the bucket and delete under tmp/ only.
	b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool {
		return in.Actions[0] == policy.ActionListBucket || strings.HasPrefix(in.Object, "tmp/")
	}))
	objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "", MaxKeys: 1000}).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "keep.txt"}, {Key: "tmp/a"}}}, nil)
	objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "bkt", Objects: []object.ObjectToDelete{{Key: "tmp/x", VersionID: "v1"}, {Key: "tmp/a"}}}).
		Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "tmp/x", VersionID: "v1"}}, Errors: []object.DeleteError{{Key: "tmp/a", Err: domain.ErrObjectLocked}}}, nil)
	res, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: req("bob"), Bucket: "bkt",
		Objects: []admin.ObjectRef{{Key: "tmp/x", VersionID: "v1"}, {Key: "a//b"}}, Prefixes: []string{""}})
	require.NoError(t, err)
	require.Len(t, res.Deleted, 1)
	keys := []string{}
	for _, e := range res.Errors {
		keys = append(keys, e.Key)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"a//b", "keep.txt", "tmp/a"}, keys)

	_, err = b.Delete(ctx, admin.DeleteObjectsInput{Requester: req("bob"), Bucket: "bkt"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestBrowserBulkPrefixNeedsList: the bulk operations on prefixes walk
// the listing, so they need s3:ListBucket (s3:ListBucketVersions for every
// version) with s3:prefix; without it the whole request is denied before
// listing anything, so no key of the prefix leaks in the per-object errors
// or in the errors entry of a zip (regression: bob, allowed s3:GetObject
// and s3:DeleteObject on vault/tmp/* only, learned the keys of private/).
func TestBrowserBulkPrefixNeedsList(t *testing.T) {
	ctx := context.Background()
	bob := req("bob")
	tests := []struct {
		name   string
		action policy.Action
		prefix string
		run    func(b admin.ObjectBrowser) error
	}{
		{name: "delete a prefix", action: policy.ActionListBucket, prefix: "private/", run: func(b admin.ObjectBrowser) error {
			_, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: bob, Bucket: "vault", Prefixes: []string{"private/"}})
			return err
		}},
		{name: "delete every version of a prefix", action: policy.ActionListBucketVersions, prefix: "private/", run: func(b admin.ObjectBrowser) error {
			_, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: bob, Bucket: "vault", Prefixes: []string{"private/"}, AllVersions: true})
			return err
		}},
		{name: "delete objects and a prefix", action: policy.ActionListBucket, prefix: "", run: func(b admin.ObjectBrowser) error {
			_, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: bob, Bucket: "vault", Objects: []admin.ObjectRef{{Key: "tmp/a"}}, Prefixes: []string{""}})
			return err
		}},
		{name: "rename a prefix", action: policy.ActionListBucket, prefix: "private/", run: func(b admin.ObjectBrowser) error {
			_, err := b.Rename(ctx, admin.RenameInput{Requester: bob, Bucket: "vault", Source: "private/", Destination: "tmp/private/", Prefix: true})
			return err
		}},
		{name: "zip a prefix", action: policy.ActionListBucket, prefix: "", run: func(b admin.ObjectBrowser) error {
			_, err := b.Zip(ctx, admin.ZipInput{Requester: bob, Bucket: "vault", Prefixes: []string{""}}, func(string) (io.Writer, error) {
				t.Fatal("the archive must not be opened")
				return nil, nil
			})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []admin.AuthorizeInput
			// The object layer mock fails on any call: nothing is listed.
			b, _ := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool {
				switch in.Actions[0] {
				case policy.ActionListBucket, policy.ActionListBucketVersions:
					asked = append(asked, in)
					return false
				}
				return strings.HasPrefix(in.Object, "tmp/")
			}))
			err := tt.run(b)
			require.ErrorIs(t, err, domain.ErrAccessDenied)
			assert.Contains(t, err.Error(), string(tt.action))
			require.Len(t, asked, 1)
			assert.Equal(t, []policy.Action{tt.action}, asked[0].Actions)
			assert.Equal(t, "vault", asked[0].Bucket)
			assert.Empty(t, asked[0].Object)
			assert.Equal(t, map[string][]string{"s3:prefix": {tt.prefix}}, asked[0].Conditions)
		})
	}
}

// siblingStore makes the object layer mock list the keys of a bucket with
// sibling prefixes (photos/, photos2/, photosx.txt) by string prefix, the
// way the object layer does.
func siblingStore(objects *objectmock.Service) []string {
	keys := []string{"photos/a.txt", "photos2/b.txt", "photosx.txt"}
	objects.EXPECT().ListObjects(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error) {
			var out domain.ListObjectsResult
			for _, k := range keys {
				if strings.HasPrefix(k, in.Prefix) {
					out.Objects = append(out.Objects, domain.ObjectInfo{Key: k})
				}
			}
			return out, nil
		}).Maybe()
	return keys
}

// TestBrowserPrefixesAreFolders: the prefixes of the bulk operations
// name folders, as in the CLI: "photos" is "photos/", never photos2/ nor
// photosx.txt (regression: a prefix without its trailing slash deleted,
// moved and archived the siblings too).
func TestBrowserPrefixesAreFolders(t *testing.T) {
	ctx := context.Background()
	for _, prefix := range []string{"photos", "photos/"} {
		t.Run("delete "+prefix, func(t *testing.T) {
			var asked []admin.AuthorizeInput
			b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool {
				asked = append(asked, in)
				return true
			}))
			siblingStore(objects)
			var deleted []string
			objects.EXPECT().DeleteObjects(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.DeleteObjectsInput) (object.DeleteObjectsResult, error) {
				var out object.DeleteObjectsResult
				for _, o := range in.Objects {
					deleted = append(deleted, o.Key)
					out.Deleted = append(out.Deleted, object.DeletedObject{Key: o.Key})
				}
				return out, nil
			})
			_, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: req("bob"), Bucket: "bkt", Prefixes: []string{prefix}})
			require.NoError(t, err)
			assert.Equal(t, []string{"photos/a.txt"}, deleted)
			require.NotEmpty(t, asked)
			assert.Equal(t, map[string][]string{"s3:prefix": {"photos/"}}, asked[0].Conditions, "the listing is authorized on the folder")
		})
		t.Run("rename "+prefix, func(t *testing.T) {
			b, objects := newBrowser(t, allowAll(t))
			siblingStore(objects)
			objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", mock.Anything, object.GetObjectOptions{}).RunAndReturn(
				func(_ context.Context, _, key string, _ object.GetObjectOptions) (domain.ObjectInfo, error) {
					if strings.HasPrefix(key, "pics") {
						return domain.ObjectInfo{}, domain.ErrObjectNotFound
					}
					return domain.ObjectInfo{Key: key}, nil
				})
			objects.EXPECT().CopyObject(mock.Anything, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "photos/a.txt", DstBucket: "bkt", DstKey: "pics/a.txt"}).Return(domain.ObjectInfo{}, nil).Once()
			objects.EXPECT().DeleteObject(mock.Anything, "bkt", "photos/a.txt", object.DeleteObjectOptions{}).Return(object.DeletedObject{}, nil).Once()
			res, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: prefix, Destination: "pics", Prefix: true})
			require.NoError(t, err)
			assert.Equal(t, []admin.RenamedObject{{Source: "photos/a.txt", Destination: "pics/a.txt"}}, res.Renamed)
		})
		t.Run("zip "+prefix, func(t *testing.T) {
			b, objects := newBrowser(t, allowAll(t))
			siblingStore(objects)
			zipObjects(objects, map[string]string{"photos/a.txt": "A", "photos2/b.txt": "B", "photosx.txt": "X"})
			var buf bytes.Buffer
			var name string
			_, err := b.Zip(ctx, admin.ZipInput{Requester: req("bob"), Bucket: "bkt", Base: "photos", Prefixes: []string{prefix}},
				func(filename string) (io.Writer, error) { name = filename; return &buf, nil })
			require.NoError(t, err)
			assert.Equal(t, "photos.zip", name)
			assert.Equal(t, map[string]string{"a.txt": "A"}, readZip(t, buf.Bytes()))
		})
	}
	t.Run("a sibling destination is not under the source", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		siblingStore(objects)
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "photos2/a.txt", object.GetObjectOptions{}).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound)
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "photos/a.txt", object.GetObjectOptions{}).Return(domain.ObjectInfo{Key: "photos/a.txt"}, nil)
		objects.EXPECT().CopyObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, nil).Once()
		objects.EXPECT().DeleteObject(mock.Anything, "bkt", "photos/a.txt", object.DeleteObjectOptions{}).Return(object.DeletedObject{}, nil).Once()
		res, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "photos", Destination: "photos2", Prefix: true})
		require.NoError(t, err)
		assert.Equal(t, []admin.RenamedObject{{Source: "photos/a.txt", Destination: "photos2/a.txt"}}, res.Renamed)
	})
}

func TestBrowserDeleteAllVersionsInBatches(t *testing.T) {
	ctx := context.Background()
	b, objects := newBrowser(t, allowAll(t))
	var page []domain.ObjectInfo
	for i := range 1500 {
		page = append(page, domain.ObjectInfo{Key: fmt.Sprintf("p/%04d", i), VersionID: "v"})
	}
	objects.EXPECT().ListObjectVersions(mock.Anything, mock.Anything).Return(domain.ListObjectsResult{Objects: page}, nil)
	var batches []int
	objects.EXPECT().DeleteObjects(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.DeleteObjectsInput) (object.DeleteObjectsResult, error) {
		batches = append(batches, len(in.Objects))
		out := object.DeleteObjectsResult{}
		for _, o := range in.Objects {
			out.Deleted = append(out.Deleted, object.DeletedObject{Key: o.Key, VersionID: o.VersionID})
		}
		return out, nil
	})
	res, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: req("bob"), Bucket: "bkt", Prefixes: []string{"p/"}, AllVersions: true})
	require.NoError(t, err)
	assert.Equal(t, []int{1000, 500}, batches)
	assert.Len(t, res.Deleted, 1500)
}

func TestBrowserRename(t *testing.T) {
	ctx := context.Background()
	t.Run("single object", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "new.txt", object.GetObjectOptions{}).Return(domain.ObjectInfo{}, fmt.Errorf("x: %w", domain.ErrObjectNotFound))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "old.txt", object.GetObjectOptions{}).Return(domain.ObjectInfo{Key: "old.txt", Size: 3}, nil)
		objects.EXPECT().CopyObject(mock.Anything, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "old.txt", DstBucket: "bkt", DstKey: "new.txt"}).Return(domain.ObjectInfo{}, nil)
		objects.EXPECT().DeleteObject(mock.Anything, "bkt", "old.txt", object.DeleteObjectOptions{}).Return(object.DeletedObject{}, nil)
		res, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "old.txt", Destination: "new.txt"})
		require.NoError(t, err)
		assert.Equal(t, []admin.RenamedObject{{Source: "old.txt", Destination: "new.txt"}}, res.Renamed)
	})
	t.Run("existing destination", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "new.txt", object.GetObjectOptions{}).Return(domain.ObjectInfo{Key: "new.txt"}, nil)
		_, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "old.txt", Destination: "new.txt"})
		assert.ErrorIs(t, err, domain.ErrConflict)
	})
	t.Run("delete denied", func(t *testing.T) {
		b, _ := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Actions[0] != policy.ActionDeleteObject }))
		_, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "old.txt", Destination: "new.txt"})
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("prefix", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "a/", MaxKeys: 1000}).
			Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "a/1"}, {Key: "a/x/2"}}}, nil)
		objects.EXPECT().CopyObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{}, nil).Twice()
		objects.EXPECT().DeleteObject(mock.Anything, "bkt", mock.Anything, object.DeleteObjectOptions{}).Return(object.DeletedObject{}, nil).Twice()
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", mock.Anything, object.GetObjectOptions{}).RunAndReturn(
			func(_ context.Context, _, key string, _ object.GetObjectOptions) (domain.ObjectInfo, error) {
				if strings.HasPrefix(key, "b/") {
					return domain.ObjectInfo{}, domain.ErrObjectNotFound
				}
				return domain.ObjectInfo{Key: key}, nil
			})
		res, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "a/", Destination: "b/", Prefix: true})
		require.NoError(t, err)
		assert.Equal(t, []admin.RenamedObject{{Source: "a/1", Destination: "b/1"}, {Source: "a/x/2", Destination: "b/x/2"}}, res.Renamed)
	})
	t.Run("destination under the source", func(t *testing.T) {
		b, _ := newBrowser(t, allowAll(t))
		_, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "a/", Destination: "a/b/", Prefix: true})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
	t.Run("large objects are copied part by part", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t), admin.WithCopyPartSize(2<<30))
		size := int64(5<<30) + 1
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "dst", object.GetObjectOptions{}).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound)
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "src", object.GetObjectOptions{}).Return(domain.ObjectInfo{Key: "src", Size: size, ContentType: "video/mp4"}, nil)
		objects.EXPECT().NewMultipartUpload(mock.Anything, mock.MatchedBy(func(in object.NewMultipartInput) bool { return in.ContentType == "video/mp4" })).
			Return(domain.MultipartUpload{UploadID: "u"}, nil)
		var ranges []domain.Range
		objects.EXPECT().CopyObjectPart(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.CopyObjectPartInput) (domain.Part, error) {
			ranges = append(ranges, *in.SrcRange)
			return domain.Part{Number: in.PartNumber, ETag: "e"}, nil
		}).Times(3)
		objects.EXPECT().CompleteMultipartUpload(mock.Anything, mock.MatchedBy(func(in object.CompleteMultipartInput) bool { return len(in.Parts) == 3 })).Return(domain.ObjectInfo{}, nil)
		objects.EXPECT().DeleteObject(mock.Anything, "bkt", "src", object.DeleteObjectOptions{}).Return(object.DeletedObject{}, nil)
		_, err := b.Rename(ctx, admin.RenameInput{Requester: req("bob"), Bucket: "bkt", Source: "src", Destination: "dst"})
		require.NoError(t, err)
		require.Len(t, ranges, 3)
		assert.Equal(t, size-1, ranges[2].End)
	})
}

func TestBrowserOpenPreviewTypes(t *testing.T) {
	tests := []struct {
		stored, served, disposition string
		sandbox                     bool
	}{
		{"image/png", "image/png", admin.DispositionInline, true},
		{"image/jpeg", "image/jpeg", admin.DispositionInline, true},
		{"application/pdf", "application/pdf", admin.DispositionInline, false},
		{"text/plain; charset=latin1", "text/plain; charset=utf-8", admin.DispositionInline, true},
		{"application/json", "application/json; charset=utf-8", admin.DispositionInline, true},
		{"video/mp4", "video/mp4", admin.DispositionInline, true},
		{"audio/mpeg", "audio/mpeg", admin.DispositionInline, true},
		{"image/svg+xml", "application/octet-stream", admin.DispositionAttachment, false},
		{"text/html", "application/octet-stream", admin.DispositionAttachment, false},
		{"application/xhtml+xml", "application/octet-stream", admin.DispositionAttachment, false},
		{"text/xml", "application/octet-stream", admin.DispositionAttachment, false},
		{"", "application/octet-stream", admin.DispositionAttachment, false},
		{"garbage;;", "application/octet-stream", admin.DispositionAttachment, false},
	}
	for _, tt := range tests {
		t.Run(tt.stored, func(t *testing.T) {
			b, objects := newBrowser(t, allowAll(t))
			objects.EXPECT().GetObject(mock.Anything, "bkt", "dir/f", object.GetObjectOptions{}).
				Return(&object.Reader{ReadCloser: io.NopCloser(strings.NewReader("x")), Info: domain.ObjectInfo{ContentType: tt.stored}, Length: 1}, nil)
			d, err := b.Open(context.Background(), admin.OpenInput{Bucket: "bkt", Key: "dir/f", Preview: true})
			require.NoError(t, err)
			assert.Equal(t, tt.served, d.ContentType)
			assert.Equal(t, tt.disposition, d.Disposition)
			assert.Equal(t, tt.sandbox, d.Sandbox)
			assert.Equal(t, "f", d.FileName)
		})
	}
	t.Run("downloads keep the type as attachments", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().GetObject(mock.Anything, "bkt", "page.html", mock.Anything).
			Return(&object.Reader{ReadCloser: io.NopCloser(strings.NewReader("x")), Info: domain.ObjectInfo{ContentType: "text/html"}, Length: 1}, nil)
		d, err := b.Open(context.Background(), admin.OpenInput{Bucket: "bkt", Key: "page.html", Range: &domain.Range{Start: 0, End: 0}})
		require.NoError(t, err)
		assert.Equal(t, "text/html", d.ContentType)
		assert.Equal(t, admin.DispositionAttachment, d.Disposition)
		assert.True(t, d.Partial)
	})
}

// zipObjects serves objects from a map through the mocked object layer.
func zipObjects(objects *objectmock.Service, content map[string]string) {
	objects.EXPECT().GetObject(mock.Anything, "bkt", mock.Anything, object.GetObjectOptions{}).RunAndReturn(
		func(_ context.Context, _, key string, _ object.GetObjectOptions) (*object.Reader, error) {
			body, ok := content[key]
			if !ok {
				return nil, domain.ErrObjectNotFound
			}
			return &object.Reader{ReadCloser: io.NopCloser(strings.NewReader(body)), Info: domain.ObjectInfo{Key: key, ContentType: "text/plain"}, Length: int64(len(body))}, nil
		}).Maybe()
}

func readZip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		body, err := io.ReadAll(rc)
		require.NoError(t, err)
		_ = rc.Close()
		out[f.Name] = string(body)
	}
	return out
}

func TestBrowserZip(t *testing.T) {
	ctx := context.Background()
	content := map[string]string{
		"logs/a.txt": "A", "logs/sub/b.txt": "B", "logs/secret/c.txt": "C", "logs/gone.txt": "", "top.txt": "T",
		"logs/we:ird\\..\\name.txt": "W",
	}
	b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool { return !strings.Contains(in.Object, "secret") }))
	zipObjects(objects, content)
	delete(content, "logs/gone.txt")
	objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "top.txt", object.GetObjectOptions{}).Return(domain.ObjectInfo{Key: "top.txt"}, nil)
	objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "logs/", MaxKeys: 1}).Return(domain.ListObjectsResult{}, nil)
	objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "logs/", MaxKeys: 1000}).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{
			{Key: "logs/"}, {Key: "logs/a.txt"}, {Key: "logs/gone.txt"}, {Key: "logs/secret/c.txt"}, {Key: "logs/sub/b.txt"}, {Key: "logs/we:ird\\..\\name.txt"},
		}}, nil)
	var buf bytes.Buffer
	var name string
	res, err := b.Zip(ctx, admin.ZipInput{Requester: req("bob"), Bucket: "bkt", Base: "logs/", Keys: []string{"top.txt"}, Prefixes: []string{"logs/"}},
		func(filename string) (io.Writer, error) { name = filename; return &buf, nil })
	require.NoError(t, err)
	assert.Equal(t, "logs.zip", name)
	files := readZip(t, buf.Bytes())
	assert.Equal(t, "T", files["top.txt"], "a key outside the base keeps its path")
	assert.Equal(t, "A", files["a.txt"])
	assert.Equal(t, "B", files["sub/b.txt"])
	assert.Equal(t, "W", files["we_ird/name.txt"], "backslashes, dot segments and ':' are neutralized")
	assert.NotContains(t, files, "secret/c.txt")
	assert.Contains(t, files["nexo-zip-errors.txt"], "logs/secret/c.txt")
	assert.Contains(t, files["nexo-zip-errors.txt"], "logs/gone.txt")
	assert.Equal(t, 4, res.Files)
	assert.Len(t, res.Skipped, 2)
}

func TestBrowserZipFailsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	opened := false
	open := func(string) (io.Writer, error) { opened = true; return io.Discard, nil }
	t.Run("missing explicit object", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "nope", object.GetObjectOptions{}).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound)
		_, err := b.Zip(ctx, admin.ZipInput{Requester: req("bob"), Bucket: "bkt", Keys: []string{"nope"}}, open)
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	})
	t.Run("forbidden explicit object", func(t *testing.T) {
		b, _ := newBrowser(t, allowing(t, func(admin.AuthorizeInput) bool { return false }))
		_, err := b.Zip(ctx, admin.ZipInput{Requester: req("bob"), Bucket: "bkt", Keys: []string{"k"}}, open)
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("nothing to archive", func(t *testing.T) {
		b, _ := newBrowser(t, allowAll(t))
		_, err := b.Zip(ctx, admin.ZipInput{Requester: req("bob"), Bucket: "bkt"}, open)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
	assert.False(t, opened)
}

func TestBrowserZipLimit(t *testing.T) {
	b, objects := newBrowser(t, allowAll(t), admin.WithZipMaxObjects(2))
	zipObjects(objects, map[string]string{"p/1": "1", "p/2": "2", "p/3": "3"})
	objects.EXPECT().ListObjects(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool { return in.MaxKeys == 1 })).Return(domain.ListObjectsResult{}, nil)
	objects.EXPECT().ListObjects(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool { return in.MaxKeys == 1000 })).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "p/1"}, {Key: "p/2"}, {Key: "p/3"}}}, nil)
	var buf bytes.Buffer
	res, err := b.Zip(context.Background(), admin.ZipInput{Requester: req("bob"), Bucket: "bkt", Prefixes: []string{"p/"}},
		func(string) (io.Writer, error) { return &buf, nil })
	require.NoError(t, err)
	assert.Equal(t, 2, res.Files)
	files := readZip(t, buf.Bytes())
	assert.Contains(t, files["nexo-zip-errors.txt"], "truncated")
}

func TestBrowserSetTags(t *testing.T) {
	b, objects := newBrowser(t, allowAll(t))
	objects.EXPECT().PutObjectTags(mock.Anything, "bkt", "k", map[string]string{}, object.TagOptions{VersionID: "v"}).Return(domain.ObjectInfo{}, nil)
	tags, err := b.SetTags(context.Background(), admin.SetTagsInput{Bucket: "bkt", Key: "k", VersionID: "v"})
	require.NoError(t, err)
	assert.Empty(t, tags)
}

// TestObjectAuthorizationsCarryS3Conditions: every object authorization of
// the services sees the condition values of the S3 request it stands for
// (regression: a Deny conditioned on s3:versionid was bypassed through the
// console because only the aws:* keys reached the policy engine).
func TestObjectAuthorizationsCarryS3Conditions(t *testing.T) {
	ctx := context.Background()
	// The policy denies version v1 only, as a bucket policy with
	// StringEquals s3:versionid would.
	denyV1 := func(in admin.AuthorizeInput) bool {
		v := in.Conditions["s3:versionid"]
		return len(v) == 0 || v[0] != "v1"
	}

	t.Run("delete", func(t *testing.T) {
		b, objects := newBrowser(t, allowing(t, denyV1))
		objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "bkt", Objects: []object.ObjectToDelete{{Key: "doc.txt", VersionID: "v2"}}}).
			Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "doc.txt", VersionID: "v2"}}}, nil)
		res, err := b.Delete(ctx, admin.DeleteObjectsInput{Requester: req("frank"), Bucket: "bkt",
			Objects: []admin.ObjectRef{{Key: "doc.txt", VersionID: "v1"}, {Key: "doc.txt", VersionID: "v2"}}})
		require.NoError(t, err)
		require.Len(t, res.Errors, 1)
		assert.Equal(t, "v1", res.Errors[0].VersionID)
		assert.ErrorIs(t, res.Errors[0].Err, domain.ErrAccessDenied)
	})

	t.Run("stat tags", func(t *testing.T) {
		var asked []admin.AuthorizeInput
		b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return denyV1(in) }))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "doc.txt", object.GetObjectOptions{VersionID: "v1"}).Return(domain.ObjectInfo{Key: "doc.txt", Tags: map[string]string{"a": "1"}}, nil)
		objects.EXPECT().ListObjectVersions(mock.Anything, mock.Anything).Return(domain.ListObjectsResult{}, nil).Maybe()
		d, err := b.Stat(ctx, admin.StatInput{Requester: req("frank"), Bucket: "bkt", Key: "doc.txt", VersionID: "v1"})
		require.NoError(t, err)
		assert.Nil(t, d.Tags, "the tags of the denied version are not disclosed")
		require.NotEmpty(t, asked)
		assert.Equal(t, []policy.Action{policy.ActionGetObjectVersionTagging}, asked[0].Actions)
	})

	t.Run("share link of a version", func(t *testing.T) {
		var asked []admin.AuthorizeInput
		e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return denyV1(in) }))
		_, err := e.mgr.Create(ctx, admin.CreateShareInput{Requester: req("frank"), Bucket: "bkt", Key: "doc.txt", VersionID: "v1"})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
		require.Len(t, asked, 1)
		assert.Equal(t, []policy.Action{policy.ActionGetObjectVersion}, asked[0].Actions)
		assert.Equal(t, map[string][]string{"s3:versionid": {"v1"}, "s3:authType": {"REST-QUERY-STRING"}, "s3:signatureversion": {"AWS4-HMAC-SHA256"}}, asked[0].Conditions)
	})

	t.Run("presigned URL", func(t *testing.T) {
		var asked []admin.AuthorizeInput
		e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return false }))
		_, err := e.mgr.Presign(ctx, admin.PresignInput{Requester: req("frank"), Method: "PUT", Bucket: "bkt", Key: "doc.txt",
			Headers: map[string]string{"X-Amz-Tagging": "class=secret"}})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
		require.Len(t, asked, 1)
		assert.Equal(t, map[string][]string{
			"s3:authType": {"REST-QUERY-STRING"}, "s3:signatureversion": {"AWS4-HMAC-SHA256"}, "s3:x-amz-tagging": {"class=secret"},
			"s3:RequestObjectTag/class": {"secret"}, "s3:RequestObjectTagKeys": {"class"},
		}, asked[0].Conditions)
	})
}

// covers: D-OBJ-06 (restoring a version copies it onto its key, authorized
// as the S3 CopyObject it stands for)
func TestBrowserRestoreVersion(t *testing.T) {
	ctx := context.Background()
	t.Run("copies the version onto its key", func(t *testing.T) {
		var asked []admin.AuthorizeInput
		b, objects := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return true }))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "doc.txt", object.GetObjectOptions{VersionID: "v1"}).Return(domain.ObjectInfo{Key: "doc.txt", VersionID: "v1", Size: 2}, nil)
		objects.EXPECT().CopyObject(mock.Anything, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "doc.txt", SrcVersionID: "v1", DstBucket: "bkt", DstKey: "doc.txt"}).
			Return(domain.ObjectInfo{Key: "doc.txt", VersionID: "v3", IsLatest: true}, nil)
		got, err := b.RestoreVersion(ctx, admin.RestoreVersionInput{Requester: req("bob"), Bucket: "bkt", Key: "doc.txt", VersionID: "v1"})
		require.NoError(t, err)
		assert.Equal(t, "v3", got.VersionID)
		require.Len(t, asked, 2)
		assert.Equal(t, []policy.Action{policy.ActionGetObjectVersion}, asked[0].Actions)
		assert.Equal(t, []string{"v1"}, asked[0].Conditions["s3:versionid"])
		assert.Equal(t, []policy.Action{policy.ActionPutObject}, asked[1].Actions)
		assert.Equal(t, []string{"bkt/doc.txt?versionId=v1"}, asked[1].Conditions["s3:x-amz-copy-source"])
	})
	t.Run("a denied version is not read", func(t *testing.T) {
		b, _ := newBrowser(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Actions[0] != policy.ActionGetObjectVersion }))
		_, err := b.RestoreVersion(ctx, admin.RestoreVersionInput{Requester: req("bob"), Bucket: "bkt", Key: "doc.txt", VersionID: "v1"})
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("a delete marker cannot be restored", func(t *testing.T) {
		b, objects := newBrowser(t, allowAll(t))
		objects.EXPECT().GetObjectInfo(mock.Anything, "bkt", "doc.txt", object.GetObjectOptions{VersionID: "dm"}).Return(domain.ObjectInfo{Key: "doc.txt", VersionID: "dm", IsDeleteMarker: true}, nil)
		_, err := b.RestoreVersion(ctx, admin.RestoreVersionInput{Requester: req("bob"), Bucket: "bkt", Key: "doc.txt", VersionID: "dm"})
		assert.ErrorIs(t, err, domain.ErrMethodNotAllowed)
	})
	t.Run("the version is required", func(t *testing.T) {
		b, _ := newBrowser(t, allowAll(t))
		_, err := b.RestoreVersion(ctx, admin.RestoreVersionInput{Requester: req("bob"), Bucket: "bkt", Key: "doc.txt"})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}
