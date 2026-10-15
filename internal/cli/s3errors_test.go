package cli_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// TestSDKClientWithoutQuorum: a deployment without quorum answers 503 with
// its own codes; nexoctl says what to check (not "reduce your request
// rate") and does not multiply the wait of the server with retries.
func TestSDKClientWithoutQuorum(t *testing.T) {
	// A lock timeout is first of all a contention (a stalled reader holds
	// the lock while every drive is online): its hint says so (regression:
	// "the deployment has no quorum" sent the operator the wrong way).
	tests := []struct {
		code     string
		sentinel error
		hint     string
	}{
		{"SlowDownWrite", domain.ErrWriteQuorum, "the deployment has no quorum"},
		{"SlowDownRead", domain.ErrReadQuorum, "the deployment has no quorum"},
		{"XNexoLockTimeout", domain.ErrLockTimeout, "the object is locked by another operation (a slow or stalled read or write) or too many nodes are offline"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + tt.code +
					`</Code><Message>The deployment has lost its quorum.</Message></Error>`))
			}))
			defer srv.Close()
			c, err := cli.NewS3Factory(cli.S3FactoryDeps{}).Client(context.Background(),
				cli.Alias{Name: "cl", URL: srv.URL, AccessKey: "ak", SecretKey: "secret-key", Path: "on"}, cli.ClientOptions{})
			require.NoError(t, err)
			err = c.MakeBucket(context.Background(), "bkt1", cli.MakeBucketOptions{})
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.sentinel)
			assert.Contains(t, err.Error(), tt.hint)
			if tt.code == "XNexoLockTimeout" {
				assert.NotContains(t, err.Error(), "no quorum")
			}
			assert.Contains(t, err.Error(), `nexoctl admin info`)
			assert.Contains(t, err.Error(), "("+tt.code+")")
			assert.NotContains(t, err.Error(), "reduce your request rate")
			assert.Equal(t, int32(1), calls.Load(), "no retry on a deployment without quorum")
			assert.Equal(t, cli.ExitError, cli.ExitCode(err))
		})
	}

	// The other 503 keep the retries of the SDK.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<Error><Code>ServiceUnavailable</Code><Message>offline</Message></Error>`))
	}))
	defer srv.Close()
	c, err := cli.NewS3Factory(cli.S3FactoryDeps{}).Client(context.Background(),
		cli.Alias{Name: "cl", URL: srv.URL, AccessKey: "ak", SecretKey: "secret-key", Path: "on"}, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = c.ListBuckets(context.Background())
	require.Error(t, err)
	assert.Equal(t, int32(3), calls.Load())
}

// TestSDKClientUnreachable: a server that cannot be contacted is reported
// with its URL and the cause, not with the error chain of the SDK.
func TestSDKClientUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close()) // nothing listens there any more

	c, err := cli.NewS3Factory(cli.S3FactoryDeps{}).Client(context.Background(),
		cli.Alias{Name: "down", URL: "http://" + addr, AccessKey: "ak", SecretKey: "secret-key", Path: "on"}, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = c.ListBuckets(context.Background())
	require.Error(t, err)
	assert.Equal(t, "cannot reach http://"+addr+": connection refused", err.Error())
	var unreachable *cli.UnreachableError
	require.ErrorAs(t, err, &unreachable)
	assert.Equal(t, cli.ExitError, cli.ExitCode(err))

	// A host name that does not resolve.
	c, err = cli.NewS3Factory(cli.S3FactoryDeps{}).Client(context.Background(),
		cli.Alias{Name: "typo", URL: "http://nexo.invalid:9000", AccessKey: "ak", SecretKey: "secret-key", Path: "on"}, cli.ClientOptions{})
	require.NoError(t, err)
	_, err = c.ListBuckets(context.Background())
	require.Error(t, err)
	assert.Regexp(t, `^cannot reach http://nexo\.invalid:9000: lookup nexo\.invalid`, err.Error())
	assert.NotContains(t, err.Error(), "operation error")
}
