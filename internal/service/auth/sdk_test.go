package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// sdkKeys are object keys with characters that stress the canonical URI:
// spaces, reserved and unreserved punctuation, UTF-8, repeated and dot
// segments (S3 does not normalize paths).
func sdkKeys() []string {
	return []string{
		"plain.txt",
		"dir/My File (1).txt",
		"unicode/è€ü 日本.txt",
		"sym/!$&'()*+,;=:@.txt",
		"tilde~/percent%25/hash#?.txt",
		"a//b/./c/../d",
		"plus+and space",
		"quote\"<>[]{}|\\^`.bin",
	}
}

// recorded is what the test server saw of a request.
type recorded struct {
	method        string
	path          string
	identity      *domain.Identity
	err           error
	body          []byte
	bodyErr       error
	trailer       http.Header
	contentSHA256 string
	encoding      string
	contentLength int64
}

// sdkServer verifies every request with the verifier and answers with
// minimal S3 responses so that the SDK calls complete.
type sdkServer struct {
	v        auth.Verifier
	mu       sync.Mutex
	requests []recorded
}

func (s *sdkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := recorded{method: r.Method, path: r.URL.Path, contentSHA256: r.Header.Get("X-Amz-Content-Sha256")}
	id, err := s.v.Verify(r.Context(), r)
	rec.identity, rec.err = id, err
	if err == nil {
		rec.body, rec.bodyErr = io.ReadAll(r.Body)
		rec.trailer = r.Trailer.Clone()
		rec.encoding = r.Header.Get("Content-Encoding")
		rec.contentLength = r.ContentLength
	}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.mu.Unlock()
	switch {
	case err != nil:
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>"+err.Error()+"</Message></Error>")
	case rec.bodyErr != nil:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "<Error><Code>BadDigest</Code><Message>"+rec.bodyErr.Error()+"</Message></Error>")
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`)
	case r.Method == http.MethodGet:
		w.Header().Set("ETag", `"5d41402abc4b2a76b9719d911017c592"`)
		_, _ = io.WriteString(w, "hello")
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("ETag", `"5d41402abc4b2a76b9719d911017c592"`)
		w.WriteHeader(http.StatusOK)
	}
}

func (s *sdkServer) last(t *testing.T) recorded {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.requests)
	return s.requests[len(s.requests)-1]
}

func newSDKClient(endpoint string, httpClient *http.Client, accessKey, secretKey, token string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, token),
		BaseEndpoint: aws.String(endpoint),
		UsePathStyle: true,
		HTTPClient:   httpClient,
		Retryer:      aws.NopRetryer{},
	})
}

// TestAWSSDKRequests drives the real aws-sdk-go-v2 S3 client against a
// server that authenticates every request with the verifier: header
// signatures with a payload hash (HTTP), UNSIGNED-PAYLOAD and aws-chunked
// uploads with a trailing checksum (HTTPS, unseekable bodies).
func TestAWSSDKRequests(t *testing.T) {
	ctx := context.Background()
	for _, tlsMode := range []bool{false, true} {
		srv := &sdkServer{v: newVerifier(t, clock.NewReal(), auth.WithRegion("us-east-1"))}
		var ts *httptest.Server
		if tlsMode {
			ts = httptest.NewTLSServer(srv)
		} else {
			ts = httptest.NewServer(srv)
		}
		client := newSDKClient(ts.URL, ts.Client(), awsAccessKey, awsSecretKey, "")
		name := map[bool]string{false: "http", true: "https"}[tlsMode]
		for _, key := range sdkKeys() {
			t.Run(name+"/"+key, func(t *testing.T) {
				payload := []byte("payload of " + key)
				_, err := client.PutObject(ctx, &s3.PutObjectInput{
					Bucket: aws.String("bucket"), Key: aws.String(key), Body: bytes.NewReader(payload),
					ContentType: aws.String("text/plain"), Metadata: map[string]string{"owner": "alice  smith"},
				})
				require.NoError(t, err)
				rec := srv.last(t)
				require.NoError(t, rec.err)
				require.NoError(t, rec.bodyErr)
				assert.Equal(t, payload, rec.body)
				assert.Equal(t, "/bucket/"+key, rec.path)
				assert.Equal(t, domain.AuthTypeHeader, rec.identity.AuthType)

				_, err = client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String(key), Range: aws.String("bytes=0-4"), VersionId: aws.String("v1/2+3")})
				require.NoError(t, err)
				require.NoError(t, srv.last(t).err)
				_, err = client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String(key)})
				require.NoError(t, err)
				require.NoError(t, srv.last(t).err)
				_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bucket"), Key: aws.String(key)})
				require.NoError(t, err)
				require.NoError(t, srv.last(t).err)
				_, err = client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bucket"), Prefix: aws.String(key), Delimiter: aws.String("/"), StartAfter: aws.String(key + "~")})
				require.NoError(t, err)
				require.NoError(t, srv.last(t).err)
			})
		}

		t.Run(name+"/unseekable body", func(t *testing.T) {
			payload := bytes.Repeat([]byte("nexo"), 40000)
			_, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("bucket"), Key: aws.String("stream/obj"),
				Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: aws.Int64(int64(len(payload))),
				ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c,
			})
			if !tlsMode {
				// Unseekable bodies need TLS for the trailing checksum.
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			rec := srv.last(t)
			require.NoError(t, rec.err)
			require.NoError(t, rec.bodyErr)
			assert.Equal(t, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", rec.contentSHA256)
			assert.Equal(t, payload, rec.body)
			assert.Equal(t, int64(len(payload)), rec.contentLength, "the decoded length replaces the wire length")
			assert.Empty(t, rec.encoding, "aws-chunked is removed from Content-Encoding")
			assert.NotEmpty(t, rec.trailer.Get("x-amz-checksum-crc32c"), "the verified trailer is exposed in r.Trailer")
		})

		t.Run(name+"/temporary credentials", func(t *testing.T) {
			temp := newSDKClient(ts.URL, ts.Client(), tempAccessKey, tempSecretKey, tempToken)
			_, err := temp.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("k")})
			require.NoError(t, err)
			rec := srv.last(t)
			require.NoError(t, rec.err)
			assert.Equal(t, tempParent, rec.identity.Parent)
		})

		t.Run(name+"/wrong secret", func(t *testing.T) {
			bad := newSDKClient(ts.URL, ts.Client(), awsAccessKey, "not-the-secret", "")
			_, err := bad.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("k")})
			require.Error(t, err)
			assert.ErrorIs(t, srv.last(t).err, domain.ErrSignatureMismatch)
		})
		ts.Close()
	}
}

// TestAWSSDKPresignedURLs checks the URLs of the SDK presign client, for
// every operation the share links use.
func TestAWSSDKPresignedURLs(t *testing.T) {
	ctx := context.Background()
	v := newVerifier(t, clock.NewReal(), auth.WithRegion("us-east-1"))
	client := newSDKClient("http://localhost:9000", http.DefaultClient, awsAccessKey, awsSecretKey, "")
	presigner := s3.NewPresignClient(client, s3.WithPresignExpires(15*time.Minute))
	for _, key := range sdkKeys() {
		t.Run(key, func(t *testing.T) {
			bucket, k := aws.String("bucket"), aws.String(key)
			presigned := map[string]*v4.PresignedHTTPRequest{}
			var err error
			presigned["GET"], err = presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: k,
				ResponseContentDisposition: aws.String(`attachment; filename="a b.txt"`), VersionId: aws.String("v1")})
			require.NoError(t, err)
			presigned["PUT"], err = presigner.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: k, ContentType: aws.String("image/png")})
			require.NoError(t, err)
			presigned["HEAD"], err = presigner.PresignHeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: k})
			require.NoError(t, err)
			presigned["DELETE"], err = presigner.PresignDeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: k})
			require.NoError(t, err)
			presigned["UploadPart"], err = presigner.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: bucket, Key: k, UploadId: aws.String("upload/1"), PartNumber: aws.Int32(3)})
			require.NoError(t, err)
			for op, p := range presigned {
				req := httptest.NewRequest(p.Method, p.URL, strings.NewReader("body"))
				for name, values := range p.SignedHeader {
					if !strings.EqualFold(name, "host") {
						req.Header[name] = values
					}
				}
				id, err := v.Verify(ctx, req)
				require.NoError(t, err, op)
				assert.Equal(t, domain.AuthTypeQuery, id.AuthType, op)
				assert.Equal(t, awsAccessKey, id.AccessKey, op)

				tampered := httptest.NewRequest(p.Method, strings.Replace(p.URL, "/bucket/", "/other/", 1), nil)
				for name, values := range p.SignedHeader {
					if !strings.EqualFold(name, "host") {
						tampered.Header[name] = values
					}
				}
				_, err = v.Verify(ctx, tampered)
				assert.ErrorIs(t, err, domain.ErrSignatureMismatch, op)
			}
		})
	}
}

// TestAWSSDKGenericSigner signs requests with the SDK v4 signer directly:
// requests without X-Amz-Content-Sha256 (generic SigV4 clients, STS form
// posts) are verified against the hash of the body.
func TestAWSSDKGenericSigner(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: awsAccessKey, SecretAccessKey: awsSecretKey}
	build := func(t *testing.T, service, body string) *http.Request {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://localhost:9000/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		require.NoError(t, signer.SignHTTP(ctx, creds, req, sha256Hex([]byte(body)), service, "us-east-1", testNow))
		return req
	}

	v := newVerifier(t, clk)
	form := url.Values{"Action": {"AssumeRole"}, "Version": {"2011-06-15"}, "DurationSeconds": {"3600"}}.Encode()
	stsCtx := auth.WithExpectedService(ctx, auth.ServiceSTS)
	for _, tc := range []struct {
		service string
		ctx     context.Context
	}{{"sts", stsCtx}, {"s3", ctx}} {
		req := build(t, tc.service, form)
		id, err := v.Verify(tc.ctx, req)
		require.NoError(t, err, tc.service)
		assert.Equal(t, awsAccessKey, id.AccessKey)
		data, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, form, string(data), "the buffered body is replayed")
		require.NoError(t, req.Body.Close())
	}
	// Empty bodies hash to the empty SHA-256.
	_, err := v.Verify(ctx, build(t, "s3", ""))
	require.NoError(t, err)
	// A different body breaks the signature.
	req := build(t, "sts", form)
	req.Body = io.NopCloser(strings.NewReader(form + "&x=1"))
	req.ContentLength += 4
	_, err = v.Verify(stsCtx, req)
	assert.ErrorIs(t, err, domain.ErrSignatureMismatch)

	// Bodies above the bound need X-Amz-Content-Sha256.
	small := newVerifier(t, clk, auth.WithMaxUnhashedBody(8))
	_, err = small.Verify(stsCtx, build(t, "sts", form))
	assertS3Error(t, err, domain.ErrInvalidArgument, "MissingSecurityHeader")
	req = build(t, "sts", form)
	req.ContentLength = -1
	_, err = small.Verify(stsCtx, req)
	assertS3Error(t, err, domain.ErrInvalidArgument, "MissingSecurityHeader", "unknown length")
	req = build(t, "sts", form)
	req.ContentLength = -1
	req.Body = io.NopCloser(errReader{})
	_, err = v.Verify(stsCtx, req)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// Presigned with a payload hash: the SDK signs the X-Amz-Content-Sha256
	// header, which the client must then send.
	put := httptest.NewRequest(http.MethodPut, "http://localhost:9000/bucket/key?X-Amz-Expires=900", nil)
	put.Header.Set("X-Amz-Content-Sha256", sha256Hex([]byte("data")))
	uri, signedHeader, err := signer.PresignHTTP(ctx, creds, put, sha256Hex([]byte("data")), "s3", "us-east-1", testNow)
	require.NoError(t, err)
	send := func(body string) *http.Request {
		req := httptest.NewRequest(http.MethodPut, uri, strings.NewReader(body))
		req.Header.Set("X-Amz-Content-Sha256", signedHeader.Get("X-Amz-Content-Sha256"))
		return req
	}
	ok := send("data")
	_, err = v.Verify(ctx, ok)
	require.NoError(t, err)
	data, err := io.ReadAll(ok.Body)
	require.NoError(t, err)
	assert.Equal(t, "data", string(data))
	bad := send("nope")
	_, err = v.Verify(ctx, bad)
	require.NoError(t, err)
	_, err = io.ReadAll(bad.Body)
	assertS3Error(t, err, domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch")
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
