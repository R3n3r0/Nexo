package testutil

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"testing"
	"time"
)

// startLB starts the load balancer of an in-process cluster: two reverse
// proxies, for the S3 and the console listeners of the nodes, that keep
// the Host header and every client header (X-Forwarded-* included), as
// nginx in front of a cluster does (docs/08 §8.4: the SigV4 signature
// covers the Host). The S3 balancer sends every request to the next node
// in turn (round robin); the console balancer keeps each client on one
// node (hash of the client address, nginx ip_hash), since the login
// throttle of the console counts per node. A node that refuses the
// connection (stopped) is skipped. S3URL and ConsoleURL point to the
// balancer.
func (s *Server) startLB(t testing.TB, s3Addr, consoleAddr string, nodes int) {
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
	for _, lb := range []struct {
		addr    string
		console bool
	}{{s3Addr, false}, {consoleAddr, true}} {
		ln, err := net.Listen("tcp", lb.addr)
		if err != nil {
			t.Fatalf("testutil: load balancer: %v", err)
		}
		rr := &balancer{targets: targets(lb.console), sticky: lb.console, next: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     30 * time.Second,
		}}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.Out.URL.Scheme = "http"
				r.Out.URL.Host = "balanced" // chosen by the balancer
				r.Out.Host = r.In.Host
				// Transparent: the forwarding headers of the client go
				// through as they are (the nodes do not trust the
				// balancer, only the proxies a test configures).
				for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Forwarded"} {
					if v, ok := r.In.Header[h]; ok {
						r.Out.Header[h] = v
					}
				}
			},
			Transport:     rr,
			FlushInterval: -1,
			ErrorLog:      log.New(io.Discard, "", 0),
		}
		hs := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = hs.Serve(ln) }()
		s.lb = append(s.lb, hs)
		if lb.console {
			s.ConsoleURL = "http://" + ln.Addr().String()
		} else {
			s.S3URL = "http://" + ln.Addr().String()
		}
	}
}

// closeLB stops the load balancer.
func (s *Server) closeLB() {
	for _, hs := range s.lb {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = hs.Shutdown(ctx)
		cancel()
		_ = hs.Close()
	}
	s.lb = nil
}

// balancer sends each request to a target: the next one in turn, or the
// one of the client (sticky). A target that refuses the connection is
// skipped (nothing of the request was sent).
type balancer struct {
	targets func() []string
	sticky  bool
	counter atomic.Uint64
	next    http.RoundTripper
}

func (b *balancer) RoundTrip(req *http.Request) (*http.Response, error) {
	targets := b.targets()
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
	var lastErr error
	for i := range targets {
		r := req.Clone(req.Context())
		r.Body = req.Body
		r.URL.Host = targets[(start+uint64(i))%uint64(len(targets))]
		resp, err := b.next.RoundTrip(r)
		var opErr *net.OpError
		if err != nil && errors.As(err, &opErr) && opErr.Op == "dial" {
			lastErr = err
			continue
		}
		return resp, err
	}
	return nil, lastErr
}
