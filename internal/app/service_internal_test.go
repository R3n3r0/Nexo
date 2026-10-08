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
		name    string
		running bool
		action  adminsvc.Action
		sigErr  error
		want    os.Signal
		wantErr error
	}{
		{name: "outside Run", action: adminsvc.ActionStop, wantErr: domain.ErrNotImplemented},
		{name: "stop", running: true, action: adminsvc.ActionStop, want: syscall.SIGTERM},
		{name: "restart", running: true, action: adminsvc.ActionRestart, want: syscall.SIGHUP},
		{name: "signal failure", running: true, action: adminsvc.ActionRestart, sigErr: errors.New("no"), wantErr: errors.New("no")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			running := &atomic.Bool{}
			running.Store(tt.running)
			var got os.Signal
			p := processActions{running: running, signal: func(s os.Signal) error { got = s; return tt.sigErr }}
			err := p.Handle(context.Background(), tt.action)
			if tt.wantErr != nil {
				require.Error(t, err)
				if errors.Is(tt.wantErr, domain.ErrNotImplemented) {
					assert.ErrorIs(t, err, domain.ErrNotImplemented)
					assert.Nil(t, got, "no signal outside Run")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
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

// TestRunRestartsOnServiceRestart drives POST /service/restart through the
// admin service of a running App: Run stops every component and serves
// again from the same configuration, then stops on cancellation.
func TestRunRestartsOnServiceRestart(t *testing.T) {
	cfg := config.Default()
	cfg.RootUser, cfg.RootPassword = "admin", "password123"
	cfg.Endpoints = []string{filepath.Join(t.TempDir(), "data")}
	cfg.Address, cfg.ConsoleAddress = freeAddr(t), freeAddr(t)
	a, err := Build(context.Background(), cfg, WithLogger(slog.New(slog.DiscardHandler)), WithStopTimeout(5*time.Second))
	require.NoError(t, err)

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
	adm := adminsvc.New(adminsvc.Deps{Actions: processActions{running: a.running, signal: signalSelf}})
	require.NoError(t, adm.ServiceAction(context.Background(), adminsvc.ActionRestart))
	require.Eventually(t, func() bool { return !a.running.Load() }, 10*time.Second, 5*time.Millisecond, "the first instance stops")
	require.Eventually(t, ready, 10*time.Second, 20*time.Millisecond, "the restarted instance serves again")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}
