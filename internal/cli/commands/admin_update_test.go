package commands_test

import "testing"

// TestScenarioAdminUpdate: nexoctl admin update and admin service
// restart|stop --cluster (S-OPS-09, C-ADM-09) against the in-memory
// store: the status before any job, an update of the nodes followed to its
// end, its status, a failed staging, the coordinated restart (rolling and
// all at once) and stop, the usage errors.
//
// covers: S-OPS-09, C-ADM-09
func TestScenarioAdminUpdate(t *testing.T) {
	h := newHarness(t)
	h.admin.SetUpdateNodes("node1:9000", "node2:9000", "node3:9000")
	s := h.session()
	sum := "3f5a0c3e1b2d4f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8"
	s.run("admin", "update", "--status", "mem")
	s.run("admin", "update", "--yes", "--sha256", sum, "mem", "https://dl.example.test/nexo-v1.1.0-linux-amd64")
	s.run("admin", "update", "--status", "mem")
	s.run("admin", "update", "--status", "--json", "mem")
	s.run("admin", "update", "--yes", "--no-wait", "--all-at-once", "--sha256", sum, "mem", "https://dl.example.test/nexo")
	s.run("admin", "update", "--yes", "--sha256", sum, "mem", "https://dl.example.test/broken")
	s.run("admin", "service", "restart", "--cluster", "--yes", "mem")
	s.run("admin", "service", "restart", "--cluster", "--all-at-once", "--yes", "--json", "mem")
	s.run("admin", "service", "stop", "--cluster", "--yes", "mem")
	// Usage errors and failures.
	s.run("admin", "update", "--yes", "mem", "https://dl.example.test/nexo")
	s.run("admin", "update", "--yes", "--sha256", "abc", "mem", "https://dl.example.test/nexo")
	s.run("admin", "update", "--yes", "--sha256", sum, "mem", "ftp://dl.example.test/nexo")
	s.run("admin", "update", "--sha256", sum, "--json", "mem", "https://dl.example.test/nexo")
	s.run("admin", "update", "mem")
	golden(t, "scenario/admin_update", s.b.String())
}
