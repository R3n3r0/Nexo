package integration_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// lastNode returns the S3 URL of the last node of the deployment (the
// only one out of a cluster) and its node id ("local" out of a cluster).
func lastNode(e *env) (s3URL, nodeID string) {
	n := e.srv.Nodes[len(e.srv.Nodes)-1]
	if len(e.srv.Nodes) == 1 {
		return n.S3URL, "local"
	}
	u, _ := url.Parse(n.S3URL)
	return n.S3URL, u.Host
}

// nodeS3Client returns a root S3 client of one node (not the load
// balancer).
func nodeS3Client(e *env, s3URL string) *s3.Client {
	return s3.NewFromConfig(e.sdkConfig(e.rootCreds()), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3URL)
		o.UsePathStyle = true
	})
}

// nodeAdmin returns a root Admin API client of the first node (not the
// load balancer), without timeout for the streams.
func nodeAdmin(t *testing.T, e *env) nexoadmin.AdminClient {
	t.Helper()
	c, err := nexoadmin.New(nexoadmin.Config{Endpoint: e.srv.Nodes[0].ConsoleURL, AccessKey: e.srv.RootUser, SecretKey: e.srv.RootPassword,
		Region: e.srv.Config.Region, HTTPClient: &http.Client{}})
	require.NoError(t, err)
	return c
}

// TestLiveTrace follows the live trace on the first node while the last
// node serves a PUT (S-OPS-07): in a cluster the event crosses the
// internode stream; the query string reaches the subscriber without its
// signature.
//
// covers: S-OPS-07, D-MON-02
func TestLiveTrace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("traced")
	s3URL, nodeID := lastNode(e)
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Minute)
	defer cancel()

	stream, err := nodeAdmin(t, e).Trace(ctx, nexoadmin.TraceOptions{Types: []string{"s3"}, Buckets: []string{"traced"},
		APIs: []string{"PutObject", "PresignedGetObject"}})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	events := make(chan *nexoadmin.TraceEvent, 16)
	go func() {
		defer close(events)
		for {
			ev, err := stream.Next()
			if err != nil {
				return
			}
			events <- ev
		}
	}()

	client := nodeS3Client(e, s3URL)
	presigned, err := s3.NewPresignClient(client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("traced"), Key: aws.String("k")})
	require.NoError(t, err)
	var put, get *nexoadmin.TraceEvent
	var putID string
	require.Eventually(t, func() bool {
		if put == nil {
			out, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("traced"), Key: aws.String("k"), Body: strings.NewReader("hello")})
			require.NoError(t, err)
			putID, _ = awsmiddleware.GetRequestIDMetadata(out.ResultMetadata)
		} else {
			resp, err := http.Get(presigned.URL)
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}
		deadline := time.After(300 * time.Millisecond)
		for {
			select {
			case ev, ok := <-events:
				require.True(t, ok, "the stream ended")
				switch {
				case ev.API == "PutObject" && ev.RequestID == putID:
					put = ev
				case ev.API == "PresignedGetObject" && put != nil:
					get = ev
				}
				if put != nil && get != nil {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}, time.Minute, 10*time.Millisecond, "the requests served by %s reach the trace of the first node", nodeID)

	assert.Equal(t, nodeID, put.Node, "the event names the node that served the request")
	assert.Equal(t, http.MethodPut, put.Method)
	assert.Equal(t, "/traced/k", put.Path)
	assert.Equal(t, http.StatusOK, put.Status)
	assert.Equal(t, int64(5), put.InputBytes)
	assert.Equal(t, e.srv.RootUser, put.User)
	assert.Equal(t, "traced", put.Bucket)
	assert.Positive(t, put.DurationMs)

	assert.Equal(t, int64(5), get.OutputBytes)
	assert.Contains(t, get.Query, "X-Amz-Signature=REDACTED")
	assert.Contains(t, get.Query, "X-Amz-Credential=REDACTED")
	assert.NotContains(t, get.Query, e.srv.RootUser, "the access key of the credential is redacted")
	sig := regexp.MustCompile(`X-Amz-Signature=([0-9a-f]+)`).FindStringSubmatch(presigned.URL)
	require.Len(t, sig, 2)
	assert.NotContains(t, get.Query, sig[1])
}

// TestAuditLog checks the audit record of an authenticated request
// (S-OPS-06): the identity, the operation, the target, the outcome and
// the sizes, searchable from any node of the deployment.
//
// covers: S-OPS-06, D-MON-03
func TestAuditLog(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("audited")
	user := e.createUser("auditor-user", "auditor-secret-1", "readwrite")
	s3URL, nodeID := lastNode(e)
	ctx := e.ctx

	client := s3.NewFromConfig(e.sdkConfig(user), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3URL)
		o.UsePathStyle = true
	})
	out, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("audited"), Key: aws.String("dir/a.txt"), Body: strings.NewReader("hello")})
	require.NoError(t, err)
	requestID, ok := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata)
	require.True(t, ok)
	_, err = client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("audited"), Key: aws.String("missing")})
	require.Error(t, err)

	admin := nodeAdmin(t, e)
	var list *nexoadmin.AuditList
	require.Eventually(t, func() bool {
		list, err = admin.SearchAudit(ctx, nexoadmin.AuditQuery{RequestID: requestID})
		require.NoError(t, err)
		return len(list.Items) == 1
	}, 30*time.Second, 50*time.Millisecond)
	a := list.Items[0]
	assert.Equal(t, nodeID, a.Node)
	assert.Equal(t, "s3", a.Type)
	assert.Equal(t, "PutObject", a.API)
	assert.Equal(t, http.MethodPut, a.Method)
	assert.Equal(t, "audited", a.Bucket)
	assert.Equal(t, "dir/a.txt", a.Object)
	assert.Equal(t, http.StatusOK, a.StatusCode)
	assert.Equal(t, int64(5), a.RX)
	assert.Equal(t, user.AccessKey, a.AccessKey)
	assert.NotEmpty(t, a.RemoteHost)

	failed, err := admin.SearchAudit(ctx, nexoadmin.AuditQuery{User: user.AccessKey, ErrorsOnly: true})
	require.NoError(t, err)
	require.NotEmpty(t, failed.Items)
	assert.Equal(t, http.StatusNotFound, failed.Items[0].StatusCode)
	assert.Equal(t, "GetObject", failed.Items[0].API)

	// The console requests are audited too (type admin).
	consoleReqs, err := admin.SearchAudit(ctx, nexoadmin.AuditQuery{Type: "admin", API: "GET /audit", Limit: 5})
	require.NoError(t, err)
	assert.NotEmpty(t, consoleReqs.Items)

	// The audit search needs admin:ConsoleLog.
	readwrite, err := nexoadmin.New(nexoadmin.Config{Endpoint: e.srv.ConsoleURL, AccessKey: user.AccessKey, SecretKey: user.SecretKey, Region: e.srv.Config.Region})
	require.NoError(t, err)
	_, err = readwrite.SearchAudit(ctx, nexoadmin.AuditQuery{})
	var apiErr *nexoadmin.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Status)
}

// metricLine matches a sample of the text exposition format.
var metricLine = regexp.MustCompile(`^([a-z_:][a-z0-9_:]*)(\{[^}]*\})? (\S+)$`)

// scrape returns the samples of a metrics endpoint: name and labels to
// value.
func scrape(t *testing.T, rawURL string) map[string]float64 {
	t.Helper()
	resp, err := http.Get(rawURL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		m := metricLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		require.NoError(t, err)
		out[m[1]+m[2]] = v
	}
	require.NoError(t, sc.Err())
	return out
}

// sum adds the samples of a family whose labels contain every fragment.
func sum(samples map[string]float64, family string, fragments ...string) (float64, int) {
	var total float64
	n := 0
	for k, v := range samples {
		name, labels, _ := strings.Cut(k, "{")
		if name != family {
			continue
		}
		ok := true
		for _, f := range fragments {
			ok = ok && strings.Contains(labels, f)
		}
		if ok {
			total += v
			n++
		}
	}
	return total, n
}

// TestClusterMetrics scrapes the cluster and bucket endpoints of the
// first node after requests served by the last one (T-8.1): the families
// of every node with their server label, the cluster families from the
// state of the deployment, the bucket families summed over the nodes; the
// objects and the space of the server info, the data usage and the
// cluster families agree (regression: the server info and
// nexo_cluster_objects summed counters the bucket listing never fills,
// always 0); and the series of the console aggregated over the nodes
// (T-8.2).
//
// covers: D-MON-01, S-OPS-04, S-OPS-08, D-DASH-01
func TestClusterMetrics(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithConfig(func(c *config.Static) { c.PrometheusAuthType = config.PrometheusAuthPublic }))
	e.mkBucket("measured")
	s3URL, nodeID := lastNode(e)
	client := nodeS3Client(e, s3URL)
	for i := 0; i < 3; i++ {
		_, err := client.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("measured"), Key: aws.String("k" + strconv.Itoa(i)),
			Body: strings.NewReader("12345")})
		require.NoError(t, err)
	}
	first := e.srv.Nodes[0].S3URL
	nodes := len(e.srv.Nodes)

	cluster := scrape(t, first+"/nexo/metrics/v1/cluster")
	online, n := sum(cluster, "nexo_cluster_nodes_online")
	require.Equal(t, 1, n)
	assert.InDelta(t, float64(nodes), online, 0)
	drives, _ := sum(cluster, "nexo_cluster_drives_online")
	assert.Positive(t, drives)
	total, _ := sum(cluster, "nexo_cluster_capacity_usable_total_bytes")
	assert.Positive(t, total)
	puts, _ := sum(cluster, "nexo_s3_requests_total", `api="PutObject"`, `server="`+nodeID+`"`, `status="200"`)
	assert.GreaterOrEqual(t, puts, 3.0, "the families of the node that served the requests, labeled with it")
	servers := map[string]bool{}
	for k := range cluster {
		if m := regexp.MustCompile(`server="([^"]+)"`).FindStringSubmatch(k); m != nil && strings.HasPrefix(k, "nexo_drive_online{") {
			servers[m[1]] = true
		}
	}
	assert.Len(t, servers, nodes, "the drives of every node")
	if nodes > 1 {
		healthy, _ := sum(cluster, "nexo_cluster_sets_healthy")
		assert.Positive(t, healthy)
		_, rpc := sum(cluster, "nexo_rpc_requests_total")
		assert.Positive(t, rpc, "the internode calls are measured")
	}

	admin := nodeAdmin(t, e)
	info, err := admin.ServerInfo(e.ctx)
	require.NoError(t, err)
	usage, err := admin.DataUsage(e.ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 3, info.Objects, "the objects of the deployment")
	assert.EqualValues(t, 15, info.Usage)
	assert.Equal(t, usage.ObjectsCount, info.Objects, "server info and data usage agree")
	assert.Equal(t, usage.TotalSize, info.Usage)
	objects, _ := sum(cluster, "nexo_cluster_objects")
	assert.InDelta(t, 3, objects, 0)
	used, _ := sum(cluster, "nexo_cluster_usage_bytes")
	assert.InDelta(t, 15, used, 0)

	bucket := scrape(t, first+"/nexo/metrics/v1/bucket")
	rx, _ := sum(bucket, "nexo_bucket_traffic_received_bytes_total", `bucket="measured"`)
	assert.InDelta(t, 15, rx, 0, "summed over the nodes")
	for k := range bucket {
		assert.NotContains(t, k, "k0", "no object key labels a series")
	}

	node := scrape(t, e.srv.Nodes[0].S3URL+"/nexo/metrics/v1/node")
	_, n = sum(node, "nexo_drive_online")
	assert.Positive(t, n)

	// The series of the console: every node, and the cluster.
	require.Eventually(t, func() bool {
		series, err := admin.QueryMetric(e.ctx, nexoadmin.MetricQuery{Metric: "drives_online", Range: 5 * time.Minute})
		require.NoError(t, err)
		if len(series.Nodes) != nodes {
			return false
		}
		last := series.Cluster[len(series.Cluster)-1]
		return last.V != nil && *last.V == drives
	}, 30*time.Second, 200*time.Millisecond, "the cluster series sums the drives of the nodes")
}
