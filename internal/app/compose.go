package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/settings"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/transport/http/console"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
	"github.com/r3n3r0/nexo/internal/version"
)

// services are the composed services and HTTP servers of a deployment
// whose id is known (after the bootstrap).
type services struct {
	deploymentID string
	objects      object.Service
	identities   iam.Service
	policies     policy.Service
	buckets      bucket.Service
	shares       share.Service
	signer       auth.Signer
	events       event.Dispatcher
	// s3Srv and consSrv are the composed servers: started as is in the
	// single-node modes, behind the gates of the listeners in a cluster.
	s3Srv   server.Service
	consSrv server.Service
	// healing are the MRF queue, the healing service and the scanner.
	healing healingServices
	// unsubscribe unregisters the cache invalidations of the notifier.
	unsubscribe func()
}

// composeDeps are the inputs of compose.
type composeDeps struct {
	cfg          config.Static
	f            *foundation
	deploymentID string
	clock        clock.Clock
	logger       *slog.Logger
	assets       fs.FS
	running      *atomic.Bool
}

// sharedStores are the stores of the documents shared by the services:
// files of the single drive (SNSD) or replicated documents of the first
// erasure set (SNMD, distributed), decorated with the broadcast of the
// invalidations.
type sharedStores struct {
	config   configsvc.Store
	iam      iam.Store
	policies policy.PolicyStore
	// sharedPolicies is the cache of the named policies to invalidate
	// (nil in SNSD, where the file store is the only writer).
	sharedPolicies policy.SharedStore
	buckets        bucket.MetadataStore
}

// openStores opens the stores of the system documents of the deployment
// mode. SNSD keeps the documents of earlier releases on its drive
// (.nexo.sys/config); the erasure-coded modes keep them in the replicated
// system document store, shared by every node.
func openStores(ctx context.Context, d composeDeps, coh cluster.Coherence) (sharedStores, error) {
	var out sharedStores
	if d.f.layout.Mode == domain.ModeSNSD {
		sysConfigDir := filepath.Join(d.f.local[0].Endpoint().Path, domain.SystemBucket, storage.ConfigDir)
		var err error
		if out.config, err = configsvc.NewFileStore(filepath.Join(sysConfigDir, configFile)); err != nil {
			return out, fmt.Errorf("config store: %w", err)
		}
		if out.iam, err = iam.NewFileStore(ctx, filepath.Join(sysConfigDir, iamDir), d.cfg.RootPassword); err != nil {
			return out, fmt.Errorf("iam store: %w", err)
		}
		if out.policies, err = policy.NewFileStore(filepath.Join(sysConfigDir, policiesDir)); err != nil {
			return out, fmt.Errorf("policy store: %w", err)
		}
		if out.buckets, err = bucket.NewStore(bucket.StoreDeps{Drives: d.f.local, Logger: d.logger}); err != nil {
			return out, err
		}
		return out, nil
	}
	sets, err := d.f.poolSets()
	if err != nil {
		return out, err
	}
	docs, err := sysstore.New(sysstore.Deps{Drives: sets[0][0], Locks: d.f.locks, Clock: d.clock, Logger: d.logger})
	if err != nil {
		return out, err
	}
	cfgStore, err := configsvc.NewDocsStore(docs)
	if err != nil {
		return out, err
	}
	out.config = coh.ConfigStore(cfgStore)
	iamStore, err := iam.NewDocsStore(ctx, docs, d.cfg.RootPassword)
	if err != nil {
		return out, fmt.Errorf("iam store: %w", err)
	}
	out.iam = coh.IAMStore(iamStore)
	policyStore, err := policy.NewDocsStore(docs)
	if err != nil {
		return out, err
	}
	out.sharedPolicies = policyStore
	out.policies = coh.PolicyStore(policyStore)
	bucketStore, err := bucket.NewStore(bucket.StoreDeps{Docs: docs, Logger: d.logger})
	if err != nil {
		return out, err
	}
	out.buckets = coh.BucketStore(bucketStore)
	return out, nil
}

// newObjectLayer builds the object layer of the deployment mode: the
// filesystem layer on the single drive (SNSD), the erasure-coded server
// pools otherwise, which report the objects to heal to mrf.
func newObjectLayer(d composeDeps, buckets object.BucketConfigSource, events event.Service, mrf object.HealNotifier) (object.Service, error) {
	f := d.f
	opts := []object.Option{object.WithRegion(d.cfg.Region)}
	if f.layout.Mode == domain.ModeSNSD {
		return object.NewFS(object.Deps{
			Store: f.local[0], Buckets: buckets, Locks: f.locks, Events: events, DeploymentID: d.deploymentID,
			Clock: d.clock, Logger: d.logger,
		}, opts...)
	}
	if v := d.cfg.StorageClassStandard; v != "" {
		opts = append(opts, object.WithStorageClass(domain.StorageClassStandard, v))
	}
	if v := d.cfg.StorageClassRRS; v != "" {
		opts = append(opts, object.WithStorageClass(domain.StorageClassReducedRedundancy, v))
	}
	pools, err := poolSpecs(f)
	if err != nil {
		return nil, err
	}
	// The codecs key the bitrot hashes of the shards with the deployment.
	codecs := erasure.NewFactory(erasure.Deps{Logger: d.logger}, erasure.WithDeploymentID(d.deploymentID))
	return object.NewServerPools(object.PoolsDeps{
		Pools: pools, DeploymentID: d.deploymentID, Locks: f.locks, Buckets: buckets, Events: events, Heal: mrf,
		Codecs: codecs, Clock: d.clock, Logger: d.logger,
	}, opts...)
}

// compose builds the services and the HTTP servers of a deployment whose
// drives are formatted (deploymentID): the object layer of the mode, the
// identities, policies, buckets, share links and console sessions on the
// shared stores, the Admin API and the S3 API. It registers the cache
// invalidations on the notifier.
func compose(ctx context.Context, d composeDeps) (*services, error) {
	cfg, f, logger := d.cfg, d.f, d.logger
	coh, err := cluster.NewCoherence(cluster.CoherenceDeps{Notifier: f.notifier, Logger: logger})
	if err != nil {
		return nil, err
	}
	stores, err := openStores(ctx, d, coh)
	if err != nil {
		return nil, err
	}

	// Cross-cutting services.
	registry := d.f.registry
	auditor := audit.NewSlog(audit.Deps{Logger: logger.With("component", "audit"), Clock: d.clock})
	dynConfig := configsvc.New(configsvc.Deps{Store: stores.config, Logger: logger})
	events := event.NewDispatcher(event.Deps{Logger: logger.With("component", "events")})

	// Object layer and the bucket metadata it reads the bucket
	// configuration from; the MRF queue receives the objects it finds
	// damaged, the healing service and the scanner work on it.
	mrf := f.mrf
	var notifier object.HealNotifier
	if mrf != nil {
		notifier = mrf
	}
	objects, err := newObjectLayer(d, stores.buckets, events, notifier)
	if err != nil {
		return nil, fmt.Errorf("object layer: %w", err)
	}
	healing, err := newHealing(d, objects, mrf, dynConfig)
	if err != nil {
		return nil, fmt.Errorf("healing: %w", err)
	}
	if healing.heal != nil {
		// Before serving: a pool added by an expansion gets the buckets
		// of the deployment (failures are logged by the service and
		// healed later by the jobs and the scanner).
		_, _ = healing.heal.HealPools(ctx)
	}

	// Identities, policies and buckets. IAM is the source of truth of the
	// policy attachments; the policy engine resolves bucket policies from
	// the bucket metadata store, which breaks the bucket → policy → bucket
	// construction cycle.
	identities := iam.New(iam.Deps{Store: stores.iam, Clock: d.clock, Logger: logger.With("component", "iam")})
	if err := identities.Bootstrap(ctx, domain.Credentials{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword}); err != nil {
		return nil, fmt.Errorf("bootstrap root credentials: %w", err)
	}
	policies := policy.New(policy.Deps{
		Store:       stores.policies,
		Attachments: identities,
		Buckets:     bucketPolicies{store: stores.buckets},
		Clock:       d.clock,
		Logger:      logger,
	})
	buckets, err := bucket.New(bucket.Deps{Objects: objects, Store: stores.buckets, Policy: policies, Locks: f.locks, Clock: d.clock, Logger: logger},
		bucket.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}

	// Authentication, share links and console sessions.
	authDeps := auth.Deps{Creds: identities, Clock: d.clock, Logger: logger}
	verifier, err := auth.NewVerifier(authDeps, auth.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}
	signer := auth.NewSigner(authDeps, auth.WithRegion(cfg.Region))
	shares := share.New(share.Deps{IAM: identities, Signer: signer, Store: stores.iam, Policy: policies, Clock: d.clock, Logger: logger},
		share.WithServerURL(publicS3URL(cfg)), share.WithRegion(cfg.Region))
	// The signing key derives from the root credentials and the deployment
	// ID: every node of the deployment accepts the same cookies, no other
	// deployment does.
	sessions, err := session.New(session.Deps{IAM: identities, Store: stores.iam, Clock: d.clock, Logger: logger},
		session.WithSigningKey(session.DeriveKey(cfg.RootUser, cfg.RootPassword, d.deploymentID)))
	if err != nil {
		return nil, err
	}

	// Administration and health.
	build := version.Get()
	adminSvc := adminsvc.New(adminsvc.Deps{
		Clock:        d.clock,
		Logger:       logger,
		Build:        adminsvc.BuildInfo{Version: build.Version, Commit: build.Commit, Date: build.Date},
		Mode:         f.layout.Mode,
		Region:       cfg.Region,
		DeploymentID: d.deploymentID,
		Drives:       f.local,
		Pools:        f.layout.PoolInfos(),
		Objects:      objects,
		Heal:         healing.heal,
		Scanner:      healing.scanner,
		Actions:      processActions{running: d.running, signal: signalSelf},
		// Every node of the deployment (this one only on a single node),
		// with its background healing and clock offset.
		Cluster: f.info,
	})
	healthSvc := health.New(health.Deps{Readiness: map[string]health.Check{
		"storage": object.ReadinessCheck(objects),
	}})

	codec := httpx.New()
	ids := httpx.NewIDGenerator()

	// The S3 API evaluates with the engine decorated by the applied
	// dynamic settings (api.root_access, hot reloaded).
	applied, err := settings.New(settings.Deps{Config: dynConfig, Logger: logger})
	if err != nil {
		return nil, err
	}
	s3Policy := settings.GuardRootAccess(policies, applied)

	// S3 server (:9000): every route goes through the SigV4 middleware,
	// which admits anonymous requests; the handlers authorize with the
	// policy engine.
	s3Middlewares, err := s3Middlewares(middlewareDeps{
		logger: logger, clock: d.clock, codec: codec, ids: ids,
		registry: registry, verifier: verifier, auditor: auditor, domains: domainsOf(cfg),
		trustedProxies: cfg.TrustedProxies,
	})
	if err != nil {
		return nil, err
	}
	metricsRoutes, err := s3.NewMetricsRoutes(
		s3.MetricsDeps{Registry: registry, Verifier: verifier, Policy: s3Policy, Enc: codec, Clock: d.clock},
		s3.WithPublicMetrics(cfg.PrometheusAuthType == config.PrometheusAuthPublic))
	if err != nil {
		return nil, err
	}
	s3API := s3.NewAPI(s3.APIDeps{Enc: codec, Domains: domainsOf(cfg), System: []httpx.Routes{
		s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
		metricsRoutes,
	}},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: buckets, Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger},
			s3.WithRegion(cfg.Region)),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Verifier: verifier, Objects: objects, Buckets: buckets, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger},
			s3.WithRegion(cfg.Region)),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Verifier: verifier, Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger}),
		s3.NewSTSRoutes(s3.STSDeps{IAM: identities, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger}),
	)
	s3Srv := server.New(server.Deps{
		Routes:      []httpx.Routes{s3API},
		Middlewares: s3Middlewares,
		Logger:      logger,
	}, serverOptions(cfg, cfg.Address, "s3")...)

	// Console + Admin API server (:9001).
	consoleMiddlewares, err := consoleMiddlewares(middlewareDeps{
		logger: logger, clock: d.clock, codec: codec, ids: ids, registry: registry, trustedProxies: cfg.TrustedProxies,
	})
	if err != nil {
		return nil, err
	}
	adminAPI, err := buildAdminAPI(adminDeps{
		codec: codec, clock: d.clock, logger: logger, region: cfg.Region, s3URL: publicS3URL(cfg),
		sessions: sessions, verifier: verifier, signer: signer, admin: adminSvc, iam: identities, policy: policies,
		buckets: buckets, objects: objects, shares: shares, config: dynConfig, locks: f.lockManager, secureCookies: secureConsole(cfg),
		consoleURL: cfg.BrowserRedirectURL,
	})
	if err != nil {
		return nil, err
	}
	consSrv := server.New(server.Deps{
		Routes: []httpx.Routes{
			s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
			adminAPI,
			console.NewSPARoutes(console.SPADeps{Assets: console.NewAssets(console.AssetsDeps{FS: d.assets}), Enc: codec}),
		},
		Middlewares: consoleMiddlewares,
		Logger:      logger,
	}, serverOptions(cfg, cfg.ConsoleAddress, "console")...)

	caches := cluster.Caches{IAM: identities, Config: dynConfig, Buckets: stores.buckets}
	if stores.sharedPolicies != nil {
		caches.Policies = stores.sharedPolicies
	}
	unsubscribe := coh.Subscribe(caches)
	return &services{
		deploymentID: d.deploymentID,
		objects:      objects,
		identities:   identities,
		policies:     policies,
		buckets:      buckets,
		shares:       shares,
		signer:       signer,
		events:       events,
		s3Srv:        s3Srv,
		consSrv:      consSrv,
		healing:      healing,
		unsubscribe:  unsubscribe,
	}, nil
}

// runners returns the background components of the services, started
// after the drives and stopped before them: the event dispatcher, then
// the healing services (stopped first).
func (s *services) runners() lifecycle.Runner {
	healing := s.healing.runner()
	return lifecycle.Funcs{
		OnStart: func(ctx context.Context) error {
			if err := s.events.Start(ctx); err != nil {
				return err
			}
			if err := healing.Start(ctx); err != nil {
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			s.unsubscribe()
			herr := healing.Stop(ctx)
			if err := s.events.Stop(ctx); err != nil {
				return err
			}
			return herr
		},
	}
}
