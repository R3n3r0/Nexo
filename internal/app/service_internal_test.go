package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
)

func TestProcessActions(t *testing.T) {
	tests := []struct {
		name        string
		running     bool
		action      adminsvc.Action
		sigErr      error
		want        os.Signal
		wantRestart bool
		wantErr     error
	}{
		{name: "outside Run", action: adminsvc.ActionStop, wantErr: domain.ErrNotImplemented},
		{name: "restart outside Run", action: adminsvc.ActionRestart, wantErr: domain.ErrNotImplemented},
		{name: "stop", running: true, action: adminsvc.ActionStop, want: syscall.SIGTERM},
		// SIGHUP reloads the certificates: a restart is a request to Run.
		{name: "restart", running: true, action: adminsvc.ActionRestart, wantRestart: true},
		{name: "signal failure", running: true, action: adminsvc.ActionStop, sigErr: errors.New("no"), wantErr: errors.New("no")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			running := &atomic.Bool{}
			running.Store(tt.running)
			var got os.Signal
			restarted := false
			p := processActions{running: running, signal: func(s os.Signal) error { got = s; return tt.sigErr },
				restart: func() error { restarted = true; return nil }}
			err := p.Handle(context.Background(), tt.action)
			if tt.wantErr != nil {
				require.Error(t, err)
				if errors.Is(tt.wantErr, domain.ErrNotImplemented) {
					assert.ErrorIs(t, err, domain.ErrNotImplemented)
					assert.Nil(t, got, "no signal outside Run")
					assert.False(t, restarted, "no restart outside Run")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantRestart, restarted)
		})
	}
}

// freeAddr returns a free loopback address.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Addr().(*net.TCPAddr).Port))
}

// startCounter is a slog.Handler counting the starts of the application
// ("nexo started"): the instance that answers is the restarted one only
// once there were two.
type startCounter struct{ starts *atomic.Int32 }

func (startCounter) Enabled(context.Context, slog.Level) bool { return true }

func (c startCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "nexo started" {
		c.starts.Add(1)
	}
	return nil
}

func (c startCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c startCounter) WithGroup(string) slog.Handler      { return c }

// buildRestartable builds a single-drive App on free loopback ports whose
// starts are counted.
func buildRestartable(t *testing.T) (*App, config.Static, *atomic.Int32) {
	t.Helper()
	cfg := config.Default()
	cfg.RootUser, cfg.RootPassword = "admin", "password123"
	cfg.Endpoints = []string{filepath.Join(t.TempDir(), "data")}
	cfg.Address, cfg.ConsoleAddress = freeAddr(t), freeAddr(t)
	starts := &atomic.Int32{}
	a, err := Build(context.Background(), cfg, WithLogger(slog.New(startCounter{starts: starts})), WithStopTimeout(5*time.Second))
	require.NoError(t, err)
	return a, cfg, starts
}

// TestRunRestartsOnServiceRestart drives POST /service/restart through the
// admin service of a running App: Run stops every component and serves
// again from the same configuration, then stops on cancellation.
func TestRunRestartsOnServiceRestart(t *testing.T) {
	a, cfg, starts := buildRestartable(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, a.running.Load, 10*time.Second, 10*time.Millisecond)

	ready := func() bool {
		resp, err := http.Get("http://" + cfg.ConsoleAddress + domain.HealthLivePath)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	require.Eventually(t, ready, 10*time.Second, 10*time.Millisecond)
	// The admin service asks for the restart the way the Admin API does.
	adm := adminsvc.New(adminsvc.Deps{Actions: processActions{running: a.running, signal: signalSelf, restart: a.requestRestart}})
	require.NoError(t, adm.ServiceAction(context.Background(), adminsvc.ActionRestart))
	require.Eventually(t, func() bool { return !a.running.Load() }, 10*time.Second, 5*time.Millisecond, "the first instance stops")
	// Not the first instance still closing its listeners (regression: the
	// test canceled Run during the restart).
	require.Eventually(t, func() bool { return starts.Load() == 2 }, 10*time.Second, 5*time.Millisecond, "the application starts again")
	require.Eventually(t, ready, 10*time.Second, 20*time.Millisecond, "the restarted instance serves again")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestRunShutdownDuringRestart: a shutdown requested while the application
// restarts (SIGTERM, a canceled context) stops it without an error
// (regression: Run returned "app: restart: ... context canceled").
func TestRunShutdownDuringRestart(t *testing.T) {
	a, _, _ := buildRestartable(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, a.running.Load, 10*time.Second, 10*time.Millisecond)

	require.NoError(t, a.requestRestart())
	require.Eventually(t, func() bool { return !a.running.Load() }, 10*time.Second, time.Millisecond, "the restart begins")
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestProcessExecutable: the update service may replace the process only
// while Run drives it and the command gave the path of its binary.
//
// covers: S-OPS-09
func TestProcessExecutable(t *testing.T) {
	running := &atomic.Bool{}
	execs := 0
	p := processActions{running: running, exe: "/opt/nexo/nexo", exec: func() error { execs++; return nil },
		restart: func() error { return nil }, signal: func(os.Signal) error { return nil }}
	_, err := p.Executable()
	require.ErrorIs(t, err, domain.ErrNotImplemented, "outside Run")
	require.ErrorIs(t, p.Exec(), domain.ErrNotImplemented)
	running.Store(true)
	exe, err := p.Executable()
	require.NoError(t, err)
	assert.Equal(t, "/opt/nexo/nexo", exe)
	require.NoError(t, p.Exec())
	assert.Equal(t, 1, execs)
	require.NoError(t, p.Restart())
	require.NoError(t, p.Stop())
	p.exe = ""
	_, err = p.Executable()
	require.ErrorIs(t, err, domain.ErrNotImplemented, "without the path of the binary")
}

// TestRunReturnsErrExec: once an update installed a new binary, Run stops
// every component and returns ErrExec, without building the application
// again: the command replaces the process.
//
// covers: S-OPS-09
func TestRunReturnsErrExec(t *testing.T) {
	a, _, starts := buildRestartable(t)
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	require.Eventually(t, a.running.Load, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, a.requestExec())
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrExec)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
	assert.Equal(t, int32(1), starts.Load(), "the application is not built again")
}
