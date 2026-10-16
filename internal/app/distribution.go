package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/r3n3r0/nexo/internal/service/certs/dns01"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/service/trace"
	"github.com/r3n3r0/nexo/internal/transport/http/dist"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
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
	// dnsProviders are the DNS providers of the DNS-01 challenge.
	dnsProviders certs.DNSProviders
	signer       distribution.Signer
	website      distribution.Website
	// matcher is the matcher of the access rules, of the behaviors (in
	// the service and in the website engine) and of the cache: one
	// pattern syntax and one path normalization.
	matcher match.Service
	// s3Hosts is the host router of the S3 listener; s3Routes resolves
	// its hosts (the aliases only when the listener serves the
	// distributions, config.Static.ServesDistributionsOnS3).
	s3Hosts  dist.HostRouter
	s3Routes distribution.Resolver
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
		distribution.WithMainHosts(deploymentHosts(d.cfg, d.f.layout)...),
		distribution.WithUnknownHostMisdirected(d.cfg.UnknownHost == config.UnknownHostMisdirected),
		distribution.WithRoutingDisabled(d.cfg.NoDistributions),
	}
	raw, err := distribution.New(distribution.Deps{
		Store: docs, Locks: d.f.locks, Notifier: d.f.notifier, Matcher: d.matcher, Clock: d.clock, Logger: logger,
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
		certs.WithMainHosts(mainHosts(d.cfg)...), certs.WithDomains(domainsOf(d.cfg)...),
	}
	if d.cfg.ACMECACert != "" {
		client, err := acmeHTTPClient(d.cfg.ACMECACert)
		if err != nil {
			return nil, err
		}
		certOpts = append(certOpts, certs.WithHTTPClient(client))
	}
	var checkerOpts []dns01.CheckerOption
	if ns := d.cfg.ACMEDNSNameserverList(); len(ns) > 0 {
		checkerOpts = append(checkerOpts, dns01.WithNameservers(ns...))
	}
	propagation := dns01.NewChecker(checkerOpts...)
	certLogger := d.logger.With("component", "certs")
	dnsProviders, err := certs.NewDNSProviders(certs.DNSProvidersDeps{
		Store: docs, Locks: d.f.locks, Sealer: sealer, Propagation: propagation, Clock: d.clock, Logger: certLogger,
	})
	if err != nil {
		return nil, err
	}
	certSvc, err := certs.New(certs.Deps{
		Store: docs, Locks: d.f.locks, Notifier: d.f.notifier, Leader: d.f.leader, Sealer: sealer,
		Domains: acmeDomains{dists: raw}, Hosts: certificateHosts{routes: raw}, DNSProviders: dnsProviders, Propagation: propagation,
		Metrics: d.registry, Clock: d.clock, Logger: certLogger,
	}, certOpts...)
	if err != nil {
		return nil, err
	}
	if !d.cfg.ServesDistributionsOnS3() && !d.cfg.NoDistributions {
		logger.Info("the S3 listener does not serve the distributions: name the S3 endpoint (--server-url or --domain), " +
			"opt in with --s3-distributions on, or serve them on --web-address/--web-tls-address")
	}
	return &distStack{
		service:      reconcilingDistributions{Service: raw, certs: certSvc},
		raw:          raw,
		s3Routes:     raw.ListenerResolver(d.cfg.ServesDistributionsOnS3()),
		certs:        certSvc,
		dnsProviders: dnsProviders,
		signer:       distribution.NewSigner(distribution.SignerDeps{Clock: d.clock}),
		matcher:      d.matcher,
		logger:       logger,
	}, nil
}

// hostBuckets resolves the hosts of the s3 distributions the S3 listener
// serves, for the S3 API (which never applies it to the reserved
// "/nexo/" paths: httpx.HostBucketResolver).
func (s *distStack) hostBuckets() httpx.HostBucketResolver {
	return dist.S3BucketOfHost(s.s3Routes)
}

// systemRequest selects the requests of the S3 listener under the reserved
// "/nexo/" prefix (health, metrics, internode RPC): no distribution takes
// them, whatever their host (domain.SystemPathPrefix).
func systemRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, domain.SystemPathPrefix)
}

// authSkip leaves to the website routes the requests of the website
// distributions (they verify the signatures of their authenticated
// behaviors and never answer S3 XML); skip selects the other requests the
// authentication leaves alone (the metrics routes, path-style only).
func (s *distStack) authSkip(skip func(*http.Request) bool) func(*http.Request) bool {
	website := dist.WebsiteRequest(s.s3Routes)
	s3Host := s.hostBuckets()
	return func(r *http.Request) bool {
		if systemRequest(r) {
			return skip != nil && skip(r)
		}
		if website(r) {
			return true
		}
		if _, ok := s3Host.BucketOfHost(middleware.HostOf(r)); ok {
			// An object key of an s3 distribution, whatever its path; a
			// request its admission refuses (origin secret, address) is
			// not authenticated at all: the host router refuses it first.
			return s.website != nil && dist.RefusedS3Request(s.s3Routes, s.website)(r)
		}
		return skip != nil && skip(r)
	}
}

// authService names the SigV4 service of a request: S3 for the requests
// of the s3 distributions (their "POST /" is a bucket request, not STS).
func (s *distStack) authService(service func(*http.Request) string) func(*http.Request) string {
	s3Host := s.hostBuckets()
	return func(r *http.Request) string {
		if !systemRequest(r) {
			if _, ok := s3Host.BucketOfHost(middleware.HostOf(r)); ok {
				return auth.ServiceS3
			}
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
	// keystore holds the certificates of the server (nil without TLS):
	// the fallback of the TLS web listener for the names no distribution
	// certificate covers.
	keystore certs.Keystore
	// tracer and telemetry observe the requests of the web listeners
	// (live trace of type web, spans "web <method>"); nil: not observed.
	tracer    trace.Service
	telemetry trace.Telemetry
	// limiter enforces the rate limits of the distributions (S-DST-08;
	// nil: not enforced); codec writes the S3 errors of their refusals.
	limiter ratelimit.Service
	codec   httpx.Encoder
}

// composeHTTP builds the website engine, the host routers and the web
// listeners.
func (s *distStack) composeHTTP(d distHTTPDeps) error {
	var secrets distribution.SecretSource
	if d.secrets != nil {
		secrets = secretSource{store: d.secrets}
	}
	website, err := distribution.NewWebsite(distribution.WebsiteDeps{
		Objects: d.objects, Policy: d.policy, Signatures: s.raw, Matcher: s.matcher, PublicAccess: d.publicAccess, Secrets: secrets,
		Clock: d.clock, Logger: s.logger,
	}, distribution.WithServerHeader("Nexo"))
	if err != nil {
		return err
	}
	s.website = website
	// The downloads of the websites are bounded like the ones of the S3
	// API (NEXO_SEND_TIMEOUT, default 20s): a client that stops reading
	// releases the read lock of the object (docs/02 §2.4).
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
		Cache: d.cache, Limiter: d.limiter, Clock: d.clock, Logger: s.logger}
	if d.codec != nil {
		hostDeps.S3Errors = middleware.S3ErrorWriter(d.codec)
	}
	// The S3 listener: its aliases only when it serves the distributions,
	// and never the reserved "/nexo/" paths (health probes, metrics and
	// internode RPC stay the server's whatever the host).
	s3Deps := hostDeps
	s3Deps.Resolver = s.s3Routes
	if s.s3Hosts, err = dist.NewHostRouter(s3Deps, dist.WithListenerName("s3"), dist.WithPassthrough(domain.SystemPathPrefix)); err != nil {
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
		if d.keystore != nil {
			// The certificates of the server are the fallback of the
			// names no distribution certificate covers.
			opts = append(opts, server.WithKeystore(d.keystore))
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
		middleware.WithForwardedHeaders(middleware.ForwardedHeaders(d.cfg.ForwardedHeaders)), middleware.WithForwardedHost(d.cfg.TrustForwardedHost))
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
	var tracing, traced middleware.Middleware
	if d.telemetry != nil {
		if tracing, err = middleware.NewTracing(middleware.TracingDeps{Telemetry: d.telemetry}, middleware.WithTracingServer("web")); err != nil {
			return nil, err
		}
	}
	if d.tracer != nil {
		if traced, err = middleware.NewTrace(middleware.TraceDeps{Tracer: d.tracer, Clock: d.clock}, middleware.WithTraceType(trace.TypeWeb)); err != nil {
			return nil, err
		}
	}
	// Chain skips the nil middlewares.
	return []middleware.Middleware{
		middleware.NewRequestID(middleware.RequestIDDeps{IDs: d.ids}),
		forwarded,
		middleware.NewRecover(middleware.RecoverDeps{Logger: d.logger, OnError: plain}),
		tracing,
		middleware.NewLogging(middleware.LoggingDeps{Logger: d.logger.With("server", "web"), Clock: d.clock}, middleware.WithLogLevel(slog.LevelDebug)),
		httpMetrics,
		traced,
	}, nil
}

// webServerOptions are the options of a web listener: its address and
// name, the PROXY protocol of the trusted proxies when enabled for the
// web listeners.
func webServerOptions(cfg config.Static, addr, name string) []server.Option {
	opts := []server.Option{server.WithAddr(addr), server.WithName(name), server.WithMinTLSVersion(tlsMinVersion(cfg))}
	if cfg.SendTimeout > 0 {
		opts = append(opts, server.WithSendTimeout(cfg.SendTimeout))
	}
	if cfg.ProxyProtocolOn(config.ListenerWeb) {
		opts = append(opts, server.WithProxyProtocol(cfg.TrustedProxies...))
	}
	return opts
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

// deploymentHosts are the host names the deployment itself answers on,
// which no alias of a distribution may take (docs/10 §10.2): the S3 and
// console URLs (mainHosts), the hosts of the node endpoints (the peers
// reach each other's RPC and health probes through them), the hosts of the
// S3 and console listen addresses, "localhost" and the host name of this
// machine (and its first label). IP literals are never valid aliases.
func deploymentHosts(cfg config.Static, layout *topology.Layout) []string {
	out := mainHosts(cfg)
	if layout != nil {
		for _, ep := range layout.Endpoints() {
			if ep.Host != "" {
				out = append(out, ep.Host)
			}
		}
	}
	for _, addr := range []string{cfg.Address, cfg.ConsoleAddress} {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			out = append(out, host)
		}
	}
	out = append(out, "localhost")
	if name, err := os.Hostname(); err == nil && name != "" {
		out = append(out, name)
		if short, _, ok := strings.Cut(name, "."); ok && short != "" {
			out = append(out, short)
		}
	}
	return out
}

// acmeHTTPClient returns the client of an ACME server whose CA is in file
// (besides the system roots).
func acmeHTTPClient(file string) (*http.Client, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- a certificate file named by the operator's configuration
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
		out = append(out, certs.ManagedDomains{ID: acmeCertificateID(d.ID), Domains: d.Aliases, Challenge: d.TLS.Challenge, DNSProvider: d.TLS.DNSProvider})
	}
	return out, nil
}

// acmeCertificateID is the id of the ACME certificate of a distribution.
func acmeCertificateID(distributionID string) string {
	id := "acme-" + distributionID
	if len(id) > 64 {
		id = id[:64]
	}
	return strings.TrimRight(id, "-._")
}

// certificateHosts binds the hosts of the TLS handshakes to the
// certificate of the enabled distribution serving them (its routing
// table: no I/O).
type certificateHosts struct {
	routes distribution.Resolver
}

var _ certs.HostSource = certificateHosts{}

// CertificateFor implements certs.HostSource.
func (h certificateHosts) CertificateFor(ctx context.Context, host string) (string, bool) {
	route := h.routes.Resolve(ctx, host)
	d := route.Distribution
	if route.Kind != distribution.RouteDistribution || d == nil || !d.Enabled {
		return "", false
	}
	if d.TLS.Certificate == distribution.CertificateAuto {
		return acmeCertificateID(d.ID), true
	}
	if id, ok := strings.CutPrefix(d.TLS.Certificate, distribution.CertificateUploadPrefix); ok && id != "" {
		return id, true
	}
	return "", false
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
