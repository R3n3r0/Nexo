// Package testutil starts Nexo in-process for integration tests: free
// ports, temporary drive directories and test credentials, in every
// deployment mode (docs/08 §8.1): a single drive (SNSD), several drives
// of one node (SNMD, Drives) or a distributed cluster of several nodes
// running in the test process (StartCluster), each on its own ports, in
// front of an in-process load balancer. The Server exposes the services
// of the running application (IAM, policies, buckets, share links,
// signer) so that tests provision identities and policies without going
// through the Admin API, and can stop and restart the application on the
// same data directory.
package testutil

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// Test credentials.
const (
	RootUser     = "nexoadmin"
	RootPassword = "nexoadmin-secret"
	// Domain is the virtual-host domain of the test servers (NEXO_DOMAIN):
	// "{bucket}.localhost" addresses a bucket.
	Domain = "localhost"
)

// stopTimeout bounds the shutdown of a test server.
const stopTimeout = 10 * time.Second

// startAttempts is how many times StartServer retries when a reserved port
// was taken by another process between its reservation and the listen.
const startAttempts = 3

// clusterStartTimeout bounds the bootstrap of an in-process cluster.
const clusterStartTimeout = 2 * time.Minute

// Node is one server of the deployment: the only one in the single-node
// modes, one of the cluster nodes otherwise.
type Node struct {
	App *app.App
	// S3URL and ConsoleURL are the base URLs of the node itself.
	S3URL      string
	ConsoleURL string
	Config     config.Static
	logger     *slog.Logger
}

// Server is a running in-process Nexo deployment.
type Server struct {
	// App is the application of the first node.
	App *app.App
	// S3URL and ConsoleURL are the base URLs (http://127.0.0.1:port) of the
	// deployment: the node of a single-node deployment, the load balancer
	// in front of the nodes of a cluster (round robin, the Host header is
	// preserved). They survive Restart: the servers listen on the same
	// ports again.
	S3URL      string
	ConsoleURL string
	// Client is an HTTP client with a cookie jar for console sessions.
	Client *http.Client
	// Dir is the temporary directory holding the data drives ("data" for
	// a single drive, "d1".."dN" for SNMD, "n<i>/d<j>" for a cluster).
	Dir          string
	RootUser     string
	RootPassword string
	// Config is the configuration of the first node.
	Config config.Static
	// Nodes are the servers of the deployment (one in the single-node
	// modes).
	Nodes []*Node

	// Services of the running application (of the first node), refreshed
	// by Restart.
	IAM      iam.Service
	Policies policy.Service
	Buckets  bucket.Service
	Shares   share.Service
	Signer   auth.Signer
	// ILM applies the bucket lifecycle at once (ApplyNow) and Events
	// manages the notification targets.
	ILM    ilm.Service
	Events event.Service
	// Clock is the clock of the application (WithClock); nil means the
	// wall clock.
	Clock clock.Clock
	// TLS is the configuration of the clients of a deployment served
	// over TLS (WithTLS): it trusts the test CA. Nil without TLS.
	TLS *tls.Config

	logger *slog.Logger
	mu     sync.Mutex
	// running reports that the nodes are started and not yet stopped.
	running bool
	lb      []*LoadBalancer
}

// settings collects the options of StartServer.
type settings struct {
	cfg    config.Static
	clock  clock.Clock
	dir    string
	drives int
	nodes  int
	// tls serves the deployment over TLS (WithTLS), with the certificates
	// of pki.
	tls bool
	pki *testPKI
}

// Option customizes StartServer.
type Option func(*settings)

// WithEndpoints replaces the single temporary drive with the given
// endpoints (e.g. four directories for SNMD).
func WithEndpoints(eps ...string) Option {
	return func(s *settings) { s.cfg.Endpoints = eps }
}

// Drives runs a single node with n temporary drives (SNMD with erasure
// coding when n > 1): Dir/d1 .. Dir/dn.
func Drives(n int) Option {
	return func(s *settings) { s.drives = n }
}

// WithConfig applies an arbitrary mutation to the configuration (of every
// node).
func WithConfig(fn func(*config.Static)) Option {
	return func(s *settings) { fn(&s.cfg) }
}

// WithClock sets the clock of the application (e.g. a clock.Fake to expire
// presigned URLs and STS sessions without waiting).
func WithClock(c clock.Clock) Option {
	return func(s *settings) { s.clock = c }
}

// WithDir uses dir instead of a fresh temporary directory: the drive is
// dir/data. It lets a test start a second server on the data of a first
// one.
func WithDir(dir string) Option {
	return func(s *settings) { s.dir = dir }
}

// StartServer builds and starts the application on free ports and stops it
// when the test ends. Logs go to the test log at debug level.
func StartServer(t testing.TB, opts ...Option) *Server {
	t.Helper()
	st := newSettings(opts)
	setupTLS(t, &st)
	if st.nodes > 0 {
		return startCluster(t, st)
	}
	if st.dir == "" {
		st.dir = t.TempDir()
	}
	if len(st.cfg.Endpoints) == 0 {
		st.cfg.Endpoints = []string{filepath.Join(st.dir, "data")}
		if st.drives > 1 {
			st.cfg.Endpoints = nil
			for i := 1; i <= st.drives; i++ {
				st.cfg.Endpoints = append(st.cfg.Endpoints, filepath.Join(st.dir, "d"+strconv.Itoa(i)))
			}
		}
	}
	srv := newServer(t, st)
	// The ports are reserved before Build so that the configuration names
	// them: the public URL of the share links and the restarts use them.
	for attempt := 1; ; attempt++ {
		cfg := st.cfg
		ports := reservePorts(t, 2)
		cfg.Address, cfg.ConsoleAddress = ports[0], ports[1]
		srv.Nodes = []*Node{{Config: cfg, logger: srv.logger}}
		err := srv.start()
		if err == nil {
			break
		}
		if attempt == startAttempts || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("testutil: %v", err)
		}
	}
	return srv
}

// StartCluster starts a distributed deployment of nodes servers with
// drivesPerNode drives each inside the test process: every node is an
// App on its own ports of 127.0.0.1, its drives are the endpoints
// http://127.0.0.1:<port>/<Dir>/n<i>/d<j> of a single pool, and the nodes
// reach each other through the internode RPC. The Server URLs are those of
// a load balancer in front of the nodes; Nodes gives each node. The
// cluster stops when the test ends.
func StartCluster(t testing.TB, nodes, drivesPerNode int, opts ...Option) *Server {
	t.Helper()
	opts = append(opts, func(s *settings) { s.nodes, s.drives = nodes, drivesPerNode })
	return StartServer(t, opts...)
}

func newSettings(opts []Option) settings {
	st := settings{cfg: config.Default()}
	st.cfg.RootUser = RootUser
	st.cfg.RootPassword = RootPassword
	st.cfg.LogLevel = "debug"
	st.cfg.Domain = Domain
	for _, o := range opts {
		o(&st)
	}
	return st
}

// setupTLS creates the certificates of a TLS deployment and points the
// configuration to them.
func setupTLS(t testing.TB, st *settings) {
	t.Helper()
	if !st.tls || st.pki != nil {
		return
	}
	pki := newTestPKI(t)
	st.pki = &pki
	st.cfg.CertsDir = pki.dir
}

func newServer(t testing.TB, st settings) *Server {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("testutil: cookie jar: %v", err)
	}
	setupTLS(t, &st)
	client := &http.Client{Timeout: 10 * time.Second, Jar: jar}
	var clientTLS *tls.Config
	if st.pki != nil {
		clientTLS = st.pki.client
		client.Transport = tlsTransport(clientTLS)
	}
	srv := &Server{
		TLS:          clientTLS,
		Client:       client,
		Dir:          st.dir,
		RootUser:     st.cfg.RootUser,
		RootPassword: st.cfg.RootPassword,
		Clock:        st.clock,
		logger:       slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	t.Cleanup(func() {
		if err := srv.stop(); err != nil {
			t.Errorf("testutil: stop app: %v", err)
		}
		srv.closeLB()
	})
	return srv
}

// startCluster starts the nodes of an in-process cluster behind a load
// balancer.
func startCluster(t testing.TB, st settings) *Server {
	t.Helper()
	if st.nodes < 2 || st.drives < 1 {
		t.Fatalf("testutil: a cluster needs at least 2 nodes with a drive each, got %d x %d", st.nodes, st.drives)
	}
	if st.dir == "" {
		st.dir = t.TempDir()
	}
	srv := newServer(t, st)
	for attempt := 1; ; attempt++ {
		// Every attempt uses new ports, hence new endpoints: fresh drives.
		base := filepath.Join(st.dir, "attempt"+strconv.Itoa(attempt))
		if attempt == 1 {
			base = st.dir
		}
		ports := reservePorts(t, 2*st.nodes+2)
		var lbOpts []LBOption
		scheme := "http"
		if st.pki != nil {
			lbOpts = append(lbOpts, LBTLS(st.pki.server, st.pki.client))
			scheme = "https"
		}
		srv.startLB(t, ports[2*st.nodes], ports[2*st.nodes+1], st.nodes, lbOpts...)
		var endpoints []string
		for n := range st.nodes {
			for d := 1; d <= st.drives; d++ {
				endpoints = append(endpoints, fmt.Sprintf("%s://%s%s/n%d/d%d", scheme, ports[n], filepath.ToSlash(base), n+1, d))
			}
		}
		srv.Nodes = nil
		for n := range st.nodes {
			cfg := st.cfg
			cfg.Endpoints = endpoints
			cfg.Address, cfg.ConsoleAddress = ports[n], ports[st.nodes+n]
			if cfg.ServerURL == "" {
				cfg.ServerURL = srv.S3URL
			}
			// Cluster nodes log at info level: four debug streams slow the
			// suite down without helping a diagnosis.
			logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelInfo})).With("instance", n+1)
			srv.Nodes = append(srv.Nodes, &Node{Config: cfg, logger: logger})
		}
		srv.Dir = base
		err := srv.start()
		if err == nil {
			return srv
		}
		srv.closeLB()
		if attempt == startAttempts || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("testutil: %v", err)
		}
	}
}

// Stop stops the application; the data directory is kept, Restart starts
// it again. Stopping a stopped server is a no-op.
func (s *Server) Stop(t testing.TB) {
	t.Helper()
	if err := s.stop(); err != nil {
		t.Fatalf("testutil: stop app: %v", err)
	}
}

// Restart stops the application (when running) and starts a new one on
// the same data directory, configuration and ports, as a process restart
// would (every node of a cluster). The service fields are refreshed:
// clients built from S3URL keep working.
func (s *Server) Restart(t testing.TB) {
	t.Helper()
	s.Stop(t)
	if err := s.start(); err != nil {
		t.Fatalf("testutil: restart: %v", err)
	}
}

// StopNode stops node i of the deployment (a cluster node going down).
func (s *Server) StopNode(t testing.TB, i int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.Nodes[i]
	if n.App == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := n.App.Stop(ctx); err != nil {
		t.Fatalf("testutil: stop node %d: %v", i+1, err)
	}
	n.App = nil
}

// StartNode starts node i again after StopNode, on the same drives and
// ports.
func (s *Server) StartNode(t testing.TB, i int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Nodes[i].App != nil {
		return
	}
	if err := s.startNode(context.Background(), s.Nodes[i]); err != nil {
		t.Fatalf("testutil: start node %d: %v", i+1, err)
	}
}

// start builds and starts the application of every node: concurrently,
// since the nodes of a cluster wait for each other in their bootstrap.
func (s *Server) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), clusterStartTimeout)
	defer cancel()
	errs := make([]error, len(s.Nodes))
	var wg sync.WaitGroup
	for i, n := range s.Nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.startNode(ctx, n)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		// The nodes that started are stopped; the start error is the one
		// reported.
		return errors.Join(err, s.stopNodes())
	}
	first := s.Nodes[0]
	s.App = first.App
	s.Config = first.Config
	if len(s.Nodes) == 1 {
		s.S3URL, s.ConsoleURL = first.S3URL, first.ConsoleURL
	}
	s.IAM = first.App.IAM()
	s.Policies = first.App.Policies()
	s.Buckets = first.App.Buckets()
	s.Shares = first.App.Shares()
	s.Signer = first.App.Signer()
	s.ILM = first.App.ILM()
	s.Events = first.App.Events()
	s.running = true
	return nil
}

// startNode builds and starts the application of one node.
func (s *Server) startNode(ctx context.Context, n *Node) error {
	appOpts := []app.Option{app.WithLogger(n.logger), app.WithStopTimeout(stopTimeout), app.WithoutFsync()}
	if s.Clock != nil {
		appOpts = append(appOpts, app.WithClock(s.Clock))
	}
	application, err := app.Build(ctx, n.Config, appOpts...)
	if err != nil {
		return fmt.Errorf("build app: %w", err)
	}
	if err := application.Start(ctx); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		_ = application.Stop(stopCtx)
		return fmt.Errorf("start app: %w", err)
	}
	n.App = application
	scheme := "http"
	if n.Config.TLSEnabled() {
		scheme = "https"
	}
	n.S3URL = scheme + "://" + application.S3Addr()
	n.ConsoleURL = scheme + "://" + application.ConsoleAddr()
	return nil
}

// stop stops the running application, if any.
func (s *Server) stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return nil
	}
	s.running = false
	return s.stopNodes()
}

// stopNodes stops every started node, concurrently.
func (s *Server) stopNodes() error {
	errs := make([]error, len(s.Nodes))
	var wg sync.WaitGroup
	for i, n := range s.Nodes {
		if n.App == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			errs[i] = n.App.Stop(ctx)
			n.App = nil
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// reservePorts returns n distinct free loopback addresses
// ("127.0.0.1:port"): the listeners are held together so that the kernel
// hands out different ports, then released for the server.
func reservePorts(t testing.TB, n int) []string {
	t.Helper()
	addrs := make([]string, 0, n)
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("testutil: reserve port: %v", err)
		}
		listeners = append(listeners, l)
		addrs = append(addrs, net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Addr().(*net.TCPAddr).Port)))
	}
	return addrs
}

// testWriter routes log output to the test log, ignoring writes after the
// test finished.
type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	defer func() { _ = recover() }()
	w.t.Logf("%s", p)
	return len(p), nil
}

// DriveDirs returns the directories of every drive of the deployment, of
// every node, in endpoint order (all of them are on this machine).
func (s *Server) DriveDirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	seen := map[string]bool{}
	for _, n := range s.Nodes {
		for _, raw := range n.Config.Endpoints {
			dir := raw
			if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
				dir = filepath.FromSlash(u.Path)
			}
			if !seen[dir] {
				seen[dir] = true
				out = append(out, dir)
			}
		}
	}
	return out
}
