package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// Deps are the dependencies of the server.
type Deps struct {
	// Routes are mounted on the router in order.
	Routes []httpx.Routes
	// Middlewares wrap the whole router, first one outermost.
	Middlewares []middleware.Middleware
	Logger      *slog.Logger
	// NotFound and MethodNotAllowed override chi's default handlers.
	NotFound         http.Handler
	MethodNotAllowed http.Handler
	// Hosts selects by Host what serves a request (the routing of the
	// distributions, docs/10 §10.2): the innermost middleware, after
	// Middlewares and in front of the routes. Nil serves every host with
	// the routes.
	Hosts middleware.Middleware
}

// Option configures the server.
type Option func(*service)

// WithAddr sets the listen address (default ":9000").
func WithAddr(addr string) Option {
	return func(s *service) {
		if addr != "" {
			s.addr = addr
		}
	}
}

// WithName names the server in logs ("s3", "console").
func WithName(name string) Option {
	return func(s *service) { s.name = name }
}

// WithTLS enables TLS with the given certificate and key files.
func WithTLS(certFile, keyFile string) Option {
	return func(s *service) {
		s.certFile, s.keyFile = certFile, keyFile
	}
}

// WithClientCAs asks the TLS clients for a certificate and verifies the
// ones presented with pool (tls.VerifyClientCertIfGiven): the S3 clients
// present none, the nodes of a cluster with mutual TLS present theirs,
// which the internode RPC server then requires.
func WithClientCAs(pool *x509.CertPool) Option {
	return func(s *service) { s.clientCAs = pool }
}

// WithTLSConfig enables TLS with a ready configuration (takes precedence
// over WithTLS files).
func WithTLSConfig(cfg *tls.Config) Option {
	return func(s *service) { s.tlsConfig = cfg }
}

// WithReadHeaderTimeout sets the header read timeout (default 30s).
func WithReadHeaderTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.readHeaderTimeout = d
		}
	}
}

// WithIdleTimeout sets the keep-alive idle timeout (default 2m).
func WithIdleTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.idleTimeout = d
		}
	}
}

// WithShutdownTimeout bounds Stop when its context has no deadline
// (default 30s).
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.shutdownTimeout = d
		}
	}
}

// WithoutH2C disables cleartext HTTP/2 (enabled by default without TLS).
func WithoutH2C() Option {
	return func(s *service) { s.h2c = false }
}

// HTTP/2 tuning of the listeners (see Start).
const (
	http2StreamWindow = 16 << 20
	http2ConnWindow   = 64 << 20
	http2MaxFrameSize = 1 << 20
)

type service struct {
	deps              Deps
	name              string
	addr              string
	certFile, keyFile string
	tlsConfig         *tls.Config
	clientCAs         *x509.CertPool
	// certificates selects the certificates by SNI (WithCertificates).
	certificates      CertificateSource
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	shutdownTimeout   time.Duration
	h2c               bool
	// proxyProtocol accepts the PROXY protocol from proxyTrusted.
	proxyProtocol bool
	proxyTrusted  []string

	handler http.Handler

	mu       sync.Mutex
	listener net.Listener
	srv      *http.Server
	serveErr chan error
}

var _ Service = (*service)(nil)

// New composes the router from deps and returns the server.
func New(deps Deps, opts ...Option) Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{
		deps:              deps,
		name:              "http",
		addr:              ":9000",
		readHeaderTimeout: 30 * time.Second,
		idleTimeout:       2 * time.Minute,
		shutdownTimeout:   30 * time.Second,
		h2c:               true,
	}
	for _, o := range opts {
		o(s)
	}
	s.handler = s.build()
	return s
}

func (s *service) build() http.Handler {
	r := chi.NewRouter()
	if s.deps.NotFound != nil {
		r.NotFound(s.deps.NotFound.ServeHTTP)
	}
	if s.deps.MethodNotAllowed != nil {
		r.MethodNotAllowed(s.deps.MethodNotAllowed.ServeHTTP)
	}
	for _, rt := range s.deps.Routes {
		if rt != nil {
			rt.Register(r)
		}
	}
	mws := append(append([]middleware.Middleware(nil), s.deps.Middlewares...), s.deps.Hosts)
	return middleware.Chain(r, mws...)
}

// Handler implements Service.
func (s *service) Handler() http.Handler { return s.handler }

// Addr implements Service.
func (s *service) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// Start implements lifecycle.Runner: it binds the listener synchronously
// (so that bind errors surface here) and serves in the background.
func (s *service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return fmt.Errorf("server %s: already started", s.name)
	}
	tlsCfg, err := s.buildTLS()
	if err != nil {
		return err
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("server %s: listen %s: %w", s.name, s.addr, err)
	}
	if s.proxyProtocol {
		pln, err := s.proxyListener(ln)
		if err != nil {
			_ = ln.Close()
			return err
		}
		ln = pln
	}
	handler := s.handler
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: s.readHeaderTimeout,
		IdleTimeout:       s.idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.deps.Logger.Handler(), slog.LevelWarn),
		TLSConfig:         tlsCfg,
		// The internode RPC streams erasure shards over HTTP/2: receive
		// windows of a few MiB and large frames keep a stream from
		// stalling on flow control and cut the frames per byte (the
		// defaults are 1 MiB windows and 16 KiB frames).
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream:     http2StreamWindow,
			MaxReceiveBufferPerConnection: http2ConnWindow,
			MaxReadFrameSize:              http2MaxFrameSize,
		},
	}
	if s.proxyProtocol {
		srv.ConnContext = proxyConnContext
	}
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
	} else if s.h2c {
		// Cleartext HTTP/2 with prior knowledge (the internode RPC) served
		// by net/http itself rather than by a hijacking h2c handler: the
		// server tracks these connections, so Shutdown closes them and a
		// stopped node stops answering its peers.
		var protocols http.Protocols
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		srv.Protocols = &protocols
	}
	s.listener = ln
	s.srv = srv
	s.serveErr = make(chan error, 1)
	s.deps.Logger.Info("http server listening", "server", s.name, "addr", ln.Addr().String(), "tls", tlsCfg != nil)
	go func(errCh chan<- error) {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.deps.Logger.Error("http server stopped", "server", s.name, "err", err)
			errCh <- err
		}
		close(errCh)
	}(s.serveErr)
	return nil
}

func (s *service) buildTLS() (*tls.Config, error) {
	cfg, err := s.baseTLS()
	if err == nil {
		cfg = s.withCertificates(cfg)
	}
	if cfg != nil && s.clientCAs != nil {
		cfg.ClientAuth, cfg.ClientCAs = tls.VerifyClientCertIfGiven, s.clientCAs
	}
	return cfg, err
}

func (s *service) baseTLS() (*tls.Config, error) {
	if s.tlsConfig != nil {
		cfg := s.tlsConfig.Clone()
		if cfg.MinVersion == 0 {
			cfg.MinVersion = tls.VersionTLS12
		}
		cfg.NextProtos = appendMissing(cfg.NextProtos, "h2", "http/1.1")
		return cfg, nil
	}
	if s.certFile == "" && s.keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("server %s: load TLS key pair: %w", s.name, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

func appendMissing(list []string, values ...string) []string {
	for _, v := range values {
		found := false
		for _, x := range list {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			list = append(list, v)
		}
	}
	return list
}

// Stop implements lifecycle.Runner with a graceful shutdown.
func (s *service) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	errCh := s.serveErr
	s.srv = nil
	s.listener = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.shutdownTimeout)
		defer cancel()
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		if cerr := srv.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return fmt.Errorf("server %s: shutdown: %w", s.name, err)
	}
	if serveErr := <-errCh; serveErr != nil {
		return fmt.Errorf("server %s: serve: %w", s.name, serveErr)
	}
	return nil
}
