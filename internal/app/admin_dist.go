package app

import (
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	adminhttp "github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// buildDistributionManagers composes the console services of the access
// rules, the distributions and their certificates (docs/10 §10.5) when
// their domain services are wired.
func buildDistributionManagers(d adminDeps, out *adminServices) error {
	var err error
	if d.accessRules != nil {
		if out.accessRules, err = adminsvc.NewAccessRuleManager(adminsvc.AccessRuleManagerDeps{
			Rules: d.accessRules, Authz: out.authz, Creds: d.iam, Logger: d.logger,
		}); err != nil {
			return err
		}
	}
	if d.dist == nil {
		return nil
	}
	if out.distributions, err = adminsvc.NewDistributionManager(adminsvc.DistributionManagerDeps{
		Distributions: d.dist.service, Signer: d.dist.signer, Authz: out.authz, Cache: cacheManager(d), DNSProviders: d.dist.dnsProviders,
		Clock: d.clock, Logger: d.logger,
	}); err != nil {
		return err
	}
	out.certificates, err = adminsvc.NewCertificateManager(adminsvc.CertificateManagerDeps{
		Certs: d.dist.certs, Distributions: d.dist.raw, Domains: acmeDomains{dists: d.dist.raw}, DNSProviders: d.dist.dnsProviders, Logger: d.logger,
	}, adminsvc.WithACME(d.acmeDirectory, d.acmeChallenge))
	return err
}

// buildDistributionRoutes returns the Admin API route groups of the access
// rules, the distributions and the certificates whose services are wired.
func buildDistributionRoutes(d adminDeps, svc adminServices) ([]httpx.Routes, error) {
	var out []httpx.Routes
	if svc.accessRules != nil {
		routes, err := adminhttp.NewAccessRuleRoutes(adminhttp.AccessRuleDeps{Rules: svc.accessRules, Authz: svc.authz, Enc: d.codec})
		if err != nil {
			return nil, err
		}
		out = append(out, routes)
	}
	if svc.distributions != nil {
		routes, err := adminhttp.NewDistributionRoutes(adminhttp.DistributionDeps{Distributions: svc.distributions, Authz: svc.authz, Enc: d.codec})
		if err != nil {
			return nil, err
		}
		out = append(out, routes)
	}
	if svc.certificates != nil {
		routes, err := adminhttp.NewCertificateRoutes(adminhttp.CertificateDeps{Certificates: svc.certificates, Authz: svc.authz, Enc: d.codec})
		if err != nil {
			return nil, err
		}
		out = append(out, routes)
	}
	return out, nil
}
