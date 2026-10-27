package app

import (
	"context"
	"encoding/json"

	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/certs"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/service/siterepl"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/version"
)

// sitePolicies adapts the policy engine to the site replication: the
// policies travel as their JSON documents.
type sitePolicies struct{ p policy.Service }

func (a sitePolicies) ListPolicies(ctx context.Context) ([]siterepl.NamedPolicy, error) {
	named, err := a.p.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]siterepl.NamedPolicy, 0, len(named))
	for _, n := range named {
		doc, err := json.Marshal(n.Policy)
		if err != nil {
			return nil, err
		}
		out = append(out, siterepl.NamedPolicy{Name: n.Name, Doc: doc, UpdatedAt: n.UpdatedAt, BuiltIn: n.BuiltIn})
	}
	return out, nil
}

func (a sitePolicies) SetPolicy(ctx context.Context, name string, doc []byte) error {
	p, err := a.p.Parse(ctx, doc)
	if err != nil {
		return err
	}
	return a.p.SetPolicy(ctx, name, p)
}

func (a sitePolicies) DeletePolicy(ctx context.Context, name string) error {
	return a.p.DeletePolicy(ctx, name)
}

// newSiteReplication builds the site replication (S-STO-11): the entries
// exchanged with the other sites are sealed with a key derived from the
// root credentials, which the sites share.
func newSiteReplication(d composeDeps, identities iam.Service, policies policy.Service, buckets bucket.Service,
	config configsvc.Service, repl replication.Service, docs sysstore.Store,
) (siterepl.Service, error) {
	sealer, err := certs.NewSealer(replication.DeriveKey(d.cfg.RootUser, d.cfg.RootPassword, "site-replication"))
	if err != nil {
		return nil, err
	}
	cfgMgr, err := admin.NewConfigManager(admin.ConfigManagerDeps{Config: config, Logger: d.logger})
	if err != nil {
		return nil, err
	}
	settings, err := admin.NewClusterManager(admin.ClusterManagerDeps{IAM: identities, Policy: policies, Config: cfgMgr, Buckets: buckets,
		Clock: d.clock, Logger: d.logger}, admin.WithServerVersion(version.Get().Version))
	if err != nil {
		return nil, err
	}
	deps := siterepl.Deps{DeploymentID: d.deploymentID, RootUser: d.cfg.RootUser, RootPassword: d.cfg.RootPassword,
		IAM: identities, Policies: sitePolicies{p: policies}, Buckets: buckets, Settings: settings, Replication: repl,
		Docs: docs, Sealer: sealer, Clock: d.clock, Logger: d.logger.With("component", "siterepl")}
	if d.f.leader != nil {
		deps.Leader = d.f.leader
	}
	return siterepl.New(deps)
}
