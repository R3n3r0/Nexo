package admin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// fixture is a complete Admin API with a real session service and policy
// engine, mocked IAM/bucket/object services and an in-memory configuration.
type fixture struct {
	handler http.Handler
	iam     *iammock.Service
	buckets *bucketmock.Service
	objects *objectmock.Service
	config  configsvc.Service
	policy  policy.Service
	clk     clock.Clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	users := iammock.NewService(t)
	// Login resolves the root user and a plain user through IAM.
	users.EXPECT().LookupCredentials(mock.Anything, "admin").
		Return(domain.Credentials{AccessKey: "admin", SecretKey: "password123", Status: domain.StatusEnabled}, domain.Identity{AccessKey: "admin", IsRoot: true}, nil).Maybe()
	users.EXPECT().LookupCredentials(mock.Anything, "bob").
		Return(domain.Credentials{AccessKey: "bob", SecretKey: "bobsecret", Status: domain.StatusEnabled}, domain.Identity{AccessKey: "bob"}, nil).Maybe()
	users.EXPECT().LookupCredentials(mock.Anything, mock.Anything).
		Return(domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: unknown", domain.ErrInvalidAccessKey)).Maybe()

	sessions, err := session.New(session.Deps{IAM: users, Clock: clk}, session.WithTTL(time.Hour))
	require.NoError(t, err)
	engine := policy.New(policy.Deps{Clock: clk})
	enc := httpx.New()
	drive := storagemock.NewStorageAPI(t)
	drive.EXPECT().Endpoint().Return(domain.Endpoint{Path: "/data", IsLocal: true}).Maybe()
	drive.EXPECT().DiskInfo(mock.Anything, mock.Anything).Return(domain.DiskInfo{Path: "/data", State: domain.StateOnline}, nil).Maybe()
	drive.EXPECT().Healing().Return(nil).Maybe()
	adm := adminsvc.New(adminsvc.Deps{
		Clock: clk, Build: adminsvc.BuildInfo{Version: "v-test", Commit: "c0ffee"}, Mode: domain.ModeSNSD, Region: "us-east-1",
		Drives: []storage.StorageAPI{drive},
		Pools:  []domain.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 1}},
	})
	buckets := bucketmock.NewService(t)
	objects := objectmock.NewService(t)
	cfg := configsvc.New(configsvc.Deps{Store: configsvc.NewMemoryStore(nil)})

	sessionAuth, err := middleware.NewSessionAuth(middleware.SessionAuthDeps{Sessions: sessions, Enc: enc})
	require.NoError(t, err)
	sessionRoutes, err := admin.NewSessionRoutes(admin.SessionDeps{Sessions: sessions, Codec: enc, Clock: clk})
	require.NoError(t, err)
	infoRoutes, err := admin.NewInfoRoutes(admin.InfoDeps{Admin: adm, Policy: engine, Enc: enc})
	require.NoError(t, err)
	userRoutes, err := admin.NewUserRoutes(admin.UserDeps{IAM: users, Policy: engine, Enc: enc})
	require.NoError(t, err)
	policyRoutes, err := admin.NewPolicyRoutes(admin.PolicyDeps{Policy: engine, Enc: enc})
	require.NoError(t, err)
	bucketRoutes, err := admin.NewBucketRoutes(admin.BucketDeps{Buckets: buckets, Objects: objects, Policy: engine, Enc: enc})
	require.NoError(t, err)
	configRoutes, err := admin.NewConfigRoutes(admin.ConfigDeps{Config: cfg, Policy: engine, Enc: enc})
	require.NoError(t, err)

	api := admin.NewAPI(admin.APIDeps{Enc: enc, Auth: sessionAuth},
		sessionRoutes, infoRoutes, userRoutes, policyRoutes, bucketRoutes, configRoutes)
	r := chi.NewRouter()
	api.Register(r)
	return &fixture{handler: r, iam: users, buckets: buckets, objects: objects, config: cfg, policy: engine, clk: clk}
}

// client performs authenticated requests on behalf of one session.
type client struct {
	t       *testing.T
	h       http.Handler
	cookie  *http.Cookie
	csrf    string
	session admin.SessionResponse
}

func (f *fixture) login(t *testing.T, access, secret string) (*httptest.ResponseRecorder, admin.SessionResponse) {
	t.Helper()
	body := strings.NewReader(`{"accessKey":"` + access + `","secretKey":"` + secret + `"}`)
	req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	var resp admin.SessionResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	}
	return rec, resp
}

func (f *fixture) client(t *testing.T, access, secret string) *client {
	t.Helper()
	rec, resp := f.login(t, access, secret)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return &client{t: t, h: f.handler, cookie: namedCookie(t, rec, session.CookieName), csrf: resp.CSRFToken, session: resp}
}

// do sends method path with an optional JSON body and returns the recorder.
func (c *client) do(method, path, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, admin.Prefix+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(c.cookie)
	req.Header.Set(session.CSRFHeader, c.csrf)
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

func problem(t *testing.T, rec *httptest.ResponseRecorder) httpx.Problem {
	t.Helper()
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))
	return decode[httpx.Problem](t, rec)
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
	f := newFixture(t)

	rec, resp := f.login(t, "admin", "password123")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "admin", resp.AccessKey)
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

	c := &client{t: t, h: f.handler, cookie: cookie, csrf: resp.CSRFToken}
	rec = c.do(http.MethodGet, "/auth/session", "")
	require.Equal(t, http.StatusOK, rec.Code)
	cur := decode[admin.SessionResponse](t, rec)
	assert.Equal(t, resp.ID, cur.ID)
	assert.Equal(t, resp.CSRFToken, cur.CSRFToken, "a reloaded page recovers the csrf token")

	// Without cookie: 401 problem+json from the middleware.
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, admin.Prefix+"/auth/session", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "unauthenticated", problem(t, rec).Code)

	// Logout requires the CSRF header.
	req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "invalid_csrf_token", problem(t, rec).Code)

	rec = c.do(http.MethodPost, "/auth/logout", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	cleared := namedCookie(t, rec, session.CookieName)
	assert.Empty(t, cleared.Value)
	assert.Equal(t, -1, cleared.MaxAge)
	assert.Equal(t, -1, namedCookie(t, rec, session.CSRFCookieName).MaxAge)

	// The session is revoked.
	rec = c.do(http.MethodGet, "/auth/session", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestSessionLoginFailures(t *testing.T) {
	f := newFixture(t)
	rec, _ := f.login(t, "admin", "wrong")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "invalid_credentials", problem(t, rec).Code)

	rec, _ = f.login(t, "nobody", "x")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "invalid_credentials", problem(t, rec).Code)

	req := httptest.NewRequest(http.MethodPost, admin.Prefix+"/auth/login", strings.NewReader("{bad json"))
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid_argument", problem(t, rec).Code)
}

func TestInfoRoutes(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, admin.Prefix+"/info", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "info requires a session")

	c := f.client(t, "admin", "password123")
	rec = c.do(http.MethodGet, "/info", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info := decode[admin.ServerInfoResponse](t, rec)
	assert.Equal(t, "v-test", info.Version)
	assert.Equal(t, "snsd", info.Mode)
	assert.Equal(t, "us-east-1", info.Region)
	require.Len(t, info.Nodes, 1)
	assert.Equal(t, "c0ffee", info.Nodes[0].Commit)
	require.Len(t, info.Nodes[0].Disks, 1)
	assert.Equal(t, "/data", info.Nodes[0].Disks[0].Path)
	require.Len(t, info.Pools, 1)
	// The wire format is camelCase.
	assert.Contains(t, rec.Body.String(), `"drivesPerSet":1`)
	assert.Contains(t, rec.Body.String(), `"deploymentId":""`)

	rec = c.do(http.MethodGet, "/usage", "")
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "no object layer in this fixture")

	rec = c.do(http.MethodPost, "/heal", `{"bucket":"photos","recursive":true}`)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)

	rec = c.do(http.MethodPost, "/service/reboot", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = c.do(http.MethodPost, "/service/restart", "")
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "no action handler configured")
}

func TestOpenAPIWithoutAuth(t *testing.T) {
	// Without an authentication middleware and a policy engine the routes
	// are open: used by smoke tests.
	info, err := admin.NewInfoRoutes(admin.InfoDeps{Admin: adminsvc.New(adminsvc.Deps{})})
	require.NoError(t, err)
	r := chi.NewRouter()
	admin.NewAPI(admin.APIDeps{}, info).Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, admin.Prefix+"/info", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Constructors refuse missing services.
	_, err = admin.NewInfoRoutes(admin.InfoDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewUserRoutes(admin.UserDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewPolicyRoutes(admin.PolicyDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewBucketRoutes(admin.BucketDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewConfigRoutes(admin.ConfigDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewSessionRoutes(admin.SessionDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestAuthorizationDenied(t *testing.T) {
	f := newFixture(t)
	// bob has no policies: the engine denies every admin action.
	bob := f.client(t, "bob", "bobsecret")
	rec := bob.do(http.MethodGet, "/info", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "access_denied", problem(t, rec).Code)
	rec = bob.do(http.MethodGet, "/users", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Attaching readonly gives bob admin:ServerInfo but nothing else.
	require.NoError(t, f.policy.Attach(t.Context(), policy.AttachInput{PolicyNames: []string{policy.PolicyDiagnostics}, User: "bob"}))
	f.iam.ExpectedCalls = nil
	f.iam.EXPECT().LookupCredentials(mock.Anything, "bob").
		Return(domain.Credentials{AccessKey: "bob", SecretKey: "bobsecret", Status: domain.StatusEnabled},
			domain.Identity{AccessKey: "bob", PolicyNames: []string{policy.PolicyDiagnostics}}, nil).Maybe()
	bob = f.client(t, "bob", "bobsecret")
	rec = bob.do(http.MethodGet, "/info", "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = bob.do(http.MethodGet, "/users", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestUserRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.client(t, "admin", "password123")
	now := f.clk.Now()

	f.iam.EXPECT().ListUsers(mock.Anything).Return([]iam.User{{AccessKey: "bob", Status: domain.StatusEnabled, CreatedAt: now}}, nil).Once()
	rec := c.do(http.MethodGet, "/users", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	users := decode[[]admin.UserResponse](t, rec)
	require.Len(t, users, 1)
	assert.Equal(t, "bob", users[0].AccessKey)
	assert.NotNil(t, users[0].PolicyNames)
	assert.NotNil(t, users[0].Groups)

	f.iam.EXPECT().CreateUser(mock.Anything, iam.CreateUserInput{AccessKey: "carol", SecretKey: "carolsecret", Status: domain.StatusEnabled, PolicyNames: []string{"readonly"}}).
		Return(&iam.User{AccessKey: "carol", Status: domain.StatusEnabled, PolicyNames: []string{"readonly"}, CreatedAt: now}, nil).Once()
	rec = c.do(http.MethodPost, "/users", `{"accessKey":"carol","secretKey":"carolsecret","policyNames":["readonly"]}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "carol", decode[admin.UserResponse](t, rec).AccessKey)

	rec = c.do(http.MethodPost, "/users", `{"accessKey":"dave","secretKey":"x","status":"frozen"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = c.do(http.MethodPost, "/users", `{"accessKey":"dave","unknown":1}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "unknown fields are rejected")

	f.iam.EXPECT().GetUser(mock.Anything, "ghost").Return(nil, fmt.Errorf("%w: ghost", domain.ErrUserNotFound)).Once()
	rec = c.do(http.MethodGet, "/users/ghost", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "user_not_found", problem(t, rec).Code)

	f.iam.EXPECT().SetUserStatus(mock.Anything, "bob", domain.StatusDisabled).Return(nil).Once()
	f.iam.EXPECT().SetUserPolicy(mock.Anything, "bob", []string{"readwrite"}).Return(nil).Once()
	f.iam.EXPECT().GetUser(mock.Anything, "bob").Return(&iam.User{AccessKey: "bob", Status: domain.StatusDisabled, PolicyNames: []string{"readwrite"}}, nil).Once()
	rec = c.do(http.MethodPut, "/users/bob", `{"status":"disabled","policyNames":["readwrite"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "disabled", decode[admin.UserResponse](t, rec).Status)

	f.iam.EXPECT().DeleteUser(mock.Anything, "bob").Return(nil).Once()
	rec = c.do(http.MethodDelete, "/users/bob", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Groups.
	f.iam.EXPECT().CreateGroup(mock.Anything, iam.CreateGroupInput{Name: "dev", Members: []string{"bob"}}).
		Return(&iam.Group{Name: "dev", Status: domain.StatusEnabled, Members: []string{"bob"}, CreatedAt: now}, nil).Once()
	rec = c.do(http.MethodPost, "/groups", `{"name":"dev","members":["bob"]}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	g := decode[admin.GroupResponse](t, rec)
	assert.Equal(t, []string{"bob"}, g.Members)
	assert.NotNil(t, g.PolicyNames)

	f.iam.EXPECT().AddGroupMembers(mock.Anything, "dev", []string{"carol"}).Return(nil).Once()
	f.iam.EXPECT().RemoveGroupMembers(mock.Anything, "dev", []string{"bob"}).Return(nil).Once()
	f.iam.EXPECT().GetGroup(mock.Anything, "dev").Return(&iam.Group{Name: "dev", Members: []string{"carol"}}, nil).Once()
	rec = c.do(http.MethodPut, "/groups/dev", `{"addMembers":["carol"],"removeMembers":["bob"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"carol"}, decode[admin.GroupResponse](t, rec).Members)

	f.iam.EXPECT().ListGroups(mock.Anything).Return(nil, nil).Once()
	rec = c.do(http.MethodGet, "/groups", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String(), "empty lists are arrays")

	f.iam.EXPECT().DeleteGroup(mock.Anything, "dev").Return(nil).Once()
	rec = c.do(http.MethodDelete, "/groups/dev", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Access keys: root creates a key for bob and sees the secret once.
	exp := now.Add(24 * time.Hour)
	f.iam.EXPECT().CreateAccessKey(mock.Anything, iam.CreateAccessKeyInput{ParentUser: "bob", Name: "ci", Expiration: exp}).
		Return(&iam.AccessKey{AccessKey: "AKIACI", SecretKey: "s3cr3t", ParentUser: "bob", Name: "ci", Status: domain.StatusEnabled, Expiration: exp, CreatedAt: now}, nil).Once()
	rec = c.do(http.MethodPost, "/access-keys", `{"parentUser":"bob","name":"ci","expiration":"`+exp.Format(time.RFC3339)+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := decode[admin.AccessKeyResponse](t, rec)
	assert.Equal(t, "s3cr3t", created.SecretKey)
	require.NotNil(t, created.Expiration)
	assert.True(t, created.Expiration.Equal(exp))

	f.iam.EXPECT().ListAccessKeys(mock.Anything, "bob").Return([]iam.AccessKey{{AccessKey: "AKIACI", SecretKey: "s3cr3t", ParentUser: "bob"}}, nil).Once()
	rec = c.do(http.MethodGet, "/access-keys?parent=bob", "")
	require.Equal(t, http.StatusOK, rec.Code)
	keys := decode[[]admin.AccessKeyResponse](t, rec)
	require.Len(t, keys, 1)
	assert.Empty(t, keys[0].SecretKey, "the secret is never listed")
	assert.Nil(t, keys[0].Expiration)

	f.iam.EXPECT().GetAccessKey(mock.Anything, "AKIACI").Return(&iam.AccessKey{AccessKey: "AKIACI", ParentUser: "bob"}, nil).Once()
	f.iam.EXPECT().DeleteAccessKey(mock.Anything, "AKIACI").Return(nil).Once()
	rec = c.do(http.MethodDelete, "/access-keys/AKIACI", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAccessKeysOfOtherUsersAreRootOnly(t *testing.T) {
	f := newFixture(t)
	// bob may manage service accounts (consoleAdmin would do, readwrite
	// does not): give him an explicit policy.
	doc := &domain.Policy{Version: "2012-10-17", Statement: domain.Statements{{
		Effect:   domain.Allow,
		Action:   domain.StringSet{string(policy.ActionAdminCreateAccessKey), string(policy.ActionAdminListAccessKeys), string(policy.ActionAdminDeleteAccessKey)},
		Resource: domain.StringSet{"*"},
	}}}
	require.NoError(t, f.policy.SetPolicy(t.Context(), "keys", doc))
	f.iam.ExpectedCalls = nil
	f.iam.EXPECT().LookupCredentials(mock.Anything, "bob").
		Return(domain.Credentials{AccessKey: "bob", SecretKey: "bobsecret", Status: domain.StatusEnabled},
			domain.Identity{AccessKey: "bob", PolicyNames: []string{"keys"}}, nil).Maybe()
	bob := f.client(t, "bob", "bobsecret")

	rec := bob.do(http.MethodPost, "/access-keys", `{"parentUser":"alice"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	f.iam.EXPECT().CreateAccessKey(mock.Anything, iam.CreateAccessKeyInput{ParentUser: "bob"}).
		Return(&iam.AccessKey{AccessKey: "AK1", SecretKey: "s", ParentUser: "bob"}, nil).Once()
	rec = bob.do(http.MethodPost, "/access-keys", `{}`)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	f.iam.EXPECT().ListAccessKeys(mock.Anything, "bob").Return(nil, nil).Once()
	rec = bob.do(http.MethodGet, "/access-keys", "")
	assert.Equal(t, http.StatusOK, rec.Code)

	f.iam.EXPECT().GetAccessKey(mock.Anything, "AK9").Return(&iam.AccessKey{AccessKey: "AK9", ParentUser: "alice"}, nil).Once()
	rec = bob.do(http.MethodDelete, "/access-keys/AK9", "")
	assert.Equal(t, http.StatusForbidden, rec.Code, "bob cannot delete alice's key")
}

func TestPolicyRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.client(t, "admin", "password123")

	rec := c.do(http.MethodGet, "/policies", "")
	require.Equal(t, http.StatusOK, rec.Code)
	all := decode[[]admin.PolicyResponse](t, rec)
	names := make([]string, 0, len(all))
	for _, p := range all {
		names = append(names, p.Name)
		assert.True(t, p.BuiltIn)
		assert.NotNil(t, p.Document)
	}
	assert.Contains(t, names, policy.PolicyConsoleAdmin)
	assert.Contains(t, names, policy.PolicyReadOnly)

	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::photos/*"]}]}`
	rec = c.do(http.MethodPut, "/policies/photos-ro", `{"document":`+doc+`}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	saved := decode[admin.PolicyResponse](t, rec)
	assert.Equal(t, "photos-ro", saved.Name)
	assert.False(t, saved.BuiltIn)
	assert.Equal(t, f.clk.Now(), saved.UpdatedAt)
	require.NotNil(t, saved.Document)
	assert.Equal(t, "2012-10-17", saved.Document.Version)

	rec = c.do(http.MethodPost, "/policies", `{"name":"photos-rw","document":`+doc+`}`)
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	rec = c.do(http.MethodGet, "/policies/photos-ro", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "photos-ro", decode[admin.PolicyResponse](t, rec).Name)

	rec = c.do(http.MethodGet, "/policies/nope", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "policy_not_found", problem(t, rec).Code)

	rec = c.do(http.MethodPut, "/policies/bad", `{"document":{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"s3:*","Resource":"*"}]}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = c.do(http.MethodPut, "/policies/bad", `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = c.do(http.MethodPost, "/policies/attach", `{"policyNames":["photos-ro"],"user":"bob"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	rec = c.do(http.MethodPost, "/policies/attach", `{"policyNames":["photos-ro"],"user":"bob","group":"dev"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = c.do(http.MethodPost, "/policies/attach", `{"policyNames":["ghost"],"group":"dev"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = c.do(http.MethodPost, "/policies/detach", `{"policyNames":["photos-ro"],"user":"bob"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = c.do(http.MethodDelete, "/policies/photos-ro", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = c.do(http.MethodDelete, "/policies/"+policy.PolicyReadOnly, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "built-ins are read-only")
}

func TestBucketRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.client(t, "admin", "password123")
	now := f.clk.Now()
	info := &domain.BucketInfo{
		Bucket:     domain.Bucket{Name: "photos", Created: now, Region: "us-east-1"},
		Versioning: domain.VersioningEnabled, Objects: 3, Size: 1024, QuotaBytes: 1 << 30,
	}

	f.buckets.EXPECT().List(mock.Anything, bucket.ListInput{WithUsage: true}).Return([]domain.BucketInfo{*info}, nil).Once()
	rec := c.do(http.MethodGet, "/buckets", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list := decode[[]admin.BucketResponse](t, rec)
	require.Len(t, list, 1)
	assert.Equal(t, "enabled", list[0].Versioning)
	assert.Equal(t, uint64(1<<30), list[0].QuotaBytes)
	assert.NotNil(t, list[0].Tags)

	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos", ObjectLock: true}).Return(&info.Bucket, nil).Once()
	f.buckets.EXPECT().Get(mock.Anything, "photos").Return(info, nil).Once()
	rec = c.do(http.MethodPost, "/buckets", `{"name":"photos","objectLock":true}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, "photos", decode[admin.BucketResponse](t, rec).Name)

	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos"}).Return(nil, fmt.Errorf("%w: photos", domain.ErrBucketAlreadyExists)).Once()
	rec = c.do(http.MethodPost, "/buckets", `{"name":"photos"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "bucket_already_exists", problem(t, rec).Code)

	f.buckets.EXPECT().Get(mock.Anything, "nope").Return(nil, fmt.Errorf("%w: nope", domain.ErrBucketNotFound)).Once()
	rec = c.do(http.MethodGet, "/buckets/nope", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	f.buckets.EXPECT().Delete(mock.Anything, "photos", bucket.DeleteOptions{Force: true}).Return(nil).Once()
	rec = c.do(http.MethodDelete, "/buckets/photos?force=true", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)

	f.buckets.EXPECT().SetVersioning(mock.Anything, "photos", domain.VersioningSuspended).Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/versioning", `{"status":"suspended"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "suspended", decode[admin.VersioningResponse](t, rec).Status)
	f.buckets.EXPECT().SetVersioning(mock.Anything, "photos", domain.VersioningOff).Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/versioning", `{"status":"off"}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = c.do(http.MethodPut, "/buckets/photos/versioning", `{"status":"maybe"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.buckets.EXPECT().GetPolicy(mock.Anything, "photos").Return(nil, fmt.Errorf("%w: photos", domain.ErrPolicyNotFound)).Once()
	rec = c.do(http.MethodGet, "/buckets/photos/policy", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	doc := `{"Version":"2012-10-17","Statement":[]}`
	f.buckets.EXPECT().SetPolicy(mock.Anything, "photos", mock.MatchedBy(func(raw json.RawMessage) bool { return string(raw) == doc })).Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/policy", doc)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	f.buckets.EXPECT().SetQuota(mock.Anything, "photos", bucket.Quota{Type: bucket.QuotaHard, Size: 2048}).Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/quota", `{"size":2048}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "hard", decode[admin.QuotaResponse](t, rec).Type)

	f.buckets.EXPECT().SetTags(mock.Anything, "photos", map[string]string{"team": "media"}).Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/tags", `{"tags":{"team":"media"}}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.buckets.EXPECT().DeleteTags(mock.Anything, "photos").Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/tags", `{"tags":{}}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.buckets.EXPECT().SetObjectLock(mock.Anything, "photos", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 30}).Return(nil).Once()
	rec = c.do(http.MethodPut, "/buckets/photos/objectlock", `{"enabled":true,"mode":"GOVERNANCE","days":30}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.buckets.EXPECT().GetEncryption(mock.Anything, "photos").Return(bucket.EncryptionConfig{Algorithm: "AES256"}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/photos/encryption", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "AES256", decode[admin.EncryptionResponse](t, rec).Algorithm)
	f.buckets.EXPECT().DeleteEncryption(mock.Anything, "photos").Return(nil).Once()
	rec = c.do(http.MethodDelete, "/buckets/photos/encryption", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)

	rec = c.do(http.MethodPut, "/buckets/photos/lifecycle", `{}`)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "not_implemented", problem(t, rec).Code)

	// Object browser listing.
	f.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", Prefix: "2026/", Delimiter: "/", ContinuationToken: "tok", MaxKeys: 50}).
		Return(domain.ListObjectsResult{
			Objects:               []domain.ObjectInfo{{Key: "2026/a.jpg", Size: 10, ETag: "etag", ModTime: now, ContentType: "image/jpeg", IsLatest: true}},
			CommonPrefixes:        []string{"2026/sub/"},
			IsTruncated:           true,
			NextContinuationToken: "next",
		}, nil).Once()
	rec = c.do(http.MethodGet, "/buckets/photos/objects?prefix=2026/&delimiter=/&cursor=tok&limit=50", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	objs := decode[admin.ObjectListResponse](t, rec)
	require.Len(t, objs.Objects, 1)
	assert.Equal(t, "2026/a.jpg", objs.Objects[0].Key)
	assert.Equal(t, []string{"2026/sub/"}, objs.Prefixes)
	assert.Equal(t, "next", objs.NextCursor)
	assert.True(t, objs.IsTruncated)
	rec = c.do(http.MethodGet, "/buckets/photos/objects?limit=zero", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestConfigRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.client(t, "admin", "password123")

	rec := c.do(http.MethodPut, "/config/scanner", `{"settings":{"speed":"fast","cycle":"1h"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sub := decode[admin.SubsystemResponse](t, rec)
	assert.Equal(t, "scanner", sub.Name)
	assert.Equal(t, "fast", sub.Settings["speed"])

	rec = c.do(http.MethodPut, "/config", `{"notify_webhook":{"endpoint":"http://hook"}}`)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = c.do(http.MethodGet, "/config", "")
	require.Equal(t, http.StatusOK, rec.Code)
	all := decode[[]admin.SubsystemResponse](t, rec)
	require.Len(t, all, 2)
	assert.Equal(t, "notify_webhook", all[0].Name, "sorted by name")
	assert.Equal(t, "scanner", all[1].Name)

	rec = c.do(http.MethodGet, "/config/missing", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"name":"missing","settings":{}}`+"\n", rec.Body.String(), "a missing subsystem is empty, not an error")

	rec = c.do(http.MethodDelete, "/config/scanner?keys=speed", "")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	kv, err := f.config.Get(t.Context(), "scanner")
	require.NoError(t, err)
	assert.Equal(t, configsvc.KV{"cycle": "1h"}, kv)

	rec = c.do(http.MethodGet, "/config/export", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, httpx.ContentTypeJSON, rec.Header().Get("Content-Type"))
	exported := rec.Body.String()
	assert.Contains(t, exported, `"notify_webhook"`)

	rec = c.do(http.MethodPost, "/config/import", `{"scanner":{"speed":"slow"}}`)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	names, err := f.config.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"scanner"}, names)

	rec = c.do(http.MethodPost, "/config/import", `not json`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotFoundAndMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	c := f.client(t, "admin", "password123")

	rec := c.do(http.MethodGet, "/nope", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not_found", problem(t, rec).Code)

	rec = c.do(http.MethodDelete, "/info", "")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "method_not_allowed", problem(t, rec).Code)

	// Unknown paths are answered before authentication is checked only
	// for the 404 of the public group; protected ones need a session too.
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, admin.Prefix+"/users", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestProblemFromServiceErrors(t *testing.T) {
	f := newFixture(t)
	c := f.client(t, "admin", "password123")
	f.iam.EXPECT().ListUsers(mock.Anything).Return(nil, errors.New("disk on fire")).Once()
	rec := c.do(http.MethodGet, "/users", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	p := problem(t, rec)
	assert.Equal(t, "internal_error", p.Code)
	assert.NotContains(t, rec.Body.String(), "disk on fire", "internal errors never leak")
}
