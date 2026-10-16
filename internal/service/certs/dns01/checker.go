package dns01

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Defaults of the propagation check.
const (
	// DefaultPropagationTimeout bounds the wait of a record.
	DefaultPropagationTimeout = 2 * time.Minute
	// DefaultPollingInterval is the delay between two checks.
	DefaultPollingInterval = 4 * time.Second
	// DefaultTTL is the TTL of the challenge records.
	DefaultTTL = 60
	// queryTimeout bounds one DNS query.
	queryTimeout = 5 * time.Second
)

// fallbackResolvers returns the recursive resolvers used when the node
// has none configured (no /etc/resolv.conf).
func fallbackResolvers() []string { return []string{"1.1.1.1:53", "8.8.8.8:53"} }

// CheckerOption configures the Checker.
type CheckerOption func(*checker)

// WithResolvers sets the recursive resolvers that find the authoritative
// nameservers of a zone (host or host:port; default: those of
// /etc/resolv.conf).
func WithResolvers(addrs ...string) CheckerOption {
	return func(c *checker) {
		if a := normalizeAddrs(addrs); len(a) > 0 {
			c.resolvers = a
		}
	}
}

// WithNameservers makes the Checker query these nameservers for the
// records instead of the authoritative nameservers of their zones
// (split-horizon DNS, a private zone, tests).
func WithNameservers(addrs ...string) CheckerOption {
	return func(c *checker) { c.nameservers = normalizeAddrs(addrs) }
}

type checker struct {
	resolvers   []string
	nameservers []string
	client      *dns.Client
	// nsPort is the port of the authoritative nameservers (53; tests).
	nsPort string
}

var _ Checker = (*checker)(nil)

// NewChecker returns the propagation check of the DNS-01 records.
func NewChecker(opts ...CheckerOption) Checker {
	c := &checker{client: &dns.Client{Timeout: queryTimeout}, nsPort: "53"}
	for _, o := range opts {
		o(c)
	}
	if len(c.resolvers) == 0 {
		c.resolvers = systemResolvers()
	}
	return c
}

// systemResolvers reads the resolvers of /etc/resolv.conf.
func systemResolvers() []string {
	conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(conf.Servers) == 0 {
		return fallbackResolvers()
	}
	out := make([]string, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		out = append(out, net.JoinHostPort(s, conf.Port))
	}
	return out
}

// normalizeAddrs adds the port 53 to the addresses without one.
func normalizeAddrs(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(a); err != nil {
			a = net.JoinHostPort(strings.Trim(a, "[]"), "53")
		}
		out = append(out, a)
	}
	return out
}

// Wait implements Checker.
func (c *checker) Wait(ctx context.Context, fqdn, value string, timeout, interval time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultPropagationTimeout
	}
	if interval <= 0 {
		interval = DefaultPollingInterval
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fqdn = dns.Fqdn(strings.ToLower(fqdn))
	var last error
	for {
		ok, err := c.served(ctx, fqdn, value)
		if ok {
			return nil
		}
		last = err
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			if last == nil {
				last = ctx.Err()
			}
			return fmt.Errorf("the TXT record %s is not served by the nameservers of its zone after %s: %w", fqdn, timeout, last)
		case <-t.C:
		}
	}
}

// served reports whether every nameserver of fqdn serves value.
func (c *checker) served(ctx context.Context, fqdn, value string) (bool, error) {
	servers := c.nameservers
	if len(servers) == 0 {
		var err error
		if servers, err = c.authoritative(ctx, fqdn); err != nil {
			return false, err
		}
	}
	for _, ns := range servers {
		values, err := c.txt(ctx, fqdn, ns)
		if err != nil {
			return false, fmt.Errorf("nameserver %s: %w", ns, err)
		}
		if !slices.Contains(values, value) {
			return false, fmt.Errorf("nameserver %s does not serve the value yet", ns)
		}
	}
	return true, nil
}

// txt queries ns for the TXT values of fqdn (not recursively).
func (c *checker) txt(ctx context.Context, fqdn, ns string) ([]string, error) {
	m := new(dns.Msg)
	m.SetQuestion(fqdn, dns.TypeTXT)
	m.RecursionDesired = false
	r, err := c.exchange(ctx, m, ns)
	if err != nil {
		return nil, err
	}
	if r.Rcode != dns.RcodeSuccess && r.Rcode != dns.RcodeNameError {
		return nil, fmt.Errorf("TXT %s: %s", fqdn, dns.RcodeToString[r.Rcode])
	}
	var out []string
	for _, rr := range r.Answer {
		if t, ok := rr.(*dns.TXT); ok && strings.EqualFold(t.Hdr.Name, fqdn) {
			out = append(out, strings.Join(t.Txt, ""))
		}
	}
	return out, nil
}

// exchange sends m to addr, over TCP when the UDP answer is truncated.
func (c *checker) exchange(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, error) {
	return exchange(ctx, c.client, m, addr)
}

// exchange sends m to addr with client, over TCP when the UDP answer is
// truncated.
func exchange(ctx context.Context, client *dns.Client, m *dns.Msg, addr string) (*dns.Msg, error) {
	r, _, err := client.ExchangeContext(ctx, m, addr)
	if err == nil && r.Truncated && client.Net != "tcp" {
		tcp := *client
		tcp.Net = "tcp"
		r, _, err = tcp.ExchangeContext(ctx, m, addr)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// recursive asks the resolvers, in order, the question of m.
func (c *checker) recursive(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.RecursionDesired = true
	var errs []error
	for _, r := range c.resolvers {
		out, err := c.exchange(ctx, m, r)
		if err == nil && (out.Rcode == dns.RcodeSuccess || out.Rcode == dns.RcodeNameError) {
			return out, nil
		}
		if err == nil {
			err = fmt.Errorf("%s", dns.RcodeToString[out.Rcode])
		}
		errs = append(errs, fmt.Errorf("resolver %s: %w", r, err))
	}
	return nil, errors.Join(errs...)
}

// Zone returns the zone of fqdn (the closest enclosing name with an SOA
// record) as the resolvers of the checker see it.
func (c *checker) zone(ctx context.Context, fqdn string) (string, error) {
	return findZone(ctx, fqdn, func(ctx context.Context, name string) (*dns.Msg, error) {
		return c.recursive(ctx, name, dns.TypeSOA)
	})
}

// findZone walks up the labels of fqdn until a name has an SOA record
// (or an answer carries the SOA of its zone in the authority section).
func findZone(ctx context.Context, fqdn string, soa func(context.Context, string) (*dns.Msg, error)) (string, error) {
	for name := dns.Fqdn(fqdn); name != "."; {
		r, err := soa(ctx, name)
		if err != nil {
			return "", err
		}
		for _, rr := range r.Answer {
			if s, ok := rr.(*dns.SOA); ok && strings.EqualFold(s.Hdr.Name, name) {
				return strings.ToLower(s.Hdr.Name), nil
			}
		}
		for _, rr := range r.Ns {
			if s, ok := rr.(*dns.SOA); ok && dns.IsSubDomain(s.Hdr.Name, name) {
				return strings.ToLower(s.Hdr.Name), nil
			}
		}
		i, end := dns.NextLabel(name, 0)
		if end {
			break
		}
		name = name[i:]
	}
	return "", fmt.Errorf("no DNS zone found for %s", fqdn)
}

// authoritative returns the addresses of the authoritative nameservers of
// the zone of fqdn.
func (c *checker) authoritative(ctx context.Context, fqdn string) ([]string, error) {
	zone, err := c.zone(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	r, err := c.recursive(ctx, zone, dns.TypeNS)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rr := range r.Answer {
		ns, ok := rr.(*dns.NS)
		if !ok {
			continue
		}
		// IPv4 first: a node without IPv6 connectivity would never reach
		// the IPv6 addresses of a nameserver that has both.
		for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
			a, err := c.recursive(ctx, ns.Ns, qtype)
			if err != nil {
				continue
			}
			before := len(out)
			for _, rr := range a.Answer {
				switch v := rr.(type) {
				case *dns.A:
					out = append(out, net.JoinHostPort(v.A.String(), c.nsPort))
				case *dns.AAAA:
					out = append(out, net.JoinHostPort(v.AAAA.String(), c.nsPort))
				}
			}
			if len(out) > before {
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no authoritative nameserver found for the zone %s", zone)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
