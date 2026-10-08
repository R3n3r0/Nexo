package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
)

func TestPublicS3URL(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Static
		want string
	}{
		{name: "explicit server url", cfg: config.Static{ServerURL: "https://s3.example.com", Address: ":9000"}, want: "https://s3.example.com"},
		{name: "any interface", cfg: config.Static{Address: ":9000"}, want: "http://localhost:9000"},
		{name: "unspecified ip", cfg: config.Static{Address: "0.0.0.0:9000"}, want: "http://localhost:9000"},
		{name: "named host", cfg: config.Static{Address: "10.0.0.5:9100"}, want: "http://10.0.0.5:9100"},
		{name: "ipv6 host", cfg: config.Static{Address: "[::1]:9000"}, want: "http://[::1]:9000"},
		{name: "tls", cfg: config.Static{Address: ":9443", CertsDir: "/certs"}, want: "https://localhost:9443"},
		{name: "invalid address", cfg: config.Static{Address: "nonsense"}, want: "http://localhost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, publicS3URL(tc.cfg))
		})
	}
}

func TestDomainsOf(t *testing.T) {
	assert.Nil(t, domainsOf(config.Static{}))
	assert.Equal(t, []string{"s3.example.com"}, domainsOf(config.Static{Domain: "s3.example.com"}))
}

func TestBucketPoliciesAdapter(t *testing.T) {
	ctx := context.Background()
	raw := json.RawMessage(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	tests := []struct {
		name    string
		meta    *bucket.Metadata
		loadErr error
		wantErr error
		check   func(t *testing.T, p []byte)
	}{
		{
			name: "stored policy", meta: &bucket.Metadata{Name: "b", Policy: raw},
			check: func(t *testing.T, p []byte) {
				assert.JSONEq(t, string(raw), string(p), "the stored document, parsed by the policy engine")
			},
		},
		{name: "no policy", meta: &bucket.Metadata{Name: "b"}, wantErr: domain.ErrPolicyNotFound},
		{name: "store failure", loadErr: errors.New("drive offline"), wantErr: errors.New("drive offline")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := bucketmock.NewMetadataStore(t)
			store.EXPECT().Load(ctx, "b").Return(tc.meta, tc.loadErr).Once()
			p, err := bucketPolicies{store: store}.GetBucketPolicy(ctx, "b")
			if tc.wantErr != nil {
				require.Error(t, err)
				if errors.Is(tc.wantErr, domain.ErrPolicyNotFound) {
					assert.ErrorIs(t, err, tc.wantErr)
				}
				assert.Nil(t, p)
				return
			}
			require.NoError(t, err)
			tc.check(t, p)
		})
	}
}
