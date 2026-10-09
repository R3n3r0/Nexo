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

// ForwardedHeaders selects the headers the trusted proxies describe the
// client with.
type ForwardedHeaders string

// Header families of the trusted proxies.
const (
	// HeadersXForwarded reads X-Forwarded-For, X-Forwarded-Proto and
	// X-Forwarded-Host (the default: nginx, Traefik, Caddy, HAProxy
	// "option forwardfor", Cloudflare).
	HeadersXForwarded ForwardedHeaders = "x-forwarded"
	// HeadersForwarded reads the Forwarded header of RFC 7239 (for=,
	// proto=, host=).
	HeadersForwarded ForwardedHeaders = "forwarded"
)

// WithTrustedProxies sets the reverse proxies, as IPs or CIDRs, whose
// forwarding headers are trusted. Without them the client is the peer of
// the connection.
func WithTrustedProxies(proxies ...string) ForwardedOption {
	return func(f *forwarded) { f.raw = append(f.raw, proxies...) }
}

// WithForwardedHeaders selects the header family the trusted proxies set
// (default HeadersXForwarded). Only that family is read: a proxy forwards
// the other one as the client sent it, so reading both would let a
// client forge its address.
func WithForwardedHeaders(h ForwardedHeaders) ForwardedOption {
	return func(f *forwarded) {
		if h != "" {
			f.headers = h
		}
	}
}

type forwarded struct {
	raw     []string
	trusted []netip.Prefix
	headers ForwardedHeaders
}

var _ Middleware = (*forwarded)(nil)

// NewForwarded returns the middleware that derives the client of a request
// (ClientFromContext): the peer of the connection (the source of the
// PROXY protocol header of a trusted proxy, see server.WithProxyProtocol)
// or, when the peer is a trusted proxy, the nearest untrusted address of
// the forwarding headers (read from the right, so that a client cannot
// forge the entries the proxies append), the scheme and the host they
// forward. An invalid proxy or header family is a wiring error
// (domain.ErrInvalidArgument).
func NewForwarded(_ ForwardedDeps, opts ...ForwardedOption) (Middleware, error) {
	f := &forwarded{headers: HeadersXForwarded}
	for _, o := range opts {
		o(f)
	}
	switch f.headers {
	case HeadersXForwarded, HeadersForwarded:
	default:
		return nil, fmt.Errorf("%w: forwarded headers %q (x-forwarded or forwarded)", domain.ErrInvalidArgument, f.headers)
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
	// or to a trusted proxy that says so (X-Forwarded-Proto, Forwarded
	// proto=, or the SSL information of a PROXY protocol v2 header).
	Secure bool
	// Host is the host the client addressed, as sent (with its port, if
	// any): the Host header, or the host a trusted proxy forwarded
	// (X-Forwarded-Host, Forwarded host=). The routing of the
	// distributions resolves it.
	Host string
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
	return Client{IP: peerIP(r), Secure: r.TLS != nil, Host: r.Host}
}

// HostOf returns the host the client of a request addressed (Client.Host).
func HostOf(r *http.Request) string { return ClientOf(r).Host }

// ProxyConn describes a connection that started with the PROXY protocol
// header of a trusted proxy (server.WithProxyProtocol).
type ProxyConn struct {
	// Proxy is the address of the proxy, the peer of the connection.
	Proxy string
	// Secure reports a client connection the proxy received over TLS
	// (the SSL information of a PROXY protocol v2 header).
	Secure bool
}

// ProxyConnFunc describes the PROXY protocol header of a connection; ok
// is false when the connection did not start with one. It is called by
// the request handlers, once the header was read.
type ProxyConnFunc func() (ProxyConn, bool)

type proxyConnKey struct{}

// WithProxyConn stores the PROXY protocol description of a connection in
// its context (http.Server.ConnContext).
func WithProxyConn(ctx context.Context, fn ProxyConnFunc) context.Context {
	return context.WithValue(ctx, proxyConnKey{}, fn)
}

// ProxyConnFromContext returns the PROXY protocol header of the connection
// of a request; ok is false without one.
func ProxyConnFromContext(ctx context.Context) (ProxyConn, bool) {
	fn, ok := ctx.Value(proxyConnKey{}).(ProxyConnFunc)
	if !ok || fn == nil {
		return ProxyConn{}, false
	}
	return fn()
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

// hop is an entry of the forwarding headers.
type hop struct {
	addr  string
	proto string
	host  string
}

// client derives the client of r. The remote address is the peer of the
// connection, or the source of the PROXY protocol header (already
// substituted by the listener); a PROXY header over TLS terminated by the
// proxy makes the request secure.
func (f *forwarded) client(r *http.Request) Client {
	c := Client{IP: peerIP(r), Secure: r.TLS != nil, Host: r.Host}
	if pc, ok := ProxyConnFromContext(r.Context()); ok && pc.Secure {
		c.Secure = true
	}
	if !f.isTrusted(c.IP) {
		return c
	}
	var hops []hop
	if f.headers == HeadersForwarded {
		hops = forwardedHops(r.Header.Values("Forwarded"))
	} else {
		hops = xForwardedHops(r.Header)
	}
	if len(hops) == 0 {
		return c
	}
	client := hops[len(hops)-1]
	for i := len(hops) - 1; i >= 0; i-- {
		client = hops[i]
		if !f.isTrusted(hops[i].addr) {
			break
		}
	}
	if client.addr != "" {
		c.IP = client.addr
	}
	switch strings.ToLower(client.proto) {
	case "https":
		c.Secure = true
	case "http":
		c.Secure = r.TLS != nil
	}
	if client.host != "" {
		c.Host = client.host
	}
	return c
}

// xForwardedHops returns the hops of the X-Forwarded-* headers: the valid
// addresses of X-Forwarded-For in order (an invalid entry ends the list
// there: nothing to its left can be trusted), the scheme and the host the
// proxy facing this server forwarded (the last X-Forwarded-Proto and
// X-Forwarded-Host values) on the nearest hop.
func xForwardedHops(h http.Header) []hop {
	var out []hop
	for _, v := range h.Values("X-Forwarded-For") {
		for _, entry := range strings.Split(v, ",") {
			a, err := netip.ParseAddr(strings.TrimSpace(entry))
			if err != nil {
				out = out[:0]
				continue
			}
			out = append(out, hop{addr: a.Unmap().String()})
		}
	}
	proto, host := lastValue(h.Values("X-Forwarded-Proto")), lastValue(h.Values("X-Forwarded-Host"))
	if len(out) == 0 {
		if proto == "" && host == "" {
			return nil
		}
		// The scheme and host of a trusted proxy that did not name the
		// client: the client is the proxy itself.
		return []hop{{proto: proto, host: host}}
	}
	for i := range out {
		out[i].proto, out[i].host = proto, host
	}
	return out
}

// lastValue returns the last comma-separated value of the last header.
func lastValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	list := strings.Split(values[len(values)-1], ",")
	return strings.TrimSpace(list[len(list)-1])
}

// forwardedHops parses the Forwarded headers (RFC 7239): one hop per
// element, with its for=, proto= and host= parameters. An element whose
// for= is not an address ("unknown", an obfuscated name, a malformed
// value) ends the list there, like an invalid X-Forwarded-For entry.
func forwardedHops(values []string) []hop {
	var out []hop
	for _, v := range values {
		for _, element := range splitQuoted(v, ',') {
			var h hop
			valid := false
			for _, pair := range splitQuoted(element, ';') {
				name, value, ok := strings.Cut(pair, "=")
				if !ok {
					continue
				}
				value = unquote(strings.TrimSpace(value))
				switch strings.ToLower(strings.TrimSpace(name)) {
				case "for":
					if a, ok := nodeAddr(value); ok {
						h.addr, valid = a, true
					}
				case "proto":
					h.proto = value
				case "host":
					h.host = value
				}
			}
			if !valid {
				out = out[:0]
				continue
			}
			out = append(out, h)
		}
	}
	return out
}

// nodeAddr parses the node of a for= parameter: an IPv4 address, an IPv6
// address in brackets, either with an optional port.
func nodeAddr(node string) (string, bool) {
	if strings.HasPrefix(node, "[") {
		end := strings.IndexByte(node, ']')
		if end < 0 {
			return "", false
		}
		node = node[1:end]
	} else if host, _, err := net.SplitHostPort(node); err == nil {
		node = host
	}
	a, err := netip.ParseAddr(node)
	if err != nil || a.Zone() != "" {
		return "", false
	}
	return a.Unmap().String(), true
}

// splitQuoted splits s on sep outside of quoted strings.
func splitQuoted(s string, sep byte) []string {
	var out []string
	quoted, escaped, start := false, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case c == '\\' && quoted:
			escaped = true
		case c == '"':
			quoted = !quoted
		case c == sep && !quoted:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// unquote removes the quotes and escapes of a quoted string.
func unquote(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	s = s[1 : len(s)-1]
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	escaped := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && !escaped {
			escaped = true
			continue
		}
		escaped = false
		b.WriteByte(s[i])
	}
	return b.String()
}

// isTrusted reports whether ip belongs to a trusted proxy.
func (f *forwarded) isTrusted(ip string) bool {
	if len(f.trusted) == 0 || ip == "" {
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
