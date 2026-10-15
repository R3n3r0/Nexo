package nexoadmin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TraceOptions filter the live trace (Trace); empty fields select
// everything (every type but rpc without Types).
type TraceOptions struct {
	// Nodes restricts the nodes of a cluster.
	Nodes []string
	// Types are s3, admin, web and rpc.
	Types []string
	// APIs are operation names (PutObject, "GET /buckets").
	APIs    []string
	Buckets []string
	// Statuses are status codes ("404") or classes ("5xx").
	Statuses []string
	// ErrorsOnly keeps the failed requests (status >= 400).
	ErrorsOnly bool
	// Threshold keeps the requests slower than it.
	Threshold time.Duration
}

// TraceEvent is a request of the live trace. Query has its signatures,
// credentials and tokens redacted; headers and bodies are never part of
// an event.
type TraceEvent struct {
	Time        time.Time `json:"time"`
	Node        string    `json:"node"`
	Type        string    `json:"type"`
	API         string    `json:"api,omitempty"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Query       string    `json:"query,omitempty"`
	Status      int       `json:"status"`
	DurationMs  float64   `json:"durationMs"`
	InputBytes  int64     `json:"inputBytes"`
	OutputBytes int64     `json:"outputBytes"`
	RequestID   string    `json:"requestId,omitempty"`
	ClientIP    string    `json:"clientIp,omitempty"`
	User        string    `json:"user,omitempty"`
	Bucket      string    `json:"bucket,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// LogOptions filter the live logs (Logs).
type LogOptions struct {
	Nodes []string
	// Level is the minimum level (debug, info, warn, error).
	Level string
	// Last replays up to that many recent entries of each node first.
	Last int
}

// LogEntry is an entry of the server logs, its secrets redacted.
type LogEntry struct {
	Time    time.Time         `json:"time"`
	Node    string            `json:"node"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// AuditQuery searches the recent audit records (SearchAudit).
type AuditQuery struct {
	Nodes  []string
	API    string
	Bucket string
	// User is an access key or a parent user.
	User string
	// Type is s3 or admin.
	Type string
	// Statuses are status codes ("404") or classes ("5xx").
	Statuses   []string
	ErrorsOnly bool
	Since      time.Time
	Until      time.Time
	RequestID  string
	// Limit bounds the records (0: the server default, 100; at most
	// 1000).
	Limit int
}

// AuditEntry is an audit record.
type AuditEntry struct {
	Time       time.Time         `json:"time"`
	Node       string            `json:"node,omitempty"`
	Type       string            `json:"type,omitempty"`
	RequestID  string            `json:"requestId,omitempty"`
	API        string            `json:"api"`
	Method     string            `json:"method,omitempty"`
	Path       string            `json:"path,omitempty"`
	Bucket     string            `json:"bucket,omitempty"`
	Object     string            `json:"object,omitempty"`
	StatusCode int               `json:"statusCode"`
	DurationMs float64           `json:"durationMs"`
	RX         int64             `json:"rx"`
	TX         int64             `json:"tx"`
	RemoteHost string            `json:"remoteHost,omitempty"`
	UserAgent  string            `json:"userAgent,omitempty"`
	AccessKey  string            `json:"accessKey,omitempty"`
	ParentUser string            `json:"parentUser,omitempty"`
	Query      map[string]string `json:"query,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// AuditList are the records of a search, newest first.
type AuditList struct {
	Items []AuditEntry `json:"items"`
}

// MetricInfo describes a series of the metrics of the console.
type MetricInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Unit        string `json:"unit"`
	// Aggregation is how the nodes combine into the cluster series (sum,
	// avg, max).
	Aggregation string `json:"aggregation"`
}

// MetricList are the series available.
type MetricList struct {
	Items []MetricInfo `json:"items"`
}

// MetricQuery selects the points of a series (QueryMetric).
type MetricQuery struct {
	Metric string
	// Range is the span of the points (0: one hour), ending at End (zero:
	// now).
	Range time.Duration
	End   time.Time
	// Step is the distance between two points (0: the server picks).
	Step  time.Duration
	Nodes []string
}

// MetricPoint is a point: the unix time and the value, nil without sample.
type MetricPoint struct {
	T int64    `json:"t"`
	V *float64 `json:"v"`
}

// MetricNodeSeries are the points of a node.
type MetricNodeSeries struct {
	Node   string        `json:"node"`
	Points []MetricPoint `json:"points"`
	// Error tells why the points of the node could not be read.
	Error string `json:"error,omitempty"`
}

// MetricSeries are the points of a series per node and for the cluster.
type MetricSeries struct {
	Metric      MetricInfo         `json:"metric"`
	Start       time.Time          `json:"start"`
	End         time.Time          `json:"end"`
	StepSeconds int64              `json:"stepSeconds"`
	Nodes       []MetricNodeSeries `json:"nodes"`
	Cluster     []MetricPoint      `json:"cluster"`
}

// Stream is a live stream of the Admin API (Trace, Logs), read with Next
// until io.EOF (the server ended it) or an error; Close ends it. A stream
// is not safe for concurrent calls of Next.
type Stream[T any] struct {
	body    io.ReadCloser
	r       *bufio.Reader
	name    string
	cancel  context.CancelFunc
	dropped atomic.Uint64
	once    sync.Once
}

// maxEventLine bounds a line of a stream.
const maxEventLine = 1 << 20

// Next returns the next item of the stream, waiting for it.
func (s *Stream[T]) Next() (*T, error) {
	var event string
	var data bytes.Buffer
	for {
		line, err := s.r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, fmt.Errorf("nexoadmin: stream line longer than %d bytes", maxEventLine)
			}
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if data.Len() == 0 {
				event = ""
				continue
			}
			switch event {
			case "dropped":
				var d struct {
					Dropped uint64 `json:"dropped"`
				}
				if json.Unmarshal(data.Bytes(), &d) == nil {
					s.dropped.Store(d.Dropped)
				}
			case s.name:
				var v T
				if err := json.Unmarshal(data.Bytes(), &v); err != nil {
					return nil, fmt.Errorf("nexoadmin: decode %s event: %w", s.name, err)
				}
				return &v, nil
			}
			event = ""
			data.Reset()
		case line[0] == ':':
			// A heartbeat comment.
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "event":
				event = string(value)
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(value)
			}
		}
	}
}

// Dropped returns the items the server dropped so far because the client
// did not keep up.
func (s *Stream[T]) Dropped() uint64 { return s.dropped.Load() }

// Close ends the stream.
func (s *Stream[T]) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		err = s.body.Close()
	})
	return err
}

// list appends the values of a list parameter.
func list(q url.Values, name string, values []string) {
	if len(values) > 0 {
		q.Set(name, strings.Join(values, ","))
	}
}

// openStream opens a Server-Sent Events stream of the Admin API: the
// request is signed like the others and lives until ctx is canceled or
// the stream closed (the timeout of the HTTP client does not apply).
func openStream[T any](ctx context.Context, c *client, path string, q url.Values, name string) (*Stream[T], error) {
	u, err := c.endpoint(path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("nexoadmin: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if err := c.signer.sign(ctx, req, nil); err != nil {
		cancel()
		return nil, fmt.Errorf("nexoadmin: sign request: %w", err)
	}
	httpClient := *c.cfg.HTTPClient
	httpClient.Timeout = 0
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("nexoadmin: GET %s: %w", path, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
		_ = resp.Body.Close()
		cancel()
		return nil, newAPIError(resp, data)
	}
	return &Stream[T]{body: resp.Body, r: bufio.NewReaderSize(resp.Body, maxEventLine), name: name, cancel: cancel}, nil
}

// Trace implements AdminClient.
func (c *client) Trace(ctx context.Context, opts TraceOptions) (*Stream[TraceEvent], error) {
	q := url.Values{}
	list(q, "node", opts.Nodes)
	list(q, "type", opts.Types)
	list(q, "api", opts.APIs)
	list(q, "bucket", opts.Buckets)
	list(q, "status", opts.Statuses)
	if opts.ErrorsOnly {
		q.Set("errors", "true")
	}
	if opts.Threshold > 0 {
		q.Set("threshold", opts.Threshold.String())
	}
	return openStream[TraceEvent](ctx, c, "/trace", q, "trace")
}

// Logs implements AdminClient.
func (c *client) Logs(ctx context.Context, opts LogOptions) (*Stream[LogEntry], error) {
	q := url.Values{}
	list(q, "node", opts.Nodes)
	if opts.Level != "" {
		q.Set("level", opts.Level)
	}
	if opts.Last > 0 {
		q.Set("last", strconv.Itoa(opts.Last))
	}
	return openStream[LogEntry](ctx, c, "/logs", q, "log")
}

// SearchAudit implements AdminClient.
func (c *client) SearchAudit(ctx context.Context, in AuditQuery) (*AuditList, error) {
	q := url.Values{}
	list(q, "node", in.Nodes)
	list(q, "status", in.Statuses)
	for name, v := range map[string]string{"api": in.API, "bucket": in.Bucket, "user": in.User, "type": in.Type, "requestId": in.RequestID} {
		if v != "" {
			q.Set(name, v)
		}
	}
	if in.ErrorsOnly {
		q.Set("errors", "true")
	}
	if !in.Since.IsZero() {
		q.Set("since", in.Since.UTC().Format(time.RFC3339))
	}
	if !in.Until.IsZero() {
		q.Set("until", in.Until.UTC().Format(time.RFC3339))
	}
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	var out AuditList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/audit", query: q}, &out)
}

// ListMetrics implements AdminClient.
func (c *client) ListMetrics(ctx context.Context) (*MetricList, error) {
	var out MetricList
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/metrics"}, &out)
}

// QueryMetric implements AdminClient.
func (c *client) QueryMetric(ctx context.Context, in MetricQuery) (*MetricSeries, error) {
	q := url.Values{"metric": {in.Metric}}
	list(q, "node", in.Nodes)
	if in.Range > 0 {
		q.Set("range", in.Range.String())
	}
	if in.Step > 0 {
		q.Set("step", in.Step.String())
	}
	if !in.End.IsZero() {
		q.Set("end", in.End.UTC().Format(time.RFC3339))
	}
	var out MetricSeries
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/metrics/query", query: q}, &out)
}
