package app

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// kmsDeps are the inputs of newKMS.
type kmsDeps struct {
	cfg config.Static
	// localRoot is the path of the single drive (SNSD), docs the system
	// documents of the erasure-coded modes: the registry of the keys
	// created on the builtin KMS lives there, shared by every node.
	localRoot string
	docs      sysstore.Store
	clock     clock.Clock
	logger    *slog.Logger
}

// newKMS builds the KMS of the configuration (docs/07 §7.2): the builtin
// KMS keyed by NEXO_KMS_SECRET_KEY (and the previous master keys), the
// client of the KES servers of NEXO_KMS_ENDPOINT, or the disabled KMS that
// refuses SSE-S3 and SSE-KMS.
func newKMS(d kmsDeps) (kms.Service, error) {
	logger := d.logger.With("component", "kms")
	switch {
	case d.cfg.KMSSecretKey != "":
		master, err := kms.ParseMasterKey(d.cfg.KMSSecretKey)
		if err != nil {
			return nil, fmt.Errorf("kms: NEXO_KMS_SECRET_KEY: %w", err)
		}
		opts := []kms.Option{kms.WithMasterKey(master)}
		for _, v := range d.cfg.KMSPreviousSecretKeys {
			prev, err := kms.ParseMasterKey(v)
			if err != nil {
				return nil, fmt.Errorf("kms: NEXO_KMS_SECRET_KEY_PREVIOUS: %w", err)
			}
			opts = append(opts, kms.WithPreviousKeys(prev))
		}
		var keys kms.KeyStore
		if d.docs != nil {
			keys, err = kms.NewDocsKeyStore(d.docs)
		} else {
			keys, err = kms.NewFileKeyStore(filepath.Join(d.localRoot, domain.SystemBucket, storage.ConfigDir, filepath.FromSlash(kms.DocsKeysDir)))
		}
		if err != nil {
			return nil, err
		}
		logger.Info("kms: builtin KMS", "defaultKey", master.Name, "previousKeys", len(d.cfg.KMSPreviousSecretKeys))
		return kms.NewBuiltin(kms.BuiltinDeps{Keys: keys, Clock: d.clock, Logger: logger}, opts...)
	case len(d.cfg.KMSEndpoints) > 0:
		client, err := kms.NewKESClient(d.cfg.KMSCertFile, d.cfg.KMSKeyFile, d.cfg.KMSCAPath)
		if err != nil {
			return nil, err
		}
		logger.Info("kms: KES", "endpoints", d.cfg.KMSEndpoints, "defaultKey", d.cfg.KMSKeyName)
		return kms.NewKES(kms.KESDeps{HTTP: client, Clock: d.clock, Logger: logger},
			kms.WithEndpoints(d.cfg.KMSEndpoints...), kms.WithDefaultKey(d.cfg.KMSKeyName))
	default:
		return kms.NewDisabled(), nil
	}
}
