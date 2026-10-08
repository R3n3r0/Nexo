package testutil_test

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
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

	// Metrics are exposed.
	resp, err = srv.Client.Get(srv.S3URL + "/nexo/metrics/v1/node")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "nexo_http_requests_total")
}

func TestSmokeSNMD(t *testing.T) {
	dir := t.TempDir()
	srv := testutil.StartServer(t, testutil.WithEndpoints(dir+"/d1", dir+"/d2", dir+"/d3", dir+"/d4"))
	assert.Equal(t, domain.ModeSNMD, srv.App.Mode())
}
