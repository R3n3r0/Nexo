package admin_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func TestSessionLoginSessionLogout(t *testing.T) {
	f := newFixture(t)

	rec, resp := f.login(t, rootKey, rootSecret)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, rootKey, resp.AccessKey)
	assert.NotEmpty(t, resp.ID)
	assert.True(t, resp.Identity.IsRoot)
	assert.NotNil(t, resp.Identity.Groups, "arrays are never null")
	assert.NotNil(t, resp.Identity.PolicyNames, "arrays are never null")
	assert.True(t, resp.ExpiresAt.After(resp.IssuedAt))
	assert.NotEmpty(t, resp.CSRFToken)
	cookie := namedCookie(t, rec, session.CookieName)
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	assert.False(t, cookie.Secure, "plain HTTP request: no Secure flag")
	csrfCookie := namedCookie(t, rec, session.CSRFCookieName)
	assert.False(t, csrfCookie.HttpOnly, "the console scripts read the CSRF cookie")
	assert.Equal(t, resp.CSRFToken, csrfCookie.Value)

	c := &client{t: t, f: f, cookie: cookie, csrf: resp.CSRFToken}
	rec = c.do(http.MethodGet, "/auth/session", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	cur := decode[admin.SessionResponse](t, rec)
	assert.Equal(t, resp.ID, cur.ID)
	assert.Equal(t, resp.CSRFToken, cur.CSRFToken, "a reloaded page recovers the csrf token")

	bob := f.session(t, bobKey, bobSecret)
	rec = bob.do(http.MethodGet, "/auth/session", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	bs := decode[admin.SessionResponse](t, rec)
	assert.Equal(t, []string{"readonly"}, bs.Identity.PolicyNames, "the effective policies")
	assert.Equal(t, []string{"staff"}, bs.Identity.Groups)

	// Logout requires the CSRF header.
	req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/logout", nil)
	req.AddCookie(cookie)
	rec = c.serve(req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "invalid_csrf_token", problemOf(t, rec).Code)

	rec = c.do(http.MethodPost, "/auth/logout", nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	cleared := namedCookie(t, rec, session.CookieName)
	assert.Empty(t, cleared.Value)
	assert.Equal(t, -1, cleared.MaxAge)
	assert.Equal(t, -1, namedCookie(t, rec, session.CSRFCookieName).MaxAge)
	rec = c.do(http.MethodGet, "/auth/session", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "the session is revoked")
}

func TestSessionLoginFailures(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name, body, code string
		status           int
	}{
		{"wrong secret", `{"accessKey":"admin","secretKey":"wrong"}`, "invalid_credentials", http.StatusUnauthorized},
		{"unknown user", `{"accessKey":"nobody","secretKey":"x"}`, "invalid_credentials", http.StatusUnauthorized},
		{"bad json", `{bad json`, "invalid_argument", http.StatusBadRequest},
		{"unknown field", `{"accessKey":"admin","secretKey":"x","extra":1}`, "invalid_argument", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/login", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			assert.Equal(t, tt.status, rec.Code)
			assert.Equal(t, tt.code, problemOf(t, rec).Code)
		})
	}
}

// TestSessionLoginRefusesForgeableRequests: a page of another site cannot
// sign the victim in to the account of the attacker (login CSRF). A form
// cannot send application/json, and a browser fetch must not be
// cross-site; no cookie is set (regression: a text/plain form logged in).
func TestSessionLoginRefusesForgeableRequests(t *testing.T) {
	f := newFixture(t)
	body := `{"accessKey":"` + rootKey + `","secretKey":"` + rootSecret + `"}`
	tests := []struct {
		name    string
		headers map[string]string
		status  int
		code    string
	}{
		{name: "text/plain form", headers: map[string]string{"Content-Type": "text/plain"}, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "urlencoded form", headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "multipart form", headers: map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "no content type", status: http.StatusUnsupportedMediaType, code: "unsupported_media_type"},
		{name: "cross-site fetch", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, status: http.StatusForbidden, code: "cross_site_request"},
		{name: "same-site fetch", headers: map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "same-site"}, status: http.StatusForbidden, code: "cross_site_request"},
		{name: "console fetch", headers: map[string]string{"Content-Type": "application/json; charset=utf-8", "Sec-Fetch-Site": "same-origin"}, status: http.StatusOK},
		{name: "non-browser client", headers: map[string]string{"Content-Type": "application/json"}, status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/login", strings.NewReader(body))
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.code == "" {
				return
			}
			assert.Equal(t, tt.code, problemOf(t, rec).Code)
			assert.Empty(t, rec.Result().Cookies(), "no session cookie")
		})
	}
}

// TestSessionLoginThrottle: after session.DefaultMaxFailures failed logins
// of a client on an access key, the next attempts answer 429 with
// Retry-After, even with the right secret; other clients may still sign in.
func TestSessionLoginThrottle(t *testing.T) {
	f := newFixture(t)
	login := func(remote, secret string) *httptest.ResponseRecorder {
		body := `{"accessKey":"` + rootKey + `","secretKey":"` + secret + `"}`
		req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec
	}
	for range session.DefaultMaxFailures {
		rec := login("198.51.100.7:4000", "wrong")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "invalid_credentials", problemOf(t, rec).Code)
	}
	rec := login("198.51.100.7:4001", rootSecret)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "too_many_requests", problemOf(t, rec).Code)
	assert.Equal(t, "60", rec.Header().Get("Retry-After"))

	rec = login("198.51.100.8:4000", rootSecret)
	assert.Equal(t, http.StatusOK, rec.Code, "another client is not locked out")
}

// TestAuthentication covers the two ways in: the console session (with
// CSRF on mutating requests) and SigV4.
func TestAuthentication(t *testing.T) {
	f := newFixture(t)
	f.adm.EXPECT().ServerInfo(mock.Anything).Return(&domain.ServerInfo{Mode: domain.ModeSNSD}, nil).Maybe()
	f.buckets.EXPECT().Create(mock.Anything, mock.Anything).Return(&adminsvc.BucketSummary{Info: domain.BucketInfo{Bucket: domain.Bucket{Name: "b1"}}}, nil).Maybe()
	root := f.root(t)
	noCSRF := &client{t: t, f: f, cookie: root.cookie}
	tests := []struct {
		name   string
		req    func() *http.Request
		status int
		code   string
	}{
		{"no credentials", func() *http.Request { return httptest.NewRequest(http.MethodGet, admin.Prefix+"/info", nil) }, http.StatusUnauthorized, "unauthenticated"},
		{"forged cookie", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, admin.Prefix+"/info", nil)
			r.AddCookie(&http.Cookie{Name: session.CookieName, Value: "forged"})
			return r
		}, http.StatusUnauthorized, "unauthenticated"},
		{"session read without csrf", func() *http.Request { return noCSRF.newRequest(http.MethodGet, "/info", nil) }, http.StatusOK, ""},
		{"session write without csrf", func() *http.Request { return noCSRF.newRequest(http.MethodPost, "/buckets", `{"name":"b1"}`) }, http.StatusForbidden, "invalid_csrf_token"},
		{"session write with csrf", func() *http.Request { return root.newRequest(http.MethodPost, "/buckets", `{"name":"b1"}`) }, http.StatusCreated, ""},
		{"sigv4 read", func() *http.Request { return f.signed(t, rootKey, rootSecret).newRequest(http.MethodGet, "/info", nil) }, http.StatusOK, ""},
		{"sigv4 write without csrf", func() *http.Request {
			return f.signed(t, rootKey, rootSecret).newRequest(http.MethodPost, "/buckets", `{"name":"b1"}`)
		}, http.StatusCreated, ""},
		{"sigv4 wrong secret", func() *http.Request {
			return f.signed(t, rootKey, "not-the-secret").newRequest(http.MethodGet, "/info", nil)
		},
			http.StatusUnauthorized, "signature_mismatch"},
		{"sigv4 unknown key", func() *http.Request {
			return f.signed(t, "ghost", "ghost-secret").newRequest(http.MethodGet, "/info", nil)
		},
			http.StatusUnauthorized, "invalid_access_key"},
		{"sigv4 tampered body", func() *http.Request {
			r := f.signed(t, rootKey, rootSecret).newRequest(http.MethodPost, "/buckets", `{"name":"b1"}`)
			r.Body = httptestBody(`{"name":"b2"}`)
			return r
		}, http.StatusBadRequest, ""},
		{"basic auth", func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, admin.Prefix+"/info", nil)
			r.SetBasicAuth(rootKey, rootSecret)
			return r
		}, http.StatusUnauthorized, "unauthenticated"},
		{"presigned query", func() *http.Request {
			return httptest.NewRequest(http.MethodGet, admin.Prefix+"/info?X-Amz-Credential=a&X-Amz-Signature=b", nil)
		}, http.StatusUnauthorized, "unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, tt.req())
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.code != "" {
				assert.Equal(t, tt.code, problemOf(t, rec).Code)
			}
		})
	}
	// The signed identity reaches the authorizer.
	f.resetChecks()
	rec := f.signed(t, bobKey, bobSecret).do(http.MethodGet, "/info", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	in := f.lastCheck()
	assert.Equal(t, bobKey, in.Requester.Identity.AccessKey)
	assert.Equal(t, domain.AuthTypeHeader, in.Requester.Identity.AuthType)
	assert.Equal(t, []policy.Action{policy.ActionAdminServerInfo}, in.Actions)
	assert.NotEmpty(t, in.Requester.Conditions["aws:SourceIp"])
}

// TestAuthorizationDeniedPerGroup sends one request of every route group
// with a denying authorizer: the guard answers 403 access_denied and the
// services are never called (the mocks would fail the test).
func TestAuthorizationDeniedPerGroup(t *testing.T) {
	tests := []struct {
		group, method, path, body string
		action                    policy.Action
	}{
		{"info", http.MethodGet, "/info", "", policy.ActionAdminServerInfo},
		{"info", http.MethodPost, "/service/restart", "", policy.ActionAdminServiceRestart},
		{"session", http.MethodGet, "/me/permissions?user=alice", "", policy.ActionAdminGetUser},
		{"users", http.MethodGet, "/users", "", policy.ActionAdminListUsers},
		{"users", http.MethodDelete, "/users/alice", "", policy.ActionAdminDeleteUser},
		{"groups", http.MethodGet, "/groups/g1", "", policy.ActionAdminGetGroup},
		{"policies", http.MethodPost, "/policies/attach", `{"policyNames":["readonly"],"user":"alice"}`, policy.ActionAdminAttachUserOrGroupPolicy},
		{"policies", http.MethodPost, "/policies/simulate", `{"action":"s3:GetObject"}`, policy.ActionAdminSimulatePolicy},
		{"buckets", http.MethodGet, "/buckets/b1/versioning", "", policy.ActionGetBucketVersioning},
		{"buckets", http.MethodPut, "/buckets/b1/access", `{"preset":"public"}`, policy.ActionPutBucketPolicy},
		{"buckets", http.MethodGet, "/buckets/b1/quota", "", policy.ActionAdminGetBucketQuota},
		{"objects", http.MethodGet, "/buckets/b1/objects/stat?key=k", "", policy.ActionGetObject},
		{"objects", http.MethodGet, "/buckets/b1/objects/download?key=k", "", policy.ActionGetObject},
		{"uploads", http.MethodPost, "/buckets/b1/objects/upload?key=k", "", policy.ActionPutObject},
		{"uploads", http.MethodDelete, "/buckets/b1/uploads/u1?key=k", "", policy.ActionAbortMultipart},
		{"config", http.MethodGet, "/config", "", policy.ActionAdminConfigWrite},
	}
	for _, tt := range tests {
		t.Run(tt.group+" "+tt.method+" "+tt.path, func(t *testing.T) {
			f := newFixture(t)
			c := f.root(t)
			f.denyAll()
			var body any
			if tt.body != "" {
				body = tt.body
			}
			rec := c.do(tt.method, tt.path, body)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			p := problemOf(t, rec)
			assert.Equal(t, "access_denied", p.Code)
			assert.Contains(t, p.Detail, string(tt.action))
			assert.Equal(t, tt.action, f.lastCheck().Actions[0])
		})
	}
}

// TestAuthorizedByService checks that the routes whose resources travel in
// the body pass the requester to their service, which denies.
func TestAuthorizedByService(t *testing.T) {
	denied := fmt.Errorf("%w: s3:PutObject: implicit deny", domain.ErrAccessDenied)
	tests := []struct {
		name, method, path, body string
		expect                   func(f *fixture)
	}{
		{"create bucket", http.MethodPost, "/buckets", `{"name":"b1"}`, func(f *fixture) {
			f.buckets.EXPECT().Create(mock.Anything, mock.MatchedBy(func(in adminsvc.CreateBucketInput) bool { return in.Requester.Identity.AccessKey == bobKey })).Return(nil, denied)
		}},
		{"list objects", http.MethodGet, "/buckets/b1/objects?prefix=private/", "", func(f *fixture) {
			f.browser.EXPECT().List(mock.Anything, mock.MatchedBy(func(in adminsvc.ListObjectsInput) bool { return in.Requester.Identity.AccessKey == bobKey })).
				Return(nil, fmt.Errorf("%w: s3:ListBucket: implicit deny", domain.ErrAccessDenied))
		}},
		{"delete objects", http.MethodDelete, "/buckets/b1/objects", `{"prefixes":["a/"]}`, func(f *fixture) {
			f.browser.EXPECT().Delete(mock.Anything, mock.Anything).Return(nil, denied)
		}},
		{"share", http.MethodPost, "/shares", `{"bucket":"b1","key":"k"}`, func(f *fixture) {
			f.shares.EXPECT().Create(mock.Anything, mock.Anything).Return(nil, denied)
		}},
		{"access keys of another user", http.MethodGet, "/access-keys?user=alice", "", func(f *fixture) {
			f.accessKeys.EXPECT().List(mock.Anything, requester(bobKey), "alice").Return(nil, denied)
		}},
		{"update user", http.MethodPut, "/users/alice", `{"status":"disabled"}`, func(f *fixture) {
			f.identities.EXPECT().UpdateUser(mock.Anything, mock.Anything).Return(nil, denied)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.expect(f)
			var body any
			if tt.body != "" {
				body = tt.body
			}
			rec := f.session(t, bobKey, bobSecret).do(tt.method, tt.path, body)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, "access_denied", problemOf(t, rec).Code)
		})
	}
}

// TestGuardInputs checks the scope, actions and conditions the guard asks.
func TestGuardInputs(t *testing.T) {
	tests := []struct {
		name, method, path string
		expect             func(f *fixture)
		check              func(t *testing.T, in adminsvc.AuthorizeInput)
	}{
		{"bucket scope", http.MethodGet, "/buckets/photos/tags", func(f *fixture) {
			f.buckets.EXPECT().Tags(mock.Anything, "photos").Return(map[string]string{}, nil)
		}, func(t *testing.T, in adminsvc.AuthorizeInput) {
			assert.Equal(t, "photos", in.Bucket)
			assert.Empty(t, in.Object)
			assert.Equal(t, []policy.Action{policy.ActionGetBucketTagging}, in.Actions)
		}},
		{"forced deletion", http.MethodDelete, "/buckets/photos?force=true", func(f *fixture) {
			f.buckets.EXPECT().Delete(mock.Anything, adminsvc.DeleteBucketInput{Name: "photos", Force: true}).Return(nil)
		}, func(t *testing.T, in adminsvc.AuthorizeInput) {
			assert.Equal(t, []policy.Action{policy.ActionDeleteBucket, policy.ActionForceDeleteBucket}, in.Actions)
		}},
		{"object scope with a version", http.MethodGet, "/buckets/photos/objects/stat?key=a%2Fb.txt&versionId=v1", func(f *fixture) {
			f.browser.EXPECT().Stat(mock.Anything, mock.Anything).Return(&adminsvc.ObjectDetails{Info: domain.ObjectInfo{Key: "a/b.txt"}}, nil)
		}, func(t *testing.T, in adminsvc.AuthorizeInput) {
			assert.Equal(t, "photos", in.Bucket)
			assert.Equal(t, "a/b.txt", in.Object)
			assert.Equal(t, []policy.Action{policy.ActionGetObjectVersion}, in.Actions)
			// The condition values of the S3 GetObject?versionId=v1
			// (regression: a Deny on s3:versionid was bypassed).
			assert.Equal(t, map[string][]string{"s3:versionid": {"v1"}}, in.Conditions)
			assert.Equal(t, []string{"REST-HEADER"}, in.Requester.Conditions["s3:authType"])
		}},
		{"download of a version", http.MethodGet, "/buckets/photos/objects/download?key=a.txt&versionId=v1", func(f *fixture) {
			f.browser.EXPECT().Open(mock.Anything, mock.Anything).Return(download("x", "text/plain", adminsvc.DispositionAttachment, false), nil)
		}, func(t *testing.T, in adminsvc.AuthorizeInput) {
			assert.Equal(t, []policy.Action{policy.ActionGetObjectVersion}, in.Actions)
			assert.Equal(t, map[string][]string{"s3:versionid": {"v1"}}, in.Conditions)
		}},
		{"tags of a version", http.MethodPut, "/buckets/photos/objects/tags?key=a.txt&versionId=v1", func(f *fixture) {
			f.browser.EXPECT().SetTags(mock.Anything, mock.Anything).Return(map[string]string{}, nil)
		}, func(t *testing.T, in adminsvc.AuthorizeInput) {
			assert.Equal(t, []policy.Action{policy.ActionPutObjectVersionTagging}, in.Actions)
			assert.Equal(t, map[string][]string{"s3:versionid": {"v1"}}, in.Conditions)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.expect(f)
			c := f.root(t)
			f.resetChecks()
			var body any
			if tt.method == http.MethodPut {
				body = `{"tags":{}}`
			}
			rec := c.do(tt.method, tt.path, body)
			require.Less(t, rec.Code, 300, rec.Body.String())
			tt.check(t, f.lastCheck())
		})
	}
	t.Run("object scope needs a key", func(t *testing.T) {
		f := newFixture(t)
		rec := f.root(t).do(http.MethodGet, "/buckets/photos/objects/download", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "invalid_argument", problemOf(t, rec).Code)
	})
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	rec := c.do(http.MethodGet, "/nope", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", problemOf(t, rec).Code)
	rec = c.do(http.MethodPatch, "/users", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "method_not_allowed", problemOf(t, rec).Code)
}

func TestConstructorsRefuseMissingServices(t *testing.T) {
	_, err := admin.NewInfoRoutes(admin.InfoDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewUserRoutes(admin.UserDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewPolicyRoutes(admin.PolicyDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewBucketRoutes(admin.BucketDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewObjectRoutes(admin.ObjectDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewShareRoutes(admin.ShareDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewConfigRoutes(admin.ConfigDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewSessionRoutes(admin.SessionDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewAuthenticator(admin.AuthenticatorDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestNewAPIRequiresAuthentication: a missing authentication middleware
// is a wiring error, never an open API (docs/12 §12.3); a no-op
// middleware still leaves every request anonymous, which the guard
// refuses.
func TestNewAPIRequiresAuthentication(t *testing.T) {
	f := newFixture(t)
	info, err := admin.NewInfoRoutes(admin.InfoDeps{Admin: f.adm, Authz: f.authz})
	require.NoError(t, err)
	_, err = admin.NewAPI(admin.APIDeps{}, info)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewAPI(admin.APIDeps{Auth: middleware.Func(func(next http.Handler) http.Handler { return next })}, info, nil)
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "a nil group")

	api, err := admin.NewAPI(admin.APIDeps{Auth: middleware.Func(func(next http.Handler) http.Handler { return next })}, info)
	require.NoError(t, err)
	r := chi.NewRouter()
	api.Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, admin.Prefix+"/info", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
