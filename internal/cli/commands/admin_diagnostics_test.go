package commands_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
)

// covers: S-OPS-11, C-ADM-12
func TestAdminProfile(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("diag", "diag-secret-1", "diagnostics")
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.mustRun("alias", "set", "diag", "http://s3.mem.test", "diag", "diag-secret-1")
	h.mustRun("alias", "set", "alice", "http://s3.mem.test", "alice", "alice-secret")

	r := h.mustRun("admin", "profile", "mem")
	assert.Contains(t, r.stdout, "Wrote the cpu profile of mem:9000")
	assert.Equal(t, "pprof:cpu", h.read("nexo-mem-9000-cpu.pb.gz"), "the name suggested by the server")

	r = h.mustRun("admin", "profile", "--type", "heap", "--output", "heap.out", "--json", "diag")
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &rec))
	assert.Equal(t, "heap.out", rec["file"])
	assert.Equal(t, "heap", rec["type"])
	assert.Equal(t, "pprof:heap", h.read("heap.out"))

	tests := []struct {
		name string
		args []string
		code int
	}{
		{"unknown type", []string{"admin", "profile", "--type", "threads", "mem"}, cli.ExitUsage},
		{"fractional duration", []string{"admin", "profile", "--duration", "1500ms", "mem"}, cli.ExitUsage},
		{"over the limit of the server", []string{"admin", "profile", "--duration", "10m", "mem"}, cli.ExitError},
		{"denied without admin:Profiling", []string{"admin", "profile", "alice"}, cli.ExitAccess},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run(tc.args...)
			assert.Equal(t, tc.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
		})
	}
}

// covers: S-OPS-11, C-ADM-12
func TestAdminSpeedtest(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.mustRun("alias", "set", "alice", "http://s3.mem.test", "alice", "alice-secret")

	r := h.mustRun("admin", "speedtest", "mem", "drive")
	assert.Contains(t, r.stdout, "Node mem:9000, drive 4.0MiB")
	assert.Contains(t, r.stdout, "/data1")
	assert.Regexp(t, `/data1\s+write`, r.stdout)
	assert.Regexp(t, `/data1\s+read`, r.stdout)

	r = h.mustRun("admin", "speedtest", "--size", "1MiB", "--duration", "2s", "--concurrency", "4", "--json", "mem", "OBJECT")
	var rec struct {
		Alias           string  `json:"alias"`
		Kind            string  `json:"kind"`
		Size            int64   `json:"size"`
		DurationSeconds float64 `json:"durationSeconds"`
		Concurrency     int     `json:"concurrency"`
		Targets         []struct {
			Target string `json:"target"`
		} `json:"targets"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &rec))
	assert.Equal(t, "mem", rec.Alias)
	assert.Equal(t, "object", rec.Kind)
	assert.Equal(t, int64(1<<20), rec.Size)
	assert.InDelta(t, 2.0, rec.DurationSeconds, 1e-9)
	assert.Equal(t, 4, rec.Concurrency)
	require.Len(t, rec.Targets, 1)

	tests := []struct {
		name string
		args []string
		code int
	}{
		{"unknown kind", []string{"admin", "speedtest", "mem", "gpu"}, cli.ExitUsage},
		{"missing kind", []string{"admin", "speedtest", "mem"}, cli.ExitUsage},
		{"bad size", []string{"admin", "speedtest", "--size", "lots", "mem", "drive"}, cli.ExitUsage},
		{"net out of a cluster", []string{"admin", "speedtest", "mem", "net"}, cli.ExitError},
		{"denied without admin:Speedtest", []string{"admin", "speedtest", "alice", "drive"}, cli.ExitAccess},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run(tc.args...)
			assert.Equal(t, tc.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
		})
	}
}
