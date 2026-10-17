package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// throttledConn reads its connection at rate bytes per second.
type throttledConn struct {
	net.Conn
	rate  float64
	start time.Time
	read  int
}

func (c *throttledConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p[:min(len(p), 1<<10)])
	c.read += n
	time.Sleep(time.Until(c.start.Add(time.Duration(float64(c.read) / c.rate * float64(time.Second)))))
	return n, err
}

// sndbufListener keeps the send buffer of its connections small, so that
// what the server writes is soon what the client reads.
type sndbufListener struct{ net.Listener }

func (l sndbufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(32 << 10)
	}
	return c, err
}

// zeroReader reads zeros.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestServerKeepsSlowSharedHTTP2Streams: the streams of an HTTP/2
// connection share its bandwidth in turns, so many streams over a slow
// connection that the client reads steadily each progress slower than a
// write of their handler (32 KiB with io.Copy) per send timeout; they are
// not cut, the bound measuring the progress of a stream piece by piece
// (regression: every stream of a browser loading a gallery over a slow
// link was reset). The net/http server is the one of the listener, served
// with a small send buffer.
func TestServerKeepsSlowSharedHTTP2Streams(t *testing.T) {
	const (
		timeout = time.Second
		streams = 16
		size    = 64 << 10
		// 16 KiB/s per stream: a write of 32 KiB takes twice the timeout.
		rate = streams * 16 << 10
	)
	failed := make(chan error, streams+1)
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/file", func(w http.ResponseWriter, _ *http.Request) {
			// No WriteTo: io.Copy writes 32 KiB at a time.
			if _, err := io.Copy(w, struct{ io.Reader }{io.LimitReader(zeroReader{}, size)}); err != nil {
				failed <- err
			}
		})
	})
	srv, ok := New(Deps{Routes: []httpx.Routes{routes}}, WithSendTimeout(timeout)).(*service)
	require.True(t, ok)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	hs := srv.httpServer(nil)
	go func() { _ = hs.Serve(sndbufListener{ln}) }()
	t.Cleanup(func() { _ = hs.Close() })

	var dials atomic.Int32
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := &http.Client{Transport: &http.Transport{Protocols: &protocols,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{Control: realMSS}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			dials.Add(1)
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(16 << 10)
			}
			return &throttledConn{Conn: c, rate: rate, start: time.Now()}, nil
		}}}
	t.Cleanup(client.CloseIdleConnections)
	get := func() error {
		resp, err := client.Get("http://" + ln.Addr().String() + "/file")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		n, err := io.Copy(io.Discard, resp.Body)
		if err == nil && n != size {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	// One stream first, so that the others share its connection.
	require.NoError(t, get())

	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for range streams {
		wg.Go(func() {
			if err := get(); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a stream read steadily was cut: %v", err)
	}
	close(failed)
	for err := range failed {
		t.Errorf("a handler failed: %v", err)
	}
	assert.Equal(t, int32(1), dials.Load(), "the streams share one connection")
}

// TestStallWriterPiece: the pieces of the writes of an HTTP/2 stream are
// its share of h2MinRate in half the timeout, at least h2MinPiece; a
// connection serving one request keeps the whole writes of io.Copy.
func TestStallWriterPiece(t *testing.T) {
	requests := &connRequests{}
	g := &stallWriter{timeout: 20 * time.Second, http2: true, requests: requests}
	for _, tt := range []struct {
		requests int64
		want     int
	}{
		{requests: 0, want: 320 << 10},
		{requests: 1, want: 320 << 10},
		{requests: 10, want: 32 << 10},
		{requests: 40, want: 8 << 10},
		{requests: 128, want: h2MinPiece},
	} {
		requests.n.Store(tt.requests)
		assert.Equal(t, tt.want, g.piece(), "%d requests", tt.requests)
	}
	assert.Equal(t, h2MinPiece, (&stallWriter{timeout: time.Hour, http2: true}).piece(), "requests unknown")
}
