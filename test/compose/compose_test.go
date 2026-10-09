//go:build compose

package compose_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settings of the deployment under test (see doc.go).
type settings struct {
	s3, console, user, password, file, stop string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func load(t *testing.T) settings {
	t.Helper()
	s := settings{
		s3:       strings.TrimSuffix(env("NEXO_COMPOSE_S3", "http://localhost:9000"), "/"),
		console:  strings.TrimSuffix(env("NEXO_COMPOSE_CONSOLE", "http://localhost:9001"), "/"),
		user:     env("NEXO_COMPOSE_USER", "admin"),
		password: env("NEXO_COMPOSE_PASSWORD", "change-me-please"),
		file:     env("NEXO_COMPOSE_FILE", "deploy/docker/compose.cluster.yml"),
		stop:     env("NEXO_COMPOSE_STOP", "nexo3"),
	}
	waitReady(t, s, 3*time.Minute)
	return s
}

// waitReady waits until the load balancer answers the readiness probe.
func waitReady(t *testing.T, s settings, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(s.s3 + "/nexo/health/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the deployment is not ready after %s: %v", timeout, err)
		}
		time.Sleep(time.Second)
	}
}

// client is an S3 client of the load balancer that retries like an
// application in front of a cluster losing a node.
func client(s settings) *s3.Client {
	return s3.New(s3.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider(s.user, s.password, ""),
		BaseEndpoint: aws.String(s.s3),
		UsePathStyle: true,
		HTTPClient:   &http.Client{Timeout: time.Minute},
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 20
			o.MaxBackoff = 2 * time.Second
			o.RateLimiter = ratelimit.None
		}),
	})
}

// compose runs docker compose on the deployment file.
func compose(t *testing.T, s settings, args ...string) {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"compose", "-f", s.file}, args...)...).CombinedOutput()
	require.NoError(t, err, "docker compose %s: %s", strings.Join(args, " "), out)
}

func bucketName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestTM06StopContainerNoClientError (docs/08 TM-06): a container of the
// cluster stopped and started again while clients write and read through
// the load balancer: no client error (with the retries of the SDK), and
// every write acknowledged reads back.
func TestTM06StopContainerNoClientError(t *testing.T) {
	s := load(t)
	ctx := context.Background()
	c := client(s)
	bucket := bucketName("tm06")
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)

	var (
		mu      sync.Mutex
		written = map[string][32]byte{}
		errs    []error
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 6))
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("w%d/k%03d", w, rng.IntN(50))
				body := make([]byte, 1+rng.IntN(256<<10))
				for j := range body {
					body[j] = byte(rng.UintN(256))
				}
				if i%2 == 0 {
					_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body)})
					mu.Lock()
					if err != nil {
						errs = append(errs, fmt.Errorf("put %s: %w", key, err))
					} else {
						written[key] = sha256.Sum256(body)
					}
					mu.Unlock()
					continue
				}
				out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
				var nsk interface{ ErrorCode() string }
				if errors.As(err, &nsk) && nsk.ErrorCode() == "NoSuchKey" {
					continue
				}
				if err == nil {
					_, err = io.Copy(io.Discard, out.Body)
					_ = out.Body.Close()
				}
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("get %s: %w", key, err))
					mu.Unlock()
				}
			}
		}()
	}
	time.Sleep(5 * time.Second)
	compose(t, s, "stop", s.stop)
	time.Sleep(20 * time.Second)
	compose(t, s, "start", s.stop)
	waitReady(t, s, 3*time.Minute)
	time.Sleep(10 * time.Second)
	close(stop)
	wg.Wait()

	assert.Empty(t, errs, "no client error while %s was stopped", s.stop)
	for key, sum := range written {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err, key)
		data, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		require.NoError(t, err, key)
		assert.Equal(t, sum, sha256.Sum256(data), "%s holds the last acknowledged write", key)
	}
}

// TestTM09PresignedBehindNginx (docs/08 TM-09, TPS-19): the presigned
// URL and the share link the console generates behind nginx (with
// NEXO_SERVER_URL the public URL of the load balancer) download from
// outside the compose network.
func TestTM09PresignedBehindNginx(t *testing.T) {
	s := load(t)
	ctx := context.Background()
	c := client(s)
	bucket := bucketName("tm09")
	_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	body := bytes.Repeat([]byte("TM-09 "), 10000)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("report.bin"), Body: bytes.NewReader(body)})
	require.NoError(t, err)

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	web := &http.Client{Timeout: 30 * time.Second, Jar: jar}
	call := func(method, path, csrf, payload string) []byte {
		t.Helper()
		req, err := http.NewRequest(method, s.console+"/api/v1"+path, strings.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := web.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Less(t, resp.StatusCode, 300, "%s %s: %s", method, path, data)
		return data
	}
	var login struct {
		CSRFToken string `json:"csrfToken"`
	}
	require.NoError(t, json.Unmarshal(call(http.MethodPost, "/auth/login", "", fmt.Sprintf(`{"accessKey":%q,"secretKey":%q}`, s.user, s.password)), &login))
	var presigned, shared struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(call(http.MethodPost, "/presign", login.CSRFToken,
		fmt.Sprintf(`{"bucket":%q,"key":"report.bin","expiresIn":3600}`, bucket)), &presigned))
	require.NoError(t, json.Unmarshal(call(http.MethodPost, "/buckets/"+bucket+"/objects/share", login.CSRFToken,
		`{"key":"report.bin","expiresIn":3600}`), &shared))
	for _, link := range []string{presigned.URL, shared.URL} {
		require.True(t, strings.HasPrefix(link, s.s3+"/"), "the link uses the public URL %s: %s", s.s3, link)
		for range 4 { // nginx spreads the requests over the nodes
			resp, err := http.Get(link)
			require.NoError(t, err)
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", link, data)
			assert.Equal(t, body, data)
		}
	}
}
