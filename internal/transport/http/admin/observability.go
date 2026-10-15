package admin

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/logs"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/trace"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ObservabilityRoutes exposes the observability of the deployment
// (docs/05 T-8.2..T-8.4, D-MON-01..03): the live trace of the requests and
// the live logs as Server-Sent Events (GET /trace, admin:ServerTrace; GET
// /logs, admin:ConsoleLog), the search of the audit log (GET /audit,
// admin:ConsoleLog) and the series of the metrics of the console (GET
// /metrics, GET /metrics/query: admin:Prometheus or admin:ServerInfo),
// aggregated over the nodes of a cluster.
type ObservabilityRoutes interface{ Routes }

// ObservabilityDeps are the dependencies of the observability routes; a
// nil service answers 501 not_implemented on its routes.
type ObservabilityDeps struct {
	Trace   trace.Service
	Logs    logs.Service
	Audit   audit.Service
	History metrics.History
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// ObservabilityOption configures the observability routes.
type ObservabilityOption func(*observabilityRoutes)

// WithHeartbeat sets the interval of the heartbeats of the streams
// (default 15s): a comment line that keeps the proxies and the browsers
// from closing an idle stream.
func WithHeartbeat(d time.Duration) ObservabilityOption {
	return func(h *observabilityRoutes) {
		if d > 0 {
			h.heartbeat = d
		}
	}
}

type observabilityRoutes struct {
	deps      ObservabilityDeps
	guard     guard
	heartbeat time.Duration
}

var _ ObservabilityRoutes = (*observabilityRoutes)(nil)

// NewObservabilityRoutes returns the observability routes.
func NewObservabilityRoutes(deps ObservabilityDeps, opts ...ObservabilityOption) (ObservabilityRoutes, error) {
	if deps.Authz == nil {
		return nil, fmt.Errorf("%w: observability routes require the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &observabilityRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}, heartbeat: 15 * time.Second}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *observabilityRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *observabilityRoutes) routes() []route {
	ni := notImplemented(h.deps.Enc)
	traceH, logsH, auditH, listH, queryH := h.trace, h.logs, h.audit, h.metricList, h.metricQuery
	if h.deps.Trace == nil {
		traceH = ni
	}
	if h.deps.Logs == nil {
		logsH = ni
	}
	if h.deps.Audit == nil {
		auditH = ni
	}
	if h.deps.History == nil {
		listH, queryH = ni, ni
	}
	metricsReq := anyOf(policy.ActionAdminPrometheus, policy.ActionAdminServerInfo)
	return []route{
		{http.MethodGet, "/trace", global(policy.ActionAdminServerTrace), traceH},
		{http.MethodGet, "/logs", global(policy.ActionAdminConsoleLog), logsH},
		{http.MethodGet, "/audit", global(policy.ActionAdminConsoleLog), auditH},
		{http.MethodGet, "/metrics", metricsReq, listH},
		{http.MethodGet, "/metrics/query", metricsReq, queryH},
	}
}

// TraceEventResponse is an event of GET /trace (SSE event "trace").
type TraceEventResponse struct {
	Time time.Time `json:"time"`
	Node string    `json:"node"`
	// Type is s3, admin, web or rpc.
	Type   string `json:"type"`
	API    string `json:"api,omitempty"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Query is the query string, its signatures, credentials and tokens
	// redacted.
	Query       string  `json:"query,omitempty"`
	Status      int     `json:"status"`
	DurationMs  float64 `json:"durationMs"`
	InputBytes  int64   `json:"inputBytes"`
	OutputBytes int64   `json:"outputBytes"`
	RequestID   string  `json:"requestId,omitempty"`
	ClientIP    string  `json:"clientIp,omitempty"`
	User        string  `json:"user,omitempty"`
	Bucket      string  `json:"bucket,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// LogEntryResponse is an entry of GET /logs (SSE event "log").
type LogEntryResponse struct {
	Time    time.Time         `json:"time"`
	Node    string            `json:"node"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// StreamDroppedResponse is the SSE event "dropped": the events or
// entries the stream lost so far because the client did not keep up.
type StreamDroppedResponse struct {
	Dropped uint64 `json:"dropped"`
}

// AuditEntryResponse is an entry of GET /audit.
type AuditEntryResponse struct {
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

// AuditListResponse is the body of GET /audit.
type AuditListResponse struct {
	Items []AuditEntryResponse `json:"items"`
}

// MetricInfoResponse describes a series of the metrics of the console.
type MetricInfoResponse struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Unit        string `json:"unit"`
	// Aggregation is how the nodes combine into the cluster series (sum,
	// avg, max).
	Aggregation string `json:"aggregation"`
}

// MetricListResponse is the body of GET /metrics.
type MetricListResponse struct {
	Items []MetricInfoResponse `json:"items"`
}

// MetricPointResponse is a point of a series: the unix time and the
// value, null where there is none.
type MetricPointResponse struct {
	T int64    `json:"t"`
	V *float64 `json:"v"`
}

// MetricNodeSeriesResponse are the points of one node.
type MetricNodeSeriesResponse struct {
	Node   string                `json:"node"`
	Points []MetricPointResponse `json:"points"`
	// Error tells why the points of the node could not be read.
	Error string `json:"error,omitempty"`
}

// MetricSeriesResponse is the body of GET /metrics/query.
type MetricSeriesResponse struct {
	Metric      MetricInfoResponse         `json:"metric"`
	Start       time.Time                  `json:"start"`
	End         time.Time                  `json:"end"`
	StepSeconds int64                      `json:"stepSeconds"`
	Nodes       []MetricNodeSeriesResponse `json:"nodes"`
	Cluster     []MetricPointResponse      `json:"cluster"`
}

func (h *observabilityRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// queryList returns the values of a query parameter, repeated or comma
// separated.
func queryList(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// statusFilter parses a list of statuses: codes (404) and classes (4xx).
func statusFilter(values []string) (codes, classes []int, err error) {
	for _, v := range values {
		if c, ok := strings.CutSuffix(strings.ToLower(v), "xx"); ok && len(c) == 1 && c[0] >= '1' && c[0] <= '5' {
			classes = append(classes, int(c[0]-'0'))
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 100 || n > 599 {
			return nil, nil, fmt.Errorf("%w: status %q is neither a status code nor a class (4xx)", domain.ErrInvalidArgument, v)
		}
		codes = append(codes, n)
	}
	return codes, classes, nil
}

// queryDuration parses an optional duration query parameter.
func queryDuration(r *http.Request, name string) (time.Duration, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%w: %s must be a duration (500ms, 1h)", domain.ErrInvalidArgument, name)
	}
	return d, nil
}

// queryTime parses an optional RFC 3339 query parameter.
func queryTime(r *http.Request, name string) (time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be an RFC 3339 time", domain.ErrInvalidArgument, name)
	}
	return t, nil
}

// traceFilter decodes the filter of GET /trace.
func traceFilter(r *http.Request) (trace.Filter, error) {
	f := trace.Filter{Nodes: queryList(r, "node"), APIs: queryList(r, "api"), Buckets: queryList(r, "bucket")}
	for _, t := range queryList(r, "type") {
		f.Types = append(f.Types, trace.Type(strings.ToLower(t)))
	}
	var err error
	if f.StatusCodes, f.StatusClasses, err = statusFilter(queryList(r, "status")); err != nil {
		return f, err
	}
	if f.ErrorsOnly, err = queryBool(r, "errors"); err != nil {
		return f, err
	}
	f.MinDuration, err = queryDuration(r, "threshold")
	return f, err
}

// trace streams the live trace (SSE): event "trace" per request, "dropped"
// when the stream lost events, a heartbeat comment while idle.
func (h *observabilityRoutes) trace(w http.ResponseWriter, r *http.Request) {
	f, err := traceFilter(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sub, err := h.deps.Trace.Subscribe(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	streamSSE(w, r, h.heartbeat, sub.Events(), sub.Dropped, "trace", func(e trace.Event) any {
		return TraceEventResponse{
			Time: e.Time.UTC(), Node: e.Node, Type: string(e.Type), API: e.API, Method: e.Method, Path: e.Path, Query: e.Query,
			Status: e.Status, DurationMs: milliseconds(e.Duration), InputBytes: e.InputBytes, OutputBytes: e.OutputBytes,
			RequestID: e.RequestID, ClientIP: e.ClientIP, User: e.User, Bucket: e.Bucket, Error: e.Error,
		}
	})
}

// logs streams the live logs (SSE): event "log" per entry.
func (h *observabilityRoutes) logs(w http.ResponseWriter, r *http.Request) {
	last, err := queryInt(r, "last")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	sub, err := h.deps.Logs.Subscribe(r.Context(), logs.Filter{Nodes: queryList(r, "node"), Level: r.URL.Query().Get("level"), Last: last})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	streamSSE(w, r, h.heartbeat, sub.Entries(), sub.Dropped, "log", func(e logs.Entry) any {
		return LogEntryResponse{Time: e.Time.UTC(), Node: e.Node, Level: e.Level, Message: e.Message, Attrs: e.Attrs}
	})
}

// streamSSE writes the items of ch as Server-Sent Events of type name
// until the client goes away or ch is closed. The writes are flushed when
// no other item waits; dropped is reported when it grows.
func streamSSE[T any](w http.ResponseWriter, r *http.Request, heartbeat time.Duration, ch <-chan T, dropped func() uint64, name string, encode func(T) any) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	flush := func() bool { return rc.Flush() == nil }
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil || !flush() {
		return
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	var reported uint64
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || !flush() {
				return
			}
		case item, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(encode(item))
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
				return
			}
			if d := dropped(); d > reported {
				reported = d
				dd, _ := json.Marshal(StreamDroppedResponse{Dropped: d})
				if _, err := fmt.Fprintf(w, "event: dropped\ndata: %s\n\n", dd); err != nil {
					return
				}
			}
			if len(ch) == 0 && !flush() {
				return
			}
		}
	}
}

func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// audit searches the audit log.
func (h *observabilityRoutes) audit(w http.ResponseWriter, r *http.Request) {
	q := audit.Query{
		Nodes: queryList(r, "node"), API: r.URL.Query().Get("api"), Bucket: r.URL.Query().Get("bucket"),
		User: r.URL.Query().Get("user"), Type: r.URL.Query().Get("type"), RequestID: r.URL.Query().Get("requestId"),
	}
	var err error
	if q.StatusCodes, q.StatusClasses, err = statusFilter(queryList(r, "status")); err == nil {
		q.ErrorsOnly, err = queryBool(r, "errors")
	}
	if err == nil {
		q.Since, err = queryTime(r, "since")
	}
	if err == nil {
		q.Until, err = queryTime(r, "until")
	}
	if err == nil {
		q.Limit, err = queryInt(r, "limit")
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	entries, err := h.deps.Audit.Search(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := AuditListResponse{Items: make([]AuditEntryResponse, 0, len(entries))}
	for _, e := range entries {
		out.Items = append(out.Items, AuditEntryResponse{
			Time: e.Time.UTC(), Node: e.Node, Type: e.Type, RequestID: e.RequestID, API: e.API.Name, Method: e.API.Method,
			Path: e.API.Path, Bucket: e.API.Bucket, Object: e.API.Object, StatusCode: e.API.StatusCode,
			DurationMs: milliseconds(e.API.TimeToResponse), RX: e.API.InputBytes, TX: e.API.OutputBytes,
			RemoteHost: e.RemoteHost, UserAgent: e.UserAgent, AccessKey: e.AccessKey, ParentUser: e.ParentUser,
			Query: e.ReqQuery, Error: e.Error,
		})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func newMetricInfo(s metrics.SeriesInfo) MetricInfoResponse {
	return MetricInfoResponse{Name: s.Name, Description: s.Description, Unit: s.Unit, Aggregation: s.Aggregation}
}

// metricList lists the series of the console.
func (h *observabilityRoutes) metricList(w http.ResponseWriter, r *http.Request) {
	series := h.deps.History.Metrics(r.Context())
	out := MetricListResponse{Items: make([]MetricInfoResponse, 0, len(series))}
	for _, s := range series {
		out.Items = append(out.Items, newMetricInfo(s))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// metricQuery returns the points of a series: ?metric=&range=&step=&end=
// (range default 1h, at most the retention; step default the resolution
// or what keeps 360 points).
func (h *observabilityRoutes) metricQuery(w http.ResponseWriter, r *http.Request) {
	q := metrics.HistoryQuery{Metric: r.URL.Query().Get("metric"), Nodes: queryList(r, "node")}
	if q.Metric == "" {
		h.fail(w, r, fmt.Errorf("%w: the metric query parameter is required", domain.ErrInvalidArgument))
		return
	}
	var err error
	q.Range, err = queryDuration(r, "range")
	if err == nil {
		q.Step, err = queryDuration(r, "step")
	}
	if err == nil {
		q.End, err = queryTime(r, "end")
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.History.Query(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	step := res.Step
	points := func(values []float64) []MetricPointResponse {
		out := make([]MetricPointResponse, len(values))
		for i, v := range values {
			out[i] = MetricPointResponse{T: res.Start.Add(time.Duration(i) * step).Unix()}
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				val := v
				out[i].V = &val
			}
		}
		return out
	}
	out := MetricSeriesResponse{
		Metric: newMetricInfo(res.Metric), Start: res.Start.UTC(), End: res.End.UTC(), StepSeconds: int64(step / time.Second),
		Nodes: make([]MetricNodeSeriesResponse, 0, len(res.Nodes)), Cluster: points(res.Cluster),
	}
	for _, n := range res.Nodes {
		out.Nodes = append(out.Nodes, MetricNodeSeriesResponse{Node: n.Node, Points: points(n.Values), Error: n.Error})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}
