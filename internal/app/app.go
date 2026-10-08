// Package app composes the Nexo server: it wires service implementations to
// their interfaces according to the configuration and the detected
// deployment mode, builds the S3 and console HTTP servers and runs them as
// a lifecycle.Group. It is the only package that knows about concrete
// implementations.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
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
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/service/topology"
	adminhttp "github.com/r3n3r0/nexo/internal/transport/http/admin"
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
	s3Gate   *gate
	consGate *gate
	// running is set while Run drives the application: only then may the
	// Admin API stop or restart the process (processActions).
	running *atomic.Bool

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

	f, err := newFoundation(foundationDeps{cfg: cfg, layout: layout, topo: topo, clock: o.clock, logger: logger, noFsync: o.noFsync})
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
		running: &atomic.Bool{},
	}

	// Components stop in reverse order: the drives are released last.
	group := lifecycle.NewGroup(lifecycle.WithLogger(logger), lifecycle.WithStopTimeout(o.stopWait))
	group.Add("drives", lifecycle.Funcs{OnStop: f.close})
	group.Add("notifier", f.notifier)
	if f.distributed() {
		a.s3Gate = newGate(true, "the cluster bootstrap is not complete: this node waits for its peers")
		a.consGate = newGate(false, "the cluster bootstrap is not complete: this node waits for its peers")
		a.s3Srv = server.New(server.Deps{Middlewares: []middleware.Middleware{f.rpc, a.s3Gate}, Logger: logger},
			serverOptions(cfg, cfg.Address, "s3")...)
		a.consSrv = server.New(server.Deps{Middlewares: []middleware.Middleware{a.consGate}, Logger: logger},
			serverOptions(cfg, cfg.ConsoleAddress, "console")...)
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
		assets: a.o.assets, running: a.running,
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
		return fmt.Errorf("cluster bootstrap: %w", err)
	}
	a.f.auth.SetDeploymentID(ctx, res.DeploymentID)
	if len(res.Healing) > 0 {
		a.logger.Warn("cluster: local drives formatted as replacements, to be healed", "drives", len(res.Healing))
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
	a.s3Gate.open(svc.s3Srv.Handler())
	a.consGate.open(svc.consSrv.Handler())
	a.logger.Info("cluster: node ready", "node", a.f.nodeID, "deployment_id", res.DeploymentID,
		"offline_drives", len(res.Offline), "healing_drives", len(res.Healing))
	return nil
}

// leaveCluster turns the requests away and stops the background
// components of a cluster node.
func (a *App) leaveCluster(ctx context.Context) error {
	a.s3Gate.close("the node is stopping")
	a.consGate.close("the node is stopping")
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
	m, err := b.store.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.Policy) == 0 {
		return nil, fmt.Errorf("bucket %s: %w", name, domain.ErrPolicyNotFound)
	}
	return m.Policy, nil
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
}

// s3Middlewares returns the middleware chain of the S3 listener, outermost
// first: request ids, forwarded client, panic recovery, access log,
// metrics, CORS, audit and SigV4 authentication. Audit wraps the
// authentication so that rejected requests are recorded too; it learns the
// identity through its holder. The client behind the trusted proxies is
// the one of the console listener too, so that aws:SourceIp and
// aws:SecureTransport decide alike through S3 and through the console
// (docs/04 §4.3).
func s3Middlewares(d middlewareDeps) ([]middleware.Middleware, error) {
	forwarded, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(d.trustedProxies...))
	if err != nil {
		return nil, err
	}
	httpMetrics, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: d.registry, Clock: d.clock}, middleware.WithMetricsServer("s3"))
	if err != nil {
		return nil, err
	}
	// The metrics routes authenticate their bearer tokens themselves; a
	// virtual-host request with the same path is an object request.
	authn, err := middleware.NewAuth(middleware.AuthDeps{Verifier: d.verifier, OnError: middleware.S3ErrorWriter(d.codec),
		Skip: s3.MetricsRequest(d.domains...), Service: s3.SigningService(d.domains...)})
	if err != nil {
		return nil, err
	}
	auditing, err := middleware.NewAudit(middleware.AuditDeps{Auditor: d.auditor, Clock: d.clock})
	if err != nil {
		return nil, err
	}
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		forwarded,
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: middleware.S3ErrorWriter(d.codec)}),
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "s3"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
		middleware.NewCORS(middleware.CORSDeps{}),
		auditing,
		authn,
	}, nil
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
	forwarded, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(d.trustedProxies...))
	if err != nil {
		return nil, err
	}
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		forwarded,
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: middleware.ProblemErrorWriter(d.codec)}),
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "console"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
		// The proxied uploads of the console stream up to their own limits;
		// the JSON endpoints bound their bodies themselves.
		middleware.NewMaxBody(middleware.MaxBodyDeps{}, middleware.WithMaxBodyLimit(
			max(middleware.DefaultMaxBody, adminsvc.DefaultMaxUploadSize, adminsvc.DefaultMaxPartSize))),
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
	shares   share.Service
	config   configsvc.Service
	// secureCookies forces the Secure flag of the session cookies (the
	// console is served over HTTPS, possibly by a proxy terminating TLS).
	secureCookies bool
	// consoleURL is the public URL of the console, when configured: an
	// origin the console requests may come from besides their Host.
	consoleURL string
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
	if out.buckets, err = adminsvc.NewBucketManager(adminsvc.BucketManagerDeps{Buckets: d.buckets, Objects: d.objects, Policy: d.policy, Authz: out.authz, Logger: d.logger}); err != nil {
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
	if out.accessKeys, err = adminsvc.NewAccessKeyManager(adminsvc.AccessKeyManagerDeps{IAM: d.iam, Authz: out.authz, Logger: d.logger}); err != nil {
		return out, err
	}
	if out.policies, err = adminsvc.NewPolicyManager(adminsvc.PolicyManagerDeps{Policy: d.policy, Creds: d.iam, Logger: d.logger}); err != nil {
		return out, err
	}
	out.config, err = adminsvc.NewConfigManager(adminsvc.ConfigManagerDeps{Config: d.config, Logger: d.logger})
	return out, err
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
	objectRoutes, err := adminhttp.NewObjectRoutes(adminhttp.ObjectDeps{Browser: svc.browser, Uploads: svc.uploads, Authz: svc.authz, Enc: d.codec, Logger: d.logger})
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
	return adminhttp.NewAPI(adminhttp.APIDeps{Enc: d.codec, Auth: authn},
		sessionRoutes, infoRoutes, userRoutes, policyRoutes, bucketRoutes, objectRoutes, shareRoutes, configRoutes)
}

// processActions implements the service control of the Admin API (POST
// /service/restart|stop) for a server driven by App.Run: the action is a
// signal to the process (SIGHUP restarts, SIGTERM stops), handled by Run
// like the signal of an operator, after the response is sent (the
// graceful shutdown waits for it). Outside Run (tests, embedding) the
// process belongs to someone else: the actions are not available.
type processActions struct {
	running *atomic.Bool
	signal  func(os.Signal) error
}

var _ adminsvc.ActionHandler = processActions{}

// Handle implements adminsvc.ActionHandler.
func (p processActions) Handle(_ context.Context, action adminsvc.Action) error {
	if !p.running.Load() {
		return fmt.Errorf("app: service %s: only a server run as a process can be controlled: %w", action, domain.ErrNotImplemented)
	}
	sig := os.Signal(syscall.SIGTERM)
	if action == adminsvc.ActionRestart {
		sig = syscall.SIGHUP
	}
	if err := p.signal(sig); err != nil {
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

// Start starts every component. It returns once the servers listen and,
// for a cluster node, once the bootstrap is complete and the APIs are
// served.
func (a *App) Start(ctx context.Context) error {
	if err := a.group.Start(ctx); err != nil {
		return fmt.Errorf("app: start: %w", err)
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
// then stops it. SIGHUP (sent by POST /service/restart) restarts it in
// place: every component stops, the application is built again from the
// same configuration (re-reading the persisted state) and started. It
// returns the start, restart or stop error.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cur := a
	for {
		if err := cur.Start(ctx); err != nil {
			return err
		}
		cur.running.Store(true)
		select {
		case <-ctx.Done():
			cur.running.Store(false)
			cur.logger.Info("shutdown requested", "reason", ctx.Err())
			return cur.Stop(context.WithoutCancel(ctx))
		case <-hup:
			cur.running.Store(false)
			cur.logger.Info("restart requested")
			if err := cur.Stop(context.WithoutCancel(ctx)); err != nil {
				return err
			}
			next, err := Build(ctx, cur.cfg, cur.opts...)
			if err != nil {
				return fmt.Errorf("app: restart: %w", err)
			}
			cur = next
		}
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

// Signer returns the SigV4 signer of the server (integration tests build
// POST policy forms and presigned URLs with it).
func (a *App) Signer() auth.Signer {
	if s := a.services(); s != nil {
		return s.signer
	}
	return nil
}

func serverOptions(cfg config.Static, addr, name string) []server.Option {
	opts := []server.Option{server.WithAddr(addr), server.WithName(name)}
	if cfg.TLSEnabled() {
		opts = append(opts, server.WithTLS(filepath.Join(cfg.CertsDir, "public.crt"), filepath.Join(cfg.CertsDir, "private.key")))
	}
	return opts
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

// poolInfos summarizes the pools of the layout for ServerInfo.
func poolInfos(layout *topology.Layout) []domain.PoolInfo {
	out := make([]domain.PoolInfo, 0, len(layout.Pools))
	for _, p := range layout.Pools {
		out = append(out, domain.PoolInfo{Index: p.Index, Sets: p.SetCount, DrivesPerSet: p.SetDriveCount, Parity: p.Parity})
	}
	return out
}
