package admin_test

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

func TestClusterExportImportRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	archive := []byte("PK\x03\x04 archive")

	f.cluster.EXPECT().Export(mock.Anything).Return(archive, nil).Once()
	rec := c.do(http.MethodGet, "/cluster/export", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/zip", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "nexo-cluster.zip")
	assert.Equal(t, archive, rec.Body.Bytes())
	assert.Equal(t, []policy.Action{policy.ActionAdminExportCluster}, f.lastCheck().Actions)

	f.cluster.EXPECT().Import(mock.Anything, archive, adminsvc.ImportOptions{Mode: adminsvc.ImportReplace, DryRun: true}).
		Return(adminsvc.ImportSummary{Mode: adminsvc.ImportReplace, DryRun: true, Users: 2, Buckets: 1, Removed: []string{"user:mallory"}}, nil).Once()
	rec = c.do(http.MethodPost, "/cluster/import?mode=replace&dryRun=true", bytes.NewReader(archive))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	res := decode[admin.ClusterImportResponse](t, rec)
	assert.Equal(t, admin.ClusterImportResponse{Mode: "replace", DryRun: true, Users: 2, Buckets: 1, BucketsCreated: []string{}, Removed: []string{"user:mallory"}}, res)
	assert.Equal(t, []policy.Action{policy.ActionAdminImportCluster}, f.lastCheck().Actions)

	f.cluster.EXPECT().Import(mock.Anything, []byte("junk"), adminsvc.ImportOptions{}).
		Return(adminsvc.ImportSummary{}, fmt.Errorf("%w: cluster archive: not a zip file", domain.ErrInvalidArgument)).Once()
	rec = c.do(http.MethodPost, "/cluster/import", bytes.NewReader([]byte("junk")))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = c.do(http.MethodPost, "/cluster/import?dryRun=maybe", bytes.NewReader(archive))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	f.deny = true
	rec = c.do(http.MethodGet, "/cluster/export", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLockRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	since := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	f.locks.EXPECT().List(mock.Anything).Return([]lock.Status{{
		Info:    lock.Info{Resource: "photos/a.jpg", UID: "u1", Owner: "n1:9000", Writer: true, Quorum: 3, Since: since, Refreshed: since.Add(time.Second)},
		Servers: 3, Total: 4,
	}}, nil).Once()
	rec := c.do(http.MethodGet, "/locks", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	locks := decode[[]admin.LockResponse](t, rec)
	require.Len(t, locks, 1)
	assert.Equal(t, admin.LockResponse{Resource: "photos/a.jpg", UID: "u1", Owner: "n1:9000", Type: "write", Quorum: 3,
		Since: since, Refreshed: since.Add(time.Second), Servers: 3, Total: 4}, locks[0])
	assert.Equal(t, []policy.Action{policy.ActionAdminListLocks}, f.lastCheck().Actions)

	f.locks.EXPECT().ForceUnlock(mock.Anything, "photos/a.jpg", "photos/b.jpg").Return(nil).Once()
	rec = c.do(http.MethodDelete, "/locks?resource=photos/a.jpg&resource=photos/b.jpg", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []policy.Action{policy.ActionAdminForceUnlock}, f.lastCheck().Actions)
	rec = c.do(http.MethodDelete, "/locks", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a resource is required")
	rec = c.do(http.MethodDelete, "/locks?resource=", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
