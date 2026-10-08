package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func newRouter(t *testing.T) (http.Handler, session.Service) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	users := iam.NewRootOnly(iam.Deps{Clock: clk})
	require.NoError(t, users.Bootstrap(t.Context(), domain.Credentials{AccessKey: "admin", SecretKey: "password123"}))
	sessions, err := session.New(session.Deps{IAM: users, Clock: clk}, session.WithTTL(time.Hour))
	require.NoError(t, err)
	enc := httpx.New()
	adm := adminsvc.New(adminsvc.Deps{
		Clock: clk, Build: adminsvc.BuildInfo{Version: "v-test", Commit: "c0ffee"}, Mode: domain.ModeSNSD, Region: "us-east-1",
		Endpoints: []domain.Endpoint{{Path: "/data", IsLocal: true}},
		Pools:     []domain.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 1}},
	})
	api := NewAPI(enc,
		NewSessionRoutes(SessionDeps{Sessions: sessions, Codec: enc, Clock: clk}),
		NewInfoRoutes(InfoDeps{Admin: adm, Sessions: sessions, Enc: enc}),
		NewUserRoutes(UserDeps{Enc: enc}),
		NewPolicyRoutes(PolicyDeps{Enc: enc}),
		NewBucketRoutes(BucketDeps{Enc: enc}),
		NewConfigRoutes(ConfigDeps{Enc: enc}),
	)
	r := chi.NewRouter()
	api.Register(r)
	return r, sessions
}

func login(t *testing.T, h http.Handler, access, secret string) (*httptest.ResponseRecorder, SessionResponse) {
	t.Helper()
	body := strings.NewReader(`{"accessKey":"` + access + `","secretKey":"` + secret + `"}`)
	req := httptest.NewRequest(http.MethodPost, Prefix+"/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp SessionResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	return namedCookie(t, rec, session.CookieName)
}

func namedCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie", name)
	return nil
}

func TestSessionLoginSessionLogout(t *testing.T) {
	h, _ := newRouter(t)

	rec, resp := login(t, h, "admin", "password123")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "admin", resp.AccessKey)
	assert.NotEmpty(t, resp.ID)
	assert.Equal(t, "admin", resp.Identity.AccessKey)
	assert.True(t, resp.Identity.IsRoot)
	assert.NotNil(t, resp.Identity.Groups, "arrays are never null")
	assert.NotNil(t, resp.Identity.PolicyNames, "arrays are never null")
	assert.False(t, resp.IssuedAt.IsZero())
	assert.True(t, resp.ExpiresAt.After(resp.IssuedAt))
	assert.NotEmpty(t, resp.CSRFToken)
	cookie := sessionCookie(t, rec)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	assert.False(t, cookie.Secure, "plain HTTP request → no Secure flag")
	assert.NotEmpty(t, cookie.Value)
	csrfCookie := namedCookie(t, rec, session.CSRFCookieName)
	assert.False(t, csrfCookie.HttpOnly, "the console scripts read the CSRF cookie")
	assert.Equal(t, resp.CSRFToken, csrfCookie.Value)

	// GET /auth/session with the cookie.
	req := httptest.NewRequest(http.MethodGet, Prefix+"/auth/session", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var cur SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cur))
	assert.Equal(t, "admin", cur.AccessKey)
	assert.Equal(t, resp.ID, cur.ID)
	assert.Equal(t, resp.CSRFToken, cur.CSRFToken, "a reloaded page recovers the csrf token")

	// Without cookie → 401 problem+json.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/auth/session", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))

	// Logout requires the CSRF header.
	req = httptest.NewRequest(http.MethodPost, Prefix+"/auth/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	req = httptest.NewRequest(http.MethodPost, Prefix+"/auth/logout", nil)
	req.AddCookie(cookie)
	req.Header.Set(session.CSRFHeader, resp.CSRFToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	cleared := sessionCookie(t, rec)
	assert.Empty(t, cleared.Value)
	assert.Equal(t, -1, cleared.MaxAge)
	clearedCSRF := namedCookie(t, rec, session.CSRFCookieName)
	assert.Empty(t, clearedCSRF.Value)
	assert.Equal(t, -1, clearedCSRF.MaxAge)

	// The session is revoked.
	req = httptest.NewRequest(http.MethodGet, Prefix+"/auth/session", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Logout without cookie.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, Prefix+"/auth/logout", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSessionLoginFailures(t *testing.T) {
	h, _ := newRouter(t)
	rec, _ := login(t, h, "admin", "wrong")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	var p httpx.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.Equal(t, "invalid_credentials", p.Code)

	req := httptest.NewRequest(http.MethodPost, Prefix+"/auth/login", strings.NewReader("{bad json"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInfoRoutes(t *testing.T) {
	h, _ := newRouter(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/info", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "info requires a session")

	loginRec, _ := login(t, h, "admin", "password123")
	cookie := sessionCookie(t, loginRec)
	req := httptest.NewRequest(http.MethodGet, Prefix+"/info", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var info ServerInfoResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	assert.Equal(t, "v-test", info.Version)
	assert.Equal(t, "snsd", info.Mode)
	assert.Equal(t, "us-east-1", info.Region)
	require.Len(t, info.Nodes, 1)
	assert.Equal(t, "c0ffee", info.Nodes[0].Commit)
	require.Len(t, info.Nodes[0].Disks, 1)
	assert.Equal(t, "/data", info.Nodes[0].Disks[0].Path)
	require.Len(t, info.Pools, 1)
	assert.Equal(t, 1, info.Pools[0].DrivesPerSet)
	// The wire format is camelCase and arrays are never null.
	assert.Contains(t, rec.Body.String(), `"drivesPerSet":1`)
	assert.Contains(t, rec.Body.String(), `"poolIdx":0`)
	assert.Contains(t, rec.Body.String(), `"deploymentId":""`)

	req = httptest.NewRequest(http.MethodGet, Prefix+"/usage", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)

	// Open variant (no session service).
	open := chi.NewRouter()
	NewAPI(nil, NewInfoRoutes(InfoDeps{Admin: adminsvc.New(adminsvc.Deps{})})).Register(open)
	rec = httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/info", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestStubRoutesAndErrors(t *testing.T) {
	h, _ := newRouter(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/users"}, {"POST", "/users"}, {"DELETE", "/users/bob"},
		{"GET", "/groups"}, {"PUT", "/groups/dev"},
		{"GET", "/access-keys"},
		{"GET", "/policies"}, {"POST", "/policies/attach"},
		{"GET", "/buckets"}, {"PUT", "/buckets/b/quota"}, {"GET", "/buckets/b/objects"},
		{"GET", "/config"}, {"PUT", "/config/scanner"}, {"GET", "/config/export"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, Prefix+tc.path, nil))
		assert.Equal(t, http.StatusNotImplemented, rec.Code, "%s %s", tc.method, tc.path)
		assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"), "%s %s", tc.method, tc.path)
		var p httpx.Problem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		assert.Equal(t, "not_implemented", p.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Prefix+"/nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, Prefix+"/info", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))
}
