package s3_test

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// testKey is an SSE-C key with its base64 form and MD5.
type testKey struct {
	raw []byte
	b64 string
	md5 string
}

func newTestKey(fill byte) testKey {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = fill
	}
	sum := md5.Sum(raw)
	return testKey{raw: raw, b64: base64.StdEncoding.EncodeToString(raw), md5: base64.StdEncoding.EncodeToString(sum[:])}
}

// ssecHeaders are the SSE-C headers of key with the given prefix.
func ssecHeaders(prefix string, k testKey) []string {
	return []string{prefix + "algorithm", "AES256", prefix + "key", k.b64, prefix + "key-MD5", k.md5}
}

// secure marks a request as received over TLS.
func secure(req *http.Request) *http.Request {
	req.TLS = &tls.ConnectionState{}
	return req
}

func TestPutObjectEncryption(t *testing.T) {
	key := newTestKey(7)
	ctxDoc := base64.StdEncoding.EncodeToString([]byte(`{"app":"photos"}`))
	tests := []struct {
		name     string
		headers  []string
		insecure bool
		want     domain.SSERequest
		info     domain.EncryptionInfo
		wantResp map[string]string
		status   int
		code     string
	}{
		{
			name: "SSE-S3", headers: []string{"x-amz-server-side-encryption", "AES256"},
			want: domain.SSERequest{Type: domain.SSES3}, info: domain.EncryptionInfo{Type: domain.SSES3, KMSKeyID: "master"},
			wantResp: map[string]string{"x-amz-server-side-encryption": "AES256", "x-amz-server-side-encryption-aws-kms-key-id": ""},
		},
		{
			name: "SSE-KMS with an ARN and a context",
			headers: []string{
				"x-amz-server-side-encryption", "aws:kms",
				"x-amz-server-side-encryption-aws-kms-key-id", "arn:aws:kms:us-east-1:123456789012:key/tenant-a",
				"x-amz-server-side-encryption-context", ctxDoc,
			},
			want: domain.SSERequest{Type: domain.SSEKMS, KMSKeyID: "tenant-a", Context: map[string]string{"app": "photos"}},
			info: domain.EncryptionInfo{Type: domain.SSEKMS, KMSKeyID: "tenant-a", Context: map[string]string{"app": "photos"}},
			wantResp: map[string]string{
				"x-amz-server-side-encryption": "aws:kms", "x-amz-server-side-encryption-aws-kms-key-id": "tenant-a",
				"x-amz-server-side-encryption-context": ctxDoc,
			},
		},
		{
			name: "SSE-C", headers: ssecHeaders("x-amz-server-side-encryption-customer-", key),
			want: domain.SSERequest{Type: domain.SSEC, CustomerKey: domain.SSECustomerKey{Key: key.raw, KeyMD5: key.md5}},
			info: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: key.md5},
			wantResp: map[string]string{
				"x-amz-server-side-encryption-customer-algorithm": "AES256", "x-amz-server-side-encryption-customer-key-MD5": key.md5,
				"x-amz-server-side-encryption": "",
			},
		},
		{name: "SSE-C over plain HTTP", headers: ssecHeaders("x-amz-server-side-encryption-customer-", key), insecure: true, status: 400, code: "InvalidRequest"},
		{name: "SSE-C wrong MD5", headers: []string{
			"x-amz-server-side-encryption-customer-algorithm", "AES256", "x-amz-server-side-encryption-customer-key", key.b64,
			"x-amz-server-side-encryption-customer-key-MD5", newTestKey(8).md5,
		}, status: 400, code: "InvalidArgument"},
		{name: "SSE-C short key", headers: []string{
			"x-amz-server-side-encryption-customer-algorithm", "AES256",
			"x-amz-server-side-encryption-customer-key", base64.StdEncoding.EncodeToString([]byte("short")),
		}, status: 400, code: "InvalidArgument"},
		{name: "SSE-C without the key MD5", headers: []string{
			"x-amz-server-side-encryption-customer-algorithm", "AES256", "x-amz-server-side-encryption-customer-key", key.b64,
		}, status: 400, code: "InvalidArgument"},
		{name: "SSE-C without key", headers: []string{"x-amz-server-side-encryption-customer-algorithm", "AES256"}, status: 400, code: "InvalidArgument"},
		{name: "SSE-C other algorithm", headers: []string{
			"x-amz-server-side-encryption-customer-algorithm", "AES128", "x-amz-server-side-encryption-customer-key", key.b64,
		}, status: 400, code: "InvalidArgument"},
		{name: "SSE-C and SSE-S3", headers: append(ssecHeaders("x-amz-server-side-encryption-customer-", key), "x-amz-server-side-encryption", "AES256"), status: 400, code: "InvalidArgument"},
		{name: "KMS key without aws:kms", headers: []string{"x-amz-server-side-encryption-aws-kms-key-id", "k"}, status: 400, code: "InvalidArgument"},
		{name: "KMS key with AES256", headers: []string{"x-amz-server-side-encryption", "AES256", "x-amz-server-side-encryption-aws-kms-key-id", "k"}, status: 400, code: "InvalidArgument"},
		{name: "malformed context", headers: []string{"x-amz-server-side-encryption", "aws:kms", "x-amz-server-side-encryption-context", "not-json"}, status: 400, code: "InvalidArgument"},
		{name: "unknown algorithm", headers: []string{"x-amz-server-side-encryption", "ROT13"}, status: 400, code: "InvalidArgument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.status == 0 {
				f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
					return assert.Equal(t, tt.want, in.SSE)
				})).Return(domain.ObjectInfo{ETag: "e", Encryption: tt.info}, nil)
			}
			req := request(http.MethodPut, "/b/k", "data", tt.headers...)
			if !tt.insecure {
				req = secure(req)
			}
			rec := f.serve(req)
			if tt.status != 0 {
				requireError(t, rec, tt.status, tt.code)
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			for h, v := range tt.wantResp {
				assert.Equal(t, v, rec.Header().Get(h), h)
			}
		})
	}
}

// TestInsecureSSECOption: WithInsecureSSEC accepts SSE-C keys over plain
// HTTP (tests and development).
func TestInsecureSSECOption(t *testing.T) {
	f := newFixture(t)
	key := newTestKey(3)
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", mock.MatchedBy(func(o object.GetObjectOptions) bool {
		return assert.Equal(t, key.raw, o.SSECustomerKey.Key)
	})).Return(&object.Reader{ReadCloser: http.NoBody, Info: domain.ObjectInfo{Encryption: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: key.md5}}}, nil)
	api := s3.NewAPI(s3.APIDeps{Enc: f.enc},
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}, s3.WithInsecureSSEC(true)))
	r := chi.NewRouter()
	api.Register(r)
	req := request(http.MethodGet, "/b/k", "", ssecHeaders("x-amz-server-side-encryption-customer-", key)...)
	req = req.WithContext(middleware.WithIdentity(req.Context(), &domain.Identity{AccessKey: "root", IsRoot: true}))
	rec := serve(r, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, key.md5, rec.Header().Get("x-amz-server-side-encryption-customer-key-MD5"))
}

func TestGetHeadWithCustomerKey(t *testing.T) {
	key := newTestKey(5)
	f := newFixture(t)
	info := domain.ObjectInfo{Size: 4, ETag: "e", Encryption: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: key.md5}}
	matchKey := mock.MatchedBy(func(o object.GetObjectOptions) bool { return string(o.SSECustomerKey.Key) == string(key.raw) })
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", matchKey).
		Return(&object.Reader{ReadCloser: http.NoBody, Info: info}, nil)
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", matchKey).Return(info, nil)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := f.serve(secure(request(method, "/b/k", "", ssecHeaders("x-amz-server-side-encryption-customer-", key)...)))
		require.Equal(t, http.StatusOK, rec.Code, method)
		assert.Equal(t, "AES256", rec.Header().Get("x-amz-server-side-encryption-customer-algorithm"), method)
		assert.Equal(t, key.md5, rec.Header().Get("x-amz-server-side-encryption-customer-key-MD5"), method)
	}
	// The layer refuses a read without the key (InvalidRequest) or with
	// another one (AccessDenied).
	f.objects.EXPECT().GetObject(mock.Anything, "b", "other", mock.Anything).
		Return(nil, domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "The SSE-C key provided does not match"))
	requireError(t, f.serve(secure(request(http.MethodGet, "/b/other", "", ssecHeaders("x-amz-server-side-encryption-customer-", key)...))), 403, "AccessDenied")
}

// The SSE-S3/SSE-KMS headers are refused on a read (InvalidArgument), as
// S3 does: they describe the encryption of a write.
func TestReadWithWriteEncryptionHeaders(t *testing.T) {
	f := newFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, h := range [][]string{
			{"x-amz-server-side-encryption", "aws:kms"},
			{"x-amz-server-side-encryption", "AES256"},
			{"x-amz-server-side-encryption-aws-kms-key-id", "k"},
		} {
			rec := f.serve(secure(request(method, "/b/k", "", h...)))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s %v", method, h)
			if method == http.MethodGet {
				requireError(t, rec, http.StatusBadRequest, "InvalidArgument")
			}
		}
	}
}

func TestCopyObjectEncryption(t *testing.T) {
	src, dst := newTestKey(1), newTestKey(2)
	f := newFixture(t)
	f.objects.EXPECT().CopyObject(mock.Anything, mock.MatchedBy(func(in object.CopyObjectInput) bool {
		return assert.Equal(t, src.raw, in.SrcSSECustomerKey.Key) && assert.Equal(t, domain.SSEC, in.SSE.Type) &&
			assert.Equal(t, dst.raw, in.SSE.CustomerKey.Key)
	})).Return(domain.ObjectInfo{ETag: "e", Encryption: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: dst.md5}}, nil)
	headers := append([]string{"x-amz-copy-source", "/src/k"}, ssecHeaders("x-amz-copy-source-server-side-encryption-customer-", src)...)
	headers = append(headers, ssecHeaders("x-amz-server-side-encryption-customer-", dst)...)
	rec := f.serve(secure(request(http.MethodPut, "/dst/k", "", headers...)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, dst.md5, rec.Header().Get("x-amz-server-side-encryption-customer-key-MD5"))

	// The source key needs TLS as well.
	headers = append([]string{"x-amz-copy-source", "/src/k"}, ssecHeaders("x-amz-copy-source-server-side-encryption-customer-", src)...)
	requireError(t, f.do(http.MethodPut, "/dst/k", "", headers...), 400, "InvalidRequest")
}

func TestMultipartEncryption(t *testing.T) {
	key := newTestKey(9)
	f := newFixture(t)
	f.objects.EXPECT().NewMultipartUpload(mock.Anything, mock.MatchedBy(func(in object.NewMultipartInput) bool {
		return in.SSE.Type == domain.SSEKMS && in.SSE.KMSKeyID == "k1"
	})).Return(domain.MultipartUpload{UploadID: "u1", Encryption: domain.EncryptionInfo{Type: domain.SSEKMS, KMSKeyID: "k1"}}, nil)
	rec := f.serve(request(http.MethodPost, "/b/k?uploads", "", "x-amz-server-side-encryption", "aws:kms", "x-amz-server-side-encryption-aws-kms-key-id", "k1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "aws:kms", rec.Header().Get("x-amz-server-side-encryption"))
	assert.Equal(t, "k1", rec.Header().Get("x-amz-server-side-encryption-aws-kms-key-id"))

	f.objects.EXPECT().PutObjectPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool {
		return string(in.SSECustomerKey.Key) == string(key.raw)
	})).Return(domain.Part{ETag: "p", Encryption: domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: key.md5}}, nil)
	rec = f.serve(secure(request(http.MethodPut, "/b/k?uploadId=u1&partNumber=1", "data", ssecHeaders("x-amz-server-side-encryption-customer-", key)...)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, key.md5, rec.Header().Get("x-amz-server-side-encryption-customer-key-MD5"))
}

func TestPutBucketEncryption(t *testing.T) {
	const doc = `<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule>` +
		`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm>` +
		`<KMSMasterKeyID>arn:aws:kms:eu-west-1:1:key/tenant</KMSMasterKeyID></ApplyServerSideEncryptionByDefault>` +
		`<BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`
	tests := []struct {
		name   string
		body   string
		err    error
		status int
		code   string
	}{
		{name: "SSE-KMS", body: doc, status: 200},
		{name: "no rule", body: `<ServerSideEncryptionConfiguration/>`, status: 400, code: "MalformedXML"},
		{name: "malformed", body: `<ServerSideEncryptionConfiguration>`, status: 400, code: "MalformedXML"},
		{name: "KMS not configured", body: doc, err: domain.NewS3Error(domain.ErrKMSNotConfigured, "NotImplemented", "KMS is not configured"), status: 501, code: "NotImplemented"},
		{name: "unknown key", body: doc, err: domain.NewS3Error(domain.ErrKMSKeyNotFound, "KMS.NotFoundException", "Invalid keyId"), status: 400, code: "KMS.NotFoundException"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.code != "MalformedXML" {
				f.buckets.EXPECT().SetEncryption(mock.Anything, "b", bucket.EncryptionConfig{Algorithm: "aws:kms", KMSKeyID: "tenant"}).Return(tt.err)
			}
			rec := f.do(http.MethodPut, "/b?encryption", tt.body)
			if tt.status != http.StatusOK {
				requireError(t, rec, tt.status, tt.code)
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

func TestQuotaExceeded(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).
		Return(domain.ObjectInfo{}, domain.NewS3Error(domain.ErrQuotaExceeded, "XNexoQuotaExceeded", "Bucket quota exceeded"))
	requireError(t, f.do(http.MethodPut, "/b/k", "data"), http.StatusInsufficientStorage, "XNexoQuotaExceeded")
}

// TestEncryptionPolicyCondition: the s3:x-amz-server-side-encryption
// condition key carries the header of the request, so that a policy can
// require encrypted uploads.
func TestEncryptionPolicyCondition(t *testing.T) {
	f := newFixture(t, withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, args policy.Args) (policy.Decision, error) {
		v := args.Conditions["s3:x-amz-server-side-encryption"]
		return policy.Decision{Allowed: len(v) == 1 && v[0] == "AES256"}, nil
	})
	requireError(t, f.do(http.MethodPut, "/b/k", "data"), http.StatusForbidden, "AccessDenied")
	f.objects.EXPECT().PutObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{ETag: "e", Encryption: domain.EncryptionInfo{Type: domain.SSES3}}, nil)
	rec := f.do(http.MethodPut, "/b/k", "data", "x-amz-server-side-encryption", "AES256")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
