package preflight_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/preflight"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func newChecks(t *testing.T) preflight.Service {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	s, err := preflight.New(preflight.Deps{Topology: topology.New(topology.Deps{Logger: logger}), Clock: clock.NewFake(now), Logger: logger})
	require.NoError(t, err)
	return s
}

// selfSigned writes a self-signed key pair valid in [notBefore, notAfter].
func selfSigned(t *testing.T, dir string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nexo.test"}, NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "private.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
}

// caSigned writes a key pair of dir issued by the CA "Corp Nodes CA".
func caSigned(t *testing.T, dir string, notBefore, notAfter time.Time) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Corp Nodes CA"}, NotBefore: notBefore,
		NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "node1.test"}, NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "private.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
}

func TestNewRequiresTopology(t *testing.T) {
	_, err := preflight.New(preflight.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })

	tests := []struct {
		name    string
		input   func(t *testing.T, dir string) preflight.Input
		check   func(t *testing.T, rep *preflight.Report)
		wantErr string
	}{
		{name: "single drive", input: func(_ *testing.T, dir string) preflight.Input {
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}}
		}, check: func(t *testing.T, rep *preflight.Report) {
			assert.Equal(t, domain.ModeSNSD, rep.Mode)
			assert.Equal(t, "local", rep.NodeID)
			assert.Len(t, rep.LocalDrives, 1)
			assert.Zero(t, rep.Formatted)
		}},
		{name: "erasure set and storage class", input: func(_ *testing.T, dir string) preflight.Input {
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "d{1...4}")}, StorageClassStandard: "EC:2", StorageClassRRS: "EC:1"}
		}, check: func(t *testing.T, rep *preflight.Report) {
			assert.Equal(t, domain.ModeSNMD, rep.Mode)
			assert.Equal(t, []domain.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 4, Parity: 2}}, rep.Pools)
		}},
		{name: "parity beyond the set", input: func(_ *testing.T, dir string) preflight.Input {
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "d{1...4}")}, StorageClassStandard: "EC:3"}
		}, wantErr: "at most EC:2"},
		{name: "drive is a file", input: func(t *testing.T, dir string) preflight.Input {
			file := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(file, nil, 0o600))
			return preflight.Input{Endpoints: []string{file}}
		}, wantErr: "not a directory"},
		{name: "address in use", input: func(_ *testing.T, dir string) preflight.Input {
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, Listen: []preflight.Address{{Name: "s3 address", Addr: busy.Addr().String()}}}
		}, wantErr: "s3 address"},
		{name: "certificate expiring soon", input: func(t *testing.T, dir string) preflight.Input {
			selfSigned(t, dir, now.Add(-time.Hour), now.Add(10*24*time.Hour))
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, CertsDir: dir}
		}, check: func(t *testing.T, rep *preflight.Report) {
			require.Len(t, rep.Warnings, 1)
			assert.Contains(t, rep.Warnings[0], `the certificate "nexo.test" expires on`)
		}},
		{name: "internode verify without the CA of the nodes", input: func(t *testing.T, dir string) preflight.Input {
			caSigned(t, dir, now.Add(-time.Hour), now.Add(365*24*time.Hour))
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, CertsDir: dir, InternodeVerify: true}
		}, check: func(t *testing.T, rep *preflight.Report) {
			require.Len(t, rep.Warnings, 1)
			assert.Contains(t, rep.Warnings[0], `--tls-internode-verify: CAs/ holds no CA, so the nodes whose certificate is issued by "Corp Nodes CA" are refused`)
		}},
		{name: "internode verify with the CA in CAs", input: func(t *testing.T, dir string) preflight.Input {
			caSigned(t, dir, now.Add(-time.Hour), now.Add(365*24*time.Hour))
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "CAs"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "CAs", "ca.crt"), []byte("ca"), 0o600))
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, CertsDir: dir, InternodeVerify: true}
		}, check: func(t *testing.T, rep *preflight.Report) {
			assert.Empty(t, rep.Warnings)
		}},
		{name: "internode verify with a self-signed certificate", input: func(t *testing.T, dir string) preflight.Input {
			selfSigned(t, dir, now.Add(-time.Hour), now.Add(365*24*time.Hour))
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, CertsDir: dir, InternodeVerify: true}
		}, check: func(t *testing.T, rep *preflight.Report) {
			assert.Empty(t, rep.Warnings, "the default certificate verifies itself")
		}},
		{name: "certificate expired", input: func(t *testing.T, dir string) preflight.Input {
			selfSigned(t, dir, now.Add(-48*time.Hour), now.Add(-time.Hour))
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, CertsDir: dir}
		}, wantErr: "expired"},
		{name: "certificate not yet valid", input: func(t *testing.T, dir string) preflight.Input {
			selfSigned(t, dir, now.Add(time.Hour), now.Add(48*time.Hour))
			return preflight.Input{Endpoints: []string{filepath.Join(dir, "data")}, CertsDir: dir}
		}, wantErr: "is valid from"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := newChecks(t).Check(ctx, tc.input(t, t.TempDir()))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			tc.check(t, rep)
		})
	}
}

func TestCheckTopologyError(t *testing.T) {
	rep, err := newChecks(t).Check(context.Background(), preflight.Input{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "topology")
	assert.Nil(t, rep)
	// The error of the topology is not prefixed a second time
	// (regression: "topology: invalid argument: topology: ...").
	_, err = newChecks(t).Check(context.Background(), preflight.Input{Endpoints: []string{"./data"}})
	require.Error(t, err)
	assert.Equal(t, 1, strings.Count(err.Error(), "topology:"), err.Error())
	assert.Contains(t, err.Error(), "drive path must be absolute")
}
