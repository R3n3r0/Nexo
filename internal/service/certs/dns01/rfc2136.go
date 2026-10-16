package dns01

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// TSIG algorithms of the RFC 2136 provider.
const (
	TSIGHmacSHA1   = "hmac-sha1"
	TSIGHmacSHA224 = "hmac-sha224"
	TSIGHmacSHA256 = "hmac-sha256"
	TSIGHmacSHA384 = "hmac-sha384"
	TSIGHmacSHA512 = "hmac-sha512"
)

// TSIGAlgorithms returns the TSIG algorithms the RFC 2136 provider signs
// with.
func TSIGAlgorithms() []string {
	return []string{TSIGHmacSHA256, TSIGHmacSHA512, TSIGHmacSHA384, TSIGHmacSHA224, TSIGHmacSHA1}
}

// RFC2136Config configures the RFC 2136 provider.
type RFC2136Config struct {
	// Nameserver is the primary nameserver accepting the dynamic updates
	// (host or host:port, port 53 by default).
	Nameserver string
	// TSIGKeyName, TSIGAlgorithm (default hmac-sha256) and TSIGSecret
	// (base64, a secret) sign the updates; without a key name the updates
	// are not signed (a server that trusts the address of the node).
	TSIGKeyName   string
	TSIGAlgorithm string
	TSIGSecret    string
	// Zone is the zone of the records (default: the zone the nameserver
	// reports in the SOA of the record).
	Zone string
	Settings
}

type rfc2136 struct {
	cfg    RFC2136Config
	algo   string
	client *dns.Client
}

var _ Provider = (*rfc2136)(nil)

// NewRFC2136 returns the provider of the zones accepting the dynamic
// updates of RFC 2136.
func NewRFC2136(cfg RFC2136Config, _ ...Option) (Provider, error) {
	cfg.Nameserver = strings.TrimSpace(cfg.Nameserver)
	if cfg.Nameserver == "" {
		return nil, fmt.Errorf("%w: rfc2136: the nameserver is required", ErrInvalidConfig)
	}
	addrs := normalizeAddrs([]string{cfg.Nameserver})
	if _, _, err := net.SplitHostPort(addrs[0]); err != nil {
		return nil, fmt.Errorf("%w: rfc2136: nameserver %q must be host or host:port", ErrInvalidConfig, cfg.Nameserver)
	}
	cfg.Nameserver = addrs[0]
	cfg.TSIGKeyName = strings.TrimSpace(cfg.TSIGKeyName)
	cfg.TSIGSecret = strings.TrimSpace(cfg.TSIGSecret)
	cfg.TSIGAlgorithm = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.TSIGAlgorithm)), ".")
	if cfg.Zone = strings.TrimSpace(cfg.Zone); cfg.Zone != "" {
		cfg.Zone = dns.Fqdn(strings.ToLower(cfg.Zone))
		if _, ok := dns.IsDomainName(cfg.Zone); !ok {
			return nil, fmt.Errorf("%w: rfc2136: zone %q is not a domain name", ErrInvalidConfig, cfg.Zone)
		}
	}
	p := &rfc2136{cfg: cfg, client: &dns.Client{Timeout: queryTimeout}}
	if cfg.TSIGKeyName == "" {
		if cfg.TSIGSecret != "" {
			return nil, fmt.Errorf("%w: rfc2136: a TSIG secret needs its key name", ErrInvalidConfig)
		}
		return p, nil
	}
	if cfg.TSIGSecret == "" {
		return nil, fmt.Errorf("%w: rfc2136: the TSIG key %s needs its secret", ErrInvalidConfig, cfg.TSIGKeyName)
	}
	if _, err := base64.StdEncoding.DecodeString(cfg.TSIGSecret); err != nil {
		return nil, fmt.Errorf("%w: rfc2136: the TSIG secret must be base64", ErrInvalidConfig)
	}
	if cfg.TSIGAlgorithm == "" {
		p.cfg.TSIGAlgorithm = TSIGHmacSHA256
	}
	known := false
	for _, a := range TSIGAlgorithms() {
		known = known || a == p.cfg.TSIGAlgorithm
	}
	if !known {
		return nil, fmt.Errorf("%w: rfc2136: TSIG algorithm %q must be one of %s", ErrInvalidConfig, cfg.TSIGAlgorithm, strings.Join(TSIGAlgorithms(), ", "))
	}
	p.algo = dns.Fqdn(p.cfg.TSIGAlgorithm)
	p.cfg.TSIGKeyName = dns.Fqdn(strings.ToLower(cfg.TSIGKeyName))
	p.client.TsigSecret = map[string]string{p.cfg.TSIGKeyName: cfg.TSIGSecret}
	return p, nil
}

// Propagation implements Provider.
func (p *rfc2136) Propagation() (time.Duration, time.Duration) { return p.cfg.propagation() }

// zone returns the zone of fqdn.
func (p *rfc2136) zone(ctx context.Context, fqdn string) (string, error) {
	if p.cfg.Zone != "" {
		return p.cfg.Zone, nil
	}
	return findZone(ctx, fqdn, func(ctx context.Context, name string) (*dns.Msg, error) {
		m := new(dns.Msg)
		m.SetQuestion(name, dns.TypeSOA)
		r, err := exchange(ctx, p.client, m, p.cfg.Nameserver)
		if err != nil {
			return nil, fmt.Errorf("rfc2136: SOA of %s on %s: %w", unFqdn(name), p.cfg.Nameserver, err)
		}
		return r, nil
	})
}

// Present implements Provider.
func (p *rfc2136) Present(ctx context.Context, fqdn, value string) error {
	return p.update(ctx, fqdn, value, true)
}

// CleanUp implements Provider.
func (p *rfc2136) CleanUp(ctx context.Context, fqdn, value string) error {
	return p.update(ctx, fqdn, value, false)
}

// update adds (insert) or removes the value of the TXT record fqdn.
func (p *rfc2136) update(ctx context.Context, fqdn, value string, insert bool) error {
	fqdn = dns.Fqdn(strings.ToLower(fqdn))
	zone, err := p.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	rr := &dns.TXT{Hdr: dns.RR_Header{Name: fqdn, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: uint32(p.cfg.ttl())}, Txt: []string{value}} // #nosec G115 -- a TTL of a few seconds
	m := new(dns.Msg)
	m.SetUpdate(zone)
	if insert {
		m.Insert([]dns.RR{rr})
	} else {
		m.Remove([]dns.RR{rr})
	}
	if p.cfg.TSIGKeyName != "" {
		m.SetTsig(p.cfg.TSIGKeyName, p.algo, 300, time.Now().Unix())
	}
	r, err := exchange(ctx, p.client, m, p.cfg.Nameserver)
	if err != nil {
		return fmt.Errorf("rfc2136: update of %s on %s: %w", unFqdn(fqdn), p.cfg.Nameserver, err)
	}
	if r.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("rfc2136: update of %s on %s refused: %s", unFqdn(fqdn), p.cfg.Nameserver, dns.RcodeToString[r.Rcode])
	}
	return nil
}
