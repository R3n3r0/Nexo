package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// distributionBody is the Admin API document of a public distribution.
func distributionBody(t *testing.T, id, bucket, mode string, aliases ...string) string {
	t.Helper()
	behavior := map[string]any{"access": "public"}
	if mode == "website" {
		behavior["indexDocument"] = "index.html"
	}
	raw, err := json.Marshal(map[string]any{
		"id": id, "enabled": true, "aliases": aliases, "mode": mode,
		"origin": map[string]string{"bucket": bucket}, "defaultBehavior": behavior,
	})
	require.NoError(t, err)
	return string(raw)
}

// TestDistributionAliasTakeover: a user who manages the distributions of
// his own bucket only cannot take over what the deployment itself answers
// on (regression of three takeovers):
//   - the hosts of the deployment (localhost, the machine name, the node
//     endpoints): an alias "localhost" served a website in place of the S3
//     API, the health probes and the internode RPC of a cluster reached
//     through it, and wrote the S3 uploads of the other users into his
//     bucket;
//   - the reserved "/nexo/" paths of the S3 listener (health, metrics,
//     internode RPC) on the hosts of his distributions;
//   - the hosts under the wildcard alias of another distribution.
//
// covers: S-DST-01
func TestDistributionAliasTakeover(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("webteam")
	e.mkBucket("victim")
	e.putTyped("webteam", "index.html", "<h1>attacker</h1>", "text/html")
	e.putTyped("victim", "index.html", "<h1>corp</h1>", "text/html")
	// Distributions on every bucket but victim.
	e.setPolicy("webadmin", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::webteam","arn:aws:s3:::webteam/*"]},
		{"Effect":"Allow","Action":["admin:ManageDistributions"],"Resource":["arn:aws:s3:::*"]},
		{"Effect":"Deny","Action":["admin:ManageDistributions"],"Resource":["arn:aws:s3:::victim"]}]}`)
	web := e.consoleLogin(e.createUser("webadmin", "webadmin-secret-1", "webadmin"))
	root := e.consoleLogin(e.rootCreds())

	// The hosts of the deployment are no aliases, exact or under a
	// wildcard.
	hosts := []string{"localhost"}
	if name, err := os.Hostname(); err == nil && name != "" && !strings.Contains(name, "_") {
		hosts = append(hosts, strings.ToLower(name))
		if _, rest, ok := strings.Cut(name, "."); ok && strings.Contains(rest, ".") {
			hosts = append(hosts, "*."+strings.ToLower(rest))
		}
	}
	for _, host := range hosts {
		for _, mode := range []string{"website", "s3"} {
			resp := web.do(http.MethodPost, "/distributions", distributionBody(t, "takeover", "webteam", mode, host))
			assert.Equal(t, http.StatusConflict, resp.StatusCode, "%s (%s): %s", host, mode, resp.Body)
		}
	}

	// The wildcard of another distribution: its hosts belong to it.
	resp := root.do(http.MethodPost, "/distributions", distributionBody(t, "corp", "victim", "website", "*.corp.test"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	for _, alias := range []string{"app.corp.test", "*.x.corp.test"} {
		resp := web.do(http.MethodPost, "/distributions", distributionBody(t, "shadow", "webteam", "website", alias))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s: %s", alias, resp.Body)
	}

	// Its own hosts: served, except the reserved paths.
	resp = web.do(http.MethodPost, "/distributions", distributionBody(t, "site", "webteam", "website", "takeover.test"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	resp = web.do(http.MethodPost, "/distributions", distributionBody(t, "api", "webteam", "s3", "s3take.test"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	for _, base := range e.nodeURLs() {
		require.Eventually(t, func() bool {
			r := hostRequest(t, http.MethodGet, base, "takeover.test", "/", nil)
			return r.StatusCode == http.StatusOK && string(r.Body) == "<h1>attacker</h1>"
		}, 10*time.Second, 50*time.Millisecond, "the site on %s", base)
		r := hostRequest(t, http.MethodGet, base, "a.corp.test", "/", nil)
		assert.Equal(t, "<h1>corp</h1>", string(r.Body), "the wildcard keeps its hosts on %s", base)

		mainHost := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
		paths := []string{domain.HealthLivePath, domain.HealthReadyPath, "/nexo/metrics/v1/node"}
		if len(e.srv.Nodes) > 1 {
			// The internode protocol of the cluster (an HTTP/1 call is
			// refused by the RPC server itself).
			paths = append(paths, "/nexo/rpc/v1/peer/health")
		}
		for _, path := range paths {
			want := hostRequest(t, http.MethodGet, base, mainHost, path, nil)
			for _, host := range []string{"takeover.test", "s3take.test"} {
				got := hostRequest(t, http.MethodGet, base, host, path, nil)
				assert.Equal(t, want.StatusCode, got.StatusCode, "%s%s on %s: %s", host, path, base, got.Body)
				assert.NotContains(t, string(got.Body), "attacker", "%s%s on %s", host, path, base)
			}
		}
	}
}

// TestDistributionListenerModes: the S3 listener serves the distributions
// only when the deployment opts in (--s3-distributions, by default when it
// names its S3 endpoint): with "off" an alias reaching the S3 port is an
// unknown host of the S3 API while the web listener serves it; with
// --no-distributions (recovery) no listener routes a host, and the
// distributions stay manageable to remove the one at fault.
//
// covers: S-DST-01
func TestDistributionListenerModes(t *testing.T) {
	t.Parallel()
	t.Run("s3 listener off", func(t *testing.T) {
		e := newEnv(t, testutil.WithConfig(func(c *config.Static) {
			c.S3Distributions = config.S3DistributionsOff
			c.WebAddress = "127.0.0.1:0"
		}))
		e.mkBucket("web")
		e.putTyped("web", "index.html", "home", "text/html")
		root := e.consoleLogin(e.rootCreds())
		resp := root.do(http.MethodPost, "/distributions", distributionBody(t, "site", "web", "website", "www.site.test"))
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
		for i, n := range e.srv.Nodes {
			require.Eventually(t, func() bool {
				r := hostRequest(t, http.MethodGet, "http://"+n.App.WebAddr(), "www.site.test", "/", nil)
				return r.StatusCode == http.StatusOK && string(r.Body) == "home"
			}, 10*time.Second, 50*time.Millisecond, "web listener of node %d", i+1)
			r := hostRequest(t, http.MethodGet, n.S3URL, "www.site.test", "/", nil)
			assert.NotEqual(t, "home", string(r.Body), "S3 listener of node %d", i+1)
			assert.Contains(t, r.Header.Get("Content-Type"), "xml", "the S3 API answers on node %d", i+1)
		}
	})
	t.Run("no distributions", func(t *testing.T) {
		e := newEnv(t, testutil.WithConfig(func(c *config.Static) {
			c.NoDistributions = true
			c.WebAddress = "127.0.0.1:0"
		}))
		e.mkBucket("web")
		e.putTyped("web", "index.html", "home", "text/html")
		root := e.consoleLogin(e.rootCreds())
		resp := root.do(http.MethodPost, "/distributions", distributionBody(t, "site", "web", "website", "www.site.test"))
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
		for i, n := range e.srv.Nodes {
			r := hostRequest(t, http.MethodGet, n.S3URL, "www.site.test", "/", nil)
			assert.NotEqual(t, "home", string(r.Body), "S3 listener of node %d", i+1)
			r = hostRequest(t, http.MethodGet, "http://"+n.App.WebAddr(), "www.site.test", "/", nil)
			assert.Equal(t, http.StatusMisdirectedRequest, r.StatusCode, "web listener of node %d", i+1)
		}
		resp = root.do(http.MethodGet, "/distributions", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		assert.Contains(t, string(resp.Body), "www.site.test")
		resp = root.do(http.MethodDelete, "/distributions/site", "")
		assert.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
	})
}

// TestInternodeMutualTLS: a cluster with --tls-internode-verify forms and
// serves: every node presents its certificate to its peers, which accept
// it because it is issued by a CA of CAs/ (or is the shared certificate)
// and names a node of the deployment (the client certificates of other
// services of the same CA are refused: TestServerMutualTLSBindsTheNodes).
//
// covers: S-OPS-02
func TestInternodeMutualTLS(t *testing.T) {
	if modeOf(t) != modeCluster {
		t.Skip("mutual TLS between the nodes of a cluster")
	}
	t.Parallel()
	e := newEnv(t, testutil.WithTLS(), testutil.WithConfig(func(c *config.Static) { c.TLSInternodeVerify = true }))
	c := e.tlsClient()
	_, err := c.CreateBucket(e.ctx, &s3.CreateBucketInput{Bucket: aws.String("mtls")})
	require.NoError(t, err)
	body := []byte(strings.Repeat("internode ", 1<<15))
	for i := range 8 {
		key := "k" + strconv.Itoa(i)
		_, err := c.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("mtls"), Key: aws.String(key), Body: bytes.NewReader(body)})
		require.NoError(t, err, "write %s (the load balancer spreads the writes over the nodes)", key)
		assert.Equal(t, body, getWith(t, e.ctx, c, "mtls", key))
	}
}
