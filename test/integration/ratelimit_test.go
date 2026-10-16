package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// behindProxy is a deployment whose clients are forwarded by a trusted
// proxy on the loopback: every test names its clients with
// X-Forwarded-For, so the buckets of the client addresses are its own.
func behindProxy(t *testing.T) *env {
	t.Helper()
	return newEnv(t, testutil.WithConfig(func(cfg *config.Static) {
		cfg.TrustedProxies = []string{"127.0.0.1", "::1"}
	}))
}

// setConfig writes a subsystem of the dynamic configuration through the
// Admin API, retrying while a rate limit of the Admin API refuses it.
func (c *console) setConfig(subsystem string, kv map[string]string) {
	c.t.Helper()
	body, err := json.Marshal(map[string]any{"settings": kv})
	require.NoError(c.t, err)
	require.EventuallyWithT(c.t, func(ct *assert.CollectT) {
		resp := c.do(http.MethodPut, "/config/"+subsystem, string(body))
		assert.Equal(ct, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	}, 30*time.Second, 200*time.Millisecond)
}

// resetConfig deletes a subsystem of the dynamic configuration (retrying
// while a rate limit of the Admin API refuses it).
func (c *console) resetConfig(subsystem string) {
	c.t.Helper()
	require.EventuallyWithT(c.t, func(ct *assert.CollectT) {
		resp := c.do(http.MethodDelete, "/config/"+subsystem, "")
		assert.Contains(ct, []int{http.StatusOK, http.StatusNoContent}, resp.StatusCode, "%s", resp.Body)
	}, 30*time.Second, 200*time.Millisecond)
}

// forwardedGet sends an anonymous GET for the client ip to base.
func forwardedGet(t *testing.T, base, path, ip string) response {
	t.Helper()
	req := newRequest(t, http.MethodGet, base+path, nil)
	req.Header.Set("X-Forwarded-For", ip)
	return do(t, req)
}

// slowDown reports an S3 SlowDown answer with its Retry-After.
func slowDown(t require.TestingT, resp response) bool {
	if resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	assert.Contains(t, string(resp.Body), "<Code>SlowDown</Code>")
	retry, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	assert.NoError(t, err, "Retry-After in seconds")
	assert.GreaterOrEqual(t, retry, 1)
	return true
}

// throttledWithin sends up to n requests and reports whether one of them
// was refused by a rate limit (refused tells).
func throttledWithin(n int, send func() response, refused func(response) bool) bool {
	for range n {
		if refused(send()) {
			return true
		}
	}
	return false
}

// TestRateLimitS3: the rate limits of the S3 API per client address
// (behind the trusted proxies), per access key and per bucket, set and
// cleared through the dynamic configuration on every node; the refusals
// are 503 SlowDown in S3 XML with Retry-After; the health probes are
// exempt; without a limit nothing is refused.
//
// covers: S-OPS-10
func TestRateLimitS3(t *testing.T) {
	e := behindProxy(t)
	e.mkBucket("rlimits")
	e.put("rlimits", "a.txt", []byte("A"))
	root := e.consoleLogin(e.rootCreds())

	for _, base := range e.nodeURLs() {
		for range 20 {
			require.NotEqual(t, http.StatusServiceUnavailable, forwardedGet(t, base, "/rlimits/a.txt", "198.51.100.1").StatusCode, "no limit by default")
		}
	}

	root.setConfig("ratelimit", map[string]string{"s3_ip": "1", "s3_ip_burst": "3"})
	for _, base := range e.nodeURLs() {
		// Every node applies the limit (per node: each has its buckets).
		require.Eventually(t, func() bool {
			return throttledWithin(10, func() response { return forwardedGet(t, base, "/rlimits/a.txt", "198.51.100.2") },
				func(r response) bool { return slowDown(t, r) })
		}, 30*time.Second, 100*time.Millisecond, "node %s", base)
		resp := forwardedGet(t, base, domain.HealthLivePath, "198.51.100.2")
		assert.Equal(t, http.StatusOK, resp.StatusCode, "the health probes are exempt")
		assert.NotEqual(t, http.StatusServiceUnavailable, forwardedGet(t, base, "/rlimits/a.txt", "198.51.100.3").StatusCode,
			"another client has its own bucket")
	}

	// The access key: a client without retries sees SlowDown.
	root.setConfig("ratelimit", map[string]string{"s3_ip": "0", "s3_access_key": "1", "s3_access_key_burst": "2"})
	alice := e.createUser("alice", "alice-secret-1", "readwrite")
	client := s3.NewFromConfig(e.sdkConfig(alice), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(e.srv.Nodes[0].S3URL)
		o.UsePathStyle = true
		o.Retryer = aws.NopRetryer{}
	})
	require.Eventually(t, func() bool {
		for range 10 {
			_, err := client.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("rlimits"), Key: aws.String("a.txt")})
			if httpStatus(err) == http.StatusServiceUnavailable {
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond)
	require.Eventually(t, func() bool {
		for range 10 {
			if _, err := client.ListObjectsV2(e.ctx, &s3.ListObjectsV2Input{Bucket: aws.String("rlimits")}); err != nil {
				return apiCode(err) == "SlowDown"
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "the SDK reads the SlowDown code")

	// The bucket, whoever the client.
	root.setConfig("ratelimit", map[string]string{"s3_access_key": "0", "s3_bucket": "1", "s3_bucket_burst": "2"})
	base := e.srv.Nodes[0].S3URL
	ip := 0
	require.Eventually(t, func() bool {
		return throttledWithin(10, func() response {
			ip++
			return forwardedGet(t, base, "/rlimits/a.txt", fmt.Sprintf("198.51.100.%d", 10+ip%200))
		}, func(r response) bool { return slowDown(t, r) })
	}, 30*time.Second, 100*time.Millisecond)

	root.resetConfig("ratelimit")
	require.Eventually(t, func() bool {
		for range 10 {
			if forwardedGet(t, base, "/rlimits/a.txt", "198.51.100.2").StatusCode == http.StatusServiceUnavailable {
				return false
			}
		}
		return true
	}, 30*time.Second, 200*time.Millisecond, "the limits are gone once cleared")
}

// TestRateLimitConcurrency: api.requests_max bounds the S3 requests a node
// serves at once; a request beyond it waits api.requests_deadline for a
// slot, then gets 503 SlowDown; the slot freed, requests are served again.
//
// covers: S-OPS-10
func TestRateLimitConcurrency(t *testing.T) {
	e := behindProxy(t)
	e.mkBucket("slots")
	e.put("slots", "a.txt", []byte("A"))
	e.putBucketPolicy("slots", policyJSON(`{"Effect":"Allow","Principal":"*","Action":["s3:PutObject","s3:GetObject"],"Resource":["arn:aws:s3:::slots/*"]}`))
	root := e.consoleLogin(e.rootCreds())
	root.setConfig("api", map[string]string{"requests_max": "1", "requests_deadline": "200ms"})
	base := e.srv.Nodes[0].S3URL

	// A slow upload holds the only slot of the node: its body arrives
	// when the test writes it.
	pr, pw := io.Pipe()
	req := newRequest(t, http.MethodPut, base+"/slots/slow.bin", nil)
	req.Body, req.ContentLength = pr, 4
	done := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	require.Eventually(t, func() bool {
		return slowDown(t, forwardedGet(t, base, "/slots/a.txt", "198.51.100.20"))
	}, 30*time.Second, 50*time.Millisecond, "the second request waits, then is refused")
	assert.Equal(t, http.StatusOK, forwardedGet(t, base, domain.HealthReadyPath, "198.51.100.20").StatusCode, "the probes are exempt")

	_, err := pw.Write([]byte("data"))
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	assert.Equal(t, http.StatusOK, <-done)
	require.Eventually(t, func() bool {
		return forwardedGet(t, base, "/slots/a.txt", "198.51.100.20").StatusCode == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond, "the slot is free again")

	root.resetConfig("api")
}

// TestRateLimitAdminAPI: the rate limits of the Admin API per client
// address and per user answer 429 problem+json (too_many_requests) with
// Retry-After.
//
// covers: S-OPS-10
func TestRateLimitAdminAPI(t *testing.T) {
	e := behindProxy(t)
	root := e.consoleLogin(e.rootCreds())
	root.setConfig("ratelimit", map[string]string{"admin_user": "1", "admin_user_burst": "1"})
	tooMany := func(r response) bool {
		if r.StatusCode != http.StatusTooManyRequests {
			return false
		}
		var p struct {
			Code string `json:"code"`
		}
		assert.NoError(t, json.Unmarshal(r.Body, &p))
		assert.Equal(t, "too_many_requests", p.Code)
		assert.Contains(t, r.Header.Get("Content-Type"), "application/problem+json")
		assert.NotEmpty(t, r.Header.Get("Retry-After"))
		return true
	}
	require.Eventually(t, func() bool {
		return throttledWithin(30, func() response { return root.do(http.MethodGet, "/buckets", "") }, tooMany)
	}, 30*time.Second, 100*time.Millisecond)

	root.setConfig("ratelimit", map[string]string{"admin_user": "0", "admin_ip": "1", "admin_ip_burst": "1"})
	from := func(ip string) response {
		return root.doRaw(http.MethodGet, "/buckets", nil, http.Header{"X-Forwarded-For": {ip}})
	}
	require.Eventually(t, func() bool {
		return throttledWithin(30, func() response { return from("198.51.100.30") }, tooMany)
	}, 30*time.Second, 100*time.Millisecond)
	resp := do(t, newRequest(t, http.MethodGet, e.srv.ConsoleURL+domain.HealthLivePath, nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the probes of the console listener are exempt")
	root.resetConfig("ratelimit")
}

// TestRateLimitDistributions: the rate limit of a distribution
// (restrictions.rateLimit) is enforced on every node: 429 plain text for a
// website (per client address), 503 SlowDown for an s3 distribution
// (shared by its clients), with Retry-After (docs/10 TDS-13).
//
// covers: S-DST-08
func TestRateLimitDistributions(t *testing.T) {
	e := behindProxy(t)
	e.mkBucket("rlweb")
	e.putTyped("rlweb", "index.html", "home", "text/html")
	dists := e.srv.App.Distributions()
	_, err := dists.Create(e.ctx, distribution.Distribution{
		ID: "rl-site", Enabled: true, Aliases: []string{"rl.site.test"}, Mode: distribution.ModeWebsite,
		Origin:          distribution.Origin{Bucket: "rlweb"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
		Restrictions:    distribution.Restrictions{RateLimit: &distribution.RateLimit{RequestsPerSecond: 1, Burst: 2, Per: distribution.RateLimitPerIP}},
	})
	require.NoError(t, err)
	_, err = dists.Create(e.ctx, distribution.Distribution{
		ID: "rl-s3", Enabled: true, Aliases: []string{"s3.rl.test"}, Mode: distribution.ModeS3,
		Origin:       distribution.Origin{Bucket: "rlweb"},
		Restrictions: distribution.Restrictions{RateLimit: &distribution.RateLimit{RequestsPerSecond: 1, Burst: 1, Per: distribution.RateLimitPerDistribution}},
	})
	require.NoError(t, err)

	get := func(base, host, ip string) response {
		return hostRequest(t, http.MethodGet, base, host, "/", http.Header{"X-Forwarded-For": {ip}})
	}
	for _, base := range e.nodeURLs() {
		require.Eventually(t, func() bool {
			return throttledWithin(10, func() response { return get(base, "rl.site.test", "198.51.100.40") }, func(r response) bool {
				if r.StatusCode != http.StatusTooManyRequests {
					return false
				}
				assert.Contains(t, string(r.Body), "429 Too Many Requests")
				assert.NotEmpty(t, r.Header.Get("Retry-After"))
				return true
			})
		}, 30*time.Second, 100*time.Millisecond, "node %s", base)
		assert.Equal(t, http.StatusOK, get(base, "rl.site.test", "198.51.100.41").StatusCode, "another client of the website")

		require.Eventually(t, func() bool {
			return throttledWithin(10, func() response { return get(base, "s3.rl.test", "198.51.100.42") },
				func(r response) bool { return slowDown(t, r) })
		}, 30*time.Second, 100*time.Millisecond, "node %s", base)
	}

	// Without the limit the website serves every request again.
	d, err := dists.Get(e.ctx, "rl-site")
	require.NoError(t, err)
	d.Restrictions.RateLimit = nil
	_, err = dists.Update(e.ctx, d, "")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		for range 10 {
			if get(e.srv.S3URL, "rl.site.test", "198.51.100.40").StatusCode != http.StatusOK {
				return false
			}
		}
		return true
	}, 30*time.Second, 200*time.Millisecond)
}
