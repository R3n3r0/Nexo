package testutil_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func TestSmokeHealthS3AndConsoleLogin(t *testing.T) {
	srv := testutil.StartServer(t)
	assert.Equal(t, domain.ModeSNSD, srv.App.Mode())

	// Liveness on both listeners.
	for _, base := range []string{srv.ConsoleURL, srv.S3URL} {
		resp, err := srv.Client.Get(base + "/nexo/health/live")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, base)
		assert.Contains(t, string(body), `"ok":true`)
	}
	resp, err := srv.Client.Get(srv.ConsoleURL + "/nexo/health/ready")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// S3 root answers S3 XML: anonymous ListBuckets is denied.
	resp, err = srv.Client.Get(srv.S3URL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	var s3err httpx.S3ErrorResponse
	require.NoError(t, xml.Unmarshal(body, &s3err))
	assert.Equal(t, "AccessDenied", s3err.Code)
	assert.NotEmpty(t, s3err.RequestID)

	// Console login → session → info.
	loginBody := strings.NewReader(`{"accessKey":"` + srv.RootUser + `","secretKey":"` + srv.RootPassword + `"}`)
	resp, err = srv.Client.Post(srv.ConsoleURL+"/api/v1/auth/login", "application/json", loginBody)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var login struct {
		AccessKey string `json:"accessKey"`
		CSRFToken string `json:"csrfToken"`
	}
	require.NoError(t, json.Unmarshal(body, &login))
	assert.Equal(t, srv.RootUser, login.AccessKey)
	assert.NotEmpty(t, login.CSRFToken)

	resp, err = srv.Client.Get(srv.ConsoleURL + "/api/v1/info")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"mode":"snsd"`)

	// Metrics are exposed to a bearer token of an admin (default
	// NEXO_PROMETHEUS_AUTH_TYPE=jwt).
	token, err := auth.NewSigner(auth.Deps{}).SignBearerToken(context.Background(),
		domain.Credentials{AccessKey: srv.RootUser, SecretKey: srv.RootPassword}, time.Hour)
	require.NoError(t, err)
	status, metricsBody := scrape(t, srv, token)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, metricsBody, "nexo_http_requests_total")
}

// scrape GETs the node metrics with an optional bearer token.
func scrape(t *testing.T, srv *testutil.Server, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.S3URL+"/nexo/metrics/v1/node", nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// Regression: the metrics endpoints were served to anyone. They require a
// bearer token of an identity allowed admin:Prometheus unless
// NEXO_PROMETHEUS_AUTH_TYPE=public.
func TestMetricsAuthentication(t *testing.T) {
	ctx := context.Background()
	srv := testutil.StartServer(t)
	signer := auth.NewSigner(auth.Deps{})
	root := domain.Credentials{AccessKey: srv.RootUser, SecretKey: srv.RootPassword}
	rootToken, err := signer.SignBearerToken(ctx, root, time.Hour)
	require.NoError(t, err)
	forged, err := signer.SignBearerToken(ctx, domain.Credentials{AccessKey: srv.RootUser, SecretKey: "not-the-secret"}, time.Hour)
	require.NoError(t, err)
	_, err = srv.IAM.CreateUser(ctx, iam.CreateUserInput{AccessKey: "reader", SecretKey: "reader-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	readerToken, err := signer.SignBearerToken(ctx, domain.Credentials{AccessKey: "reader", SecretKey: "reader-secret"}, time.Hour)
	require.NoError(t, err)

	tests := []struct {
		name  string
		token string
		want  int
	}{
		{name: "no token", want: http.StatusForbidden},
		{name: "forged token", token: forged, want: http.StatusForbidden},
		{name: "user without admin:Prometheus", token: readerToken, want: http.StatusForbidden},
		{name: "root token", token: rootToken, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := scrape(t, srv, tt.token)
			assert.Equal(t, tt.want, status, body)
			if tt.want != http.StatusOK {
				assert.NotContains(t, body, "go_goroutines")
			}
		})
	}
	// A SigV4 signature is no metrics credential, and a bearer token is no
	// S3 credential.
	req, err := http.NewRequest(http.MethodGet, srv.S3URL+"/", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+rootToken)
	resp, err := srv.Client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	public := testutil.StartServer(t, testutil.WithConfig(func(c *config.Static) { c.PrometheusAuthType = config.PrometheusAuthPublic }))
	status, body := scrape(t, public, "")
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "go_goroutines")
}
