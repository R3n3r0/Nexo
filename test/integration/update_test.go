package integration_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestUpdateNotAProcess (S-OPS-09, C-ADM-09): a server embedded in another
// process (the tests) does not own its process: the update and the
// coordinated restart and stop answer 501, and nothing ran; the input of
// an update is checked first.
//
// covers: S-OPS-09, C-ADM-09
func TestUpdateNotAProcess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	admin := adminClient(t, e)
	ctx := e.ctx
	status := func(err error) int {
		var apiErr *nexoadmin.APIError
		require.True(t, errors.As(err, &apiErr), "%v", err)
		return apiErr.Status
	}
	_, err := admin.UpdateStatus(ctx)
	assert.True(t, nexoadmin.IsNotFound(err), "%v", err)
	_, err = admin.StartUpdate(ctx, nexoadmin.UpdateRequest{URL: "ftp://example.test/nexo", SHA256: strings.Repeat("0", 64)})
	assert.Equal(t, http.StatusBadRequest, status(err))
	_, err = admin.StartUpdate(ctx, nexoadmin.UpdateRequest{URL: "https://example.test/nexo", SHA256: strings.Repeat("0", 64)})
	assert.Equal(t, http.StatusNotImplemented, status(err))
	_, err = admin.RestartCluster(ctx, "rolling")
	assert.Equal(t, http.StatusNotImplemented, status(err))
	assert.Equal(t, http.StatusNotImplemented, status(admin.StopCluster(ctx)))
	_, err = admin.UpdateStatus(ctx)
	assert.True(t, nexoadmin.IsNotFound(err), "no job ran: %v", err)
}
