// Package app composes the Nexo server: it wires service implementations to
// their interfaces according to the configuration and the detected
// deployment mode, builds the S3 and console HTTP servers and runs them as
// a lifecycle.Group. It is the only package that knows about concrete
// implementations.
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	adminhttp "github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/console"
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
	cfg          config.Static
	layout       *topology.Layout
	deploymentID string
	logger       *slog.Logger
	group        *lifecycle.Group
	s3Srv        server.Service
	consSrv      server.Service
	stopWait     time.Duration
	// identities is the IAM service (integration tests provision users
	// through IAM).
	identities iam.Service
	// shares is the share-link service; the Admin API exposes it from M3
	// (/buckets/{bucket}/objects/share).
	shares share.Service
	// policies, buckets and signer are exposed to the integration tests
	// (internal/testutil), which drive the services directly.
	policies policy.Service
	buckets  bucket.Service
	signer   auth.Signer
}

// Build composes the application: it parses the topology, opens and formats
// the local drives, builds the object layer of the deployment mode, the
// identity and policy services and the HTTP servers. It does not open
// sockets: that happens in Start. On error every drive opened so far is
// closed.
//
// Only the single-drive mode (SNSD) is served for now: the erasure-coded
// modes (SNMD, distributed) return domain.ErrNotImplemented.
func Build(ctx context.Context, cfg config.Static, opts ...Option) (app *App, err error) {
	o := options{logger: slog.Default(), clock: clock.NewReal(), stopWait: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	logger := o.logger

	// Topology: mode and drive layout from the endpoint arguments.
	topo := o.topology
	if topo == nil {
		topoOpts := []topology.Option{}
		if port := portOf(cfg.Address); port > 0 {
			topoOpts = append(topoOpts, topology.WithLocalPort(port), topology.WithDefaultPort(port))
		}
		topo = topology.New(topology.Deps{Logger: logger}, topoOpts...)
	}
	layout, err := topo.Parse(ctx, cfg.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("app: parse endpoints: %w", err)
	}
	if layout.Mode != domain.ModeSNSD {
		// TODO(M5): erasure object layer (SNMD); TODO(M6): remote drives
		// and distributed locks.
		return nil, fmt.Errorf("app: %s deployment: erasure-coded modes are available from milestone M5, only a single drive (snsd) is served: %w",
			layout.Mode, domain.ErrNotImplemented)
	}

	// Drives: one local storage per local endpoint, bound together by the
	// format.json documents.
	drives, err := openDrives(storage.Deps{Clock: o.clock, Logger: logger}, layout)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	closeDrives := func(ctx context.Context) error {
		var errs []error
		for _, d := range drives {
			if cerr := d.Close(ctx); cerr != nil {
				errs = append(errs, cerr)
			}
		}
		return errors.Join(errs...)
	}
	defer func() {
		if err != nil {
			_ = closeDrives(context.WithoutCancel(ctx))
		}
	}()
	formatter := storage.NewFormatter(storage.FormatterDeps{Topology: topo, Logger: logger})
	deploymentID, err := formatter.EnsureFormats(ctx, layout, drives)
	if err != nil {
		return nil, fmt.Errorf("app: format drives: %w", err)
	}
	logger.Info("deployment layout", "mode", layout.Mode.String(), "pools", len(layout.Pools), "drives", len(drives), "deployment_id", deploymentID)

	// Cross-cutting services. The dynamic configuration, the identities
	// and the named policies are documents of the system volume
	// (<drive>/.nexo.sys/config).
	sysConfigDir := filepath.Join(drives[0].Endpoint().Path, domain.SystemBucket, storage.ConfigDir)
	registry := metrics.New(metrics.Deps{Logger: logger})
	auditor := audit.NewSlog(audit.Deps{Logger: logger.With("component", "audit"), Clock: o.clock})
	locks := lock.NewLocal(lock.Deps{})
	configStore, err := configsvc.NewFileStore(filepath.Join(sysConfigDir, configFile))
	if err != nil {
		return nil, fmt.Errorf("app: config store: %w", err)
	}
	dynConfig := configsvc.New(configsvc.Deps{Store: configStore, Logger: logger})
	events := event.NewDispatcher(event.Deps{Logger: logger.With("component", "events")})

	// Object layer (SNSD: the single local drive) and the bucket metadata
	// documents it reads the bucket configuration from.
	bucketStore, err := bucket.NewStore(bucket.StoreDeps{Drives: drives, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	objects, err := object.NewFS(object.Deps{
		Store:        drives[0],
		Buckets:      bucketStore,
		Locks:        locks,
		Events:       events,
		DeploymentID: deploymentID,
		Clock:        o.clock,
		Logger:       logger,
	}, object.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("app: object layer: %w", err)
	}

	// Identities, policies and buckets. IAM is the source of truth of the
	// policy attachments; the policy engine resolves bucket policies from
	// the bucket metadata store, which breaks the bucket → policy → bucket
	// construction cycle.
	iamStore, err := iam.NewFileStore(ctx, filepath.Join(sysConfigDir, iamDir), cfg.RootPassword)
	if err != nil {
		return nil, fmt.Errorf("app: iam store: %w", err)
	}
	identities := iam.New(iam.Deps{Store: iamStore, Clock: o.clock, Logger: logger.With("component", "iam")})
	if err := identities.Bootstrap(ctx, domain.Credentials{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword}); err != nil {
		return nil, fmt.Errorf("app: bootstrap root credentials: %w", err)
	}
	policyStore, err := policy.NewFileStore(filepath.Join(sysConfigDir, policiesDir))
	if err != nil {
		return nil, fmt.Errorf("app: policy store: %w", err)
	}
	policies := policy.New(policy.Deps{
		Store:       policyStore,
		Attachments: identities,
		Buckets:     bucketPolicies{store: bucketStore},
		Clock:       o.clock,
		Logger:      logger,
	})
	buckets, err := bucket.New(bucket.Deps{Objects: objects, Store: bucketStore, Policy: policies, Locks: locks, Clock: o.clock, Logger: logger},
		bucket.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	// Authentication, share links and console sessions.
	authDeps := auth.Deps{Creds: identities, Clock: o.clock, Logger: logger}
	verifier, err := auth.NewVerifier(authDeps, auth.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	signer := auth.NewSigner(authDeps, auth.WithRegion(cfg.Region))
	shares := share.New(share.Deps{IAM: identities, Signer: signer, Store: iamStore, Policy: policies, Clock: o.clock, Logger: logger},
		share.WithServerURL(publicS3URL(cfg)), share.WithRegion(cfg.Region))
	// The signing key derives from the root credentials so that every node
	// of a deployment accepts the same cookies.
	sessions, err := session.New(session.Deps{IAM: identities, Clock: o.clock, Logger: logger},
		session.WithSigningKey(session.DeriveKey(cfg.RootUser, cfg.RootPassword)))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	// Administration and health.
	build := version.Get()
	adminSvc := adminsvc.New(adminsvc.Deps{
		Clock:        o.clock,
		Logger:       logger,
		Build:        adminsvc.BuildInfo{Version: build.Version, Commit: build.Commit, Date: build.Date},
		Mode:         layout.Mode,
		Region:       cfg.Region,
		DeploymentID: deploymentID,
		Drives:       drives,
		Pools:        poolInfos(layout),
		Objects:      objects,
	})
	healthSvc := health.New(health.Deps{Readiness: map[string]health.Check{
		"storage": object.ReadinessCheck(objects),
	}})

	codec := httpx.New()
	ids := httpx.NewIDGenerator()

	// S3 server (:9000): every route goes through the SigV4 middleware,
	// which admits anonymous requests; the handlers authorize with the
	// policy engine.
	s3Middlewares, err := s3Middlewares(middlewareDeps{
		logger: logger, clock: o.clock, codec: codec, ids: ids,
		registry: registry, verifier: verifier, auditor: auditor, domains: domainsOf(cfg),
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	metricsRoutes, err := s3.NewMetricsRoutes(
		s3.MetricsDeps{Registry: registry, Verifier: verifier, Policy: policies, Enc: codec, Clock: o.clock},
		s3.WithPublicMetrics(cfg.PrometheusAuthType == config.PrometheusAuthPublic))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	s3API := s3.NewAPI(s3.APIDeps{Enc: codec, Domains: domainsOf(cfg), System: []httpx.Routes{
		s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
		metricsRoutes,
	}},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: buckets, Objects: objects, Policy: policies, Enc: codec, Clock: o.clock, Logger: logger},
			s3.WithRegion(cfg.Region)),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: objects, Policy: policies, Enc: codec, Clock: o.clock, Logger: logger}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Objects: objects, Policy: policies, Enc: codec, Clock: o.clock, Logger: logger}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Verifier: verifier, Objects: objects, Buckets: buckets, Policy: policies, Enc: codec, Clock: o.clock, Logger: logger},
			s3.WithRegion(cfg.Region)),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Verifier: verifier, Objects: objects, Policy: policies, Enc: codec, Clock: o.clock, Logger: logger}),
		s3.NewSTSRoutes(s3.STSDeps{IAM: identities, Policy: policies, Enc: codec, Clock: o.clock, Logger: logger}),
	)
	s3Srv := server.New(server.Deps{
		Routes:      []httpx.Routes{s3API},
		Middlewares: s3Middlewares,
		Logger:      logger,
	}, serverOptions(cfg, cfg.Address, "s3")...)

	// Console + Admin API server (:9001).
	consoleMiddlewares, err := consoleMiddlewares(middlewareDeps{
		logger: logger, clock: o.clock, codec: codec, ids: ids, registry: registry,
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	sessionAuth, err := middleware.NewSessionAuth(middleware.SessionAuthDeps{Sessions: sessions, Enc: codec})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	adminAPI, err := buildAdminAPI(adminDeps{
		codec: codec, clock: o.clock, auth: sessionAuth,
		sessions: sessions, admin: adminSvc, iam: identities, policy: policies,
		buckets: buckets, objects: objects, config: dynConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	consSrv := server.New(server.Deps{
		Routes: []httpx.Routes{
			s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
			adminAPI,
			console.NewSPARoutes(console.SPADeps{Assets: console.NewAssets(console.AssetsDeps{FS: o.assets}), Enc: codec}),
		},
		Middlewares: consoleMiddlewares,
		Logger:      logger,
	}, serverOptions(cfg, cfg.ConsoleAddress, "console")...)

	// Components stop in reverse order: the drives are released last.
	group := lifecycle.NewGroup(lifecycle.WithLogger(logger), lifecycle.WithStopTimeout(o.stopWait))
	group.Add("drives", lifecycle.Funcs{OnStop: closeDrives})
	group.Add("events", events)
	group.Add("s3", s3Srv)
	group.Add("console", consSrv)

	return &App{
		cfg:          cfg,
		layout:       layout,
		deploymentID: deploymentID,
		logger:       logger,
		group:        group,
		s3Srv:        s3Srv,
		consSrv:      consSrv,
		stopWait:     o.stopWait,
		identities:   identities,
		shares:       shares,
		policies:     policies,
		buckets:      buckets,
		signer:       signer,
	}, nil
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
}

// s3Middlewares returns the middleware chain of the S3 listener, outermost
// first: request ids, panic recovery, access log, metrics, CORS, audit and
// SigV4 authentication. Audit wraps the authentication so that rejected
// requests are recorded too; it learns the identity through its holder.
func s3Middlewares(d middlewareDeps) ([]middleware.Middleware, error) {
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
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: middleware.ProblemErrorWriter(d.codec)}),
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "console"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
		middleware.NewMaxBody(middleware.MaxBodyDeps{}, middleware.WithMaxBodyLimit(middleware.DefaultMaxBody)),
	}, nil
}

// adminDeps are the services the Admin API route groups are built on.
type adminDeps struct {
	codec    httpx.Codec
	clock    clock.Clock
	auth     middleware.Middleware
	sessions session.Service
	admin    adminsvc.Service
	iam      iam.Service
	policy   policy.Service
	buckets  bucket.Service
	objects  object.Service
	config   configsvc.Service
}

// buildAdminAPI assembles the Admin API route groups.
func buildAdminAPI(d adminDeps) (httpx.Routes, error) {
	sessionRoutes, err := adminhttp.NewSessionRoutes(adminhttp.SessionDeps{Sessions: d.sessions, Codec: d.codec, Clock: d.clock})
	if err != nil {
		return nil, err
	}
	infoRoutes, err := adminhttp.NewInfoRoutes(adminhttp.InfoDeps{Admin: d.admin, Policy: d.policy, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	userRoutes, err := adminhttp.NewUserRoutes(adminhttp.UserDeps{IAM: d.iam, Policy: d.policy, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	policyRoutes, err := adminhttp.NewPolicyRoutes(adminhttp.PolicyDeps{Policy: d.policy, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	bucketRoutes, err := adminhttp.NewBucketRoutes(adminhttp.BucketDeps{Buckets: d.buckets, Objects: d.objects, Policy: d.policy, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	configRoutes, err := adminhttp.NewConfigRoutes(adminhttp.ConfigDeps{Config: d.config, Policy: d.policy, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	return adminhttp.NewAPI(adminhttp.APIDeps{Enc: d.codec, Auth: d.auth},
		sessionRoutes, infoRoutes, userRoutes, policyRoutes, bucketRoutes, configRoutes), nil
}

// Start starts every component. It returns once the servers listen.
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
// then stops it. It returns the start or stop error.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	a.logger.Info("shutdown requested", "reason", ctx.Err())
	return a.Stop(context.WithoutCancel(ctx))
}

// S3Addr returns the S3 listen address (actual port after Start).
func (a *App) S3Addr() string { return a.s3Srv.Addr() }

// ConsoleAddr returns the console listen address (actual port after Start).
func (a *App) ConsoleAddr() string { return a.consSrv.Addr() }

// Mode returns the detected deployment mode.
func (a *App) Mode() domain.Mode { return a.layout.Mode }

// Layout returns the parsed topology.
func (a *App) Layout() *topology.Layout { return a.layout }

// DeploymentID returns the identifier written in the drives' format.json.
func (a *App) DeploymentID() string { return a.deploymentID }

// IAM returns the identity service, for the integration tests that
// provision users and groups without going through the Admin API.
func (a *App) IAM() iam.Service { return a.identities }

// Policies returns the policy engine and the store of named IAM policies
// (integration tests).
func (a *App) Policies() policy.Service { return a.policies }

// Buckets returns the bucket service (integration tests).
func (a *App) Buckets() bucket.Service { return a.buckets }

// Shares returns the managed share-link service (integration tests: the
// Admin API exposes it from M3).
func (a *App) Shares() share.Service { return a.shares }

// Signer returns the SigV4 signer of the server (integration tests build
// POST policy forms and presigned URLs with it).
func (a *App) Signer() auth.Signer { return a.signer }

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

// openDrives opens a local storage for every local drive of the layout, in
// layout order; on error the drives opened so far are closed.
func openDrives(deps storage.Deps, layout *topology.Layout) ([]storage.StorageAPI, error) {
	var drives []storage.StorageAPI
	for _, ep := range layout.LocalEndpoints() {
		if ep.IsURL() {
			continue
		}
		d, err := storage.NewLocal(deps, ep)
		if err != nil {
			for _, opened := range drives {
				_ = opened.Close(context.Background())
			}
			return nil, fmt.Errorf("open drive %s: %w", ep, err)
		}
		drives = append(drives, d)
	}
	if len(drives) == 0 {
		return nil, fmt.Errorf("%w: no local drives in the deployment", domain.ErrInvalidArgument)
	}
	return drives, nil
}
