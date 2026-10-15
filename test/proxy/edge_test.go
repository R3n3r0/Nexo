//go:build proxy

package proxy_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
	"github.com/stretchr/testify/require"
)

// Headers of the edges.
const (
	// headerUpstream is set on every answer of an edge to the upstream
	// node that served it (nginx $upstream_addr).
	headerUpstream = "X-Upstream"
	// headerPin asks an edge to send the request to one node only (a
	// test hook: it checks every node through the real proxy path). The
	// edge removes it.
	headerPin = "X-Test-Upstream"
)

// edgeMode is how an edge tells Nexo who the client is.
type edgeMode int

const (
	// forwardHeaders sets X-Forwarded-For (appended to the one of the
	// client), X-Forwarded-Proto and X-Forwarded-Host, as nginx with
	// proxy_add_x_forwarded_for, Caddy and Traefik do.
	forwardHeaders edgeMode = iota
	// proxyProtocolV2 opens every upstream connection with a PROXY
	// protocol v2 header carrying the client address and the SSL TLV of
	// its TLS connection (HAProxy send-proxy-v2-ssl), one connection per
	// request (HAProxy never shares a connection opened with a PROXY
	// header between clients). The forwarding headers of the client go
	// through as sent: Nexo must ignore them.
	proxyProtocolV2
)

// edgeConfig describes an edge proxy.
type edgeConfig struct {
	name string
	// ip is the address the edge listens on and opens its upstream
	// connections from (a trusted proxy of the deployment).
	ip string
	// domain is the parent domain the edge serves (".alpha.test"): any
	// other host is refused with 421, as a server block per domain does.
	domain string
	mode   edgeMode
	// secretHeader and secret are the origin secret the edge adds to every
	// request, replacing the value a client may send.
	secretHeader, secret string
	cert                 tls.Certificate
	// s3 returns the S3 listeners of the nodes; console the console
	// listeners, served on consoleHost (nil: no console on this edge).
	s3          func() []string
	console     func() []string
	consoleHost string
}

// edge is an in-process stand-in of nginx or HAProxy in front of the
// nodes: it terminates TLS for its domains, adds its origin secret,
// balances over the nodes (round robin) and fails over like nginx
// proxy_next_upstream: a connection refused is retried on the next node
// for any request (nothing was sent), an error or a 502/503/504 only for
// the idempotent bodyless ones; the nodes failing their readiness probe
// are tried last.
type edge struct {
	cfg  edgeConfig
	addr string // host:port the viewers connect to
	port string

	dialer    *net.Dialer
	transport *http.Transport // forwardHeaders: keep-alive pool
	s3        *pool
	console   *pool
	srv       *http.Server
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	// failures counts the requests the edge could not serve (502),
	// lastFailure describes the last one.
	failures    atomic.Int64
	lastFailure atomic.Value
}

// failed reports the requests the edge could not serve.
func (e *edge) failed() (int64, any) { return e.failures.Load(), e.lastFailure.Load() }

// newEdge listens on cfg.ip and serves until the test ends.
func newEdge(t *testing.T, cfg edgeConfig) *edge {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.ip, "0"))
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	e := &edge{
		cfg: cfg, addr: ln.Addr().String(), port: port, cancel: cancel,
		dialer: &net.Dialer{Timeout: 3 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(cfg.ip)}},
	}
	e.transport = &http.Transport{
		Proxy: nil, DialContext: e.dialer.DialContext,
		MaxIdleConnsPerHost: 64, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
	}
	e.s3 = &pool{targets: cfg.s3}
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.s3.probe(ctx, e.dialer) }()
	if cfg.console != nil {
		e.console = &pool{targets: cfg.console}
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.console.probe(ctx, e.dialer) }()
	}
	s3Proxy, consoleProxy := e.reverseProxy(e.s3), e.reverseProxy(e.console)
	e.srv = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			host = strings.ToLower(host)
			switch {
			case !strings.HasSuffix(host, cfg.domain):
				http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			case e.console != nil && host == cfg.consoleHost:
				consoleProxy.ServeHTTP(w, r)
			default:
				s3Proxy.ServeHTTP(w, r)
			}
		}),
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cfg.cert}, MinVersion: tls.VersionTLS12})
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		_ = e.srv.Serve(tlsLn)
	}()
	t.Cleanup(e.close)
	return e
}

// close stops the edge.
func (e *edge) close() {
	e.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = e.srv.Shutdown(ctx)
	cancel()
	_ = e.srv.Close()
	e.transport.CloseIdleConnections()
	e.wg.Wait()
}

// url is the base URL of host on the edge ("https://www.alpha.test:port").
func (e *edge) url(host string) string { return "https://" + host + ":" + e.port }

// reverseProxy returns the proxy of a pool of upstreams.
func (e *edge) reverseProxy(p *pool) http.Handler {
	if p == nil {
		return nil
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "upstream" // chosen by the balancer
			// The Host header is forwarded unchanged: SigV4 signs it.
			pr.Out.Host = pr.In.Host
			switch e.cfg.mode {
			case forwardHeaders:
				pr.SetXForwarded()
			case proxyProtocolV2:
				for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
					if v, ok := pr.In.Header[h]; ok {
						pr.Out.Header[h] = v
					}
				}
			}
			pr.Out.Header.Set(e.cfg.secretHeader, e.cfg.secret)
		},
		Transport: &upstream{edge: e, pool: p},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set(headerUpstream, resp.Request.URL.Host)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// A client that went away is not a failure of the upstreams.
			if r.Context().Err() == nil {
				e.failures.Add(1)
				e.lastFailure.Store(fmt.Sprintf("%s %s%s: %v", r.Method, r.Host, r.URL.RequestURI(), err))
			}
			http.Error(w, "edge "+e.cfg.name+": "+err.Error(), http.StatusBadGateway)
		},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
}

// roundTripper returns the transport of a request: the shared pool of
// connections, or a connection of its own opened with the PROXY header of
// its client.
func (e *edge) roundTripper(req *http.Request) http.RoundTripper {
	if e.cfg.mode != proxyProtocolV2 {
		return e.transport
	}
	client, _ := net.ResolveTCPAddr("tcp", req.RemoteAddr)
	local, _ := net.ResolveTCPAddr("tcp", e.addr)
	return &http.Transport{
		Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := e.dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if err := writeProxyHeader(conn, client, local, true); err != nil {
				_ = conn.Close()
				return nil, &net.OpError{Op: "dial", Net: network, Err: err}
			}
			return conn, nil
		},
	}
}

// writeProxyHeader writes a PROXY protocol v2 header for a TCP connection
// of src to dst, with the SSL TLV of a client connection over TLS when
// secure.
func writeProxyHeader(w io.Writer, src, dst *net.TCPAddr, secure bool) error {
	if src == nil || dst == nil {
		return errors.New("PROXY header: missing address")
	}
	h := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4, SourceAddr: src, DestinationAddr: dst}
	if src.IP.To4() == nil {
		h.TransportProtocol = proxyproto.TCPv6
	}
	if secure {
		tlv, err := tlvparse.PP2SSL{Client: tlvparse.PP2_BITFIELD_CLIENT_SSL, Verify: 0, TLV: []proxyproto.TLV{
			{Type: proxyproto.PP2_SUBTYPE_SSL_VERSION, Value: []byte("TLSv1.3")},
		}}.Marshal()
		if err != nil {
			return err
		}
		if err := h.SetTLVs([]proxyproto.TLV{tlv}); err != nil {
			return err
		}
	}
	_, err := h.WriteTo(w)
	return err
}

// upstream sends the requests of an edge to the nodes of a pool.
type upstream struct {
	edge *edge
	pool *pool
}

// RoundTrip implements http.RoundTripper.
func (u *upstream) RoundTrip(req *http.Request) (*http.Response, error) {
	targets := u.pool.order()
	if pin := req.Header.Get(headerPin); pin != "" {
		targets = []string{pin}
		req.Header.Del(headerPin)
	}
	if len(targets) == 0 {
		return nil, errors.New("no upstream")
	}
	retryable := (req.Method == http.MethodGet || req.Method == http.MethodHead || req.Method == http.MethodOptions) &&
		(req.Body == nil || req.Body == http.NoBody)
	var lastErr error
	for i, target := range targets {
		r := req.Clone(req.Context())
		r.Body = req.Body
		r.URL.Host = target
		resp, err := u.edge.roundTripper(req).RoundTrip(r)
		last := i == len(targets)-1
		if err != nil {
			var opErr *net.OpError
			if (errors.As(err, &opErr) && opErr.Op == "dial") || retryable {
				lastErr = fmt.Errorf("%s: %w", target, err)
				continue
			}
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			if retryable && !last {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				lastErr = fmt.Errorf("%s: %s", target, resp.Status)
				continue
			}
		}
		return resp, nil
	}
	return nil, lastErr
}

// pool balances over the upstreams of an edge.
type pool struct {
	targets func() []string
	counter atomic.Uint64
	// down holds the upstreams that failed their last readiness probe.
	down sync.Map
}

// order returns the upstreams in the order a request tries them: the
// next one in turn first, the ones found down last.
func (p *pool) order() []string {
	targets := p.targets()
	if len(targets) == 0 {
		return nil
	}
	start := p.counter.Add(1)
	out := make([]string, 0, len(targets))
	var down []string
	for i := range targets {
		target := targets[(start+uint64(i))%uint64(len(targets))]
		if _, ok := p.down.Load(target); ok {
			down = append(down, target)
			continue
		}
		out = append(out, target)
	}
	return append(out, down...)
}

// probeInterval is the period of the readiness probes of the edges.
const probeInterval = 100 * time.Millisecond

// probe checks the readiness of every upstream each probeInterval (nginx
// max_fails/fail_timeout, HAProxy check).
func (p *pool) probe(ctx context.Context, dialer *net.Dialer) {
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}}
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		var wg sync.WaitGroup
		for _, target := range p.targets() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/nexo/health/ready", nil)
				if err != nil {
					return
				}
				resp, err := client.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				switch {
				case err == nil && resp.StatusCode == http.StatusOK:
					p.down.Delete(target)
				case ctx.Err() == nil:
					p.down.Store(target, true)
				}
			}()
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// isDown reports whether the last probe of target failed.
func (p *pool) isDown(target string) bool {
	_, ok := p.down.Load(target)
	return ok
}
