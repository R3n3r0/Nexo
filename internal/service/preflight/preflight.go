package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// DefaultCertExpiryWarning is how close to its expiry a certificate is
// reported.
const DefaultCertExpiryWarning = 30 * 24 * time.Hour

// Deps are the dependencies of the preflight checks.
type Deps struct {
	// Topology parses the endpoints and checks the formats (required).
	Topology topology.Service
	// Clock dates the certificate checks (default: the wall clock).
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the preflight checks.
type Option func(*service)

// WithCertExpiryWarning sets how close to its expiry a certificate is
// reported (default DefaultCertExpiryWarning).
func WithCertExpiryWarning(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.certWarning = d
		}
	}
}

type service struct {
	deps        Deps
	certWarning time.Duration
}

var _ Service = (*service)(nil)

// New returns the preflight checks.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Topology == nil {
		return nil, fmt.Errorf("%w: preflight: the topology service is required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{deps: deps, certWarning: DefaultCertExpiryWarning}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Check implements Service.
func (s *service) Check(ctx context.Context, in Input) (*Report, error) {
	layout, err := s.deps.Topology.Parse(ctx, in.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	rep := &Report{Mode: layout.Mode, NodeID: "local", Nodes: len(layout.Nodes), Pools: layout.PoolInfos()}
	if n, ok := layout.LocalNode(); ok && layout.Mode == domain.ModeDistributed {
		rep.NodeID = n.String()
	}
	var errs []error
	errs = append(errs, checkStorageClasses(in, layout)...)
	errs = append(errs, s.checkDrives(ctx, layout, rep)...)
	for _, a := range in.Listen {
		if err := checkListen(ctx, a.Addr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
		}
	}
	if in.CertsDir != "" {
		warn, err := s.checkCertificates(in.CertsDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("certificates: %w", err))
		}
		rep.Warnings = append(rep.Warnings, warn...)
	}
	return rep, errors.Join(errs...)
}

// checkStorageClasses verifies that the parities of the storage classes
// fit the erasure sets of every pool (at least as many data shards as
// parity shards).
func checkStorageClasses(in Input, layout *topology.Layout) []error {
	if layout.Mode == domain.ModeSNSD {
		return nil
	}
	var errs []error
	for _, sc := range []struct{ name, value string }{
		{domain.StorageClassStandard, in.StorageClassStandard},
		{domain.StorageClassReducedRedundancy, in.StorageClassRRS},
	} {
		if sc.value == "" {
			continue
		}
		parity, err := object.ParseStorageClass(sc.value)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range layout.Pools {
			if parity > p.SetDriveCount/2 {
				errs = append(errs, fmt.Errorf("%w: storage class %s %s: pool %d has erasure sets of %d drives, at most EC:%d",
					domain.ErrInvalidArgument, sc.name, sc.value, p.Index+1, p.SetDriveCount, p.SetDriveCount/2))
			}
		}
	}
	return errs
}

// checkDrives verifies the local drive directories and the formats they
// carry.
func (s *service) checkDrives(ctx context.Context, layout *topology.Layout, rep *Report) []error {
	var errs []error
	for pi, pool := range layout.Pools {
		formats := make([]*topology.FormatV1, len(pool.Endpoints))
		found := false
		for i, ep := range pool.Endpoints {
			if !ep.IsLocal {
				continue
			}
			dir := filepath.FromSlash(ep.Path)
			rep.LocalDrives = append(rep.LocalDrives, dir)
			if err := checkWritableDir(dir); err != nil {
				errs = append(errs, fmt.Errorf("drive %s: %w", dir, err))
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, domain.SystemBucket, storage.FormatFile))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("drive %s: read format.json: %w", dir, err))
				continue
			}
			doc, err := topology.UnmarshalFormat(data)
			if err != nil {
				errs = append(errs, fmt.Errorf("drive %s: %w", dir, err))
				continue
			}
			formats[i] = doc
			found = true
			rep.Formatted++
		}
		if !found {
			continue
		}
		ref, err := s.deps.Topology.CheckFormats(ctx, layout, pi, formats)
		if err != nil {
			errs = append(errs, fmt.Errorf("pool %d: the drives were formatted for another topology: %w", pi+1, err))
			continue
		}
		if rep.DeploymentID != "" && ref.ID != rep.DeploymentID {
			errs = append(errs, fmt.Errorf("%w: pool %d belongs to deployment %s, another pool to %s",
				domain.ErrFormatMismatch, pi+1, ref.ID, rep.DeploymentID))
			continue
		}
		rep.DeploymentID = ref.ID
	}
	return errs
}

// checkWritableDir verifies that dir is a writable directory or, when it
// does not exist, that its closest existing parent is (the server creates
// it). The probe file is removed at once.
func checkWritableDir(dir string) error {
	probe := dir
	for {
		info, err := os.Stat(probe)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%w: %s is not a directory", domain.ErrInvalidArgument, probe)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("%w: no existing parent directory", domain.ErrInvalidArgument)
		}
		probe = parent
	}
	f, err := os.CreateTemp(probe, ".nexo-dry-run-*")
	if err != nil {
		return fmt.Errorf("%s is not writable by this user (uid %d): %w", probe, os.Getuid(), err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// checkListen verifies that addr can be listened on.
func checkListen(ctx context.Context, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// checkCertificates verifies the TLS key pair of dir and returns the
// warnings (a certificate expiring soon).
func (s *service) checkCertificates(dir string) ([]string, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "public.crt"), filepath.Join(dir, "private.key"))
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	now := s.deps.Clock.Now()
	switch {
	case now.After(leaf.NotAfter):
		return nil, fmt.Errorf("%w: the certificate %q expired on %s", domain.ErrInvalidArgument, leaf.Subject.CommonName, leaf.NotAfter.Format(time.RFC3339))
	case now.Before(leaf.NotBefore):
		return nil, fmt.Errorf("%w: the certificate %q is valid from %s", domain.ErrInvalidArgument, leaf.Subject.CommonName, leaf.NotBefore.Format(time.RFC3339))
	case leaf.NotAfter.Sub(now) < s.certWarning:
		return []string{fmt.Sprintf("the certificate %q expires on %s", leaf.Subject.CommonName, leaf.NotAfter.Format(time.RFC3339))}, nil
	}
	return nil, nil
}
