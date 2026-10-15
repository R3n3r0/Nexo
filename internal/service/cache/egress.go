package cache

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

// webhookSecretPrefix is the namespace of the secrets a purge webhook may
// reference ("${secret:webhook-cf}" is NEXO_SECRET_WEBHOOK_CF): whoever
// manages the cache configures the webhooks, so a webhook never reads the
// other secrets of the node (origin secrets of the proxies, secrets of the
// access rules), which would leave with its calls.
const webhookSecretPrefix = "webhook-"

// webhookSecretName reports a secret name of the webhook namespace ('-'
// and '_' are equivalent, case does not matter).
func webhookSecretName(name string) bool {
	n := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	return strings.HasPrefix(n, webhookSecretPrefix) && len(n) > len(webhookSecretPrefix)
}

// errWebhookTarget reports a webhook endpoint the egress policy refuses.
var errWebhookTarget = fmt.Errorf("%w: webhook endpoint not allowed", ErrInvalid)

// egress decides which addresses the purge webhooks may reach: the public
// unicast addresses, plus the allowed networks (--cache-webhook-allow);
// never the nodes of the cluster. The check runs on the address dialed,
// after the name resolution, so a name that resolves (or rebinds) to an
// internal address is refused too.
type egress struct {
	allow []netip.Prefix
	// nodes are the S3 and internode endpoints of the cluster
	// ("host:port"), resolved once.
	nodes     []string
	nodesOnce sync.Once
	nodeAddrs map[netip.AddrPort]bool
}

// internalAddr reports an address that is not public unicast: loopback,
// private, link-local, shared (100.64.0.0/10), unspecified, multicast,
// unique local, documentation and the like.
func internalAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || !a.IsGlobalUnicast() {
		return true
	}
	for _, p := range internalPrefixes() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// internalPrefixes are the internal ranges netip does not classify.
func internalPrefixes() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),   // shared address space (CGNAT)
		netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
		netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
		netip.MustParsePrefix("192.0.2.0/24"),    // documentation
		netip.MustParsePrefix("198.51.100.0/24"), // documentation
		netip.MustParsePrefix("203.0.113.0/24"),  // documentation
		netip.MustParsePrefix("240.0.0.0/4"),     // reserved
		netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 (maps IPv4, private ones too)
		netip.MustParsePrefix("2001:db8::/32"),   // documentation
	}
}

// allowedAddr reports an address of an allowed network.
func (e *egress) allowedAddr(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range e.allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// checkAddr is the egress policy of a dialed address.
func (e *egress) checkAddr(ap netip.AddrPort) error {
	addr := netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	if e.isNode(addr) {
		return fmt.Errorf("%w: %s is a node of the cluster", errWebhookTarget, addr)
	}
	if internalAddr(addr.Addr()) && !e.allowedAddr(addr.Addr()) {
		return fmt.Errorf("%w: %s is an internal address (allow its network with --cache-webhook-allow)", errWebhookTarget, addr.Addr())
	}
	return nil
}

// isNode reports an endpoint of a node of the cluster.
func (e *egress) isNode(ap netip.AddrPort) bool {
	e.nodesOnce.Do(func() {
		e.nodeAddrs = map[netip.AddrPort]bool{}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, n := range e.nodes {
			host, port, err := net.SplitHostPort(n)
			if err != nil {
				continue
			}
			p, err := netip.ParseAddrPort(net.JoinHostPort("0.0.0.0", port))
			if err != nil {
				continue
			}
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				continue
			}
			for _, a := range addrs {
				e.nodeAddrs[netip.AddrPortFrom(a.Unmap(), p.Port())] = true
			}
		}
	})
	return e.nodeAddrs[ap]
}

// checkURLHost refuses, when a webhook is saved, an endpoint whose host is
// an internal address literal (or localhost); the names are checked when
// they are dialed.
func (e *egress) checkURLHost(host string) error {
	h := strings.Trim(strings.ToLower(host), "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		h = "127.0.0.1"
	}
	a, err := netip.ParseAddr(h)
	if err != nil {
		return nil
	}
	if internalAddr(a) && !e.allowedAddr(a) {
		return fmt.Errorf("%w: %s is an internal address (allow its network with --cache-webhook-allow)", errWebhookTarget, host)
	}
	return nil
}

// client returns the HTTP client of the webhooks: no proxy, no redirect,
// and every connection checked by the egress policy when it is dialed.
func (e *egress) client() *http.Client {
	dialer := &net.Dialer{
		Timeout: webhookTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", errWebhookTarget, address)
			}
			return e.checkAddr(ap)
		},
	}
	return &http.Client{
		Timeout: webhookTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        16,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
