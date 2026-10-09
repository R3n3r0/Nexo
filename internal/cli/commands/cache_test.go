package commands_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// cacheHarness is a harness with the distribution "site" of the bucket
// web on www.site.test and preview.site.test.
func cacheHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.mustRun("dist", "create", "mem", "site", "--bucket", "web", "--domain", "www.site.test", "--domain", "preview.site.test", "--index", "index.html")
	return h
}

// covers: TCL-C-01..05 (unit side; the e2e side is test/cli/cache.txtar)
func TestScenarioCache(t *testing.T) {
	h := cacheHarness(t)
	h.write("urls.txt", "# the release\nhttps://www.site.test/assets/app.js\n/assets/style.css\n\n")
	h.write("body.tmpl", `{"paths":"{{range .Paths}}{{.}} {{end}}"}`)
	s := h.session()
	// Policies.
	s.run("cache", "policy", "ls", "mem")
	s.run("cache", "policy", "create", "mem", "short", "--ttl-default", "10m", "--ttl-max", "1d", "--browser-cache-control", "public, max-age=600",
		"--query", "allowlist:v,lang", "--key-header", "Accept-Language", "--stale-while-revalidate", "1m", "--comment", "ten minutes")
	s.run("cache", "policy", "create", "mem", "short")
	s.run("cache", "policy", "create", "mem", "bad", "--ttl-default", "1d", "--ttl-max", "1h")
	s.run("cache", "policy", "create", "mem", "badquery", "--query", "maybe")
	s.run("cache", "policy", "info", "mem", "short")
	s.run("cache", "policy", "edit", "mem", "short", "--negative-ttl", "30s", "--bypass-cookie", "session")
	s.run("cache", "policy", "edit", "mem", "short")
	s.run("cache", "policy", "edit", "mem", "standard", "--ttl-max", "1d")
	s.run("cache", "policy", "info", "mem", "nope")
	// Settings of the cluster.
	s.run("cache", "settings", "mem")
	s.run("cache", "settings", "mem", "--policy", "standard", "--node-header", "--debug-secret", "s3cret")
	s.run("cache", "settings", "mem", "--enable", "--disable")
	// Settings of the distribution, its domains and paths.
	s.run("dist", "cache", "set", "mem", "site", "--domain", "www.site.test", "--policy", "short", "--quota", "64MiB")
	s.run("dist", "cache", "set", "mem", "site", "--domain", "preview.site.test", "--disable")
	s.run("dist", "cache", "set", "mem", "site", "--behavior", "/assets/**", "--ttl-default", "365d", "--ttl-max", "365d")
	s.run("dist", "cache", "set", "mem", "site", "--auto-invalidate=false", "--warm-on-write", "/assets/**")
	s.run("dist", "cache", "set", "mem", "site", "--domain", "other.test", "--disable")
	s.run("dist", "cache", "set", "mem", "site", "--domain", "www.site.test")
	s.run("dist", "cache", "set", "mem", "site", "--domain", "www.site.test", "--behavior", "/a")
	s.run("dist", "cache", "set", "mem", "nope", "--disable")
	s.run("dist", "cache", "show", "mem", "site", "--path", "/assets/app.js")
	// Invalidations.
	h.admin.MarkCached("site", "www.site.test", "/assets/app.js")
	h.admin.MarkCached("site", "www.site.test", "/index.html")
	s.run("cache", "inspect", "mem", "https://www.site.test/assets/app.js")
	s.run("cache", "inspect", "mem", "/index.html", "--dist", "site", "--header", "Accept-Encoding: br")
	s.run("cache", "inspect", "mem", "https://preview.site.test/")
	s.run("cache", "inspect", "mem", "/index.html")
	s.run("cache", "purge", "mem", "site", "--domain", "www.site.test", "/assets/**", "--soft")
	s.run("cache", "purge", "mem", "site", "https://www.site.test/index.html", "--prefix", "/img/", "--regex", `^/x/`, "--tag", "p42", "--wait")
	s.run("cache", "purge", "status", "mem", "inv-9")
	s.run("cache", "purge", "status", "mem", "inv-10", "--dist", "site", "--wait")
	s.run("cache", "purge", "status", "mem", "inv-99")
	s.run("cache", "purge", "ls", "mem", "site")
	s.run("cache", "purge", "mem", "--all")
	s.run("cache", "purge", "ls", "mem")
	s.run("cache", "purge", "mem", "site")
	s.run("cache", "purge", "mem", "/assets/**")
	s.run("cache", "purge", "mem", "--tag", "x")
	s.run("cache", "purge", "mem", "site", "assets/app.js")
	s.run("dist", "invalidate", "mem", "site", "/index.html")
	// Warm-ups.
	s.run("cache", "warm", "mem", "site", "--domain", "www.site.test", "--from", "urls.txt", "--wait")
	s.run("cache", "warm", "mem", "site", "--prefix", "/assets/", "--concurrency", "2", "--bandwidth", "10MiB")
	s.run("cache", "warm", "status", "mem", "site", "warm-14")
	s.run("cache", "warm", "status", "mem", "site", "warm-99")
	s.run("cache", "warm", "mem", "site")
	s.run("cache", "warm", "mem", "site", "--glob", "/a/**", "--regex", "^/b")
	s.run("cache", "warm", "mem", "site", "x.js")
	// Statistics.
	s.run("cache", "stats", "mem")
	s.run("cache", "stats", "mem", "site", "--domain", "www.site.test", "--watch", "--interval", "1ms", "--count", "2")
	s.run("cache", "stats", "mem", "nope")
	// Webhooks.
	s.run("cache", "webhook", "add", "mem", "site", "--type", "cloudflare", "--zone", "z1", "--token", "tok", "--domain", "www.site.test")
	s.run("cache", "webhook", "add", "mem", "site", "--type", "generic", "--id", "edge", "--url", "http://edge.test/purge", "--body-file", "body.tmpl",
		"--header", "X-Token: abc")
	s.run("cache", "webhook", "add", "mem", "site", "--type", "generic", "--id", "down", "--url", "http://unreachable.test/purge")
	s.run("cache", "webhook", "add", "mem", "site", "--type", "cloudflare")
	s.run("cache", "webhook", "add", "mem", "site", "--zone", "z1")
	s.run("cache", "webhook", "ls", "mem", "site")
	s.run("cache", "webhook", "test", "mem", "site", "edge")
	s.run("cache", "webhook", "test", "mem", "site", "down")
	s.run("cache", "webhook", "rm", "mem", "site", "down")
	s.run("cache", "webhook", "rm", "mem", "site", "down")
	// GitOps.
	s.run("dist", "cache", "export", "mem", "site", "-o", "cache.yaml")
	s.b.WriteString("--- cache.yaml\n" + h.read("cache.yaml") + "\n")
	s.run("cache", "policy", "export", "mem")
	s.run("dist", "cache", "set", "mem", "site", "--domain", "www.site.test", "--reset")
	s.run("dist", "cache", "import", "mem", "cache.yaml")
	s.run("cache", "policy", "rm", "mem", "short")
	s.run("dist", "cache", "set", "mem", "site", "--domain", "www.site.test", "--reset", "--policy", "aggressive")
	s.run("cache", "policy", "rm", "mem", "short")
	s.run("cache", "policy", "rm", "mem", "standard")
	golden(t, "scenario/cache", s.b.String())
	assert.Equal(t, 2, h.admin.Calls("TestCacheWebhook"))
}

// TestCacheJSON: every cache command prints JSON Lines, the records carry
// the documents of the API.
func TestCacheJSON(t *testing.T) {
	h := cacheHarness(t)
	h.mustRun("cache", "policy", "create", "mem", "short", "--ttl-default", "10m")
	h.admin.MarkCached("site", "www.site.test", "/index.html")
	for _, args := range [][]string{
		{"cache", "policy", "ls", "mem"},
		{"cache", "policy", "info", "mem", "short"},
		{"cache", "settings", "mem"},
		{"dist", "cache", "set", "mem", "site", "--domain", "www.site.test", "--policy", "short"},
		{"dist", "cache", "show", "mem", "site"},
		{"dist", "cache", "export", "mem", "site"},
		{"cache", "inspect", "mem", "https://www.site.test/index.html"},
		{"cache", "purge", "mem", "site", "/index.html"},
		{"cache", "purge", "ls", "mem", "site"},
		{"cache", "warm", "mem", "site", "/index.html"},
		{"cache", "stats", "mem", "site"},
		{"cache", "webhook", "add", "mem", "site", "--type", "nginx", "--url", "http://nginx.test"},
		{"cache", "webhook", "ls", "mem", "site"},
		{"cache", "webhook", "test", "mem", "site", "nginx"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := h.mustRun(append(args, "--json")...)
			recs := jsonLines(t, r.stdout)
			require.NotEmpty(t, recs)
			assert.Equal(t, "mem", recs[0]["alias"])
		})
	}
	r := h.mustRun("cache", "stats", "mem", "--json")
	var rec struct {
		Stats nexoadmin.CacheStats `json:"stats"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &rec))
	require.Len(t, rec.Stats.Nodes, 1)
	assert.True(t, rec.Stats.Nodes[0].Synced)
	r = h.run("cache", "purge", "status", "mem", "inv-nope", "--json")
	assert.Equal(t, 3, r.code)
	recs := jsonLines(t, r.stderr)
	require.Len(t, recs, 1)
	assert.Equal(t, "error", recs[0]["status"])
}

// TestCachePolicyFromFile: a policy document (YAML) changed by the flags.
func TestCachePolicyFromFile(t *testing.T) {
	h := newHarness(t)
	h.write("p.yaml", "name: ignored\nttl:\n  default: 2h\n  max: 1d\nbrowserCacheControl: 'public, max-age=60'\ncacheKey:\n  queryString: {mode: all}\n")
	h.write("bad.yaml", "ttl: {default: 2h}\ncolour: red\n")
	h.mustRun("cache", "policy", "create", "mem", "doc", "--file", "p.yaml", "--ttl-min", "1m")
	r := h.mustRun("cache", "policy", "info", "mem", "doc", "--json")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	p := recs[0]["policy"].(map[string]any)
	assert.Equal(t, "doc", p["name"], "the name is the one of the command")
	assert.Equal(t, map[string]any{"default": "2h", "min": "1m", "max": "1d"}, p["ttl"])
	assert.Equal(t, "public, max-age=60", recs[0]["cacheControl"])
	r = h.run("cache", "policy", "create", "mem", "bad", "--file", "bad.yaml")
	assert.Equal(t, 1, r.code)
	assert.Contains(t, r.stderr, "unknown field")
	h.write("policies.yaml", "name: a\nttl: {default: 1h}\n---\nname: doc\ncomment: replaced\n")
	r = h.mustRun("cache", "policy", "import", "mem", "policies.yaml")
	assert.Equal(t, "Cache policy `a` created.\nCache policy `doc` updated.\n", r.stdout)
}
