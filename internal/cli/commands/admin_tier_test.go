package commands_test

import "testing"

// TestScenarioAdminTier: nexoctl admin tier (S-STO-12) against the
// in-memory store: a tier added from a remote alias (its URL, region and
// credentials), listed, its credentials replaced from another alias, its
// removal refused while it holds data, then removed; the lifecycle rules
// transition to it.
//
// covers: S-STO-12
func TestScenarioAdminTier(t *testing.T) {
	h := newHarness(t)
	h.mustRun("alias", "set", "archive", "https://archive.example.test", "ARCHKEY", "ARCHSECRET", "--no-verify")
	h.mustRun("alias", "set", "archive2", "https://archive.example.test", "ARCHKEY2", "ARCHSECRET2", "--no-verify")
	s := h.session()
	s.run("admin", "tier", "ls", "mem")
	s.run("admin", "tier", "add", "mem", "COLD", "archive/cold-data", "--prefix", "nexo/")
	s.run("admin", "tier", "add", "mem", "WARM", "archive/warm", "--storage-class", "STANDARD_IA")
	s.run("admin", "tier", "ls", "mem")
	s.run("admin", "tier", "ls", "--json", "mem")
	s.run("admin", "tier", "edit", "mem", "COLD", "archive2")
	s.run("mb", "mem/app")
	s.run("ilm", "rule", "add", "--id", "cold", "--prefix", "logs/", "--transition-days", "30", "--transition-tier", "COLD", "--noncurrent-transition-days", "7", "mem/app")
	s.run("ilm", "rule", "edit", "--id", "cold", "--transition-days", "-1", "mem/app")
	s.run("ilm", "rule", "ls", "mem/app")
	h.admin.SetTierHoldsData("COLD", true)
	// Usage errors and failures.
	s.run("ilm", "rule", "add", "--transition-days", "30", "mem/app")
	s.run("admin", "tier", "add", "mem", "cold", "archive/x")
	s.run("admin", "tier", "add", "mem", "COLD", "archive/cold-data")
	s.run("admin", "tier", "add", "mem", "OTHER", "archive")
	s.run("admin", "tier", "add", "mem", "OTHER", "ghost/bucket")
	s.run("admin", "tier", "edit", "mem", "NOPE", "archive2")
	s.run("admin", "tier", "rm", "mem", "COLD")
	s.run("admin", "tier", "rm", "mem", "WARM")
	s.run("admin", "tier", "rm", "mem", "WARM")
	s.run("admin", "tier", "ls", "mem")
	golden(t, "scenario/admin_tier", s.b.String())
}
