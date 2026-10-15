package commands

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The JSON records of the observability commands (--json). The live
// streams wrap their item (a trace event has its own "status" member).

// adminTraceRecord is a request of the live trace (admin trace).
type adminTraceRecord struct {
	Alias string               `json:"alias"`
	Trace nexoadmin.TraceEvent `json:"trace"`
}

// adminLogRecord is an entry of the live logs (admin logs).
type adminLogRecord struct {
	Alias string             `json:"alias"`
	Log   nexoadmin.LogEntry `json:"log"`
}

// adminAuditRecord is an audit record (admin audit).
type adminAuditRecord struct {
	Alias string `json:"alias"`
	nexoadmin.AuditEntry
}

// adminMetricRecord is a series of the metrics (admin metrics list).
type adminMetricRecord struct {
	Alias string `json:"alias"`
	nexoadmin.MetricInfo
}

// adminMetricSeriesRecord are the points of a series (admin metrics query).
type adminMetricSeriesRecord struct {
	Alias string `json:"alias"`
	nexoadmin.MetricSeries
}

// adminPrometheusRecord is a scrape configuration (admin prometheus
// generate).
type adminPrometheusRecord struct {
	Alias string `json:"alias"`
	Type  string `json:"type"`
	cli.PrometheusScrape
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Config is the scrape_configs section of prometheus.yml.
	Config string `json:"config"`
}

// traceTypes are the request types of the live trace.
var traceTypes = []string{"s3", "admin", "web", "rpc"}

// statusFilterRE matches a status code (404) or class (5xx).
var statusFilterRE = regexp.MustCompile(`^[1-5]([0-9]{2}|xx)$`)

// checkStatuses validates and normalizes --status.
func checkStatuses(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if !statusFilterRE.MatchString(v) {
			return nil, cli.UsageError("--status %q must be a status code (404) or class (5xx)", v)
		}
		out = append(out, v)
	}
	return out, nil
}

// streamFlags are the flags shared by the live streams.
type streamFlags struct {
	nodes   []string
	timeout time.Duration
	count   int
}

func (f *streamFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringSliceVar(&f.nodes, "node", nil, "nodes of a cluster (host:port, repeated or comma-separated; default all)")
	fl.DurationVar(&f.timeout, "timeout", 0, "stop after this time (default: until Ctrl-C)")
	fl.IntVar(&f.count, "count", 0, "stop after this many items (default: no limit)")
}

func (f *streamFlags) check() error {
	if f.timeout < 0 || f.count < 0 {
		return cli.UsageError("--timeout and --count must not be negative")
	}
	return nil
}

// follow runs a live stream until Ctrl-C, --timeout or --count, opened
// again when it fails (cli.Follow). what names the stream in the
// warnings.
func follow[T any](ctx context.Context, e *env, f *streamFlags, what string, open func(context.Context) (cli.EventStream[T], error), each func(*T) error) error {
	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}
	return cli.Follow(ctx, open, each, cli.FollowOptions{
		ReconnectDelay: e.deps.ReconnectDelay,
		Max:            f.count,
		OnReconnect: func(err error, delay time.Duration) {
			e.out.Warn(fmt.Sprintf("the %s stream ended (%v): reconnecting in %s", what, err, delay))
		},
		OnDropped: func(n uint64) {
			e.out.Warn(fmt.Sprintf("the server dropped %d %s item(s): the client did not keep up (narrow the filter)", n, what))
		},
	})
}

func newAdminTraceCmd(e *env) *cobra.Command {
	var (
		stream      streamFlags
		types       []string
		apis        []string
		buckets     []string
		statuses    []string
		errorsOnly  bool
		minDuration time.Duration
	)
	cmd := &cobra.Command{
		Use:   "trace [flags] ALIAS",
		Short: "Show the requests served by the server, live",
		Long: `Show the requests served by every node of the deployment as they complete, one line
per request: time, node, type, operation, status, duration, method and path (queries
redacted), bytes received and sent, user and error. Headers and bodies are never part of
the trace. The internode calls (type rpc) are shown only when asked with --type.

The stream lasts until Ctrl-C, --timeout or --count, and reconnects by itself when the
server restarts. With --json each request is a JSON line {"alias":...,"trace":{...}}.

Needs admin:ServerTrace (root, consoleAdmin).`,
		Example: "  nexoctl admin trace local\n  nexoctl admin trace --errors --type s3,admin local\n" +
			"  nexoctl admin trace --api PutObject,GetObject --bucket photos --min-duration 500ms local\n" +
			"  nexoctl admin trace --status 5xx,404 --node n1:9000 --json local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := stream.check(); err != nil {
				return err
			}
			opts := nexoadmin.TraceOptions{Nodes: stream.nodes, APIs: apis, Buckets: buckets, ErrorsOnly: errorsOnly, Threshold: minDuration}
			for _, t := range types {
				t = strings.ToLower(strings.TrimSpace(t))
				if !slices.Contains(traceTypes, t) {
					return cli.UsageError("--type %q must be s3, admin, web or rpc", t)
				}
				opts.Types = append(opts.Types, t)
			}
			var err error
			if opts.Statuses, err = checkStatuses(statuses); err != nil {
				return err
			}
			if minDuration < 0 {
				return cli.UsageError("--min-duration must not be negative")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			open := func(ctx context.Context) (cli.EventStream[nexoadmin.TraceEvent], error) {
				return client.Trace(ctx, opts)
			}
			first := true
			err = follow(ctx, e, &stream, "trace", func(ctx context.Context) (cli.EventStream[nexoadmin.TraceEvent], error) {
				s, err := open(ctx)
				if err == nil && first {
					first = false
					_ = e.out.Note(fmt.Sprintf("Tracing the requests of %q (Ctrl-C to stop).", alias))
				}
				return s, err
			}, func(ev *nexoadmin.TraceEvent) error {
				return e.out.Result(adminTraceRecord{Alias: alias, Trace: *ev}, e.formatTrace(ev))
			})
			if err != nil {
				return fmt.Errorf("trace %s: %w", alias, err)
			}
			return nil
		},
	}
	stream.register(cmd)
	f := cmd.Flags()
	f.StringSliceVar(&types, "type", nil, "request types: s3, admin, web, rpc (default all but rpc)")
	f.StringSliceVar(&apis, "api", nil, `operations (PutObject, "GET /buckets")`)
	f.StringSliceVar(&buckets, "bucket", nil, "buckets")
	f.StringSliceVar(&statuses, "status", nil, "status codes or classes (404, 5xx)")
	f.BoolVarP(&errorsOnly, "errors", "e", false, "the failed requests only (status 400 or more)")
	f.DurationVar(&minDuration, "min-duration", 0, "the requests slower than this only (e.g. 500ms)")
	return cmd
}

// formatTrace renders a request of the trace on one line.
func (e *env) formatTrace(ev *nexoadmin.TraceEvent) string {
	p := e.out
	api := ev.API
	if api == "" {
		api = "-"
	}
	target := ev.Path
	if ev.Query != "" {
		target += "?" + ev.Query
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %-5s %-24s %s %9s  %s %s", traceTime(ev.Time), ev.Node, ev.Type, api,
		p.Paint(statusColor(ev.Status), strconv.Itoa(ev.Status)), formatMs(ev.DurationMs), ev.Method, target)
	if ev.InputBytes > 0 || ev.OutputBytes > 0 {
		fmt.Fprintf(&b, "  rx %s tx %s", cli.HumanSize(ev.InputBytes), cli.HumanSize(ev.OutputBytes))
	}
	if ev.User != "" {
		fmt.Fprintf(&b, "  user %s", ev.User)
	}
	if ev.ClientIP != "" {
		fmt.Fprintf(&b, "  from %s", ev.ClientIP)
	}
	if ev.Error != "" {
		fmt.Fprintf(&b, "  %s", p.Paint(cli.ColorRed, "error: "+ev.Error))
	}
	return b.String()
}

// traceTime renders the time of a stream item, to the millisecond, UTC.
func traceTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000") }

// statusColor is the color of an HTTP status.
func statusColor(status int) cli.Color {
	switch {
	case status >= 500:
		return cli.ColorRed
	case status >= 400:
		return cli.ColorYellow
	case status >= 300:
		return cli.ColorCyan
	default:
		return cli.ColorGreen
	}
}

// formatMs renders a duration in milliseconds: 0.42ms, 12.5ms, 1.50s.
func formatMs(ms float64) string {
	switch {
	case ms < 1:
		return fmt.Sprintf("%.2fms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.1fms", ms)
	default:
		return fmt.Sprintf("%.2fs", ms/1000)
	}
}

// logLevels are the levels of the server logs, lowest first.
var logLevels = []string{"debug", "info", "warn", "error"}

func newAdminLogsCmd(e *env) *cobra.Command {
	var (
		stream streamFlags
		level  string
		last   int
	)
	cmd := &cobra.Command{
		Use:   "logs [flags] ALIAS",
		Short: "Show the logs of the server, live",
		Long: `Show the logs of every node of the deployment as they are written, secrets redacted:
the --last recent entries of each node first, then the new ones, at --level or above.

The stream lasts until Ctrl-C, --timeout or --count, and reconnects by itself when the
server restarts. With --json each entry is a JSON line {"alias":...,"log":{...}}.

Needs admin:ConsoleLog (root, consoleAdmin).`,
		Example:           "  nexoctl admin logs local\n  nexoctl admin logs --level warn --last 100 local\n  nexoctl admin logs --node n2:9000 --json local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := stream.check(); err != nil {
				return err
			}
			level = strings.ToLower(strings.TrimSpace(level))
			if level != "" && !slices.Contains(logLevels, level) {
				return cli.UsageError("--level %q must be debug, info, warn or error", level)
			}
			if last < 0 || last > 10000 {
				return cli.UsageError("--last must be between 0 and 10000")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			// The replay is asked once: a reconnection shows the new entries
			// only.
			opts := nexoadmin.LogOptions{Nodes: stream.nodes, Level: level, Last: last}
			first := true
			err = follow(ctx, e, &stream, "log", func(ctx context.Context) (cli.EventStream[nexoadmin.LogEntry], error) {
				s, err := client.Logs(ctx, opts)
				if err == nil && first {
					first = false
					opts.Last = 0
					_ = e.out.Note(fmt.Sprintf("Following the logs of %q (Ctrl-C to stop).", alias))
				}
				return s, err
			}, func(entry *nexoadmin.LogEntry) error {
				return e.out.Result(adminLogRecord{Alias: alias, Log: *entry}, e.formatLog(entry))
			})
			if err != nil {
				return fmt.Errorf("logs %s: %w", alias, err)
			}
			return nil
		},
	}
	stream.register(cmd)
	f := cmd.Flags()
	f.StringVar(&level, "level", "", "minimum level: debug, info, warn or error (default: the level of the server)")
	f.IntVar(&last, "last", 10, "recent entries of each node shown first (0 to 10000)")
	return cmd
}

// formatLog renders an entry of the logs on one line, its attributes
// sorted.
func (e *env) formatLog(entry *nexoadmin.LogEntry) string {
	level := strings.ToUpper(entry.Level)
	color := cli.ColorBlue
	switch strings.ToLower(entry.Level) {
	case "warn", "warning":
		color = cli.ColorYellow
	case "error":
		color = cli.ColorRed
	case "debug":
		color = cli.ColorCyan
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s %s", traceTime(entry.Time), entry.Node, e.out.Paint(color, fmt.Sprintf("%-5s", level)), entry.Message)
	for _, k := range cli.SortedKeys(entry.Attrs) {
		v := entry.Attrs[k]
		if strings.ContainsAny(v, " \t\"=") || v == "" {
			v = strconv.Quote(v)
		}
		fmt.Fprintf(&b, " %s=%s", k, v)
	}
	return b.String()
}

func newAdminAuditCmd(e *env) *cobra.Command {
	var (
		user, api, bucket, kind, requestID string
		nodes, statuses                    []string
		errorsOnly                         bool
		since, until                       string
		limit                              int
	)
	cmd := &cobra.Command{
		Use:   "audit [flags] ALIAS",
		Short: "Search the audit log of the server",
		Long: `Search the recent audit records kept in memory by every node of the deployment
(the external audit targets keep the complete log: see "nexoctl admin config set
ALIAS audit_webhook ..."), newest first. One line per record: time, status, duration,
user, operation, bucket/object and error.

--since and --until take a duration before now (15m, 2h, 7d) or a date (RFC 3339).

Needs admin:ServerTrace (root, consoleAdmin).`,
		Example: "  nexoctl admin audit local\n  nexoctl admin audit --user alice --since 1h local\n" +
			"  nexoctl admin audit --action DeleteObject --bucket photos --since 2026-10-15T09:00:00Z local\n" +
			"  nexoctl admin audit --errors --status 403 --limit 1000 --json local",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := nexoadmin.AuditQuery{Nodes: nodes, API: strings.TrimSpace(api), Bucket: bucket, User: strings.TrimSpace(user),
				ErrorsOnly: errorsOnly, RequestID: requestID, Limit: limit}
			if kind != "" {
				q.Type = strings.ToLower(kind)
				if q.Type != "s3" && q.Type != "admin" {
					return cli.UsageError("--type %q must be s3 or admin", kind)
				}
			}
			var err error
			if q.Statuses, err = checkStatuses(statuses); err != nil {
				return err
			}
			if q.Since, err = e.parseTimeFlag("--since", since); err != nil {
				return err
			}
			if q.Until, err = e.parseTimeFlag("--until", until); err != nil {
				return err
			}
			if !q.Since.IsZero() && !q.Until.IsZero() && q.Until.Before(q.Since) {
				return cli.UsageError("--until must not be before --since")
			}
			if limit < 0 || limit > 1000 {
				return cli.UsageError("--limit must be between 0 and 1000")
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			list, err := client.SearchAudit(ctx, q)
			if err != nil {
				return fmt.Errorf("audit %s: %w", alias, err)
			}
			if len(list.Items) == 0 {
				return e.out.Note("No audit record matches.")
			}
			if err := e.out.Text(fmt.Sprintf("%-23s %-6s %9s  %-20s %-24s %s", "TIME", "STATUS", "DURATION", "USER", "API", "RESOURCE")); err != nil {
				return err
			}
			for i := range list.Items {
				if err := e.out.Result(adminAuditRecord{Alias: alias, AuditEntry: list.Items[i]}, e.formatAudit(&list.Items[i])); err != nil {
					return err
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&user, "user", "", "access key or parent user")
	f.StringVar(&api, "action", "", "operation (PutObject, DeleteObject, ...)")
	f.StringVar(&api, "api", "", "same as --action")
	_ = f.MarkHidden("api")
	f.StringVar(&bucket, "bucket", "", "bucket")
	f.StringVar(&kind, "type", "", "request type: s3 or admin (default both)")
	f.StringSliceVar(&statuses, "status", nil, "status codes or classes (404, 5xx)")
	f.BoolVarP(&errorsOnly, "errors", "e", false, "the failed requests only (status 400 or more)")
	f.StringVar(&since, "since", "", "records after this time (a duration before now, or RFC 3339)")
	f.StringVar(&until, "until", "", "records before this time (a duration before now, or RFC 3339)")
	f.StringSliceVar(&nodes, "node", nil, "nodes of a cluster (host:port; default all)")
	f.StringVar(&requestID, "request-id", "", "the records of a request ID")
	f.IntVar(&limit, "limit", 0, "maximum number of records (default 100, at most 1000)")
	return cmd
}

// formatAudit renders an audit record on one line.
func (e *env) formatAudit(a *nexoadmin.AuditEntry) string {
	user := a.AccessKey
	if a.ParentUser != "" && a.ParentUser != a.AccessKey {
		user += " (" + a.ParentUser + ")"
	}
	resource := strings.TrimSuffix(a.Bucket+"/"+a.Object, "/")
	if resource == "" {
		resource = a.Path
	}
	line := fmt.Sprintf("%-23s %s %9s  %-20s %-24s %s", traceTime(a.Time),
		e.out.Paint(statusColor(a.StatusCode), fmt.Sprintf("%-6d", a.StatusCode)), formatMs(a.DurationMs), orDash(user), orDash(a.API), orDash(resource))
	if a.Error != "" {
		line += "  " + e.out.Paint(cli.ColorRed, "error: "+a.Error)
	}
	return line
}

// parseTimeFlag parses a time flag: a duration before now (15m, 7d) or a
// date (RFC 3339, or 2006-01-02 at midnight UTC); "" is the zero time.
func (e *env) parseTimeFlag(flag, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := cli.ParseDuration(s); err == nil {
		return e.deps.Clock.Now().UTC().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, cli.UsageError("invalid %s %q: use a duration before now (15m, 2h, 7d) or a date (RFC 3339)", flag, s)
}

func newAdminMetricsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Query the recent metrics kept by the server",
		Long: `Query the series of metrics the nodes sample every 10 seconds and keep for 24 hours (the
charts of the console): requests, errors, latencies, traffic, drives, healing, scanner,
internode calls, CPU and memory, per node and for the cluster. Prometheus scrapes the
complete metrics: see "nexoctl admin prometheus generate".

Needs admin:Prometheus (root, consoleAdmin, the diagnostics policy).`,
	}
	cmd.AddCommand(newAdminMetricsListCmd(e), newAdminMetricsQueryCmd(e))
	return cmd
}

func newAdminMetricsListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the series of metrics",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			list, err := client.ListMetrics(ctx)
			if err != nil {
				return fmt.Errorf("metrics %s: %w", alias, err)
			}
			if len(list.Items) > 0 {
				if err := e.out.Text(fmt.Sprintf("%-28s %-12s %-5s %s", "NAME", "UNIT", "AGG", "DESCRIPTION")); err != nil {
					return err
				}
			}
			for _, m := range list.Items {
				text := fmt.Sprintf("%s %-12s %-5s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-28s", m.Name)), m.Unit, m.Aggregation, m.Description)
				if err := e.out.Result(adminMetricRecord{Alias: alias, MetricInfo: m}, text); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminMetricsQueryCmd(e *env) *cobra.Command {
	var (
		span, step time.Duration
		end        string
		nodes      []string
	)
	cmd := &cobra.Command{
		Use:   "query [flags] ALIAS METRIC",
		Short: "Show the points of a series of metrics",
		Long: `Show the points of a series (see "nexoctl admin metrics list") over --range, ending
now or at --end: one row per point, the cluster value then the value of each node (a
dash where a node has no sample).`,
		Example: "  nexoctl admin metrics query local s3_requests\n  nexoctl admin metrics query --range 6h --step 5m local s3_latency_p99\n" +
			"  nexoctl admin metrics query --node n1:9000 --json local drives_online",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			if span < 0 || step < 0 {
				return cli.UsageError("--range and --step must not be negative")
			}
			q := nexoadmin.MetricQuery{Metric: args[1], Range: span, Step: step, Nodes: nodes}
			if end != "" {
				t, err := e.parseTimeFlag("--end", end)
				if err != nil {
					return err
				}
				q.End = t
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			series, err := client.QueryMetric(ctx, q)
			if err != nil {
				return fmt.Errorf("metrics %s: %w", alias, err)
			}
			return e.out.Result(adminMetricSeriesRecord{Alias: alias, MetricSeries: *series}, formatSeries(series))
		},
	}
	f := cmd.Flags()
	f.DurationVar(&span, "range", 0, "span of the points (default 1h, at most 24h)")
	f.DurationVar(&step, "step", 0, "distance between two points (default: chosen by the server, at least 10s)")
	f.StringVar(&end, "end", "", "end of the range: a duration before now (30m) or a date (RFC 3339; default now)")
	f.StringSliceVar(&nodes, "node", nil, "nodes of a cluster (host:port; default all)")
	return cmd
}

// formatSeries renders the points of a series as a table.
func formatSeries(s *nexoadmin.MetricSeries) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (%s, %s of the nodes)\n", s.Metric.Name, s.Metric.Description, orDash(s.Metric.Unit), orDash(s.Metric.Aggregation))
	fmt.Fprintf(&b, "From %s to %s, every %s", cli.FormatTime(s.Start), cli.FormatTime(s.End), time.Duration(s.StepSeconds)*time.Second)
	for _, n := range s.Nodes {
		if n.Error != "" {
			fmt.Fprintf(&b, "\n%s: %s", n.Node, n.Error)
		}
	}
	header := []string{"CLUSTER"}
	for _, n := range s.Nodes {
		header = append(header, n.Node)
	}
	fmt.Fprintf(&b, "\n%-23s", "TIME")
	for _, h := range header {
		fmt.Fprintf(&b, " %14s", h)
	}
	for i, p := range s.Cluster {
		fmt.Fprintf(&b, "\n%-23s %14s", cli.FormatTime(time.Unix(p.T, 0)), metricValue(p.V, s.Metric.Unit))
		for _, n := range s.Nodes {
			var v *float64
			if i < len(n.Points) {
				v = n.Points[i].V
			}
			fmt.Fprintf(&b, " %14s", metricValue(v, s.Metric.Unit))
		}
	}
	return b.String()
}

// metricValue renders a value of a series in its unit ("-" without
// sample).
func metricValue(v *float64, unit string) string {
	if v == nil || math.IsNaN(*v) {
		return "-"
	}
	x := *v
	switch unit {
	case "bytes":
		return cli.HumanSize(int64(math.Round(x)))
	case "bytes/s":
		return cli.HumanSize(int64(math.Round(x))) + "/s"
	case "seconds":
		return formatMs(x * 1000)
	case "percent", "%":
		return strconv.FormatFloat(x, 'f', 1, 64) + "%"
	}
	if x == math.Trunc(x) && math.Abs(x) < 1e15 {
		return strconv.FormatFloat(x, 'f', 0, 64)
	}
	return strconv.FormatFloat(x, 'g', 4, 64)
}

func newAdminPrometheusCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prometheus",
		Short: "Integrate the metrics of the server with Prometheus",
	}
	cmd.AddCommand(newAdminPrometheusGenerateCmd(e))
	return cmd
}

func newAdminPrometheusGenerateCmd(e *env) *cobra.Command {
	var (
		expiry string
		job    string
	)
	cmd := &cobra.Command{
		Use:   "generate [flags] ALIAS [cluster|node|bucket]",
		Short: "Generate the Prometheus scrape configuration of the server",
		Long: `Print the scrape_configs section of prometheus.yml that scrapes the metrics of the
server of the alias (default: cluster) on its S3 endpoint, under /nexo/metrics/v1/:

  cluster  the metrics of every node and of the deployment (one scrape for the cluster)
  node     the metrics of the node that answers (scrape each node)
  bucket   the metrics of the buckets (every minute)

With NEXO_PROMETHEUS_AUTH_TYPE=jwt (the default) the scrapes carry a bearer token: a
JWT (HS512, issuer "prometheus") signed locally with the secret key of the alias,
whose subject is its access key, valid until --expiry. The access key needs
admin:Prometheus (root, consoleAdmin, the diagnostics policy): prefer a dedicated
access key with the diagnostics policy. The token stops working when it expires or
when the secret key changes; keep the configuration as secret as the key.`,
		Example: "  nexoctl admin prometheus generate local\n  nexoctl admin prometheus generate --expiry 30d local node\n" +
			"  nexoctl admin prometheus generate local bucket >> prometheus.yml",
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := cli.PrometheusCluster
			if len(args) == 2 {
				kind = strings.ToLower(args[1])
			}
			switch kind {
			case cli.PrometheusCluster, cli.PrometheusNode, cli.PrometheusBucket:
			default:
				return cli.UsageError("metrics %q must be cluster, node or bucket", args[1])
			}
			exp, err := e.parseExpiration(expiry)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			name, rest, _ := strings.Cut(args[0], "/")
			if name == "" || strings.Trim(rest, "/") != "" {
				return cli.UsageError("%s: expected an alias name (ALIAS), not a bucket or an object", args[0])
			}
			aliases, err := e.loadAliases(ctx)
			if err != nil {
				return err
			}
			alias, ok := aliases[name]
			if !ok {
				return cli.NotFoundError("alias %q not found (see nexoctl alias list)", name)
			}
			token, err := cli.SignPrometheusToken(alias.AccessKey, alias.SecretKey, e.deps.Clock.Now(), exp)
			if err != nil {
				return err
			}
			scrape, err := cli.NewPrometheusScrape(alias.URL, kind, job, token)
			if err != nil {
				return err
			}
			config := fmt.Sprintf("# Nexo %s metrics of nexoctl alias %q (access key %s), token valid until %s\n%s",
				kind, name, alias.AccessKey, exp.UTC().Format(time.RFC3339), scrape.YAML())
			return e.out.Result(adminPrometheusRecord{Alias: name, Type: kind, PrometheusScrape: scrape, Token: token, ExpiresAt: exp.UTC(), Config: config},
				strings.TrimSuffix(config, "\n"))
		},
	}
	f := cmd.Flags()
	f.StringVar(&expiry, "expiry", "365d", "validity of the token: a duration (30d, 12h) or a date (2027-12-31, RFC 3339)")
	f.StringVar(&job, "job", "", "name of the scrape job (default nexo-<type>)")
	return cmd
}
