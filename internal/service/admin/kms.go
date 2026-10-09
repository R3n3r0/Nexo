package admin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
)

// maxInfoKeys bounds the key names reported by ServerInfo.
const maxInfoKeys = 100

// KMSManagerDeps are the dependencies of the KMS manager. KMS is required
// (kms.NewDisabled without KMS).
type KMSManagerDeps struct {
	KMS    kms.Service
	Logger *slog.Logger
}

// KMSManagerOption configures the KMS manager.
type KMSManagerOption func(*kmsManager)

type kmsManager struct {
	deps KMSManagerDeps
}

var _ KMSManager = (*kmsManager)(nil)

// NewKMSManager returns the key management of the Admin API.
func NewKMSManager(deps KMSManagerDeps, opts ...KMSManagerOption) (KMSManager, error) {
	if deps.KMS == nil {
		return nil, fmt.Errorf("%w: kms manager: the KMS is required", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &kmsManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Status implements KMSManager.
func (m *kmsManager) Status(ctx context.Context) (kms.Status, error) {
	st, err := m.deps.KMS.Status(ctx)
	if err != nil {
		return kms.Status{}, fmt.Errorf("admin: kms status: %w", err)
	}
	return st, nil
}

// ListKeys implements KMSManager.
func (m *kmsManager) ListKeys(ctx context.Context, prefix string) ([]kms.KeyInfo, error) {
	keys, err := m.deps.KMS.ListKeys(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("admin: kms keys: %w", err)
	}
	return keys, nil
}

// CreateKey implements KMSManager.
func (m *kmsManager) CreateKey(ctx context.Context, name string) (kms.KeyInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return kms.KeyInfo{}, fmt.Errorf("%w: the key name is required", domain.ErrInvalidArgument)
	}
	if err := m.deps.KMS.CreateKey(ctx, name); err != nil {
		return kms.KeyInfo{}, fmt.Errorf("admin: create kms key %q: %w", name, err)
	}
	keys, err := m.deps.KMS.ListKeys(ctx, name)
	if err == nil {
		for _, k := range keys {
			if k.Name == name {
				return k, nil
			}
		}
	}
	return kms.KeyInfo{Name: name}, nil
}

// kmsInfo describes the KMS in ServerInfo: its state and the first keys.
// A KMS that cannot be listed is reported without keys.
func kmsInfo(ctx context.Context, k kms.Service, logger *slog.Logger) *domain.KMSInfo {
	st, err := k.Status(ctx)
	if err != nil {
		logger.WarnContext(ctx, "admin: kms status for server info", "err", err)
		return &domain.KMSInfo{Error: err.Error()}
	}
	out := &domain.KMSInfo{
		Configured: st.Configured, Backend: st.Backend, DefaultKey: st.DefaultKey,
		Endpoints: st.Endpoints, Online: st.Online, Error: st.Error,
	}
	if !st.Configured {
		return out
	}
	keys, err := k.ListKeys(ctx, "")
	if err != nil {
		logger.WarnContext(ctx, "admin: kms keys for server info", "err", err)
		return out
	}
	out.Keys = make([]string, 0, min(len(keys), maxInfoKeys))
	for _, key := range keys[:min(len(keys), maxInfoKeys)] {
		out.Keys = append(out.Keys, key.Name)
	}
	return out
}
