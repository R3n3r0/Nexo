package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
)

// distStack is the composition of the distributions (docs/10 §10.2): the
// distribution service and its routing table, the certificates of their
// domains, the website engine, the host routers of the listeners and the
// optional web listeners (--web-address, --web-tls-address).
type distStack struct {
	// service is the distribution service the APIs use (its changes ask
	// the certificate job for a reconciliation); raw is the bare one.
	service distribution.Service
	raw     distribution.Service
	certs   certs.Service
	signer  distribution.Signer
	website distribution.Website
	// s3Hosts is the host router of the S3 listener.
	s3Hosts dist.HostRouter
	// webSrv and webTLSSrv are the web listeners (nil when not
	// configured).
	webSrv    server.Service
	webTLSSrv server.Service
	logger    *slog.Logger
}

// distCoreDeps are the inputs of newDistCore.
type distCoreDeps struct {
	cfg          config.Static
	f            *foundation
	docs         sysstore.Store
	deploymentID string
	// matcher compiles the path patterns of the behaviors (the matcher
	// of the access rules: one syntax for both).
	matcher  match.Service
	registry metrics.Registry
	clock    clock.Clock
	logger   *slog.Logger
}

// newDistCore builds the distribution and certificate services on the
// system documents (in SNSD, a system store on the single drive).
func newDistCore(d distCoreDeps) (*distStack, error) {
	logger := d.logger.With("component", "distribution")
	docs := d.docs
	if docs == nil {
		if len(d.f.local) == 0 {
			return nil, fmt.Errorf("%w: distributions: no local drive", domain.ErrInvalidArgument)
		}
		var err error
		if docs, err = sysstore.New(sysstore.Deps{Drives: d.f.local[:1], Locks: d.f.locks, Clock: d.clock, Logger: logger}); err != nil {
			return nil, err
		}
	}
	distOpts := []distribution.Option{
		distribution.WithDomains(domainsOf(d.cfg)...),
		distribution.WithMainHosts(mainHosts(d.cfg)...),
		distribution.WithUnknownHostMisdirected(d.cfg.UnknownHost == config.UnknownHostMisdirected),
	}
	var matcher distribution.Matcher
	if d.matcher != nil {
		matcher = matchAdapter{m: d.matcher}
	}
	raw, err := distribution.New(distribution.Deps{
		Store: docs, Locks: d.f.locks, Notifier: d.f.notifier, Matcher: matcher, Clock: d.clock, Logger: logger,
	}, distOpts...)
	if err != nil {
		return nil, err
	}
	sealer, err := certs.NewSealer(certs.DeriveKey(d.cfg.RootUser, d.cfg.RootPassword, d.deploymentID))
	if err != nil {
		return nil, err
	}
	certOpts := []certs.Option{
		certs.WithACMEDirectory(d.cfg.ACMEDirectory), certs.WithACMEEmail(d.cfg.ACMEEmail),
		certs.WithChallenge(d.cfg.ACMEChallenge), certs.WithReconcileOn(distribution.EventDistribution),
	}
	if d.cfg.ACMECACert != "" {
		client, err := acmeHTTPClient(d.cfg.ACMECACert)
		if err != nil {
			return nil, err
		}
		certOpts = append(certOpts, certs.WithHTTPClient(client))
	}
	certSvc, err := certs.New(certs.Deps{
		Store: docs, Locks: d.f.locks, Notifier: d.f.notifier, Leader: d.f.leader, Sealer: sealer,
		Domains: acmeDomains{dists: raw}, Metrics: d.registry, Clock: d.clock, Logger: d.logger.With("component", "certs"),
	}, certOpts...)
	if err != nil {
		return nil, err
	}
	return &distStack{
		service: reconcilingDistributions{Service: raw, certs: certSvc},
		raw:     raw,
		certs:   certSvc,
		signer:  distribution.NewSigner(distribution.SignerDeps{Clock: d.clock}),
		logger:  logger,
	}, nil
}

// hostBuckets resolves the hosts of the s3 distributions for the S3 API.
func (s *distStack) hostBuckets() s3.HostBuckets {
	return s3.HostBuckets(dist.S3BucketOfHost(s.raw))
}

// authSkip leaves to the website routes the requests of the website
// distributions (they verify the signatures of their authenticated
// behaviors and never answer S3 XML); skip selects the other requests the
// authentication leaves alone (the metrics routes, path-style only).
func (s *distStack) authSkip(skip func(*http.Request) bool) func(*http.Request) bool {
	website := dist.WebsiteRequest(s.raw)
	s3Host := s.hostBuckets()
	return func(r *http.Request) bool {
		if website(r) {
			return true
		}
		if _, ok := s3Host(middleware.HostOf(r)); ok {
			// An object key of an s3 distribution, whatever its path.
			return false
		}
		return skip != nil && skip(r)
	}
}

// authService names the SigV4 service of a request: S3 for the requests
// of the s3 distributions (their "POST /" is a bucket request, not STS).
func (s *distStack) authService(service func(*http.Request) string) func(*http.Request) string {
	s3Host := s.hostBuckets()
	return func(r *http.Request) string {
		if _, ok := s3Host(middleware.HostOf(r)); ok {
			return auth.ServiceS3
		}
		return service(r)
	}
}

// distHTTPDeps are the inputs of composeHTTP.
type distHTTPDeps struct {
	cfg      config.Static
	objects  object.Service
	policy   policy.Service
	verifier auth.Verifier
	// publicAccess reports the block public access of the buckets (the
	// access rules); secrets resolves the ${secret:<name>} references.
	publicAccess distribution.PublicAccessBlock
	secrets      accessrule.SecretStore
	// cache serves the requests of the distributions from the cache of
	// docs/11 (nil: no cache).
	cache    cache.Runtime
	registry metrics.Registry
	ids      httpx.IDGenerator
	clock    clock.Clock
	logger   *slog.Logger
}

// composeHTTP builds the website engine, the host routers and the web
// listeners.
func (s *distStack) composeHTTP(d distHTTPDeps) error {
	var secrets distribution.SecretSource
	if d.secrets != nil {
		secrets = secretSource{store: d.secrets}
	}
	website, err := distribution.NewWebsite(distribution.WebsiteDeps{
		Objects: d.objects, Policy: d.policy, Signatures: s.raw, PublicAccess: d.publicAccess, Secrets: secrets,
		Clock: d.clock, Logger: s.logger,
	}, distribution.WithServerHeader("Nexo"))
	if err != nil {
		return err
	}
	s.website = website
	var websiteOpts []dist.WebsiteOption
	if d.cfg.SendTimeout > 0 {
		websiteOpts = append(websiteOpts, dist.WithSendTimeout(d.cfg.SendTimeout))
	}
	routes, err := dist.NewWebsiteRoutes(dist.WebsiteDeps{Website: website, Resolver: s.raw, Verifier: d.verifier, Logger: s.logger}, websiteOpts...)
	if err != nil {
		return err
	}
	accessLog := dist.NewAccessLog(dist.AccessLogDeps{Logger: d.logger.With("component", "distribution-access")})
	hostDeps := dist.HostRouterDeps{Resolver: s.raw, Website: routes, Admission: website, Registry: d.registry, AccessLog: accessLog,
		Cache: d.cache, Clock: d.clock, Logger: s.logger}
	if s.s3Hosts, err = dist.NewHostRouter(hostDeps, dist.WithListenerName("s3")); err != nil {
		return err
	}
	if d.cfg.WebAddress == "" && d.cfg.WebTLSAddress == "" {
		return nil
	}
	webMws, err := webMiddlewares(d)
	if err != nil {
		return err
	}
	challenges, err := dist.NewChallengeRoutes(dist.ChallengeDeps{Certs: s.certs, Logger: s.logger})
	if err != nil {
		return err
	}
	misdirected := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "421 Misdirected Request: no distribution serves this host", http.StatusMisdirectedRequest)
	})
	if d.cfg.WebAddress != "" {
		hosts, err := dist.NewHostRouter(hostDeps, dist.WithListenerName("web"), dist.WithDistributionsOnly(), dist.WithPassthrough(dist.ChallengePrefix))
		if err != nil {
			return err
		}
		s.webSrv = server.New(server.Deps{Routes: []httpx.Routes{challenges}, Middlewares: webMws, Hosts: hosts, NotFound: misdirected, Logger: d.logger},
			webServerOptions(d.cfg, d.cfg.WebAddress, "web")...)
	}
	if d.cfg.WebTLSAddress != "" {
		hosts, err := dist.NewHostRouter(hostDeps, dist.WithListenerName("web-tls"), dist.WithDistributionsOnly())
		if err != nil {
			return err
		}
		opts := append(webServerOptions(d.cfg, d.cfg.WebTLSAddress, "web-tls"), server.WithCertificates(s.certs))
		if d.cfg.TLSEnabled() {
			// The certificate of the server is the fallback of the names
			// no distribution certificate covers.
			opts = append(opts, server.WithTLS(certFiles(d.cfg)))
		}
		s.webTLSSrv = server.New(server.Deps{Middlewares: webMws, Hosts: hosts, NotFound: misdirected, Logger: d.logger}, opts...)
	}
	return nil
}

// runner starts the distributions, the certificates and the web
// listeners, and stops them in reverse order.
func (s *distStack) runner() lifecycle.Runner {
	var runners []lifecycle.Runner
	runners = append(runners, s.raw, s.certs)
	if s.webSrv != nil {
		runners = append(runners, s.webSrv)
	}
	if s.webTLSSrv != nil {
		runners = append(runners, s.webTLSSrv)
	}
	return lifecycle.Funcs{
		OnStart: func(ctx context.Context) error {
			for i, r := range runners {
				if err := r.Start(ctx); err != nil {
					for j := i - 1; j >= 0; j-- {
						_ = runners[j].Stop(context.WithoutCancel(ctx))
					}
					return err
				}
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			var errs []error
			for i := len(runners) - 1; i >= 0; i-- {
				if err := runners[i].Stop(ctx); err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) > 0 {
				return fmt.Errorf("distributions: stop: %v", errs)
			}
			return nil
		},
	}
}

// webMiddlewares are the middlewares of the web listeners: request ids,
// the client behind the trusted proxies, panic recovery, logging and
// metrics (the website routes authenticate their own requests).
func webMiddlewares(d distHTTPDeps) ([]middleware.Middleware, error) {
	forwarded, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(d.cfg.TrustedProxies...),
		middleware.WithForwardedHeaders(middleware.ForwardedHeaders(d.cfg.ForwardedHeaders)))
	if err != nil {
		return nil, err
	}
	httpMetrics, err := middleware.NewMetrics(middleware.MetricsDeps{Registry: d.registry, Clock: d.clock}, middleware.WithMetricsServer("web"))
	if err != nil {
		return nil, err
	}
	plain := middleware.ErrorWriter(func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
	})
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		forwarded,
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: plain}),
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "web"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
	}, nil
}

// webServerOptions are the options of a web listener: its address and
// name, the PROXY protocol of the trusted proxies when enabled for the
// web listeners.
func webServerOptions(cfg config.Static, addr, name string) []server.Option {
	opts := []server.Option{server.WithAddr(addr), server.WithName(name)}
	if cfg.ProxyProtocolOn(config.ListenerWeb) {
		opts = append(opts, server.WithProxyProtocol(cfg.TrustedProxies...))
	}
	return opts
}

// certFiles are the certificate and key files of the server.
func certFiles(cfg config.Static) (string, string) {
	return cfg.CertsDir + "/public.crt", cfg.CertsDir + "/private.key"
}

// mainHosts are the host names of the S3 API and of the console.
func mainHosts(cfg config.Static) []string {
	var out []string
	for _, raw := range []string{cfg.ServerURL, cfg.BrowserRedirectURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	return out
}

// acmeHTTPClient returns the client of an ACME server whose CA is in file
// (besides the system roots).
func acmeHTTPClient(file string) (*http.Client, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("acme CA certificate: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%w: acme CA certificate %s holds no PEM certificate", domain.ErrInvalidArgument, file)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: time.Minute}, nil
}

// acmeDomains lists the ACME certificates of the distributions: one per
// enabled distribution with tls.certificate "auto", for its aliases.
type acmeDomains struct {
	dists distribution.Service
}

var _ certs.DomainSource = acmeDomains{}

// ACMEDomains implements certs.DomainSource.
func (a acmeDomains) ACMEDomains(ctx context.Context) ([]certs.ManagedDomains, error) {
	list, err := a.dists.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []certs.ManagedDomains
	for _, d := range list {
		if !d.Enabled || d.TLS.Certificate != distribution.CertificateAuto {
			continue
		}
		id := "acme-" + d.ID
		if len(id) > 64 {
			id = id[:64]
		}
		out = append(out, certs.ManagedDomains{ID: strings.TrimRight(id, "-._"), Domains: d.Aliases})
	}
	return out, nil
}

// reconcilingDistributions asks the certificate job for a reconciliation
// after every change of a distribution made on this node.
type reconcilingDistributions struct {
	distribution.Service
	certs certs.Service
}

var _ distribution.Service = reconcilingDistributions{}

// Create implements distribution.Service.
func (r reconcilingDistributions) Create(ctx context.Context, d distribution.Distribution) (distribution.Distribution, error) {
	out, err := r.Service.Create(ctx, d)
	if err == nil {
		r.certs.Trigger(ctx)
	}
	return out, err
}

// Update implements distribution.Service.
func (r reconcilingDistributions) Update(ctx context.Context, d distribution.Distribution, ifMatch string) (distribution.Distribution, error) {
	out, err := r.Service.Update(ctx, d, ifMatch)
	if err == nil {
		r.certs.Trigger(ctx)
	}
	return out, err
}

// SetEnabled implements distribution.Service.
func (r reconcilingDistributions) SetEnabled(ctx context.Context, id string, enabled bool) (distribution.Distribution, error) {
	out, err := r.Service.SetEnabled(ctx, id, enabled)
	if err == nil {
		r.certs.Trigger(ctx)
	}
	return out, err
}

// Delete implements distribution.Service.
func (r reconcilingDistributions) Delete(ctx context.Context, id string) error {
	err := r.Service.Delete(ctx, id)
	if err == nil {
		r.certs.Trigger(ctx)
	}
	return err
}

// matchAdapter makes the matcher of the access rules the matcher of the
// behaviors: one pattern syntax (and one normalization) for both.
type matchAdapter struct {
	m match.Service
}

var _ distribution.Matcher = matchAdapter{}

// Compile implements distribution.Matcher.
func (a matchAdapter) Compile(ctx context.Context, p distribution.PathPattern) (distribution.CompiledPattern, error) {
	m, err := a.m.Compile(ctx, match.Pattern{Type: match.Type(p.Type), Pattern: p.Pattern, CaseInsensitive: p.CaseInsensitive})
	if err != nil {
		return nil, err
	}
	return compiledMatch{svc: a.m, m: m}, nil
}

// compiledMatch matches the normalized request paths.
type compiledMatch struct {
	svc match.Service
	m   match.Matcher
}

// Match implements distribution.CompiledPattern.
func (c compiledMatch) Match(path string) bool {
	return c.m.Match(c.svc.Normalize(context.Background(), path)).Matched
}

// secretSource resolves the ${secret:<name>} origin secrets with the
// secrets of the configuration (NEXO_SECRET_<NAME>).
type secretSource struct {
	store accessrule.SecretStore
}

var _ distribution.SecretSource = secretSource{}

// LookupSecret implements distribution.SecretSource.
func (s secretSource) LookupSecret(ctx context.Context, name string) (string, bool) {
	v, err := s.store.Secret(ctx, name)
	return v, err == nil && v != ""
}

// certificateHolder is the certificate source of a listener built before
// the services (the S3 listener of a cluster node): it serves the
// certificates of the distributions once they are composed.
type certificateHolder struct {
	src atomic.Pointer[server.CertificateSource]
}

var _ server.CertificateSource = (*certificateHolder)(nil)

// set installs the source.
func (h *certificateHolder) set(src server.CertificateSource) { h.src.Store(&src) }

// GetCertificate implements server.CertificateSource.
func (h *certificateHolder) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	src := h.src.Load()
	if src == nil {
		return nil, nil
	}
	return (*src).GetCertificate(hello)
}

// tlsOptions are the options of the S3 listener of the composed services:
// the certificates of the distributions by SNI, when it serves TLS.
func (s *distStack) tlsOptions(cfg config.Static) []server.Option {
	if !cfg.TLSEnabled() {
		return nil
	}
	return []server.Option{server.WithCertificates(s.certs)}
}

// requestPredicate is a request selector installed after its owner was
// built (the RPC server of a cluster node, before the services).
type requestPredicate struct {
	fn atomic.Pointer[func(*http.Request) bool]
}

// set installs the selector.
func (p *requestPredicate) set(fn func(*http.Request) bool) { p.fn.Store(&fn) }

// match reports whether the installed selector selects r (false before
// set).
func (p *requestPredicate) match(r *http.Request) bool {
	if fn := p.fn.Load(); fn != nil {
		return (*fn)(r)
	}
	return false
}
