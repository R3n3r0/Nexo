package trace_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/trace"
	tracemock "github.com/r3n3r0/nexo/internal/service/trace/mock"
	"github.com/r3n3r0/nexo/internal/testutil/redaction"
)

// receive waits for the next event of sub.
func receive(t *testing.T, sub trace.Subscription) trace.Event {
	t.Helper()
	select {
	case e, ok := <-sub.Events():
		require.True(t, ok, "the subscription ended")
		return e
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no event")
		return trace.Event{}
	}
}

// none checks that no event is pending.
func none(t *testing.T, sub trace.Subscription) {
	t.Helper()
	select {
	case e := <-sub.Events():
		assert.Failf(t, "unexpected event", "%+v", e)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDisabledWithoutSubscribers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := trace.New(trace.Deps{NodeID: "n1"})
	require.NoError(t, err)
	assert.False(t, svc.Enabled(), "no subscriber, nothing to trace")
	svc.Publish(ctx, trace.Event{Type: trace.TypeS3})

	sub, err := svc.Subscribe(ctx, trace.Filter{})
	require.NoError(t, err)
	assert.True(t, svc.Enabled())
	svc.Publish(ctx, trace.Event{Type: trace.TypeS3, API: "GetObject"})
	e := receive(t, sub)
	assert.Equal(t, "n1", e.Node, "the node is filled in")
	assert.Equal(t, "GetObject", e.API)

	cancel()
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-sub.Events():
			return !ok
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond, "the subscription ends with its context")
	assert.Eventually(t, func() bool { return !svc.Enabled() }, 5*time.Second, 5*time.Millisecond)
}

// covers: S-OPS-07
func TestFilters(t *testing.T) {
	events := []trace.Event{
		{Node: "n1", Type: trace.TypeS3, API: "PutObject", Bucket: "photos", Status: 200, Duration: 10 * time.Millisecond},
		{Node: "n1", Type: trace.TypeS3, API: "GetObject", Bucket: "docs", Status: 404, Duration: time.Second},
		{Node: "n1", Type: trace.TypeAdmin, API: "GET /info", Status: 500, Duration: 2 * time.Second},
		{Node: "n1", Type: trace.TypeRPC, API: "/storage/v1/read", Status: 200},
		{Node: "n1", Type: trace.TypeWeb, API: "GetObject", Bucket: "site", Status: 200, Error: "client went away"},
	}
	tests := []struct {
		name   string
		filter trace.Filter
		want   []int
	}{
		{"default skips rpc", trace.Filter{}, []int{0, 1, 2, 4}},
		{"rpc on request", trace.Filter{Types: []trace.Type{trace.TypeRPC}}, []int{3}},
		{"types", trace.Filter{Types: []trace.Type{trace.TypeS3, trace.TypeWeb}}, []int{0, 1, 4}},
		{"api case-insensitive", trace.Filter{APIs: []string{"getobject"}}, []int{1, 4}},
		{"bucket", trace.Filter{Buckets: []string{"photos", "site"}}, []int{0, 4}},
		{"status codes", trace.Filter{StatusCodes: []int{404}}, []int{1}},
		{"status classes", trace.Filter{StatusClasses: []int{4, 5}}, []int{1, 2}},
		{"codes or classes", trace.Filter{StatusCodes: []int{200}, StatusClasses: []int{5}}, []int{0, 2, 4}},
		{"errors only", trace.Filter{ErrorsOnly: true}, []int{1, 2, 4}},
		{"duration threshold", trace.Filter{MinDuration: time.Second}, []int{1, 2}},
		{"node", trace.Filter{Nodes: []string{"n2"}}, nil},
		{"combined", trace.Filter{Types: []trace.Type{trace.TypeS3}, ErrorsOnly: true, MinDuration: 500 * time.Millisecond}, []int{1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc, err := trace.New(trace.Deps{NodeID: "n1"})
			require.NoError(t, err)
			sub, err := svc.Subscribe(ctx, tc.filter)
			require.NoError(t, err)
			for _, e := range events {
				svc.Publish(ctx, e)
			}
			for _, i := range tc.want {
				assert.Equal(t, events[i], receive(t, sub))
			}
			none(t, sub)
		})
	}
}

func TestInvalidFilters(t *testing.T) {
	svc, err := trace.New(trace.Deps{})
	require.NoError(t, err)
	for name, f := range map[string]trace.Filter{
		"type":      {Types: []trace.Type{"ftp"}},
		"status":    {StatusCodes: []int{42}},
		"class":     {StatusClasses: []int{7}},
		"threshold": {MinDuration: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Subscribe(context.Background(), f)
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
}

// TestRedaction checks that no credential, signature or token of a query
// string reaches a subscriber (S-OPS-07).
//
// covers: S-OPS-07
func TestRedaction(t *testing.T) {
	secrets := map[string]string{
		"X-Amz-Signature":      "f00dfacecafe",
		"X-Amz-Credential":     "AKIDSECRET%2F20260101%2Fus-east-1%2Fs3%2Faws4_request",
		"X-Amz-Security-Token": "sessiontokenvalue",
		"Signature":            "v2signaturevalue",
		"AWSAccessKeyId":       "AKIDV2VALUE",
		"Key-Pair-Id":          "KPIDVALUE",
		"Policy":               "base64policyvalue",
		"access_token":         "oauthtokenvalue",
		"client_secret":        "clientsecretvalue",
		"password":             "hunter2value",
	}
	var parts []string
	for k, v := range secrets {
		parts = append(parts, k+"="+v)
	}
	parts = append(parts, "versionId=v1", "list-type=2", "prefix=a%2Fb")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := trace.New(trace.Deps{})
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctx, trace.Filter{})
	require.NoError(t, err)
	svc.Publish(ctx, trace.Event{Type: trace.TypeS3, Query: strings.Join(parts, "&")})
	got := receive(t, sub).Query
	for k, v := range secrets {
		assert.NotContains(t, got, v, k)
		assert.Contains(t, got, k+"=REDACTED")
	}
	assert.Contains(t, got, "versionId=v1")
	assert.Contains(t, got, "list-type=2")
	assert.Contains(t, got, "prefix=a%2Fb", "the other pairs keep their encoding")

	svc.Publish(ctx, trace.Event{Type: trace.TypeS3, Query: "%zz=secret&a=1"})
	assert.Equal(t, "REDACTED&a=1", receive(t, sub).Query, "an undecodable name is redacted whole")

	// The table every redaction shares.
	svc.Publish(ctx, trace.Event{Type: trace.TypeS3, Query: redaction.Query})
	got = receive(t, sub).Query
	assert.Empty(t, redaction.Leaks(got))
	for _, kept := range redaction.Kept() {
		assert.Contains(t, got, kept)
	}
}

// TestBackpressure checks that a slow subscriber loses events, counted,
// and never blocks the requests.
func TestBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	svc, err := trace.New(trace.Deps{Registry: reg}, trace.WithBuffer(2))
	require.NoError(t, err)
	slow, err := svc.Subscribe(ctx, trace.Filter{})
	require.NoError(t, err)
	fast, err := svc.Subscribe(ctx, trace.Filter{})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			svc.Publish(ctx, trace.Event{Type: trace.TypeS3, Status: 200 + i})
			// fast drains as it goes: it never loses.
			<-fast.Events()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Publish blocked on a slow subscriber")
	}
	assert.Equal(t, uint64(3), slow.Dropped())
	assert.Zero(t, fast.Dropped())
	assert.Equal(t, 200, receive(t, slow).Status)
	assert.Equal(t, 201, receive(t, slow).Status)

	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "nexo_trace_subscribers 2")
	assert.Contains(t, string(body), "nexo_trace_dropped_total")
}

// TestPeers checks the cluster trace: the events of the peers reach the
// subscriber, filtered, and a broken stream is opened again.
//
// covers: S-OPS-07, S-STO-06
func TestPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := tracemock.NewPeer(t)
	peer.EXPECT().NodeID().Return("n2")
	var calls atomic.Int32
	peer.EXPECT().Trace(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, f trace.Filter, fn func(trace.Delivery) error) error {
			assert.True(t, f.Local, "a peer answers with its own events only")
			assert.Empty(t, f.Nodes)
			assert.Equal(t, []string{"PutObject"}, f.APIs, "the filter applies on the peer")
			if calls.Add(1) == 1 {
				// First stream: one event, then it breaks.
				require.NoError(t, fn(trace.Delivery{Event: &trace.Event{Node: "n2", Type: trace.TypeS3, API: "PutObject", Status: 200, Query: "X-Amz-Signature=abc"}}))
				return errors.New("connection reset")
			}
			require.NoError(t, fn(trace.Delivery{Event: &trace.Event{Node: "n2", Type: trace.TypeS3, API: "PutObject", Status: 201}}))
			require.NoError(t, fn(trace.Delivery{Dropped: 4}))
			<-ctx.Done()
			return ctx.Err()
		})
	svc, err := trace.New(trace.Deps{NodeID: "n1", Peers: []trace.Peer{peer}}, trace.WithReconnect(time.Millisecond, 5*time.Millisecond))
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctx, trace.Filter{APIs: []string{"PutObject"}})
	require.NoError(t, err)

	first := receive(t, sub)
	assert.Equal(t, "n2", first.Node)
	assert.Equal(t, "X-Amz-Signature=REDACTED", first.Query, "the events of the peers are redacted too")
	assert.Equal(t, 201, receive(t, sub).Status, "the stream is opened again")
	assert.Eventually(t, func() bool { return sub.Dropped() == 4 }, 5*time.Second, 5*time.Millisecond, "the losses of the peer are reported")

	svc.Publish(ctx, trace.Event{Type: trace.TypeS3, API: "PutObject", Status: 202})
	local := receive(t, sub)
	assert.Equal(t, "n1", local.Node)
	assert.Equal(t, 202, local.Status)
}

func TestPeersSelectedByNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := tracemock.NewPeer(t)
	peer.EXPECT().NodeID().Return("n2")
	// No Trace expectation: a filter on n1 does not open the stream of n2,
	// and a local subscription opens none.
	svc, err := trace.New(trace.Deps{NodeID: "n1", Peers: []trace.Peer{peer}})
	require.NoError(t, err)
	_, err = svc.Subscribe(ctx, trace.Filter{Nodes: []string{"n1"}})
	require.NoError(t, err)
	_, err = svc.Subscribe(ctx, trace.Filter{Local: true})
	require.NoError(t, err)
}
