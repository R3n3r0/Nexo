package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// console is a console session of a user (cookie jar and CSRF token).
type console struct {
	t      *testing.T
	e      *env
	client *http.Client
	csrf   string
}

// consoleLogin opens a console session with the credentials c.
func (e *env) consoleLogin(c creds) *console {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(e.t, err)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar, CheckRedirect: noRedirect}
	body := fmt.Sprintf(`{"accessKey":%q,"secretKey":%q}`, c.AccessKey, c.SecretKey)
	req := newRequest(e.t, http.MethodPost, e.srv.ConsoleURL+"/api/v1/auth/login", []byte(body))
	req.Header.Set("Content-Type", "application/json")
	resp := doWith(e.t, client, req)
	require.Equal(e.t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var login struct {
		CSRFToken string `json:"csrfToken"`
	}
	require.NoError(e.t, json.Unmarshal(resp.Body, &login))
	return &console{t: e.t, e: e, client: client, csrf: login.CSRFToken}
}

// do sends an Admin API request with the session.
func (c *console) do(method, path, body string) response {
	c.t.Helper()
	var raw []byte
	if body != "" {
		raw = []byte(body)
	}
	req := newRequest(c.t, method, c.e.srv.ConsoleURL+"/api/v1"+path, raw)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-CSRF-Token", c.csrf)
	return doWith(c.t, c.client, req)
}

// TestConsoleBulkPrefixNeedsListBucket: the bulk operations of the console
// on prefixes (delete, rename, zip) list the prefix, so they need
// s3:ListBucket as the listing itself: without it they are denied as a
// whole, and the keys under the prefix never appear in their per-object
// errors or in the errors entry of the archive (regression).
func TestConsoleBulkPrefixNeedsListBucket(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("vault")
	e.put("vault", "tmp/a.txt", []byte("tmp"))
	e.put("vault", "private/ceo-salary.txt", []byte("secret"))
	e.put("vault", "private/payroll-2026.xlsx", []byte("secret"))
	e.setPolicy("vault-tmp", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:GetObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::vault/tmp/*"]}]}`)
	bob := e.consoleLogin(e.createUser("bob", "bob-secret-1", "vault-tmp"))

	tests := []struct {
		name         string
		method, path string
		body         string
	}{
		{name: "listing", method: http.MethodGet, path: "/buckets/vault/objects?prefix=private/"},
		{name: "zip of the bucket", method: http.MethodPost, path: "/buckets/vault/objects/download-zip", body: `{"prefixes":[""]}`},
		{name: "zip of a prefix", method: http.MethodPost, path: "/buckets/vault/objects/download-zip", body: `{"prefixes":["private/"]}`},
		{name: "delete a prefix", method: http.MethodDelete, path: "/buckets/vault/objects", body: `{"prefixes":["private/"]}`},
		{name: "delete every version", method: http.MethodDelete, path: "/buckets/vault/objects", body: `{"prefixes":["private/"],"allVersions":true}`},
		{name: "rename a prefix", method: http.MethodPost, path: "/buckets/vault/objects/rename", body: `{"source":"private/","destination":"tmp/private/","prefix":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := bob.do(tt.method, tt.path, tt.body)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s", resp.Body)
			assert.Contains(t, resp.Header.Get("Content-Type"), "application/problem+json")
			for _, leak := range []string{"ceo-salary", "payroll-2026"} {
				assert.NotContains(t, string(resp.Body), leak)
			}
		})
	}
	// Nothing was deleted or moved.
	assert.Equal(t, []byte("secret"), e.get("vault", "private/ceo-salary.txt"))
	assert.Equal(t, []byte("secret"), e.get("vault", "private/payroll-2026.xlsx"))

	// The objects bob may handle stay reachable one by one.
	resp := bob.do(http.MethodPost, "/buckets/vault/objects/download-zip", `{"keys":["tmp/a.txt"]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	resp = bob.do(http.MethodDelete, "/buckets/vault/objects", `{"objects":[{"key":"tmp/a.txt"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), `"deleted":[{"key":"tmp/a.txt"`)
}

// consoleLoginFrom sends a login through a proxy that forwards for client.
func (e *env) consoleLoginFrom(client, accessKey, secretKey string) response {
	e.t.Helper()
	body := fmt.Sprintf(`{"accessKey":%q,"secretKey":%q}`, accessKey, secretKey)
	req := newRequest(e.t, http.MethodPost, e.srv.ConsoleURL+"/api/v1/auth/login", []byte(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", client)
	req.Header.Set("X-Forwarded-Proto", "https")
	return do(e.t, req)
}

// TestConsoleBehindProxy: behind a trusted reverse proxy terminating TLS,
// the login throttle counts per forwarded client (one client failing does
// not lock out the others sharing the proxy) and the session cookies carry
// Secure (regression: the throttle keyed on the proxy address and only a
// TLS connection set Secure).
func TestConsoleBehindProxy(t *testing.T) {
	e := newEnv(t, testutil.WithConfig(func(cfg *config.Static) {
		cfg.TrustedProxies = []string{"127.0.0.1", "::1"}
		cfg.BrowserRedirectURL = "https://console.example.com"
	}))
	for range session.DefaultMaxFailures {
		resp := e.consoleLoginFrom("198.51.100.7", e.srv.RootUser, "wrong-secret")
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s", resp.Body)
	}
	resp := e.consoleLoginFrom("198.51.100.7", e.srv.RootUser, e.srv.RootPassword)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "the failing client is locked out")

	resp = e.consoleLoginFrom("198.51.100.8", e.srv.RootUser, e.srv.RootPassword)
	require.Equal(t, http.StatusOK, resp.StatusCode, "another client behind the same proxy signs in: %s", resp.Body)
	cookies := resp.Cookies()
	require.NotEmpty(t, cookies)
	for _, c := range cookies {
		assert.True(t, c.Secure, "cookie %s carries Secure", c.Name)
	}
}

// doRaw sends an Admin API request with a raw body and extra headers.
func (c *console) doRaw(method, path string, body []byte, hdr http.Header) response {
	c.t.Helper()
	req := newRequest(c.t, method, c.e.srv.ConsoleURL+"/api/v1"+path, body)
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	req.Header.Set("X-CSRF-Token", c.csrf)
	return doWith(c.t, c.client, req)
}

// parityCase is one operation performed through S3 and through the console.
type parityCase struct {
	name    string
	viaS3   func() error
	console func() response
}

// TestConsoleMatchesS3PolicyConditions: the console evaluates the condition
// values of the S3 request each proxied operation stands for (s3:versionid,
// the forwarded x-amz-* headers, s3:RequestObjectTag/*), so the same policy
// decides alike through S3 and through the Admin API (docs/12 §12.7;
// regression: a Deny conditioned on s3:versionid was bypassed through the
// console download, stat, preview, tags and delete).
func TestConsoleMatchesS3PolicyConditions(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("vers")
	_, err := e.root.PutBucketVersioning(e.ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("vers"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	versionOf := func(body string) string {
		out, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("vers"), Key: aws.String("doc.txt"), Body: strings.NewReader(body)})
		require.NoError(t, err)
		return aws.ToString(out.VersionId)
	}
	v1, v2 := versionOf("v1"), versionOf("v2")
	e.putBucketPolicy("vers", fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":{"AWS":["*"]},"Action":["s3:GetObject","s3:GetObjectVersion","s3:DeleteObjectVersion",
			"s3:GetObjectVersionTagging","s3:PutObjectVersionTagging"],
		 "Resource":"arn:aws:s3:::vers/*","Condition":{"StringEquals":{"s3:versionid":[%q]}}},
		{"Effect":"Deny","Principal":{"AWS":["*"]},"Action":"s3:PutObject","Resource":"arn:aws:s3:::vers/*",
		 "Condition":{"StringEquals":{"s3:RequestObjectTag/class":"secret"}}}]}`, v1))
	frankCreds := e.createUser("frank", "frank-secret-1", "readwrite")
	frankS3 := e.clientFor(frankCreds)
	frank := e.consoleLogin(frankCreds)

	getVersion := func(v string) func() error {
		return func() error {
			out, err := frankS3.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("vers"), Key: aws.String("doc.txt"), VersionId: aws.String(v)})
			if err == nil {
				_ = out.Body.Close()
			}
			return err
		}
	}
	var tests []parityCase
	for _, v := range []string{v1, v2} {
		tests = append(tests,
			parityCase{"get " + v, getVersion(v), func() response {
				return frank.do(http.MethodGet, "/buckets/vers/objects/download?key=doc.txt&versionId="+v, "")
			}},
			parityCase{"preview " + v, getVersion(v), func() response {
				return frank.do(http.MethodGet, "/buckets/vers/objects/preview?key=doc.txt&versionId="+v, "")
			}},
			parityCase{"stat " + v, func() error {
				_, err := frankS3.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("vers"), Key: aws.String("doc.txt"), VersionId: aws.String(v)})
				return err
			}, func() response {
				return frank.do(http.MethodGet, "/buckets/vers/objects/stat?key=doc.txt&versionId="+v, "")
			}},
			parityCase{"tags " + v, func() error {
				_, err := frankS3.PutObjectTagging(e.ctx, &s3.PutObjectTaggingInput{Bucket: aws.String("vers"), Key: aws.String("doc.txt"), VersionId: aws.String(v),
					Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("a"), Value: aws.String("1")}}}})
				return err
			}, func() response {
				return frank.do(http.MethodPut, "/buckets/vers/objects/tags?key=doc.txt&versionId="+v, `{"tags":{"a":"1"}}`)
			}},
		)
	}
	for _, tags := range []string{"class=secret", "class=public"} {
		tests = append(tests, parityCase{"upload tagged " + tags, func() error {
			_, err := frankS3.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("vers"), Key: aws.String("new.txt"), Body: strings.NewReader("x"), Tagging: aws.String(tags)})
			return err
		}, func() response {
			return frank.doRaw(http.MethodPost, "/buckets/vers/objects/upload?key=new.txt", []byte("x"), http.Header{"X-Amz-Tagging": {tags}, "Content-Type": {"text/plain"}})
		}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.viaS3()
			if err != nil {
				require.Equal(t, http.StatusForbidden, httpStatus(err), "unexpected S3 error: %v", err)
			}
			resp := tt.console()
			if resp.StatusCode >= 300 {
				require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s", resp.Body)
			}
			denied := strings.Contains(tt.name, v1) || strings.Contains(tt.name, "secret")
			assert.Equal(t, denied, err != nil, "S3: %v", err)
			assert.Equal(t, denied, resp.StatusCode >= 300, "console: %d %s", resp.StatusCode, resp.Body)
		})
	}

	// DeleteObject of a version: denied through S3 and reported as a
	// per-object access_denied through the console; v1 survives.
	_, err = frankS3.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("vers"), Key: aws.String("doc.txt"), VersionId: aws.String(v1)})
	require.Equal(t, http.StatusForbidden, httpStatus(err))
	resp := frank.do(http.MethodDelete, "/buckets/vers/objects", fmt.Sprintf(`{"objects":[{"key":"doc.txt","versionId":%q}]}`, v1))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), `"code":"access_denied"`)
	out, err := e.root.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("vers"), Key: aws.String("doc.txt"), VersionId: aws.String(v1)})
	require.NoError(t, err, "version v1 is still there")
	_ = out.Body.Close()
}

// homeDirPolicy is the "home directory" policy of docs/09 §9.3: each user
// lists and handles only home/<username>/ of the bucket homes.
const homeDirPolicy = `{"Version":"2012-10-17","Statement":[
	{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::homes"],
	 "Condition":{"StringLike":{"s3:prefix":["home/${aws:username}/*","home/${aws:username}/"]}}},
	{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::homes/home/${aws:username}/*"]}]}`

// TestConsoleHomeDirectory: a user restricted to a home directory by the
// documented policy sees the bucket, opens its page and browses, reads and
// writes the home directory from the console, and nothing else
// (regression: the bucket was hidden, its page answered 403 and the
// permissions of the folder denied s3:ListBucket).
func TestConsoleHomeDirectory(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("homes")
	e.mkBucket("other")
	e.put("homes", "home/gina/notes.txt", []byte("mine"))
	e.put("homes", "home/hugo/secret.txt", []byte("not mine"))
	e.setPolicy("home-dir", homeDirPolicy)
	gina := e.consoleLogin(e.createUser("gina", "gina-secret-1", "home-dir"))

	resp := gina.do(http.MethodGet, "/buckets", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var buckets struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &buckets))
	require.Len(t, buckets.Items, 1, "%s", resp.Body)
	assert.Equal(t, "homes", buckets.Items[0].Name, "only the bucket of the home directory is visible")

	resp = gina.do(http.MethodGet, "/buckets/homes", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the page of the bucket opens: %s", resp.Body)
	assert.Equal(t, http.StatusForbidden, gina.do(http.MethodGet, "/buckets/other", "").StatusCode)

	resp = gina.do(http.MethodGet, "/buckets/homes/objects?prefix=home/gina/", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "home/gina/notes.txt")
	for _, prefix := range []string{"", "home/", "home/hugo/"} {
		resp = gina.do(http.MethodGet, "/buckets/homes/objects?prefix="+prefix, "")
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "prefix %q: %s", prefix, resp.Body)
	}

	permissions := func(prefix string) (allowed []string) {
		resp := gina.do(http.MethodGet, "/me/permissions?bucket=homes&prefix="+prefix, "")
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		var p struct {
			Allowed []string `json:"allowed"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &p))
		return p.Allowed
	}
	mine := permissions("home/gina/")
	for _, a := range []string{"s3:ListBucket", "s3:GetObject", "s3:PutObject", "s3:DeleteObject"} {
		assert.Contains(t, mine, a, "home/gina/")
	}
	others := permissions("home/hugo/")
	for _, a := range []string{"s3:ListBucket", "s3:GetObject", "s3:PutObject"} {
		assert.NotContains(t, others, a, "home/hugo/")
	}

	resp = gina.doRaw(http.MethodPost, "/buckets/homes/objects/upload?key=home/gina/new.txt", []byte("new"), http.Header{"Content-Type": {"text/plain"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	resp = gina.do(http.MethodGet, "/buckets/homes/objects/download?key=home/gina/new.txt", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "new", string(resp.Body))
	assert.Equal(t, http.StatusForbidden, gina.do(http.MethodGet, "/buckets/homes/objects/download?key=home/hugo/secret.txt", "").StatusCode)
}
