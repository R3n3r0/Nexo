package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/preflight"
)

// CheckReport describes a configuration validated by DryRun.
type CheckReport = preflight.Report

// DryRun validates a configuration without starting anything nor writing
// to the drives (nexo server --dry-run, docs/08 §8.3, TM-12): the settings
// here, then the checks of the preflight service on the topology of the
// configuration (mode, erasure sets, this node among the URL endpoints),
// the storage classes, the local drives and their formats, the listen
// addresses (S3, console and the web listeners of the distributions), the
// TLS certificates and the CA of a private ACME server. Every problem
// found is returned, joined.
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
	checks, err := preflight.New(preflight.Deps{Topology: topo, Clock: o.clock, Logger: o.logger})
	if err != nil {
		return nil, err
	}
	in := preflight.Input{
		Endpoints: cfg.Endpoints,
		Listen: []preflight.Address{
			{Name: "s3 address", Addr: cfg.Address},
			{Name: "console address", Addr: cfg.ConsoleAddress},
		},
		StorageClassStandard: cfg.StorageClassStandard,
		StorageClassRRS:      cfg.StorageClassRRS,
		ACMECACert:           cfg.ACMECACert,
	}
	// The listeners of the distributions, when configured (--web-address,
	// --web-tls-address), are checked like the others.
	for _, web := range []preflight.Address{
		{Name: "web address", Addr: cfg.WebAddress},
		{Name: "web TLS address", Addr: cfg.WebTLSAddress},
	} {
		if web.Addr != "" {
			in.Listen = append(in.Listen, web)
		}
	}
	if cfg.TLSEnabled() {
		in.CertsDir = cfg.CertsDir
		in.InternodeVerify = cfg.TLSInternodeVerify
	}
	return checks.Check(ctx, in)
}
