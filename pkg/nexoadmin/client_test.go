package nexoadmin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// harness is a running server with a contract-checking root client.
type harness struct {
	t        *testing.T
	ctx      context.Context
	srv      *testutil.Server
	contract *contractTransport
	root     nexoadmin.AdminClient
	s3       *s3.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv := testutil.StartServer(t)
	h := &harness{t: t, ctx: context.Background(), srv: srv, contract: newContractTransport(t, srv.ConsoleURL)}
	h.root = h.client(srv.RootUser, srv.RootPassword)
	h.s3 = s3.NewFromConfig(aws.Config{
		Region:      srv.Config.Region,
		Credentials: credentials.NewStaticCredentialsProvider(srv.RootUser, srv.RootPassword, ""),
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.S3URL)
		o.UsePathStyle = true
	})
	return h
}

// client returns a contract-checked client signing with the credentials.
func (h *harness) client(accessKey, secretKey string) nexoadmin.AdminClient {
	h.t.Helper()
	c, err := nexoadmin.New(nexoadmin.Config{
		Endpoint: h.srv.ConsoleURL, AccessKey: accessKey, SecretKey: secretKey, Region: h.srv.Config.Region,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: h.contract},
	})
	require.NoError(h.t, err)
	return c
}

func (h *harness) put(bucket, key, body string) {
	h.t.Helper()
	_, err := h.s3.PutObject(h.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body)})
	require.NoError(h.t, err)
}

func apiError(t *testing.T, err error) *nexoadmin.APIError {
	t.Helper()
	var e *nexoadmin.APIError
	require.True(t, errors.As(err, &e), "want an APIError, got %v", err)
	return e
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  nexoadmin.Config
	}{
		{"no endpoint", nexoadmin.Config{AccessKey: "a", SecretKey: "b"}},
		{"relative endpoint", nexoadmin.Config{Endpoint: "localhost:9001", AccessKey: "a", SecretKey: "b"}},
		{"no credentials", nexoadmin.Config{Endpoint: "http://localhost:9001"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := nexoadmin.New(tt.cfg)
			assert.Error(t, err)
		})
	}
}

func TestAPIErrorDecoding(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		ctype    string
		body     string
		code     string
		check    func(error) bool
		position bool
	}{
		{"problem", 404, "application/problem+json", `{"title":"Not Found","status":404,"code":"user_not_found","detail":"no"}`, "user_not_found", nexoadmin.IsNotFound, false},
		{"policy problem", 400, "application/problem+json",
			`{"title":"Bad Request","status":400,"code":"malformed_policy","errors":[{"statement":1,"line":3,"column":7,"message":"bad"}]}`, "malformed_policy", nil, true},
		{"plain text", 403, "text/plain", "nope", "forbidden", nexoadmin.IsAccessDenied, false},
		{"conflict", 409, "application/problem+json", `{"title":"Conflict","status":409,"code":"conflict"}`, "conflict", nexoadmin.IsConflict, false},
		{"unauthenticated", 401, "application/problem+json", `{"title":"Unauthorized","status":401,"code":"unauthenticated"}`, "unauthenticated", nexoadmin.IsUnauthenticated, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/"), "signed request")
				assert.Contains(t, r.Header.Get("Authorization"), "/us-east-1/s3/aws4_request")
				assert.NotEmpty(t, r.Header.Get("X-Amz-Content-Sha256"))
				w.Header().Set("Content-Type", tt.ctype)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			c, err := nexoadmin.New(nexoadmin.Config{Endpoint: srv.URL, AccessKey: "ak", SecretKey: "secret-key"})
			require.NoError(t, err)
			_, err = c.GetUser(context.Background(), "alice")
			e := apiError(t, err)
			assert.Equal(t, tt.status, e.Status)
			assert.Equal(t, tt.code, e.Code)
			if tt.check != nil {
				assert.True(t, tt.check(err))
			}
			if tt.position {
				require.Len(t, e.Errors, 1)
				assert.Equal(t, 3, e.Errors[0].Line)
				assert.Contains(t, e.Error(), "line 3, column 7")
			}
		})
	}
}

// TestClientAgainstServer drives every area of the Admin API through the
// client against a real in-process server, each exchange validated
// against api/openapi.yaml.
func TestClientAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root

	t.Run("server", func(t *testing.T) {
		info, err := c.ServerInfo(ctx)
		require.NoError(t, err)
		assert.Equal(t, "snsd", info.Mode)
		require.Len(t, info.Nodes, 1)
		_, err = c.DataUsage(ctx)
		require.NoError(t, err)
		perms, err := c.MyPermissions(ctx, nexoadmin.PermissionsQuery{})
		require.NoError(t, err)
		assert.True(t, perms.IsRoot)
		assert.Empty(t, perms.Denied)
		_, err = c.Heal(ctx, nexoadmin.HealRequest{DryRun: true})
		assert.Equal(t, http.StatusNotImplemented, apiError(t, err).Status)
		// Outside a server process the service cannot be controlled.
		assert.Equal(t, http.StatusNotImplemented, apiError(t, c.RestartService(ctx)).Status)
	})

	t.Run("buckets", func(t *testing.T) {
		b, err := c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "photos", Versioning: true, QuotaBytes: 1 << 30})
		require.NoError(t, err)
		assert.Equal(t, "enabled", b.Versioning)
		assert.Equal(t, uint64(1<<30), b.QuotaBytes)
		_, err = c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "photos"})
		assert.True(t, nexoadmin.IsConflict(err), "%v", err)
		_, err = c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "Bad_Name"})
		assert.Equal(t, http.StatusBadRequest, apiError(t, err).Status)

		h.put("photos", "a/1.txt", "one")
		h.put("photos", "a/2.txt", "two")
		h.put("photos", "top.txt", "top")

		list, err := c.ListBuckets(ctx, nexoadmin.ListBucketsOptions{})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		assert.Equal(t, uint64(3), list.Items[0].Objects)
		got, err := c.GetBucket(ctx, "photos")
		require.NoError(t, err)
		assert.Equal(t, uint64(3), got.Objects)

		require.NoError(t, c.SetBucketVersioning(ctx, "photos", "suspended"))
		st, err := c.GetBucketVersioning(ctx, "photos")
		require.NoError(t, err)
		assert.Equal(t, "suspended", st)

		q, err := c.SetBucketQuota(ctx, "photos", nexoadmin.BucketQuota{Size: 2 << 30})
		require.NoError(t, err)
		assert.Equal(t, "hard", q.Type)
		require.NoError(t, c.SetBucketTags(ctx, "photos", map[string]string{"team": "web"}))
		tags, err := c.GetBucketTags(ctx, "photos")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"team": "web"}, tags)

		access, err := c.SetBucketAccess(ctx, "photos", "a/", "download")
		require.NoError(t, err)
		assert.True(t, access.Public)
		require.Equal(t, []nexoadmin.AccessRule{{Prefix: "a/", Preset: "download"}}, access.Rules)
		resp, err := http.Get(h.srv.S3URL + "/photos/a/1.txt")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "anonymous download of the public prefix")
		raw, err := c.GetBucketPolicy(ctx, "photos")
		require.NoError(t, err)
		assert.Contains(t, string(raw), "s3:GetObject")
		access, err = c.SetBucketAccess(ctx, "photos", "a/", "private")
		require.NoError(t, err)
		assert.Empty(t, access.Rules)
		_, err = c.GetBucketPolicy(ctx, "photos")
		assert.True(t, nexoadmin.IsNotFound(err), "%v", err)

		err = c.SetBucketPolicy(ctx, "photos", []byte("{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [ {\"Effect\": \"Allow\", \"Principal\": \"*\", \"Action\": \"s3:Nope\", \"Resource\": \"arn:aws:s3:::photos/*\"} ]\n}"))
		e := apiError(t, err)
		assert.Equal(t, nexoadmin.CodeMalformedPolicy, e.Code)
		require.Len(t, e.Errors, 1)
		assert.Equal(t, 1, e.Errors[0].Statement)
	})

	t.Run("objects", func(t *testing.T) {
		list, err := c.ListObjects(ctx, "photos", nexoadmin.ListObjectsOptions{})
		require.NoError(t, err)
		assert.Equal(t, []string{"a/"}, list.Prefixes)
		require.Len(t, list.Objects, 1)
		assert.Equal(t, "top.txt", list.Objects[0].Key)
		flat, err := c.ListObjects(ctx, "photos", nexoadmin.ListObjectsOptions{Recursive: true, ListOptions: nexoadmin.ListOptions{Limit: 2}})
		require.NoError(t, err)
		assert.Len(t, flat.Objects, 2)
		require.True(t, flat.IsTruncated)
		next, err := c.ListObjects(ctx, "photos", nexoadmin.ListObjectsOptions{Recursive: true, ListOptions: nexoadmin.ListOptions{Cursor: flat.NextCursor}})
		require.NoError(t, err)
		assert.Len(t, next.Objects, 1)

		st, err := c.StatObject(ctx, "photos", "a/1.txt", "")
		require.NoError(t, err)
		assert.Equal(t, int64(3), st.Size)
		assert.NotNil(t, st.Tags)
		_, err = c.StatObject(ctx, "photos", "missing", "")
		assert.True(t, nexoadmin.IsNotFound(err))

		res, err := c.DeleteObjects(ctx, "photos", nexoadmin.DeleteObjectsRequest{Prefixes: []string{"a/"}})
		require.NoError(t, err)
		assert.Len(t, res.Deleted, 2)
		assert.Empty(t, res.Errors)
	})

	t.Run("iam", func(t *testing.T) {
		_, err := c.CreatePolicy(ctx, "photos-ro", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]}]}`))
		require.NoError(t, err)
		_, err = c.CreatePolicy(ctx, "photos-ro", []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`))
		assert.True(t, nexoadmin.IsConflict(err), "%v", err)
		_, err = c.CreatePolicy(ctx, "broken", []byte("{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [\n    {\"Effect\": \"Allow\" \"Action\": \"s3:*\"}\n  ]\n}"))
		e := apiError(t, err)
		assert.Equal(t, nexoadmin.CodeMalformedPolicy, e.Code)
		require.Len(t, e.Errors, 1)
		assert.Equal(t, 4, e.Errors[0].Line, "the position is in the submitted text")
		rep, err := c.ValidatePolicy(ctx, []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"s3:*","Resource":"*"}]}`), "")
		require.NoError(t, err)
		assert.False(t, rep.Valid)
		require.Len(t, rep.Errors, 1)
		assert.Equal(t, 1, rep.Errors[0].Statement)

		_, err = c.CreateGroup(ctx, nexoadmin.CreateGroupRequest{Name: "viewers", PolicyNames: []string{"photos-ro"}})
		require.NoError(t, err)
		u, err := c.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "alice", SecretKey: "alice-secret", Groups: []string{"viewers"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"viewers"}, u.Groups)
		users, err := c.ListUsers(ctx, nexoadmin.ListOptions{})
		require.NoError(t, err)
		require.Len(t, users.Items, 1)

		disabled := nexoadmin.StatusDisabled
		u, err = c.UpdateUser(ctx, "alice", nexoadmin.UpdateUserRequest{Status: &disabled})
		require.NoError(t, err)
		assert.Equal(t, "disabled", u.Status)
		enabled := nexoadmin.StatusEnabled
		_, err = c.UpdateUser(ctx, "alice", nexoadmin.UpdateUserRequest{Status: &enabled})
		require.NoError(t, err)

		ents, err := c.PolicyEntities(ctx, "photos-ro")
		require.NoError(t, err)
		assert.Equal(t, []string{"viewers"}, ents.Groups)
		require.NoError(t, c.AttachPolicy(ctx, nexoadmin.AttachRequest{PolicyNames: []string{"readonly"}, User: "alice"}))
		require.NoError(t, c.DetachPolicy(ctx, nexoadmin.AttachRequest{PolicyNames: []string{"readonly"}, User: "alice"}))

		sim, err := c.SimulatePolicy(ctx, nexoadmin.SimulateRequest{User: "alice", Action: "s3:GetObject", Bucket: "photos", Object: "x"})
		require.NoError(t, err)
		assert.True(t, sim.Allowed)
		assert.Equal(t, "allow", sim.Decision)
		require.NotNil(t, sim.Statement)
		assert.Equal(t, "photos-ro", sim.PolicyName)
		sim, err = c.SimulatePolicy(ctx, nexoadmin.SimulateRequest{User: "alice", Action: "s3:PutObject", Bucket: "photos", Object: "x"})
		require.NoError(t, err)
		assert.False(t, sim.Allowed)

		pols, err := c.ListPolicies(ctx, nexoadmin.ListOptions{Limit: 2})
		require.NoError(t, err)
		assert.Len(t, pols.Items, 2)
		assert.NotEmpty(t, pols.NextCursor)
		p, err := c.GetPolicy(ctx, "readonly")
		require.NoError(t, err)
		assert.True(t, p.BuiltIn)

		// alice: no admin action, but her own access keys and shares.
		alice := h.client("alice", "alice-secret")
		_, err = alice.ListUsers(ctx, nexoadmin.ListOptions{})
		assert.True(t, nexoadmin.IsAccessDenied(err), "%v", err)
		ak, err := alice.CreateAccessKey(ctx, nexoadmin.CreateAccessKeyRequest{Name: "laptop"})
		require.NoError(t, err)
		assert.NotEmpty(t, ak.SecretKey, "the secret is returned once")
		assert.Equal(t, "alice", ak.ParentUser)
		keys, err := alice.ListAccessKeys(ctx, nexoadmin.ListAccessKeysOptions{})
		require.NoError(t, err)
		require.Len(t, keys.Items, 1)
		got, err := alice.GetAccessKey(ctx, ak.AccessKey.AccessKey)
		require.NoError(t, err)
		assert.Equal(t, "laptop", got.Name)
		_, err = alice.ListAccessKeys(ctx, nexoadmin.ListAccessKeysOptions{User: "bob"})
		assert.True(t, nexoadmin.IsAccessDenied(err), "%v", err)
		perms, err := alice.MyPermissions(ctx, nexoadmin.PermissionsQuery{Bucket: "photos"})
		require.NoError(t, err)
		assert.Contains(t, perms.Allowed, "s3:GetObject")
		assert.Contains(t, perms.Denied, "s3:PutObject")
		assert.Contains(t, perms.Denied, "admin:ListUsers")

		// Root manages the keys of alice too.
		name := "renamed"
		k, err := c.UpdateAccessKey(ctx, ak.AccessKey.AccessKey, nexoadmin.UpdateAccessKeyRequest{Name: &name})
		require.NoError(t, err)
		assert.Equal(t, "renamed", k.Name)
		all, err := c.ListAccessKeys(ctx, nexoadmin.ListAccessKeysOptions{User: "alice"})
		require.NoError(t, err)
		assert.Len(t, all.Items, 1)
		require.NoError(t, c.DeleteAccessKey(ctx, ak.AccessKey.AccessKey))

		g, err := c.UpdateGroup(ctx, "viewers", nexoadmin.UpdateGroupRequest{RemoveMembers: []string{"alice"}})
		require.NoError(t, err)
		assert.Empty(t, g.Members)
		groups, err := c.ListGroups(ctx, nexoadmin.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, groups.Items, 1)
		_, err = c.GetGroup(ctx, "viewers")
		require.NoError(t, err)
		require.NoError(t, c.DeleteGroup(ctx, "viewers"))
		require.NoError(t, c.DeleteUser(ctx, "alice"))
		_, err = c.GetUser(ctx, "alice")
		assert.True(t, nexoadmin.IsNotFound(err), "%v", err)
		require.NoError(t, c.DeletePolicy(ctx, "photos-ro"))
	})

	t.Run("shares", func(t *testing.T) {
		h.put("photos", "share/me.txt", "shared content")
		s, err := c.CreateShare(ctx, nexoadmin.CreateShareRequest{Bucket: "photos", Key: "share/me.txt", ExpiresIn: 3600})
		require.NoError(t, err)
		require.NotEmpty(t, s.URL)
		assert.Equal(t, "download", s.Permission)
		body := httpGet(t, s.URL, http.StatusOK)
		assert.Equal(t, "shared content", body)
		list, err := c.ListShares(ctx, nexoadmin.ListSharesOptions{Bucket: "photos"})
		require.NoError(t, err)
		require.Len(t, list.Items, 1)
		got, err := c.GetShare(ctx, s.ID)
		require.NoError(t, err)
		assert.Equal(t, s.ID, got.ID)
		require.NoError(t, c.RevokeShare(ctx, s.ID))
		httpGet(t, s.URL, http.StatusForbidden)
		n, err := c.RevokeAllShares(ctx, "")
		require.NoError(t, err)
		assert.Zero(t, n)

		u, err := c.Presign(ctx, nexoadmin.PresignRequest{Bucket: "photos", Key: "share/me.txt", ExpiresIn: 60, ContentDisposition: "attachment"})
		require.NoError(t, err)
		assert.Equal(t, "GET", u.Method)
		assert.Equal(t, "shared content", httpGet(t, u.URL, http.StatusOK))
		put, err := c.Presign(ctx, nexoadmin.PresignRequest{Method: "PUT", Bucket: "photos", Key: "share/up.txt", ContentType: "text/plain"})
		require.NoError(t, err)
		assert.Equal(t, "text/plain", put.Headers["content-type"])
		req, err := http.NewRequest(http.MethodPut, put.URL, bytes.NewReader([]byte("uploaded")))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		form, err := c.PostPolicy(ctx, nexoadmin.PostPolicyRequest{Bucket: "photos", KeyPrefix: "uploads/", MaxSize: 1024, ContentTypePrefix: "text/"})
		require.NoError(t, err)
		assert.Equal(t, h.srv.S3URL+"/photos", form.URL)
		assert.Equal(t, "uploads/${filename}", form.Fields["key"])
		assert.NotEmpty(t, form.Fields["policy"])
		assert.NotEmpty(t, form.Conditions)
	})

	t.Run("config", func(t *testing.T) {
		schema, err := c.ConfigSchema(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, schema)
		sub, err := c.SetConfig(ctx, "notify_webhook", map[string]string{"enable": "on", "endpoint": "https://hooks.example.com/x", "auth_token": "s3cret"})
		require.NoError(t, err)
		assert.Equal(t, "********", sub.Settings["auth_token"], "secrets are masked")
		_, err = c.SetConfig(ctx, "notify_webhook", map[string]string{"auth_token": "********", "queue_limit": "10"})
		require.NoError(t, err)
		exported, err := c.ExportConfig(ctx)
		require.NoError(t, err)
		assert.Contains(t, string(exported), "s3cret", "the masked value kept the secret")
		_, err = c.SetConfig(ctx, "notify_webhook", map[string]string{"enable": "maybe"})
		assert.Equal(t, http.StatusBadRequest, apiError(t, err).Status)
		_, err = c.SetConfig(ctx, "nope", map[string]string{"a": "b"})
		assert.Error(t, err)
		require.NoError(t, c.MergeConfig(ctx, map[string]map[string]string{"region": {"name": "eu-south-1"}}))
		all, err := c.ListConfig(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, all)
		require.NoError(t, c.DeleteConfig(ctx, "notify_webhook", "queue_limit"))
		got, err := c.GetConfig(ctx, "notify_webhook")
		require.NoError(t, err)
		assert.NotContains(t, got.Settings, "queue_limit")
		require.NoError(t, c.ImportConfig(ctx, exported))
		got, err = c.GetConfig(ctx, "notify_webhook")
		require.NoError(t, err)
		assert.Equal(t, "10", got.Settings["queue_limit"])
	})

	t.Run("cleanup", func(t *testing.T) {
		err := c.DeleteBucket(ctx, "photos", false)
		assert.True(t, nexoadmin.IsConflict(err), "not empty: %v", err)
		require.NoError(t, c.DeleteBucket(ctx, "photos", true))
		_, err = c.GetBucket(ctx, "photos")
		assert.True(t, nexoadmin.IsNotFound(err))
	})

	t.Run("authentication", func(t *testing.T) {
		bad := h.client(h.srv.RootUser, "wrong-secret")
		_, err := bad.ServerInfo(ctx)
		assert.True(t, nexoadmin.IsUnauthenticated(err), "%v", err)
		unknown := h.client("nobody", "nobody-secret")
		_, err = unknown.ServerInfo(ctx)
		assert.True(t, nexoadmin.IsUnauthenticated(err), "%v", err)
	})

	t.Logf("operations exercised: %v", h.contract.operations())
}

func httpGet(t *testing.T, url string, status int) string {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, status, resp.StatusCode, string(data))
	return string(data)
}

// TestPolicyDocumentsRoundTrip checks that policies created from text come
// back as JSON documents.
func TestPolicyDocumentsRoundTrip(t *testing.T) {
	h := newHarness(t)
	doc := `{"Version":"2012-10-17","Statement":[{"Sid":"S","Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::x/*"}]}`
	p, err := h.root.SavePolicy(h.ctx, "deny-delete", []byte(doc))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(p.Document, &got))
	assert.Equal(t, "2012-10-17", got["Version"])
}
