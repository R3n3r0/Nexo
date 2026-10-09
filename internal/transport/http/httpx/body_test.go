package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// copyResult is what the handler of a CopyBody test observed.
type copyResult struct {
	n   int64
	err error
}

// zeros is an endless stream of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// copyServer serves size bytes with CopyBody and reports each copy.
func copyServer(t *testing.T, size int, timeout time.Duration) (*httptest.Server, <-chan copyResult) {
	t.Helper()
	results := make(chan copyResult, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		n, err := httpx.CopyBody(w, io.LimitReader(zeros{}, int64(size)), timeout)
		results <- copyResult{n: n, err: err}
	}))
	t.Cleanup(srv.Close)
	return srv, results
}

func TestCopyBody(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		timeout time.Duration
	}{
		{name: "guarded", size: 1 << 20, timeout: 100 * time.Millisecond},
		{name: "unguarded", size: 1 << 20, timeout: 0},
		{name: "empty", size: 0, timeout: 100 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, results := copyServer(t, tt.size, tt.timeout)
			client := srv.Client()
			for i := range 2 {
				resp, err := client.Get(srv.URL)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				assert.Len(t, body, tt.size)
				res := <-results
				require.NoError(t, res.err)
				assert.Equal(t, int64(tt.size), res.n)
				if i == 0 {
					// The deadline is cleared: the kept-alive connection
					// serves the next request after the timeout.
					time.Sleep(2 * tt.timeout)
				}
			}
		})
	}
}

func TestCopyBodyAbortsStalledClient(t *testing.T) {
	const timeout = 200 * time.Millisecond
	// Far more than the socket buffers hold.
	srv, results := copyServer(t, 256<<20, timeout)
	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// The client reads the headers, then nothing.
	select {
	case res := <-results:
		require.Error(t, res.err, "a client that stops reading has its response aborted")
		assert.Less(t, res.n, int64(256<<20))
	case <-time.After(30 * time.Second):
		t.Fatal("the copy did not give up on the stalled client")
	}
}

func TestCopyBodyWithoutDeadlines(t *testing.T) {
	// A ResponseWriter that cannot set deadlines is written to unguarded.
	rec := httptest.NewRecorder()
	n, err := httpx.CopyBody(rec, strings.NewReader("hello"), time.Second)
	require.NoError(t, err)
	assert.Equal(t, int64(5), n)
	assert.Equal(t, "hello", rec.Body.String())

	g := httpx.NewSendGuard(httptest.NewRecorder(), time.Second)
	_, err = g.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, g.Close())
}
