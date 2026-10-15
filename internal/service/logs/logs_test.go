package logs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/logs"
	logsmock "github.com/r3n3r0/nexo/internal/service/logs/mock"
	"github.com/r3n3r0/nexo/internal/testutil/redaction"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of slog.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func next(t *testing.T, sub logs.Subscription) logs.Entry {
	t.Helper()
	select {
	case e, ok := <-sub.Entries():
		require.True(t, ok, "the subscription ended")
		return e
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no entry")
		return logs.Entry{}
	}
}

func nothing(t *testing.T, sub logs.Subscription) {
	t.Helper()
	select {
	case e := <-sub.Entries():
		assert.Failf(t, "unexpected entry", "%+v", e)
	case <-time.After(20 * time.Millisecond):
	}
}

// covers: S-OPS-05
func TestTeeHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	svc, err := logs.New(logs.Deps{Output: out, NodeID: "n1"})
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctx, logs.Filter{})
	require.NoError(t, err)

	logger := slog.New(svc.Handler()).With("component", "heal").WithGroup("job")
	logger.Info("healing started", "id", "h1", slog.Group("drive", "path", "/d1", "pool", 0))

	e := next(t, sub)
	assert.Equal(t, "n1", e.Node)
	assert.Equal(t, "INFO", e.Level)
	assert.Equal(t, "healing started", e.Message)
	assert.False(t, e.Time.IsZero())
	assert.Equal(t, map[string]string{"component": "heal", "job.id": "h1", "job.drive.path": "/d1", "job.drive.pool": "0"}, e.Attrs)

	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out.String())), &line), out.String())
	assert.Equal(t, "healing started", line["msg"])
	assert.Equal(t, "heal", line["component"])
	assert.Equal(t, map[string]any{"id": "h1", "drive": map[string]any{"path": "/d1", "pool": float64(0)}}, line["job"])
}

// covers: S-OPS-05
func TestLevelsAndFormat(t *testing.T) {
	ctx := context.Background()
	out := &syncBuffer{}
	svc, err := logs.New(logs.Deps{Output: out}, logs.WithLevel("warn"))
	require.NoError(t, err)
	logger := slog.New(svc.Handler())
	derived := logger.With("k", "v")

	logger.Info("hidden")
	logger.Warn("shown")
	assert.NotContains(t, out.String(), "hidden")
	assert.Contains(t, out.String(), `"msg":"shown"`)

	require.NoError(t, svc.Configure(ctx, logs.Settings{Level: "debug", Format: "text"}))
	out.Reset()
	logger.Debug("debug now")
	derived.Info("derived")
	assert.Contains(t, out.String(), "level=DEBUG msg=\"debug now\"")
	assert.Contains(t, out.String(), "msg=derived k=v", "the derived loggers switch format too")

	// Empty settings restore the startup ones.
	require.NoError(t, svc.Configure(ctx, logs.Settings{}))
	out.Reset()
	logger.Info("hidden again")
	derived.Error("json again")
	assert.NotContains(t, out.String(), "hidden again")
	assert.Contains(t, out.String(), `"msg":"json again","k":"v"`)

	require.ErrorIs(t, svc.Configure(ctx, logs.Settings{Level: "loud"}), domain.ErrInvalidArgument)
	require.ErrorIs(t, svc.Configure(ctx, logs.Settings{Format: "xml"}), domain.ErrInvalidArgument)
}

func TestBaseHandler(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	svc, err := logs.New(logs.Deps{Base: base})
	require.NoError(t, err)
	logger := slog.New(svc.Handler())
	logger.Debug("filtered by the level of the stream")
	logger.Info("kept")
	assert.NotContains(t, buf.String(), "filtered")
	assert.Contains(t, buf.String(), "msg=kept")

	// Without output nor base the entries are only streamed.
	svc, err = logs.New(logs.Deps{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := svc.Subscribe(ctx, logs.Filter{})
	require.NoError(t, err)
	slog.New(svc.Handler()).Warn("only streamed")
	assert.Equal(t, "only streamed", next(t, sub).Message)
}

// TestRedaction checks that no credential reaches the stream (S-OPS-07):
// the authorization headers, the presigned signatures and credentials,
// the tokens and the secrets named as such.
func TestRedaction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := logs.New(logs.Deps{})
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctx, logs.Filter{})
	require.NoError(t, err)
	logger := slog.New(svc.Handler())

	logger.Info("request Authorization: AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0123abcdsig",
		"Authorization", "Bearer eyJsecretjwt",
		"query", "list-type=2&X-Amz-Credential=AKIDQUERY%2F2026&X-Amz-Signature=fedcbaquerysig&X-Amz-Security-Token=stsTokenValue",
		"url", "https://hook.example/notify?token=webhookTokenValue&x=1",
		"secret_key", "rootSecretValue",
		slog.Group("headers", "Cookie", "nexo_session=cookieValue", "X-Amz-Server-Side-Encryption-Customer-Key", "c3NlY0tleVZhbHVl"),
		"note", "Basic dXNlcjpiYXNpY3Bhc3M=",
	)
	e := next(t, sub)
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	for _, secret := range []string{"AKIDEXAMPLE", "0123abcdsig", "eyJsecretjwt", "AKIDQUERY", "fedcbaquerysig", "stsTokenValue",
		"webhookTokenValue", "rootSecretValue", "cookieValue", "c3NlY0tleVZhbHVl", "dXNlcjpiYXNpY3Bhc3M="} {
		assert.NotContains(t, string(raw), secret)
	}
	assert.NotContains(t, e.Message, "Signature=0123")
	assert.Contains(t, e.Message, "AWS4-HMAC-SHA256 REDACTED")
	assert.Equal(t, "REDACTED", e.Attrs["Authorization"])
	assert.Equal(t, "REDACTED", e.Attrs["headers.Cookie"])
	assert.Contains(t, e.Attrs["query"], "list-type=2", "the rest of the value stays")
	assert.Contains(t, e.Attrs["url"], "x=1")

	// The table every redaction shares (regression: password and secret
	// values reached the log stream).
	logger.Info("http request", "query", redaction.Query, "url", "https://example.com/x?"+redaction.Query)
	e = next(t, sub)
	raw, err = json.Marshal(e)
	require.NoError(t, err)
	assert.Empty(t, redaction.Leaks(string(raw)))
	for _, kept := range redaction.Kept() {
		assert.Contains(t, e.Attrs["query"], kept)
	}
}

// covers: S-OPS-07
func TestReplayAndRing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := logs.New(logs.Deps{}, logs.WithRingSize(4), logs.WithLevel("debug"))
	require.NoError(t, err)
	logger := slog.New(svc.Handler())
	for _, m := range []string{"a", "b", "c", "d", "e", "f"} {
		level := slog.LevelInfo
		if m == "e" {
			level = slog.LevelError
		}
		logger.Log(ctx, level, m)
	}
	sub, err := svc.Subscribe(ctx, logs.Filter{Last: 3})
	require.NoError(t, err)
	assert.Equal(t, "d", next(t, sub).Message)
	assert.Equal(t, "e", next(t, sub).Message)
	assert.Equal(t, "f", next(t, sub).Message)
	nothing(t, sub)

	all, err := svc.Subscribe(ctx, logs.Filter{Last: 100})
	require.NoError(t, err)
	for _, want := range []string{"c", "d", "e", "f"} {
		assert.Equal(t, want, next(t, all).Message, "the ring keeps the last 4")
	}
	errs, err := svc.Subscribe(ctx, logs.Filter{Last: 10, Level: "error"})
	require.NoError(t, err)
	assert.Equal(t, "e", next(t, errs).Message)
	nothing(t, errs)
	logger.Warn("live warn")
	logger.Error("live error")
	assert.Equal(t, "live error", next(t, errs).Message, "the level filters the live entries")

	other, err := svc.Subscribe(ctx, logs.Filter{Last: 10, Nodes: []string{"elsewhere"}})
	require.NoError(t, err)
	nothing(t, other)
}

func TestInvalidFilter(t *testing.T) {
	svc, err := logs.New(logs.Deps{})
	require.NoError(t, err)
	for _, f := range []logs.Filter{{Level: "loud"}, {Last: -1}, {Last: 10001}} {
		_, err := svc.Subscribe(context.Background(), f)
		require.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", f)
	}
}

func TestBackpressureAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc, err := logs.New(logs.Deps{}, logs.WithBuffer(2))
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctx, logs.Filter{})
	require.NoError(t, err)
	logger := slog.New(svc.Handler())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			logger.Info("entry")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "logging blocked on a slow subscriber")
	}
	assert.Equal(t, uint64(3), sub.Dropped())
	cancel()
	require.Eventually(t, func() bool {
		for {
			select {
			case _, ok := <-sub.Entries():
				if !ok {
					return true
				}
			default:
				return false
			}
		}
	}, 5*time.Second, 5*time.Millisecond, "the subscription ends with its context")
	logger.Info("after the end") // no panic on a closed subscription
}

// covers: S-OPS-07
func TestCluster(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	local, err := logs.New(logs.Deps{NodeID: "n1"})
	require.NoError(t, err)
	peer := logsmock.NewPeer(t)
	peer.EXPECT().NodeID().Return("n2")
	var calls atomic.Int32
	peer.EXPECT().Logs(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, f logs.Filter, fn func(logs.Delivery) error) error {
			assert.True(t, f.Local)
			assert.Empty(t, f.Nodes)
			if calls.Add(1) == 1 {
				assert.Equal(t, 5, f.Last, "the first stream replays")
				require.NoError(t, fn(logs.Delivery{Entry: &logs.Entry{Node: "n2", Level: "INFO", Message: "remote token=abc123remote"}}))
				require.NoError(t, fn(logs.Delivery{Entry: &logs.Entry{Node: "n2", Level: "DEBUG", Message: "filtered"}}))
				return errors.New("stream broken")
			}
			assert.Zero(t, f.Last, "a stream opened again does not replay")
			require.NoError(t, fn(logs.Delivery{Entry: &logs.Entry{Node: "n2", Level: "WARN", Message: "again",
				Attrs: map[string]string{"password": "p4ss"}}}))
			require.NoError(t, fn(logs.Delivery{Dropped: 2}))
			<-ctx.Done()
			return ctx.Err()
		})
	svc, err := logs.NewCluster(logs.ClusterDeps{Local: local, Peers: []logs.Peer{peer}}, logs.WithReconnect(time.Millisecond, 5*time.Millisecond))
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctx, logs.Filter{Level: "info", Last: 5})
	require.NoError(t, err)

	first := next(t, sub)
	assert.Equal(t, "n2", first.Node)
	assert.Equal(t, "remote token=REDACTED", first.Message, "the entries of the peers are redacted")
	second := next(t, sub)
	assert.Equal(t, "again", second.Message)
	assert.Equal(t, "REDACTED", second.Attrs["password"])
	slog.New(svc.Handler()).Info("local")
	third := next(t, sub)
	assert.Equal(t, "n1", third.Node)
	assert.Eventually(t, func() bool { return sub.Dropped() == 2 }, 5*time.Second, 5*time.Millisecond)

	// Local or restricted to this node, the peers are not asked.
	own, err := svc.Subscribe(ctx, logs.Filter{Local: true})
	require.NoError(t, err)
	assert.NotNil(t, own)
	require.NoError(t, svc.Configure(ctx, logs.Settings{Level: "debug"}))

	_, err = logs.NewCluster(logs.ClusterDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestConfigurator(t *testing.T) {
	ctx := context.Background()
	cfg := config.New(config.Deps{Store: config.NewMemoryStore(map[string]config.KV{logs.SubsystemLogger: {logs.KeyLevel: "error"}})})
	out := &syncBuffer{}
	svc, err := logs.New(logs.Deps{Output: out})
	require.NoError(t, err)
	c, err := logs.NewConfigurator(logs.ConfiguratorDeps{Config: cfg, Logs: svc, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	require.NoError(t, c.Start(ctx))
	defer func() { require.NoError(t, c.Stop(ctx)) }()
	logger := slog.New(svc.Handler())

	logger.Warn("hidden")
	assert.NotContains(t, out.String(), "hidden", "the stored level applies at start")

	require.NoError(t, cfg.Set(ctx, logs.SubsystemLogger, config.KV{logs.KeyLevel: "info", logs.KeyFormat: "text"}))
	require.Eventually(t, func() bool {
		logger.Info("probe")
		return strings.Contains(out.String(), "level=INFO msg=probe")
	}, 5*time.Second, 10*time.Millisecond, "a change applies at once (hot)")

	require.NoError(t, cfg.Set(ctx, logs.SubsystemLogger, config.KV{logs.KeyLevel: "loud"}))
	time.Sleep(20 * time.Millisecond)
	out.Reset()
	logger.Info("still info")
	assert.Contains(t, out.String(), "still info", "an invalid value keeps the previous settings")

	_, err = logs.NewConfigurator(logs.ConfiguratorDeps{Logs: svc})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
