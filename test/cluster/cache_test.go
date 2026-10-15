//go:build cluster

package cluster_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// hostGet reads path through the distribution of host on node n: the
// body and the X-Cache of the answer ("" with an error).
func hostGet(n *node, host, path string) (status int, body, xcache string) {
	req, err := http.NewRequest(http.MethodGet, "http://"+n.S3+path, nil)
	if err != nil {
		return 0, "", ""
	}
	req.Host = host
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, "", ""
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header.Get("X-Cache")
}

// TestTCCC04PausedNode: a node that does not notice it was cut off (a
// paused process: SIGSTOP) while a write is acknowledged never serves the
// old content once it resumes, in the local and the shared mode of the
// cache (TCC-C-04, TCC-05): its trust lease expired during the pause, the
// write waited for it, and its entries are revalidated until it compared
// the sequences of its peers again.
//
// covers: TCC-C-04, TCC-05
func TestTCCC04PausedNode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"local", "shared"} {
		t.Run(mode, func(t *testing.T) {
			c := newCluster(t, 4)
			for _, nd := range c.nodes {
				nd.env = append(nd.env, "NEXO_CACHE_MODE="+mode)
			}
			c.start()
			ctx := context.Background()
			bucket := "tccc04-" + mode
			mustCreateBucket(t, c.node(1).rootClient(), bucket)
			require.NoError(t, put(ctx, c.node(1).rootClient(), bucket, "news.html", []byte("OLD CONTENT")))
			hosts := []string{"www." + mode + ".test", mode + ".test"}
			_, err := c.node(1).admin().CreateDistribution(ctx, nexoadmin.Distribution{
				ID: "site-" + mode, Enabled: true, Aliases: hosts, Mode: "website",
				Origin:          nexoadmin.Origin{Bucket: bucket},
				DefaultBehavior: nexoadmin.Behavior{Access: "public", IndexDocument: "index.html"},
			})
			require.NoError(t, err)
			// Every node serves it, from its cache (or the owner's).
			for _, nd := range c.nodes {
				for _, h := range hosts {
					eventually(t, 20*time.Second, "cached on node "+strconv.Itoa(nd.idx), func() bool {
						status, body, x := hostGet(nd, h, "/news.html")
						return status == http.StatusOK && body == "OLD CONTENT" && x == "HIT"
					})
				}
			}

			paused := c.node(3)
			paused.pause()
			start := time.Now()
			require.NoError(t, put(ctx, c.node(1).rootClient(), bucket, "news.html", []byte("NEW CONTENT")))
			t.Logf("write acknowledged with node 3 paused after %s", time.Since(start).Round(time.Millisecond))
			paused.resume()

			// From the resume on, never the old content on any domain.
			stale := 0
			deadline := time.Now().Add(3 * time.Second)
			reads := 0
			for time.Now().Before(deadline) {
				for _, h := range hosts {
					status, body, x := hostGet(paused, h, "/news.html")
					if status == 0 {
						continue
					}
					reads++
					if strings.Contains(body, "OLD") {
						stale++
						t.Logf("stale answer on %s: %d %q X-Cache %s", h, status, body, x)
					}
				}
			}
			assert.Positive(t, reads)
			assert.Zero(t, stale, "old content after the 200 of the write")
			for _, nd := range c.nodes {
				_, body, _ := hostGet(nd, hosts[0], "/news.html")
				assert.Equal(t, "NEW CONTENT", body, "node %d", nd.idx)
			}
		})
	}
}
