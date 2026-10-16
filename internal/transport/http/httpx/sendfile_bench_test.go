//go:build linux

package httpx

import (
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkCopyBodyFile serves a 16 MiB file section over HTTP/1.1 on
// loopback with CopyBody, as a file section (sendfile) and hidden behind a
// plain reader (read and write through user space, the copy before
// T-9.2), to a client that discards it. cpu-ns/op is the CPU time of the
// whole process (server and client) per download. Run it several times
// rather than with -count, so that both sources share the load of the
// machine:
//
//	go test -c -o httpx.test ./internal/transport/http/httpx/
//	for i in $(seq 10); do ./httpx.test -test.run '^$' -test.bench CopyBodyFile; done > out.txt
//	benchstat -col /src out.txt
func BenchmarkCopyBodyFile(b *testing.B) {
	const size = 16 << 20
	name := filepath.Join(b.TempDir(), "f")
	data := make([]byte, size)
	_, _ = rand.Read(data)
	require.NoError(b, os.WriteFile(name, data, 0o600))
	for _, mode := range []string{"file", "reader"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f, err := os.Open(name) // #nosec G304 -- a benchmark file
			if err != nil {
				b.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			w.Header().Set("Content-Length", strconv.Itoa(size))
			var src io.Reader = &io.LimitedReader{R: f, N: size}
			if mode == "reader" {
				src = struct{ io.Reader }{src}
			}
			if _, err := CopyBody(w, src, DefaultSendTimeout); err != nil {
				b.Error(err)
			}
		}))
		client := srv.Client()
		b.Run("src="+mode, func(b *testing.B) {
			b.SetBytes(size)
			var ru0, ru1 syscall.Rusage
			_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
			defer func() {
				_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
				cpu := ru1.Utime.Nano() - ru0.Utime.Nano() + ru1.Stime.Nano() - ru0.Stime.Nano()
				b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			}()
			b.RunParallel(func(pb *testing.PB) {
				buf := make([]byte, 256<<10)
				for pb.Next() {
					resp, err := client.Get(srv.URL)
					if err != nil {
						b.Error(err)
						return
					}
					_, err = io.CopyBuffer(io.Discard, struct{ io.Reader }{resp.Body}, buf)
					_ = resp.Body.Close()
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
		srv.Close()
	}
}
