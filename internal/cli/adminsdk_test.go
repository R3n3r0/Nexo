package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func TestAdminURL(t *testing.T) {
	tests := []struct {
		name  string
		alias cli.Alias
		want  string
		err   bool
	}{
		{"next port", cli.Alias{URL: "http://localhost:9000"}, "http://localhost:9001", false},
		{"https next port", cli.Alias{URL: "https://s3.example.com:8443/"}, "https://s3.example.com:8444", false},
		{"ipv6", cli.Alias{URL: "http://[::1]:9000"}, "http://[::1]:9001", false},
		{"no port", cli.Alias{URL: "https://s3.example.com"}, "https://s3.example.com", false},
		{"path kept", cli.Alias{URL: "https://example.com/s3"}, "https://example.com/s3", false},
		{"explicit", cli.Alias{URL: "http://localhost:9000", AdminURL: "https://console.example.com/"}, "https://console.example.com", false},
		{"invalid explicit", cli.Alias{URL: "http://localhost:9000", AdminURL: "ftp://x"}, "", true},
		{"invalid URL", cli.Alias{URL: "localhost:9000"}, "", true},
		{"last port", cli.Alias{URL: "http://localhost:65535"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cli.AdminURL(tc.alias)
			if tc.err {
				require.Error(t, err)
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestValidateAliasAdminURL(t *testing.T) {
	a := cli.Alias{Name: "x", URL: "http://localhost:9000", AccessKey: "a", SecretKey: "b", AdminURL: "not a url"}
	assert.ErrorIs(t, cli.ValidateAlias(a), domain.ErrInvalidArgument)
	a.AdminURL = "http://localhost:9001"
	assert.NoError(t, cli.ValidateAlias(a))
}

// covers: TCL-U-04 (Admin API errors), TCL-E-11
func TestMapAdminError(t *testing.T) {
	problem := func(status int, code string) error {
		return &nexoadmin.APIError{Status: status, Code: code, Title: http.StatusText(status), Detail: "detail of " + code}
	}
	tests := []struct {
		name     string
		err      error
		sentinel error
		exit     int
	}{
		{"access denied", problem(403, "access_denied"), domain.ErrAccessDenied, cli.ExitAccess},
		{"csrf", problem(403, "invalid_csrf_token"), domain.ErrAccessDenied, cli.ExitAccess},
		{"unauthenticated", problem(401, "unauthenticated"), domain.ErrUnauthenticated, cli.ExitAccess},
		{"signature", problem(401, "signature_mismatch"), domain.ErrSignatureMismatch, cli.ExitAccess},
		{"unknown key", problem(401, "invalid_access_key"), domain.ErrInvalidAccessKey, cli.ExitAccess},
		{"expired", problem(401, "request_expired"), domain.ErrRequestExpired, cli.ExitAccess},
		{"user", problem(404, "user_not_found"), domain.ErrUserNotFound, cli.ExitNotFound},
		{"group", problem(404, "group_not_found"), domain.ErrGroupNotFound, cli.ExitNotFound},
		{"policy", problem(404, "policy_not_found"), domain.ErrPolicyNotFound, cli.ExitNotFound},
		{"share", problem(404, "share_not_found"), domain.ErrShareNotFound, cli.ExitNotFound},
		{"bucket", problem(404, "bucket_not_found"), domain.ErrBucketNotFound, cli.ExitNotFound},
		{"access key", problem(404, "access_key_not_found"), domain.ErrAccessKeyNotFound, cli.ExitNotFound},
		{"other 404", problem(404, "not_found"), cli.ErrAdminNotFound, cli.ExitNotFound},
		{"404 without code", problem(404, ""), domain.ErrNotFound, cli.ExitNotFound},
		{"other 403", problem(403, "forbidden"), domain.ErrAccessDenied, cli.ExitAccess},
		{"other 401", problem(401, "unauthorized"), domain.ErrUnauthenticated, cli.ExitAccess},
		{"conflict", problem(409, "conflict"), domain.ErrConflict, cli.ExitError},
		{"exists", problem(409, "user_already_exists"), domain.ErrUserAlreadyExists, cli.ExitError},
		{"malformed", problem(400, "malformed_policy"), domain.ErrMalformedPolicy, cli.ExitError},
		{"invalid", problem(400, "invalid_argument"), domain.ErrInvalidArgument, cli.ExitError},
		{"not implemented", problem(501, "not_implemented"), domain.ErrNotImplemented, cli.ExitError},
		{"server error", problem(500, "internal_error"), nil, cli.ExitError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := cli.MapAdminError(fmt.Errorf("wrapped: %w", tc.err), "http://admin.test")
			var ae *cli.AdminError
			require.ErrorAs(t, err, &ae)
			if tc.sentinel != nil {
				assert.ErrorIs(t, err, tc.sentinel)
			}
			var api *nexoadmin.APIError
			assert.ErrorAs(t, err, &api, "the problem stays reachable")
			assert.Equal(t, tc.exit, cli.ExitCode(err))
			assert.Contains(t, err.Error(), "detail of ")
			if tc.sentinel != domain.ErrObjectNotFound {
				// Regression: every admin 404 claimed to be a missing object.
				assert.NotErrorIs(t, err, domain.ErrObjectNotFound)
			}
		})
	}

	assert.NoError(t, cli.MapAdminError(nil, ""))
	assert.Equal(t, cli.ExitInterrupted, cli.ExitCode(cli.MapAdminError(fmt.Errorf("x: %w", context.Canceled), "")))
	plain := errors.New("plain")
	assert.Equal(t, plain, cli.MapAdminError(plain, ""))
}

func TestAdminErrorMessages(t *testing.T) {
	located := &nexoadmin.APIError{
		Status: 400, Code: "malformed_policy", Detail: "policy has 2 errors",
		Errors: []nexoadmin.PolicyError{
			{Statement: 1, Sid: "A", Element: "Action", Line: 4, Column: 7, Message: "unknown action"},
			{Line: 9, Column: 2, Message: "trailing data"},
		},
	}
	err := cli.MapAdminError(located, "http://admin.test")
	assert.Equal(t, "policy has 2 errors (malformed_policy)\n"+
		"  line 4, column 7: statement 1 (A), Action: unknown action\n"+
		"  line 9, column 2: trailing data", err.Error())
	details := cli.ErrorDetails(err)
	require.Len(t, details, 2)
	assert.Equal(t, cli.ErrorDetail{Statement: 1, Sid: "A", Element: "Action", Line: 4, Column: 7, Message: "unknown action"}, details[0])

	// A detail already in the message is not repeated.
	once := cli.MapAdminError(&nexoadmin.APIError{Status: 400, Code: "malformed_policy", Detail: "line 2, column 3: bad",
		Errors: []nexoadmin.PolicyError{{Line: 2, Column: 3, Message: "bad"}}}, "")
	assert.Equal(t, "line 2, column 3: bad (malformed_policy)", once.Error())

	// An S3 (XML) answer means the URL is not the Admin API.
	xml := cli.MapAdminError(&nexoadmin.APIError{Status: 404, Code: "not_found", Detail: "<?xml version=\"1.0\"?><Error/>"}, "http://localhost:9000")
	assert.Contains(t, xml.Error(), "http://localhost:9000 did not answer as the Admin API")
	assert.Contains(t, xml.Error(), "--admin-url")
	assert.Equal(t, cli.ExitNotFound, cli.ExitCode(xml))

	// The JSON error record carries the details.
	var out, errOut bytes.Buffer
	cli.NewPrinter(&out, &errOut, cli.PrinterOptions{JSON: true}).Error(err)
	var rec struct {
		Error cli.ErrorRecord `json:"error"`
	}
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &rec))
	assert.Len(t, rec.Error.Details, 2)
	assert.Equal(t, 4, rec.Error.Details[0].Line)
	assert.Empty(t, cli.ErrorDetails(errors.New("x")))

	assert.Equal(t, "message", cli.ErrorDetail{Message: "message"}.String())
	assert.Equal(t, "Effect: bad", cli.ErrorDetail{Element: "Effect", Message: "bad"}.String())
}

// covers: C-ADM-01, C-ADM-02 (client of nexoctl against a real server)
func TestAdminFactoryAgainstServer(t *testing.T) {
	srv := testutil.StartServer(t)
	ctx := context.Background()
	var debug bytes.Buffer
	factory := cli.NewAdminFactory(cli.AdminFactoryDeps{})
	alias := cli.Alias{Name: "local", URL: srv.S3URL, AdminURL: srv.ConsoleURL, AccessKey: srv.RootUser, SecretKey: srv.RootPassword}
	client, err := factory.Client(ctx, alias, cli.ClientOptions{Debug: &debug})
	require.NoError(t, err)

	info, err := client.ServerInfo(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, info.Mode)
	require.NotEmpty(t, info.Nodes)
	assert.Equal(t, "online", info.Nodes[0].State)
	assert.Contains(t, debug.String(), "> GET /api/v1/info")
	assert.NotContains(t, debug.String(), srv.RootPassword)

	_, err = client.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "alice", SecretKey: "alice-secret-1"})
	require.NoError(t, err)
	_, err = client.GetUser(ctx, "ghost")
	assert.ErrorIs(t, err, domain.ErrUserNotFound)
	assert.Equal(t, cli.ExitNotFound, cli.ExitCode(err))

	// alice has no admin permission: 403, exit code 4.
	aliceAlias := alias
	aliceAlias.AccessKey, aliceAlias.SecretKey = "alice", "alice-secret-1"
	alice, err := factory.Client(ctx, aliceAlias, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = alice.ListUsers(ctx, nexoadmin.ListOptions{})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	assert.Equal(t, cli.ExitAccess, cli.ExitCode(err))

	// Wrong secret: 401, exit code 4.
	wrong := alias
	wrong.SecretKey = "not-the-secret"
	bad, err := factory.Client(ctx, wrong, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = bad.ServerInfo(ctx)
	assert.Equal(t, cli.ExitAccess, cli.ExitCode(err))

	// The S3 listener is not the Admin API.
	s3Only := alias
	s3Only.AdminURL = srv.S3URL
	s3c, err := factory.Client(ctx, s3Only, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = s3c.ServerInfo(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer as the Admin API")

	// Nothing listens: the error names the endpoint and --admin-url.
	down := alias
	down.AdminURL = "http://127.0.0.1:1"
	dc, err := factory.Client(ctx, down, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = dc.ServerInfo(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot reach the Admin API at http://127.0.0.1:1")
	assert.Equal(t, cli.ExitError, cli.ExitCode(err))

	_, err = factory.Client(ctx, cli.Alias{Name: "x", URL: "nope"}, cli.ClientOptions{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
