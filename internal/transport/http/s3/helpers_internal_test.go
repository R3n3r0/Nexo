package s3

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func s3Code(err error) string {
	var s3err *domain.S3Error
	if errors.As(err, &s3err) {
		return s3err.Code
	}
	return ""
}

func TestS3URLEncode(t *testing.T) {
	tests := map[string]string{
		"":                "",
		"plain/key.txt":   "plain/key.txt",
		"a b":             "a%20b",
		"a+b":             "a%2Bb",
		"x*y-z_w.v":       "x*y-z_w.v",
		"~tilde":          "%7Etilde",
		"ü/é":             "%C3%BC/%C3%A9",
		"q?=&#%":          "q%3F%3D%26%23%25",
		"tab\tnewline\n":  "tab%09newline%0A",
		"100%/<x>\"quote": "100%25/%3Cx%3E%22quote",
	}
	for in, want := range tests {
		got := s3URLEncode(in)
		assert.Equal(t, want, got, in)
		decoded, err := url.QueryUnescape(got)
		require.NoError(t, err)
		assert.Equal(t, in, decoded, "clients decode with a query unescape")
		decoded, err = url.PathUnescape(got)
		require.NoError(t, err)
		assert.Equal(t, in, decoded, "or with a path unescape")
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"Example.COM":         "example.com",
		"example.com:9000":    "example.com",
		".s3.example.com.":    "s3.example.com",
		"[::1]:9000":          "::1",
		"  spaced.example  ":  "spaced.example",
		"":                    "",
		"::1":                 "::1",
		"bucket.example:abcd": "bucket.example",
	}
	for in, want := range tests {
		assert.Equal(t, want, normalizeHost(in), in)
	}
}

func TestStripAWSChunked(t *testing.T) {
	tests := map[string]string{
		"":                       "",
		"aws-chunked":            "",
		"AWS-CHUNKED, gzip":      "gzip",
		"gzip,aws-chunked,br":    "gzip, br",
		"identity":               "identity",
		" , aws-chunked , gzip ": "gzip",
		"deflate, gzip":          "deflate, gzip",
		"deflate,gzip":           "deflate,gzip",
	}
	for in, want := range tests {
		assert.Equal(t, want, stripAWSChunked(in), in)
	}
}

func TestETagHelpers(t *testing.T) {
	assert.Equal(t, "", quoteETag(""))
	assert.Equal(t, `"e"`, quoteETag("e"))
	assert.Equal(t, `"e"`, quoteETag(`"e"`))
	assert.Equal(t, "e", trimETag(` "e" `))
	assert.Equal(t, "e", trimETag(`W/"e"`))
	assert.Equal(t, "e-3", trimETag("e-3"))
	assert.Equal(t, "null", versionIDOrNull(""))
	assert.Equal(t, "v", versionIDOrNull("v"))
}

func TestMatchRank(t *testing.T) {
	q := url.Values{"uploads": {""}, "list-type": {"2"}, "X-Amz-Signature": {"s"}}
	tests := []struct {
		op   Operation
		want int
	}{
		{Operation{}, rankDefault},
		{Operation{Query: "uploads"}, rankSubResource},
		{Operation{Query: "missing"}, rankNone},
		{Operation{Query: "list-type", QueryValue: "2"}, rankQueryValue},
		{Operation{Query: "list-type", QueryValue: "1"}, rankNone},
		{Operation{Query: "X-Amz-Signature"}, rankAuthParam},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, matchRank(tc.op, q), "%+v", tc.op)
	}
	assert.Equal(t, rankNone, matchRank(Operation{Query: "list-type", QueryValue: "2"}, url.Values{"list-type": {}}))
	assert.Nil(t, match(nil, q))
}

func TestBodyReadError(t *testing.T) {
	signed := domain.NewS3Error(domain.ErrSignatureMismatch, "SignatureDoesNotMatch", "chunk")
	assert.Same(t, signed, bodyReadError(signed))
	assert.Equal(t, "EntityTooLarge", s3Code(bodyReadError(&http.MaxBytesError{Limit: 1})))
	assert.Equal(t, "IncompleteBody", s3Code(bodyReadError(io.ErrUnexpectedEOF)))
	err := bodyReadError(errors.New("connection reset"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// errReader fails every read.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/", nil)
	req.Body = io.NopCloser(errReader{io.ErrUnexpectedEOF})
	_, err := readBody(req, 10)
	assert.Equal(t, "IncompleteBody", s3Code(err))

	req = httptest.NewRequest(http.MethodPut, "/", strings.NewReader("12345"))
	_, err = readBody(req, 4)
	assert.ErrorIs(t, err, errBodyTooLarge)

	req = httptest.NewRequest(http.MethodPut, "/", nil)
	body, err := readBody(req, 4)
	require.NoError(t, err)
	assert.Empty(t, body)

	req = httptest.NewRequest(http.MethodPut, "/", strings.NewReader("<x/>"))
	req.Body = io.NopCloser(errReader{errors.New("boom")})
	err = decodeXMLBody(req, httpx.New(), 10, &struct{}{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestContentLength(t *testing.T) {
	newReq := func(cl int64, header string, te []string, proto int) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "/b/k", strings.NewReader("x"))
		req.ContentLength = cl
		req.TransferEncoding = te
		req.ProtoMajor = proto
		if header != "" {
			req.Header.Set("Content-Length", header)
		}
		return req
	}
	tests := []struct {
		name string
		req  *http.Request
		want int64
		code string
	}{
		{"declared", newReq(5, "5", nil, 1), 5, ""},
		{"empty declared", newReq(0, "0", nil, 1), 0, ""},
		{"empty HTTP/2", newReq(0, "", nil, 2), 0, ""},
		{"no length", newReq(0, "", nil, 1), 0, "MissingContentLength"},
		{"chunked", newReq(-1, "", []string{"chunked"}, 1), -1, ""},
		{"unknown HTTP/2", newReq(-1, "", nil, 2), -1, ""},
		{"unknown HTTP/1", newReq(-1, "", []string{"gzip"}, 1), 0, "MissingContentLength"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := contentLength(tc.req)
			if tc.code != "" {
				assert.Equal(t, tc.code, s3Code(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, n)
		})
	}
	req := newReq(7, "40", nil, 1)
	req.Header.Set(httpx.HeaderAmzDecodedLength, "7")
	n, err := contentLength(req)
	require.NoError(t, err)
	assert.Equal(t, int64(7), n)
	req.Header.Set(httpx.HeaderAmzDecodedLength, "-1")
	_, err = contentLength(req)
	assert.Equal(t, "InvalidRequest", s3Code(err))
}

func TestChecksumFromHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    domain.Checksum
		code    string
	}{
		{name: "none", headers: map[string]string{}},
		{name: "header", headers: map[string]string{"x-amz-checksum-sha1": "s"}, want: domain.Checksum{Algorithm: domain.ChecksumSHA1, Value: "s"}},
		{name: "crc64nvme header", headers: map[string]string{"x-amz-checksum-crc64nvme": "n", httpx.HeaderAmzSDKChecksumAlgorithm: "CRC64NVME"},
			want: domain.Checksum{Algorithm: domain.ChecksumCRC64NVME, Value: "n"}},
		{name: "trailer announces its algorithm only", headers: map[string]string{httpx.HeaderAmzTrailer: "x-amz-checksum-crc32c", httpx.HeaderAmzSDKChecksumAlgorithm: "crc32c"},
			want: domain.Checksum{Algorithm: domain.ChecksumCRC32C}},
		{name: "trailer without sdk algorithm", headers: map[string]string{httpx.HeaderAmzTrailer: "x-amz-checksum-sha256"},
			want: domain.Checksum{Algorithm: domain.ChecksumSHA256}},
		{name: "sdk algorithm alone is not a checksum", headers: map[string]string{httpx.HeaderAmzSDKChecksumAlgorithm: "crc32"}},
		{name: "sdk algorithm disagreeing", headers: map[string]string{"x-amz-checksum-sha1": "s", httpx.HeaderAmzSDKChecksumAlgorithm: "crc32"}, code: "InvalidRequest"},
		{name: "two headers", headers: map[string]string{"x-amz-checksum-sha1": "s", "x-amz-checksum-crc32": "c"}, code: "InvalidRequest"},
		{name: "unsupported trailer", headers: map[string]string{httpx.HeaderAmzTrailer: "x-amz-checksum-md5"}, code: "InvalidRequest"},
		{name: "unsupported header", headers: map[string]string{"x-amz-checksum-md5": "1B2M2Y8AsgTpgAmY7PhCfg=="}, code: "InvalidRequest"},
		{name: "unsupported sdk algorithm", headers: map[string]string{httpx.HeaderAmzSDKChecksumAlgorithm: "SHA512"}, code: "InvalidRequest"},
		{name: "checksum parameters are not checksums", headers: map[string]string{"x-amz-checksum-type": "COMPOSITE", "x-amz-checksum-mode": "ENABLED", "x-amz-checksum-algorithm": "CRC32"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			c, err := checksumFromHeaders(h)
			if tt.code != "" {
				assert.Equal(t, tt.code, s3Code(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, c)
		})
	}
}

func TestUnsignedPayloadSHA256(t *testing.T) {
	sha := strings.Repeat("A", 64)
	tests := []struct {
		name, target, value, auth string
		want                      string
	}{
		{"anonymous", "/b/k", sha, "", strings.ToLower(sha)},
		{"not hex", "/b/k", strings.Repeat("z", 64), "", ""},
		{"short", "/b/k", "abc", "", ""},
		{"marker", "/b/k", "UNSIGNED-PAYLOAD", "", ""},
		{"signed", "/b/k", sha, "AWS4-HMAC-SHA256 Credential=x", ""},
		{"presigned", "/b/k?X-Amz-Signature=s", sha, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, tc.target, nil)
			req.Header.Set(httpx.HeaderAmzContentSHA256, tc.value)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			assert.Equal(t, tc.want, unsignedPayloadSHA256(req))
		})
	}
}

func TestRequestScheme(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.Equal(t, "http", requestScheme(req))
	req.Header.Set("X-Forwarded-Proto", "HTTPS")
	assert.Equal(t, "https", requestScheme(req))
	req.Header.Set("X-Forwarded-Proto", "ftp")
	assert.Equal(t, "http", requestScheme(req))
	req = httptest.NewRequest(http.MethodGet, "https://s3.local/", nil)
	assert.Equal(t, "https", requestScheme(req))
}

func TestOwnerOf(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.Equal(t, defaultOwner(), ownerOf(req))
	anon := req.WithContext(middleware.WithIdentity(req.Context(), domain.Anonymous()))
	assert.Equal(t, defaultOwner(), ownerOf(anon))
	nameless := req.WithContext(middleware.WithIdentity(req.Context(), &domain.Identity{}))
	assert.Equal(t, defaultOwner(), ownerOf(nameless))
	alice := req.WithContext(middleware.WithIdentity(req.Context(), &domain.Identity{AccessKey: "alice"}))
	assert.Equal(t, Owner{ID: "alice", DisplayName: "alice"}, ownerOf(alice))
}

func TestCheckerRejectsUnknownActions(t *testing.T) {
	a := &authorizer{}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(middleware.WithIdentity(req.Context(), &domain.Identity{IsRoot: true}))
	a.clock = clock.NewFake(time.Now())
	check := a.prepare(req, "")
	assert.ErrorIs(t, check("", "k"), domain.ErrAccessDenied, "an operation without action is denied without asking the engine")
}

func TestParseAmzDate(t *testing.T) {
	assert.True(t, parseAmzDate("bad").IsZero())
	assert.Equal(t, 2026, parseAmzDate("20261008T120000Z").Year())
}

func TestDirective(t *testing.T) {
	for in, want := range map[string]string{"": directiveCopy, "copy": directiveCopy, " REPLACE ": directiveReplace} {
		h := http.Header{}
		h.Set(httpx.HeaderAmzMetadataDirective, in)
		got, err := directive(h, httpx.HeaderAmzMetadataDirective)
		require.NoError(t, err)
		assert.Equal(t, want, got, fmt.Sprintf("%q", in))
	}
}
