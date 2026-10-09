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

// kmsOptions returns the options of the KMS of the configuration: the
// master keys of the builtin KMS (NEXO_KMS_SECRET_KEY and
// NEXO_KMS_SECRET_KEY_PREVIOUS), or the KES servers and their default key
// (NEXO_KMS_ENDPOINT, NEXO_KMS_KEY_NAME); none without KMS.
func kmsOptions(cfg config.Static) ([]kms.Option, error) {
	switch {
	case cfg.KMSSecretKey != "":
		master, err := kms.ParseMasterKey(cfg.KMSSecretKey)
		if err != nil {
			return nil, fmt.Errorf("kms: NEXO_KMS_SECRET_KEY: %w", err)
		}
		opts := []kms.Option{kms.WithMasterKey(master)}
		for _, v := range cfg.KMSPreviousSecretKeys {
			prev, err := kms.ParseMasterKey(v)
			if err != nil {
				return nil, fmt.Errorf("kms: NEXO_KMS_SECRET_KEY_PREVIOUS: %w", err)
			}
			opts = append(opts, kms.WithPreviousKeys(prev))
		}
		return opts, nil
	case len(cfg.KMSEndpoints) > 0:
		return []kms.Option{kms.WithEndpoints(cfg.KMSEndpoints...), kms.WithDefaultKey(cfg.KMSKeyName)}, nil
	}
	return nil, nil
}

// kmsFingerprint identifies the KMS configuration of this node for its
// peers (cluster.WithBootstrapKMSFingerprint): the nodes of a distributed
// deployment must share it.
func kmsFingerprint(cfg config.Static) (string, error) {
	opts, err := kmsOptions(cfg)
	if err != nil {
		return "", err
	}
	return kms.Fingerprint(opts...), nil
}

// newKMS builds the KMS of the configuration (docs/07 §7.2): the builtin
// KMS keyed by NEXO_KMS_SECRET_KEY (and the previous master keys), the
// client of the KES servers of NEXO_KMS_ENDPOINT, or the disabled KMS that
// refuses SSE-S3 and SSE-KMS.
func newKMS(d kmsDeps) (kms.Service, error) {
	logger := d.logger.With("component", "kms")
	opts, err := kmsOptions(d.cfg)
	if err != nil {
		return nil, err
	}
	switch {
	case d.cfg.KMSSecretKey != "":
		var keysOpts []kms.Option
		if d.docs == nil {
			keysOpts = append(keysOpts, kms.WithKeyStorePath(filepath.Join(d.localRoot, domain.SystemBucket, storage.ConfigDir, filepath.FromSlash(kms.DocsKeysDir))))
		}
		keys, err := kms.NewKeyStore(kms.KeyStoreDeps{Docs: d.docs}, keysOpts...)
		if err != nil {
			return nil, err
		}
		master, err := kms.ParseMasterKey(d.cfg.KMSSecretKey)
		if err != nil {
			return nil, fmt.Errorf("kms: NEXO_KMS_SECRET_KEY: %w", err)
		}
		logger.Info("kms: builtin KMS", "defaultKey", master.Name, "previousKeys", len(d.cfg.KMSPreviousSecretKeys))
		return kms.NewBuiltin(kms.BuiltinDeps{Keys: keys, Clock: d.clock, Logger: logger}, opts...)
	case len(d.cfg.KMSEndpoints) > 0:
		client, err := kms.NewKESClient(d.cfg.KMSCertFile, d.cfg.KMSKeyFile, d.cfg.KMSCAPath)
		if err != nil {
			return nil, err
		}
		logger.Info("kms: KES", "endpoints", d.cfg.KMSEndpoints, "defaultKey", d.cfg.KMSKeyName)
		if d.cfg.KMSCertFile == "" {
			logger.Warn("kms: no client certificate for the KES servers (NEXO_KMS_CLIENT_CERT, NEXO_KMS_CLIENT_KEY): " +
				"KES authenticates its clients with mutual TLS, the requests will be refused unless the server accepts anonymous clients")
		}
		return kms.NewKES(kms.KESDeps{HTTP: client, Clock: d.clock, Logger: logger}, opts...)
	default:
		return kms.NewDisabled(), nil
	}
}
