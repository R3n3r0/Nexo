// Package fuzz_test fuzzes the request bodies of the S3 API and of the
// Admin API end to end: the documents reach a server started in-process
// (internal/testutil, SNSD with the builtin KMS) in requests signed with
// SigV4 as root, through the whole stack (routing, authentication,
// limits, decoding, validation, services, storage). A response 500 (a
// panic recovered by the server, an unexpected error) or a broken
// connection is a finding; 501 (not implemented) is accepted.
//
// Run a target with "make fuzz" or
// "go test -run '^$' -fuzz '^FuzzS3Bodies$' -fuzztime 60s ./test/fuzz/".
package fuzz_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/r3n3r0/nexo/internal/testutil"
)

// maxBody bounds the fuzzed documents: the limits of the server (1 MiB
// for most documents) are tested elsewhere.
const maxBody = 64 << 10

// quietTB drops the logs of the server: a fuzzing run sends hundreds of
// thousands of requests.
type quietTB struct{ testing.TB }

func (quietTB) Log(...any)          {}
func (quietTB) Logf(string, ...any) {}

// target is a server and the root credentials.
type target struct {
	srv    *testutil.Server
	creds  aws.Credentials
	client *http.Client
	signer *v4.Signer
}

// start runs the server of a fuzz target.
func start(f *testing.F) *target {
	f.Helper()
	srv := testutil.StartServer(quietTB{f}, testutil.WithKMS())
	return &target{
		srv:    srv,
		creds:  aws.Credentials{AccessKeyID: srv.RootUser, SecretAccessKey: srv.RootPassword},
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		// The canonical URI is the path as sent.
		signer: v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }),
	}
}

// do sends a request signed as root with body (its SHA-256 and MD5) and
// the headers hdr (key, value pairs), and returns the status and the
// response body.
func (tg *target) do(t testing.TB, method, rawURL string, body []byte, hdr ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request %s %s: %v", method, rawURL, err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	sum := md5.Sum(body)
	req.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
	hash := sha256.Sum256(body)
	payload := hex.EncodeToString(hash[:])
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if err := tg.signer.SignHTTP(req.Context(), tg.creds, req, payload, "s3", tg.srv.Config.Region, time.Now()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	resp, err := tg.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, out
}

// mustDo sends a request of the set up of a target: it must succeed.
func (tg *target) mustDo(t testing.TB, method, rawURL string, body []byte, hdr ...string) []byte {
	t.Helper()
	status, out := tg.do(t, method, rawURL, body, hdr...)
	if status >= 300 {
		t.Fatalf("set up %s %s: %d %s", method, rawURL, status, out)
	}
	return out
}

// checkStatus fails on a response the fuzzed input must never cause.
func checkStatus(t *testing.T, op string, status int, body, out []byte) {
	t.Helper()
	if status >= 500 && status != http.StatusNotImplemented {
		t.Fatalf("%s: status %d for the document %q: %s", op, status, body, out)
	}
}
