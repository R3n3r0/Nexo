package app

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

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// certExpiryWarning is how close to its expiry a certificate is reported.
const certExpiryWarning = 30 * 24 * time.Hour

// CheckReport describes a configuration validated by DryRun.
type CheckReport struct {
	Mode domain.Mode
	// NodeID is the "host:port" of this node in a cluster, "local"
	// otherwise; Nodes the number of nodes of the deployment.
	NodeID string
	Nodes  int
	// Pools summarizes the erasure pools of the layout.
	Pools []domain.PoolInfo
	// LocalDrives are the directories of the drives of this node.
	LocalDrives []string
	// DeploymentID is the deployment found in the format.json of the
	// local drives, empty for fresh drives.
	DeploymentID string
	// Formatted counts the local drives that carry a format.json.
	Formatted int
	// Warnings are the problems that do not prevent the start (a
	// certificate close to its expiry).
	Warnings []string
}

// DryRun validates a configuration without starting anything nor writing
// to the drives (nexo server --dry-run, docs/08 §8.3, TM-12): the
// settings, the topology (mode, erasure sets, recognition of this node
// among the URL endpoints by the port of its S3 address), the local drive
// directories (existing and writable, or creatable), the format.json
// already on them (same deployment, compatible with the topology), the
// listen addresses (free) and the TLS certificates (readable key pair,
// not expired). Every problem found is returned, joined.
func DryRun(ctx context.Context, cfg config.Static, opts ...Option) (*CheckReport, error) {
	o := options{logger: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	topo := o.topology
	if topo == nil {
		topo = newTopology(cfg, o.logger)
	}
	layout, err := topo.Parse(ctx, cfg.Endpoints)
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	rep := &CheckReport{Mode: layout.Mode, NodeID: "local", Nodes: len(layout.Nodes), Pools: poolInfos(layout)}
	if n, ok := layout.LocalNode(); ok && layout.Mode == domain.ModeDistributed {
		rep.NodeID = n.String()
	}
	var errs []error
	errs = append(errs, checkStorageClasses(cfg, layout)...)
	errs = append(errs, checkDrives(ctx, topo, layout, rep)...)
	for name, addr := range map[string]string{"s3 address": cfg.Address, "console address": cfg.ConsoleAddress} {
		if err := checkListen(ctx, addr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if cfg.TLSEnabled() {
		warn, err := checkCertificates(cfg.CertsDir, time.Now())
		if err != nil {
			errs = append(errs, fmt.Errorf("certificates: %w", err))
		}
		rep.Warnings = append(rep.Warnings, warn...)
	}
	if err := errors.Join(errs...); err != nil {
		return rep, err
	}
	return rep, nil
}

// checkStorageClasses verifies that the parities of the storage classes
// fit the erasure sets of every pool (at least as many data shards as
// parity shards).
func checkStorageClasses(cfg config.Static, layout *topology.Layout) []error {
	if layout.Mode == domain.ModeSNSD {
		return nil
	}
	var errs []error
	for name, v := range map[string]string{domain.StorageClassStandard: cfg.StorageClassStandard, domain.StorageClassReducedRedundancy: cfg.StorageClassRRS} {
		if v == "" {
			continue
		}
		parity, err := object.ParseStorageClass(v)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range layout.Pools {
			if parity > p.SetDriveCount/2 {
				errs = append(errs, fmt.Errorf("%w: storage class %s %s: pool %d has erasure sets of %d drives, at most EC:%d",
					domain.ErrInvalidArgument, name, v, p.Index+1, p.SetDriveCount, p.SetDriveCount/2))
			}
		}
	}
	return errs
}

// checkDrives verifies the local drive directories and the formats they
// carry.
func checkDrives(ctx context.Context, topo topology.Service, layout *topology.Layout, rep *CheckReport) []error {
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
		ref, err := topo.CheckFormats(ctx, layout, pi, formats)
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
// does not exist, that its closest existing parent is (Build creates it).
// The probe file is removed at once.
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
func checkCertificates(dir string, now time.Time) ([]string, error) {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "public.crt"), filepath.Join(dir, "private.key"))
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	switch {
	case now.After(leaf.NotAfter):
		return nil, fmt.Errorf("%w: the certificate %q expired on %s", domain.ErrInvalidArgument, leaf.Subject.CommonName, leaf.NotAfter.Format(time.RFC3339))
	case now.Before(leaf.NotBefore):
		return nil, fmt.Errorf("%w: the certificate %q is valid from %s", domain.ErrInvalidArgument, leaf.Subject.CommonName, leaf.NotBefore.Format(time.RFC3339))
	case leaf.NotAfter.Sub(now) < certExpiryWarning:
		return []string{fmt.Sprintf("the certificate %q expires on %s", leaf.Subject.CommonName, leaf.NotAfter.Format(time.RFC3339))}, nil
	}
	return nil, nil
}
