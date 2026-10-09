package testutil

import (
	"context"
	"crypto/tls"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// LoadBalancer is a reverse proxy in front of the nodes of a cluster, as
// nginx in front of a deployment (docs/08 §8.4): it keeps the Host header
// and every client header (X-Forwarded-* included), since the SigV4
// signature covers the Host. It sends every request to the next node in
// turn (round robin) or, when sticky, keeps each client on one node (hash
// of the client address, nginx ip_hash). A node that refuses the
// connection (stopped) is skipped; with a health check, so is a node that
// does not answer its liveness probe (paused).
type LoadBalancer struct {
	// URL is the base URL of the balancer (http://127.0.0.1:port).
	URL string

	srv    *http.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// lbSettings collects the options of NewLoadBalancer.
type lbSettings struct {
	addr     string
	sticky   bool
	health   string
	interval time.Duration
	// serverTLS and clientTLS serve the balancer and reach the targets
	// over TLS (LBTLS).
	serverTLS *tls.Config
	clientTLS *tls.Config
}

// LBOption customizes NewLoadBalancer.
type LBOption func(*lbSettings)

// LBAddr sets the listen address of the balancer (default a free port of
// 127.0.0.1).
func LBAddr(addr string) LBOption {
	return func(s *lbSettings) { s.addr = addr }
}

// LBSticky keeps every client on one node (hash of its address).
func LBSticky() LBOption {
	return func(s *lbSettings) { s.sticky = true }
}

// LBHealthCheck probes path on every node each interval and skips the
// nodes that do not answer 200 in time, unless none does.
func LBHealthCheck(path string, interval time.Duration) LBOption {
	return func(s *lbSettings) { s.health, s.interval = path, interval }
}

// NewLoadBalancer starts a load balancer in front of the "host:port"
// addresses targets returns (asked again for every request, so that the
// nodes may change). It stops when the test ends, or with Close.
func NewLoadBalancer(t testing.TB, targets func() []string, opts ...LBOption) *LoadBalancer {
	t.Helper()
	st := lbSettings{addr: "127.0.0.1:0"}
	for _, o := range opts {
		o(&st)
	}
	ln, err := net.Listen("tcp", st.addr)
	if err != nil {
		t.Fatalf("testutil: load balancer: %v", err)
	}
	scheme := "http"
	if st.serverTLS != nil {
		ln = tls.NewListener(ln, st.serverTLS)
		scheme = "https"
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
		TLSClientConfig:     st.clientTLS,
	}
	targetScheme := "http"
	if st.clientTLS != nil {
		targetScheme = "https"
	}
	b := &balancer{targets: targets, sticky: st.sticky, next: transport}
	lb := &LoadBalancer{URL: scheme + "://" + ln.Addr().String(), cancel: cancel}
	if st.health != "" {
		b.down = &sync.Map{}
		lb.wg.Add(1)
		go func() {
			defer lb.wg.Done()
			b.probe(ctx, st.health, st.interval)
		}()
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = targetScheme
			r.Out.URL.Host = "balanced" // chosen by the balancer
			r.Out.Host = r.In.Host
			// Transparent: the forwarding headers of the client go
			// through as they are (the nodes do not trust the balancer,
			// only the proxies a test configures).
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
				if v, ok := r.In.Header[h]; ok {
					r.Out.Header[h] = v
				}
			}
		},
		Transport:     b,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	lb.srv = &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	lb.wg.Add(1)
	go func() {
		defer lb.wg.Done()
		_ = lb.srv.Serve(ln)
	}()
	t.Cleanup(lb.Close)
	return lb
}

// Close stops the balancer. It is idempotent.
func (lb *LoadBalancer) Close() {
	lb.once.Do(func() {
		lb.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = lb.srv.Shutdown(ctx)
		cancel()
		_ = lb.srv.Close()
		lb.wg.Wait()
	})
}

// startLB starts the load balancers of an in-process cluster, for the S3
// and the console listeners of the nodes: round robin for S3, sticky for
// the console (its login throttle counts per node). S3URL and ConsoleURL
// point to them.
func (s *Server) startLB(t testing.TB, s3Addr, consoleAddr string, nodes int, opts ...LBOption) {
	t.Helper()
	targets := func(console bool) func() []string {
		return func() []string {
			s.mu.Lock()
			defer s.mu.Unlock()
			out := make([]string, 0, nodes)
			for _, n := range s.Nodes {
				if console {
					out = append(out, n.Config.ConsoleAddress)
				} else {
					out = append(out, n.Config.Address)
				}
			}
			return out
		}
	}
	s3LB := NewLoadBalancer(t, targets(false), append([]LBOption{LBAddr(s3Addr)}, opts...)...)
	consLB := NewLoadBalancer(t, targets(true), append([]LBOption{LBAddr(consoleAddr), LBSticky()}, opts...)...)
	s.lb = append(s.lb, s3LB, consLB)
	s.S3URL, s.ConsoleURL = s3LB.URL, consLB.URL
}

// closeLB stops the load balancers.
func (s *Server) closeLB() {
	for _, lb := range s.lb {
		lb.Close()
	}
	s.lb = nil
}

// balancer sends each request to a target: the next one in turn, or the
// one of the client (sticky). A target that refuses the connection is
// skipped (nothing of the request was sent), and so are the targets the
// health check found down, unless every target is.
type balancer struct {
	targets func() []string
	sticky  bool
	counter atomic.Uint64
	next    http.RoundTripper
	// down holds the targets that failed their last probe (nil without a
	// health check).
	down *sync.Map
}

// probe checks the liveness of every target each interval.
func (b *balancer) probe(ctx context.Context, path string, interval time.Duration) {
	client := &http.Client{Timeout: max(interval, 200*time.Millisecond)}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var wg sync.WaitGroup
		for _, target := range b.targets() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+path, nil)
				if err != nil {
					return
				}
				resp, err := client.Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				if err == nil && resp.StatusCode == http.StatusOK {
					b.down.Delete(target)
				} else if ctx.Err() == nil {
					b.down.Store(target, true)
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

// order returns the targets in the order they are tried for req: the
// healthy ones first.
func (b *balancer) order(req *http.Request) []string {
	targets := b.targets()
	if len(targets) == 0 {
		return nil
	}
	var start uint64
	if b.sticky {
		client := req.Header.Get("X-Forwarded-For")
		if client == "" {
			client = req.RemoteAddr
			if host, _, err := net.SplitHostPort(client); err == nil {
				client = host
			}
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(client))
		start = h.Sum64()
	} else {
		start = b.counter.Add(1)
	}
	out := make([]string, 0, len(targets))
	var down []string
	for i := range targets {
		target := targets[(start+uint64(i))%uint64(len(targets))]
		if b.down != nil {
			if _, ok := b.down.Load(target); ok {
				down = append(down, target)
				continue
			}
		}
		out = append(out, target)
	}
	return append(out, down...)
}

// RoundTrip implements http.RoundTripper.
func (b *balancer) RoundTrip(req *http.Request) (*http.Response, error) {
	var lastErr error
	for _, target := range b.order(req) {
		r := req.Clone(req.Context())
		r.Body = req.Body
		r.URL.Host = target
		resp, err := b.next.RoundTrip(r)
		var opErr *net.OpError
		if err != nil && errors.As(err, &opErr) && opErr.Op == "dial" {
			lastErr = err
			continue
		}
		return resp, err
	}
	if lastErr == nil {
		lastErr = errors.New("testutil: load balancer: no target")
	}
	return nil, lastErr
}
