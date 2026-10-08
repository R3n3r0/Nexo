// Package integration_test exercises a Nexo server started in-process
// (internal/testutil) through real clients: aws-sdk-go-v2 (S3 and STS) and
// raw net/http requests signed with the SDK's SigV4 signer. Identities and
// policies are provisioned through the services the test server exposes.
//
// The suite runs in every deployment mode (docs/08 §8.6 TM-01): the
// top-level tests on a single drive (SNSD), TestDeploymentModes the same
// tests on a node with four drives (SNMD) and on a cluster of 4 nodes x 2
// drives running in the test process behind a round-robin load balancer.
//
// The test IDs refer to docs/06 §6.2 (TS-I-*) and docs/09 §9.6 (TPS-*).
package integration_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// unsignedPayload is the payload hash of presigned and unsigned requests.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// env is a running server with a root S3 client.
type env struct {
	t    *testing.T
	srv  *testutil.Server
	root *s3.Client
	ctx  context.Context
}

// newEnv starts a server for the test, in the deployment mode the test
// runs in (modeOf).
func newEnv(t *testing.T, opts ...testutil.Option) *env {
	t.Helper()
	srv := startDeployment(t, modeOf(t), opts...)
	e := &env{t: t, srv: srv, ctx: context.Background()}
	e.root = e.client(srv.RootUser, srv.RootPassword, "")
	return e
}

// creds are static credentials.
type creds struct {
	AccessKey, SecretKey, Token string
}

func (c creds) aws() aws.Credentials {
	return aws.Credentials{AccessKeyID: c.AccessKey, SecretAccessKey: c.SecretKey, SessionToken: c.Token}
}

// rootCreds returns the root credentials.
func (e *env) rootCreds() creds {
	return creds{AccessKey: e.srv.RootUser, SecretKey: e.srv.RootPassword}
}

// base64MD5 is the Content-MD5 of b.
func base64MD5(b []byte) string {
	sum := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// sdkConfig returns an SDK configuration with static credentials.
func (e *env) sdkConfig(c creds) aws.Config {
	return aws.Config{
		Region:      e.srv.Config.Region,
		Credentials: credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, c.Token),
		HTTPClient:  &http.Client{Timeout: 60 * time.Second},
		// The tests assert the first answer: no retries.
		RetryMaxAttempts: 1,
	}
}

// client returns a path-style S3 client for the server.
func (e *env) client(accessKey, secretKey, token string) *s3.Client {
	return e.clientFor(creds{AccessKey: accessKey, SecretKey: secretKey, Token: token})
}

func (e *env) clientFor(c creds) *s3.Client {
	return s3.NewFromConfig(e.sdkConfig(c), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(e.srv.S3URL)
		o.UsePathStyle = true
	})
}

// stsClient returns an STS client for the server.
func (e *env) stsClient(c creds) *sts.Client {
	return sts.NewFromConfig(e.sdkConfig(c), func(o *sts.Options) { o.BaseEndpoint = aws.String(e.srv.S3URL) })
}

// presigner returns a presign client signing with c.
func (e *env) presigner(c creds) *s3.PresignClient {
	return s3.NewPresignClient(e.clientFor(c))
}

// mkBucket creates a bucket as root.
func (e *env) mkBucket(name string) {
	e.t.Helper()
	_, err := e.root.CreateBucket(e.ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})
	require.NoError(e.t, err, "create bucket %s", name)
}

// put stores an object as root and returns its ETag (unquoted).
func (e *env) put(bucket, key string, body []byte) string {
	e.t.Helper()
	out, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)})
	require.NoError(e.t, err, "put %s/%s", bucket, key)
	return strings.Trim(aws.ToString(out.ETag), `"`)
}

// get reads an object as root.
func (e *env) get(bucket, key string) []byte {
	e.t.Helper()
	return getWith(e.t, e.ctx, e.root, bucket, key)
}

func getWith(t *testing.T, ctx context.Context, c *s3.Client, bucket, key string) []byte {
	t.Helper()
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(t, err, "get %s/%s", bucket, key)
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	return data
}

// createUser creates an IAM user with the given policies.
func (e *env) createUser(accessKey, secretKey string, policies ...string) creds {
	e.t.Helper()
	_, err := e.srv.IAM.CreateUser(e.ctx, iam.CreateUserInput{AccessKey: accessKey, SecretKey: secretKey, PolicyNames: policies})
	require.NoError(e.t, err, "create user %s", accessKey)
	return creds{AccessKey: accessKey, SecretKey: secretKey}
}

// identity resolves the identity of an access key, as the server does.
func (e *env) identity(accessKey string) domain.Identity {
	e.t.Helper()
	_, id, err := e.srv.IAM.LookupCredentials(e.ctx, accessKey)
	require.NoError(e.t, err)
	return id
}

// setPolicy stores a named IAM policy given as JSON.
func (e *env) setPolicy(name, doc string) {
	e.t.Helper()
	p, err := e.srv.Policies.Parse(e.ctx, []byte(doc))
	require.NoError(e.t, err, "parse policy %s", name)
	require.NoError(e.t, e.srv.Policies.SetPolicy(e.ctx, name, p))
}

// parsePolicy decodes a policy document.
func (e *env) parsePolicy(doc string) *domain.Policy {
	e.t.Helper()
	p, err := e.srv.Policies.Parse(e.ctx, []byte(doc))
	require.NoError(e.t, err)
	return p
}

// putBucketPolicy sets a bucket policy (any JSON value) through S3.
func (e *env) putBucketPolicy(bucket string, doc any) {
	e.t.Helper()
	var raw string
	switch v := doc.(type) {
	case string:
		raw = v
	default:
		b, err := json.Marshal(v)
		require.NoError(e.t, err)
		raw = string(b)
	}
	_, err := e.root.PutBucketPolicy(e.ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(raw)})
	require.NoError(e.t, err)
}

// apiCode returns the S3 error code of an SDK error ("" when none).
func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// httpStatus returns the HTTP status of an SDK error (0 when none).
func httpStatus(err error) int {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// response is a fully read HTTP response.
type response struct {
	*http.Response
	Body []byte
}

// do sends a request with a plain HTTP client and reads the response.
func do(t *testing.T, req *http.Request) response {
	t.Helper()
	return doWith(t, &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirect}, req)
}

func doWith(t *testing.T, c *http.Client, req *http.Request) response {
	t.Helper()
	resp, err := c.Do(req)
	require.NoError(t, err, "%s %s", req.Method, req.URL)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{Response: resp, Body: body}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// newRequest builds a request (fails the test on error).
func newRequest(t *testing.T, method, rawURL string, body []byte) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	require.NoError(t, err)
	return req
}

// s3Signer returns the SDK SigV4 signer configured for S3: the canonical
// URI is the escaped path as sent, not escaped a second time.
func s3Signer() *v4.Signer {
	return v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
}

// signRequest signs req with SigV4 (Authorization header) using the SDK
// signer, with the SHA-256 of body as payload hash.
func (e *env) signRequest(req *http.Request, c creds, body []byte) {
	e.t.Helper()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", hash)
	if c.Token != "" {
		req.Header.Set("X-Amz-Security-Token", c.Token)
	}
	err := s3Signer().SignHTTP(e.ctx, c.aws(), req, hash, "s3", e.srv.Config.Region, time.Now())
	require.NoError(e.t, err)
}

// signRequestUnsigned signs req with UNSIGNED-PAYLOAD (streamed bodies).
func (e *env) signRequestUnsigned(req *http.Request, c creds) {
	e.t.Helper()
	req.Header.Set("X-Amz-Content-Sha256", unsignedPayload)
	err := s3Signer().SignHTTP(e.ctx, c.aws(), req, unsignedPayload, "s3", e.srv.Config.Region, time.Now())
	require.NoError(e.t, err)
}

// signedDo signs and sends a request as c.
func (e *env) signedDo(method, path string, c creds, body []byte, hdr http.Header) response {
	e.t.Helper()
	req := newRequest(e.t, method, e.srv.S3URL+path, body)
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	e.signRequest(req, c, body)
	return do(e.t, req)
}

// presignRaw presigns a request with the SDK signer: query parameters
// already in rawURL (X-Amz-Expires included) are signed as they are.
func (e *env) presignRaw(method, rawURL string, c creds, signedHeaders http.Header, at time.Time) string {
	e.t.Helper()
	req := newRequest(e.t, method, rawURL, nil)
	for k, vs := range signedHeaders {
		req.Header[http.CanonicalHeaderKey(k)] = vs
	}
	signed, _, err := s3Signer().PresignHTTP(e.ctx, c.aws(), req, unsignedPayload, "s3", e.srv.Config.Region, at)
	require.NoError(e.t, err)
	return signed
}

// s3Error is the S3 error document, decoded independently of the server
// types.
type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

// requireS3Error checks an S3 error response: status, well-formed XML
// document with the code, a message and the request id of the header.
func requireS3Error(t *testing.T, resp response, status int, code string) s3Error {
	t.Helper()
	require.Equal(t, status, resp.StatusCode, "body: %s", resp.Body)
	reqID := resp.Header.Get("x-amz-request-id")
	assert.NotEmpty(t, reqID, "x-amz-request-id")
	assert.NotEmpty(t, resp.Header.Get("x-amz-id-2"), "x-amz-id-2")
	var doc s3Error
	if resp.Request.Method == http.MethodHead || status == http.StatusNotModified {
		assert.Empty(t, resp.Body, "no body for HEAD/304")
		return doc
	}
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/xml")
	require.True(t, bytes.HasPrefix(resp.Body, []byte("<?xml")), "XML declaration: %s", resp.Body)
	require.NoError(t, xml.Unmarshal(resp.Body, &doc), "well-formed XML: %s", resp.Body)
	assert.Equal(t, code, doc.Code, "body: %s", resp.Body)
	assert.NotEmpty(t, doc.Message)
	assert.Equal(t, reqID, doc.RequestID)
	return doc
}

// anonGet sends an unsigned GET to the S3 API and returns the status.
func (e *env) anonStatus(method, path string, body []byte) int {
	e.t.Helper()
	return do(e.t, newRequest(e.t, method, e.srv.S3URL+path, body)).StatusCode
}

// policyJSON formats a policy document from statements.
func policyJSON(statements ...string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[%s]}`, strings.Join(statements, ","))
}

// hostPort returns the host:port of the S3 URL.
func (e *env) hostPort() string {
	u, err := url.Parse(e.srv.S3URL)
	require.NoError(e.t, err)
	return u.Host
}

// loopbackClient returns an HTTP client that dials the server whatever the
// host name of the URL (virtual-host style "{bucket}.localhost").
func (e *env) loopbackClient() *http.Client {
	target := e.hostPort()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: noRedirect,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target)
		}},
	}
}
