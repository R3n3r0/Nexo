package distribution_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// TestResolve covers the host resolution order of docs/10 §10.2 (TDS-01).
func TestResolve(t *testing.T) {
	ctx := context.Background()
	svc := newService(t,
		distribution.WithDomains("s3.example.org"),
		distribution.WithMainHosts("console.example.org", "api.example.org"))
	site := sample("sito-web", "www.example.com", "example.com", "*.preview.example.com")
	_, err := svc.Create(ctx, site)
	require.NoError(t, err)
	deep := sample("deep", "*.eu.preview.example.com")
	deep.Origin.Bucket = "deep"
	_, err = svc.Create(ctx, deep)
	require.NoError(t, err)
	off := sample("off", "off.example.com")
	off.Enabled = false
	_, err = svc.Create(ctx, off)
	require.NoError(t, err)
	s3d := sample("s3-cliente", "s3.cliente.it")
	s3d.Mode = distribution.ModeS3
	s3d.Origin.Bucket = "cliente"
	_, err = svc.Create(ctx, s3d)
	require.NoError(t, err)

	tests := []struct {
		host   string
		kind   distribution.RouteKind
		id     string
		alias  string
		bucket string
	}{
		{host: "www.example.com", kind: distribution.RouteDistribution, id: "sito-web", alias: "www.example.com", bucket: "web"},
		{host: "WWW.Example.COM:8443", kind: distribution.RouteDistribution, id: "sito-web", alias: "www.example.com", bucket: "web"},
		{host: "example.com.", kind: distribution.RouteDistribution, id: "sito-web", alias: "example.com", bucket: "web"},
		{host: "pr-42.preview.example.com", kind: distribution.RouteDistribution, id: "sito-web", alias: "*.preview.example.com", bucket: "web"},
		{host: "a.b.preview.example.com", kind: distribution.RouteDistribution, id: "sito-web", alias: "*.preview.example.com", bucket: "web"},
		// The longest wildcard suffix wins.
		{host: "x.eu.preview.example.com", kind: distribution.RouteDistribution, id: "deep", alias: "*.eu.preview.example.com", bucket: "deep"},
		// A wildcard needs a label of its own.
		{host: "preview.example.com", kind: distribution.RoutePathStyle},
		{host: "eu.preview.example.com", kind: distribution.RouteDistribution, id: "sito-web", alias: "*.preview.example.com", bucket: "web"},
		// Disabled distributions keep their hosts (answered 403).
		{host: "off.example.com", kind: distribution.RouteDistribution, id: "off", alias: "off.example.com", bucket: "web"},
		{host: "s3.cliente.it", kind: distribution.RouteDistribution, id: "s3-cliente", alias: "s3.cliente.it", bucket: "cliente"},
		// Virtual host, main domains, IP literals, unknown hosts.
		{host: "photos.s3.example.org", kind: distribution.RouteVirtualHost, bucket: "photos"},
		{host: "s3.example.org:9000", kind: distribution.RouteMain},
		{host: "console.example.org", kind: distribution.RouteMain},
		{host: "127.0.0.1:9000", kind: distribution.RouteMain},
		{host: "[::1]:9000", kind: distribution.RouteMain},
		{host: "", kind: distribution.RouteMain},
		{host: "unknown.example.net", kind: distribution.RoutePathStyle},
		{host: "example.com.evil.net", kind: distribution.RoutePathStyle},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			r := svc.Resolve(ctx, tc.host)
			assert.Equal(t, tc.kind, r.Kind)
			assert.Equal(t, tc.alias, r.Alias)
			assert.Equal(t, tc.bucket, r.Bucket)
			if tc.id == "" {
				assert.Nil(t, r.Distribution)
				return
			}
			require.NotNil(t, r.Distribution)
			assert.Equal(t, tc.id, r.Distribution.ID)
		})
	}

	misdirected := newService(t, distribution.WithUnknownHostMisdirected(true), distribution.WithMainHosts("s3.example.org"))
	assert.Equal(t, distribution.RouteMisdirected, misdirected.Resolve(ctx, "unknown.example.net").Kind)
	assert.Equal(t, distribution.RouteMain, misdirected.Resolve(ctx, "s3.example.org").Kind)
	assert.Equal(t, distribution.RouteMain, misdirected.Resolve(ctx, "10.0.0.1").Kind)
}

// TestAliasConflicts covers TDS-02: an alias belongs to one distribution.
func TestAliasConflicts(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, distribution.WithMainHosts("s3.example.org"))
	_, err := svc.Create(ctx, sample("a", "www.example.com", "*.preview.example.com"))
	require.NoError(t, err)

	tests := []struct {
		name    string
		d       distribution.Distribution
		wantErr error
	}{
		{name: "same exact alias", d: sample("b", "WWW.example.com"), wantErr: distribution.ErrAliasConflict},
		{name: "same wildcard", d: sample("b", "*.Preview.example.com"), wantErr: distribution.ErrAliasConflict},
		{name: "main host", d: sample("b", "s3.example.org"), wantErr: distribution.ErrAliasConflict},
		{name: "same id", d: sample("a", "other.example.com"), wantErr: distribution.ErrAlreadyExists},
		{name: "overlapping wildcard and exact is fine", d: sample("b", "x.preview.example.com")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, tc.d)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.ErrorIs(t, err, domain.ErrConflict, "409")
		})
	}
	// The more specific exact alias wins over the wildcard of a.
	r := svc.Resolve(ctx, "x.preview.example.com")
	require.NotNil(t, r.Distribution)
	assert.Equal(t, "b", r.Distribution.ID)

	// Updating a distribution keeps its own aliases.
	a, err := svc.Get(ctx, "a")
	require.NoError(t, err)
	a.Comment = "updated"
	_, err = svc.Update(ctx, a, a.ETag)
	require.NoError(t, err)
	_, err = svc.Update(ctx, a, a.ETag)
	require.ErrorIs(t, err, domain.ErrPreconditionFailed, "stale ETag")
}

// BenchmarkResolve measures the routing table with 9900 exact and 1100
// wildcard aliases.
func BenchmarkResolve(b *testing.B) {
	ctx := context.Background()
	svc := newService(b, distribution.WithDomains("s3.example.org"))
	for i := range 110 {
		aliases := make([]string, 0, 100)
		for j := range 90 {
			aliases = append(aliases, fmt.Sprintf("h%d-%d.example.com", i, j))
		}
		for j := range 10 {
			aliases = append(aliases, fmt.Sprintf("*.w%d-%d.example.com", i, j))
		}
		d := sample(fmt.Sprintf("d%d", i), aliases...)
		if _, err := svc.Create(ctx, d); err != nil {
			b.Fatal(err)
		}
	}
	hosts := []string{"h50-50.example.com:443", "a.b.w109-9.example.com", "bucket.s3.example.org", "unknown.example.net"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = svc.Resolve(ctx, hosts[i%len(hosts)])
	}
}

// FuzzResolve checks that no Host value panics nor resolves to a
// distribution it does not belong to (TDS-14).
func FuzzResolve(f *testing.F) {
	for _, s := range []string{"www.example.com", "[::1]:80", "a.preview.example.com", "..", "*.preview.example.com", "%00", ""} {
		f.Add(s)
	}
	svc := newService(f)
	if _, err := svc.Create(context.Background(), sample("site", "www.example.com", "*.preview.example.com")); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, host string) {
		r := svc.Resolve(context.Background(), host)
		if r.Kind != distribution.RouteDistribution {
			return
		}
		h := r.Host
		if h != "www.example.com" && !strings.HasSuffix(h, ".preview.example.com") {
			t.Fatalf("host %q resolved to %s through %s", host, r.Distribution.ID, r.Alias)
		}
	})
}
