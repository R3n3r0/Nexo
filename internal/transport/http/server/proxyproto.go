package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// defaultProxyHeaderTimeout bounds the read of a PROXY protocol header
// when the server has no header timeout of its own.
const defaultProxyHeaderTimeout = 10 * time.Second

// WithProxyProtocol accepts the PROXY protocol (v1 and v2) on the
// listener from the trusted proxies, given as IPs or CIDRs: a connection
// of a trusted proxy may start with a PROXY header, whose source becomes
// the remote address of its requests (and, for a v2 header carrying the
// SSL information, makes them secure; see middleware.ProxyConn). The
// header of any other peer is read and ignored: the requests keep the
// address of the peer. A connection of a trusted proxy without header is
// served as a direct one. An invalid proxy fails Start
// (domain.ErrInvalidArgument).
func WithProxyProtocol(trusted ...string) Option {
	return func(s *service) {
		s.proxyProtocol = true
		s.proxyTrusted = append(s.proxyTrusted, trusted...)
	}
}

// proxyListener wraps the TCP listener with the PROXY protocol, before
// TLS: the header precedes the TLS handshake.
func (s *service) proxyListener(ln net.Listener) (net.Listener, error) {
	trusted := make([]netip.Prefix, 0, len(s.proxyTrusted))
	for _, p := range s.proxyTrusted {
		prefix, err := parsePrefix(p)
		if err != nil {
			return nil, fmt.Errorf("server %s: %w", s.name, err)
		}
		trusted = append(trusted, prefix)
	}
	timeout := s.readHeaderTimeout
	if timeout <= 0 {
		timeout = defaultProxyHeaderTimeout
	}
	return &proxyproto.Listener{
		Listener: ln,
		ConnPolicy: func(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			if isTrustedAddr(trusted, o.Upstream) {
				return proxyproto.USE, nil
			}
			return proxyproto.IGNORE, nil
		},
		ReadHeaderTimeout: timeout,
	}, nil
}

// proxyConnContext stores, in the context of a connection, the
// description of its PROXY header, read lazily by the request handlers
// (the accept loop never waits for a header).
func proxyConnContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	pc, ok := c.(*proxyproto.Conn)
	if !ok {
		return ctx
	}
	return middleware.WithProxyConn(ctx, func() (middleware.ProxyConn, bool) {
		h := pc.ProxyHeader()
		if h == nil || h.Command.IsLocal() {
			return middleware.ProxyConn{}, false
		}
		out := middleware.ProxyConn{Proxy: pc.Raw().RemoteAddr().String()}
		if tlvs, err := h.TLVs(); err == nil {
			if ssl, found := tlvparse.FindSSL(tlvs); found && ssl.ClientSSL() {
				out.Secure = true
			}
		}
		return out, true
	})
}

// parsePrefix parses a trusted proxy: an IP is a single-address prefix.
func parsePrefix(s string) (netip.Prefix, error) {
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

// isTrustedAddr reports whether the address of a peer belongs to a
// trusted proxy.
func isTrustedAddr(trusted []netip.Prefix, addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return false
	}
	a := ap.Addr().Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
