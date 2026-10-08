// Package testutil starts Nexo in-process for integration tests: free
// ports, a temporary drive directory and test credentials.
package testutil

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"testing"
	"time"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
)

// Test credentials.
const (
	RootUser     = "nexoadmin"
	RootPassword = "nexoadmin-secret"
)

// Server is a running in-process Nexo.
type Server struct {
	App *app.App
	// S3URL and ConsoleURL are the base URLs (http://127.0.0.1:port).
	S3URL      string
	ConsoleURL string
	// Client is an HTTP client with a cookie jar for console sessions.
	Client *http.Client
	// Dir is the temporary data directory.
	Dir          string
	RootUser     string
	RootPassword string
	Config       config.Static
}

// Option customizes StartServer.
type Option func(*config.Static)

// WithEndpoints replaces the single temporary drive with the given
// endpoints (e.g. four directories for SNMD).
func WithEndpoints(eps ...string) Option {
	return func(c *config.Static) { c.Endpoints = eps }
}

// WithConfig applies an arbitrary mutation to the configuration.
func WithConfig(fn func(*config.Static)) Option {
	return func(c *config.Static) { fn(c) }
}

// StartServer builds and starts the application on free ports and stops it
// when the test ends. Logs go to the test log at debug level.
func StartServer(t testing.TB, opts ...Option) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Address = "127.0.0.1:0"
	cfg.ConsoleAddress = "127.0.0.1:0"
	cfg.RootUser = RootUser
	cfg.RootPassword = RootPassword
	cfg.LogLevel = "debug"
	cfg.Endpoints = []string{filepath.Join(dir, "data")}
	for _, o := range opts {
		o(&cfg)
	}

	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()
	application, err := app.Build(ctx, cfg, app.WithLogger(logger), app.WithStopTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("testutil: build app: %v", err)
	}
	if err := application.Start(ctx); err != nil {
		t.Fatalf("testutil: start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := application.Stop(stopCtx); err != nil {
			t.Errorf("testutil: stop app: %v", err)
		}
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("testutil: cookie jar: %v", err)
	}
	return &Server{
		App:          application,
		S3URL:        "http://" + application.S3Addr(),
		ConsoleURL:   "http://" + application.ConsoleAddr(),
		Client:       &http.Client{Timeout: 10 * time.Second, Jar: jar},
		Dir:          dir,
		RootUser:     cfg.RootUser,
		RootPassword: cfg.RootPassword,
		Config:       cfg,
	}
}

// testWriter routes log output to the test log, ignoring writes after the
// test finished.
type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	defer func() { _ = recover() }()
	w.t.Logf("%s", p)
	return len(p), nil
}
