package metrics_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

func scrape(t *testing.T, reg metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

func TestRegistryInstruments(t *testing.T) {
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())

	c, err := reg.Counter(ctx, metrics.Definition{Subsystem: "test", Name: "events_total", Help: "Events.", Labels: []string{"kind"}})
	require.NoError(t, err)
	c.Add(ctx, 2, "a")
	c.Add(ctx, 1, "a")

	g, err := reg.Gauge(ctx, metrics.Definition{Subsystem: "test", Name: "level", Help: "Level.", Labels: []string{"server"}})
	require.NoError(t, err)
	g.Set(ctx, 5, "s3")
	g.Add(ctx, -2, "s3")

	h, err := reg.Histogram(ctx, metrics.Definition{Subsystem: "test", Name: "latency_seconds", Help: "Latency."})
	require.NoError(t, err)
	h.Observe(ctx, 0.02)

	out := scrape(t, reg)
	assert.Contains(t, out, `nexo_test_events_total{kind="a"} 3`)
	assert.Contains(t, out, `nexo_test_level{server="s3"} 3`)
	assert.Contains(t, out, `nexo_test_latency_seconds_bucket{le="0.025"} 1`)
	assert.Contains(t, out, `nexo_test_latency_seconds_bucket{le="30"} 1`, "default latency buckets")
	assert.Contains(t, out, `nexo_test_latency_seconds_count 1`)
}

func TestRegistryIdempotent(t *testing.T) {
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	def := metrics.Definition{Subsystem: "http", Name: "requests_total", Help: "Requests.", Labels: []string{"server"}}

	first, err := reg.Counter(ctx, def)
	require.NoError(t, err)
	second, err := reg.Counter(ctx, def)
	require.NoError(t, err, "a second registration returns the existing vector")
	first.Add(ctx, 1, "s3")
	second.Add(ctx, 1, "console")
	second.Add(ctx, 1, "s3")

	out := scrape(t, reg)
	assert.Contains(t, out, `nexo_http_requests_total{server="s3"} 2`)
	assert.Contains(t, out, `nexo_http_requests_total{server="console"} 1`)
}

func TestRegistryInvalidDefinitions(t *testing.T) {
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	_, err := reg.Counter(ctx, metrics.Definition{Name: "taken_total", Help: "Taken.", Labels: []string{"a"}})
	require.NoError(t, err)

	tests := []struct {
		name     string
		register func() error
	}{
		{"missing name", func() error {
			_, err := reg.Counter(ctx, metrics.Definition{Help: "x"})
			return err
		}},
		{"missing help", func() error {
			_, err := reg.Gauge(ctx, metrics.Definition{Name: "g"})
			return err
		}},
		{"invalid metric name", func() error {
			_, err := reg.Counter(ctx, metrics.Definition{Name: "bad-name", Help: "x"})
			return err
		}},
		{"invalid label name", func() error {
			_, err := reg.Gauge(ctx, metrics.Definition{Name: "g2", Help: "x", Labels: []string{"bad-label"}})
			return err
		}},
		{"same name other labels", func() error {
			_, err := reg.Counter(ctx, metrics.Definition{Name: "taken_total", Help: "Taken.", Labels: []string{"b"}})
			return err
		}},
		{"same name other kind", func() error {
			_, err := reg.Gauge(ctx, metrics.Definition{Name: "taken_total", Help: "Taken.", Labels: []string{"a"}})
			return err
		}},
		{"label starting with a digit", func() error {
			_, err := reg.Gauge(ctx, metrics.Definition{Name: "g3", Help: "x", Labels: []string{"1a"}})
			return err
		}},
		{"reserved label prefix", func() error {
			_, err := reg.Gauge(ctx, metrics.Definition{Name: "g4", Help: "x", Labels: []string{"__name"}})
			return err
		}},
		{"duplicate label", func() error {
			_, err := reg.Gauge(ctx, metrics.Definition{Name: "g5", Help: "x", Labels: []string{"a", "a"}})
			return err
		}},
		{"histogram unordered buckets", func() error {
			_, err := reg.Histogram(ctx, metrics.Definition{Name: "h1", Help: "x", Buckets: []float64{1, 0.5}})
			return err
		}},
		{"histogram reserved label", func() error {
			_, err := reg.Histogram(ctx, metrics.Definition{Name: "h2", Help: "x", Labels: []string{"le"}})
			return err
		}},
		{"histogram clashes with counter", func() error {
			_, err := reg.Histogram(ctx, metrics.Definition{Name: "taken_total", Help: "Taken.", Labels: []string{"a"}})
			return err
		}},
		{"counter clashes with gauge", func() error {
			if _, err := reg.Gauge(ctx, metrics.Definition{Name: "level", Help: "L."}); err != nil {
				return err
			}
			_, err := reg.Counter(ctx, metrics.Definition{Name: "level", Help: "L."})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.register(), domain.ErrInvalidArgument)
		})
	}
}

func TestRegistryDropsBadSamples(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	reg := metrics.New(metrics.Deps{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, metrics.WithoutRuntimeCollectors())

	c, err := reg.Counter(ctx, metrics.Definition{Name: "c_total", Help: "C.", Labels: []string{"a"}})
	require.NoError(t, err)
	g, err := reg.Gauge(ctx, metrics.Definition{Name: "g", Help: "G.", Labels: []string{"a"}})
	require.NoError(t, err)
	h, err := reg.Histogram(ctx, metrics.Definition{Name: "h", Help: "H.", Labels: []string{"a"}})
	require.NoError(t, err)

	tests := []struct {
		name   string
		update func()
	}{
		{"counter wrong label count", func() { c.Add(ctx, 1) }},
		{"counter negative delta", func() { c.Add(ctx, -1, "x") }},
		{"gauge add wrong label count", func() { g.Add(ctx, 1, "x", "y") }},
		{"gauge set wrong label count", func() { g.Set(ctx, 1) }},
		{"histogram wrong label count", func() { h.Observe(ctx, 1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs.Reset()
			assert.NotPanics(t, tt.update)
			assert.Contains(t, logs.String(), "metrics: sample dropped")
		})
	}
	assert.NotContains(t, scrape(t, reg), "nexo_c_total{", "dropped samples leave no series")
}

func TestRuntimeCollectors(t *testing.T) {
	out := scrape(t, metrics.New(metrics.Deps{}))
	assert.Contains(t, out, "go_goroutines")
}
