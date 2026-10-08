package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ForwardedDeps are the dependencies of the forwarded-client middleware
// (none: the struct keeps the constructor shape of every middleware).
type ForwardedDeps struct{}

// ForwardedOption configures the forwarded-client middleware.
type ForwardedOption func(*forwarded)

// WithTrustedProxies sets the reverse proxies, as IPs or CIDRs, whose
// X-Forwarded-For and X-Forwarded-Proto headers are trusted. Without them
// the client is the peer of the connection.
func WithTrustedProxies(proxies ...string) ForwardedOption {
	return func(f *forwarded) { f.raw = append(f.raw, proxies...) }
}

type forwarded struct {
	raw     []string
	trusted []netip.Prefix
}

var _ Middleware = (*forwarded)(nil)

// NewForwarded returns the middleware that derives the client of a request
// (ClientFromContext): the peer of the connection, or, when the peer is a
// trusted proxy, the nearest untrusted address of X-Forwarded-For (read
// from the right, so that a client cannot forge the entries the proxies
// append) and the scheme of X-Forwarded-Proto. An invalid proxy is a
// wiring error (domain.ErrInvalidArgument).
func NewForwarded(_ ForwardedDeps, opts ...ForwardedOption) (Middleware, error) {
	f := &forwarded{}
	for _, o := range opts {
		o(f)
	}
	for _, p := range f.raw {
		prefix, err := parseProxy(p)
		if err != nil {
			return nil, err
		}
		f.trusted = append(f.trusted, prefix)
	}
	return f, nil
}

// parseProxy parses a trusted proxy: an IP is a single-address prefix.
func parseProxy(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: trusted proxy %q is neither an IP nor a CIDR", domain.ErrInvalidArgument, s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Client is the client of a request as seen through the trusted proxies.
type Client struct {
	// IP is the address of the client, without port.
	IP string
	// Secure reports a request the client sent over TLS: to this server,
	// or to a trusted proxy that says so in X-Forwarded-Proto.
	Secure bool
}

type clientKey struct{}

// WithClient stores the client of a request in the context.
func WithClient(ctx context.Context, c Client) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

// ClientFromContext returns the client stored by NewForwarded.
func ClientFromContext(ctx context.Context) (Client, bool) {
	c, ok := ctx.Value(clientKey{}).(Client)
	return c, ok
}

// ClientOf returns the client of a request: the one derived by
// NewForwarded, or the peer of the connection when the middleware did not
// run.
func ClientOf(r *http.Request) Client {
	if c, ok := ClientFromContext(r.Context()); ok {
		return c
	}
	return Client{IP: peerIP(r), Secure: r.TLS != nil}
}

// peerIP is the address of the peer of the connection, without port.
func peerIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// Wrap implements Middleware.
func (f *forwarded) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(WithClient(r.Context(), f.client(r))))
	})
}

// client derives the client of r.
func (f *forwarded) client(r *http.Request) Client {
	c := Client{IP: peerIP(r), Secure: r.TLS != nil}
	if !f.isTrusted(c.IP) {
		return c
	}
	hops := forwardedFor(r.Header.Values("X-Forwarded-For"))
	for i := len(hops) - 1; i >= 0; i-- {
		c.IP = hops[i]
		if !f.isTrusted(hops[i]) {
			break
		}
	}
	if protos := r.Header.Values("X-Forwarded-Proto"); len(protos) > 0 {
		// The last proxy appends last; it is the one facing this server.
		list := strings.Split(protos[len(protos)-1], ",")
		switch strings.ToLower(strings.TrimSpace(list[len(list)-1])) {
		case "https":
			c.Secure = true
		case "http":
			c.Secure = r.TLS != nil
		}
	}
	return c
}

// forwardedFor returns the valid addresses of the X-Forwarded-For headers,
// in order; an invalid entry ends the list there (nothing to its left can
// be trusted).
func forwardedFor(values []string) []string {
	var out []string
	for _, v := range values {
		for _, hop := range strings.Split(v, ",") {
			a, err := netip.ParseAddr(strings.TrimSpace(hop))
			if err != nil {
				out = out[:0]
				continue
			}
			out = append(out, a.Unmap().String())
		}
	}
	return out
}

// isTrusted reports whether ip belongs to a trusted proxy.
func (f *forwarded) isTrusted(ip string) bool {
	if len(f.trusted) == 0 {
		return false
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range f.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
