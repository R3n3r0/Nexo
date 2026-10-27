package admin_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/update"
)

// TestUpdateAuthorizationDenied: the update needs admin:ServerUpdate, its
// status admin:ServerInfo, the coordinated restart and stop the actions of
// the restart and the stop of a node (S-OPS-09, C-ADM-09).
//
// covers: S-OPS-09, C-ADM-09
func TestUpdateAuthorizationDenied(t *testing.T) {
	tests := []struct {
		method, path, body string
		action             policy.Action
	}{
		{http.MethodGet, "/service/update", "", policy.ActionAdminServerInfo},
		{http.MethodPost, "/service/update", `{"url":"https://a.test/nexo","sha256":"00"}`, policy.ActionAdminServerUpdate},
		{http.MethodPost, "/service/cluster/restart", "", policy.ActionAdminServiceRestart},
		{http.MethodPost, "/service/cluster/stop", "", policy.ActionAdminServiceStop},
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
			assert.Equal(t, []policy.Action{tt.action}, f.lastCheck().Actions)
		})
	}
}

// TestUpdateRoutes: the jobs are started and read through the service;
// its errors map to the statuses of the specification.
//
// covers: S-OPS-09, C-ADM-09
func TestUpdateRoutes(t *testing.T) {
	job := update.Job{ID: "j1", Kind: update.KindUpdate, Mode: update.ModeRolling, State: update.JobRunning, Node: "n1:9000",
		Nodes: []update.NodeProgress{{Node: "n1:9000", Phase: update.PhasePending}}}
	t.Run("update", func(t *testing.T) {
		f := newFixture(t)
		f.updates.EXPECT().Update(mock.Anything, update.UpdateInput{URL: "https://a.test/nexo", SHA256: "ab", Mode: update.ModeAll}).Return(job, nil).Once()
		rec := f.root(t).do(http.MethodPost, "/service/update", `{"url":"https://a.test/nexo","sha256":"ab","mode":"all"}`)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"id":"j1"`)
		assert.Contains(t, rec.Body.String(), `"phase":"pending"`)
	})
	t.Run("status", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.updates.EXPECT().Job(mock.Anything).Return(update.Job{}, fmt.Errorf("%w: none", domain.ErrNotFound)).Once()
		assert.Equal(t, http.StatusNotFound, c.do(http.MethodGet, "/service/update", nil).Code)
		f.updates.EXPECT().Job(mock.Anything).Return(job, nil).Once()
		rec := c.do(http.MethodGet, "/service/update", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"state":"running"`)
	})
	t.Run("restart and stop", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.updates.EXPECT().Restart(mock.Anything, update.RestartInput{}).Return(job, nil).Once()
		assert.Equal(t, http.StatusAccepted, c.do(http.MethodPost, "/service/cluster/restart", nil).Code)
		f.updates.EXPECT().Restart(mock.Anything, update.RestartInput{Mode: update.ModeAll}).Return(job, nil).Once()
		assert.Equal(t, http.StatusAccepted, c.do(http.MethodPost, "/service/cluster/restart", `{"mode":"all"}`).Code)
		f.updates.EXPECT().StopAll(mock.Anything).Return(nil).Once()
		assert.Equal(t, http.StatusAccepted, c.do(http.MethodPost, "/service/cluster/stop", nil).Code)
	})
	t.Run("errors", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		for _, tc := range []struct {
			err  error
			want int
		}{
			{domain.ErrConflict, http.StatusConflict},
			{domain.ErrPreconditionFailed, http.StatusPreconditionFailed},
			{domain.ErrNotImplemented, http.StatusNotImplemented},
			{domain.ErrInvalidArgument, http.StatusBadRequest},
		} {
			f.updates.EXPECT().Update(mock.Anything, mock.Anything).Return(update.Job{}, fmt.Errorf("update: %w", tc.err)).Once()
			rec := c.do(http.MethodPost, "/service/update", `{"url":"https://a.test/nexo","sha256":"ab"}`)
			assert.Equal(t, tc.want, rec.Code, "%v: %s", tc.err, rec.Body.String())
		}
		assert.Equal(t, http.StatusBadRequest, c.do(http.MethodPost, "/service/update", `{"url":`).Code)
	})
}
