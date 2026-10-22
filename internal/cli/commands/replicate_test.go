package commands_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestScenarioReplicate: nexoctl replicate (S-BKT-12) against the
// in-memory store: the remote alias becomes a target of the bucket with its
// credentials, the rules are added, replaced, listed and removed with their
// targets, the status and the resync.
//
// covers: S-BKT-12
func TestScenarioReplicate(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("photos")
	h.mustRun("alias", "set", "backup", "https://backup.example.test:9000", "backup-key", "backup-secret", "--no-verify")
	s := h.session()
	s.run("replicate", "ls", "mem/photos")
	s.run("replicate", "add", "mem/photos", "backup/photos")
	s.run("replicate", "add", "mem/photos", "backup/photos", "--id", "docs", "--prefix", "docs/", "--tags", "class=public&tier=hot", "--existing", "--deletes")
	s.run("replicate", "add", "mem/photos", "backup/archive", "--no-delete-markers", "--disable", "--json")
	s.run("replicate", "add", "mem/photos", "backup/photos", "--id", "docs", "--prefix", "docs/v2/", "--tags", "class=public", "--existing")
	s.run("replicate", "ls", "mem/photos")
	s.run("replicate", "ls", "--json", "mem/photos")
	s.run("replicate", "status", "mem/photos")
	targets := h.admin.ReplicationTargets("photos")
	require.Len(t, targets, 2)
	for _, target := range targets {
		assert.Equal(t, "https://backup.example.test:9000", target.Endpoint)
		assert.Equal(t, "backup-key", target.AccessKey)
		assert.Equal(t, "backup-secret", target.SecretKey)
	}
	last := time.Date(2026, 1, 2, 3, 4, 7, 0, time.UTC)
	h.admin.SetReplicationStatus("photos", nexoadmin.ReplicationStatus{Queued: 3, Targets: []nexoadmin.ReplicationTargetStatus{
		{ARN: targets[0].ARN, Online: true, Queued: 3, Replicated: 120, Bytes: 5 << 20, LastOK: last},
		{ARN: targets[1].ARN, Failed: 2, LastError: "dial tcp: connection refused", LastFailed: last},
	}})
	s.run("replicate", "resync", "mem/photos", "backup/photos")
	s.run("replicate", "status", "mem/photos")
	s.run("replicate", "status", "--json", "mem/photos")
	// Usage errors and failures.
	s.run("replicate", "resync", "mem/photos", "backup/photos")
	s.run("replicate", "resync", "mem/photos", "backup/other")
	s.run("replicate", "add", "mem/photos", "mem/photos")
	s.run("replicate", "add", "mem/photos", "backup/photos/key")
	s.run("replicate", "add", "mem/photos", "backup/photos", "--tags", "novalue")
	s.run("replicate", "add", "mem/photos", "ghost/photos")
	s.run("replicate", "rm", "mem/photos")
	s.run("replicate", "rm", "mem/photos", "--id", "ghost")
	// Removing a rule removes the target no rule uses any more.
	s.run("replicate", "rm", "mem/photos", "--id", "backup-archive")
	assert.Len(t, h.admin.ReplicationTargets("photos"), 1)
	s.run("replicate", "rm", "mem/photos", "--all")
	s.run("replicate", "ls", "mem/photos")
	assert.Empty(t, h.admin.ReplicationTargets("photos"))
	golden(t, "scenario/replicate", s.b.String())
}
