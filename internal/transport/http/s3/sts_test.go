package s3_test

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// stsFixture serves the STS routes over mocked IAM and policy services.
type stsFixture struct {
	iam      *iammock.Service
	policy   *policymock.Service
	identity *domain.Identity
	handler  http.Handler
}

func newSTSFixture(t *testing.T, identity *domain.Identity) *stsFixture {
	t.Helper()
	f := &stsFixture{iam: iammock.NewService(t), policy: policymock.NewService(t), identity: identity}
	routes := s3.NewSTSRoutes(s3.STSDeps{IAM: f.iam, Policy: f.policy, Enc: httpx.New()})
	r := chi.NewRouter()
	routes.Register(r)
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.identity != nil {
			req = req.WithContext(middleware.WithIdentity(req.Context(), f.identity))
		}
		r.ServeHTTP(w, req)
	})
	return f
}

func stsForm(params ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(params); i += 2 {
		v.Set(params[i], params[i+1])
	}
	return v.Encode()
}

func (f *stsFixture) post(target, body string, headers ...string) (int, string) {
	req := request(http.MethodPost, target, body, append([]string{"Content-Type", "application/x-www-form-urlencoded"}, headers...)...)
	rec := serve(f.handler, req)
	return rec.Code, rec.Body.String()
}

func stsErrorOf(t *testing.T, body string) s3.STSErrorResponse {
	t.Helper()
	var e s3.STSErrorResponse
	require.NoError(t, xml.Unmarshal([]byte(body), &e), body)
	return e
}

var stsUser = &domain.Identity{AccessKey: "alice", PolicyNames: []string{"readwrite"}, AuthType: domain.AuthTypeHeader}

func TestSTSAssumeRole(t *testing.T) {
	f := newSTSFixture(t, stsUser)
	exp := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
	sessionPolicy := &domain.Policy{Version: domain.PolicyVersion}
	rawPolicy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	f.policy.EXPECT().Parse(mock.Anything, []byte(rawPolicy)).Return(sessionPolicy, nil).Once()
	f.iam.EXPECT().AssumeRole(mock.Anything, iam.AssumeRoleInput{
		Caller: *stsUser, Policy: sessionPolicy, Duration: 900 * time.Second, RoleSessionName: "nightly",
	}).Return(&domain.Credentials{AccessKey: "STSKEY", SecretKey: "stssecret", SessionToken: "token", Expiration: exp}, nil).Once()

	status, body := f.post("/", stsForm("Action", "AssumeRole", "Version", s3.STSVersion,
		"DurationSeconds", "900", "RoleSessionName", "nightly", "Policy", rawPolicy))
	require.Equal(t, http.StatusOK, status, body)
	var resp s3.AssumeRoleResponse
	require.NoError(t, xml.Unmarshal([]byte(body), &resp), body)
	assert.Equal(t, "STSKEY", resp.Result.Credentials.AccessKeyID)
	assert.Equal(t, "stssecret", resp.Result.Credentials.SecretAccessKey)
	assert.Equal(t, "token", resp.Result.Credentials.SessionToken)
	assert.Equal(t, "2026-10-08T13:00:00Z", resp.Result.Credentials.Expiration)
	assert.Equal(t, "arn:aws:sts:::assumed-role/alice/nightly", resp.Result.AssumedRoleUser.Arn)
	assert.Contains(t, body, `xmlns="https://sts.amazonaws.com/doc/2011-06-15/"`)
}

func TestSTSAssumeRoleQueryParameters(t *testing.T) {
	// Parameters may travel in the query string; an empty body is fine
	// and the session name defaults to the access key.
	f := newSTSFixture(t, &domain.Identity{AccessKey: "svc", Parent: "alice"})
	f.iam.EXPECT().AssumeRole(mock.Anything, mock.MatchedBy(func(in iam.AssumeRoleInput) bool {
		return in.Caller.AccessKey == "svc" && in.Duration == 0 && in.Policy == nil
	})).Return(&domain.Credentials{AccessKey: "K", SecretKey: "S", SessionToken: "T", Expiration: time.Unix(0, 0)}, nil).Once()
	req := request(http.MethodPost, "/?Action=AssumeRole&Version=2011-06-15", "")
	rec := serve(f.handler, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "assumed-role/alice/K")
}

func TestSTSErrors(t *testing.T) {
	valid := func(extra ...string) string {
		return stsForm(append([]string{"Action", "AssumeRole", "Version", s3.STSVersion}, extra...)...)
	}
	tests := []struct {
		name     string
		identity *domain.Identity
		body     string
		headers  []string
		setup    func(f *stsFixture)
		status   int
		code     string
		kind     string
	}{
		{name: "missing action", identity: stsUser, body: stsForm("Version", s3.STSVersion), status: 400, code: "MissingAction", kind: "Sender"},
		{name: "unknown action", identity: stsUser, body: stsForm("Action", "Dance", "Version", s3.STSVersion), status: 400, code: "InvalidAction"},
		{name: "unsupported action", identity: stsUser, body: stsForm("Action", "AssumeRoleWithWebIdentity", "Version", s3.STSVersion), status: 501, code: "NotImplemented", kind: "Receiver"},
		{name: "wrong version", identity: stsUser, body: stsForm("Action", "AssumeRole", "Version", "2000-01-01"), status: 400, code: "InvalidParameterValue"},
		{name: "anonymous", identity: domain.Anonymous(), body: valid(), status: 403, code: "AccessDenied"},
		{name: "no identity", body: valid(), status: 403, code: "AccessDenied"},
		{name: "bad duration", identity: stsUser, body: valid("DurationSeconds", "soon"), status: 400, code: "InvalidParameterValue"},
		{name: "negative duration", identity: stsUser, body: valid("DurationSeconds", "-5"), status: 400, code: "InvalidParameterValue"},
		{name: "overflowing duration", identity: stsUser, body: valid("DurationSeconds", "9223372036854775807"), status: 400, code: "InvalidParameterValue"},
		{name: "bad content type", identity: stsUser, body: valid(), headers: []string{"Content-Type", "application/json"}, status: 400, code: "InvalidParameterValue"},
		{name: "malformed content type", identity: stsUser, body: valid(), headers: []string{"Content-Type", "a/b; ="}, status: 400, code: "InvalidParameterValue"},
		{name: "malformed form", identity: stsUser, body: "Action=%zz", status: 400, code: "InvalidParameterValue"},
		{name: "too large", identity: stsUser, body: valid("Policy", strings.Repeat("x", 70<<10)), status: 400, code: "InvalidParameterValue"},
		{
			name: "malformed policy", identity: stsUser, body: valid("Policy", "{}"),
			setup: func(f *stsFixture) {
				f.policy.EXPECT().Parse(mock.Anything, []byte("{}")).
					Return(nil, domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", "missing Statement")).Once()
			},
			status: 400, code: "MalformedPolicyDocument",
		},
		{
			name: "policy internal error", identity: stsUser, body: valid("Policy", "{}"),
			setup: func(f *stsFixture) {
				f.policy.EXPECT().Parse(mock.Anything, []byte("{}")).Return(nil, errors.New("boom")).Once()
			},
			status: 400, code: "MalformedPolicyDocument",
		},
		{
			name: "derived caller", identity: stsUser, body: valid(),
			setup: func(f *stsFixture) {
				f.iam.EXPECT().AssumeRole(mock.Anything, mock.Anything).Return(nil, domain.ErrAccessDenied).Once()
			},
			status: 403, code: "AccessDenied",
		},
		{
			name: "duration out of range", identity: stsUser, body: valid("DurationSeconds", "60"),
			setup: func(f *stsFixture) {
				f.iam.EXPECT().AssumeRole(mock.Anything, mock.Anything).Return(nil, domain.ErrInvalidArgument).Once()
			},
			status: 400, code: "InvalidArgument",
		},
		{
			name: "iam failure", identity: stsUser, body: valid(),
			setup: func(f *stsFixture) {
				f.iam.EXPECT().AssumeRole(mock.Anything, mock.Anything).Return(nil, errors.New("disk on fire")).Once()
			},
			status: 500, code: "InternalError", kind: "Receiver",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSTSFixture(t, tc.identity)
			if tc.setup != nil {
				tc.setup(f)
			}
			status, body := f.post("/", tc.body, tc.headers...)
			require.Equal(t, tc.status, status, body)
			e := stsErrorOf(t, body)
			assert.Equal(t, tc.code, e.Error.Code)
			assert.NotEmpty(t, e.Error.Message)
			assert.NotContains(t, body, "disk on fire", "internal errors do not leak")
			if tc.kind != "" {
				assert.Equal(t, tc.kind, e.Error.Type)
			}
		})
	}
}

func TestSTSNotConfigured(t *testing.T) {
	routes := s3.NewSTSRoutes(s3.STSDeps{})
	r := chi.NewRouter()
	routes.Register(r)
	rec := serve(r, request(http.MethodPost, "/", stsForm("Action", "AssumeRole", "Version", s3.STSVersion)))
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "NotImplemented", stsErrorOf(t, rec.Body.String()).Error.Code)
}

func TestSTSMountedOnTheS3API(t *testing.T) {
	// The STS group owns POST on "/", next to ListBuckets (GET "/").
	f := newFixture(t)
	i := iammock.NewService(t)
	p := policymock.NewService(t)
	api := s3.NewAPI(s3.APIDeps{}, s3.NewSTSRoutes(s3.STSDeps{IAM: i, Policy: p}),
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: f.buckets, Objects: f.objects, Policy: f.policy}))
	r := chi.NewRouter()
	api.Register(r)
	i.EXPECT().AssumeRole(mock.Anything, mock.Anything).Return(&domain.Credentials{AccessKey: "K", Expiration: time.Unix(0, 0)}, nil).Once()
	req := request(http.MethodPost, "/", stsForm("Action", "AssumeRole", "Version", s3.STSVersion), "Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(middleware.WithIdentity(req.Context(), stsUser))
	rec := serve(r, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
