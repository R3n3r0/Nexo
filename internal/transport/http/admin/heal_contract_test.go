package admin_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// TestHealContract runs the healing endpoints against a real SNMD server
// (4 drives), every exchange validated against api/openapi.yaml: a job
// repairing an object a drive lost, its status and the list of the jobs,
// the usage of the scanner and the drives of /info.
func TestHealContract(t *testing.T) {
	srv := testutil.StartServer(t, testutil.Drives(4))
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "docs"}, http.StatusCreated)
	resp := c.request(http.MethodPost, "/buckets/docs/objects/upload"+q("dir/a.txt"), strings.NewReader(strings.Repeat("x", 300<<10)), http.Header{"Content-Type": {"text/plain"}})
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	drives := srv.DriveDirs()
	require.Len(t, drives, 4)
	require.NoError(t, os.RemoveAll(filepath.Join(drives[3], "docs", "dir")))

	job := decodeAs[admin.HealJobResponse](t, c.call(http.MethodPost, "/heal", map[string]any{"bucket": "docs", "recursive": true, "deep": true}, http.StatusAccepted))
	require.NotEmpty(t, job.ID)
	require.Eventually(t, func() bool {
		job = decodeAs[admin.HealJobResponse](t, c.call(http.MethodGet, "/heal/"+job.ID, nil, http.StatusOK))
		return job.Finished
	}, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, "done", job.State)
	assert.Equal(t, uint64(2), job.Scanned, "the bucket and its object")
	assert.Equal(t, uint64(1), job.Healed)
	require.Len(t, job.Drives, 4)
	assert.Equal(t, uint64(1), job.Drives[3].Missing)
	assert.Equal(t, uint64(1), job.Drives[3].Healed)
	require.Len(t, job.Items, 1)
	assert.Equal(t, "dir/a.txt", job.Items[0].Object)
	_, err := os.Stat(filepath.Join(drives[3], "docs", "dir", "a.txt", "xl.meta"))
	assert.NoError(t, err, "the drive got the object back")

	list := decodeAs[admin.HealJobListResponse](t, c.call(http.MethodGet, "/heal", nil, http.StatusOK))
	require.NotEmpty(t, list.Items)
	assert.Equal(t, job.ID, list.Items[0].ID)
	c.call(http.MethodGet, "/heal/no-such-job", nil, http.StatusNotFound)
	c.call(http.MethodPost, "/heal", map[string]any{"prefix": "x"}, http.StatusBadRequest)

	info := decodeAs[admin.ServerInfoResponse](t, c.call(http.MethodGet, "/info", nil, http.StatusOK))
	assert.Equal(t, "snmd", info.Mode)
	c.call(http.MethodGet, "/usage", nil, http.StatusOK)
}
