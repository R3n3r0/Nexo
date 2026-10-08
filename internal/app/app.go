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
}

// Build composes the application: it parses the topology, opens and formats
// the local drives, selects the object layer from the deployment mode and
// assembles the HTTP servers. It does not open sockets: that happens in
// Start. On error every drive opened so far is closed.
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
	if layout.Mode == domain.ModeDistributed {
		// TODO(M6): remote drives (storage RPC client) and distributed locks.
		return nil, fmt.Errorf("app: %s deployments: %w", layout.Mode, domain.ErrNotImplemented)
	}

	// Drives: one local storage per local endpoint, bound together by the
	// format.json documents.
	drives, byEndpoint, err := openDrives(storage.Deps{Clock: o.clock, Logger: logger}, layout)
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

	// Cross-cutting services.
	registry := metrics.New(metrics.Deps{})
	auditor := audit.NewSlog(audit.Deps{Logger: logger.With("component", "audit"), Clock: o.clock})
	locks := lock.NewLocal(lock.Deps{})
	dynConfig := configsvc.New(configsvc.Deps{Store: configsvc.NewMemoryStore(nil), Logger: logger})
	events := event.NewDispatcher(event.Deps{Logger: logger.With("component", "events")})

	// Bucket metadata and the object layer, selected by the deployment mode.
	bucketStore, err := bucket.NewStore(bucket.StoreDeps{Drives: drives, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	objDeps := object.Deps{
		Buckets:      bucketStore,
		Locks:        locks,
		Events:       events,
		DeploymentID: deploymentID,
		Clock:        o.clock,
		Logger:       logger,
	}
	var objects object.Service
	switch layout.Mode {
	case domain.ModeSNSD:
		objDeps.Store = drives[0]
		objects, err = object.NewFS(objDeps, object.WithRegion(cfg.Region))
	case domain.ModeSNMD:
		pool := layout.Pools[0]
		objDeps.Sets = drivesOfSets(pool, byEndpoint)
		objDeps.Parity = pool.Parity
		objects, err = object.NewErasure(objDeps, object.WithRegion(cfg.Region))
	default:
		err = fmt.Errorf("deployment mode %s: %w", layout.Mode, domain.ErrNotImplemented)
	}
	if err != nil {
		return nil, fmt.Errorf("app: object layer: %w", err)
	}
	buckets, err := bucket.New(bucket.Deps{Objects: objects, Store: bucketStore, Clock: o.clock, Logger: logger}, bucket.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	// Identity, authentication, authorization, sessions.
	//
	// TODO(M2): iam.NewStore (.nexo.sys/config/iam) and the policy store on
	// the system bucket replace the root-only IAM and the in-memory policies.
	identities := iam.NewRootOnly(iam.Deps{Clock: o.clock, Logger: logger})
	if err := identities.Bootstrap(ctx, domain.Credentials{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword}); err != nil {
		return nil, fmt.Errorf("app: bootstrap root credentials: %w", err)
	}
	policies := policy.New(policy.Deps{Clock: o.clock, Logger: logger})
	verifier, err := auth.NewVerifier(auth.Deps{Creds: identities, Clock: o.clock, Logger: logger}, auth.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
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

	// S3 server (:9000).
	s3Metrics, err := middleware.NewMetrics(registry, "s3", middleware.WithMetricsClock(o.clock))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	metricsRoutes, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: registry})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	s3API := s3.NewAPI(codec,
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: buckets, Objects: objects, Policy: policies, Enc: codec}),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: objects, Buckets: buckets, Policy: policies, Enc: codec}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Objects: objects, Buckets: buckets, Policy: policies, Enc: codec}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Verifier: verifier, Objects: objects, Buckets: buckets, Policy: policies, Enc: codec}),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Verifier: verifier, Objects: objects, Buckets: buckets, Policy: policies, Enc: codec}),
	)
	s3Srv := server.New(server.Deps{
		Routes: []httpx.Routes{
			s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
			metricsRoutes,
			s3API,
		},
		Middlewares: []middleware.Middleware{
			middleware.NewRequestID(ids),
			middleware.NewRecover(logger, middleware.S3ErrorWriter(codec)),
			middleware.NewLogging(logger.With("server", "s3"), middleware.WithLogLevel(slog.LevelDebug)),
			s3Metrics,
			middleware.NewCORS(middleware.CORSOptions{}),
			middleware.NewAuth(verifier, middleware.S3ErrorWriter(codec)),
			middleware.NewAudit(auditor, middleware.WithAuditClock(o.clock)),
		},
		Logger: logger,
	}, serverOptions(cfg, cfg.Address, "s3")...)

	// Console + Admin API server (:9001).
	consoleMetrics, err := middleware.NewMetrics(registry, "console", middleware.WithMetricsClock(o.clock))
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
			console.NewSPARoutes(console.SPADeps{Assets: console.NewAssets(o.assets), Enc: codec}),
		},
		Middlewares: []middleware.Middleware{
			middleware.NewRequestID(ids),
			middleware.NewRecover(logger, middleware.ProblemErrorWriter(codec)),
			middleware.NewLogging(logger.With("server", "console"), middleware.WithLogLevel(slog.LevelDebug)),
			consoleMetrics,
			middleware.NewMaxBody(64 << 20),
		},
		Logger: logger,
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
// layout order. It returns the drives and an index by endpoint; on error the
// drives opened so far are closed.
func openDrives(deps storage.Deps, layout *topology.Layout) ([]storage.StorageAPI, map[string]storage.StorageAPI, error) {
	var drives []storage.StorageAPI
	byEndpoint := make(map[string]storage.StorageAPI)
	for _, ep := range layout.LocalEndpoints() {
		if ep.IsURL() {
			continue
		}
		d, err := storage.NewLocal(deps, ep)
		if err != nil {
			for _, opened := range drives {
				_ = opened.Close(context.Background())
			}
			return nil, nil, fmt.Errorf("open drive %s: %w", ep, err)
		}
		drives = append(drives, d)
		byEndpoint[ep.String()] = d
	}
	if len(drives) == 0 {
		return nil, nil, fmt.Errorf("%w: no local drives in the deployment", domain.ErrInvalidArgument)
	}
	return drives, byEndpoint, nil
}

// drivesOfSets maps the erasure sets of a pool to their opened drives.
func drivesOfSets(pool topology.Pool, byEndpoint map[string]storage.StorageAPI) [][]storage.StorageAPI {
	sets := pool.Sets()
	out := make([][]storage.StorageAPI, len(sets))
	for i, eps := range sets {
		out[i] = make([]storage.StorageAPI, len(eps))
		for j, ep := range eps {
			out[i][j] = byEndpoint[ep.String()]
		}
	}
	return out
}
