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

// WithSendTimeout bounds how long a write of a response may stall
// (default httpx.DefaultSendTimeout; <= 0 disables the bound): every
// response of the server, see stallGuard, and every HTTP/2 connection,
// closed when it accepts no byte for that long.
func WithSendTimeout(d time.Duration) Option {
	return func(s *service) { s.sendTimeout = d }
}

// WithHTTP2Windows sets the HTTP/2 receive windows (per stream and per
// connection) and the largest frame the listener accepts. The defaults
// are the ones of net/http (1 MiB windows, 16 KiB frames): the request
// bodies the server may buffer for a connection are bounded by the
// connection window, so only the listener that serves the internode RPC
// of a cluster takes the larger RPCStreamWindow, RPCConnWindow and
// RPCMaxFrameSize.
func WithHTTP2Windows(stream, conn, maxFrame int) Option {
	return func(s *service) {
		s.http2.MaxReceiveBufferPerStream = stream
		s.http2.MaxReceiveBufferPerConnection = conn
		s.http2.MaxReadFrameSize = maxFrame
	}
}

// WithMaxConcurrentStreams bounds the concurrent HTTP/2 streams (requests)
// of a connection (default DefaultMaxConcurrentStreams).
func WithMaxConcurrentStreams(n int) Option {
	return func(s *service) {
		if n > 0 {
			s.http2.MaxConcurrentStreams = n
		}
	}
}

// HTTP/2 tuning of the listeners.
const (
	// DefaultMaxConcurrentStreams bounds the concurrent requests of an
	// HTTP/2 connection (net/http allows 250): a browser opens at most
	// about a hundred, the RPC client opens another connection to a peer
	// beyond it.
	DefaultMaxConcurrentStreams = 128
	// RPCStreamWindow, RPCConnWindow and RPCMaxFrameSize tune the
	// listener of the internode RPC (WithHTTP2Windows): the shard
	// streams move megabytes, and receive windows of a few MiB and
	// large frames keep a stream from stalling on flow control and cut
	// the frames per byte. The connection window bounds what one
	// connection may have the server buffer.
	RPCStreamWindow = 8 << 20
	RPCConnWindow   = 16 << 20
	RPCMaxFrameSize = 1 << 20
)

type service struct {
	deps              Deps
	name              string
	addr              string
	certFile, keyFile string
	tlsConfig         *tls.Config
	clientCAs         *x509.CertPool
	// certificates selects the certificates by SNI (WithCertificates).
	certificates CertificateSource
	// keystore serves the certificates of the server itself, reloaded
	// at runtime (WithKeystore); clientCASource the CAs verifying the
	// client certificates (WithClientCASource).
	keystore       CertificateSource
	clientCASource ClientCASource
	// minTLSVersion is the oldest TLS version accepted (default 1.2).
	minTLSVersion     uint16
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	shutdownTimeout   time.Duration
	h2c               bool
	// proxyProtocol accepts the PROXY protocol from proxyTrusted.
	proxyProtocol bool
	proxyTrusted  []string
	// sendTimeout bounds the stalls of the responses (stallGuard);
	// http2 is the HTTP/2 configuration of the listener.
	sendTimeout time.Duration
	http2       http.HTTP2Config

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
		sendTimeout:       httpx.DefaultSendTimeout,
		http2:             http.HTTP2Config{MaxConcurrentStreams: DefaultMaxConcurrentStreams},
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
	h := middleware.Chain(r, mws...)
	if s.sendTimeout > 0 {
		// Outermost: every write of every response is bounded.
		h = stallGuard(h, s.sendTimeout)
	}
	return h
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
	srv := s.httpServer(tlsCfg)
	if tlsCfg != nil {
		ln = tls.NewListener(ln, tlsCfg)
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

// httpServer returns the net/http server of the listener, with TLS when
// tlsCfg is not nil.
func (s *service) httpServer(tlsCfg *tls.Config) *http.Server {
	h2cfg := s.http2
	if s.sendTimeout > 0 && h2cfg.WriteByteTimeout == 0 {
		// An HTTP/2 connection that accepts no byte for the send timeout
		// is closed with all its streams: a client that announces large
		// windows and stops draining its socket blocks the writer of the
		// connection, behind which the reset of a stalled stream
		// (stallGuard) waits forever.
		h2cfg.WriteByteTimeout = s.sendTimeout
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.readHeaderTimeout,
		IdleTimeout:       s.idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.deps.Logger.Handler(), slog.LevelWarn),
		TLSConfig:         tlsCfg,
		// The windows of net/http unless the listener serves the
		// internode RPC (WithHTTP2Windows).
		HTTP2: &h2cfg,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			// The requests in progress on the connection (stallGuard).
			ctx = withConnRequests(ctx)
			if s.proxyProtocol {
				ctx = proxyConnContext(ctx, c)
			}
			return ctx
		},
	}
	if tlsCfg == nil && s.h2c {
		// Cleartext HTTP/2 with prior knowledge (the internode RPC) served
		// by net/http itself rather than by a hijacking h2c handler: the
		// server tracks these connections, so Shutdown closes them and a
		// stopped node stops answering its peers.
		var protocols http.Protocols
		protocols.SetHTTP1(true)
		protocols.SetUnencryptedHTTP2(true)
		srv.Protocols = &protocols
	}
	return srv
}

// buildTLS returns the TLS configuration of the listener, nil without
// TLS: the base configuration (WithTLSConfig, WithKeystore or WithTLS),
// the distribution certificates by SNI in front of it, the TLS policy
// (harden) and the verification of the client certificates.
func (s *service) buildTLS() (*tls.Config, error) {
	cfg, err := s.baseTLS()
	if err != nil {
		return nil, err
	}
	cfg = s.withCertificates(cfg)
	if cfg == nil {
		return nil, nil
	}
	s.harden(cfg)
	s.withClientCAs(cfg)
	return cfg, nil
}

func (s *service) baseTLS() (*tls.Config, error) {
	cfg, err := s.baseTLSConfig()
	if cfg != nil {
		// HTTP/2 then HTTP/1.1, before the protocol of the ACME
		// validation (withCertificates) that only a validator offers.
		cfg.NextProtos = appendMissing(cfg.NextProtos, "h2", "http/1.1")
	}
	return cfg, err
}

func (s *service) baseTLSConfig() (*tls.Config, error) {
	if s.tlsConfig != nil {
		cfg := s.tlsConfig.Clone()
		if s.keystore != nil && cfg.GetCertificate == nil && len(cfg.Certificates) == 0 {
			cfg.GetCertificate = s.keystore.GetCertificate
		}
		return cfg, nil
	}
	if s.keystore != nil {
		// Every handshake asks the keystore: a reload of the
		// certificates applies to the next one, the established
		// connections keep theirs.
		return &tls.Config{GetCertificate: s.keystore.GetCertificate}, nil
	}
	if s.certFile == "" && s.keyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("server %s: load TLS key pair: %w", s.name, err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
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
