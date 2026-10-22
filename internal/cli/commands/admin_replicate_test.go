package commands_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestScenarioAdminReplicate: nexoctl admin replicate (C-ADM-11) against
// the in-memory store: the aliases become the sites with their S3 and
// Admin API URLs, the status of the other sites, a round asked, the group
// removed.
//
// covers: C-ADM-11
func TestScenarioAdminReplicate(t *testing.T) {
	h := newHarness(t)
	h.mustRun("alias", "set", "site-b", "https://b.example.test:9000", "MEMKEY", "MEMSECRET", "--no-verify")
	h.mustRun("alias", "set", "site-c", "https://c.example.test", "MEMKEY", "MEMSECRET", "--admin-url", "https://admin.c.example.test", "--no-verify")
	s := h.session()
	s.run("admin", "replicate", "info", "mem")
	s.run("admin", "replicate", "add", "mem", "site-b", "site-c")
	s.run("admin", "replicate", "info", "mem")
	h.admin.SetSiteReplicationPeers([]nexoadmin.SiteReplicationPeerStatus{
		{Name: "site-b", DeploymentID: "dep-site-b", LastSync: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), Pushed: 4, Pulled: 2},
		{Name: "site-c", DeploymentID: "dep-site-c", LastError: "dial tcp: connection refused", Pending: 3},
	})
	s.run("admin", "replicate", "info", "mem")
	s.run("admin", "replicate", "info", "--json", "mem")
	s.run("admin", "replicate", "sync", "mem")
	// Usage errors and failures.
	s.run("admin", "replicate", "add", "mem")
	s.run("admin", "replicate", "add", "mem", "mem")
	s.run("admin", "replicate", "add", "mem", "ghost")
	s.run("admin", "replicate", "add", "mem", "site-b")
	s.run("admin", "replicate", "remove", "mem")
	s.run("admin", "replicate", "remove", "mem")
	s.run("admin", "replicate", "sync", "mem")
	golden(t, "scenario/admin_replicate", s.b.String())
	assert.Equal(t, 1, h.admin.SiteReplicationSyncs())
}
