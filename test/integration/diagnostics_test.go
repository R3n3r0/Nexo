package integration_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestDiagnostics profiles the server and runs its speed tests through
// the Admin API (S-OPS-11): allowed to root and to the diagnostics
// policy, denied to the other identities; the object test leaves no
// bucket behind and the network test needs a cluster.
//
// covers: S-OPS-11, C-ADM-12
func TestDiagnostics(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	diag := e.createUser("diag-user", "diag-secret-123", "diagnostics")
	rw := e.createUser("rw-user", "rw-secret-12345", "readwrite")
	client := func(c creds) nexoadmin.AdminClient {
		cl, err := nexoadmin.New(nexoadmin.Config{Endpoint: e.srv.ConsoleURL, AccessKey: c.AccessKey, SecretKey: c.SecretKey, Region: e.srv.Config.Region})
		require.NoError(t, err)
		return cl
	}
	status := func(err error) int {
		var apiErr *nexoadmin.APIError
		if errors.As(err, &apiErr) {
			return apiErr.Status
		}
		return 0
	}
	root, diagnostics, readwrite := adminClient(t, e), client(diag), client(rw)

	for name, c := range map[string]nexoadmin.AdminClient{"root": root, "diagnostics policy": diagnostics} {
		p, err := c.Profile(e.ctx, nexoadmin.ProfileRequest{Type: nexoadmin.ProfileGoroutine})
		require.NoError(t, err, name)
		require.Greater(t, len(p.Data), 2, name)
		assert.Equal(t, []byte{0x1f, 0x8b}, p.Data[:2], "%s: a gzipped pprof profile", name)
		assert.NotEmpty(t, p.Node)
		assert.Contains(t, p.FileName, "goroutine.pb.gz")
	}
	_, err := readwrite.Profile(e.ctx, nexoadmin.ProfileRequest{Type: nexoadmin.ProfileHeap})
	assert.Equal(t, http.StatusForbidden, status(err), "readwrite has no admin:Profiling")
	_, err = root.Profile(e.ctx, nexoadmin.ProfileRequest{Type: "threadcreate"})
	assert.Equal(t, http.StatusBadRequest, status(err))

	small := nexoadmin.SpeedtestRequest{Size: 64 << 10, DurationSeconds: 0.2, Concurrency: 2}
	res, err := diagnostics.Speedtest(e.ctx, nexoadmin.SpeedtestDrive, small)
	require.NoError(t, err)
	require.NotEmpty(t, res.Targets)
	for _, tg := range res.Targets {
		assert.Empty(t, tg.Error, tg.Target)
		assert.Positive(t, tg.Write.Operations, tg.Target)
		assert.Positive(t, tg.Read.Operations, tg.Target)
	}
	res, err = root.Speedtest(e.ctx, nexoadmin.SpeedtestObject, small)
	require.NoError(t, err)
	require.Len(t, res.Targets, 1)
	assert.Empty(t, res.Targets[0].Error)
	assert.Positive(t, res.Targets[0].Write.Operations)
	assert.Positive(t, res.Targets[0].Read.Operations)
	buckets, err := root.ListBuckets(e.ctx, nexoadmin.ListBucketsOptions{})
	require.NoError(t, err)
	for _, b := range buckets.Items {
		assert.NotContains(t, b.Name, "nexo-speedtest-", "the temporary bucket is removed")
	}

	res, err = root.Speedtest(e.ctx, nexoadmin.SpeedtestNet, small)
	if modeOf(t) == modeCluster {
		require.NoError(t, err)
		assert.Len(t, res.Targets, 3, "every other node")
		for _, tg := range res.Targets {
			assert.Empty(t, tg.Error, tg.Target)
			assert.Positive(t, tg.Read.Operations, tg.Target)
		}
	} else {
		assert.Equal(t, http.StatusNotImplemented, status(err))
	}

	_, err = readwrite.Speedtest(e.ctx, nexoadmin.SpeedtestDrive, small)
	assert.Equal(t, http.StatusForbidden, status(err), "readwrite has no admin:Speedtest")
	_, err = root.Speedtest(e.ctx, nexoadmin.SpeedtestDrive, nexoadmin.SpeedtestRequest{Size: 1 << 30})
	assert.Equal(t, http.StatusBadRequest, status(err), "files of at most 64 MiB")
	_, err = root.Speedtest(e.ctx, "gpu", small)
	assert.Equal(t, http.StatusBadRequest, status(err))
}
