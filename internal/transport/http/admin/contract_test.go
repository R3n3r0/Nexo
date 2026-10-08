package admin_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// specPath is the OpenAPI document of the Admin API.
const specPath = "../../../../api/openapi.yaml"

// loadSpec loads and validates the OpenAPI document.
func loadSpec(t testing.TB) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specPath)
	require.NoError(t, err)
	require.NoError(t, doc.Validate(context.Background()))
	return doc
}

// contractTransport validates every exchange against the OpenAPI
// document.
type contractTransport struct {
	t      testing.TB
	router routers.Router
	next   http.RoundTripper

	mu  sync.Mutex
	ops map[string]bool
}

func newContractTransport(t testing.TB, consoleURL string) *contractTransport {
	t.Helper()
	doc := loadSpec(t)
	doc.Servers = openapi3.Servers{{URL: consoleURL + admin.Prefix}}
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	return &contractTransport{t: t, router: router, next: http.DefaultTransport, ops: map[string]bool{}}
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mt == "application/json" || mt == "application/problem+json")
}

// RoundTrip implements http.RoundTripper.
func (c *contractTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	route, params, err := c.router.FindRoute(req)
	if err != nil {
		c.t.Errorf("contract: %s %s: no operation in the document: %v", req.Method, req.URL.Path, err)
		return c.next.RoundTrip(req)
	}
	c.mu.Lock()
	c.ops[route.Operation.OperationID] = true
	c.mu.Unlock()
	validateBody := isJSON(req.Header.Get("Content-Type"))
	var body []byte
	if validateBody && req.Body != nil {
		if body, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	check := req.Clone(req.Context())
	check.Body = io.NopCloser(bytes.NewReader(body))
	in := &openapi3filter.RequestValidationInput{
		Request: check, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: !validateBody, MultiError: true},
	}
	if err := openapi3filter.ValidateRequest(req.Context(), in); err != nil {
		c.t.Errorf("contract: request %s %s (%s): %v", req.Method, req.URL.Path, route.Operation.OperationID, err)
	}
	resp, err := c.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: resp.StatusCode, Header: resp.Header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true, ExcludeResponseBody: !isJSON(resp.Header.Get("Content-Type"))},
	}
	out.SetBodyBytes(data)
	if err := openapi3filter.ValidateResponse(req.Context(), out); err != nil {
		c.t.Errorf("contract: response %d of %s %s (%s): %v\n%s", resp.StatusCode, req.Method, req.URL.Path, route.Operation.OperationID, err, data)
	}
	return resp, nil
}

// console is a browser-like client of the console: a session cookie, the
// CSRF token and contract validation.
type console struct {
	t        *testing.T
	base     string
	http     *http.Client
	csrf     string
	contract *contractTransport
}

// newConsole logs in through POST /auth/login.
func newConsole(t *testing.T, srv *testutil.Server, accessKey, secretKey string) *console {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	ct := newContractTransport(t, srv.ConsoleURL)
	c := &console{t: t, base: srv.ConsoleURL + admin.Prefix, http: &http.Client{Jar: jar, Transport: ct, Timeout: 60 * time.Second}, contract: ct}
	resp := c.call(http.MethodPost, "/auth/login", map[string]string{"accessKey": accessKey, "secretKey": secretKey}, http.StatusOK)
	var sess admin.SessionResponse
	require.NoError(t, json.Unmarshal(resp, &sess))
	c.csrf = sess.CSRFToken
	return c
}

// request sends a request; body is JSON unless it is an io.Reader.
func (c *console) request(method, path string, body any, header http.Header) *http.Response {
	c.t.Helper()
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd = b
	default:
		data, err := json.Marshal(b)
		require.NoError(c.t, err)
		rd, ctype = bytes.NewReader(data), "application/json"
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	require.NoError(c.t, err)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if c.csrf != "" && req.Header.Get(session.CSRFHeader) == "" {
		req.Header.Set(session.CSRFHeader, c.csrf)
	}
	resp, err := c.http.Do(req)
	require.NoError(c.t, err)
	return resp
}

// call sends a request and checks the status; it returns the body.
func (c *console) call(method, path string, body any, status int) []byte {
	c.t.Helper()
	resp := c.request(method, path, body, nil)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	require.Equal(c.t, status, resp.StatusCode, "%s %s: %s", method, path, data)
	return data
}

func decodeAs[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(data, &v), string(data))
	return v
}

func q(key string) string { return "?key=" + url.QueryEscape(key) }

// TestConsoleContract exercises the console flows (session cookie and
// CSRF token) against a real server, every exchange validated against
// api/openapi.yaml.
func TestConsoleContract(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)

	sess := decodeAs[admin.SessionResponse](t, c.call(http.MethodGet, "/auth/session", nil, http.StatusOK))
	assert.True(t, sess.Identity.IsRoot)
	assert.True(t, sess.ExpiresAt.After(time.Now()))
	perms := decodeAs[admin.PermissionsResponse](t, c.call(http.MethodGet, "/me/permissions", nil, http.StatusOK))
	assert.Empty(t, perms.Denied)
	c.call(http.MethodGet, "/info", nil, http.StatusOK)
	c.call(http.MethodGet, "/usage", nil, http.StatusOK)
	c.call(http.MethodGet, "/trace", nil, http.StatusNotImplemented)
	var resp *http.Response

	// CSRF: a mutating request without the token is refused.
	resp = c.request(http.MethodPost, "/buckets", map[string]string{"name": "nocsrf"}, http.Header{session.CSRFHeader: {"wrong"}})
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	c.call(http.MethodPost, "/buckets", map[string]any{"name": "docs", "versioning": true}, http.StatusCreated)
	c.call(http.MethodGet, "/buckets/docs", nil, http.StatusOK)
	// Missing configurations are empty values, not errors.
	c.call(http.MethodGet, "/buckets/docs/objectlock", nil, http.StatusOK)
	c.call(http.MethodGet, "/buckets/docs/tags", nil, http.StatusOK)
	c.call(http.MethodPut, "/buckets/docs/encryption", map[string]string{"algorithm": "AES256"}, http.StatusOK)
	c.call(http.MethodGet, "/buckets/docs/encryption", nil, http.StatusOK)
	c.call(http.MethodDelete, "/buckets/docs/encryption", nil, http.StatusNoContent)
	c.call(http.MethodGet, "/buckets/docs/quota", nil, http.StatusOK)
	c.call(http.MethodGet, "/buckets/docs/lifecycle", nil, http.StatusNotImplemented)

	// Upload in one request: the Content-Type is preserved.
	resp = c.request(http.MethodPost, "/buckets/docs/objects/upload"+q("dir/a b#1.txt"), strings.NewReader("hello world"), http.Header{"Content-Type": {"text/plain"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var obj admin.ObjectResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&obj))
	_ = resp.Body.Close()
	assert.Equal(t, "dir/a b#1.txt", obj.Key)
	assert.Equal(t, "text/plain", obj.ContentType)
	resp = c.request(http.MethodPost, "/buckets/docs/objects/upload"+q("dir/page.html"), strings.NewReader("<script>alert(1)</script>"), http.Header{"Content-Type": {"text/html"}})
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	folder := decodeAs[admin.ObjectResponse](t, c.call(http.MethodPost, "/buckets/docs/objects/folder", map[string]string{"prefix": "empty"}, http.StatusCreated))
	assert.Equal(t, "empty/", folder.Key)
	list := decodeAs[admin.ObjectListResponse](t, c.call(http.MethodGet, "/buckets/docs/objects", nil, http.StatusOK))
	assert.Equal(t, []string{"dir/", "empty/"}, list.Prefixes)
	inside := decodeAs[admin.ObjectListResponse](t, c.call(http.MethodGet, "/buckets/docs/objects?prefix=empty/", nil, http.StatusOK))
	assert.Empty(t, inside.Objects, "the folder placeholder is not listed inside the folder")

	stat := decodeAs[admin.ObjectDetailsResponse](t, c.call(http.MethodGet, "/buckets/docs/objects/stat"+q("dir/a b#1.txt"), nil, http.StatusOK))
	assert.Equal(t, int64(11), stat.Size)
	require.NotNil(t, stat.Versions)
	assert.Len(t, *stat.Versions, 1)
	c.call(http.MethodPut, "/buckets/docs/objects/tags"+q("dir/a b#1.txt"), map[string]any{"tags": map[string]string{"k": "v"}}, http.StatusOK)
	versions := decodeAs[admin.VersionListResponse](t, c.call(http.MethodGet, "/buckets/docs/objects/versions"+q("dir/a b#1.txt"), nil, http.StatusOK))
	require.Len(t, versions.Items, 1)

	// Download with a range.
	resp = c.request(http.MethodGet, "/buckets/docs/objects/download"+q("dir/a b#1.txt"), nil, http.Header{"Range": {"bytes=0-4"}})
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, "hello", string(data))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")

	// Previews: text inline and sandboxed, HTML as an attachment.
	resp = c.request(http.MethodGet, "/buckets/docs/objects/preview"+q("dir/a b#1.txt"), nil, nil)
	_ = resp.Body.Close()
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "sandbox")
	resp = c.request(http.MethodGet, "/buckets/docs/objects/preview"+q("dir/page.html"), nil, nil)
	_ = resp.Body.Close()
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment"))
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))

	// Rename a prefix, then delete recursively.
	ren := decodeAs[admin.RenameResponse](t, c.call(http.MethodPost, "/buckets/docs/objects/rename",
		map[string]any{"source": "dir/", "destination": "moved/", "prefix": true}, http.StatusOK))
	assert.Len(t, ren.Renamed, 2)
	assert.Empty(t, ren.Errors)
	del := decodeAs[admin.DeleteObjectsResponse](t, c.call(http.MethodDelete, "/buckets/docs/objects",
		map[string]any{"prefixes": []string{"moved/"}, "objects": []map[string]string{{"key": "empty/"}}}, http.StatusOK))
	assert.Len(t, del.Deleted, 3)

	// Share links from the object browser.
	c.call(http.MethodPost, "/buckets/docs/objects/upload"+q("s.txt"), strings.NewReader("share me"), http.StatusCreated)
	share := decodeAs[admin.ShareResponse](t, c.call(http.MethodPost, "/buckets/docs/objects/share", map[string]any{"key": "s.txt", "expiresIn": 600}, http.StatusCreated))
	assert.Equal(t, "docs", share.Bucket)
	shares := decodeAs[admin.ShareListResponse](t, c.call(http.MethodGet, "/shares", nil, http.StatusOK))
	assert.Len(t, shares.Items, 1)
	c.call(http.MethodDelete, "/shares/"+share.ID, nil, http.StatusNoContent)
	c.call(http.MethodGet, "/shares/"+share.ID, nil, http.StatusNotFound)

	// Anonymous access.
	c.call(http.MethodPut, "/buckets/docs/access", map[string]string{"prefix": "pub/", "preset": "download"}, http.StatusOK)
	acc := decodeAs[admin.BucketAccessResponse](t, c.call(http.MethodDelete, "/buckets/docs/access?prefix=pub/", nil, http.StatusOK))
	assert.Empty(t, acc.Rules)

	// Configuration and schema.
	c.call(http.MethodGet, "/config", nil, http.StatusOK)
	c.call(http.MethodGet, "/config/schema", nil, http.StatusOK)
	c.call(http.MethodPut, "/config/browser", map[string]any{"settings": map[string]string{"session_duration": "1h"}}, http.StatusOK)

	// Unknown endpoints and errors are problem documents.
	c.call(http.MethodGet, "/buckets/nope", nil, http.StatusNotFound)

	c.call(http.MethodDelete, "/buckets/docs?force=true", nil, http.StatusNoContent)
	c.call(http.MethodPost, "/auth/logout", nil, http.StatusNoContent)
	c.call(http.MethodGet, "/auth/session", nil, http.StatusUnauthorized)
}

// TestZipStreaming archives 1000 objects of a folder: the archive is a
// valid zip with the expected relative names and contents.
func TestZipStreaming(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "zips"}, http.StatusCreated)
	const n = 1000
	for i := range n {
		key := fmt.Sprintf("logs/day-%02d/file-%04d.txt", i%10, i)
		c.call(http.MethodPost, "/buckets/zips/objects/upload"+q(key), strings.NewReader(fmt.Sprintf("content of %d", i)), http.StatusCreated)
	}
	c.call(http.MethodPost, "/buckets/zips/objects/upload"+q("other.txt"), strings.NewReader("other"), http.StatusCreated)

	resp := c.request(http.MethodPost, "/buckets/zips/objects/download-zip", map[string]any{"base": "logs/", "prefixes": []string{"logs/"}, "keys": []string{"other.txt"}}, nil)
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(data))
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), `filename=logs.zip`)

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	require.Len(t, zr.File, n+1)
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		assert.False(t, strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, ".."), "safe name %q", f.Name)
	}
	assert.True(t, names["other.txt"], "a key outside the base keeps its path")
	for i := range n {
		name := fmt.Sprintf("day-%02d/file-%04d.txt", i%10, i)
		require.True(t, names[name], "entry %s", name)
	}
	f, err := zr.Open("day-07/file-0007.txt")
	require.NoError(t, err)
	content, err := io.ReadAll(f)
	require.NoError(t, err)
	_ = f.Close()
	assert.Equal(t, "content of 7", string(content))
}

// TestPreviewSVGIsAttachment covers docs/06 TD-S-04: an SVG never renders
// in the console origin.
func TestPreviewSVGIsAttachment(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "img"}, http.StatusCreated)
	svg := `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(document.cookie)"><script>alert(1)</script></svg>`
	resp := c.request(http.MethodPost, "/buckets/img/objects/upload"+q("evil.svg"), strings.NewReader(svg), http.Header{"Content-Type": {"image/svg+xml"}})
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	png := []byte("\x89PNG\r\n\x1a\nfake")
	resp = c.request(http.MethodPost, "/buckets/img/objects/upload"+q("ok.png"), bytes.NewReader(png), http.Header{"Content-Type": {"image/png"}})
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	tests := []struct {
		key, disposition, contentType string
	}{
		{"evil.svg", "attachment", "application/octet-stream"},
		{"ok.png", "inline", "image/png"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			resp := c.request(http.MethodGet, "/buckets/img/objects/preview"+q(tt.key), nil, nil)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Disposition"), tt.disposition), resp.Header.Get("Content-Disposition"))
			assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))
			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
		})
	}
}

// TestProxiedMultipartUpload uploads 20 MiB through the console in parts
// (the browser never holds the secret key) and downloads it back.
func TestProxiedMultipartUpload(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "big"}, http.StatusCreated)
	payload := make([]byte, 20<<20)
	_, err := rand.Read(payload)
	require.NoError(t, err)
	key := "videos/clip 1.bin"

	up := decodeAs[admin.UploadResponse](t, c.call(http.MethodPost, "/buckets/big/uploads"+q(key), map[string]string{"contentType": "video/mp4"}, http.StatusCreated))
	require.NotEmpty(t, up.UploadID)
	assert.Equal(t, int64(5<<20), up.MinPartSize)
	pending := decodeAs[admin.UploadListResponse](t, c.call(http.MethodGet, "/buckets/big/uploads", nil, http.StatusOK))
	require.Len(t, pending.Items, 1)

	const partSize = 8 << 20
	var parts []admin.CompletePartRequest
	for n, off := 1, 0; off < len(payload); n, off = n+1, off+partSize {
		end := min(off+partSize, len(payload))
		path := fmt.Sprintf("/buckets/big/uploads/%s/parts/%d%s", up.UploadID, n, q(key))
		part := decodeAs[admin.PartResponse](t, c.call(http.MethodPut, path, bytes.NewReader(payload[off:end]), http.StatusOK))
		assert.Equal(t, int64(end-off), part.Size)
		parts = append(parts, admin.CompletePartRequest{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	listed := decodeAs[admin.PartListResponse](t, c.call(http.MethodGet, "/buckets/big/uploads/"+up.UploadID+"/parts"+q(key), nil, http.StatusOK))
	assert.Len(t, listed.Items, 3)
	obj := decodeAs[admin.ObjectResponse](t, c.call(http.MethodPost, "/buckets/big/uploads/"+up.UploadID+"/complete"+q(key), admin.CompleteUploadRequest{Parts: parts}, http.StatusOK))
	assert.Equal(t, int64(len(payload)), obj.Size)
	assert.Equal(t, "video/mp4", obj.ContentType)

	resp := c.request(http.MethodGet, "/buckets/big/objects/download"+q(key), nil, nil)
	h := sha256.New()
	_, err = io.Copy(h, resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	want := sha256.Sum256(payload)
	assert.Equal(t, want[:], h.Sum(nil), "the downloaded object is the uploaded one")

	// An aborted upload disappears.
	other := decodeAs[admin.UploadResponse](t, c.call(http.MethodPost, "/buckets/big/uploads"+q("aborted.bin"), nil, http.StatusCreated))
	c.call(http.MethodDelete, "/buckets/big/uploads/"+other.UploadID+q("aborted.bin"), nil, http.StatusNoContent)
	pending = decodeAs[admin.UploadListResponse](t, c.call(http.MethodGet, "/buckets/big/uploads", nil, http.StatusOK))
	assert.Empty(t, pending.Items)
}

// TestUploadLimit refuses a single-request upload over the limit.
func TestUploadLimit(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "limits"}, http.StatusCreated)
	body := bytes.NewReader(make([]byte, admin.DefaultMaxUploadSize+1))
	data := c.call(http.MethodPost, "/buckets/limits/objects/upload"+q("too-big.bin"), body, http.StatusRequestEntityTooLarge)
	assert.Contains(t, string(data), "entity_too_large")
	c.call(http.MethodGet, "/buckets/limits/objects/stat"+q("too-big.bin"), nil, http.StatusNotFound)
}

// TestRestrictedUserConsole checks the authorization of a console user
// without admin actions: no user management, own access keys only.
func TestRestrictedUserConsole(t *testing.T) {
	srv := testutil.StartServer(t)
	root := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	root.call(http.MethodPost, "/buckets", map[string]any{"name": "team"}, http.StatusCreated)
	root.call(http.MethodPost, "/users", map[string]any{"accessKey": "carol", "secretKey": "carol-secret", "policyNames": []string{"readonly"}}, http.StatusCreated)

	carol := newConsole(t, srv, "carol", "carol-secret")
	data := carol.call(http.MethodGet, "/users", nil, http.StatusForbidden)
	assert.Contains(t, string(data), "access_denied")
	carol.call(http.MethodPost, "/buckets", map[string]any{"name": "nope"}, http.StatusForbidden)
	carol.call(http.MethodPost, "/buckets/team/objects/upload"+q("x.txt"), strings.NewReader("x"), http.StatusForbidden)
	buckets := decodeAs[admin.BucketListResponse](t, carol.call(http.MethodGet, "/buckets", nil, http.StatusOK))
	assert.Len(t, buckets.Items, 1, "readonly may list every bucket")
	key := decodeAs[admin.AccessKeyResponse](t, carol.call(http.MethodPost, "/access-keys", map[string]string{"name": "mine"}, http.StatusCreated))
	assert.Equal(t, "carol", key.ParentUser)
	carol.call(http.MethodDelete, "/access-keys/"+key.AccessKey, nil, http.StatusNoContent)
	perms := decodeAs[admin.PermissionsResponse](t, carol.call(http.MethodGet, "/me/permissions?bucket=team", nil, http.StatusOK))
	assert.Contains(t, perms.Allowed, "s3:GetObject")
	assert.Contains(t, perms.Denied, "s3:PutObject")
	carol.call(http.MethodGet, "/me/permissions?user="+srv.RootUser, nil, http.StatusForbidden)
}

// TestRouteTableMatchesSpec checks that every route of the Admin API is
// documented with its actions, and every documented operation is served.
func TestRouteTableMatchesSpec(t *testing.T) {
	doc := loadSpec(t)
	f := newFixture(t)
	routes := admin.RouteSpecs(f.groups...)
	catalog := map[string]bool{}
	for _, a := range f.catalog {
		catalog[string(a)] = true
	}
	served := map[string]bool{"POST /auth/login": true}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Pattern
		served[key] = true
		item := doc.Paths.Find(rt.Pattern)
		require.NotNil(t, item, "route %s is not documented", key)
		op := item.GetOperation(rt.Method)
		require.NotNil(t, op, "route %s is not documented", key)
		var documented []string
		raw, ok := op.Extensions["x-nexo-actions"]
		require.True(t, ok, "%s has no x-nexo-actions", key)
		for _, a := range raw.([]any) {
			documented = append(documented, a.(string))
		}
		var declared []string
		for _, a := range rt.Actions {
			declared = append(declared, string(a))
			assert.True(t, catalog[string(a)], "%s declares %s, which the permission catalog lacks", key, a)
		}
		sort.Strings(documented)
		sort.Strings(declared)
		assert.Equal(t, declared, documented, "x-nexo-actions of %s", key)
		_, byService := op.Extensions["x-nexo-authorized-by"]
		assert.Equal(t, rt.ByService && len(rt.Actions) > 0, byService, "x-nexo-authorized-by of %s", key)
	}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			assert.True(t, served[method+" "+path], "documented operation %s %s is not served", method, path)
		}
	}
}
