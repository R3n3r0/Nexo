package httpx_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"Example.COM":         "example.com",
		"example.com:9000":    "example.com",
		".s3.example.com.":    "s3.example.com",
		"[::1]:9000":          "::1",
		"  spaced.example  ":  "spaced.example",
		"":                    "",
		"::1":                 "::1",
		"bucket.example:abcd": "bucket.example",
	}
	for in, want := range tests {
		assert.Equal(t, want, httpx.NormalizeHost(in), in)
	}
}

// TestVirtualHostBucket: the bucket of a virtual host, whatever the case,
// the port and the trailing dot of a fully qualified host.
func TestVirtualHostBucket(t *testing.T) {
	domains := []string{"s3.example.com", "S3.Other.Test."}
	tests := []struct {
		host   string
		bucket string
	}{
		{host: "photos.s3.example.com", bucket: "photos"},
		{host: "Photos.S3.Example.COM:9000", bucket: "photos"},
		{host: "photos.s3.example.com.", bucket: "photos"},
		{host: "photos.s3.example.com.:9000", bucket: "photos"},
		{host: "a.b.s3.other.test", bucket: "a.b"},
		{host: "s3.example.com"},
		{host: ".s3.example.com"},
		{host: "photos.example.com"},
		{host: "photos.s3.example.com.evil"},
	}
	for _, tt := range tests {
		bucket, ok := httpx.VirtualHostBucket(domains, tt.host)
		assert.Equal(t, tt.bucket != "", ok, tt.host)
		assert.Equal(t, tt.bucket, bucket, tt.host)
	}
	_, ok := httpx.VirtualHostBucket(nil, "photos.s3.example.com")
	assert.False(t, ok, "no domain")
}
