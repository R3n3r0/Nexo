package commands_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// badPolicy has a JSON syntax error on line 4 (a missing comma).
const badPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": "s3:GetObject" "Resource": "arn:aws:s3:::pub/*"}
  ]
}
`

const goodPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "ReadPub", "Effect": "Allow", "Action": ["s3:GetObject", "s3:ListBucket"], "Resource": ["arn:aws:s3:::pub/*"]}
  ]
}
`

// covers: C-ADM-01, C-ADM-07, C-ADM-09, TCL-E-15
func TestScenarioAdminServer(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("admin", "info", "mem")
	s.run("admin", "info", "mem/bucket")
	s.run("admin", "info", "nope")
	info := nexoadmin.ServerInfo{
		Version: "dev", Mode: "distributed", Region: "eu-1", DeploymentID: "d-2", UptimeSeconds: 60,
		Pools: []nexoadmin.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 4, Parity: 2}},
		Nodes: []nexoadmin.NodeInfo{
			{Endpoint: "http://node1:9000", State: "online", UptimeSeconds: 60, Disks: []nexoadmin.DiskInfo{
				{Path: "/data1", State: "online", Total: 10 << 30, Used: 1 << 30, Free: 9 << 30},
				{Path: "/data2", State: "online", Total: 10 << 30, Used: 1 << 30, Free: 9 << 30, Healing: true},
			}},
			{Endpoint: "http://node2:9000", State: "offline", Disks: []nexoadmin.DiskInfo{
				{Endpoint: "http://node2:9000/data1", State: "offline", Error: "connection refused"},
			}},
		},
	}
	h.admin.SetServerInfo(info)
	s.run("admin", "info", "mem")
	s.run("admin", "usage", "mem")
	s.run("admin", "heal", "--recursive", "--dry-run", "mem/photos")
	h.admin.FailOn("Heal", clitestNotImplemented())
	s.run("admin", "heal", "mem")
	h.stdin = "n\n"
	s.run("admin", "service", "restart", "mem")
	h.stdin = "yes\n"
	s.run("admin", "service", "restart", "mem")
	s.run("admin", "service", "stop", "--yes", "mem")
	s.run("admin", "service", "stop", "--json", "mem")
	golden(t, "scenario/admin_server", s.b.String())
	assert.Equal(t, 1, h.admin.Calls("RestartService"), "restart only after the confirmation")
	assert.Equal(t, 1, h.admin.Calls("StopService"))
}

// clitestNotImplemented is the 501 of an endpoint the server does not
// implement yet.
func clitestNotImplemented() error {
	return cli.MapAdminError(&nexoadmin.APIError{Status: http.StatusNotImplemented, Code: "not_implemented", Detail: "admin: heal: not implemented"}, clitest.MemAdminEndpoint)
}

// covers: C-ADM-02, C-ADM-03, TCL-E-10
func TestScenarioAdminUsersAndGroups(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("admin", "user", "list", "mem")
	s.run("admin", "user", "add", "mem", "alice", "alice-secret", "--policy", "readwrite")
	s.run("admin", "user", "add", "mem", "bob", "bob-secret-1", "--policy", "readonly,diagnostics")
	s.run("admin", "user", "add", "mem", "carol", "carol-secret", "--disabled")
	s.run("admin", "user", "add", "mem", "alice", "alice-secret")
	s.run("admin", "user", "add", "mem", "dave", "dave-secret", "--policy", "missing")
	s.run("admin", "user", "list", "mem")
	s.run("admin", "user", "info", "mem", "alice")
	s.run("admin", "user", "disable", "mem", "alice", "bob")
	s.run("admin", "user", "enable", "mem", "alice")
	s.run("admin", "user", "info", "mem", "ghost")
	s.run("admin", "group", "add", "mem", "devs", "alice", "bob", "--policy", "writeonly")
	s.run("admin", "group", "add", "mem", "devs", "carol")
	s.run("admin", "group", "add", "mem", "devs")
	s.run("admin", "group", "add", "mem", "ops")
	s.run("admin", "group", "list", "mem")
	s.run("admin", "group", "info", "mem", "devs")
	s.run("admin", "group", "members", "mem", "devs")
	s.run("admin", "group", "members", "mem", "ops")
	s.run("admin", "user", "policy", "mem", "alice")
	s.run("admin", "group", "disable", "mem", "devs")
	s.run("admin", "user", "policy", "mem", "alice")
	s.run("admin", "group", "enable", "mem", "devs")
	s.run("admin", "group", "remove", "mem", "devs", "carol")
	s.run("admin", "group", "remove", "mem", "ops")
	s.run("admin", "group", "info", "mem", "ops")
	s.run("admin", "user", "remove", "mem", "carol", "ghost")
	s.run("admin", "user", "list", "mem")
	// A user without admin permissions is denied (exit code 4).
	h.mustRun("alias", "set", "bob", "http://s3.mem.test", "bob", "bob-secret-1")
	s.run("admin", "user", "list", "bob")
	golden(t, "scenario/admin_users", s.b.String())
	_, ok := h.admin.User("carol")
	assert.False(t, ok)
}

// covers: C-ADM-04, TCL-E-11, TCL-E-12, TCL-P-05
func TestScenarioAdminPolicies(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret")
	h.write("bad.json", badPolicy)
	h.write("good.json", goodPolicy)
	h.write("noeffect.json", `{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"s3:GetObject"}]}`)
	s := h.session()
	s.run("admin", "policy", "list", "mem")
	s.run("admin", "policy", "create", "mem", "pub-read", "bad.json")
	s.run("admin", "policy", "create", "mem", "pub-read", "bad.json", "--json")
	s.run("admin", "policy", "validate", "mem", "bad.json")
	s.run("admin", "policy", "validate", "mem", "noeffect.json")
	s.run("admin", "policy", "validate", "mem", "good.json")
	s.run("admin", "policy", "create", "mem", "pub-read", "good.json")
	s.run("admin", "policy", "create", "mem", "pub-read", "good.json")
	h.stdin = goodPolicy
	s.run("admin", "policy", "create", "--update", "mem", "pub-read", "-")
	s.run("admin", "policy", "create", "mem", "x", "missing.json")
	s.run("admin", "policy", "info", "mem", "pub-read")
	s.run("admin", "policy", "simulate", "--user", "alice", "--action", "s3:GetObject", "mem/pub/a.txt")
	s.run("admin", "policy", "attach", "mem", "pub-read", "--user", "alice")
	s.run("admin", "policy", "attach", "mem", "pub-read")
	s.run("admin", "policy", "attach", "mem", "pub-read", "--user", "alice", "--group", "g")
	s.run("admin", "policy", "entities", "mem", "pub-read")
	s.run("admin", "policy", "simulate", "--user", "alice", "--action", "s3:GetObject", "mem/pub/a.txt")
	s.run("admin", "policy", "simulate", "--user", "alice", "--action", "s3:PutObject", "--verbose", "mem/pub/a.txt")
	s.run("admin", "policy", "simulate", "--action", "s3:PutObject", "--condition", "s3:prefix=a/", "--source-ip", "10.0.0.1", "--secure", "mem/pub")
	s.run("admin", "policy", "simulate", "mem/pub")
	s.run("admin", "policy", "simulate", "--action", "s3:GetObject", "--time", "yesterday", "mem/pub")
	s.run("admin", "policy", "detach", "mem", "pub-read", "--user", "alice")
	s.run("admin", "policy", "detach", "mem", "pub-read", "--user", "alice")
	s.run("admin", "policy", "remove", "mem", "pub-read")
	s.run("admin", "policy", "remove", "mem", "readonly")
	s.run("admin", "policy", "info", "mem", "pub-read")
	golden(t, "scenario/admin_policies", s.b.String())
}

// covers: C-ADM-05, TCL-E-13
func TestScenarioAdminAccessKeys(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.write("inline.json", goodPolicy)
	h.write("broken.json", "{\n  \"Statement\": [\n}\n")
	s := h.session()
	s.run("admin", "accesskey", "list", "mem")
	s.run("admin", "accesskey", "create", "mem", "--user", "alice", "--expiry", "30d", "--policy", "inline.json", "--name", "ci", "--description", "CI uploads")
	s.run("admin", "accesskey", "create", "mem", "--access-key", "AKFIXED", "--secret-key", "fixed-secret-key", "--expiry", "2026-03-01")
	s.run("admin", "accesskey", "create", "mem", "--json", "--user", "alice")
	s.run("admin", "accesskey", "create", "mem", "--expiry", "soon")
	s.run("admin", "accesskey", "create", "mem", "--expiry", "2020-01-01")
	s.run("admin", "accesskey", "create", "mem", "--policy", "broken.json")
	s.run("admin", "accesskey", "list", "mem")
	s.run("admin", "accesskey", "list", "mem", "--user", "alice")
	s.run("admin", "accesskey", "info", "mem", "AKMEM0001")
	s.run("admin", "accesskey", "edit", "mem", "AKMEM0001")
	s.run("admin", "accesskey", "edit", "mem", "AKMEM0001", "--enable", "--disable")
	s.run("admin", "accesskey", "edit", "mem", "AKMEM0001", "--disable", "--clear-expiry", "--clear-policy", "--name", "ci-old")
	s.run("admin", "accesskey", "edit", "mem", "AKMEM0001", "--expiry", "7d", "--policy", "inline.json")
	s.run("admin", "accesskey", "info", "mem", "AKMEM0001")
	s.run("admin", "accesskey", "remove", "mem", "AKMEM0001", "AKMEM0001")
	s.run("admin", "accesskey", "info", "mem", "AKMEM0001")
	golden(t, "scenario/admin_accesskeys", s.b.String())
}

// TestAccessKeyExpiryRoundsUp: --expiry is rounded up to the second, so a
// key is valid at least for the requested duration (regression: the
// truncation left a key created at .947 with 2.05 s out of 3 s, and one
// with --expiry 1s valid for a few milliseconds).
func TestAccessKeyExpiryRoundsUp(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		expiry string
		want   time.Time
	}{
		{name: "late in the second", offset: 947 * time.Millisecond, expiry: "3s", want: t0.Add(4 * time.Second)},
		{name: "one second", offset: 999 * time.Millisecond, expiry: "1s", want: t0.Add(2 * time.Second)},
		{name: "whole second unchanged", expiry: "3s", want: t0.Add(3 * time.Second)},
		{name: "date unchanged", offset: 500 * time.Millisecond, expiry: "2027-01-01", want: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.clock.Set(t0.Add(tt.offset))
			r := h.mustRun("admin", "accesskey", "create", "mem", "--json", "--expiry", tt.expiry)
			var rec struct {
				Expiration time.Time `json:"expiration"`
			}
			require.NoError(t, json.Unmarshal([]byte(strings.SplitN(r.stdout, "\n", 2)[0]), &rec), r.stdout)
			assert.True(t, rec.Expiration.Equal(tt.want), "expiration %s, want %s", rec.Expiration, tt.want)
			assert.GreaterOrEqual(t, rec.Expiration.Sub(h.clock.Now()), mustDuration(t, tt.expiry), "valid at least for the requested duration")
		})
	}
}

// mustDuration parses a duration of --expiry, or 0 for a date.
func mustDuration(t *testing.T, s string) time.Duration {
	t.Helper()
	d, err := cli.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// covers: C-ADM-06, TCL-E-14
func TestScenarioAdminConfig(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("admin", "config", "get", "mem")
	s.run("admin", "config", "set", "mem", "region", "name=eu-south-1", "comment=Milan DC")
	s.run("admin", "config", "set", "mem", "notify_webhook", "endpoint=http://hook.test/x", "auth_token=t0ken")
	s.run("admin", "config", "set", "mem", "logger", "level=verbose")
	s.run("admin", "config", "set", "mem", "region", "name")
	s.run("admin", "config", "get", "mem", "region", "notify_webhook")
	s.run("admin", "config", "get", "--json", "mem", "notify_webhook")
	s.run("admin", "config", "export", "mem")
	s.run("admin", "config", "export", "mem", "--output", "backup.json")
	s.run("admin", "config", "reset", "mem", "region", "comment")
	s.run("admin", "config", "get", "mem", "region")
	s.run("admin", "config", "reset", "mem", "region")
	s.run("admin", "config", "import", "mem", "backup.json")
	s.run("admin", "config", "get", "mem", "region")
	s.run("admin", "config", "get", "mem", "nope")
	h.write("broken.json", `{"region": {"name": }}`)
	s.run("admin", "config", "import", "mem", "broken.json")
	golden(t, "scenario/admin_config", s.b.String())

	// The round trip is identical (TCL-E-14).
	first := h.read("backup.json")
	h.mustRun("admin", "config", "reset", "mem", "notify_webhook")
	h.mustRun("admin", "config", "import", "mem", "backup.json")
	h.mustRun("admin", "config", "export", "mem", "--output", "again.json")
	assert.Equal(t, first, h.read("again.json"))
}

// covers: C-OBJ-07, S-OBJ-08c, TCL-P-01, TCL-P-03
func TestScenarioManagedShares(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("pub")
	h.mem.PutObject("pub", "docs/report.pdf", "%PDF", clitest.ObjectSpec{})
	h.mem.PutObject("pub", "docs/2026/a.txt", "a", clitest.ObjectSpec{})
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.mustRun("alias", "set", "alice", "http://s3.mem.test", "alice", "alice-secret")
	s := h.session()
	s.run("share", "list", "mem")
	s.run("share", "download", "--expire", "2h", "--content-disposition", "attachment", "mem/pub/docs/report.pdf")
	s.run("share", "download", "--recursive", "--expire", "1h", "mem/pub/docs/")
	s.run("share", "upload", "--expire", "1h", "--content-type", "application/pdf", "mem/pub/inbox/new.pdf")
	s.run("share", "upload", "--managed", "--post", "mem/pub/inbox/")
	s.run("share", "download", "--managed", "--raw", "mem/pub/docs/report.pdf")
	s.run("share", "download", "--json", "alice/pub/docs/report.pdf")
	s.run("share", "list", "mem")
	s.run("share", "list", "mem/pub", "--user", "alice")
	s.run("share", "list", "alice")
	s.run("share", "list", "alice", "--user", "admin")
	s.run("share", "list", "mem/pub/docs")
	s.run("share", "revoke", "mem", "sh0001")
	s.run("share", "revoke", "mem", "sh0001")
	s.run("share", "revoke", "mem")
	s.run("share", "revoke", "--all", "mem", "sh0002")
	s.run("share", "revoke", "--user", "alice", "mem", "sh0002")
	s.run("share", "revoke", "alice", "sh0002")
	s.run("share", "revoke", "--all", "--user", "alice", "mem")
	s.run("share", "revoke", "--all", "mem")
	s.run("share", "list", "mem")
	golden(t, "scenario/managed_shares", s.b.String())
	assert.Empty(t, h.admin.ShareIDs())
}

// covers: TCL-E-18 (admin and managed share commands)
func TestAdminCommandsPrintJSONLines(t *testing.T) {
	h := newHarness(t)
	h.mem.MakeBucket("pub")
	h.mem.PutObject("pub", "a.txt", "a", clitest.ObjectSpec{})
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.write("good.json", goodPolicy)
	h.write("cfg.json", `{"region":{"name":"eu-1"}}`)
	required := map[string][]string{
		"info":      {"alias", "version", "mode", "nodes", "pools"},
		"usage":     {"alias", "totalSize", "buckets"},
		"heal":      {"alias", "id", "dryRun", "scanned"},
		"user":      {"alias", "accessKey", "state", "policyNames", "groups"},
		"group":     {"alias", "name", "state", "members", "policyNames"},
		"member":    {"alias", "group", "member"},
		"policy":    {"alias", "name", "builtIn"},
		"document":  {"alias", "name", "builtIn", "document"},
		"entities":  {"alias", "policy", "users", "groups"},
		"simulate":  {"alias", "user", "action", "allowed", "decision", "reason", "evaluated"},
		"validate":  {"alias", "file", "valid", "errors"},
		"key":       {"alias", "accessKey", "parentUser", "state"},
		"newkey":    {"alias", "accessKey", "secretKey", "parentUser", "state"},
		"config":    {"alias", "subsystem", "settings"},
		"export":    {"alias", "config"},
		"action":    {"alias", "type", "action"},
		"share":     {"alias", "id", "target", "owner", "bucket", "key", "permission", "managed", "method", "url", "expire", "expiresAt", "accessKey"},
		"listshare": {"alias", "id", "target", "owner", "bucket", "key", "permission", "managed", "expiresAt", "accessKey"},
		"userpol":   {"alias", "accessKey", "direct", "groups", "effective"},
	}
	tests := []struct {
		kind string
		args []string
	}{
		{"info", []string{"admin", "info", "mem"}},
		{"usage", []string{"admin", "usage", "mem"}},
		{"heal", []string{"admin", "heal", "--dry-run", "mem"}},
		{"user", []string{"admin", "user", "add", "mem", "bob", "bob-secret-1"}},
		{"user", []string{"admin", "user", "list", "mem"}},
		{"user", []string{"admin", "user", "info", "mem", "bob"}},
		{"userpol", []string{"admin", "user", "policy", "mem", "alice"}},
		{"action", []string{"admin", "user", "disable", "mem", "bob"}},
		{"group", []string{"admin", "group", "add", "mem", "devs", "alice", "bob"}},
		{"group", []string{"admin", "group", "list", "mem"}},
		{"group", []string{"admin", "group", "info", "mem", "devs"}},
		{"member", []string{"admin", "group", "members", "mem", "devs"}},
		{"group", []string{"admin", "group", "remove", "mem", "devs", "bob"}},
		{"action", []string{"admin", "group", "disable", "mem", "devs"}},
		{"policy", []string{"admin", "policy", "create", "mem", "pub-read", "good.json"}},
		{"policy", []string{"admin", "policy", "list", "mem"}},
		{"document", []string{"admin", "policy", "info", "mem", "pub-read"}},
		{"action", []string{"admin", "policy", "attach", "mem", "pub-read", "--user", "alice"}},
		{"entities", []string{"admin", "policy", "entities", "mem", "pub-read"}},
		{"simulate", []string{"admin", "policy", "simulate", "--user", "alice", "--action", "s3:GetObject", "mem/pub/a.txt"}},
		{"validate", []string{"admin", "policy", "validate", "mem", "good.json"}},
		{"action", []string{"admin", "policy", "detach", "mem", "pub-read", "--user", "alice"}},
		{"newkey", []string{"admin", "accesskey", "create", "mem", "--expiry", "1d", "--access-key", "AKJSON"}},
		{"key", []string{"admin", "accesskey", "list", "mem"}},
		{"key", []string{"admin", "accesskey", "info", "mem", "AKJSON"}},
		{"key", []string{"admin", "accesskey", "edit", "mem", "AKJSON", "--disable"}},
		{"action", []string{"admin", "accesskey", "remove", "mem", "AKJSON"}},
		{"config", []string{"admin", "config", "set", "mem", "api", "root_access=on"}},
		{"config", []string{"admin", "config", "get", "mem"}},
		{"export", []string{"admin", "config", "export", "mem"}},
		{"action", []string{"admin", "config", "import", "mem", "cfg.json"}},
		{"action", []string{"admin", "config", "reset", "mem", "region"}},
		{"share", []string{"share", "download", "mem/pub/a.txt"}},
		{"share", []string{"share", "upload", "--content-type", "text/plain", "mem/pub/b.txt"}},
		{"listshare", []string{"share", "list", "mem"}},
		{"action", []string{"share", "revoke", "mem", "sh0003"}},
		{"action", []string{"share", "revoke", "--all", "mem"}},
		{"action", []string{"admin", "policy", "remove", "mem", "pub-read"}},
		{"action", []string{"admin", "group", "remove", "mem", "devs"}},
		{"action", []string{"admin", "user", "remove", "mem", "bob"}},
		{"action", []string{"admin", "service", "restart", "--yes", "mem"}},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			r := h.run(append(tc.args, "--json")...)
			require.Equal(t, 0, r.code, "stderr: %s", r.stderr)
			assert.Empty(t, r.stderr)
			recs := jsonLines(t, r.stdout)
			require.NotEmpty(t, recs)
			for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
				assertNoDuplicateKeys(t, line)
			}
			for _, rec := range recs {
				assert.Equal(t, "success", rec["status"])
				for _, k := range required[tc.kind] {
					assert.Contains(t, rec, k, "record %s lacks %q", tc.kind, k)
				}
			}
		})
	}

	// Errors of the Admin API are JSON on stderr, with the located details
	// of a malformed policy (TCL-E-11).
	h.write("bad.json", badPolicy)
	r := h.run("admin", "policy", "create", "mem", "bad", "bad.json", "--json")
	assert.Equal(t, cli.ExitError, r.code)
	errs := jsonLines(t, r.stderr)
	require.Len(t, errs, 1)
	e := errs[0]["error"].(map[string]any)
	assert.Equal(t, "Error", e["code"])
	details := e["details"].([]any)
	require.Len(t, details, 1)
	assert.Equal(t, float64(4), details[0].(map[string]any)["line"])
	assert.Equal(t, float64(50), details[0].(map[string]any)["column"])
}

// assertNoDuplicateKeys checks that a JSON object has no repeated member
// (encoding/json would silently keep the last one).
func assertNoDuplicateKeys(t *testing.T, line string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(line)))
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k := kt.(string)
				if seen[k] {
					return fmt.Errorf("duplicate member %q in %s", k, path)
				}
				seen[k] = true
				if err := walk(path + "." + k); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk("$"); err != nil && err != io.EOF {
		t.Errorf("%v: %s", err, line)
	}
}

// covers: TCL-U-04 (admin)
func TestAdminExitCodes(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.mustRun("alias", "set", "alice", "http://s3.mem.test", "alice", "alice-secret")
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"ok", []string{"admin", "info", "mem"}, cli.ExitOK},
		{"unknown subcommand", []string{"admin", "frob"}, cli.ExitUsage},
		{"missing alias", []string{"admin", "info"}, cli.ExitUsage},
		{"bucket instead of alias", []string{"admin", "user", "list", "mem/bkt"}, cli.ExitUsage},
		{"unknown alias", []string{"admin", "info", "nope"}, cli.ExitNotFound},
		{"missing user", []string{"admin", "user", "info", "mem", "ghost"}, cli.ExitNotFound},
		{"missing group", []string{"admin", "group", "info", "mem", "ghost"}, cli.ExitNotFound},
		{"missing policy", []string{"admin", "policy", "info", "mem", "ghost"}, cli.ExitNotFound},
		{"missing access key", []string{"admin", "accesskey", "info", "mem", "ghost"}, cli.ExitNotFound},
		{"missing share", []string{"share", "revoke", "mem", "ghost"}, cli.ExitNotFound},
		{"missing policy file", []string{"admin", "policy", "create", "mem", "p", "missing.json"}, cli.ExitNotFound},
		{"denied", []string{"admin", "user", "list", "alice"}, cli.ExitAccess},
		{"denied info", []string{"admin", "info", "alice"}, cli.ExitAccess},
		{"conflict", []string{"admin", "user", "add", "mem", "alice", "alice-secret"}, cli.ExitError},
		{"invalid", []string{"admin", "config", "get", "mem", "nope"}, cli.ExitError},
		{"no confirmation in JSON", []string{"admin", "service", "stop", "--json", "mem"}, cli.ExitUsage},
		{"attach without target", []string{"admin", "policy", "attach", "mem", "readonly"}, cli.ExitUsage},
		{"edit without changes", []string{"admin", "accesskey", "edit", "mem", "k"}, cli.ExitUsage},
		{"bad setting", []string{"admin", "config", "set", "mem", "region", "name"}, cli.ExitUsage},
		{"share revoke without ID", []string{"share", "revoke", "mem"}, cli.ExitUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run(tc.args...)
			assert.Equal(t, tc.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
		})
	}
}

// covers: C-UX-01 (dynamic completion of users, groups and policies)
func TestAdminCompletion(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret")
	h.admin.AddUser("albert", "albert-secret")
	h.admin.AddUser("bob", "bob-secret")
	h.mustRun("admin", "group", "add", "mem", "devs", "alice")
	complete := func(args ...string) []string {
		r := h.mustRun(append([]string{"__complete"}, args...)...)
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
			if !strings.HasPrefix(line, ":") && line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	assert.Equal(t, []string{"mem"}, complete("admin", "user", "info", "m"))
	assert.Equal(t, []string{"albert", "alice"}, complete("admin", "user", "info", "mem", "al"))
	assert.Equal(t, []string{"albert", "alice", "bob"}, complete("admin", "user", "disable", "mem", "alice", ""))
	assert.Equal(t, []string{"readonly", "readwrite"}, complete("admin", "policy", "info", "mem", "read"))
	assert.Equal(t, []string{"readonly", "readwrite"}, complete("admin", "policy", "attach", "mem", "read"))
	assert.Equal(t, []string{"bob"}, complete("admin", "policy", "attach", "mem", "readonly", "--user", "b"))
	assert.Equal(t, []string{"devs"}, complete("admin", "policy", "attach", "mem", "readonly", "--group", ""))
	assert.Equal(t, []string{"devs"}, complete("admin", "group", "info", "mem", ""))
	assert.Equal(t, []string{"albert", "alice", "bob"}, complete("admin", "group", "add", "mem", "devs", ""))
	assert.Equal(t, []string{"diagnostics"}, complete("admin", "user", "add", "mem", "x", "y", "--policy", "d"))
	assert.Equal(t, []string{"api", "logger", "notify_webhook", "region"}, complete("admin", "config", "get", "mem", ""))
	assert.Equal(t, []string{"alice"}, complete("share", "list", "mem", "--user", "ali"))
}

// covers: C-ADM-12, S-OPS-01 (a key the server does not apply yet is
// reported, so that an administrator does not believe it took effect)
func TestAdminConfigSetWarnsAboutUnappliedKeys(t *testing.T) {
	h := newHarness(t)
	r := h.run("admin", "config", "set", "mem", "region", "name=eu-2", "comment=milan")
	require.Equal(t, 0, r.code, r.stderr)
	assert.Contains(t, r.stderr, "region.comment is stored but not applied yet by the server")
	assert.Contains(t, r.stderr, "region.name is stored but not applied yet by the server")
	r = h.run("admin", "config", "set", "mem", "api", "root_access=off")
	require.Equal(t, 0, r.code, r.stderr)
	assert.Empty(t, r.stderr, "api.root_access is applied")
}
