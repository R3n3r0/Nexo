package admin_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	auditmock "github.com/r3n3r0/nexo/internal/service/audit/mock"
	"github.com/r3n3r0/nexo/internal/service/auth"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	lockmock "github.com/r3n3r0/nexo/internal/service/lock/mock"
	logsmock "github.com/r3n3r0/nexo/internal/service/logs/mock"
	mediamock "github.com/r3n3r0/nexo/internal/service/media/mock"
	metricsmock "github.com/r3n3r0/nexo/internal/service/metrics/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/service/session"
	tracemock "github.com/r3n3r0/nexo/internal/service/trace/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Credentials known to the fixture.
const (
	rootKey    = "admin"
	rootSecret = "password123"
	bobKey     = "bob"
	bobSecret  = "bobsecret1"
	region     = "us-east-1"
)

// fixture is the complete Admin API over mocked services, with a real
// session service, SigV4 verifier and signer (the identities come from a
// mocked IAM). The authorizer mock allows everything unless deny is set,
// and records every AuthorizeInput.
type fixture struct {
	t       *testing.T
	handler http.Handler
	groups  []admin.Routes
	catalog []policy.Action
	clk     *clock.Fake
	signer  auth.Signer

	iam        *iammock.Service
	authz      *adminmock.Authorizer
	adm        *adminmock.Service
	buckets    *adminmock.BucketManager
	bucketSvc  *bucketmock.Service
	browser    *adminmock.ObjectBrowser
	media      *mediamock.Service
	uploads    *adminmock.UploadManager
	shares     *adminmock.ShareManager
	identities *adminmock.IdentityManager
	accessKeys *adminmock.AccessKeyManager
	policies   *adminmock.PolicyManager
	policy     *policymock.Service
	config     *adminmock.ConfigManager
	cluster    *adminmock.ClusterManager
	lifecycle  *adminmock.LifecycleManager
	notify     *adminmock.NotificationManager
	cors       *adminmock.CORSManager
	rules      *adminmock.AccessRuleManager
	dists      *adminmock.DistributionManager
	certs      *adminmock.CertificateManager
	cache      *adminmock.CacheManager
	diag       *adminmock.Diagnostics
	tracer     *tracemock.Service
	logs       *logsmock.Service
	audit      *auditmock.Service
	history    *metricsmock.History
	locks      *lockmock.Manager
	sessions   session.Service

	mu     sync.Mutex
	deny   bool
	denied map[policy.Action]bool
	checks []adminsvc.AuthorizeInput
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	f := &fixture{
		t: t, clk: clk,
		iam: iammock.NewService(t), authz: adminmock.NewAuthorizer(t), adm: adminmock.NewService(t),
		buckets: adminmock.NewBucketManager(t), bucketSvc: bucketmock.NewService(t), browser: adminmock.NewObjectBrowser(t),
		media:   mediamock.NewService(t),
		uploads: adminmock.NewUploadManager(t), shares: adminmock.NewShareManager(t), identities: adminmock.NewIdentityManager(t),
		accessKeys: adminmock.NewAccessKeyManager(t), policies: adminmock.NewPolicyManager(t), policy: policymock.NewService(t),
		config: adminmock.NewConfigManager(t), cluster: adminmock.NewClusterManager(t), locks: lockmock.NewManager(t),
		lifecycle: adminmock.NewLifecycleManager(t), notify: adminmock.NewNotificationManager(t), cors: adminmock.NewCORSManager(t),
		rules: adminmock.NewAccessRuleManager(t), dists: adminmock.NewDistributionManager(t), certs: adminmock.NewCertificateManager(t),
		cache: adminmock.NewCacheManager(t), diag: adminmock.NewDiagnostics(t), catalog: adminsvc.CatalogActions(),
		tracer: tracemock.NewService(t), logs: logsmock.NewService(t), audit: auditmock.NewService(t), history: metricsmock.NewHistory(t),
	}
	f.iam.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, key string) (domain.Credentials, domain.Identity, error) {
			switch key {
			case rootKey:
				return domain.Credentials{AccessKey: rootKey, SecretKey: rootSecret, Status: domain.StatusEnabled}, domain.Identity{AccessKey: rootKey, IsRoot: true}, nil
			case bobKey:
				return domain.Credentials{AccessKey: bobKey, SecretKey: bobSecret, Status: domain.StatusEnabled},
					domain.Identity{AccessKey: bobKey, Groups: []string{"staff"}, PolicyNames: []string{"readonly"}}, nil
			}
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: %s", domain.ErrInvalidAccessKey, key)
		}).Maybe()
	f.authz.EXPECT().Authorize(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, in adminsvc.AuthorizeInput) (adminsvc.Decision, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.checks = append(f.checks, in)
			if f.deny || (len(in.Actions) == 1 && f.denied[in.Actions[0]]) {
				return adminsvc.Decision{DeniedAction: in.Actions[0], Reason: "denied by the test"}, nil
			}
			return adminsvc.Decision{Allowed: true}, nil
		}).Maybe()

	enc := httpx.New()
	sessions, err := session.New(session.Deps{IAM: f.iam, Clock: clk}, session.WithTTL(time.Hour))
	require.NoError(t, err)
	f.sessions = sessions
	authDeps := auth.Deps{Creds: f.iam, Clock: clk}
	verifier, err := auth.NewVerifier(authDeps, auth.WithRegion(region))
	require.NoError(t, err)
	f.signer = auth.NewSigner(authDeps, auth.WithRegion(region))
	authn, err := admin.NewAuthenticator(admin.AuthenticatorDeps{Sessions: sessions, Verifier: verifier, Enc: enc})
	require.NoError(t, err)

	sessionRoutes, err := admin.NewSessionRoutes(admin.SessionDeps{Sessions: sessions, Authz: f.authz, Codec: enc, Clock: clk})
	require.NoError(t, err)
	infoRoutes, err := admin.NewInfoRoutes(admin.InfoDeps{Admin: f.adm, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	userRoutes, err := admin.NewUserRoutes(admin.UserDeps{IAM: f.iam, Identities: f.identities, AccessKeys: f.accessKeys, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	policyRoutes, err := admin.NewPolicyRoutes(admin.PolicyDeps{Policy: f.policy, Manager: f.policies, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	bucketRoutes, err := admin.NewBucketRoutes(admin.BucketDeps{Manager: f.buckets, Buckets: f.bucketSvc, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	objectRoutes, err := admin.NewObjectRoutes(admin.ObjectDeps{Browser: f.browser, Uploads: f.uploads, Authz: f.authz, Media: f.media, Enc: enc})
	require.NoError(t, err)
	shareRoutes, err := admin.NewShareRoutes(admin.ShareDeps{Shares: f.shares, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	configRoutes, err := admin.NewConfigRoutes(admin.ConfigDeps{Config: f.config, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	clusterRoutes, err := admin.NewClusterRoutes(admin.ClusterDeps{Cluster: f.cluster, Locks: f.locks, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	lifecycleRoutes, err := admin.NewLifecycleRoutes(admin.LifecycleDeps{Lifecycle: f.lifecycle, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	notificationRoutes, err := admin.NewNotificationRoutes(admin.NotificationDeps{Notifications: f.notify, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	corsRoutes, err := admin.NewCORSRoutes(admin.CORSDeps{CORS: f.cors, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	kmsRoutes, err := admin.NewKMSRoutes(admin.KMSDeps{Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	accessRuleRoutes, err := admin.NewAccessRuleRoutes(admin.AccessRuleDeps{Rules: f.rules, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	distributionRoutes, err := admin.NewDistributionRoutes(admin.DistributionDeps{Distributions: f.dists, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	certificateRoutes, err := admin.NewCertificateRoutes(admin.CertificateDeps{Certificates: f.certs, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	cacheRoutes, err := admin.NewCacheRoutes(admin.CacheDeps{Cache: f.cache, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	diagnosticsRoutes, err := admin.NewDiagnosticsRoutes(admin.DiagnosticsDeps{Diagnostics: f.diag, Authz: f.authz, Enc: enc})
	require.NoError(t, err)
	observabilityRoutes, err := admin.NewObservabilityRoutes(admin.ObservabilityDeps{Trace: f.tracer, Logs: f.logs, Audit: f.audit,
		History: f.history, Authz: f.authz, Enc: enc}, admin.WithHeartbeat(20*time.Millisecond))
	require.NoError(t, err)
	f.groups = []admin.Routes{sessionRoutes, infoRoutes, userRoutes, policyRoutes, bucketRoutes, objectRoutes, shareRoutes, configRoutes, clusterRoutes,
		lifecycleRoutes, notificationRoutes, corsRoutes, kmsRoutes, accessRuleRoutes, distributionRoutes, certificateRoutes, cacheRoutes,
		diagnosticsRoutes, observabilityRoutes}

	r := chi.NewRouter()
	api, err := admin.NewAPI(admin.APIDeps{Enc: enc, Auth: authn}, f.groups...)
	require.NoError(t, err)
	api.Register(r)
	f.handler = r
	return f
}

// denyAll makes the authorizer deny every request.
func (f *fixture) denyAll() {
	f.mu.Lock()
	f.deny = true
	f.mu.Unlock()
}

// denyActions makes the authorizer deny the requests of exactly one of
// actions.
func (f *fixture) denyActions(actions ...policy.Action) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied = map[policy.Action]bool{}
	for _, a := range actions {
		f.denied[a] = true
	}
}

// lastCheck returns the last AuthorizeInput the guard or a service asked.
func (f *fixture) lastCheck() adminsvc.AuthorizeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(f.t, f.checks, "no authorization was asked")
	return f.checks[len(f.checks)-1]
}

// resetChecks forgets the recorded authorizations.
func (f *fixture) resetChecks() {
	f.mu.Lock()
	f.checks = nil
	f.mu.Unlock()
}

// login opens a session.
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

// client is an authenticated caller: a console session or SigV4.
type client struct {
	t      *testing.T
	f      *fixture
	cookie *http.Cookie
	csrf   string
	// signed callers sign with these credentials instead of the cookie.
	signedKey, signedSecret string
}

// session logs in and returns a console client.
func (f *fixture) session(t *testing.T, access, secret string) *client {
	t.Helper()
	rec, resp := f.login(t, access, secret)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return &client{t: t, f: f, cookie: namedCookie(t, rec, session.CookieName), csrf: resp.CSRFToken}
}

// signed returns a SigV4 client.
func (f *fixture) signed(t *testing.T, access, secret string) *client {
	return &client{t: t, f: f, signedKey: access, signedSecret: secret}
}

// root is the console session of root.
func (f *fixture) root(t *testing.T) *client { return f.session(t, rootKey, rootSecret) }

// newRequest builds a request with a JSON body (a string) or a raw one
// (an io.Reader).
func (c *client) newRequest(method, path string, body any) *http.Request {
	c.t.Helper()
	var rd io.Reader
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
		rd = strings.NewReader(b)
	case io.Reader:
		data, err := io.ReadAll(b)
		require.NoError(c.t, err)
		raw = data
		rd = strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, admin.Prefix+path, rd)
	if _, isJSON := body.(string); isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.signedKey != "" {
		sum := sha256.Sum256(raw)
		req.Host = "console.local"
		req.URL.Host, req.URL.Scheme = "console.local", "http"
		require.NoError(c.t, c.f.signer.Sign(context.Background(), req,
			domain.Credentials{AccessKey: c.signedKey, SecretKey: c.signedSecret}, hex.EncodeToString(sum[:])))
		return req
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.csrf != "" {
		req.Header.Set(session.CSRFHeader, c.csrf)
	}
	return req
}

// do sends a request and returns the recorder.
func (c *client) do(method, path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.serve(c.newRequest(method, path, body))
}

func (c *client) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c.f.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

// problemDoc is a problem document with the Admin API extensions.
type problemDoc struct {
	httpx.Problem
	Errors []admin.PolicyErrorResponse `json:"errors"`
}

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) problemDoc {
	t.Helper()
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"), rec.Body.String())
	return decode[problemDoc](t, rec)
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

// requester matches the AuthorizeInput/inputs of the requester key.
func requester(key string) any {
	return mock.MatchedBy(func(r adminsvc.Requester) bool { return r.Identity.AccessKey == key })
}
