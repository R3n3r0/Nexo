package auth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

func BenchmarkVerifyHeader(b *testing.B) {
	for _, cache := range []int{0, 4096} {
		b.Run(map[bool]string{false: "nocache", true: "cache"}[cache > 0], func(b *testing.B) {
			v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(b), Clock: clock.NewFake(awsTime)}, auth.WithSigningKeyCacheSize(cache))
			require.NoError(b, err)
			d := awsDocRequests()[0]
			ctx := context.Background()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := v.Verify(ctx, d.build()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkVerifyPresigned(b *testing.B) {
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(b), Clock: clock.NewFake(awsTime.Add(time.Hour))})
	require.NoError(b, err)
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := v.Verify(ctx, httptest.NewRequest(http.MethodGet, awsPresignedURL, nil)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkChunkedDecode(b *testing.B) {
	clk := clock.NewFake(testNow)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	v, err := auth.NewVerifier(auth.Deps{Creds: newLookup(b), Clock: clk})
	require.NoError(b, err)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	req := httptest.NewRequest(http.MethodPut, "http://localhost:9000/b/k", nil)
	req.Header.Set("X-Amz-Decoded-Content-Length", "1048576")
	require.NoError(b, s.Sign(ctx, req, domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"))
	seed := req.Header.Get("Authorization")[len(req.Header.Get("Authorization"))-64:]
	enc := newChunkEncoder(awsSecretKey, testNow, seed, true)
	for off := 0; off < len(payload); off += 64 << 10 {
		enc.chunk(payload[off : off+64<<10])
	}
	body := enc.final(false).bytes()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := req.Clone(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if _, err := v.Verify(ctx, r); err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			b.Fatal(err)
		}
	}
}
