package commands_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covers: TCL-A-01, TCL-A-02 (unit side; the e2e side is test/cli/testdata/access.txtar)
func TestScenarioAccessRules(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret")
	s := h.session()
	s.run("access", "rule", "list", "mem/media")
	s.run("access", "rule", "add", "mem/media", "--id", "img-pub", "--allow", "--principal", "*", "--action", "read",
		"--regex", `^img/[0-9]{4}/.*\.(jpe?g|png)$`, "--ignore-case")
	s.run("access", "rule", "add", "mem/media", "--id", "no-private", "--deny", "--principal", "*", "--action", "all",
		"--glob", "**/private/**", "--priority", "5")
	s.run("access", "rule", "add", "mem/media", "--id", "docs", "--allow", "--principal", "user:alice", "--action", "read",
		"--action", "list", "--prefix", "docs/", "--source-ip", "10.0.0.0/8", "--description", "documents of alice")
	// Invalid rules: a duplicate id, a bad regex, a missing effect.
	s.run("access", "rule", "add", "mem/media", "--id", "img-pub", "--allow", "--principal", "*", "--action", "read")
	s.run("access", "rule", "add", "mem/media", "--id", "bad", "--allow", "--principal", "*", "--action", "read", "--regex", "([")
	s.run("access", "rule", "add", "mem/media", "--id", "x", "--principal", "*", "--action", "read")
	s.run("access", "rule", "add", "mem/media", "--id", "x", "--allow", "--principal", "*", "--action", "read", "--prefix", "a", "--glob", "b")
	s.run("access", "rule", "list", "mem/media")
	s.run("access", "rule", "info", "mem/media", "--id", "img-pub")
	s.run("access", "rule", "info", "mem/media", "--id", "nope")
	s.run("access", "test", "mem/media/img/2026/a.JPG", "mem/media/img/2026/private/b.jpg", "mem/media/docs/x.txt")
	s.run("access", "test", "mem/media/docs/x.txt", "--principal", "user:alice", "--ip", "10.1.2.3", "-v")
	s.run("access", "test", "mem/media/docs/x.txt", "--principal", "user:alice", "--ip", "192.0.2.1")
	s.run("access", "rule", "move", "mem/media", "--id", "img-pub", "--priority", "1")
	s.run("access", "rule", "list", "mem/media")
	// TCL-A-02: export, import into another bucket, same rules.
	s.run("access", "export", "mem/media", "rules.yaml")
	s.b.WriteString("--- rules.yaml\n" + h.read("rules.yaml") + "\n")
	s.run("access", "import", "mem/copy", "rules.yaml")
	exported := h.mustRun("access", "rule", "list", "mem/media", "--json").stdout
	imported := h.mustRun("access", "rule", "list", "mem/copy", "--json").stdout
	assert.Equal(t, exported, strings.ReplaceAll(imported, `"bucket":"copy"`, `"bucket":"media"`), "the import reproduces the rules")
	s.run("access", "rule", "remove", "mem/media", "--id", "docs")
	s.run("access", "rule", "remove", "mem/media", "--id", "docs")
	s.run("access", "public", "get", "mem/media")
	s.run("access", "public", "set", "public-read", "mem/site")
	s.run("access", "public", "get", "mem/site")
	s.run("access", "block-public", "status", "mem")
	s.run("access", "block-public", "on", "mem/site")
	s.run("access", "block-public", "status", "mem/site")
	s.run("access", "rule", "add", "mem/site", "--id", "pub2", "--allow", "--principal", "*", "--action", "read")
	s.run("access", "block-public", "off", "mem/site")
	s.run("access", "export", "mem/media")
	golden(t, "scenario/access_rules", s.b.String())
	assert.Equal(t, 3, h.admin.Calls("TestAccessRules"))
}

func TestAccessImportRejectsBadDocuments(t *testing.T) {
	h := newHarness(t)
	h.write("unknown.yaml", "rules:\n  - id: a\n    effect: allow\n    principals: ['*']\n    actions: [read]\n    colour: red\n")
	h.write("two.yaml", "rules: []\n---\nrules: []\n")
	h.write("list.yaml", "- id: a\n  effect: allow\n  principals: ['*']\n  actions: [read]\n  match: {type: glob, pattern: 'img/**'}\n")
	for _, tc := range []struct {
		name, file string
		code       int
		stderr     string
	}{
		{"unknown member", "unknown.yaml", 1, "unknown field"},
		{"two documents", "two.yaml", 1, "2 documents"},
		{"missing file", "missing.yaml", 3, "missing.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run("access", "import", "mem/media", tc.file)
			assert.Equal(t, tc.code, r.code, r.stderr)
			assert.Contains(t, r.stderr, tc.stderr)
		})
	}
	h.mustRun("access", "import", "mem/media", "list.yaml")
	r := h.mustRun("access", "rule", "list", "mem/media", "--json")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	assert.Equal(t, "a", recs[0]["id"])
	r = h.mustRun("access", "export", "mem/media", "--json")
	recs = jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	assert.Equal(t, "media", recs[0]["bucket"])
	assert.Len(t, recs[0]["rules"], 1)
}

// covers: TCL-D-01..04 (unit side; the e2e side is test/cli/testdata/dist.txtar)
// covers: S-DST-08 (nexoctl dist restrict --rate-limit, --burst,
// --rate-limit-per, --no-rate-limit)
func TestScenarioDistributions(t *testing.T) {
	h := newHarness(t)
	certPEM, keyPEM := testCertificate(t, "www.example.com", "example.com")
	h.write("cert.pem", certPEM)
	h.write("key.pem", keyPEM)
	s := h.session()
	s.run("dist", "ls", "mem")
	s.run("dist", "create", "mem", "site", "--bucket", "web", "--prefix", "/prod", "--domain", "www.example.com",
		"--domain", "example.com", "--index", "index.html", "--spa", "/index.html", "--comment", "the web site")
	s.run("dist", "create", "mem", "api", "--bucket", "data", "--mode", "s3", "--domain", "s3.example.com", "--access", "authenticated")
	s.run("dist", "create", "mem", "clash", "--bucket", "web", "--domain", "example.com")
	s.run("dist", "create", "mem", "nobucket", "--domain", "x.example.com")
	s.run("dist", "ls", "mem")
	s.run("dist", "info", "mem", "site")
	s.run("dist", "info", "mem", "nope")
	s.run("dist", "resolve", "mem", "example.com")
	s.run("dist", "resolve", "mem", "unknown.example.org")
	s.run("dist", "domain", "add", "mem", "site", "*.preview.example.com")
	s.run("dist", "domain", "remove", "mem", "site", "*.preview.example.com")
	s.run("dist", "behavior", "add", "mem", "site", "--prefix", "/assets/", "--cache-control", "public, max-age=31536000, immutable")
	s.run("dist", "keygroup", "create", "mem", "downloads", "--comment", "download links")
	s.run("dist", "keygroup", "ls", "mem")
	s.run("dist", "behavior", "add", "mem", "site", "--glob", "/downloads/**", "--access", "signed", "--key-group", "downloads")
	s.run("dist", "behavior", "ls", "mem", "site")
	s.run("dist", "behavior", "move", "mem", "site", "2", "1")
	s.run("dist", "behavior", "ls", "mem", "site")
	s.run("dist", "error-page", "set", "mem", "site", "404", "/404.html")
	s.run("dist", "redirect", "add", "mem", "site", "--from", "^/old/(.*)$", "--to", "/new/$1", "--status", "308")
	s.run("dist", "headers", "mem", "site", "--security", "--set", "X-Site: prod", "--cors-origin", "https://app.example.com")
	s.run("dist", "restrict", "mem", "site", "--deny-ip", "203.0.113.0/24")
	// S-DST-08: the rate limit of the distribution.
	s.run("dist", "restrict", "mem", "api", "--rate-limit", "50", "--rate-limit-per", "distribution")
	s.run("dist", "info", "mem", "api")
	s.run("dist", "restrict", "mem", "api", "--no-rate-limit")
	s.run("dist", "restrict", "mem", "site", "--rate-limit", "20", "--burst", "40")
	s.run("dist", "test", "mem", "site", "/downloads/v1.zip")
	s.run("dist", "test", "mem", "site", "https://example.com/assets/app.js")
	s.run("dist", "test", "mem", "site", "/old/page.html")
	// TCL-D-02: the stored private key signs; the expiry is required.
	s.run("dist", "sign-url", "mem", "site", "/downloads/v1.zip")
	s.run("dist", "sign-url", "mem", "site", "/downloads/v1.zip", "--expire", "1h")
	s.run("dist", "sign-url", "mem", "site", "/downloads/v1.zip", "--expire", "1h", "--cookie")
	s.run("dist", "sign-url", "mem", "site", "/index.html", "--expire", "1h")
	s.run("dist", "keygroup", "rm", "mem", "downloads")
	// TCL-D-04: origin secret and certificates.
	s.run("dist", "origin-secret", "rotate", "mem", "site")
	s.run("dist", "cert", "set", "mem", "site", "--cert", "cert.pem", "--key", "key.pem")
	s.run("dist", "cert", "set", "mem", "api", "--auto")
	s.run("dist", "cert", "ls", "mem")
	s.run("dist", "cert", "rm", "mem", "dist-site")
	s.run("dist", "disable", "mem", "api")
	s.run("dist", "enable", "mem", "api")
	s.run("dist", "export", "mem", "site", "-o", "site.yaml")
	s.b.WriteString("--- site.yaml\n" + h.read("site.yaml") + "\n")
	s.run("dist", "rm", "mem", "api")
	s.run("dist", "ls", "mem")
	golden(t, "scenario/distributions", strings.ReplaceAll(s.b.String(), h.dir, "$WORK"))

	// The secrets shown once are not in the golden file (random in a real
	// server): the stored private key is private to the user.
	matches, err := filepath.Glob(filepath.Join(h.dir, ".nexoctl", "keys", "mem", "*.pem"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	fi, err := os.Stat(matches[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// covers: TCL-D-01 (GitOps)
func TestDistExportImportRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.mustRun("dist", "create", "mem", "site", "--bucket", "web", "--domain", "www.example.com", "--domain", "example.com", "--index", "index.html")
	h.mustRun("dist", "behavior", "add", "mem", "site", "--prefix", "/assets/", "--cache-control", "max-age=60")
	h.mustRun("dist", "create", "mem", "docs", "--bucket", "docs", "--domain", "docs.example.com")
	h.mustRun("dist", "export", "mem", "-o", "all.yaml")
	before := h.mustRun("dist", "ls", "mem", "--json").stdout
	h.mustRun("dist", "rm", "mem", "site")
	h.mustRun("dist", "edit", "mem", "docs", "--comment", "changed")
	r := h.mustRun("dist", "import", "mem", "all.yaml")
	assert.Contains(t, r.stdout, "site")
	after := h.mustRun("dist", "ls", "mem", "--json").stdout
	recs, want := jsonLines(t, after), jsonLines(t, before)
	require.Len(t, recs, 2)
	for i := range recs {
		got, exp := recs[i]["distribution"].(map[string]any), want[i]["distribution"].(map[string]any)
		for _, k := range []string{"etag", "createdAt", "updatedAt"} {
			delete(got, k)
			delete(exp, k)
		}
		assert.Equal(t, exp, got)
	}
	// An invalid file is refused before any change; the unknown members
	// are sent to the server, which judges them (forward compatibility).
	calls := h.admin.Calls("CreateDistribution") + h.admin.Calls("UpdateDistribution")
	h.write("noid.yaml", "id: ok\naliases: [ok.example.com]\norigin: {bucket: web}\n---\naliases: [x.example.com]\norigin: {bucket: web}\n")
	h.write("syntax.yaml", "id: x\naliases: [x.example.com\n")
	for _, file := range []string{"noid.yaml", "syntax.yaml"} {
		r = h.run("dist", "import", "mem", file)
		assert.Equal(t, 1, r.code, r.stderr)
	}
	assert.Equal(t, calls, h.admin.Calls("CreateDistribution")+h.admin.Calls("UpdateDistribution"))
}

func TestDistCommandsPrintJSONLines(t *testing.T) {
	h := newHarness(t)
	h.mustRun("dist", "create", "mem", "site", "--bucket", "web", "--domain", "www.example.com")
	h.mustRun("dist", "keygroup", "create", "mem", "kg")
	h.mustRun("dist", "behavior", "add", "mem", "site", "--prefix", "/dl/", "--access", "signed", "--key-group", "kg")
	for _, args := range [][]string{
		{"dist", "ls", "mem"},
		{"dist", "info", "mem", "site"},
		{"dist", "test", "mem", "site", "/dl/a.zip"},
		{"dist", "resolve", "mem", "www.example.com"},
		{"dist", "behavior", "ls", "mem", "site"},
		{"dist", "keygroup", "ls", "mem"},
		{"dist", "sign-url", "mem", "site", "/dl/a.zip", "--expire", "10m"},
		{"dist", "cert", "ls", "mem"},
		{"dist", "export", "mem", "site"},
		{"dist", "origin-secret", "rotate", "mem", "site"},
		{"access", "rule", "ls", "mem/web"},
		{"access", "public", "get", "mem/web"},
		{"access", "block-public", "status", "mem"},
		{"access", "test", "mem/web/a.txt"},
		{"access", "export", "mem/web"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := h.mustRun(append(args, "--json")...)
			for _, rec := range jsonLines(t, r.stdout) {
				assert.Equal(t, "success", rec["status"])
				assert.Equal(t, "mem", rec["alias"])
			}
		})
	}
}

func TestDistDeniedForNonAdmin(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("bob", "bob-secret-1", "readwrite")
	h.mustRun("alias", "set", "bob", "http://s3.mem.test", "bob", "bob-secret-1")
	for _, args := range [][]string{
		{"dist", "ls", "bob"},
		{"dist", "create", "bob", "x", "--bucket", "web", "--domain", "x.example.com"},
		{"dist", "keygroup", "ls", "bob"},
		{"dist", "cert", "ls", "bob"},
		{"access", "rule", "ls", "bob/web"},
		{"access", "block-public", "on", "bob"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := h.run(args...)
			assert.Equal(t, 4, r.code, r.stderr)
		})
	}
}

// testCertificate returns a self-signed certificate of the domains and its
// private key, in PEM, valid around t0.
func testCertificate(t *testing.T, domains ...string) (string, string) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    t0.Add(-time.Hour),
		NotAfter:     t0.Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, key.Public(), key)
	require.NoError(t, err)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))
}
