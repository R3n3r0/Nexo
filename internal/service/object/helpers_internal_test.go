package object

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// The tests in this file cover unexported helpers; everything reachable
// through the Service contract is tested from the external package.

func TestHelpers(t *testing.T) {
	assert.True(t, etagMatches("*", "x"))
	assert.True(t, etagMatches(`"abc", "def"`, "def"))
	assert.True(t, etagMatches(`W/"abc"`, "abc"))
	assert.False(t, etagMatches(`"abc"`, "abd"))
	assert.Equal(t, "a=1&b=two+words", encodeTags(map[string]string{"b": "two words", "a": "1"}))
	assert.Equal(t, map[string]string{"a": "1"}, decodeTags("a=1"))
	assert.Nil(t, decodeTags("%zz"))
	base, filter := splitPrefix("a/b/c")
	assert.Equal(t, "a/b", base)
	assert.Equal(t, "a/b/c", filter)
	base, filter = splitPrefix("abc")
	assert.Empty(t, base)
	assert.Equal(t, "abc", filter)
	assert.Equal(t, 1000, clampMaxKeys(0))
	assert.Equal(t, 1000, clampMaxKeys(5000))
	assert.Equal(t, 7, clampMaxKeys(7))
	assert.True(t, validUploadID("abc-DEF_123"))
	assert.False(t, validUploadID("a/b"))
	assert.False(t, validUploadID(""))

	errs := []error{nil, nil, domain.ErrObjectNotFound}
	assert.NoError(t, reduceErrs(errs, 2, domain.ErrWriteQuorum))
	assert.ErrorIs(t, reduceErrs(errs, 3, domain.ErrWriteQuorum), domain.ErrWriteQuorum)
	errs = []error{fmt.Errorf("a: %w", domain.ErrObjectNotFound), fmt.Errorf("b: %w", domain.ErrObjectNotFound), nil}
	assert.ErrorIs(t, reduceErrs(errs, 2, domain.ErrReadQuorum), domain.ErrObjectNotFound)
	errs = []error{domain.ErrObjectNotFound, domain.ErrFaultyDisk, nil}
	assert.ErrorIs(t, reduceErrs(errs, 2, domain.ErrReadQuorum), domain.ErrReadQuorum)
	assert.Error(t, reduceErrs(errs, 2, nil))

	info := objectInfo("b", "k", storage.FileInfo{Size: 3, Metadata: map[string]string{
		metaContentType: "text/plain", metaETag: "e-2", metaExpires: "Wed, 21 Oct 2026 07:28:00 GMT",
		metaUserPrefix + "a": "1", metaChecksumPrefix + "crc32": "AAAA", metaTagging: "x=y", metaStorageClass: "GLACIER",
	}, Parts: []storage.PartInfo{{Number: 1}, {Number: 2}}})
	assert.Equal(t, "text/plain", info.ContentType)
	assert.Equal(t, 2026, info.Expires.Year())
	assert.Equal(t, "1", info.UserMetadata["a"])
	assert.Equal(t, domain.ChecksumCRC32, info.Checksum.Algorithm)
	assert.Equal(t, map[string]string{"x": "y"}, info.Tags)
	assert.Equal(t, "GLACIER", info.StorageClass)
	assert.Equal(t, 2, info.Parts)
	assert.True(t, info.Inlined)
}

func TestUploadIDCarriesItsTime(t *testing.T) {
	l := &layer{deps: Deps{IDs: uuidV7{}}}
	at := time.Date(2026, 10, 8, 12, 0, 0, 123456789, time.UTC)
	id := l.newUploadID(at)
	assert.True(t, validUploadID(id), id)
	assert.Len(t, id, 32, "base64url of 24 bytes")
	got, ok := uploadIDTime(id)
	require.True(t, ok)
	assert.True(t, at.Equal(got))

	// A generator that does not produce UUIDs still yields valid ids.
	l.deps.IDs = fixedID("not-a-uuid")
	id2 := l.newUploadID(at)
	assert.True(t, validUploadID(id2))
	assert.NotEqual(t, id, id2)

	for _, bad := range []string{"", "!!", "AAAA", strings.Repeat("A", 40)} {
		_, ok := uploadIDTime(bad)
		assert.False(t, ok, bad)
	}
}

type fixedID string

func (f fixedID) NewID() string { return string(f) }

func TestHTTPMetaBuild(t *testing.T) {
	tests := []struct {
		name string
		meta httpMeta
		tags map[string]string
		sc   string
		want map[string]string
	}{
		{
			name: "defaults",
			want: map[string]string{metaContentType: defaultContentType},
		},
		{
			name: "user keys are lowercased and stripped of the prefix",
			meta: httpMeta{contentType: "text/plain", user: map[string]string{"Owner": "a", "X-Amz-Meta-Team": "b", " ": "skip"}},
			want: map[string]string{metaContentType: "text/plain", "x-amz-meta-owner": "a", "x-amz-meta-team": "b"},
		},
		{
			name: "legacy standard keys fill the empty fields",
			meta: httpMeta{cacheControl: "no-cache", user: map[string]string{"Cache-Control": "max-age=1", "content-language": "it"}},
			want: map[string]string{metaContentType: defaultContentType, metaCacheControl: "no-cache", metaContentLanguage: "it"},
		},
		{
			name: "standard fields, tags and storage class",
			meta: httpMeta{contentEncoding: "gzip", contentDisposition: "inline", expires: "Wed, 21 Oct 2026 07:28:00 GMT"},
			tags: map[string]string{"a": "1"},
			sc:   "REDUCED_REDUNDANCY",
			want: map[string]string{
				metaContentType: defaultContentType, metaContentEncoding: "gzip", metaContentDisposition: "inline",
				metaExpires: "Wed, 21 Oct 2026 07:28:00 GMT", metaTagging: "a=1", metaStorageClass: "REDUCED_REDUNDANCY",
			},
		},
		{
			name: "standard storage class is implicit",
			sc:   domain.StorageClassStandard,
			want: map[string]string{metaContentType: defaultContentType},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.meta.build(tt.tags, tt.sc))
		})
	}
}

func TestCopyableMeta(t *testing.T) {
	src := map[string]string{
		metaContentType: "a/b", metaCacheControl: "c", metaExpires: "e", metaUserPrefix + "x": "y",
		metaETag: "etag", metaTagging: "t=1", metaStorageClass: "GLACIER", metaChecksumPrefix + "crc32": "AAAA",
	}
	assert.Equal(t, map[string]string{metaContentType: "a/b", metaCacheControl: "c", metaExpires: "e", metaUserPrefix + "x": "y"}, copyableMeta(src))
	assert.Equal(t, map[string]string{metaContentType: defaultContentType}, copyableMeta(nil))
}

func TestParseHTTPTimeAndETags(t *testing.T) {
	want := time.Date(1994, 11, 6, 8, 49, 37, 0, time.UTC)
	for _, v := range []string{"Sun, 06 Nov 1994 08:49:37 GMT", "Sunday, 06-Nov-94 08:49:37 GMT", "Sun Nov  6 08:49:37 1994"} {
		got, ok := parseHTTPTime(v)
		require.True(t, ok, v)
		assert.True(t, want.Equal(got), v)
	}
	_, ok := parseHTTPTime("yesterday")
	assert.False(t, ok)
	assert.Equal(t, "Sun, 06 Nov 1994 08:49:37 GMT", formatHTTPTime(want))

	assert.True(t, isMultipartETag("abc-3"))
	assert.False(t, isMultipartETag("abc"))
	assert.False(t, isMultipartETag("abc-x"))
}

func TestConditionPrecedence(t *testing.T) {
	mod := time.Date(2026, 10, 8, 12, 0, 0, 500, time.UTC)
	info := domain.ObjectInfo{ETag: "abc", ModTime: mod}
	before, after := mod.Add(-time.Hour), mod.Add(time.Hour)
	tests := []struct {
		name      string
		c         domain.Conditions
		read      error
		source    error
		writeHave error
		writeNone error
	}{
		{name: "none"},
		{name: "if-match ok", c: domain.Conditions{IfMatch: `"abc"`}, writeNone: domain.ErrObjectNotFound},
		{name: "if-match list and weak", c: domain.Conditions{IfMatch: `"x", W/"abc"`}, writeNone: domain.ErrObjectNotFound},
		{name: "if-match fails", c: domain.Conditions{IfMatch: "x"}, read: domain.ErrPreconditionFailed, source: domain.ErrPreconditionFailed, writeHave: domain.ErrPreconditionFailed, writeNone: domain.ErrObjectNotFound},
		{name: "if-match star", c: domain.Conditions{IfMatch: "*"}, writeNone: domain.ErrObjectNotFound},
		{name: "if-match wins over if-unmodified-since", c: domain.Conditions{IfMatch: "abc", IfUnmodifiedSince: before}, writeNone: domain.ErrObjectNotFound},
		{name: "if-unmodified-since fails", c: domain.Conditions{IfUnmodifiedSince: before}, read: domain.ErrPreconditionFailed, source: domain.ErrPreconditionFailed},
		{name: "if-unmodified-since same second", c: domain.Conditions{IfUnmodifiedSince: mod.Truncate(time.Second)}},
		{name: "if-none-match matches", c: domain.Conditions{IfNoneMatch: "abc"}, read: domain.ErrNotModified, source: domain.ErrPreconditionFailed, writeHave: domain.ErrPreconditionFailed},
		{name: "if-none-match star", c: domain.Conditions{IfNoneMatch: "*"}, read: domain.ErrNotModified, source: domain.ErrPreconditionFailed, writeHave: domain.ErrPreconditionFailed},
		{name: "if-none-match wins over if-modified-since", c: domain.Conditions{IfNoneMatch: "other", IfModifiedSince: after}},
		{name: "if-modified-since not modified", c: domain.Conditions{IfModifiedSince: after}, read: domain.ErrNotModified, source: domain.ErrPreconditionFailed},
		{name: "if-modified-since same second", c: domain.Conditions{IfModifiedSince: mod.Truncate(time.Second)}, read: domain.ErrNotModified, source: domain.ErrPreconditionFailed},
		{name: "if-modified-since modified", c: domain.Conditions{IfModifiedSince: before}},
		{name: "precondition before not modified", c: domain.Conditions{IfMatch: "x", IfNoneMatch: "abc"}, read: domain.ErrPreconditionFailed, source: domain.ErrPreconditionFailed, writeHave: domain.ErrPreconditionFailed, writeNone: domain.ErrObjectNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(err, want error) {
				t.Helper()
				if want == nil {
					assert.NoError(t, err)
					return
				}
				assert.ErrorIs(t, err, want)
			}
			check(checkReadConditions(info, tt.c), tt.read)
			check(checkSourceConditions(info, tt.c), tt.source)
			check(checkWriteConditions(true, info, tt.c), tt.writeHave)
			check(checkWriteConditions(false, domain.ObjectInfo{}, tt.c), tt.writeNone)
		})
	}
}

func TestValidateTags(t *testing.T) {
	many := map[string]string{}
	for i := 0; i < 11; i++ {
		many[fmt.Sprint("k", i)] = "v"
	}
	tests := []struct {
		name string
		tags map[string]string
		ok   bool
	}{
		{name: "empty", tags: nil, ok: true},
		{name: "regular", tags: map[string]string{"env": "prod", "team": "a b"}, ok: true},
		{name: "empty value", tags: map[string]string{"k": ""}, ok: true},
		{name: "unicode key counted in characters", tags: map[string]string{strings.Repeat("é", 128): "v"}, ok: true},
		{name: "too many", tags: many},
		{name: "empty key", tags: map[string]string{"": "v"}},
		{name: "key too long", tags: map[string]string{strings.Repeat("k", 129): "v"}},
		{name: "value too long", tags: map[string]string{"k": strings.Repeat("v", 257)}},
		{name: "reserved prefix", tags: map[string]string{"AWS:x": "v"}},
		{name: "invalid utf-8", tags: map[string]string{"k": "\xff"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTags(tt.tags)
			if tt.ok {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
}

func TestAncestorLocks(t *testing.T) {
	tests := []struct {
		key  string
		want []lockRequest
	}{
		{key: "k"},
		{key: "dir/"},
		{key: "a/b", want: []lockRequest{{bucket: "bkt", key: "a"}}},
		{key: "a/b/c", want: []lockRequest{{bucket: "bkt", key: "a/b"}, {bucket: "bkt", key: "a"}}},
		{key: "a/b/", want: []lockRequest{{bucket: "bkt", key: "a"}}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ancestorLocks("bkt", tt.key), tt.key)
	}
}
