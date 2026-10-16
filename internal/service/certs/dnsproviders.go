package certs

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs/dns01"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Locations of the DNS providers in the system store.
const (
	dnsProvidersDir  = "acme/dns-providers"
	dnsProvidersLock = "nexo-config/dns-providers"
	// acmeChallengeLabel prefixes the names of the DNS-01 records.
	acmeChallengeLabel = "_acme-challenge."
	// maxPropagation bounds the propagation timeout of a provider.
	maxPropagation = 30 * time.Minute
)

// DNSProviderTypes returns the supported DNS provider types.
func DNSProviderTypes() []string {
	return []string{DNSProviderCloudflare, DNSProviderRoute53, DNSProviderRFC2136}
}

// String hides the credentials from the logs and the error messages.
func (DNSCredentials) String() string { return "[REDACTED]" }

// GoString hides the credentials from %#v.
func (DNSCredentials) GoString() string { return "certs.DNSCredentials{[REDACTED]}" }

// LogValue hides the credentials from the structured logs.
func (DNSCredentials) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// empty reports credentials without any value.
func (c DNSCredentials) empty() bool { return c == DNSCredentials{} }

// storedDNSProvider is the document of a provider: its settings and its
// sealed credentials.
type storedDNSProvider struct {
	Config DNSProviderConfig `json:"config"`
	Sealed []byte            `json:"sealed,omitempty"`
}

// DNSProvidersOption configures the DNS provider registry.
type DNSProvidersOption func(*dnsProviders)

// WithDNSProviderOptions passes options to the providers it builds (an
// HTTP client trusting a proxy, the endpoint of a test double).
func WithDNSProviderOptions(opts ...dns01.Option) DNSProvidersOption {
	return func(r *dnsProviders) { r.providerOpts = append(r.providerOpts, opts...) }
}

type dnsProviders struct {
	deps         DNSProvidersDeps
	providerOpts []dns01.Option
	lockTimeout  time.Duration
}

var _ DNSProviders = (*dnsProviders)(nil)

// NewDNSProviders returns the registry of the DNS providers. The store,
// the locker and the sealer are required (domain.ErrInvalidArgument).
func NewDNSProviders(deps DNSProvidersDeps, opts ...DNSProvidersOption) (DNSProviders, error) {
	if deps.Store == nil || deps.Locks == nil || deps.Sealer == nil {
		return nil, fmt.Errorf("%w: certs: dns providers: the store, the locker and the sealer are required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Propagation == nil {
		deps.Propagation = dns01.NewChecker()
	}
	r := &dnsProviders{deps: deps, lockTimeout: 30 * time.Second}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

func dnsProviderPath(name string) string { return dnsProvidersDir + "/" + name + docSuffix }

func dnsProviderAAD(name string) []byte { return []byte(sealedPrefix + "dns-provider:" + name) }

// load reads a provider and opens its credentials.
func (r *dnsProviders) load(ctx context.Context, name string) (DNSProviderConfig, DNSCredentials, error) {
	if !validID(name) {
		return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("%w: %q", ErrDNSProviderNotFound, name)
	}
	data, err := r.deps.Store.Get(ctx, dnsProviderPath(name))
	if errors.Is(err, sysstore.ErrNotFound) {
		return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("%w: %s", ErrDNSProviderNotFound, name)
	}
	if err != nil {
		return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("certs: dns provider %s: %w", name, err)
	}
	var doc storedDNSProvider
	if err := json.Unmarshal(data, &doc); err != nil {
		return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("certs: dns provider %s: %w", name, err)
	}
	var creds DNSCredentials
	if len(doc.Sealed) > 0 {
		plain, err := r.deps.Sealer.Open(ctx, doc.Sealed, dnsProviderAAD(name))
		if err != nil {
			return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("certs: dns provider %s: %w", name, err)
		}
		if err := json.Unmarshal(plain, &creds); err != nil {
			return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("certs: dns provider %s: credentials: %w", name, err)
		}
	}
	doc.Config.HasCredentials = !creds.empty()
	return doc.Config, creds, nil
}

// save seals the credentials and stores the provider.
func (r *dnsProviders) save(ctx context.Context, cfg DNSProviderConfig, creds DNSCredentials) error {
	doc := storedDNSProvider{Config: cfg}
	doc.Config.HasCredentials = false
	if !creds.empty() {
		plain, err := json.Marshal(creds) // #nosec G117 -- the plaintext is sealed below, never stored nor returned
		if err != nil {
			return err
		}
		if doc.Sealed, err = r.deps.Sealer.Seal(ctx, plain, dnsProviderAAD(cfg.Name)); err != nil {
			return err
		}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := r.deps.Store.Put(ctx, dnsProviderPath(cfg.Name), data); err != nil {
		return fmt.Errorf("certs: store the dns provider %s: %w", cfg.Name, err)
	}
	return nil
}

// List implements DNSProviders.
func (r *dnsProviders) List(ctx context.Context) ([]DNSProviderConfig, error) {
	docs, err := r.deps.Store.List(ctx, dnsProvidersDir)
	if errors.Is(err, sysstore.ErrNotFound) {
		return []DNSProviderConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("certs: dns providers: %w", err)
	}
	out := make([]DNSProviderConfig, 0, len(docs))
	for file, data := range docs {
		name, ok := strings.CutSuffix(file, docSuffix)
		if !ok {
			continue
		}
		var doc storedDNSProvider
		if err := json.Unmarshal(data, &doc); err != nil || doc.Config.Name != name {
			r.deps.Logger.WarnContext(ctx, "certs: unreadable dns provider document skipped", "name", name)
			continue
		}
		doc.Config.HasCredentials = len(doc.Sealed) > 0
		out = append(out, doc.Config)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get implements DNSProviders.
func (r *dnsProviders) Get(ctx context.Context, name string) (DNSProviderConfig, error) {
	cfg, _, err := r.load(ctx, name)
	return cfg, err
}

// Create implements DNSProviders.
func (r *dnsProviders) Create(ctx context.Context, cfg DNSProviderConfig, creds DNSCredentials) (DNSProviderConfig, error) {
	return r.write(ctx, cfg, creds, true)
}

// Update implements DNSProviders.
func (r *dnsProviders) Update(ctx context.Context, cfg DNSProviderConfig, creds DNSCredentials) (DNSProviderConfig, error) {
	return r.write(ctx, cfg, creds, false)
}

// write validates and stores a provider, created or replaced.
func (r *dnsProviders) write(ctx context.Context, cfg DNSProviderConfig, creds DNSCredentials, create bool) (DNSProviderConfig, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	if !validID(cfg.Name) {
		return DNSProviderConfig{}, fmt.Errorf("%w: name %q must be 1-64 lowercase letters, digits, '.', '_' or '-'", ErrInvalidDNSProvider, cfg.Name)
	}
	if err := r.deps.Locks.Lock(ctx, dnsProvidersLock, r.lockTimeout); err != nil {
		return DNSProviderConfig{}, fmt.Errorf("certs: lock: %w", err)
	}
	defer func() { _ = r.deps.Locks.Unlock(context.WithoutCancel(ctx), dnsProvidersLock) }()
	cur, curCreds, err := r.load(ctx, cfg.Name)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, ErrDNSProviderNotFound):
		return DNSProviderConfig{}, err
	case create && exists:
		return DNSProviderConfig{}, fmt.Errorf("%w: %s", ErrDNSProviderExists, cfg.Name)
	case !create && !exists:
		return DNSProviderConfig{}, err
	}
	cfg.Type = strings.ToLower(strings.TrimSpace(cfg.Type))
	if exists && cur.Type == cfg.Type {
		creds = mergeCredentials(cfg, creds, curCreds)
	}
	norm, creds, err := normalizeDNSProvider(cfg, creds)
	if err != nil {
		return DNSProviderConfig{}, err
	}
	if _, err := r.build(norm, creds); err != nil {
		return DNSProviderConfig{}, err
	}
	now := r.deps.Clock.Now().UTC()
	norm.CreatedAt, norm.UpdatedAt = now, now
	if exists {
		norm.CreatedAt = cur.CreatedAt
	}
	if err := r.save(ctx, norm, creds); err != nil {
		return DNSProviderConfig{}, err
	}
	r.deps.Logger.InfoContext(ctx, "certs: dns provider stored", "name", norm.Name, "type", norm.Type, "created", !exists)
	norm.HasCredentials = !creds.empty()
	return norm, nil
}

// mergeCredentials keeps the stored credentials the update leaves empty.
func mergeCredentials(cfg DNSProviderConfig, in, cur DNSCredentials) DNSCredentials {
	keep := func(v, old string) string {
		if v == "" {
			return old
		}
		return v
	}
	out := DNSCredentials{
		APIToken:        keep(in.APIToken, cur.APIToken),
		SecretAccessKey: keep(in.SecretAccessKey, cur.SecretAccessKey),
		SessionToken:    keep(in.SessionToken, cur.SessionToken),
		TSIGSecret:      keep(in.TSIGSecret, cur.TSIGSecret),
	}
	// Route 53 without an access key uses the ambient credentials: the
	// stored secret goes with the key it belonged to.
	if cfg.Route53 == nil || strings.TrimSpace(cfg.Route53.AccessKeyID) == "" {
		out.SecretAccessKey, out.SessionToken = in.SecretAccessKey, in.SessionToken
	}
	// Unsigned RFC 2136 updates have no secret.
	if cfg.RFC2136 == nil || strings.TrimSpace(cfg.RFC2136.TSIGKeyName) == "" {
		out.TSIGSecret = in.TSIGSecret
	}
	return out
}

// normalizeDNSProvider checks a configuration and keeps the settings and
// credentials of its type only.
func normalizeDNSProvider(cfg DNSProviderConfig, creds DNSCredentials) (DNSProviderConfig, DNSCredentials, error) {
	invalid := func(format string, args ...any) (DNSProviderConfig, DNSCredentials, error) {
		return DNSProviderConfig{}, DNSCredentials{}, fmt.Errorf("%w: "+format, append([]any{ErrInvalidDNSProvider}, args...)...)
	}
	out := DNSProviderConfig{Name: cfg.Name, Type: cfg.Type, Comment: strings.TrimSpace(cfg.Comment),
		TTL: cfg.TTL, PropagationTimeout: cfg.PropagationTimeout, PollingInterval: cfg.PollingInterval}
	if len(out.Comment) > 256 {
		return invalid("comment longer than 256 bytes")
	}
	switch {
	case out.TTL < 0 || out.TTL > 86400:
		return invalid("ttl %d must be 0 (default 60) to 86400 seconds", out.TTL)
	case out.PropagationTimeout < 0 || time.Duration(out.PropagationTimeout)*time.Second > maxPropagation:
		return invalid("propagationTimeout %d must be 0 (default 120) to %d seconds", out.PropagationTimeout, int(maxPropagation.Seconds()))
	case out.PollingInterval < 0 || out.PollingInterval > 600:
		return invalid("pollingInterval %d must be 0 (default 4) to 600 seconds", out.PollingInterval)
	}
	var keep DNSCredentials
	switch cfg.Type {
	case DNSProviderCloudflare:
		s := CloudflareSettings{}
		if cfg.Cloudflare != nil {
			s.ZoneID = strings.TrimSpace(cfg.Cloudflare.ZoneID)
		}
		out.Cloudflare = &s
		keep.APIToken = strings.TrimSpace(creds.APIToken)
		if keep.APIToken == "" {
			return invalid("cloudflare: the API token is required (credentials.apiToken)")
		}
	case DNSProviderRoute53:
		s := Route53Settings{}
		if cfg.Route53 != nil {
			s = Route53Settings{AccessKeyID: strings.TrimSpace(cfg.Route53.AccessKeyID), Region: strings.TrimSpace(cfg.Route53.Region),
				HostedZoneID: strings.TrimSpace(cfg.Route53.HostedZoneID)}
		}
		out.Route53 = &s
		keep.SecretAccessKey, keep.SessionToken = strings.TrimSpace(creds.SecretAccessKey), strings.TrimSpace(creds.SessionToken)
		if s.AccessKeyID != "" && keep.SecretAccessKey == "" {
			return invalid("route53: the access key %s needs its secret (credentials.secretAccessKey)", s.AccessKeyID)
		}
		if s.AccessKeyID == "" && !keep.empty() {
			return invalid("route53: the secret access key needs its access key id (route53.accessKeyId)")
		}
	case DNSProviderRFC2136:
		if cfg.RFC2136 == nil || strings.TrimSpace(cfg.RFC2136.Nameserver) == "" {
			return invalid("rfc2136: the nameserver is required (rfc2136.nameserver)")
		}
		s := RFC2136Settings{Nameserver: strings.TrimSpace(cfg.RFC2136.Nameserver), Zone: strings.TrimSpace(cfg.RFC2136.Zone),
			TSIGKeyName: strings.TrimSpace(cfg.RFC2136.TSIGKeyName), TSIGAlgorithm: strings.ToLower(strings.TrimSpace(cfg.RFC2136.TSIGAlgorithm))}
		if s.TSIGKeyName != "" && s.TSIGAlgorithm == "" {
			s.TSIGAlgorithm = dns01.TSIGHmacSHA256
		}
		out.RFC2136 = &s
		keep.TSIGSecret = strings.TrimSpace(creds.TSIGSecret)
	default:
		return invalid("type %q must be one of %s", cfg.Type, strings.Join(DNSProviderTypes(), ", "))
	}
	return out, keep, nil
}

// build returns the provider of a configuration.
func (r *dnsProviders) build(cfg DNSProviderConfig, creds DNSCredentials) (dns01.Provider, error) {
	settings := dns01.Settings{TTL: cfg.TTL, PropagationTimeout: time.Duration(cfg.PropagationTimeout) * time.Second,
		PollingInterval: time.Duration(cfg.PollingInterval) * time.Second}
	var (
		p   dns01.Provider
		err error
	)
	switch cfg.Type {
	case DNSProviderCloudflare:
		s := CloudflareSettings{}
		if cfg.Cloudflare != nil {
			s = *cfg.Cloudflare
		}
		p, err = dns01.NewCloudflare(dns01.CloudflareConfig{APIToken: creds.APIToken, ZoneID: s.ZoneID, Settings: settings}, r.providerOpts...)
	case DNSProviderRoute53:
		s := Route53Settings{}
		if cfg.Route53 != nil {
			s = *cfg.Route53
		}
		p, err = dns01.NewRoute53(dns01.Route53Config{AccessKeyID: s.AccessKeyID, SecretAccessKey: creds.SecretAccessKey, SessionToken: creds.SessionToken,
			Region: s.Region, HostedZoneID: s.HostedZoneID, Settings: settings}, r.providerOpts...)
	case DNSProviderRFC2136:
		s := RFC2136Settings{}
		if cfg.RFC2136 != nil {
			s = *cfg.RFC2136
		}
		p, err = dns01.NewRFC2136(dns01.RFC2136Config{Nameserver: s.Nameserver, TSIGKeyName: s.TSIGKeyName, TSIGAlgorithm: s.TSIGAlgorithm,
			TSIGSecret: creds.TSIGSecret, Zone: s.Zone, Settings: settings}, r.providerOpts...)
	default:
		return nil, fmt.Errorf("%w: unknown type %q", ErrInvalidDNSProvider, cfg.Type)
	}
	if errors.Is(err, dns01.ErrInvalidConfig) {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDNSProvider, err)
	}
	return p, err
}

// Delete implements DNSProviders.
func (r *dnsProviders) Delete(ctx context.Context, name string) error {
	if err := r.deps.Locks.Lock(ctx, dnsProvidersLock, r.lockTimeout); err != nil {
		return fmt.Errorf("certs: lock: %w", err)
	}
	defer func() { _ = r.deps.Locks.Unlock(context.WithoutCancel(ctx), dnsProvidersLock) }()
	if _, _, err := r.load(ctx, name); err != nil {
		return err
	}
	if err := r.deps.Store.Delete(ctx, dnsProviderPath(name)); err != nil {
		return fmt.Errorf("certs: delete the dns provider %s: %w", name, err)
	}
	r.deps.Logger.InfoContext(ctx, "certs: dns provider deleted", "name", name)
	return nil
}

// Provider implements DNSProviderSource.
func (r *dnsProviders) Provider(ctx context.Context, name string) (dns01.Provider, error) {
	cfg, creds, err := r.load(ctx, name)
	if err != nil {
		return nil, err
	}
	return r.build(cfg, creds)
}

// Test implements DNSProviders.
func (r *dnsProviders) Test(ctx context.Context, name, domainName string) (DNSTestResult, error) {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domainName)), ".")
	d = strings.TrimPrefix(d, "*.")
	if !validDNSName(d) {
		return DNSTestResult{}, fmt.Errorf("%w: domain %q is not a DNS name", ErrInvalidDNSProvider, domainName)
	}
	p, err := r.Provider(ctx, name)
	if err != nil {
		return DNSTestResult{}, err
	}
	fqdn := acmeChallengeLabel + d + "."
	res := DNSTestResult{Record: strings.TrimSuffix(fqdn, ".")}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return DNSTestResult{}, err
	}
	value := "nexo-test-" + base64.RawURLEncoding.EncodeToString(buf)
	start := r.deps.Clock.Now()
	fail := func(step string, err error) (DNSTestResult, error) {
		res.Step, res.Error = step, err.Error()
		res.DurationMS = r.deps.Clock.Now().Sub(start).Milliseconds()
		r.deps.Logger.WarnContext(ctx, "certs: dns provider test failed", "name", name, "record", res.Record, "step", step, "err", err)
		return res, nil
	}
	if err := p.Present(ctx, fqdn, value); err != nil {
		return fail("present", err)
	}
	timeout, interval := p.Propagation()
	werr := r.deps.Propagation.Wait(ctx, fqdn, value, timeout, interval)
	cerr := p.CleanUp(context.WithoutCancel(ctx), fqdn, value)
	if werr != nil {
		return fail("propagation", werr)
	}
	if cerr != nil {
		return fail("cleanup", cerr)
	}
	res.OK = true
	res.DurationMS = r.deps.Clock.Now().Sub(start).Milliseconds()
	return res, nil
}

// validDNSName reports a host name of at least two labels (no IP).
func validDNSName(name string) bool {
	if name == "" || len(name) > 253 || net.ParseIP(name) != nil || !strings.Contains(name, ".") {
		return false
	}
	return !slices.ContainsFunc(strings.Split(name, "."), func(l string) bool {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return true
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			lower, digit := c >= 'a' && c <= 'z', c >= '0' && c <= '9'
			if !lower && !digit && c != '-' && c != '_' {
				return true
			}
		}
		return false
	})
}
