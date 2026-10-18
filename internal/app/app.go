// Package app composes the Nexo server: it wires service implementations to
// their interfaces according to the configuration and the detected
// deployment mode, builds the S3 and console HTTP servers and runs them as
// a lifecycle.Group. It is the only package that knows about concrete
// implementations.
package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/logs"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/service/trace"
	adminhttp "github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
	"github.com/r3n3r0/nexo/internal/version"
)

// Option configures Build.
type Option func(*options)

type options struct {
	logger   *slog.Logger
	clock    clock.Clock
	assets   fs.FS
	topology topology.Service
	stopWait time.Duration
	noFsync  bool
	// dial opens the internode connections (nil: net.Dialer).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// keystoreOpts tune the reloads of the certificates directory
	// (internal tests).
	keystoreOpts []certs.KeystoreOption
	// logOutput is the output of the logs the server owns (WithLogOutput).
	logOutput io.Writer
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithClock sets the clock (default wall clock).
func WithClock(c clock.Clock) Option {
	return func(o *options) {
		if c != nil {
			o.clock = c
		}
	}
}

// WithAssetsFS sets the files of the embedded web console, rooted at
// index.html. Without it the console server answers with a placeholder
// page.
func WithAssetsFS(fsys fs.FS) Option {
	return func(o *options) {
		if fsys != nil {
			o.assets = fsys
		}
	}
}

// WithTopology replaces the topology service (tests).
func WithTopology(t topology.Service) Option {
	return func(o *options) {
		if t != nil {
			o.topology = t
		}
	}
}

// WithoutFsync skips the fsync calls of the drives (tests: the data
// durability is left to the operating system, which a process crash does
// not affect).
func WithoutFsync() Option {
	return func(o *options) { o.noFsync = true }
}

// WithInternodeDialer opens the TCP connections of this node to its
// peers (tests: proxies that partition the nodes or slow their links,
// docs/06 TC-07, TC-08); default a net.Dialer.
func WithInternodeDialer(d func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(o *options) { o.dial = d }
}

// WithStopTimeout bounds the shutdown (default 30s).
func WithStopTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.stopWait = d
		}
	}
}

// App is a composed, runnable Nexo server.
type App struct {
	cfg config.Static
	// opts rebuild the application on a restart (POST /service/restart).
	opts     []Option
	o        options
	layout   *topology.Layout
	logger   *slog.Logger
	group    *lifecycle.Group
	stopWait time.Duration
	f        *foundation
	// s3Srv and consSrv are the listeners: the composed servers in the
	// single-node modes; in a cluster, servers whose gate serves the
	// composed APIs once the bootstrap is complete (the S3 one serves the
	// internode RPC from the start).
	s3Srv    server.Service
	consSrv  server.Service
	s3Gate   middleware.Gate
	consGate middleware.Gate
	// s3Certs serves, by SNI, the certificates of the distributions on
	// the S3 listener of a cluster node once its services are composed.
	s3Certs *certificateHolder
	// running is set while Run drives the application: only then may the
	// Admin API stop or restart the process (processActions).
	running *atomic.Bool
	// restarts carries the restart requests of the Admin API (POST
	// /service/restart) to Run.
	restarts chan struct{}

	mu sync.RWMutex
	// svc are the composed services: set by Build in the single-node
	// modes, by Start after the bootstrap in a cluster.
	svc *services
	// late are the background components started with svc in a cluster.
	late *lifecycle.Group
}

// Build composes the application: it parses the topology and builds the
// foundation of the deployment mode (docs/08 §8.1, docs/12 §12.4). In the
// single-node modes (SNSD, SNMD) it also formats or checks the drives and
// composes the object layer, the identity and policy services and the HTTP
// servers. A cluster node cannot format its drives alone: its peers reach
// it on the S3 port during the bootstrap, so Start opens the listener,
// runs the bootstrap and only then composes the services. Build does not
// open sockets. On error every drive opened so far is closed.
func Build(ctx context.Context, cfg config.Static, opts ...Option) (app *App, err error) {
	o := options{logger: slog.Default(), clock: clock.NewReal(), stopWait: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	logger := o.logger

	// Topology: mode and drive layout from the endpoint arguments; this
	// node is recognized among the URL endpoints by the port of its S3
	// address.
	topo := o.topology
	if topo == nil {
		topo = newTopology(cfg, logger)
	}
	layout, err := topo.Parse(ctx, cfg.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("app: parse endpoints: %w", err)
	}
	// The log stream tees the process logger from here on, and the
	// distributed tracing is configured (observability.go).
	obs, logger, err := newObservability(ctx, cfg, layout, o)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	f, err := newFoundation(foundationDeps{cfg: cfg, layout: layout, topo: topo, clock: o.clock, logger: logger, noFsync: o.noFsync, dial: o.dial,
		keystoreOpts: o.keystoreOpts, obs: obs})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.close(context.WithoutCancel(ctx))
		}
	}()
	a := &App{
		cfg: cfg, opts: opts, o: o, layout: layout, logger: logger, stopWait: o.stopWait, f: f,
		running: &atomic.Bool{}, restarts: make(chan struct{}, 1),
	}

	// Components stop in reverse order: the drives are released last.
	group := lifecycle.NewGroup(lifecycle.WithLogger(logger), lifecycle.WithStopTimeout(o.stopWait))
	group.Add("drives", lifecycle.Funcs{OnStop: f.close})
	// The spans are flushed once every component stopped; the ring
	// buffer of the metrics samples while the node runs.
	group.Add("telemetry", obs.telemetry)
	group.Add("metrics-history", obs.history)
	if f.keystore != nil {
		// The certificates directory is watched while the listeners
		// serve (started before them, stopped after them).
		group.Add("tls", f.keystore)
	}
	// The leftovers of the writes a crashed peer coordinated stay in the
	// temporary area of this node's drives until they age out (file times
	// are wall-clock times: no injected clock).
	janitor, err := storage.NewJanitor(storage.JanitorDeps{Drives: f.local, Logger: logger.With("component", "janitor")})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	group.Add("janitor", janitor)
	group.Add("notifier", f.notifier)
	if f.distributed() {
		// Started before the listeners, stopped after them: every file
		// handle of the peers is closed once no call can open one.
		group.Add("rpc-handles", f.storageRoutes)
		const waiting = "the cluster bootstrap is not complete: this node waits for its peers"
		a.s3Gate = middleware.NewGate(middleware.GateDeps{}, middleware.WithS3Errors(), middleware.WithClosedReason(waiting))
		a.consGate = middleware.NewGate(middleware.GateDeps{}, middleware.WithClosedReason(waiting))
		a.s3Certs = &certificateHolder{}
		// The S3 port carries the internode RPC: the larger HTTP/2
		// windows of the shard streams (the other listeners keep the
		// defaults of net/http, docs/security/threat-model.md).
		s3Opts := append(serverOptions(cfg, f.keystore, cfg.Address, "s3"),
			server.WithHTTP2Windows(server.RPCStreamWindow, server.RPCConnWindow, server.RPCMaxFrameSize))
		if cfg.TLSEnabled() {
			s3Opts = append(s3Opts, server.WithCertificates(a.s3Certs))
		}
		// The internode calls are traced (live trace, spans) in front of
		// the RPC server.
		rpcObservers, err := f.rpcObservers(f.isRPC(cfg))
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		a.s3Srv = server.New(server.Deps{Middlewares: append(rpcObservers, f.rpc, a.s3Gate), Logger: logger}, s3Opts...)
		a.consSrv = server.New(server.Deps{Middlewares: []middleware.Middleware{a.consGate}, Logger: logger},
			serverOptions(cfg, f.keystore, cfg.ConsoleAddress, "console")...)
		group.Add("s3", a.s3Srv)
		group.Add("console", a.consSrv)
		group.Add("cluster", lifecycle.Funcs{OnStart: a.joinCluster, OnStop: a.leaveCluster})
		a.group = group
		logger.Info("deployment layout", "mode", layout.Mode.String(), "pools", len(layout.Pools), "nodes", len(layout.Nodes),
			"drives", layout.DriveCount(), "local_drives", len(f.local), "node", f.nodeID)
		return a, nil
	}

	// Single node: the drives are formatted (or checked) here.
	res, err := f.bootstrapper.Bootstrap(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: format drives: %w", err)
	}
	logger.Info("deployment layout", "mode", layout.Mode.String(), "pools", len(layout.Pools), "drives", len(f.local), "deployment_id", res.DeploymentID)
	svc, err := compose(ctx, a.composeDeps(res.DeploymentID))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.svc = svc
	a.s3Srv, a.consSrv = svc.s3Srv, svc.consSrv
	group.Add("services", svc.runners())
	group.Add("leader", f.leader)
	group.Add("s3", svc.s3Srv)
	group.Add("console", svc.consSrv)
	a.group = group
	return a, nil
}

// newTopology returns the topology service of the configuration: the S3
// port is the port of this node, and the default port of the URL
// endpoints that name none.
func newTopology(cfg config.Static, logger *slog.Logger) topology.Service {
	var topoOpts []topology.Option
	if port := portOf(cfg.Address); port > 0 {
		topoOpts = append(topoOpts, topology.WithLocalPort(port), topology.WithDefaultPort(port))
	}
	return topology.New(topology.Deps{Logger: logger}, topoOpts...)
}

// composeDeps are the inputs of compose for this application.
func (a *App) composeDeps(deploymentID string) composeDeps {
	return composeDeps{
		cfg: a.cfg, f: a.f, deploymentID: deploymentID, clock: a.o.clock, logger: a.logger,
		assets: a.o.assets, running: a.running, restart: a.requestRestart,
	}
}

// joinCluster runs the bootstrap of a cluster node (its listeners are
// open: the peers bootstrap through them), binds the internode
// authentication to the deployment, composes the services and opens the
// gates of the listeners. It blocks until a quorum of the nodes and of
// the drives of every erasure set is available, or ctx ends.
func (a *App) joinCluster(ctx context.Context) error {
	res, err := a.f.bootstrapper.Bootstrap(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	a.f.auth.SetDeploymentID(ctx, res.DeploymentID)
	if len(res.Healing) > 0 {
		a.logger.Warn("cluster: local drives formatted as replacements, to be healed", "drives", len(res.Healing))
	}
	if len(res.Joined) > 0 {
		a.logger.Info("cluster: local drives joined the fresh deployment, copying its system configuration", "drives", len(res.Joined))
	}
	svc, err := compose(ctx, a.composeDeps(res.DeploymentID))
	if err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	late := lifecycle.NewGroup(lifecycle.WithLogger(a.logger), lifecycle.WithStopTimeout(a.stopWait))
	late.Add("services", svc.runners())
	late.Add("leader", a.f.leader)
	if err := late.Start(ctx); err != nil {
		svc.unsubscribe()
		return err
	}
	a.mu.Lock()
	a.svc, a.late = svc, late
	a.mu.Unlock()
	a.s3Certs.set(svc.dist.certs)
	a.s3Gate.Open(svc.s3Srv.Handler())
	a.consGate.Open(svc.consSrv.Handler())
	a.logger.Info("cluster: node ready", "node", a.f.nodeID, "deployment_id", res.DeploymentID,
		"offline_drives", len(res.Offline), "healing_drives", len(res.Healing))
	return nil
}

// leaveCluster turns the requests away, waits for the requests in
// progress and stops the background components of a cluster node. The
// requests are drained first: what they report to the background
// components (the MRF entries of their partial writes) reaches them
// before the components stop and persist their state. The listeners stay
// open for the internode RPC of the peers until the group stops them.
func (a *App) leaveCluster(ctx context.Context) error {
	a.s3Gate.Close("the node is stopping")
	a.consGate.Close("the node is stopping")
	for _, g := range []struct {
		name string
		gate middleware.Gate
	}{{"s3", a.s3Gate}, {"console", a.consGate}} {
		if err := g.gate.Drain(ctx); err != nil {
			a.logger.Warn("cluster: requests still in progress at the stop", "listener", g.name, "err", err)
		}
	}
	a.mu.Lock()
	late := a.late
	a.late = nil
	a.mu.Unlock()
	if late == nil {
		return nil
	}
	return late.Stop(ctx)
}

// services returns the composed services, nil before a cluster node
// completed its bootstrap.
func (a *App) services() *services {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.svc
}

// Locations of the persisted identity, policy and configuration documents,
// under <drive>/.nexo.sys/config.
const (
	iamDir      = "iam"
	policiesDir = "policies"
	configFile  = "config.json"
)

// bucketPolicies adapts the bucket metadata store to
// policy.BucketPolicyProvider: the policy engine parses the bucket policy
// documents the bucket service stored (and validated).
type bucketPolicies struct {
	store bucket.MetadataStore
}

var _ policy.BucketPolicyProvider = bucketPolicies{}

// GetBucketPolicy implements policy.BucketPolicyProvider.
func (b bucketPolicies) GetBucketPolicy(ctx context.Context, name string) ([]byte, error) {
	// The access configuration: from the cache, or the last known one
	// while the drives cannot answer.
	cfg, err := b.store.AccessConfig(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(cfg.Policy) == 0 {
		return nil, fmt.Errorf("bucket %s: %w", name, domain.ErrPolicyNotFound)
	}
	return cfg.Policy, nil
}

// publicS3URL is the public URL of the S3 API the share links point to:
// NEXO_SERVER_URL, or the S3 listen address (localhost when it names no
// host).
func publicS3URL(cfg config.Static) string {
	if cfg.ServerURL != "" {
		return cfg.ServerURL
	}
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return scheme + "://localhost"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// secureConsole reports a console served over HTTPS: by this server (TLS
// certificates) or by a proxy terminating TLS in front of it (an https
// public console URL). Its session cookies then always carry Secure;
// otherwise the flag follows each request (TLS, or X-Forwarded-Proto of a
// trusted proxy).
func secureConsole(cfg config.Static) bool {
	return cfg.TLSEnabled() || strings.HasPrefix(strings.ToLower(cfg.BrowserRedirectURL), "https://")
}

// domainsOf returns the virtual-host domains of the configuration.
func domainsOf(cfg config.Static) []string {
	if cfg.Domain == "" {
		return nil
	}
	return []string{cfg.Domain}
}

// middlewareDeps are the services the HTTP middlewares are built on.
type middlewareDeps struct {
	logger   *slog.Logger
	clock    clock.Clock
	codec    httpx.Codec
	ids      httpx.IDGenerator
	registry metrics.Registry
	verifier auth.Verifier
	auditor  audit.Service
	// domains are the virtual-host domains of the S3 listener.
	domains []string
	// trustedProxies are the reverse proxies whose X-Forwarded-* headers
	// name the client of the requests of both listeners.
	trustedProxies []string
	// forwardedHeaders is the header family the trusted proxies set
	// (x-forwarded or forwarded, RFC 7239).
	forwardedHeaders string
	// trustForwardedHost uses the host the trusted proxies forward
	// (config.Static.TrustForwardedHost).
	trustForwardedHost bool
	// dist routes the requests of the distributions around the S3
	// authentication (nil: none).
	dist *distStack
	// tracer publishes the requests to the live trace, telemetry records
	// their spans (observability.go).
	tracer    trace.Service
	telemetry trace.Telemetry
	// limiter enforces the rate and concurrency limits of the requests
	// (S-OPS-10; nil: none).
	limiter ratelimit.Service
}

// s3Middlewares returns the middleware chain of the S3 listener, outermost
// first: request ids, forwarded client, panic recovery, access log,
// metrics, audit, the rate limit of the client address and the
// concurrency limit (api.requests_max), SigV4 authentication and the rate
// limits of the access key and of the bucket (the S3 API applies CORS).
// The limits run inside the observers, which see and mark the refusals,
// and leave alone the health probes, the metrics scrapes and the website
// distributions (which have their own limit). Audit wraps the
// authentication so that rejected requests are recorded too; it learns the
// identity through its holder. The client behind the trusted proxies is
// the one of the console listener too, so that aws:SourceIp and
// aws:SecureTransport decide alike through S3 and through the console
// (docs/04 §4.3).
func s3Middlewares(d middlewareDeps) ([]middleware.Middleware, error) {
	forwarded, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(d.trustedProxies...),
		middleware.WithForwardedHeaders(middleware.ForwardedHeaders(d.forwardedHeaders)), middleware.WithForwardedHost(d.trustForwardedHost))
	if err != nil {
		return nil, err
	}
	httpMetrics, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: d.registry, Clock: d.clock}, middleware.WithMetricsServer("s3"))
	if err != nil {
		return nil, err
	}
	// The metrics routes authenticate their bearer tokens themselves; a
	// virtual-host request with the same path is an object request.
	skip, service := s3.MetricsRequest(d.domains...), s3.SigningService(d.domains...)
	targetDeps := middleware.TargetDeps{}
	if d.dist != nil {
		skip, service = d.dist.authSkip(skip), d.dist.authService(service)
		targetDeps.HostBuckets = d.dist.hostBuckets()
	}
	authn, err := middleware.NewAuth(middleware.AuthDeps{Verifier: d.verifier, OnError: middleware.S3ErrorWriter(d.codec),
		Skip: skip, Service: service})
	if err != nil {
		return nil, err
	}
	auditing, err := middleware.NewAudit(middleware.AuditDeps{Auditor: d.auditor, Clock: d.clock}, middleware.WithAuditDomains(d.domains...))
	if err != nil {
		return nil, err
	}
	obs, err := observers(d, "s3", trace.TypeS3, nil)
	if err != nil {
		return nil, err
	}
	apiMetrics, err := middleware.NewAPIMetrics(middleware.APIMetricsDeps{Registry: d.registry, Clock: d.clock},
		middleware.WithAPIMetricsDomains(d.domains...))
	if err != nil {
		return nil, err
	}
	limits, err := s3Limits(d)
	if err != nil {
		return nil, err
	}
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		forwarded,
		// The bucket and object of the request as the S3 API resolves
		// them (custom domains of the s3 distributions included), for the
		// observers of a request refused before its handler.
		middleware.NewTarget(targetDeps, middleware.WithTargetDomains(d.domains...)),
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: middleware.S3ErrorWriter(d.codec)}),
		// The span of the request, child of the trace context of the
		// caller: the internode calls it makes are its children.
		obs.tracing,
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "s3"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
		// CORS is applied by the S3 API itself (s3.APIDeps.Buckets and
		// Settings): bucket CORS configurations, api.cors_allow_origin.
		auditing,
		// The S3 API metrics and the live trace see the operation the
		// router names and the identity the authentication establishes.
		apiMetrics,
		obs.trace,
		limits.ip,
		limits.concurrency,
		authn,
		limits.user,
	}, nil
}

// requestLimits are the limit middlewares of the S3 listener (nil without
// a limiter: Chain skips them).
type requestLimits struct {
	ip, concurrency, user middleware.Middleware
}

// s3Limits returns the limit middlewares of the S3 listener. The health
// probes, the metrics scrapes and the internode RPC (the reserved "/nexo/"
// paths but the video streams, outside the virtual hosts) are exempt, and
// so are the website distributions, limited by their own rate limit in the
// host router.
func s3Limits(d middlewareDeps) (requestLimits, error) {
	var out requestLimits
	if d.limiter == nil {
		return out, nil
	}
	virtualHost := s3.VirtualHostRequest(d.domains...)
	var website func(*http.Request) bool
	if d.dist != nil {
		website = dist.WebsiteRequest(d.dist.s3Routes)
	}
	exempt := func(r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, domain.SystemPathPrefix) && !strings.HasPrefix(r.URL.Path, domain.MediaPathPrefix) && !virtualHost(r) {
			return true
		}
		return website != nil && website(r)
	}
	deps := middleware.RateLimitDeps{Limiter: d.limiter, OnError: middleware.S3ErrorWriter(d.codec)}
	var err error
	if out.ip, err = middleware.NewRateLimit(deps, middleware.WithRateLimitSkip(exempt)); err != nil {
		return out, err
	}
	if out.concurrency, err = middleware.NewConcurrencyLimit(deps, middleware.WithConcurrencySkip(exempt)); err != nil {
		return out, err
	}
	out.user, err = middleware.NewRateLimit(deps, middleware.WithRateLimitSkip(exempt), middleware.WithRateLimitDomains(d.domains...),
		middleware.WithRateLimitScopes(ratelimit.KindUser, ratelimit.KindBucket))
	return out, err
}

// requestObservers are the middlewares of the distributed tracing and of
// the live trace of a listener.
type requestObservers struct {
	tracing middleware.Middleware
	trace   middleware.Middleware
}

// observers returns the request observers of a listener (server names it
// in the spans, typ in the live trace); only, when set, selects the
// requests they observe.
func observers(d middlewareDeps, server string, typ trace.Type, only func(r *http.Request) bool) (requestObservers, error) {
	var out requestObservers
	var err error
	if out.tracing, err = middleware.NewTracing(middleware.TracingDeps{Telemetry: d.telemetry},
		middleware.WithTracingServer(server), middleware.WithTracingOnly(only), middleware.WithTracingDomains(d.domains...)); err != nil {
		return out, err
	}
	out.trace, err = middleware.NewTrace(middleware.TraceDeps{Tracer: d.tracer, Clock: d.clock},
		middleware.WithTraceType(typ), middleware.WithTraceOnly(only), middleware.WithTraceDomains(d.domains...))
	return out, err
}

// consoleAPIRequest selects the requests of the Admin API among those of
// the console listener (not the assets of the web UI).
func consoleAPIRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, adminhttp.Prefix+"/")
}

// consoleMiddlewares returns the middleware chain of the console listener;
// the session authentication is applied by the Admin API itself.
func consoleMiddlewares(d middlewareDeps) ([]middleware.Middleware, error) {
	httpMetrics, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: d.registry, Clock: d.clock}, middleware.WithMetricsServer("console"))
	if err != nil {
		return nil, err
	}
	// The client of a request (login throttling, aws:SourceIp, Secure
	// cookies) is the one behind the trusted proxies.
	forwarded, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(d.trustedProxies...),
		middleware.WithForwardedHeaders(middleware.ForwardedHeaders(d.forwardedHeaders)), middleware.WithForwardedHost(d.trustForwardedHost))
	if err != nil {
		return nil, err
	}
	// The requests of the Admin API are audited and traced, not the
	// assets of the web UI.
	auditing, err := middleware.NewAudit(middleware.AuditDeps{Auditor: d.auditor, Clock: d.clock},
		middleware.WithAuditType(middleware.AuditTypeAdmin), middleware.WithAuditOnly(consoleAPIRequest))
	if err != nil {
		return nil, err
	}
	obs, err := observers(d, "console", trace.TypeAdmin, consoleAPIRequest)
	if err != nil {
		return nil, err
	}
	// The rate limit of the client address on the Admin API (not the
	// assets of the web UI, not the health probes), inside the observers.
	var ipLimit middleware.Middleware
	if d.limiter != nil {
		if ipLimit, err = middleware.NewRateLimit(middleware.RateLimitDeps{Limiter: d.limiter, OnError: middleware.ProblemErrorWriter(d.codec)},
			middleware.WithRateLimitAPI(ratelimit.APIAdmin),
			middleware.WithRateLimitSkip(func(r *http.Request) bool { return !consoleAPIRequest(r) })); err != nil {
			return nil, err
		}
	}
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		forwarded,
		// nosniff, framing, HSTS over TLS; the Admin API answers are
		// never stored (they carry the CSRF token, users, keys...).
		middleware.NewSecurityHeaders(middleware.SecurityHeadersDeps{}, middleware.WithNoStore(consoleAPIRequest)),
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: middleware.ProblemErrorWriter(d.codec)}),
		obs.tracing,
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "console"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
		// The proxied uploads of the console stream up to their own limits;
		// the JSON endpoints bound their bodies themselves.
		middleware.NewMaxBody(middleware.MaxBodyDeps{}, middleware.WithMaxBodyLimit(
			max(middleware.DefaultMaxBody, adminsvc.DefaultMaxUploadSize, adminsvc.DefaultMaxPartSize))),
		// The Admin API authenticates its requests itself: the audit and
		// the live trace learn the identity through their holder.
		auditing,
		obs.trace,
		ipLimit,
	}, nil
}

// adminDeps are the services the Admin API route groups are built on.
type adminDeps struct {
	codec    httpx.Codec
	clock    clock.Clock
	logger   *slog.Logger
	region   string
	s3URL    string
	sessions session.Service
	verifier auth.Verifier
	signer   auth.Signer
	admin    adminsvc.Service
	iam      iam.Service
	policy   policy.Service
	buckets  bucket.Service
	objects  object.Service
	media    media.Service
	shares   share.Service
	config   configsvc.Service
	// locks exposes the namespace locks of the deployment (diagnostics).
	locks lock.Manager
	// lifecycle and events back the lifecycle, notification and target
	// endpoints of the console.
	lifecycle ilm.Service
	events    event.Service
	// kms is the KMS of the server-side encryption (/kms endpoints).
	kms kms.Service
	// secureCookies forces the Secure flag of the session cookies (the
	// console is served over HTTPS, possibly by a proxy terminating TLS).
	secureCookies bool
	// consoleURL is the public URL of the console, when configured: an
	// origin the console requests may come from besides their Host.
	consoleURL string
	// accessRules and dist back the access rules, distributions and
	// certificates endpoints (docs/10 §10.5); nil leaves them out.
	accessRules accessrule.Service
	dist        *distStack
	// acmeDirectory and acmeChallenge describe the ACME of the
	// certificates of the distributions ("" for the defaults).
	acmeDirectory string
	acmeChallenge string
	// cache backs the cache endpoints (docs/11 §11.11); nil leaves them
	// out.
	cache cache.Service
	// diagnostics backs the profiling and speed test endpoints
	// (S-OPS-11); nil leaves them out.
	diagnostics adminsvc.Diagnostics
	// obsTrace, obsLogs, obsAudit and obsHistory back the live streams,
	// the audit search and the metrics of the console (docs/05 M8).
	obsTrace   trace.Service
	obsLogs    logs.Service
	obsAudit   audit.Service
	obsHistory metrics.History
	// limiter enforces the rate limit of the users of the Admin API
	// (S-OPS-10; nil: none).
	limiter ratelimit.Service
}

// adminServices are the console services of internal/service/admin the
// Admin API route groups delegate to.
type adminServices struct {
	authz      adminsvc.Authorizer
	buckets    adminsvc.BucketManager
	browser    adminsvc.ObjectBrowser
	uploads    adminsvc.UploadManager
	shares     adminsvc.ShareManager
	identities adminsvc.IdentityManager
	accessKeys adminsvc.AccessKeyManager
	policies   adminsvc.PolicyManager
	config     adminsvc.ConfigManager
	cluster    adminsvc.ClusterManager
	// lifecycle, notifications and cors edit the bucket configurations of
	// the console.
	lifecycle     adminsvc.LifecycleManager
	notifications adminsvc.NotificationManager
	cors          adminsvc.CORSManager
	// kms manages the keys of the KMS (nil without one wired).
	kms adminsvc.KMSManager
	// accessRules, distributions and certificates manage the access
	// rules, the distributions and their certificates (nil when the
	// services are not wired).
	accessRules   adminsvc.AccessRuleManager
	distributions adminsvc.DistributionManager
	certificates  adminsvc.CertificateManager
	// cache manages the cache of the distributions (nil when not wired).
	cache adminsvc.CacheManager
}

// buildAdminServices composes the console services around the
// authorization service.
func buildAdminServices(d adminDeps) (adminServices, error) {
	var (
		out adminServices
		err error
	)
	if out.authz, err = adminsvc.NewAuthorizer(adminsvc.AuthorizerDeps{Policy: d.policy, Creds: d.iam, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.buckets, err = adminsvc.NewBucketManager(adminsvc.BucketManagerDeps{Buckets: d.buckets, Objects: d.objects, Policy: d.policy, Authz: out.authz,
		AccessRules: d.accessRules, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.browser, err = adminsvc.NewObjectBrowser(adminsvc.ObjectBrowserDeps{Objects: d.objects, Authz: out.authz, Clock: d.clock, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.uploads, err = adminsvc.NewUploadManager(adminsvc.UploadManagerDeps{Objects: d.objects, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.shares, err = adminsvc.NewShareManager(adminsvc.ShareManagerDeps{Shares: d.shares, Signer: d.signer, Creds: d.iam, Authz: out.authz, Objects: d.objects, Clock: d.clock, Logger: d.logger},
		adminsvc.WithServerURL(d.s3URL), adminsvc.WithRegion(d.region)); err != nil {
		return out, err
	}
	if out.identities, err = adminsvc.NewIdentityManager(adminsvc.IdentityManagerDeps{IAM: d.iam, Authz: out.authz, Shares: d.shares, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.accessKeys, err = adminsvc.NewAccessKeyManager(adminsvc.AccessKeyManagerDeps{IAM: d.iam, Authz: out.authz, Shares: d.shares, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.policies, err = adminsvc.NewPolicyManager(adminsvc.PolicyManagerDeps{Policy: d.policy, Creds: d.iam, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.config, err = adminsvc.NewConfigManager(adminsvc.ConfigManagerDeps{Config: d.config, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.cluster, err = adminsvc.NewClusterManager(adminsvc.ClusterManagerDeps{
		IAM: d.iam, Policy: d.policy, Config: out.config, Buckets: d.buckets, Clock: d.clock, Logger: d.logger,
	}, adminsvc.WithServerVersion(version.Get().Version)); err != nil {
		return out, err
	}
	if out.lifecycle, err = adminsvc.NewLifecycleManager(adminsvc.LifecycleManagerDeps{Buckets: d.buckets, Lifecycle: d.lifecycle, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.notifications, err = adminsvc.NewNotificationManager(adminsvc.NotificationManagerDeps{
		Buckets: d.buckets, Events: d.events, Config: d.config, Logger: d.logger,
	}, adminsvc.WithARNRegion(d.region)); err != nil {
		return out, err
	}
	if out.cors, err = adminsvc.NewCORSManager(adminsvc.CORSManagerDeps{Buckets: d.buckets, Logger: d.logger}); err != nil {
		return out, err
	}
	if d.kms != nil {
		if out.kms, err = adminsvc.NewKMSManager(adminsvc.KMSManagerDeps{KMS: d.kms, Objects: d.objects, Logger: d.logger}); err != nil {
			return out, err
		}
	}
	if err := buildDistributionManagers(d, &out); err != nil {
		return out, err
	}
	return out, buildCacheManager(d, &out)
}

// buildAdminAPI assembles the Admin API route groups behind the
// authentication middleware (console session or SigV4).
func buildAdminAPI(d adminDeps) (httpx.Routes, error) {
	svc, err := buildAdminServices(d)
	if err != nil {
		return nil, err
	}
	var authnOpts []adminhttp.AuthenticatorOption
	if d.consoleURL != "" {
		authnOpts = append(authnOpts, adminhttp.WithConsoleOrigins(d.consoleURL))
	}
	authn, err := adminhttp.NewAuthenticator(adminhttp.AuthenticatorDeps{Sessions: d.sessions, Verifier: d.verifier, Enc: d.codec}, authnOpts...)
	if err != nil {
		return nil, err
	}
	var sessionOpts []adminhttp.SessionOption
	if d.secureCookies {
		sessionOpts = append(sessionOpts, adminhttp.WithSecureCookie(true))
	}
	sessionRoutes, err := adminhttp.NewSessionRoutes(adminhttp.SessionDeps{Sessions: d.sessions, Authz: svc.authz, Codec: d.codec, Clock: d.clock}, sessionOpts...)
	if err != nil {
		return nil, err
	}
	infoRoutes, err := adminhttp.NewInfoRoutes(adminhttp.InfoDeps{Admin: d.admin, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	userRoutes, err := adminhttp.NewUserRoutes(adminhttp.UserDeps{IAM: d.iam, Identities: svc.identities, AccessKeys: svc.accessKeys, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	policyRoutes, err := adminhttp.NewPolicyRoutes(adminhttp.PolicyDeps{Policy: d.policy, Manager: svc.policies, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	bucketRoutes, err := adminhttp.NewBucketRoutes(adminhttp.BucketDeps{Manager: svc.buckets, Buckets: d.buckets, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	objectRoutes, err := adminhttp.NewObjectRoutes(adminhttp.ObjectDeps{Browser: svc.browser, Uploads: svc.uploads, Authz: svc.authz, Media: d.media, Enc: d.codec, Logger: d.logger})
	if err != nil {
		return nil, err
	}
	shareRoutes, err := adminhttp.NewShareRoutes(adminhttp.ShareDeps{Shares: svc.shares, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	configRoutes, err := adminhttp.NewConfigRoutes(adminhttp.ConfigDeps{Config: svc.config, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	clusterRoutes, err := adminhttp.NewClusterRoutes(adminhttp.ClusterDeps{Cluster: svc.cluster, Locks: d.locks, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	lifecycleRoutes, err := adminhttp.NewLifecycleRoutes(adminhttp.LifecycleDeps{Lifecycle: svc.lifecycle, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	notificationRoutes, err := adminhttp.NewNotificationRoutes(adminhttp.NotificationDeps{Notifications: svc.notifications, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	corsRoutes, err := adminhttp.NewCORSRoutes(adminhttp.CORSDeps{CORS: svc.cors, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	kmsRoutes, err := adminhttp.NewKMSRoutes(adminhttp.KMSDeps{KMS: svc.kms, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	groups := []httpx.Routes{sessionRoutes, infoRoutes, userRoutes, policyRoutes, bucketRoutes, objectRoutes, shareRoutes, configRoutes, clusterRoutes,
		lifecycleRoutes, notificationRoutes, corsRoutes, kmsRoutes}
	distRoutes, err := buildDistributionRoutes(d, svc)
	if err != nil {
		return nil, err
	}
	cacheRoutes, err := buildCacheRoutes(d, svc)
	if err != nil {
		return nil, err
	}
	groups = append(append(groups, distRoutes...), cacheRoutes...)
	if d.diagnostics != nil {
		diagnosticsRoutes, err := adminhttp.NewDiagnosticsRoutes(adminhttp.DiagnosticsDeps{Diagnostics: d.diagnostics, Authz: svc.authz, Enc: d.codec})
		if err != nil {
			return nil, err
		}
		groups = append(groups, diagnosticsRoutes)
	}
	obsRoutes, err := adminhttp.NewObservabilityRoutes(adminhttp.ObservabilityDeps{
		Trace: d.obsTrace, Logs: d.obsLogs, Audit: d.obsAudit, History: d.obsHistory, Authz: svc.authz, Enc: d.codec,
	})
	if err != nil {
		return nil, err
	}
	groups = append(groups, obsRoutes)
	apiDeps := adminhttp.APIDeps{Enc: d.codec, Auth: authn}
	if d.limiter != nil {
		// The rate limit of the authenticated user (the one of the client
		// address is a middleware of the listener).
		if apiDeps.Limit, err = middleware.NewRateLimit(middleware.RateLimitDeps{Limiter: d.limiter, OnError: middleware.ProblemErrorWriter(d.codec)},
			middleware.WithRateLimitAPI(ratelimit.APIAdmin), middleware.WithRateLimitScopes(ratelimit.KindUser)); err != nil {
			return nil, err
		}
	}
	return adminhttp.NewAPI(apiDeps, groups...)
}

// processActions implements the service control of the Admin API (POST
// /service/restart|stop) for a server driven by App.Run: a stop is a
// SIGTERM to the process, handled by Run like the signal of an operator; a
// restart is a request to Run in the process itself (SIGHUP reloads the
// TLS certificates, it does not restart). Both take effect after the
// response is sent (the graceful shutdown waits for it). Outside Run
// (tests, embedding) the process belongs to someone else: the actions are
// not available.
type processActions struct {
	running *atomic.Bool
	signal  func(os.Signal) error
	restart func() error
}

var _ adminsvc.ActionHandler = processActions{}

// Handle implements adminsvc.ActionHandler.
func (p processActions) Handle(_ context.Context, action adminsvc.Action) error {
	if !p.running.Load() {
		return fmt.Errorf("app: service %s: only a server run as a process can be controlled: %w", action, domain.ErrNotImplemented)
	}
	var err error
	if action == adminsvc.ActionRestart {
		err = p.restart()
	} else {
		err = p.signal(syscall.SIGTERM)
	}
	if err != nil {
		return fmt.Errorf("app: service %s: %w", action, err)
	}
	return nil
}

// signalSelf sends a signal to the current process.
func signalSelf(sig os.Signal) error {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// requestRestart asks Run to restart the application; a request already
// pending covers this one.
func (a *App) requestRestart() error {
	select {
	case a.restarts <- struct{}{}:
	default:
	}
	return nil
}

// reloadTLS reloads the certificates of the listeners without a restart
// (SIGHUP, docs/07 §7.3): the certificates directory and the
// certificates of the distributions. The established connections keep
// their certificate; a failed reload keeps the current ones.
func (a *App) reloadTLS(ctx context.Context) {
	if a.f.keystore != nil {
		if err := a.f.keystore.Reload(ctx); err != nil {
			a.logger.ErrorContext(ctx, "SIGHUP: certificates directory not reloaded", "err", err)
		}
	}
	if s := a.services(); s != nil && s.dist.certs != nil {
		if err := s.dist.certs.Reload(ctx); err != nil {
			a.logger.ErrorContext(ctx, "SIGHUP: certificates of the distributions not reloaded", "err", err)
		}
	}
}

// Start starts every component. It returns once the servers listen and,
// for a cluster node, once the bootstrap is complete and the APIs are
// served.
func (a *App) Start(ctx context.Context) error {
	if err := a.group.Start(ctx); err != nil {
		// The error names the component ("start cluster: ...").
		return err
	}
	a.logger.Info("nexo started",
		"version", version.Get().Version,
		"mode", a.layout.Mode.String(),
		"s3", a.S3Addr(),
		"console", a.ConsoleAddr(),
	)
	return nil
}

// Stop stops every component in reverse order.
func (a *App) Stop(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.stopWait)
		defer cancel()
	}
	if err := a.group.Stop(ctx); err != nil {
		return fmt.Errorf("app: stop: %w", err)
	}
	a.logger.Info("nexo stopped")
	return nil
}

// Run starts the application, waits for ctx to end or for SIGINT/SIGTERM,
// then stops it. SIGHUP reloads the TLS certificates without a restart
// (reloadTLS). POST /service/restart restarts it in place: every
// component stops, the application is built again from the same
// configuration (re-reading the persisted state) and started. It returns
// the start, restart or stop error.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cur := a
	for restarted := false; ; restarted = true {
		if err := cur.Start(ctx); err != nil {
			if restarted && ctx.Err() != nil {
				// A shutdown requested during the restart: nothing runs.
				cur.logger.Info("shutdown requested during the restart", "reason", ctx.Err())
				return nil
			}
			return err
		}
		cur.running.Store(true)
	wait:
		for {
			select {
			case <-ctx.Done():
				cur.running.Store(false)
				cur.logger.Info("shutdown requested", "reason", ctx.Err())
				return cur.Stop(context.WithoutCancel(ctx))
			case <-hup:
				cur.logger.Info("SIGHUP: reloading the TLS certificates")
				cur.reloadTLS(ctx)
			case <-cur.restarts:
				break wait
			}
		}
		cur.running.Store(false)
		cur.logger.Info("restart requested")
		if err := cur.Stop(context.WithoutCancel(ctx)); err != nil {
			return err
		}
		if ctx.Err() != nil {
			cur.logger.Info("shutdown requested during the restart", "reason", ctx.Err())
			return nil
		}
		next, err := Build(ctx, cur.cfg, cur.opts...)
		if err != nil {
			if ctx.Err() != nil {
				// Interrupted by the shutdown, not a failure of the
				// restart (regression: SIGTERM during a restart exited
				// with "app: restart: ... context canceled").
				cur.logger.Info("shutdown requested during the restart", "reason", ctx.Err())
				return nil
			}
			return fmt.Errorf("app: restart: %w", err)
		}
		cur = next
	}
}

// S3Addr returns the S3 listen address (actual port after Start).
func (a *App) S3Addr() string { return a.s3Srv.Addr() }

// ConsoleAddr returns the console listen address (actual port after Start).
func (a *App) ConsoleAddr() string { return a.consSrv.Addr() }

// Mode returns the detected deployment mode.
func (a *App) Mode() domain.Mode { return a.layout.Mode }

// Layout returns the parsed topology.
func (a *App) Layout() *topology.Layout { return a.layout }

// NodeID returns the "host:port" of this node in a cluster, "local" in
// the single-node modes.
func (a *App) NodeID() string { return a.f.nodeID }

// IsLeader reports whether this node runs the singleton background jobs
// (docs/03 §3.5): always in the single-node modes once started, the
// holder of the leader lock in a cluster.
func (a *App) IsLeader() bool { return a.f.leader.IsLeader() }

// Leader returns the leader election of the node, for the singleton
// background jobs (scanner, MRF, ILM) to subscribe to.
func (a *App) Leader() cluster.Leader { return a.f.leader }

// Notifier returns the cluster notifier of the node (cache invalidations).
func (a *App) Notifier() cluster.Notifier { return a.f.notifier }

// DeploymentID returns the identifier written in the drives' format.json
// (empty until a cluster node completed its bootstrap).
func (a *App) DeploymentID() string {
	if s := a.services(); s != nil {
		return s.deploymentID
	}
	return ""
}

// IAM returns the identity service, for the integration tests that
// provision users and groups without going through the Admin API (nil
// until a cluster node completed its bootstrap, like the other service
// accessors).
func (a *App) IAM() iam.Service {
	if s := a.services(); s != nil {
		return s.identities
	}
	return nil
}

// Policies returns the policy engine and the store of named IAM policies
// (integration tests).
func (a *App) Policies() policy.Service {
	if s := a.services(); s != nil {
		return s.policies
	}
	return nil
}

// Buckets returns the bucket service (integration tests).
func (a *App) Buckets() bucket.Service {
	if s := a.services(); s != nil {
		return s.buckets
	}
	return nil
}

// Objects returns the object layer (integration and cluster tests).
func (a *App) Objects() object.Service {
	if s := a.services(); s != nil {
		return s.objects
	}
	return nil
}

// Shares returns the managed share-link service (integration tests: the
// Admin API exposes it from M3).
func (a *App) Shares() share.Service {
	if s := a.services(); s != nil {
		return s.shares
	}
	return nil
}

// ILM returns the bucket lifecycle service (integration tests apply the
// lifecycle at once with ApplyNow instead of waiting for the scanner).
func (a *App) ILM() ilm.Service {
	if s := a.services(); s != nil {
		return s.lifecycle
	}
	return nil
}

// Events returns the bucket notification service (integration tests).
func (a *App) Events() event.Service {
	if s := a.services(); s != nil {
		return s.events
	}
	return nil
}

// AccessRules returns the service of the access rules of the buckets
// (integration tests; the Admin API and the CLI manage them).
func (a *App) AccessRules() accessrule.Service {
	if s := a.services(); s != nil {
		return s.accessRules
	}
	return nil
}

// Distributions returns the distribution service (the Admin API, the CLI
// and the integration tests manage the distributions through it).
func (a *App) Distributions() distribution.Service {
	if s := a.services(); s != nil {
		return s.dist.service
	}
	return nil
}

// Certificates returns the certificates of the domains of the
// distributions.
func (a *App) Certificates() certs.Service {
	if s := a.services(); s != nil {
		return s.dist.certs
	}
	return nil
}

// DNSProviders returns the DNS providers of the ACME DNS-01 challenge
// (integration tests).
func (a *App) DNSProviders() certs.DNSProviders {
	if s := a.services(); s != nil {
		return s.dist.dnsProviders
	}
	return nil
}

// DistributionSigner signs the URLs and cookies of the signed behaviors
// with a private key of a key group (dist sign-url).
func (a *App) DistributionSigner() distribution.Signer {
	if s := a.services(); s != nil {
		return s.dist.signer
	}
	return nil
}

// WebAddr returns the address of the web listener (--web-address), ""
// when it is not configured or not started yet.
func (a *App) WebAddr() string {
	if s := a.services(); s != nil && s.dist.webSrv != nil {
		return s.dist.webSrv.Addr()
	}
	return ""
}

// WebTLSAddr returns the address of the TLS web listener
// (--web-tls-address), "" when it is not configured or not started yet.
func (a *App) WebTLSAddr() string {
	if s := a.services(); s != nil && s.dist.webTLSSrv != nil {
		return s.dist.webTLSSrv.Addr()
	}
	return ""
}

// Signer returns the SigV4 signer of the server (integration tests build
// POST policy forms and presigned URLs with it).
func (a *App) Signer() auth.Signer {
	if s := a.services(); s != nil {
		return s.signer
	}
	return nil
}

// serverOptions are the options of the S3 and console listeners: address,
// name, the PROXY protocol of the trusted proxies when enabled, and TLS
// with the certificates of the keystore (nil without TLS), reloaded at
// runtime.
func serverOptions(cfg config.Static, ks certs.Keystore, addr, name string) []server.Option {
	opts := []server.Option{server.WithAddr(addr), server.WithName(name), server.WithMinTLSVersion(tlsMinVersion(cfg))}
	if cfg.SendTimeout > 0 {
		// The stall bound of every response (NEXO_SEND_TIMEOUT; the
		// default of the server when zero).
		opts = append(opts, server.WithSendTimeout(cfg.SendTimeout))
	}
	if cfg.ProxyProtocolOn(name) {
		// The PROXY protocol of the trusted proxies (docs/10 §10.2).
		opts = append(opts, server.WithProxyProtocol(cfg.TrustedProxies...))
	}
	if ks != nil {
		opts = append(opts, server.WithKeystore(ks))
		if cfg.TLSInternodeVerify && name == "s3" {
			// The internode calls arrive on the S3 port: the nodes
			// present their certificate, verified with the current CAs
			// of the deployment (the S3 clients present none).
			opts = append(opts, server.WithClientCASource(ks))
		}
	}
	return opts
}

// tlsMinVersion is the oldest TLS version the listeners accept.
func tlsMinVersion(cfg config.Static) uint16 {
	if cfg.TLSMinVersion == config.TLSVersion13 {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}

func portOf(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}
