package admin_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/kms"
	kmsmock "github.com/r3n3r0/nexo/internal/service/kms/mock"
)

func TestNewKMSManagerRequiresKMS(t *testing.T) {
	_, err := admin.NewKMSManager(admin.KMSManagerDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestKMSManagerCreateKey(t *testing.T) {
	created := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		key     string
		setup   func(k *kmsmock.Service)
		want    kms.KeyInfo
		wantErr error
	}{
		{
			name: "created and described", key: " app ",
			setup: func(k *kmsmock.Service) {
				k.EXPECT().CreateKey(mock.Anything, "app").Return(nil)
				k.EXPECT().ListKeys(mock.Anything, "app").Return([]kms.KeyInfo{{Name: "app-2"}, {Name: "app", CreatedAt: created}}, nil)
			},
			want: kms.KeyInfo{Name: "app", CreatedAt: created},
		},
		{name: "empty name", key: "  ", setup: func(*kmsmock.Service) {}, wantErr: domain.ErrInvalidArgument},
		{
			name: "existing key", key: "app",
			setup:   func(k *kmsmock.Service) { k.EXPECT().CreateKey(mock.Anything, "app").Return(domain.ErrKMSKeyExists) },
			wantErr: domain.ErrConflict,
		},
		{
			name: "no KMS", key: "app",
			setup: func(k *kmsmock.Service) {
				k.EXPECT().CreateKey(mock.Anything, "app").Return(domain.ErrKMSNotConfigured)
			},
			wantErr: domain.ErrNotImplemented,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := kmsmock.NewService(t)
			tc.setup(k)
			m, err := admin.NewKMSManager(admin.KMSManagerDeps{KMS: k})
			require.NoError(t, err)
			got, err := m.CreateKey(context.Background(), tc.key)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestKMSManagerStatusAndKeys(t *testing.T) {
	k := kmsmock.NewService(t)
	st := kms.Status{Configured: true, Backend: kms.BackendBuiltin, DefaultKey: "k1", Online: true}
	k.EXPECT().Status(mock.Anything).Return(st, nil)
	k.EXPECT().ListKeys(mock.Anything, "k").Return([]kms.KeyInfo{{Name: "k1", Default: true}}, nil)
	m, err := admin.NewKMSManager(admin.KMSManagerDeps{KMS: k})
	require.NoError(t, err)
	got, err := m.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, st, got)
	keys, err := m.ListKeys(context.Background(), "k")
	require.NoError(t, err)
	assert.Equal(t, []kms.KeyInfo{{Name: "k1", Default: true}}, keys)
}
