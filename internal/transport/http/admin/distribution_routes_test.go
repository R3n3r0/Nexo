package admin_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// accessProblem is a problem document with the access rule errors.
type accessProblem struct {
	Code             string                          `json:"code"`
	Status           int                             `json:"status"`
	Detail           string                          `json:"detail"`
	AccessRuleErrors []admin.AccessRuleErrorResponse `json:"accessRuleErrors"`
}

// TestDistributionAuthorizationDenied: every route of the access rules,
// distributions and certificates asks its action first (on the bucket of
// the path for the access rules) and answers 403 access_denied when it is
// denied; the services are never called.
func TestDistributionAuthorizationDenied(t *testing.T) {
	tests := []struct {
		method, path, body string
		action             policy.Action
		bucket             string
	}{
		{http.MethodGet, "/buckets/b1/access-rules", "", policy.ActionAdminManageAccessRules, "b1"},
		{http.MethodPut, "/buckets/b1/access-rules", `{"rules":[]}`, policy.ActionAdminManageAccessRules, "b1"},
		{http.MethodGet, "/buckets/b1/access-rules/public", "", policy.ActionAdminManageAccessRules, "b1"},
		{http.MethodPut, "/buckets/b1/access-rules/public", `{"preset":"private"}`, policy.ActionAdminManageAccessRules, "b1"},
		{http.MethodGet, "/buckets/b1/block-public", "", policy.ActionAdminManageAccessRules, "b1"},
		{http.MethodPut, "/buckets/b1/block-public", `{"enabled":true}`, policy.ActionAdminManageAccessRules, "b1"},
		{http.MethodGet, "/access/block-public", "", policy.ActionAdminManageAccessRules, ""},
		{http.MethodPut, "/access/block-public", `{"enabled":true}`, policy.ActionAdminManageAccessRules, ""},
		{http.MethodGet, "/distributions", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodPost, "/distributions", `{"id":"d1","aliases":["a.test"],"origin":{"bucket":"b1"}}`, policy.ActionAdminManageDistributions, ""},
		{http.MethodGet, "/distributions/d1", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodPut, "/distributions/d1", `{"id":"d1","aliases":["a.test"],"origin":{"bucket":"b1"}}`, policy.ActionAdminManageDistributions, ""},
		{http.MethodDelete, "/distributions/d1", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodPost, "/distributions/d1/test", `{"url":"/"}`, policy.ActionAdminManageDistributions, ""},
		{http.MethodPost, "/distributions/d1/origin-secret/rotate", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodPost, "/distributions/d1/sign", `{"url":"/a","privateKeyPem":"k","expiresIn":60}`, policy.ActionAdminSignDistributionURL, ""},
		{http.MethodGet, "/keygroups", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodPost, "/keygroups", `{"id":"g1"}`, policy.ActionAdminManageDistributions, ""},
		{http.MethodGet, "/keygroups/g1", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodDelete, "/keygroups/g1", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodGet, "/keygroups/g1/keys", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodPost, "/keygroups/g1/keys", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodDelete, "/keygroups/g1/keys/K1", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodGet, "/routing/resolve?host=a.test", "", policy.ActionAdminManageDistributions, ""},
		{http.MethodGet, "/certificates", "", policy.ActionAdminManageCertificates, ""},
		{http.MethodPost, "/certificates", `{"certificatePem":"c","privateKeyPem":"k"}`, policy.ActionAdminManageCertificates, ""},
		{http.MethodGet, "/certificates/c1", "", policy.ActionAdminManageCertificates, ""},
		{http.MethodDelete, "/certificates/c1", "", policy.ActionAdminManageCertificates, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			f := newFixture(t)
			c := f.session(t, bobKey, bobSecret)
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
			in := f.lastCheck()
			assert.Equal(t, []policy.Action{tt.action}, in.Actions)
			assert.Equal(t, tt.bucket, in.Bucket, "the scope of the action")
			assert.Equal(t, bobKey, in.Requester.Identity.AccessKey)
		})
	}
}

// TestAccessRulesTestAuthorizedByService: the principal of the tester
// travels in the body, so the service authorizes it.
func TestAccessRulesTestAuthorizedByService(t *testing.T) {
	f := newFixture(t)
	f.rules.EXPECT().Test(mock.Anything, mock.MatchedBy(func(in adminsvc.TestAccessRulesInput) bool {
		return in.Requester.Identity.AccessKey == bobKey && in.Bucket == "b1" && in.Principal == "user:alice"
	})).Return(nil, fmt.Errorf("%w: admin:SimulatePolicy: implicit deny", domain.ErrAccessDenied))
	rec := f.session(t, bobKey, bobSecret).do(http.MethodPost, "/buckets/b1/access-rules/test", `{"keys":["a.jpg"],"principal":"user:alice"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, "access_denied", problemOf(t, rec).Code)
}

func TestAccessRuleRoutes(t *testing.T) {
	updated := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	cfg := &accessrule.Config{Bucket: "media", ETag: "abc123", UpdatedAt: updated, UpdatedBy: "admin", Public: true, Rules: []accessrule.Rule{
		{ID: "pub", Priority: 10, Effect: accessrule.EffectAllow, Principals: []string{"*"}, Actions: []string{"read"}},
	}}
	t.Run("get with the ETag", func(t *testing.T) {
		f := newFixture(t)
		f.rules.EXPECT().Get(mock.Anything, "media").Return(cfg, nil)
		rec := f.root(t).do(http.MethodGet, "/buckets/media/access-rules", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, `"abc123"`, rec.Header().Get("ETag"))
		got := decode[admin.AccessRulesResponse](t, rec)
		assert.Equal(t, "abc123", got.ETag)
		assert.Equal(t, &updated, got.UpdatedAt)
		assert.Equal(t, []string{}, got.Warnings, "arrays are never null")
		require.Len(t, got.Rules, 1)
		assert.True(t, got.Public)
	})
	t.Run("put with If-Match", func(t *testing.T) {
		f := newFixture(t)
		f.rules.EXPECT().Put(mock.Anything, mock.MatchedBy(func(in adminsvc.PutAccessRulesInput) bool {
			return in.Bucket == "media" && in.IfMatch == "abc123" && len(in.Rules) == 1 && in.Rules[0].ID == "pub" && in.Requester.Identity.AccessKey == rootKey
		})).Return(cfg, nil)
		c := f.root(t)
		req := c.newRequest(http.MethodPut, "/buckets/media/access-rules", `{"rules":[{"id":"pub","priority":10,"effect":"allow","principals":["*"],"actions":["read"]}]}`)
		req.Header.Set("If-Match", `"abc123"`)
		rec := c.serve(req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	errorCases := []struct {
		name       string
		err        error
		status     int
		code       string
		ruleErrors []admin.AccessRuleErrorResponse
	}{
		{name: "invalid regex", err: fmt.Errorf("x: %w", &accessrule.RuleError{Index: 1, ID: "bad", Field: "match.pattern", Position: 7, Msg: "missing closing )"}),
			status: http.StatusBadRequest, code: "invalid_access_rule",
			ruleErrors: []admin.AccessRuleErrorResponse{{Rule: 2, ID: "bad", Field: "match.pattern", Position: 7, Message: "missing closing )"}}},
		{name: "too many", err: &accessrule.RuleError{Index: -1, Field: "rules", Msg: "201 rules, at most 200 per bucket"},
			status: http.StatusBadRequest, code: "invalid_access_rule",
			ruleErrors: []admin.AccessRuleErrorResponse{{Rule: 0, Field: "rules", Message: "201 rules, at most 200 per bucket"}}},
		{name: "changed", err: fmt.Errorf("changed: %w", domain.ErrPreconditionFailed), status: http.StatusPreconditionFailed, code: "precondition_failed"},
		{name: "public blocked", err: fmt.Errorf("x: %w", accessrule.ErrPublicAccessBlocked), status: http.StatusForbidden, code: "public_access_blocked"},
		{name: "no bucket", err: fmt.Errorf("x: %w", domain.ErrBucketNotFound), status: http.StatusNotFound, code: "bucket_not_found"},
	}
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.rules.EXPECT().Put(mock.Anything, mock.Anything).Return(nil, tc.err)
			rec := f.root(t).do(http.MethodPut, "/buckets/media/access-rules", `{"rules":[]}`)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			p := decode[accessProblem](t, rec)
			assert.Equal(t, tc.code, p.Code)
			assert.Equal(t, tc.ruleErrors, p.AccessRuleErrors)
		})
	}
	t.Run("unknown fields are refused", func(t *testing.T) {
		f := newFixture(t)
		rec := f.root(t).do(http.MethodPut, "/buckets/media/access-rules", `{"rules":[{"id":"x","effect":"allow","principals":["*"],"actions":["read"],"typo":1}]}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
	t.Run("tester", func(t *testing.T) {
		f := newFixture(t)
		at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		res := &accessrule.TestResult{Bucket: "media", Action: "s3:GetObject", Results: []accessrule.KeyResult{{Key: "a.jpg", Allowed: true, Determinant: "pub"}}}
		f.rules.EXPECT().Test(mock.Anything, mock.MatchedBy(func(in adminsvc.TestAccessRulesInput) bool {
			return in.Principal == "anonymous" && in.IP == "1.2.3.4" && in.Host == "cdn.example.com" && in.Secure != nil && *in.Secure &&
				in.Time.Equal(at) && in.Headers["X-A"] == "b"
		})).Return(res, nil)
		rec := f.root(t).do(http.MethodPost, "/buckets/media/access-rules/test",
			`{"keys":["a.jpg"],"principal":"anonymous","ip":"1.2.3.4","host":"cdn.example.com","secure":true,"time":"2026-06-01T00:00:00Z","headers":{"X-A":"b"}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decode[accessrule.TestResult](t, rec)
		assert.Equal(t, "pub", got.Results[0].Determinant)
	})
	t.Run("presets and block public", func(t *testing.T) {
		f := newFixture(t)
		f.rules.EXPECT().ApplyPreset(mock.Anything, mock.MatchedBy(func(in adminsvc.AccessPresetInput) bool {
			return in.Preset == accessrule.PresetPublicPrefix && in.Match != nil && in.Match.Pattern == "pub/**" && in.IfMatch == "e1"
		})).Return(cfg, nil)
		c := f.root(t)
		req := c.newRequest(http.MethodPut, "/buckets/media/access-rules/public", `{"preset":"public-prefix","match":{"type":"glob","pattern":"pub/**"}}`)
		req.Header.Set("If-Match", "e1")
		rec := c.serve(req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		f.rules.EXPECT().ApplyPreset(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("x: %w", accessrule.ErrConfirmationRequired)).Once()
		rec = c.do(http.MethodPut, "/buckets/media/access-rules/public", `{"preset":"public-write"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "confirmation_required", problemOf(t, rec).Code)

		f.rules.EXPECT().BlockPublic(mock.Anything, "").Return(&accessrule.BlockPublic{Enabled: true, Mode: accessrule.BlockModeReject, Cluster: true, Effective: true}, nil)
		rec = c.do(http.MethodGet, "/access/block-public", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"enabled":true,"mode":"reject","cluster":true,"effective":true}`, rec.Body.String())
		f.rules.EXPECT().SetBlockPublic(mock.Anything, mock.MatchedBy(func(in adminsvc.SetBlockPublicInput) bool {
			return in.Bucket == "media" && in.Enabled
		})).Return(&accessrule.BlockPublic{Bucket: "media", Enabled: true, Effective: true, Mode: accessrule.BlockModeReject}, nil)
		rec = c.do(http.MethodPut, "/buckets/media/block-public", `{"enabled":true}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

func TestDistributionRoutes(t *testing.T) {
	d := distribution.Distribution{ID: "site", Enabled: true, Aliases: []string{"www.example.com"}, Origin: distribution.Origin{Bucket: "web"},
		Mode: distribution.ModeWebsite, ETag: "e1"}
	t.Run("create", func(t *testing.T) {
		f := newFixture(t)
		f.dists.EXPECT().Create(mock.Anything, mock.MatchedBy(func(in adminsvc.SaveDistributionInput) bool {
			return in.Distribution.ID == "site" && in.Distribution.Origin.Bucket == "web" && in.Requester.Identity.AccessKey == rootKey
		})).Return(d, nil)
		rec := f.root(t).do(http.MethodPost, "/distributions", `{"id":"site","aliases":["www.example.com"],"origin":{"bucket":"web"}}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, `"e1"`, rec.Header().Get("ETag"))
	})
	t.Run("alias conflict", func(t *testing.T) {
		f := newFixture(t)
		f.dists.EXPECT().Create(mock.Anything, mock.Anything).Return(distribution.Distribution{}, fmt.Errorf("www.example.com: %w", distribution.ErrAliasConflict))
		rec := f.root(t).do(http.MethodPost, "/distributions", `{"id":"site","aliases":["www.example.com"],"origin":{"bucket":"web"}}`)
		require.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, "alias_conflict", problemOf(t, rec).Code)
	})
	t.Run("update", func(t *testing.T) {
		f := newFixture(t)
		f.dists.EXPECT().Update(mock.Anything, mock.MatchedBy(func(in adminsvc.SaveDistributionInput) bool {
			return in.Distribution.ID == "site" && in.IfMatch == "e1"
		})).Return(d, nil)
		c := f.root(t)
		req := c.newRequest(http.MethodPut, "/distributions/site", `{"aliases":["www.example.com"],"origin":{"bucket":"web"}}`)
		req.Header.Set("If-Match", `W/"e1"`)
		rec := c.serve(req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = c.do(http.MethodPut, "/distributions/site", `{"id":"other","aliases":["www.example.com"],"origin":{"bucket":"web"}}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, "the id of the path wins")
	})
	t.Run("not found", func(t *testing.T) {
		f := newFixture(t)
		f.dists.EXPECT().Get(mock.Anything, "nope").Return(distribution.Distribution{}, fmt.Errorf("%w: nope", distribution.ErrDistributionNotFound))
		rec := f.root(t).do(http.MethodGet, "/distributions/nope", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "distribution_not_found", problemOf(t, rec).Code)
	})
	t.Run("test", func(t *testing.T) {
		f := newFixture(t)
		res := distribution.TestResult{
			Route:         distribution.Route{Kind: distribution.RouteDistribution, Host: "www.example.com", Alias: "www.example.com", Distribution: &d, Bucket: "web"},
			BehaviorIndex: 0, Behavior: distribution.Behavior{Access: distribution.AccessPublic}, Bucket: "web", Key: "assets/app.js",
			ResponseHeaders: map[string]string{"Cache-Control": "public, max-age=31536000, immutable"},
		}
		f.dists.EXPECT().Test(mock.Anything, adminsvc.TestDistributionInput{ID: "site", URL: "/assets/app.js", Method: "HEAD"}).Return(res, nil)
		rec := f.root(t).do(http.MethodPost, "/distributions/site/test", `{"url":"/assets/app.js","method":"HEAD"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decode[admin.DistributionTestResponse](t, rec)
		enabled := true
		assert.Equal(t, admin.RouteResponse{Kind: distribution.RouteDistribution, Host: "www.example.com", Alias: "www.example.com",
			Distribution: "site", Mode: distribution.ModeWebsite, Enabled: &enabled, Bucket: "web"}, got.Route)
		assert.Equal(t, "assets/app.js", got.Key)
		assert.Equal(t, "public, max-age=31536000, immutable", got.ResponseHeaders["Cache-Control"])
	})
	t.Run("sign", func(t *testing.T) {
		f := newFixture(t)
		exp := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
		f.dists.EXPECT().Sign(mock.Anything, mock.MatchedBy(func(in adminsvc.SignDistributionInput) bool {
			return in.ID == "site" && in.URL == "/dl/a.zip" && in.ExpiresIn == 90*time.Second && in.PrivateKeyPEM == "PEM" && in.IPRange == "1.2.3.4"
		})).Return(&adminsvc.SignedDistributionRequest{URL: "https://www.example.com/dl/a.zip?Expires=1", KeyGroup: "rel", KeyPairID: "K1", Expires: exp}, nil)
		c := f.root(t)
		rec := c.do(http.MethodPost, "/distributions/site/sign", `{"url":"/dl/a.zip","privateKeyPem":"PEM","expiresIn":90,"ipRange":"1.2.3.4"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decode[admin.SignDistributionResponse](t, rec)
		assert.Equal(t, admin.SignDistributionResponse{URL: "https://www.example.com/dl/a.zip?Expires=1", Cookies: []admin.SignedCookieResponse{},
			KeyGroup: "rel", KeyPairID: "K1", ExpiresAt: exp}, got)
		rec = c.do(http.MethodPost, "/distributions/site/sign", `{"url":"/dl/a.zip","privateKeyPem":"PEM","expiresIn":-1}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("rotate", func(t *testing.T) {
		f := newFixture(t)
		f.dists.EXPECT().RotateOriginSecret(mock.Anything, mock.Anything, distribution.RotateSecretInput{ID: "site", Keep: 2}).
			Return(&adminsvc.RotatedOriginSecret{Secret: "s3cr3t", Header: "X-Origin-Secret", Distribution: d}, nil)
		rec := f.root(t).do(http.MethodPost, "/distributions/site/origin-secret/rotate", `{"keep":2}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decode[admin.RotateOriginSecretResponse](t, rec)
		assert.Equal(t, "s3cr3t", got.Secret)
		assert.Equal(t, "X-Origin-Secret", got.Header)
	})
	t.Run("key groups", func(t *testing.T) {
		f := newFixture(t)
		g := distribution.KeyGroup{ID: "rel"}
		gen := &distribution.GeneratedKey{PublicKey: distribution.PublicKey{ID: "K1", Algorithm: distribution.KeyEd25519}, PrivateKeyPEM: "PRIV"}
		f.dists.EXPECT().CreateKeyGroup(mock.Anything, adminsvc.CreateKeyGroupInput{ID: "rel", Key: &adminsvc.GenerateKeySpec{Algorithm: distribution.KeyRSA, Bits: 4096}}).
			Return(&adminsvc.CreatedKeyGroup{KeyGroup: g, Key: gen}, nil)
		c := f.root(t)
		rec := c.do(http.MethodPost, "/keygroups", `{"id":"rel","generateKey":{"algorithm":"rsa","bits":4096}}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		got := decode[admin.CreateKeyGroupResponse](t, rec)
		assert.Equal(t, "PRIV", got.Key.PrivateKeyPEM)
		assert.NotNil(t, got.KeyGroup.Keys, "arrays are never null")

		f.dists.EXPECT().GetKeyGroup(mock.Anything, "rel").Return(g, nil)
		rec = c.do(http.MethodGet, "/keygroups/rel/keys", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"items":[]}`, rec.Body.String())

		f.dists.EXPECT().DeleteKeyGroup(mock.Anything, mock.Anything, "rel").Return(fmt.Errorf("used by site: %w", domain.ErrConflict))
		rec = c.do(http.MethodDelete, "/keygroups/rel", nil)
		require.Equal(t, http.StatusConflict, rec.Code)

		f.dists.EXPECT().DeleteKey(mock.Anything, mock.Anything, "rel", "K9").Return(fmt.Errorf("%w: K9", distribution.ErrKeyNotFound))
		rec = c.do(http.MethodDelete, "/keygroups/rel/keys/K9", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "key_not_found", problemOf(t, rec).Code)
	})
	t.Run("resolve", func(t *testing.T) {
		f := newFixture(t)
		f.dists.EXPECT().Resolve(mock.Anything, "photos.s3.example.com").Return(distribution.Route{Kind: distribution.RouteVirtualHost, Host: "photos.s3.example.com", Bucket: "photos"}, nil)
		rec := f.root(t).do(http.MethodGet, "/routing/resolve?host=photos.s3.example.com", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, `{"kind":"virtual-host","host":"photos.s3.example.com","bucket":"photos"}`, rec.Body.String())
	})
}

func TestCertificateRoutes(t *testing.T) {
	cert := certs.Certificate{ID: "shop", Source: certs.SourceUpload, Domains: []string{"shop.example.com"}, Status: certs.StatusValid}
	t.Run("list", func(t *testing.T) {
		f := newFixture(t)
		f.certs.EXPECT().List(mock.Anything).Return(&adminsvc.CertificateList{
			Items: []adminsvc.CertificateInfo{{Certificate: cert, UsedBy: []string{"site"}}},
			ACME: adminsvc.ACMEStatus{Directory: "https://acme.test", Challenge: certs.ChallengeHTTP01,
				Managed: []adminsvc.ManagedCertificate{{ID: "acme-b", Domains: []string{"b.test"}, Distributions: []string{"b"}, State: adminsvc.ACMEPending}}},
		}, nil)
		rec := f.root(t).do(http.MethodGet, "/certificates", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decode[admin.CertificateListResponse](t, rec)
		require.Len(t, got.Items, 1)
		assert.Equal(t, []string{"site"}, got.Items[0].UsedBy)
		assert.Equal(t, "shop", got.Items[0].ID)
		require.Len(t, got.ACME.Managed, 1)
		assert.Equal(t, "pending", got.ACME.Managed[0].State)
	})
	t.Run("upload", func(t *testing.T) {
		f := newFixture(t)
		f.certs.EXPECT().Upload(mock.Anything, certs.UploadInput{ID: "shop", ChainPEM: "CHAIN", KeyPEM: "KEY"}).Return(&adminsvc.CertificateInfo{Certificate: cert}, nil)
		rec := f.root(t).do(http.MethodPost, "/certificates", `{"id":"shop","certificatePem":"CHAIN","privateKeyPem":"KEY"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"usedBy":[]`)
		f.certs.EXPECT().Upload(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("%w: key does not match", certs.ErrInvalidCertificate))
		rec = f.root(t).do(http.MethodPost, "/certificates", `{"certificatePem":"CHAIN","privateKeyPem":"OTHER"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "invalid_certificate", problemOf(t, rec).Code)
		rec = f.root(t).do(http.MethodPost, "/certificates", strings.NewReader(strings.Repeat("x", 300<<10)))
		require.NotEqual(t, http.StatusCreated, rec.Code, "the body is bounded")
	})
	t.Run("delete in use", func(t *testing.T) {
		f := newFixture(t)
		f.certs.EXPECT().Delete(mock.Anything, "shop").Return(fmt.Errorf("%w: used by site", domain.ErrConflict))
		rec := f.root(t).do(http.MethodDelete, "/certificates/shop", nil)
		require.Equal(t, http.StatusConflict, rec.Code)
		f.certs.EXPECT().Get(mock.Anything, "nope").Return(nil, fmt.Errorf("%w: nope", certs.ErrCertificateNotFound))
		rec = f.root(t).do(http.MethodGet, "/certificates/nope", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "certificate_not_found", problemOf(t, rec).Code)
	})
}

func TestDistributionConstructorsRefuseMissingServices(t *testing.T) {
	_, err := admin.NewAccessRuleRoutes(admin.AccessRuleDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewDistributionRoutes(admin.DistributionDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewCertificateRoutes(admin.CertificateDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
