package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
)

func testDeps(env map[string]string, client *http.Client) (cmdDeps, *bytes.Buffer, *bytes.Buffer) {
	out, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	if client == nil {
		client = http.DefaultClient
	}
	return cmdDeps{
		stdout: out, stderr: errBuf, client: client,
		lookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	}, out, errBuf
}

func TestVersionCommand(t *testing.T) {
	deps, out, _ := testDeps(nil, nil)
	cmd := newRootCmd(deps)
	cmd.SetArgs([]string{"version"})
	require.NoError(t, cmd.Execute())
	assert.True(t, strings.HasPrefix(out.String(), "nexo dev"), out.String())

	out.Reset()
	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"version", "--json"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), `"version": "dev"`)
}

// covers: S-OPS-03, S-OPS-13
func TestHealthcheckCommand(t *testing.T) {
	healthy := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nexo/health/live" && healthy {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	deps, out, _ := testDeps(map[string]string{"NEXO_CONSOLE_ADDRESS": addr}, srv.Client())
	cmd := newRootCmd(deps)
	cmd.SetArgs([]string{"healthcheck"})
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "ok:")

	healthy = false
	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"healthcheck"})
	assert.Error(t, cmd.Execute())

	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"healthcheck", "--url", srv.URL + "/nexo/health/live"})
	assert.Error(t, cmd.Execute(), "explicit URL is used as is")

	u, err := healthURL("http", ":9001", "/nexo/health/live")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9001/nexo/health/live", u)
	u, err = healthURL("https", "[::]:9001", "/nexo/health/ready")
	require.NoError(t, err)
	assert.Equal(t, "https://127.0.0.1:9001/nexo/health/ready", u)
	_, err = healthURL("http", "9001", "/x")
	assert.Error(t, err)
}

// covers: S-OPS-14
func TestServerDryRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	deps, out, _ := testDeps(map[string]string{
		"NEXO_ROOT_USER":     "admin",
		"NEXO_ROOT_PASSWORD": "password123",
	}, nil)
	cmd := newRootCmd(deps)
	cmd.SetArgs([]string{"server", "--dry-run", "--address", "127.0.0.1:0", "--console-address", "127.0.0.1:0", dir})
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	assert.Contains(t, out.String(), "configuration OK: mode=snsd")

	// Missing credentials fail validation.
	deps, _, _ = testDeps(nil, nil)
	cmd = newRootCmd(deps)
	cmd.SetArgs([]string{"server", "--dry-run", dir})
	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root user")

	// Custom assets provider is accepted.
	deps, _, _ = testDeps(map[string]string{"NEXO_ROOT_USER": "admin", "NEXO_ROOT_PASSWORD": "password123"}, nil)
	custom := newServerCmdWith(deps, func() (fs.FS, bool) { return nil, false })
	custom.SetArgs([]string{"--dry-run", "--address", "127.0.0.1:0", "--console-address", "127.0.0.1:0", dir})
	require.NoError(t, custom.ExecuteContext(context.Background()))
}

// selfSigned writes a self-signed key pair valid in [notBefore, notAfter]
// into dir/public.crt and dir/private.key.
func selfSigned(t *testing.T, dir string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nexo.test"},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "private.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
}

// freePort returns a free loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestServerDryRunMatrix is TM-12: nexo server --dry-run on valid and
// invalid configurations, without starting nor writing to the drives.
//
// covers: S-OPS-14, S-STO-00
func TestServerDryRunMatrix(t *testing.T) {
	creds := map[string]string{"NEXO_ROOT_USER": "admin", "NEXO_ROOT_PASSWORD": "password123"}
	now := time.Now()
	tests := []struct {
		name string
		// setup prepares the directory and returns the arguments.
		setup   func(t *testing.T, dir string) []string
		env     map[string]string
		wantOut []string
		wantErr string
	}{
		{
			name:    "snsd",
			setup:   func(_ *testing.T, dir string) []string { return []string{filepath.Join(dir, "data")} },
			wantOut: []string{"configuration OK: mode=snsd", "local drives: 1 writable, 0 formatted"},
		},
		{
			name:    "snmd with ellipsis",
			setup:   func(_ *testing.T, dir string) []string { return []string{filepath.Join(dir, "d{1...4}")} },
			wantOut: []string{"mode=snmd", "pool 1: 1 erasure set(s) of 4 drive(s), parity 2", "local drives: 4 writable"},
		},
		{
			name: "cluster node recognized by its port",
			setup: func(t *testing.T, dir string) []string {
				port := freePort(t)
				var eps []string
				for n, p := range []int{port, 1, 2, 3} {
					for d := 1; d <= 2; d++ {
						eps = append(eps, fmt.Sprintf("http://127.0.0.1:%d%s/n%d/d%d", p, filepath.ToSlash(dir), n+1, d))
					}
				}
				return append([]string{"--address", fmt.Sprintf("127.0.0.1:%d", port)}, eps...)
			},
			wantOut: []string{"mode=distributed", "(4 node(s))", "pool 1: 1 erasure set(s) of 8 drive(s), parity 4", "local drives: 2 writable"},
		},
		{
			name: "cluster without a local endpoint",
			setup: func(_ *testing.T, dir string) []string {
				return []string{"http://nexo-node{1...4}.invalid:9000" + filepath.ToSlash(dir) + "/d{1...2}"}
			},
			wantErr: "local",
		},
		{
			name:    "missing credentials",
			setup:   func(_ *testing.T, dir string) []string { return []string{filepath.Join(dir, "data")} },
			env:     map[string]string{},
			wantErr: "root user",
		},
		{
			name: "drive is a file",
			setup: func(t *testing.T, dir string) []string {
				file := filepath.Join(dir, "file")
				require.NoError(t, os.WriteFile(file, nil, 0o600))
				return []string{file}
			},
			wantErr: "not a directory",
		},
		{
			name: "port in use",
			setup: func(t *testing.T, dir string) []string {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				t.Cleanup(func() { _ = ln.Close() })
				return []string{"--address", ln.Addr().String(), filepath.Join(dir, "data")}
			},
			wantErr: "s3 address",
		},
		{
			name: "storage class beyond the set",
			setup: func(_ *testing.T, dir string) []string {
				return []string{"--storage-class-standard", "EC:3", filepath.Join(dir, "d{1...4}")}
			},
			wantErr: "at most EC:2",
		},
		{
			name: "malformed storage class",
			setup: func(_ *testing.T, dir string) []string {
				return []string{"--storage-class-rrs", "RS:1", filepath.Join(dir, "d{1...4}")}
			},
			wantErr: "EC:<parity>",
		},
		{
			name: "drives formatted for another topology",
			setup: func(t *testing.T, dir string) []string {
				cfg := config.Default()
				cfg.RootUser, cfg.RootPassword = "admin", "password123"
				cfg.Address, cfg.ConsoleAddress = "127.0.0.1:0", "127.0.0.1:0"
				cfg.Endpoints = []string{filepath.Join(dir, "d1")}
				a, err := app.Build(context.Background(), cfg, app.WithLogger(slog.New(slog.DiscardHandler)))
				require.NoError(t, err)
				require.NoError(t, a.Stop(context.Background()))
				return []string{filepath.Join(dir, "d{1...4}")}
			},
			wantErr: "formatted for another topology",
		},
		{
			name: "valid certificate",
			setup: func(t *testing.T, dir string) []string {
				selfSigned(t, dir, now.Add(-time.Hour), now.Add(10*24*time.Hour))
				return []string{"--certs-dir", dir, filepath.Join(dir, "data")}
			},
			wantOut: []string{"configuration OK", "warning: the certificate \"nexo.test\" expires on"},
		},
		{
			name: "expired certificate",
			setup: func(t *testing.T, dir string) []string {
				selfSigned(t, dir, now.Add(-48*time.Hour), now.Add(-time.Hour))
				return []string{"--certs-dir", dir, filepath.Join(dir, "data")}
			},
			wantErr: "expired",
		},
		{
			name: "missing certificate",
			setup: func(_ *testing.T, dir string) []string {
				return []string{"--certs-dir", filepath.Join(dir, "nothing"), filepath.Join(dir, "data")}
			},
			wantErr: "certificates",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			args := tt.setup(t, dir)
			env := creds
			if tt.env != nil {
				env = tt.env
			}
			deps, out, _ := testDeps(env, nil)
			cmd := newRootCmd(deps)
			base := []string{"server", "--dry-run", "--console-address", "127.0.0.1:0"}
			if !slices.Contains(args, "--address") {
				base = append(base, "--address", "127.0.0.1:0")
			}
			cmd.SetArgs(append(base, args...))
			err := cmd.ExecuteContext(context.Background())
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			for _, want := range tt.wantOut {
				assert.Contains(t, out.String(), want)
			}
			// Nothing was written: the drives are not even created.
			assert.NoDirExists(t, filepath.Join(dir, "data"))
			assert.NoDirExists(t, filepath.Join(dir, "d1"))
			assert.NoDirExists(t, filepath.Join(dir, "n1"))
		})
	}
}
