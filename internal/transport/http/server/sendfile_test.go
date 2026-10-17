package server_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
)

// fileServer starts a server whose /file answer is the file section
// [3, size-3) of a file of size random bytes, streamed with httpx.CopyBody
// (zero-copy over HTTP/1, see httpx.FileSection) under timeout; done
// receives the outcome of every handler.
func fileServer(t *testing.T, size int, timeout time.Duration) (server.Service, []byte, chan error) {
	t.Helper()
	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)
	name := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(name, data, 0o600))
	done := make(chan error, 16)
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/file", func(w http.ResponseWriter, _ *http.Request) {
			f, err := os.Open(name) // #nosec G304 -- a test file
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = f.Close() }()
			if _, err := f.Seek(3, io.SeekStart); err != nil {
				done <- err
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(size-6))
			w.WriteHeader(http.StatusOK)
			_, err = httpx.CopyBody(w, &io.LimitedReader{R: f, N: int64(size - 6)}, timeout)
			done <- err
		})
	})
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}}, server.WithAddr("127.0.0.1:0"), server.WithSendTimeout(timeout))
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return srv, data[3 : size-3], done
}

// TestServerFileSection: a file section reaches HTTP/1 and HTTP/2 clients
// whole (sendfile over HTTP/1, Write over HTTP/2).
func TestServerFileSection(t *testing.T) {
	srv, want, done := fileServer(t, 3<<20+77, 5*time.Second)
	for _, h2 := range []bool{false, true} {
		var protocols http.Protocols
		if h2 {
			protocols.SetUnencryptedHTTP2(true)
		} else {
			protocols.SetHTTP1(true)
		}
		client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
		resp, err := client.Get("http://" + srv.Addr() + "/file")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		client.CloseIdleConnections()
		assert.Equal(t, h2, resp.ProtoMajor == 2)
		assert.Equal(t, want, body, "h2=%v", h2)
		require.NoError(t, <-done)
	}
}

// slowClient sends GET /file over HTTP/1 with a small receive buffer and
// reads the answer piece bytes at a time, pausing between pieces (forever
// when pause is negative: it stops reading after the headers). It returns
// the bytes of the body it read.
func slowClient(t *testing.T, addr string, piece int, pause time.Duration) (int64, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(16<<10))
	_, err = io.WriteString(conn, "GET /file HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 4<<10), nil)
	require.NoError(t, err)
	if pause < 0 {
		return 0, nil
	}
	var total int64
	buf := make([]byte, piece)
	for {
		n, err := io.ReadFull(resp.Body, buf)
		total += int64(n)
		if err != nil {
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return total, nil
			}
			return total, err
		}
		time.Sleep(pause)
	}
}

// TestServerFileSectionStalledClient: a client that stops reading a file
// section has its response aborted within the send timeout, as with
// writes.
func TestServerFileSectionStalledClient(t *testing.T) {
	const timeout = 300 * time.Millisecond
	srv, _, done := fileServer(t, 64<<20, timeout)
	_, err := slowClient(t, srv.Addr(), 0, -1)
	require.NoError(t, err)
	select {
	case err := <-done:
		require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	case <-time.After(20 * time.Second):
		t.Fatal("the response of a stalled client is not aborted")
	}
}

// TestServerFileSectionSteadyClient: a client that keeps reading, with a
// small receive buffer and a body larger than the send buffer of the
// server socket (up to 4 MiB with the default tcp_wmem), so that its
// reads pace the pieces for longer than the send timeout, gets the whole
// answer. (The resumption of a piece whose deadline expires after some
// progress is covered by httpx.TestCopyFileSectionDeadlines: over
// loopback the kernel wakes a blocked writer only once half of its send
// buffer is free, in bursts the pace of a test client cannot control. The
// client reads fast enough for such a burst, 2 MiB, to take a fraction of
// the timeout even on a slow runner with the race detector.)
func TestServerFileSectionSteadyClient(t *testing.T) {
	if testing.Short() {
		t.Skip("slow client")
	}
	const timeout = time.Second
	size := 24 << 20
	srv, _, done := fileServer(t, size, timeout)
	// 64 KiB every 5 ms: at most ~12 MiB/s, so at least 1.9 s in all.
	start := time.Now()
	n, err := slowClient(t, srv.Addr(), 64<<10, 5*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, int64(size-6), n)
	require.NoError(t, <-done)
	assert.Greater(t, time.Since(start), timeout, "the client must outlast the send timeout")
}
