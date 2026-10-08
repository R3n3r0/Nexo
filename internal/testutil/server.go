// Package testutil starts Nexo in-process for integration tests: free
// ports, a temporary drive directory and test credentials. The Server
// exposes the services of the running application (IAM, policies, buckets,
// share links, signer) so that tests provision identities and policies
// without going through the Admin API, and can stop and restart the
// application on the same data directory.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// Test credentials.
const (
	RootUser     = "nexoadmin"
	RootPassword = "nexoadmin-secret"
	// Domain is the virtual-host domain of the test servers (NEXO_DOMAIN):
	// "{bucket}.localhost" addresses a bucket.
	Domain = "localhost"
)

// stopTimeout bounds the shutdown of a test server.
const stopTimeout = 10 * time.Second

// startAttempts is how many times StartServer retries when a reserved port
// was taken by another process between its reservation and the listen.
const startAttempts = 3

// Server is a running in-process Nexo.
type Server struct {
	App *app.App
	// S3URL and ConsoleURL are the base URLs (http://127.0.0.1:port). They
	// survive Restart: the server listens on the same ports again.
	S3URL      string
	ConsoleURL string
	// Client is an HTTP client with a cookie jar for console sessions.
	Client *http.Client
	// Dir is the temporary directory holding the data drive ("data").
	Dir          string
	RootUser     string
	RootPassword string
	Config       config.Static

	// Services of the running application, refreshed by Restart.
	IAM      iam.Service
	Policies policy.Service
	Buckets  bucket.Service
	Shares   share.Service
	Signer   auth.Signer
	// Clock is the clock of the application (WithClock); nil means the
	// wall clock.
	Clock clock.Clock

	logger *slog.Logger
	mu     sync.Mutex
	// running reports that App is started and not yet stopped.
	running bool
}

// settings collects the options of StartServer.
type settings struct {
	cfg   config.Static
	clock clock.Clock
	dir   string
}

// Option customizes StartServer.
type Option func(*settings)

// WithEndpoints replaces the single temporary drive with the given
// endpoints (e.g. four directories for SNMD).
func WithEndpoints(eps ...string) Option {
	return func(s *settings) { s.cfg.Endpoints = eps }
}

// WithConfig applies an arbitrary mutation to the configuration.
func WithConfig(fn func(*config.Static)) Option {
	return func(s *settings) { fn(&s.cfg) }
}

// WithClock sets the clock of the application (e.g. a clock.Fake to expire
// presigned URLs and STS sessions without waiting).
func WithClock(c clock.Clock) Option {
	return func(s *settings) { s.clock = c }
}

// WithDir uses dir instead of a fresh temporary directory: the drive is
// dir/data. It lets a test start a second server on the data of a first
// one.
func WithDir(dir string) Option {
	return func(s *settings) { s.dir = dir }
}

// StartServer builds and starts the application on free ports and stops it
// when the test ends. Logs go to the test log at debug level.
func StartServer(t testing.TB, opts ...Option) *Server {
	t.Helper()
	st := settings{cfg: config.Default()}
	st.cfg.RootUser = RootUser
	st.cfg.RootPassword = RootPassword
	st.cfg.LogLevel = "debug"
	st.cfg.Domain = Domain
	for _, o := range opts {
		o(&st)
	}
	if st.dir == "" {
		st.dir = t.TempDir()
	}
	if len(st.cfg.Endpoints) == 0 {
		st.cfg.Endpoints = []string{filepath.Join(st.dir, "data")}
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("testutil: cookie jar: %v", err)
	}
	srv := &Server{
		Client:       &http.Client{Timeout: 10 * time.Second, Jar: jar},
		Dir:          st.dir,
		RootUser:     st.cfg.RootUser,
		RootPassword: st.cfg.RootPassword,
		Clock:        st.clock,
		logger:       slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	t.Cleanup(func() {
		if err := srv.stop(); err != nil {
			t.Errorf("testutil: stop app: %v", err)
		}
	})

	// The ports are reserved before Build so that the configuration names
	// them: the public URL of the share links and the restarts use them.
	for attempt := 1; ; attempt++ {
		cfg := st.cfg
		ports := reservePorts(t, 2)
		cfg.Address, cfg.ConsoleAddress = ports[0], ports[1]
		srv.Config = cfg
		err := srv.start()
		if err == nil {
			break
		}
		if attempt == startAttempts || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("testutil: %v", err)
		}
	}
	return srv
}

// Stop stops the application; the data directory is kept, Restart starts
// it again. Stopping a stopped server is a no-op.
func (s *Server) Stop(t testing.TB) {
	t.Helper()
	if err := s.stop(); err != nil {
		t.Fatalf("testutil: stop app: %v", err)
	}
}

// Restart stops the application (when running) and starts a new one on
// the same data directory, configuration and ports, as a process restart
// would. The service fields are refreshed: clients built from S3URL keep
// working.
func (s *Server) Restart(t testing.TB) {
	t.Helper()
	s.Stop(t)
	if err := s.start(); err != nil {
		t.Fatalf("testutil: restart: %v", err)
	}
}

// start builds and starts the application of s.Config.
func (s *Server) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	appOpts := []app.Option{app.WithLogger(s.logger), app.WithStopTimeout(stopTimeout)}
	if s.Clock != nil {
		appOpts = append(appOpts, app.WithClock(s.Clock))
	}
	ctx := context.Background()
	application, err := app.Build(ctx, s.Config, appOpts...)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	if err := application.Start(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		_ = application.Stop(stopCtx)
		return fmt.Errorf("start app: %w", err)
	}
	s.App = application
	s.S3URL = "http://" + application.S3Addr()
	s.ConsoleURL = "http://" + application.ConsoleAddr()
	s.IAM = application.IAM()
	s.Policies = application.Policies()
	s.Buckets = application.Buckets()
	s.Shares = application.Shares()
	s.Signer = application.Signer()
	s.running = true
	return nil
}

// stop stops the running application, if any.
func (s *Server) stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return nil
	}
	s.running = false
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return s.App.Stop(ctx)
}

// reservePorts returns n distinct free loopback addresses
// ("127.0.0.1:port"): the listeners are held together so that the kernel
// hands out different ports, then released for the server.
func reservePorts(t testing.TB, n int) []string {
	t.Helper()
	addrs := make([]string, 0, n)
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("testutil: reserve port: %v", err)
		}
		listeners = append(listeners, l)
		addrs = append(addrs, net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Addr().(*net.TCPAddr).Port)))
	}
	return addrs
}

// testWriter routes log output to the test log, ignoring writes after the
// test finished.
type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	defer func() { _ = recover() }()
	w.t.Logf("%s", p)
	return len(p), nil
}
