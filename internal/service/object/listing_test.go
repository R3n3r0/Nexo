package object_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// listAll pages through a listing and returns every key and common prefix
// in the order they were returned, failing on duplicates.
func listAll(t *testing.T, svc object.Service, in domain.ListObjectsInput, v2 bool) []string {
	t.Helper()
	var out []string
	for pages := 0; pages < 10000; pages++ {
		res, err := svc.ListObjects(context.Background(), in)
		require.NoError(t, err)
		require.LessOrEqual(t, len(res.Objects)+len(res.CommonPrefixes), max(in.MaxKeys, 1), "a page never exceeds max-keys")
		// Objects and prefixes of a page interleave in key order.
		page := make([]string, 0, len(res.Objects)+len(res.CommonPrefixes))
		for _, o := range res.Objects {
			page = append(page, o.Key)
		}
		page = append(page, res.CommonPrefixes...)
		sort.Strings(page)
		out = append(out, page...)
		if !res.IsTruncated {
			return out
		}
		if v2 {
			require.NotEmpty(t, res.NextContinuationToken)
			in.ContinuationToken = res.NextContinuationToken
		} else {
			require.NotEmpty(t, res.NextMarker)
			in.Marker = res.NextMarker
		}
	}
	t.Fatal("listing does not terminate")
	return nil
}

// TS-I-05 (scaled down): nested prefixes, every page size, v1 and v2,
// with and without delimiter: nothing duplicated, nothing omitted.
func TestListingPagination(t *testing.T) {
	t.Parallel()
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		var keys []string
		for i := 0; i < 6; i++ {
			for j := 0; j < 5; j++ {
				keys = append(keys, fmt.Sprintf("p%d/q%d/obj", i, j), fmt.Sprintf("p%d/f%02d", i, j))
			}
			keys = append(keys, fmt.Sprintf("top-%d", i), fmt.Sprintf("p%d.txt", i))
		}
		keys = append(keys, "p0/", "ü-unicode", "a b c")
		for _, k := range keys {
			put(t, fx.svc, "bkt", k, []byte(k))
		}
		sort.Strings(keys)

		rootView := map[string]bool{}
		for _, k := range keys {
			if i := strings.Index(k, "/"); i >= 0 {
				rootView[k[:i+1]] = true
			} else {
				rootView[k] = true
			}
		}
		var wantRoot []string
		for k := range rootView {
			wantRoot = append(wantRoot, k)
		}
		sort.Strings(wantRoot)
		var wantP3 []string
		for _, k := range keys {
			if strings.HasPrefix(k, "p3/") {
				wantP3 = append(wantP3, k)
			}
		}

		for _, size := range []int{1, 2, 7, 1000} {
			for _, v2 := range []bool{false, true} {
				t.Run(fmt.Sprintf("max-keys=%d v2=%v", size, v2), func(t *testing.T) {
					assert.Equal(t, keys, listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: size}, v2))
					assert.Equal(t, wantRoot, listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: size, Delimiter: "/"}, v2))
					assert.Equal(t, wantP3, listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: size, Prefix: "p3/"}, v2))
					assert.Equal(t, []string{"p3/f00", "p3/f01", "p3/f02", "p3/f03", "p3/f04", "p3/q0/", "p3/q1/", "p3/q2/", "p3/q3/", "p3/q4/"},
						listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: size, Prefix: "p3/", Delimiter: "/"}, v2))
				})
			}
		}

		// start-after.
		got := listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 5, StartAfter: "p5/q4/obj"}, true)
		var want []string
		for _, k := range keys {
			if k > "p5/q4/obj" {
				want = append(want, k)
			}
		}
		assert.Equal(t, want, got)
		// The token is opaque: the start-after of a continuation is ignored.
		res, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1})
		require.NoError(t, err)
		res, err = fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1, ContinuationToken: res.NextContinuationToken, StartAfter: "zzz"})
		require.NoError(t, err)
		require.Len(t, res.Objects, 1)
		assert.Equal(t, keys[1], res.Objects[0].Key)
	})
}

// DeleteObjects: per-key results, quiet mode, limits.
func TestDeleteObjectsBatch(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for i := 0; i < 5; i++ {
			put(t, fx.svc, "bkt", fmt.Sprint("k", i), []byte("x"))
		}
		objs := []object.ObjectToDelete{{Key: "k0"}, {Key: "k1"}, {Key: "missing"}, {Key: "../bad"}, {Key: "k2"}}
		res, err := fx.svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "bkt", Objects: objs, Quiet: true})
		require.NoError(t, err)
		assert.Empty(t, res.Deleted, "quiet mode reports errors only")
		require.Len(t, res.Errors, 1)
		assert.Equal(t, "../bad", res.Errors[0].Key)
		assert.ErrorIs(t, res.Errors[0].Err, domain.ErrInvalidObjectName)

		res, err = fx.svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "bkt", Objects: []object.ObjectToDelete{{Key: "k3"}, {Key: "k3"}, {Key: "k4", VersionID: "null"}}})
		require.NoError(t, err)
		require.Len(t, res.Deleted, 3)
		assert.Equal(t, "k3", res.Deleted[0].Key)
		assert.Equal(t, "null", res.Deleted[2].VersionID)
		list, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Empty(t, list.Objects)

		tooMany := make([]object.ObjectToDelete, 1001)
		for i := range tooMany {
			tooMany[i] = object.ObjectToDelete{Key: fmt.Sprint("k", i)}
		}
		_, err = fx.svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "bkt", Objects: tooMany})
		assert.ErrorIs(t, err, domain.ErrMalformedXML)
		_, err = fx.svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "nobucket", Objects: objs})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		_, err = fx.svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "Bad_Bucket", Objects: objs})
		assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = fx.svc.DeleteObjects(cancelled, object.DeleteObjectsInput{Bucket: "bkt", Objects: objs})
		assert.ErrorIs(t, err, context.Canceled)
	})
}

// TS-I-05 at full scale: 10k objects under nested prefixes, listed with
// pages of 1000 and of 1 (fs only, skipped with -short).
func TestListingTenThousandObjects(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("10k objects: not in -short")
	}
	fx := newFixture(t, "fs")
	ctx := context.Background()
	require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	// Under the race detector a fifth of the objects keeps the same shape
	// (every d%d prefix still holds the 37 s%02d sub-prefixes, 370 keys
	// are enough) without dominating the package's run time.
	n := 10000
	if raceEnabled {
		n = 2000
	}
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		keys = append(keys, fmt.Sprintf("d%d/s%02d/o%04d", i%10, i%37, i))
	}
	for _, k := range keys {
		put(t, fx.svc, "bkt", k, nil)
	}
	sort.Strings(keys)
	assert.Equal(t, keys, listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1000}, true))
	var d3 []string
	for _, k := range keys {
		if strings.HasPrefix(k, "d3/") {
			d3 = append(d3, k)
		}
	}
	assert.Equal(t, d3, listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1, Prefix: "d3/"}, false))
	prefixes := listAll(t, fx.svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 1, Prefix: "d3/", Delimiter: "/"}, true)
	assert.Len(t, prefixes, 37)
}

// Regression: max-keys=0 (and max-uploads=0, max-parts=0) returned the
// default 1000 entries. domain.ListLimitNone asks for none: the bucket and
// the arguments are still validated, the result is empty and not
// truncated; zero keeps meaning the default.
func TestListingLimitNone(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, k := range []string{"a", "b/c", "d"} {
			put(t, fx.svc, "bkt", k, []byte(k))
		}
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "mp"})
		require.NoError(t, err)
		for n := 1; n <= 2; n++ {
			_, err = fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, PartNumber: n,
				Reader: strings.NewReader("part"), Size: 4})
			require.NoError(t, err)
		}

		tests := []struct {
			name          string
			limit         int
			wantKeys      int
			wantTruncated bool
			wantParts     int
		}{
			{name: "none", limit: domain.ListLimitNone, wantKeys: 0, wantTruncated: false, wantParts: 0},
			{name: "zero is the default", limit: 0, wantKeys: 3, wantParts: 2},
			{name: "one", limit: 1, wantKeys: 1, wantTruncated: true, wantParts: 1},
			{name: "above the cap", limit: 5000, wantKeys: 3, wantParts: 2},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				res, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: tt.limit})
				require.NoError(t, err)
				assert.Len(t, res.Objects, tt.wantKeys)
				assert.Empty(t, res.CommonPrefixes)
				assert.Equal(t, tt.wantTruncated, res.IsTruncated)

				versions, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: tt.limit})
				require.NoError(t, err)
				assert.Len(t, versions.Objects, tt.wantKeys)
				assert.Equal(t, tt.wantTruncated, versions.IsTruncated)

				uploads, err := fx.svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", MaxUploads: tt.limit})
				require.NoError(t, err)
				assert.Len(t, uploads.Uploads, min(tt.wantKeys, 1))
				assert.False(t, uploads.IsTruncated)

				parts, err := fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, MaxParts: tt.limit})
				require.NoError(t, err)
				assert.Len(t, parts.Parts, tt.wantParts)
				assert.Equal(t, tt.wantTruncated, parts.IsTruncated)
				assert.Equal(t, up.UploadID, parts.Upload.UploadID)
			})
		}
		// The bucket and the upload are still checked.
		_, err = fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "missing", MaxKeys: domain.ListLimitNone})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		_, err = fx.svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "missing", MaxUploads: domain.ListLimitNone})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		_, err = fx.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "mp", UploadID: "bogus", MaxParts: domain.ListLimitNone})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload)
	})
}

// Regression: a marker (start-after) inside a common prefix dropped the
// prefix although keys after the marker roll up into it. S3 lists the keys
// after the marker, then rolls them up.
func TestListingMarkerInsideCommonPrefix(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, k := range []string{"a/b", "a/c", "a/d/e", "b", "x-1-a", "x-1-b", "x-2"} {
			put(t, fx.svc, "bkt", k, []byte(k))
		}
		tests := []struct {
			name         string
			in           domain.ListObjectsInput
			wantKeys     []string
			wantPrefixes []string
		}{
			{name: "inside a/", in: domain.ListObjectsInput{StartAfter: "a/b", Delimiter: "/"},
				wantKeys: []string{"b", "x-1-a", "x-1-b", "x-2"}, wantPrefixes: []string{"a/"}},
			{name: "inside a nested prefix", in: domain.ListObjectsInput{StartAfter: "a/d/", Delimiter: "/"},
				wantKeys: []string{"b", "x-1-a", "x-1-b", "x-2"}, wantPrefixes: []string{"a/"}},
			{name: "after the last key of a/", in: domain.ListObjectsInput{StartAfter: "a/d/e", Delimiter: "/"},
				wantKeys: []string{"b", "x-1-a", "x-1-b", "x-2"}},
			{name: "the prefix itself", in: domain.ListObjectsInput{StartAfter: "a/", Delimiter: "/"},
				wantKeys: []string{"b", "x-1-a", "x-1-b", "x-2"}},
			{name: "v1 marker with a prefix", in: domain.ListObjectsInput{Prefix: "a/", Marker: "a/b", Delimiter: "/"},
				wantKeys: []string{"a/c"}, wantPrefixes: []string{"a/d/"}},
			{name: "other delimiter", in: domain.ListObjectsInput{StartAfter: "x-1-a", Delimiter: "-"},
				wantPrefixes: []string{"x-"}},
			{name: "other delimiter with a prefix", in: domain.ListObjectsInput{Prefix: "x-", StartAfter: "x-1-a", Delimiter: "-"},
				wantKeys: []string{"x-2"}, wantPrefixes: []string{"x-1-"}},
			{name: "other delimiter, marker is the prefix", in: domain.ListObjectsInput{Prefix: "x-", StartAfter: "x-1-", Delimiter: "-"},
				wantKeys: []string{"x-2"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				in := tt.in
				in.Bucket = "bkt"
				res, err := fx.svc.ListObjects(ctx, in)
				require.NoError(t, err)
				assertStrings(t, tt.wantKeys, keysOf(res))
				assertStrings(t, tt.wantPrefixes, res.CommonPrefixes)
				in.Marker, in.StartAfter = cmpOr(in.Marker, in.StartAfter), ""
				versions, err := fx.svc.ListObjectVersions(ctx, in)
				require.NoError(t, err)
				assertStrings(t, tt.wantPrefixes, versions.CommonPrefixes, "versions")
			})
		}
	})
}

// assertStrings compares two lists, nil and empty being equal.
func assertStrings(t *testing.T, want, got []string, msgAndArgs ...any) {
	t.Helper()
	if len(want) == 0 {
		assert.Empty(t, got, msgAndArgs...)
		return
	}
	assert.Equal(t, want, got, msgAndArgs...)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Regression: with a prefix ending in "/", the directory object of the
// same name (stored in the parent) contributed only its latest version to
// ListObjectVersions, and resuming within its versions lost the rest.
func TestListObjectVersionsDirectoryObject(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "ver", object.MakeBucketOptions{}))
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})
		var dirVersions []string
		for i := 0; i < 3; i++ {
			dirVersions = append(dirVersions, put(t, fx.svc, "ver", "dir/", nil).VersionID)
		}
		put(t, fx.svc, "ver", "dir/x", []byte("x"))
		put(t, fx.svc, "ver", "nested/dir/", nil)

		all, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver"})
		require.NoError(t, err)
		require.Len(t, all.Objects, 5)
		for _, prefix := range []string{"dir/", "di"} {
			res, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver", Prefix: prefix})
			require.NoError(t, err)
			assert.Len(t, res.Objects, 4, "prefix %q: the three versions of dir/ and dir/x", prefix)
		}
		res, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver", Prefix: "nested/dir/"})
		require.NoError(t, err)
		require.Len(t, res.Objects, 1)
		assert.Equal(t, "nested/dir/", res.Objects[0].Key)

		// Paging one version at a time visits each version once.
		var got []string
		in := domain.ListObjectsInput{Bucket: "ver", Prefix: "dir/", MaxKeys: 1}
		for pages := 0; pages < 10; pages++ {
			page, err := fx.svc.ListObjectVersions(ctx, in)
			require.NoError(t, err)
			for _, o := range page.Objects {
				got = append(got, o.Key+"@"+o.VersionID)
			}
			if !page.IsTruncated {
				break
			}
			in.Marker, in.VersionIDMarker = page.NextMarker, page.NextVersionIDMarker
		}
		require.Len(t, got, 4)
		for i, v := range []string{dirVersions[2], dirVersions[1], dirVersions[0]} {
			assert.Equal(t, "dir/@"+v, got[i], "latest first")
		}
		assert.True(t, strings.HasPrefix(got[3], "dir/x@"))

		// ListObjects still reports the latest version only.
		objs, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "ver", Prefix: "dir/"})
		require.NoError(t, err)
		assert.Equal(t, []string{"dir/", "dir/x"}, keysOf(objs))
	})
}
