// Package app composes the Nexo server: it wires service implementations to
// their interfaces according to the configuration and the detected
// deployment mode, builds the S3 and console HTTP servers and runs them as
// a lifecycle.Group. It is the only package that knows about concrete
// implementations.
package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/session"
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
	assets   console.Assets
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

// WithAssets sets the console assets (default: none embedded).
func WithAssets(a console.Assets) Option {
	return func(o *options) {
		if a != nil {
			o.assets = a
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
	cfg      config.Static
	layout   *topology.Layout
	logger   *slog.Logger
	group    *lifecycle.Group
	s3Srv    server.Service
	consSrv  server.Service
	stopWait time.Duration
}

// Build composes the application. It does not open sockets or touch the
// drives: that happens in Start.
func Build(ctx context.Context, cfg config.Static, opts ...Option) (*App, error) {
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
	localEndpoints := localDrives(layout)
	if err := ensureLocalDrives(localEndpoints); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	logger.Info("deployment layout", "mode", layout.Mode.String(), "pools", len(layout.Pools), "local_drives", len(localEndpoints))

	// Cross-cutting services.
	registry := metrics.New(metrics.Deps{})
	auditor := audit.NewSlog(audit.Deps{Logger: logger.With("component", "audit"), Clock: o.clock})
	_ = auditor // TODO(M1): wire the audit middleware once the S3 handlers exist.
	locks := lock.NewLocal(lock.Deps{})
	_ = locks // TODO(M1): consumed by the object layer.
	dynConfig := configsvc.New(configsvc.Deps{Store: configsvc.NewMemoryStore(nil), Logger: logger})
	events := event.NewDispatcher(event.Deps{Logger: logger.With("component", "events")})
	healthSvc := health.New(health.Deps{Readiness: map[string]health.Check{
		// TODO(M1): replace with the object layer's quorum check.
		"drives": func(context.Context) error { return ensureLocalDrives(localEndpoints) },
	}})

	// Identity, sessions, administration.
	//
	// TODO(M2): storage.NewLocal / object.NewFS / object.NewErasure,
	// bucket.New, auth.NewSigV4, iam.NewStore, policy.New replace the stubs.
	identities := iam.NewRootOnly(iam.Deps{Clock: o.clock, Logger: logger})
	if err := identities.Bootstrap(ctx, domain.Credentials{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword}); err != nil {
		return nil, fmt.Errorf("app: bootstrap root credentials: %w", err)
	}
	sessions, err := session.New(session.Deps{IAM: identities, Clock: o.clock, Logger: logger},
		session.WithSigningKey(sessionKey(cfg)))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	build := version.Get()
	adminSvc := adminsvc.New(adminsvc.Deps{
		Clock:     o.clock,
		Logger:    logger,
		Build:     adminsvc.BuildInfo{Version: build.Version, Commit: build.Commit, Date: build.Date},
		Mode:      layout.Mode,
		Region:    cfg.Region,
		Endpoints: localEndpoints,
		Pools:     poolInfos(layout),
	})

	codec := httpx.New()
	ids := httpx.NewIDGenerator()

	// S3 server (:9000).
	s3Metrics, err := middleware.NewMetrics(registry, "s3")
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	s3API := s3.NewAPI(codec,
		s3.NewBucketRoutes(s3.BucketDeps{Enc: codec}),
		s3.NewObjectRoutes(s3.ObjectDeps{Enc: codec}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Enc: codec}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Enc: codec}),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Enc: codec}),
	)
	s3Srv := server.New(server.Deps{
		Routes: []httpx.Routes{
			s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
			s3.NewMetricsRoutes(s3.MetricsDeps{Registry: registry}),
			s3API,
		},
		Middlewares: []middleware.Middleware{
			middleware.NewRequestID(ids),
			middleware.NewRecover(logger, middleware.S3ErrorWriter(codec)),
			middleware.NewLogging(logger.With("server", "s3"), middleware.WithLogLevel(slog.LevelDebug)),
			s3Metrics,
			middleware.NewCORS(middleware.CORSOptions{}),
			// TODO(M2): middleware.NewAuth(sigv4Verifier, middleware.S3ErrorWriter(codec)).
		},
		Logger: logger,
	}, serverOptions(cfg, cfg.Address, "s3")...)

	// Console + Admin API server (:9001).
	consoleMetrics, err := middleware.NewMetrics(registry, "console")
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	adminAPI := adminhttp.NewAPI(codec,
		adminhttp.NewSessionRoutes(adminhttp.SessionDeps{Sessions: sessions, Codec: codec, Clock: o.clock}),
		adminhttp.NewInfoRoutes(adminhttp.InfoDeps{Admin: adminSvc, Sessions: sessions, Enc: codec}),
		adminhttp.NewUserRoutes(adminhttp.UserDeps{IAM: identities, Enc: codec}),
		adminhttp.NewPolicyRoutes(adminhttp.PolicyDeps{Enc: codec}),
		adminhttp.NewBucketRoutes(adminhttp.BucketDeps{Enc: codec}),
		adminhttp.NewConfigRoutes(adminhttp.ConfigDeps{Config: dynConfig, Enc: codec}),
	)
	consSrv := server.New(server.Deps{
		Routes: []httpx.Routes{
			s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
			adminAPI,
			console.NewSPARoutes(console.SPADeps{Assets: o.assets, Enc: codec}),
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

	group := lifecycle.NewGroup(lifecycle.WithLogger(logger), lifecycle.WithStopTimeout(o.stopWait))
	group.Add("events", events)
	group.Add("s3", s3Srv)
	group.Add("console", consSrv)

	return &App{
		cfg:      cfg,
		layout:   layout,
		logger:   logger,
		group:    group,
		s3Srv:    s3Srv,
		consSrv:  consSrv,
		stopWait: o.stopWait,
	}, nil
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

func serverOptions(cfg config.Static, addr, name string) []server.Option {
	opts := []server.Option{server.WithAddr(addr), server.WithName(name)}
	if cfg.TLSEnabled() {
		opts = append(opts, server.WithTLS(filepath.Join(cfg.CertsDir, "public.crt"), filepath.Join(cfg.CertsDir, "private.key")))
	}
	return opts
}

// sessionKey derives the session signing key from the root credentials so
// that every node of a deployment accepts the same cookies.
func sessionKey(cfg config.Static) []byte {
	sum := sha256.Sum256([]byte("nexo-console-session\x00" + cfg.RootUser + "\x00" + cfg.RootPassword))
	return sum[:]
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

func localDrives(layout *topology.Layout) []domain.Endpoint {
	var out []domain.Endpoint
	for _, p := range layout.Pools {
		for _, ep := range p.Endpoints {
			if ep.IsLocal {
				out = append(out, ep)
			}
		}
	}
	return out
}

// ensureLocalDrives checks that every local drive directory exists (creating
// missing ones) and is writable.
//
// TODO(M1): replaced by storage.NewLocal which also handles format.json.
func ensureLocalDrives(eps []domain.Endpoint) error {
	var errs []error
	for _, ep := range eps {
		if ep.IsURL() {
			continue
		}
		if err := os.MkdirAll(ep.Path, 0o750); err != nil {
			errs = append(errs, fmt.Errorf("drive %s: %w", ep.Path, err))
			continue
		}
		probe := filepath.Join(ep.Path, ".nexo.sys.probe")
		if err := os.WriteFile(probe, nil, 0o600); err != nil {
			errs = append(errs, fmt.Errorf("drive %s is not writable: %w", ep.Path, err))
			continue
		}
		_ = os.Remove(probe)
	}
	return errors.Join(errs...)
}
