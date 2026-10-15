package server_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
)

// h2Peer is a raw HTTP/2 client (prior knowledge) that announces a zero
// flow control window: the server can send headers but no body.
type h2Peer struct {
	t      *testing.T
	conn   net.Conn
	fr     *http2.Framer
	mu     sync.Mutex
	hbuf   bytes.Buffer
	henc   *hpack.Encoder
	frames chan http2.Frame
	// settings are the settings of the server; connWindow the receive
	// window of its connection (65535 plus its WINDOW_UPDATE).
	settings   map[http2.SettingID]uint32
	connWindow int64
}

func dialH2(t *testing.T, addr string) *h2Peer {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	p := &h2Peer{t: t, conn: conn, fr: http2.NewFramer(conn, conn), frames: make(chan http2.Frame, 1024),
		settings: map[http2.SettingID]uint32{}, connWindow: 65535}
	p.henc = hpack.NewEncoder(&p.hbuf)
	_, err = io.WriteString(conn, http2.ClientPreface)
	require.NoError(t, err)
	require.NoError(t, p.fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0}))
	// The settings of the server and the window of its connection
	// arrive before any answer.
	deadline := time.Now().Add(5 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for gotSettings, gotWindow := false, false; !gotSettings || !gotWindow; {
		f, err := p.fr.ReadFrame()
		require.NoError(t, err)
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if f.IsAck() {
				continue
			}
			_ = f.ForeachSetting(func(s http2.Setting) error { p.settings[s.ID] = s.Val; return nil })
			p.mu.Lock()
			require.NoError(t, p.fr.WriteSettingsAck())
			p.mu.Unlock()
			gotSettings = true
		case *http2.WindowUpdateFrame:
			if f.StreamID == 0 {
				p.connWindow += int64(f.Increment)
				gotWindow = true
			}
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	go func() {
		defer close(p.frames)
		for {
			f, err := p.fr.ReadFrame()
			if err != nil {
				return
			}
			if sf, ok := f.(*http2.SettingsFrame); ok && !sf.IsAck() {
				p.mu.Lock()
				_ = p.fr.WriteSettingsAck()
				p.mu.Unlock()
				continue
			}
			select {
			case p.frames <- f:
			default:
			}
		}
	}()
	return p
}

// get opens a GET stream with a request body the handler never reads
// (bodySize bytes, within the windows of the server).
func (p *h2Peer) get(id uint32, path string, bodySize int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hbuf.Reset()
	for _, hf := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "localhost"}, {Name: ":path", Value: path},
	} {
		require.NoError(p.t, p.henc.WriteField(hf))
	}
	require.NoError(p.t, p.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: p.hbuf.Bytes(), EndHeaders: true, EndStream: bodySize == 0}))
	chunk := make([]byte, 16<<10)
	for sent := 0; sent < bodySize; sent += len(chunk) {
		n := min(len(chunk), bodySize-sent)
		require.NoError(p.t, p.fr.WriteData(id, false, chunk[:n]))
	}
}

// resets waits for the RST_STREAM of the streams ids, false at timeout.
func (p *h2Peer) resets(ids []uint32, timeout time.Duration) bool {
	want := map[uint32]bool{}
	for _, id := range ids {
		want[id] = true
	}
	limit := time.After(timeout)
	for len(want) > 0 {
		select {
		case f, ok := <-p.frames:
			if !ok {
				return false
			}
			if rst, isRst := f.(*http2.RSTStreamFrame); isRst {
				delete(want, rst.StreamID)
			}
		case <-limit:
			return false
		}
	}
	return true
}

// stalledServer starts a server whose /big answer is larger than any
// window and whose handler never reads the request body; done receives
// the outcome of every handler.
func stalledServer(t *testing.T, lift bool, opts ...server.Option) (server.Service, chan error) {
	t.Helper()
	done := make(chan error, 64)
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/big", func(w http.ResponseWriter, r *http.Request) {
			if lift {
				httpx.LiftSendBound(r.Context())
			}
			chunk := bytes.Repeat([]byte("x"), 32<<10)
			var err error
			for i := 0; i < 64 && err == nil; i++ {
				_, err = w.Write(chunk)
			}
			done <- err
		})
	})
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}}, append([]server.Option{server.WithAddr("127.0.0.1:0")}, opts...)...)
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return srv, done
}

// TestServerReleasesStalledHTTP2Streams: a client announcing a zero
// window, with streams whose handler blocks writing and request bodies the
// handler never reads, has its streams reset within the send timeout: the
// handlers return and the buffered bodies are released (regression: the
// listeners kept the handlers, and up to 64 MiB of request body per
// connection, for as long as the client stayed connected). The windows of
// the listeners are the ones of net/http unless the listener serves the
// internode RPC.
func TestServerReleasesStalledHTTP2Streams(t *testing.T) {
	tests := []struct {
		name       string
		opts       []server.Option
		window     uint32
		connWindow int64
	}{
		{name: "default windows", window: 1 << 20, connWindow: 1 << 20},
		{
			name:   "windows of the internode RPC",
			opts:   []server.Option{server.WithHTTP2Windows(server.RPCStreamWindow, server.RPCConnWindow, server.RPCMaxFrameSize)},
			window: server.RPCStreamWindow, connWindow: server.RPCConnWindow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const timeout = 300 * time.Millisecond
			before := runtime.NumGoroutine()
			srv, done := stalledServer(t, false, append([]server.Option{server.WithSendTimeout(timeout)}, tt.opts...)...)
			p := dialH2(t, srv.Addr())
			assert.Equal(t, tt.window, p.settings[http2.SettingInitialWindowSize], "receive window of a stream")
			assert.Equal(t, tt.connWindow, p.connWindow, "receive window of the connection")
			assert.Equal(t, uint32(server.DefaultMaxConcurrentStreams), p.settings[http2.SettingMaxConcurrentStreams])

			const streams = 8
			ids := make([]uint32, 0, streams)
			body := int(min(int64(tt.window), tt.connWindow/streams))
			for i := range streams {
				id := uint32(2*i + 1)
				ids = append(ids, id)
				p.get(id, "/big", body)
			}
			start := time.Now()
			require.True(t, p.resets(ids, 10*time.Second), "the stalled streams are reset")
			assert.Less(t, time.Since(start), 5*time.Second)
			for range streams {
				select {
				case err := <-done:
					assert.Error(t, err, "the blocked write fails")
				case <-time.After(5 * time.Second):
					t.Fatal("a handler stays blocked")
				}
			}
			// The connection stays open, the goroutines of the streams
			// are gone.
			assert.Eventually(t, func() bool { return runtime.NumGoroutine() <= before+8 }, 5*time.Second, 20*time.Millisecond,
				"goroutines left: %d (before %d)", runtime.NumGoroutine(), before)
		})
	}
}

// TestServerLiftedSendBound: a handler that lifts the bound (the
// authenticated internode RPC) is never cut, however long its client
// takes.
func TestServerLiftedSendBound(t *testing.T) {
	const timeout = 100 * time.Millisecond
	srv, done := stalledServer(t, true, server.WithSendTimeout(timeout))
	p := dialH2(t, srv.Addr())
	p.get(1, "/big", 0)
	assert.False(t, p.resets([]uint32{1}, 10*timeout), "the stream of a lifted bound is not reset")
	select {
	case err := <-done:
		t.Fatalf("the handler returned: %v", err)
	default:
	}
	_ = p.conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler outlives its connection")
	}
}

// TestServerSendTimeoutKeepsIdleStreams: the deadline is cleared between
// the writes, so a stream idle longer than the timeout between two events
// (server-sent events, a heartbeat) is not cut, over HTTP/2 and HTTP/1.
func TestServerSendTimeoutKeepsIdleStreams(t *testing.T) {
	const timeout = 100 * time.Millisecond
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/events", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "a")
			_ = http.NewResponseController(w).Flush()
			time.Sleep(4 * timeout)
			_, _ = io.WriteString(w, "b")
		})
	})
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}}, server.WithAddr("127.0.0.1:0"), server.WithSendTimeout(timeout))
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	for _, h2 := range []bool{false, true} {
		var protocols http.Protocols
		if h2 {
			protocols.SetUnencryptedHTTP2(true)
		} else {
			protocols.SetHTTP1(true)
		}
		client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
		resp, err := client.Get("http://" + srv.Addr() + "/events")
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err, "h2=%v", h2)
		assert.Equal(t, "ab", string(data), "h2=%v", h2)
		client.CloseIdleConnections()
	}
}

// TestServerSendTimeoutKeepAlive: over HTTP/1 the deadline a response
// leaves armed for the last bytes of the server is cleared by the next
// request of the connection, however late it comes.
func TestServerSendTimeoutKeepAlive(t *testing.T) {
	const timeout = 100 * time.Millisecond
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/chunked", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("y", 64<<10))
		})
		r.Get("/empty", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}}, server.WithAddr("127.0.0.1:0"), server.WithSendTimeout(timeout))
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
	t.Cleanup(client.CloseIdleConnections)
	reused := 0
	for i, path := range []string{"/chunked", "/chunked", "/empty", "/chunked"} {
		if i > 0 {
			time.Sleep(3 * timeout)
		}
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				reused++
			}
		}}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, "http://"+srv.Addr()+path, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err, "request %d", i)
		_, err = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err, "request %d", i)
	}
	assert.Equal(t, 3, reused, "the connection is kept alive")
}
