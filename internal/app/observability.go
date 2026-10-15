package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/logs"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/service/trace"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
	"github.com/r3n3r0/nexo/internal/version"
)

// observability are the observability services of a node (docs/05 M8,
// docs/07 §7.4): the log stream that tees the process logger, the
// distributed tracing (OpenTelemetry), the live trace of the requests, the
// ring buffer of the metrics, the exporter of the families of this node
// (served to the peers), the audit log (composed with the services, behind
// a holder for the internode routes) and the observability side of the
// peers of a cluster.
type observability struct {
	nodeID    string
	logs      logs.Service
	telemetry trace.Telemetry
	trace     trace.Service
	history   metrics.History
	local     metrics.Exporter
	audit     *auditHolder
	peers     []rpc.ObservabilityPeer
	// dataLocks is the locker of the object layer, measured (the leader
	// election keeps the bare one: its waits are not latencies).
	dataLocks lock.Locker
}

// WithLogOutput makes the server own its log output: the records are
// written to w in the format and at the level of the configuration, both
// changed at runtime by the "logger" subsystem (without it the handler of
// WithLogger is used as is and only the level changes).
func WithLogOutput(w io.Writer) Option {
	return func(o *options) { o.logOutput = w }
}

// nodeIDOf returns the id of this node: "host:port" in a cluster, "local"
// otherwise.
func nodeIDOf(layout *topology.Layout) string {
	if layout.Mode == domain.ModeDistributed {
		if n, ok := cluster.LocalNode(cluster.NodesOf(layout)); ok {
			return n.ID
		}
	}
	return cluster.LocalNodeID
}

// newObservability builds the log stream (the returned logger writes
// through it) and the distributed tracing of the configuration; the other
// services are built with the foundation (openObservability).
func newObservability(ctx context.Context, cfg config.Static, layout *topology.Layout, o options) (*observability, *slog.Logger, error) {
	obs := &observability{nodeID: nodeIDOf(layout)}
	deps := logs.Deps{NodeID: obs.nodeID, Output: o.logOutput}
	if o.logOutput == nil {
		deps.Base = o.logger.Handler()
	}
	var err error
	if obs.logs, err = logs.New(deps, logs.WithLevel(cfg.LogLevel), logs.WithFormat(cfg.LogFormat)); err != nil {
		return nil, nil, err
	}
	logger := slog.New(obs.logs.Handler())
	build := version.Get()
	obs.telemetry, err = trace.NewTelemetry(ctx, trace.TelemetryDeps{Logger: logger.With("component", "otel")},
		trace.WithOTLP(cfg.OTelEndpoint, cfg.OTelProtocol), trace.WithSampleRatio(cfg.OTelSampleRatio),
		trace.WithServiceInfo("nexo", build.Version, obs.nodeID))
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: %w", err)
	}
	return obs, logger, nil
}

// measureDrive returns a local drive measured by the registry.
func (f *foundation) measureDrive(drive storage.StorageAPI) (storage.StorageAPI, error) {
	return metrics.WrapDrive(context.Background(), drive, f.registry)
}

// rpcTransport decorates the round tripper of the connection to a peer:
// the internode calls are counted and timed, and carry the trace context
// of the request that makes them.
func (f *foundation) rpcTransport(peer string) func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		rt, err := metrics.NewRoundTripper(context.Background(), next, f.registry, peer, metrics.WithPathPrefix(rpc.Prefix+"/"))
		if err != nil {
			rt = next
		}
		if f.obs == nil {
			return rt
		}
		return trace.NewRoundTripper(rt, f.obs.telemetry, peer, rpc.Prefix+"/")
	}
}

// addObservabilityPeer adds the observability side of the node of conn.
func (f *foundation) addObservabilityPeer(conn rpc.Conn) error {
	p, err := rpc.NewObservabilityClient(rpc.ObservabilityClientDeps{Conn: conn})
	if err != nil {
		return err
	}
	f.obs.peers = append(f.obs.peers, p)
	return nil
}

// openObservability builds the live trace, the ring buffer of the
// metrics, the exporter of the families of this node and the measured
// locker over the peers known so far, and returns the internode routes
// that serve them to the peers.
func (f *foundation) openObservability(logger *slog.Logger) (rpc.ObservabilityRoutes, error) {
	obs := f.obs
	ctx := context.Background()
	var err error
	tracePeers := make([]trace.Peer, 0, len(obs.peers))
	metricPeers := make([]metrics.Peer, 0, len(obs.peers))
	for _, p := range obs.peers {
		tracePeers = append(tracePeers, p)
		metricPeers = append(metricPeers, p)
	}
	if obs.trace, err = trace.New(trace.Deps{NodeID: obs.nodeID, Peers: tracePeers, Registry: f.registry, Logger: logger.With("component", "trace")}); err != nil {
		return nil, err
	}
	if obs.history, err = metrics.NewHistory(metrics.HistoryDeps{Registry: f.registry, NodeID: obs.nodeID, Peers: metricPeers,
		Logger: logger.With("component", "metrics")}); err != nil {
		return nil, err
	}
	if obs.local, err = metrics.NewExporter(metrics.ExporterDeps{Registry: f.registry, NodeID: obs.nodeID, Logger: logger}); err != nil {
		return nil, err
	}
	obs.audit = &auditHolder{}
	if obs.dataLocks, err = metrics.WrapLocker(ctx, f.locks, f.registry); err != nil {
		return nil, err
	}
	return rpc.NewObservabilityRoutes(rpc.ObservabilityRoutesDeps{
		Metrics: obs.local, History: obs.history, Trace: obs.trace, Logs: obs.logs, Audit: obs.audit,
	})
}

// objectLocks returns the locker of the object layer (measured).
func (f *foundation) objectLocks() lock.Locker {
	if f.obs != nil && f.obs.dataLocks != nil {
		return f.obs.dataLocks
	}
	return f.locks
}

// rpcObservers are the middlewares of the internode calls served by a
// cluster node: the live trace (type rpc, on request) and the spans of the
// distributed tracing, children of the spans of the callers.
func (f *foundation) rpcObservers(isRPC func(r *http.Request) bool) ([]middleware.Middleware, error) {
	method := func(r *http.Request) string { return strings.TrimPrefix(r.URL.Path, rpc.Prefix+"/") }
	tracing, err := middleware.NewTracing(middleware.TracingDeps{Telemetry: f.obs.telemetry},
		middleware.WithTracingServer("rpc"), middleware.WithTracingOnly(isRPC), middleware.WithTracingAPI(method))
	if err != nil {
		return nil, err
	}
	tracer, err := middleware.NewTrace(middleware.TraceDeps{Tracer: f.obs.trace},
		middleware.WithTraceType(trace.TypeRPC), middleware.WithTraceOnly(isRPC), middleware.WithTraceAPI(method))
	if err != nil {
		return nil, err
	}
	return []middleware.Middleware{tracing, tracer}, nil
}

// auditHolder is the audit log of the internode routes: the one of the
// composed services, none before.
type auditHolder struct {
	mu  sync.RWMutex
	svc audit.Service
}

var _ audit.Service = (*auditHolder)(nil)

func (h *auditHolder) set(s audit.Service) {
	h.mu.Lock()
	h.svc = s
	h.mu.Unlock()
}

func (h *auditHolder) get() audit.Service {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.svc
}

// Log implements audit.Service.
func (h *auditHolder) Log(ctx context.Context, e audit.Entry) error {
	if s := h.get(); s != nil {
		return s.Log(ctx, e)
	}
	return nil
}

// Search implements audit.Service: nothing before the services are
// composed.
func (h *auditHolder) Search(ctx context.Context, q audit.Query) ([]audit.Entry, error) {
	if s := h.get(); s != nil {
		return s.Search(ctx, q)
	}
	return nil, nil
}

// auditDir is the directory of the queues of the audit targets, on the
// first local drive (beside the queues of the notification targets).
const auditDir = "audit"

// newAuditor composes the audit log of the node: the structured log, the
// memory searched by the console (over the peers) and the audit targets
// of the configuration with their persistent queues on a local drive.
func newAuditor(d composeDeps, dyn configsvc.Service) (audit.Dispatcher, error) {
	f, logger := d.f, d.logger
	queueRoot := ""
	if len(f.local) > 0 {
		queueRoot = filepath.Join(f.local[0].Endpoint().Path, domain.SystemBucket, auditDir)
	}
	targets := event.NewDispatcher(event.Deps{
		Factory: audit.NewTargetFactory(audit.FactoryDeps{Logger: logger.With("component", "audit")}),
		Logger:  logger.With("component", "audit"),
	}, event.WithQueueRoot(queueRoot))
	peers := make([]audit.Peer, 0, len(f.obs.peers))
	for _, p := range f.obs.peers {
		peers = append(peers, p)
	}
	auditor, err := audit.New(audit.Deps{
		Logger: logger.With("component", "audit"), Clock: d.clock, NodeID: f.obs.nodeID, DeploymentID: d.deploymentID,
		Peers: peers, Targets: targets, Config: dyn,
	})
	if err != nil {
		return nil, err
	}
	f.obs.audit.set(auditor)
	return auditor, nil
}

// composedObservability are the observability services composed with the
// services of the deployment.
type composedObservability struct {
	// logs is the log stream of the cluster (the console and the CLI).
	logs logs.Service
	// exporter serves the scrape endpoints.
	exporter metrics.Exporter
	// runner starts the audit targets and the configuration of the logs.
	runner lifecycle.Runner
}

// composeObservability builds the cluster view of the log stream, the
// exporter of the scrape endpoints (cluster families from the server
// information, bucket usage from the scanner) and the runners of the
// audit log and of the logger configuration, and registers the families
// read from the services at every scrape.
func composeObservability(ctx context.Context, d composeDeps, cluster metrics.ClusterInfo, src metrics.Sources, auditor audit.Dispatcher,
	dyn configsvc.Service) (*composedObservability, error) {
	f, logger := d.f, d.logger
	logPeers := make([]logs.Peer, 0, len(f.obs.peers))
	metricPeers := make([]metrics.Peer, 0, len(f.obs.peers))
	for _, p := range f.obs.peers {
		logPeers = append(logPeers, p)
		metricPeers = append(metricPeers, p)
	}
	clusterLogs, err := logs.NewCluster(logs.ClusterDeps{Local: f.obs.logs, Peers: logPeers, Logger: logger.With("component", "logs")})
	if err != nil {
		return nil, err
	}
	exporter, err := metrics.NewExporter(metrics.ExporterDeps{
		Registry: f.registry, NodeID: f.obs.nodeID, Peers: metricPeers, Cluster: cluster, Usage: src.Scanner, Logger: logger.With("component", "metrics"),
	})
	if err != nil {
		return nil, err
	}
	src.Peers = f.peers
	if err := metrics.RegisterSources(ctx, f.registry, src); err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	configurator, err := logs.NewConfigurator(logs.ConfiguratorDeps{Config: dyn, Logs: f.obs.logs, Logger: logger.With("component", "logs")})
	if err != nil {
		return nil, err
	}
	return &composedObservability{
		logs:     clusterLogs,
		exporter: exporter,
		runner:   runners(configurator, auditor),
	}, nil
}

// runners starts rs in order and stops them in reverse order.
func runners(rs ...lifecycle.Runner) lifecycle.Runner {
	return lifecycle.Funcs{
		OnStart: func(ctx context.Context) error {
			for i, r := range rs {
				if err := r.Start(ctx); err != nil {
					for j := i - 1; j >= 0; j-- {
						_ = rs[j].Stop(context.WithoutCancel(ctx))
					}
					return err
				}
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			var first error
			for i := len(rs) - 1; i >= 0; i-- {
				if err := rs[i].Stop(ctx); err != nil && first == nil {
					first = err
				}
			}
			return first
		},
	}
}

// tlsInfoOf describes the TLS configuration of the listeners reported by
// GET /info (the certificates come from the keystore).
func tlsInfoOf(cfg config.Static) *domain.TLSInfo {
	minVersion := cfg.TLSMinVersion
	if minVersion == "" {
		minVersion = config.TLSVersion12
	}
	return &domain.TLSInfo{Enabled: cfg.TLSEnabled(), MinVersion: minVersion, InternodeVerify: cfg.TLSInternodeVerify}
}

// telemetryInfoOf describes the distributed tracing reported by GET /info:
// the endpoint of the collector without its user info and query (which
// may carry credentials).
func telemetryInfoOf(cfg config.Static, obs *observability) *domain.TelemetryInfo {
	info := &domain.TelemetryInfo{Enabled: obs != nil && obs.telemetry != nil && obs.telemetry.Enabled(), SampleRatio: cfg.OTelSampleRatio}
	if cfg.OTelEndpoint == "" {
		return info
	}
	info.Protocol = cfg.OTelProtocol
	if info.Protocol == "" {
		info.Protocol = config.OTelProtocolHTTP
	}
	if u, err := url.Parse(cfg.OTelEndpoint); err == nil {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		info.Endpoint = u.String()
	}
	return info
}
