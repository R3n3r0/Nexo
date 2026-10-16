package rpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Defaults of the internode connections.
const (
	defaultCallTimeout = 30 * time.Second
	defaultDialTimeout = 5 * time.Second
	defaultHealthMin   = 250 * time.Millisecond
	defaultHealthMax   = 3 * time.Second
	pingTimeout        = 2 * time.Second
	// http2ReadIdle and http2PingTimeout detect dead connections (a node
	// partitioned without resets) by HTTP/2 pings.
	http2ReadIdle    = 10 * time.Second
	http2PingTimeout = 5 * time.Second
	// http2MaxFrameSize is the largest DATA frame a node accepts from its
	// peers (the HTTP/2 default is 16 KiB).
	http2MaxFrameSize = 1 << 20
)

// ConnDeps are the dependencies of a connection to a node.
type ConnDeps struct {
	// Auth signs the outgoing calls (required).
	Auth   Authenticator
	Logger *slog.Logger
}

// ConnOption configures a connection.
type ConnOption func(*connOptions)

type connOptions struct {
	tls         *tls.Config
	callTimeout time.Duration
	dialTimeout time.Duration
	healthMin   time.Duration
	healthMax   time.Duration
	wrap        func(http.RoundTripper) http.RoundTripper
	dial        DialFunc
	maxReply    int64
}

// DialFunc opens the TCP connection to a node (net.Dialer.DialContext).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// WithDialer replaces the TCP dialer of the connection (default a
// net.Dialer bounded by WithDialTimeout): the tests route the internode
// traffic through proxies that partition the nodes or delay the links.
// TLS, when configured, runs over the connections it returns.
func WithDialer(d DialFunc) ConnOption {
	return func(o *connOptions) { o.dial = d }
}

// WithTLSConfig calls the node over TLS (https) with cfg; without it the
// calls use cleartext HTTP/2 (h2c).
func WithTLSConfig(cfg *tls.Config) ConnOption {
	return func(o *connOptions) { o.tls = cfg }
}

// WithCallTimeout sets the default timeout of the unary calls (default
// 30s).
func WithCallTimeout(d time.Duration) ConnOption {
	return func(o *connOptions) {
		if d > 0 {
			o.callTimeout = d
		}
	}
}

// WithDialTimeout bounds the establishment of a connection (default 5s).
func WithDialTimeout(d time.Duration) ConnOption {
	return func(o *connOptions) {
		if d > 0 {
			o.dialTimeout = d
		}
	}
}

// WithHealthInterval sets the bounds of the backoff between two probes of
// an offline node (default 250ms-3s).
func WithHealthInterval(lo, hi time.Duration) ConnOption {
	return func(o *connOptions) {
		if lo > 0 && hi >= lo {
			o.healthMin, o.healthMax = lo, hi
		}
	}
}

// WithMaxReply bounds the answer of a unary call the connection accepts
// (default 64 MiB, like the calls a node accepts): a larger one fails
// with ErrProtocol.
func WithMaxReply(n int64) ConnOption {
	return func(o *connOptions) {
		if n > 0 {
			o.maxReply = n
		}
	}
}

// WithTransportWrapper decorates the HTTP/2 round tripper of the
// connection (latency or fault injection in tests, tracing).
func WithTransportWrapper(wrap func(http.RoundTripper) http.RoundTripper) ConnOption {
	return func(o *connOptions) { o.wrap = wrap }
}

// conn implements Conn over an HTTP/2 transport, which multiplexes the
// calls on one pooled connection per node.
type conn struct {
	deps      ConnDeps
	opts      connOptions
	node      string
	base      string
	transport *http.Transport
	client    *http.Client

	online atomic.Bool
	closed atomic.Bool
	// verifying is set while a broken exchange is being verified by a
	// ping (suspect).
	verifying atomic.Bool
	// mu guards probing and lastErr, and orders the background
	// goroutines (wg.Add) before Close (closed, wg.Wait).
	mu      sync.Mutex
	probing bool
	lastErr string
	stop    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
}

var _ Conn = (*conn)(nil)

// NewConn returns the connection to the RPC server of node ("host:port").
// Nothing is dialed until the first call; the node is assumed online.
func NewConn(deps ConnDeps, node string, opts ...ConnOption) (Conn, error) {
	if deps.Auth == nil {
		return nil, fmt.Errorf("%w: rpc: a connection needs the authenticator", domain.ErrInvalidArgument)
	}
	if _, _, err := net.SplitHostPort(node); err != nil {
		return nil, fmt.Errorf("%w: rpc: node %q is not host:port: %v", domain.ErrInvalidArgument, node, err)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := connOptions{
		callTimeout: defaultCallTimeout, dialTimeout: defaultDialTimeout,
		healthMin: defaultHealthMin, healthMax: defaultHealthMax, maxReply: maxUnaryReply,
	}
	for _, opt := range opts {
		opt(&o)
	}
	c := &conn{deps: deps, opts: o, node: node, stop: make(chan struct{})}
	scheme := "http"
	if o.tls != nil {
		scheme = "https"
	}
	c.base = scheme + "://" + node + Prefix + "/"
	netDialer := &net.Dialer{Timeout: o.dialTimeout, KeepAlive: 30 * time.Second}
	dial := o.dial
	if dial == nil {
		dial = netDialer.DialContext
	}
	// HTTP/2 only, served by net/http: cleartext with prior knowledge
	// (h2c) without TLS, negotiated by ALPN with it. There is no HTTP/1
	// fallback, so every call to a node shares one multiplexed connection.
	var protocols http.Protocols
	if o.tls == nil {
		protocols.SetUnencryptedHTTP2(true)
	} else {
		protocols.SetHTTP2(true)
	}
	c.transport = &http.Transport{
		Protocols:   &protocols,
		DialContext: dial,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			raw, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(raw, h2TLSConfig(o.tls, addr))
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = raw.Close()
				return nil, err
			}
			return tc, nil
		},
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: http2ReadIdle,
			PingTimeout:     http2PingTimeout,
			// Shard streams move megabytes: large DATA frames cut the
			// number of frames, and of read syscalls, per byte.
			MaxReadFrameSize: http2MaxFrameSize,
		},
	}
	var rt http.RoundTripper = c.transport
	if o.wrap != nil {
		rt = o.wrap(rt)
	}
	c.client = &http.Client{Transport: rt}
	c.online.Store(true)
	return c, nil
}

// Node implements Conn.
func (c *conn) Node() string { return c.node }

// IsOnline implements Conn.
func (c *conn) IsOnline() bool { return c.online.Load() && !c.closed.Load() }

// Close implements Conn.
func (c *conn) Close(context.Context) error {
	c.once.Do(func() {
		// closed is set under mu, where suspect and trip check it before
		// wg.Add: no goroutine is added once Wait may have started.
		c.mu.Lock()
		c.closed.Store(true)
		c.mu.Unlock()
		close(c.stop)
		c.wg.Wait()
		c.transport.CloseIdleConnections()
	})
	return nil
}

func (c *conn) options(opts []CallOption) callOptions {
	o := callOptions{timeout: c.opts.callTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// check fails fast when the call cannot be attempted.
func (c *conn) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed.Load() {
		return fmt.Errorf("%w: connection to %s is closed", domain.ErrNodeOffline, c.node)
	}
	if !c.online.Load() {
		c.mu.Lock()
		last := c.lastErr
		c.mu.Unlock()
		return fmt.Errorf("%w: %s (circuit open: %s)", domain.ErrNodeOffline, c.node, last)
	}
	return nil
}

// newRequest builds an authenticated call: its token is bound to the
// method and target of the request and to argsHash, the SHA-256 of the
// arguments (the body of a unary call, the first frame of an upload).
func (c *conn) newRequest(ctx context.Context, path string, body io.Reader, contentType string, o callOptions, argsHash []byte) (*http.Request, error) {
	u := c.base + strings.TrimPrefix(path, "/")
	if len(o.params) > 0 {
		q := url.Values{}
		for _, p := range o.params {
			q.Add(p[0], p[1])
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, fmt.Errorf("rpc: build call %s to %s: %w", path, c.node, err)
	}
	token, err := c.deps.Auth.Token(ctx, c.node, CallBinding{Method: req.Method, Target: req.URL.RequestURI(), BodyHash: argsHash})
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	return req, nil
}

// transportError classifies the failure of an exchange: the caller gave
// up (its context error), the call timed out (the node is slow: no
// breaker), or the exchange broke. A broken exchange does not prove the
// node gone (a stream reset, a connection closed at the wrong time): the
// call fails, and the circuit opens only if a ping of the node fails too
// (verified in the background, so that the failing call is not delayed).
func (c *conn) transportError(ctx context.Context, timedOut bool, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if timedOut {
		return fmt.Errorf("%w: %s: call timed out: %w", domain.ErrNodeOffline, c.node, context.DeadlineExceeded)
	}
	if unreachable(err) {
		c.trip(err)
	} else {
		c.suspect(err)
	}
	return fmt.Errorf("%w: %s: %v", domain.ErrNodeOffline, c.node, err)
}

// unreachable reports a failure to connect to the node (refused, no
// route, unknown host): unlike a broken exchange, it needs no ping to
// tell that the node is gone.
func unreachable(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// suspect verifies, in the background, that the node still answers a
// ping after a broken exchange, and opens the circuit when it does not.
// One verification runs at a time.
func (c *conn) suspect(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || !c.online.Load() || !c.verifying.CompareAndSwap(false, true) {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.verifying.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), min(pingTimeout, c.opts.callTimeout))
		defer cancel()
		go func() {
			select {
			case <-c.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		var reply pingReply
		err := c.unary(ctx, "health/ping", struct{}{}, &reply, callOptions{timeout: min(pingTimeout, c.opts.callTimeout)})
		if err == nil || c.closed.Load() {
			if err == nil {
				c.deps.Logger.Debug("rpc: exchange failed but the node answers", "peer", c.node, "err", cause)
			}
			return
		}
		c.trip(fmt.Errorf("%v (ping: %v)", cause, err))
	}()
}

// answer turns a non-200 answer into the remote error.
func (c *conn) answer(resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var we wireError
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), contentTypeMsgpack) || msgpack.Unmarshal(body, &we) != nil || we.Code == "" {
		return fmt.Errorf("%w: %s answered %s", ErrProtocol, c.node, resp.Status)
	}
	return decodeError(c.node, we)
}

// Call implements Conn.
func (c *conn) Call(ctx context.Context, path string, in, out any, opts ...CallOption) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	return c.unary(ctx, path, in, out, c.options(opts))
}

// unary performs a unary exchange without the circuit check (the probe
// uses it while the circuit is open).
func (c *conn) unary(ctx context.Context, path string, in, out any, o callOptions) error {
	body, err := msgpack.Marshal(in)
	if err != nil {
		return fmt.Errorf("rpc: encode %T: %w", in, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	req, err := c.newRequest(callCtx, path, bytes.NewReader(body), contentTypeMsgpack, o, hashOf(body))
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return c.transportError(ctx, errors.Is(callCtx.Err(), context.DeadlineExceeded), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := c.answer(resp); err != nil {
		return err
	}
	data, err := readReply(resp.Body, c.opts.maxReply)
	if err != nil {
		if errors.Is(err, ErrProtocol) {
			return fmt.Errorf("%s from %s: %w", path, c.node, err)
		}
		return c.transportError(ctx, errors.Is(callCtx.Err(), context.DeadlineExceeded), err)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := msgpack.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: decode answer of %s from %s: %v", ErrProtocol, path, c.node, err)
	}
	return nil
}

// errUploadDone stops the writer of an upload whose answer arrived.
var errUploadDone = errors.New("rpc: upload answered")

// Upload implements Conn.
func (c *conn) Upload(ctx context.Context, path string, args any, body io.Reader, out any, opts ...CallOption) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	o := callOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timedOut := atomic.Bool{}
	if o.timeout > 0 {
		timer := time.AfterFunc(o.timeout, func() { timedOut.Store(true); cancel() })
		defer timer.Stop()
	}
	encoded, err := msgpack.Marshal(args)
	if err != nil {
		return fmt.Errorf("rpc: encode %T: %w", args, err)
	}
	pr, pw := io.Pipe()
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		readErr = writeUpload(pw, encoded, body)
	}()
	req, err := c.newRequest(callCtx, path, pr, contentTypeStream, o, hashOf(encoded))
	if err != nil {
		_ = pr.CloseWithError(err)
		<-done
		return err
	}
	resp, err := c.client.Do(req)
	// The whole answer is read before the request body is closed: the
	// node may answer before the transport has read the end of the
	// request body from the pipe, and a pipe closed under it makes the
	// transport abort the stream, answer included (a healthy node then
	// looked offline). Once the answer is in, closing the pipe only
	// stops a writer the node did not wait for (it answered early).
	var (
		data      []byte
		answerErr error
	)
	if err == nil {
		if answerErr = c.answer(resp); answerErr == nil {
			data, err = readReply(resp.Body, c.opts.maxReply)
			if errors.Is(err, ErrProtocol) {
				answerErr, err = fmt.Errorf("%s from %s: %w", path, c.node, err), nil
			}
		}
		_ = resp.Body.Close()
	}
	_ = pr.CloseWithError(errUploadDone)
	<-done
	if readErr != nil {
		return readErr
	}
	if err != nil {
		return c.transportError(ctx, timedOut.Load(), err)
	}
	if answerErr != nil {
		return answerErr
	}
	if out != nil && len(data) > 0 {
		if err := msgpack.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%w: decode answer of %s from %s: %v", ErrProtocol, path, c.node, err)
		}
	}
	return nil
}

// maxUnaryReply bounds the answer of a unary call a node accepts from a
// peer, like maxUnaryBody bounds the calls a node accepts.
const maxUnaryReply = maxUnaryBody

// readReply reads the answer of a unary call, refusing one over limit
// bytes (ErrProtocol): a peer, or a process listening on its port, cannot
// make this node buffer an unbounded answer.
func readReply(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: answer over %d bytes", ErrProtocol, limit)
	}
	return data, nil
}

// hashOf returns the SHA-256 of the arguments of a call.
func hashOf(args []byte) []byte {
	sum := sha256.Sum256(args)
	return sum[:]
}

// writeUpload writes the framed upload: the encoded arguments, the data,
// the end. It returns the read error of body, if any (sent as an error
// frame so that the node discards the partial data); write errors only
// mean that the exchange ended.
func writeUpload(pw *io.PipeWriter, args []byte, body io.Reader) error {
	fw := newFrameWriter(pw)
	if err := fw.frame(frameMsg, args); err != nil {
		_ = pw.CloseWithError(err)
		return nil
	}
	// The data frames are filled straight from body (no copy).
	buf := fw.dataFrame()
	for {
		n, rerr := fill(body, buf[frameHeaderSize:frameHeaderSize+dataChunk])
		if n > 0 {
			if err := fw.writeDataFrame(buf, n); err != nil {
				_ = pw.CloseWithError(err)
				return nil
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			_ = fw.Error(rerr)
			_ = fw.Flush()
			_ = pw.Close()
			return rerr
		}
	}
	if err := fw.End(); err == nil {
		_ = fw.Flush()
	}
	_ = pw.Close()
	return nil
}

// open performs a call answered by a frame stream. The timeout bounds the
// wait for the answer only.
func (c *conn) open(ctx context.Context, path string, in any, opts []CallOption) (*frameStream, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	o := c.options(opts)
	body, err := msgpack.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("rpc: encode %T: %w", in, err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	timedOut := atomic.Bool{}
	timer := time.AfterFunc(o.timeout, func() { timedOut.Store(true); cancel() })
	req, err := c.newRequest(streamCtx, path, bytes.NewReader(body), contentTypeMsgpack, o, hashOf(body))
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	resp, err := c.client.Do(req)
	if !timer.Stop() && err == nil && timedOut.Load() {
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, c.transportError(ctx, timedOut.Load(), err)
	}
	if err := c.answer(resp); err != nil {
		_ = resp.Body.Close()
		cancel()
		return nil, err
	}
	return &frameStream{ctx: ctx, fr: newFrameReader(resp.Body, c.node), body: resp.Body, cancel: cancel}, nil
}

// Download implements Conn.
func (c *conn) Download(ctx context.Context, path string, in any, opts ...CallOption) (io.ReadCloser, error) {
	return c.open(ctx, path, in, opts)
}

// Stream implements Conn.
func (c *conn) Stream(ctx context.Context, path string, in any, opts ...CallOption) (MsgStream, error) {
	return c.open(ctx, path, in, opts)
}

// frameStream is the reader of a streamed answer: a data reader for
// Download, a message stream for Stream.
type frameStream struct {
	ctx    context.Context
	fr     *frameReader
	body   io.Closer
	cancel context.CancelFunc
	once   sync.Once
}

var (
	_ io.ReadCloser = (*frameStream)(nil)
	_ MsgStream     = (*frameStream)(nil)
)

func (s *frameStream) Read(p []byte) (int, error) {
	n, err := s.fr.Read(p)
	return n, s.fail(err)
}

func (s *frameStream) Next(v any) error { return s.fail(s.fr.Next(v)) }

// WriteTo implements io.WriterTo (io.Copy without intermediate buffer).
func (s *frameStream) WriteTo(w io.Writer) (int64, error) {
	n, err := s.fr.WriteTo(w)
	return n, s.fail(err)
}

// fail reports the error of the caller's context rather than the broken
// stream it caused.
func (s *frameStream) fail(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if cerr := s.ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

func (s *frameStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.body.Close()
	})
	return nil
}

// trip opens the circuit and starts the background probe.
func (c *conn) trip(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastErr = err.Error()
	if c.online.CompareAndSwap(true, false) {
		c.deps.Logger.Warn("rpc: node offline, probing it in the background", "peer", c.node, "err", err)
	}
	if c.probing || c.closed.Load() {
		return
	}
	c.probing = true
	c.wg.Add(1)
	go c.probe()
}

// probe pings the node with a growing backoff until it answers.
func (c *conn) probe() {
	defer c.wg.Done()
	interval := c.opts.healthMin
	lastLogged := ""
	for {
		timer := time.NewTimer(interval)
		select {
		case <-c.stop:
			timer.Stop()
			c.mu.Lock()
			c.probing = false
			c.mu.Unlock()
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), min(pingTimeout, c.opts.callTimeout))
		var reply pingReply
		err := c.unary(ctx, "health/ping", struct{}{}, &reply, callOptions{timeout: min(pingTimeout, c.opts.callTimeout)})
		cancel()
		if err == nil {
			c.mu.Lock()
			c.probing = false
			c.lastErr = ""
			c.online.Store(true)
			c.mu.Unlock()
			c.deps.Logger.Info("rpc: node online", "peer", c.node, "peer_id", reply.Node)
			return
		}
		c.mu.Lock()
		c.lastErr = err.Error()
		c.mu.Unlock()
		if msg := err.Error(); msg != lastLogged && !errors.Is(err, domain.ErrNodeOffline) {
			// The node answers but refuses us (credentials, deployment,
			// version): worth an explicit warning.
			c.deps.Logger.Warn("rpc: node refuses the internode calls", "peer", c.node, "err", err)
			lastLogged = msg
		}
		interval = min(interval*2, c.opts.healthMax)
	}
}

// h2TLSConfig returns the client configuration of a TLS connection to addr:
// a copy of cfg that negotiates HTTP/2 by ALPN and names the server of addr
// when cfg names none, as tls.Dialer does.
func h2TLSConfig(cfg *tls.Config, addr string) *tls.Config {
	var c *tls.Config
	if cfg == nil {
		c = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		c = cfg.Clone()
	}
	if c.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		c.ServerName = host
	}
	c.NextProtos = []string{"h2"}
	return c
}
