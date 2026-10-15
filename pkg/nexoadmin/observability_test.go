package nexoadmin_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// streamClient returns a client without the contract transport, which
// reads whole bodies: the streams are read as they come.
func (h *harness) streamClient() nexoadmin.AdminClient {
	h.t.Helper()
	c, err := nexoadmin.New(nexoadmin.Config{
		Endpoint: h.srv.ConsoleURL, AccessKey: h.srv.RootUser, SecretKey: h.srv.RootPassword, Region: h.srv.Config.Region,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	})
	require.NoError(h.t, err)
	return c
}

func TestTraceStream(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(h.ctx, time.Minute)
	defer cancel()
	_, err := h.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("traced")})
	require.NoError(t, err)

	stream, err := h.streamClient().Trace(ctx, nexoadmin.TraceOptions{Types: []string{"s3"}, APIs: []string{"PutObject"}, Buckets: []string{"traced"}})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	// The subscription is set up once the stream answers: put until the
	// event comes (the first PUT may precede it).
	events := make(chan *nexoadmin.TraceEvent, 1)
	errs := make(chan error, 1)
	go func() {
		e, err := stream.Next()
		if err != nil {
			errs <- err
			return
		}
		events <- e
	}()
	var e *nexoadmin.TraceEvent
	require.Eventually(t, func() bool {
		h.put("traced", "k.txt", "hello")
		select {
		case e = <-events:
			return true
		case err := <-errs:
			require.NoError(t, err)
		case <-time.After(200 * time.Millisecond):
		}
		return false
	}, 30*time.Second, 10*time.Millisecond)
	assert.Equal(t, "s3", e.Type)
	assert.Equal(t, "PutObject", e.API)
	assert.Equal(t, http.MethodPut, e.Method)
	assert.Equal(t, "/traced/k.txt", e.Path)
	assert.Equal(t, http.StatusOK, e.Status)
	assert.Equal(t, int64(5), e.InputBytes)
	assert.Equal(t, h.srv.RootUser, e.User)
	assert.Equal(t, "traced", e.Bucket)
	assert.NotEmpty(t, e.RequestID)
	assert.NotEmpty(t, e.Node)
	require.NoError(t, stream.Close())
	require.NoError(t, stream.Close(), "idempotent")

	_, err = h.streamClient().Trace(ctx, nexoadmin.TraceOptions{Statuses: []string{"9xx"}})
	assert.Equal(t, nexoadmin.CodeInvalidArgument, apiError(t, err).Code)
}

func TestLogStream(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(h.ctx, time.Minute)
	defer cancel()
	stream, err := h.streamClient().Logs(ctx, nexoadmin.LogOptions{Level: "info", Last: 5})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	e, err := stream.Next()
	require.NoError(t, err, "the startup logs are replayed")
	assert.NotEmpty(t, e.Message)
	assert.NotEmpty(t, e.Node)
	assert.NotEqual(t, "DEBUG", e.Level)
	cancel()
	for i := 0; i < 100 && err == nil; i++ {
		_, err = stream.Next() // what was buffered, then the end
	}
	require.Error(t, err, "the stream ends with its context")
	assert.NotErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestAuditAndMetrics(t *testing.T) {
	h := newHarness(t)
	_, err := h.s3.CreateBucket(h.ctx, &s3.CreateBucketInput{Bucket: aws.String("audited")})
	require.NoError(t, err)
	h.put("audited", "a.txt", "hello")

	var list *nexoadmin.AuditList
	require.Eventually(t, func() bool {
		list, err = h.root.SearchAudit(h.ctx, nexoadmin.AuditQuery{Bucket: "audited", API: "PutObject", Type: "s3", Limit: 10})
		require.NoError(t, err)
		return len(list.Items) == 1
	}, 10*time.Second, 50*time.Millisecond)
	a := list.Items[0]
	assert.Equal(t, h.srv.RootUser, a.AccessKey)
	assert.Equal(t, http.StatusOK, a.StatusCode)
	assert.Equal(t, int64(5), a.RX)
	assert.Equal(t, "a.txt", a.Object)
	assert.NotEmpty(t, a.RequestID)

	errorsOnly, err := h.root.SearchAudit(h.ctx, nexoadmin.AuditQuery{Bucket: "audited", ErrorsOnly: true, Since: time.Now().Add(-time.Hour)})
	require.NoError(t, err)
	assert.Empty(t, errorsOnly.Items)

	metrics, err := h.root.ListMetrics(h.ctx)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, m := range metrics.Items {
		names[m.Name] = true
	}
	for _, want := range []string{"s3_requests", "s3_latency_p99", "drive_used_bytes", "drives_offline", "mrf_pending"} {
		assert.True(t, names[want], want)
	}
	series, err := h.root.QueryMetric(h.ctx, nexoadmin.MetricQuery{Metric: "goroutines", Range: 10 * time.Minute, Step: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, "goroutines", series.Metric.Name)
	assert.Equal(t, int64(60), series.StepSeconds)
	require.NotEmpty(t, series.Nodes)
	assert.Len(t, series.Cluster, len(series.Nodes[0].Points))
	assert.NotNil(t, series.Cluster[len(series.Cluster)-1].V, "the node samples at start")

	_, err = h.root.QueryMetric(h.ctx, nexoadmin.MetricQuery{Metric: "nope"})
	assert.Equal(t, http.StatusNotFound, apiError(t, err).Status)
	_, err = h.root.SearchAudit(h.ctx, nexoadmin.AuditQuery{Statuses: []string{strings.Repeat("9", 4)}})
	assert.Equal(t, nexoadmin.CodeInvalidArgument, apiError(t, err).Code)
}
