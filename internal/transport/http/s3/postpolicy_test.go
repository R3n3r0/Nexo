package s3_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// postForm builds a multipart form: the fields (sorted), then the file.
func postForm(fields map[string]string, fileName, content string) (string, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		_ = mw.WriteField(k, fields[k])
	}
	fw, _ := mw.CreateFormFile("file", fileName)
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	return buf.String(), mw.FormDataContentType()
}

func (f *fixture) post(target string, fields map[string]string, fileName, content string) *http.Request {
	body, ct := postForm(fields, fileName, content)
	req := request(http.MethodPost, target, body, "Content-Type", ct)
	req.Host = "s3.local"
	return req
}

func signedFields(extra map[string]string) map[string]string {
	fields := map[string]string{"key": "uploads/${filename}", "policy": "cG9saWN5", "x-amz-signature": "sig", "x-amz-credential": "alice/20261008/us-east-1/s3/aws4_request"}
	for k, v := range extra {
		fields[k] = v
	}
	return fields
}

func TestPostObject(t *testing.T) {
	f := newFixture(t, withoutIdentity())
	alice := &domain.Identity{AccessKey: "alice"}
	postPolicy := &auth.PostPolicy{Conditions: []auth.PostCondition{{Operator: auth.ConditionLengthRange, Min: 1, Max: 100}}}
	fields := signedFields(map[string]string{
		"success_action_status": "201", "x-amz-meta-Tag": "a", "Content-Type": "image/png", "Cache-Control": "max-age=60",
		"tagging": `<Tagging><TagSet><Tag><Key>t</Key><Value>1</Value></Tag></TagSet></Tagging>`, "x-amz-storage-class": "STANDARD",
		"acl": "private", "x-ignore-me": "1",
	})
	f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.MatchedBy(func(form auth.PostForm) bool {
		return form.Bucket == "b" && form.FileName == "photo.png" && form.Fields["key"] == "uploads/${filename}" &&
			form.Fields["content-type"] == "image/png" && form.Fields["x-amz-meta-tag"] == "a" && form.Fields["file"] == ""
	})).Return(alice, postPolicy, nil).Once()
	var got object.PutObjectInput
	var data []byte
	f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
		got = in
		data, _ = io.ReadAll(in.Reader)
		return domain.ObjectInfo{ETag: "pe", Size: 9, VersionID: "v1"}, nil
	}).Once()
	rec := f.serve(f.post("/b", fields, `C:\Users\me\photo.png`, "png-bytes"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var res s3.PostResponse
	decodeXML(t, rec, &res)
	assert.Equal(t, s3.PostResponse{XMLName: res.XMLName, Location: "http://s3.local/b/uploads/photo.png", Bucket: "b", Key: "uploads/photo.png", ETag: `"pe"`}, res)
	assert.Equal(t, `"pe"`, rec.Header().Get("ETag"))
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "http://s3.local/b/uploads/photo.png", rec.Header().Get("Location"))

	assert.Equal(t, "png-bytes", string(data))
	assert.Equal(t, object.PutObjectInput{
		Bucket: "b", Key: "uploads/photo.png", Reader: got.Reader, Size: -1,
		ContentType: "image/png", CacheControl: "max-age=60", UserMetadata: map[string]string{"tag": "a"},
		Tags: map[string]string{"t": "1"}, StorageClass: "STANDARD", LengthRange: &object.LengthRange{Min: 1, Max: 100},
	}, got)
}

func TestPostObjectStatuses(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice"}
	tests := []struct {
		name     string
		extra    map[string]string
		status   int
		location string
	}{
		{"default", nil, http.StatusNoContent, "http://s3.local/b/k"},
		{"200", map[string]string{"success_action_status": "200"}, http.StatusOK, "http://s3.local/b/k"},
		{"unknown status", map[string]string{"success_action_status": "302"}, http.StatusNoContent, "http://s3.local/b/k"},
		{"redirect", map[string]string{"success_action_redirect": "https://app.example.com/done?x=1"}, http.StatusSeeOther,
			"https://app.example.com/done?bucket=b&etag=%22pe%22&key=k&x=1"},
		{"legacy redirect", map[string]string{"redirect": "http://app.example.com/"}, http.StatusSeeOther,
			"http://app.example.com/?bucket=b&etag=%22pe%22&key=k"},
		{"invalid redirect is ignored", map[string]string{"success_action_redirect": "javascript:alert(1)", "success_action_status": "200"}, http.StatusOK,
			"http://s3.local/b/k"},
		{"relative redirect is ignored", map[string]string{"success_action_redirect": "/done"}, http.StatusNoContent, "http://s3.local/b/k"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, withoutIdentity())
			f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(alice, &auth.PostPolicy{}, nil).Once()
			f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.LengthRange == nil })).
				Return(domain.ObjectInfo{ETag: "pe"}, nil).Once()
			fields := signedFields(tc.extra)
			fields["key"] = "k"
			rec := f.serve(f.post("/b", fields, "x.txt", "x"))
			assert.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, tc.location, rec.Header().Get("Location"))
			if tc.status != http.StatusSeeOther {
				return
			}
			u, err := url.Parse(rec.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, `"pe"`, u.Query().Get("etag"))
		})
	}
}

func TestPostObjectAnonymous(t *testing.T) {
	f := newFixture(t, withoutIdentity(), withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Identity.IsAnonymous && a.Object == "in/a.txt" && a.Action == policy.ActionPutObject
	})).Return(policy.Decision{Allowed: true}, nil).Once()
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.Key == "in/a.txt" })).
		Return(domain.ObjectInfo{ETag: "e"}, nil).Once()
	rec := f.serve(f.post("/drop", map[string]string{"key": "in/${filename}"}, "a.txt", "hello"))
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Without a bucket policy allowing it, the anonymous upload is denied.
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: false}, nil).Once()
	requireError(t, f.serve(f.post("/drop", map[string]string{"key": "x"}, "a.txt", "hello")), http.StatusForbidden, "AccessDenied")
}

func TestPostObjectErrors(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice"}
	t.Run("verification failure", func(t *testing.T) {
		f := newFixture(t, withoutIdentity())
		f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).
			Return(nil, nil, domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "Invalid according to Policy: Policy expired.")).Once()
		rec := f.serve(f.post("/b", signedFields(nil), "a", "x"))
		requireError(t, rec, http.StatusForbidden, "AccessDenied")
		assert.Equal(t, "Invalid according to Policy: Policy expired.", s3Error(t, rec).Message)
	})
	t.Run("length range enforced by the object layer", func(t *testing.T) {
		f := newFixture(t, withoutIdentity())
		f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(alice, &auth.PostPolicy{}, nil).Once()
		f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).
			Return(domain.ObjectInfo{}, domain.NewS3Error(domain.ErrEntityTooLarge, "EntityTooLarge", "too large")).Once()
		requireError(t, f.serve(f.post("/b", signedFields(nil), "a", strings.Repeat("x", 101))), http.StatusBadRequest, "EntityTooLarge")
	})
	t.Run("verifier returns no identity", func(t *testing.T) {
		f := newFixture(t, withoutIdentity(), withStrictPolicy())
		f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(nil, nil, nil).Once()
		f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Identity.IsAnonymous })).
			Return(policy.Decision{Allowed: false}, nil).Once()
		requireError(t, f.serve(f.post("/b", signedFields(nil), "a", "x")), http.StatusForbidden, "AccessDenied")
	})

	f := newFixture(t, withoutIdentity())
	f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(alice, nil, nil).Maybe()
	tests := []struct {
		name   string
		fields map[string]string
		status int
		code   string
	}{
		{"missing key", signedFields(map[string]string{"key": ""}), 400, "InvalidArgument"},
		{"key too long", signedFields(map[string]string{"key": strings.Repeat("k", 1025)}), 400, "KeyTooLongError"},
		{"public ACL", signedFields(map[string]string{"acl": "public-read"}), 501, "NotImplemented"},
		{"SSE", signedFields(map[string]string{"x-amz-server-side-encryption": "ROT13"}), 400, "InvalidArgument"},
		{"bad storage class", signedFields(map[string]string{"x-amz-storage-class": "COLD"}), 400, "InvalidStorageClass"},
		{"bad tagging", signedFields(map[string]string{"tagging": "<Tagging>"}), 400, "MalformedXML"},
		{"duplicate tags", signedFields(map[string]string{"tagging": `<Tagging><TagSet><Tag><Key>a</Key></Tag><Tag><Key>a</Key></Tag></TagSet></Tagging>`}), 400, "InvalidTag"},
		{"metadata too large", signedFields(map[string]string{"x-amz-meta-big": strings.Repeat("m", 2049)}), 400, "MetadataTooLarge"},
		{"two checksums", signedFields(map[string]string{"x-amz-checksum-crc32": "a", "x-amz-checksum-sha1": "b"}), 400, "InvalidRequest"},
		{"field too large", signedFields(map[string]string{"x-amz-meta-x": strings.Repeat("v", 64<<10+1)}), 400, "MaxMessageLengthExceeded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.serve(f.post("/b", tc.fields, "a.txt", "x")), tc.status, tc.code)
		})
	}

	// Malformed requests.
	requireError(t, f.do(http.MethodPost, "/b", "plain", "Content-Type", "text/plain"), http.StatusBadRequest, "MalformedPOSTRequest")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("key", "k")
	_ = mw.Close()
	requireError(t, f.do(http.MethodPost, "/b", buf.String(), "Content-Type", mw.FormDataContentType()), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodPost, "/b", "--x\r\nbroken", "Content-Type", "multipart/form-data; boundary=x"), http.StatusBadRequest, "MalformedPOSTRequest")
	// Too many fields overall.
	big := map[string]string{}
	for i := 0; i < 20; i++ {
		big["x-amz-meta-f"+strings.Repeat("i", i)] = strings.Repeat("v", 60<<10)
	}
	requireError(t, f.serve(f.post("/b", big, "a", "x")), http.StatusBadRequest, "MaxMessageLengthExceeded")
}

func TestPostObjectIgnoresFieldsAfterTheFile(t *testing.T) {
	f := newFixture(t, withoutIdentity())
	f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.MatchedBy(func(form auth.PostForm) bool {
		_, late := form.Fields["late"]
		return !late
	})).Return(&domain.Identity{AccessKey: "alice"}, nil, nil).Once()
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		data, _ := io.ReadAll(in.Reader)
		return in.Key == "k" && string(data) == "content"
	})).Return(domain.ObjectInfo{ETag: "e"}, nil).Once()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("key", "k")
	_ = mw.WriteField("policy", "p")
	fw, _ := mw.CreateFormFile("file", "a.txt")
	_, _ = fw.Write([]byte("content"))
	_ = mw.WriteField("late", "ignored")
	_ = mw.Close()
	rec := f.do(http.MethodPost, "/b", buf.String(), "Content-Type", mw.FormDataContentType())
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}
