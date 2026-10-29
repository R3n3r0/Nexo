package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/idp"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/quota"
	"github.com/r3n3r0/nexo/internal/service/ratelimit"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/service/s3select"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/service/settings"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/service/siterepl"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/service/tier"
	"github.com/r3n3r0/nexo/internal/service/update"
	"github.com/r3n3r0/nexo/internal/transport/http/console"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
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
	lifecycle    ilm.Service
	// s3Srv and consSrv are the composed servers: started as is in the
	// single-node modes, behind the gates of the listeners in a cluster.
	s3Srv   server.Service
	consSrv server.Service
	// healing are the MRF queue, the healing service and the scanner.
	healing healingServices
	// unsubscribe unregisters the cache invalidations of the notifier.
	unsubscribe func()
	// accessRules manages the access rules of the buckets (docs/10 §10.1).
	accessRules accessrule.Service
	// dist are the distributions, their certificates and web listeners
	// (docs/10 §10.2).
	dist *distStack
	// cache is the cache of the distributions (docs/11).
	cache cache.Service
	// replication replicates the buckets to other deployments (S-BKT-12).
	replication replication.Service
	// siteRepl keeps the deployment in sync with other sites (S-STO-11).
	siteRepl siterepl.Service
	// tiers keeps the data transitioned to remote tiers (S-STO-12).
	tiers tier.Service
	// updates orchestrates the updates and the coordinated restarts
	// (S-OPS-09).
	updates update.Service
	// obs runs the audit targets and the logger configuration
	// (observability.go).
	obs lifecycle.Runner
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
	// restart asks Run for a restart (POST /service/restart).
	restart func() error
	// exe is the binary of the process; exec asks Run to replace the
	// process with it (updates, S-OPS-09).
	exe  string
	exec func() error
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
	// docs are the replicated system documents (nil in SNSD).
	docs sysstore.Store
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
	out.docs = docs
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
// pools otherwise, which report the objects to heal to mrf. sse seals the
// keys of the encrypted objects.
func newObjectLayer(d composeDeps, buckets object.BucketConfigSource, events event.Service, mrf object.HealNotifier, sse crypto.Service, applied settings.Service, tiers object.TierStore) (object.Service, error) {
	f := d.f
	opts := []object.Option{object.WithRegion(d.cfg.Region), object.WithCompression(compressionPolicy(applied, d.logger))}
	if f.layout.Mode == domain.ModeSNSD {
		return object.NewFS(object.Deps{
			Store: f.local[0], Buckets: buckets, Locks: f.objectLocks(), Events: events, Crypto: sse, Tiers: tiers, DeploymentID: d.deploymentID,
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
		Pools: pools, DeploymentID: d.deploymentID, Locks: f.objectLocks(), Buckets: buckets, Events: events, Heal: mrf,
		Codecs: codecs, Crypto: sse, Tiers: tiers, Clock: d.clock, Logger: d.logger,
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
	dynConfig := configsvc.New(configsvc.Deps{Store: stores.config, Logger: logger})
	// The applied dynamic settings, read at every use (hot reloaded).
	applied, err := settings.New(settings.Deps{Config: dynConfig, Logger: logger})
	if err != nil {
		return nil, err
	}
	// The external identity providers (identity_openid, identity_ldap).
	providers, err := idp.New(idp.Deps{Config: dynConfig, Clock: d.clock, Logger: logger})
	if err != nil {
		return nil, err
	}
	// The audit log (docs/05 T-8.4): the structured log, the memory the
	// console searches and the audit targets (observability.go).
	auditor, err := newAuditor(d, dynConfig)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	// Bucket notifications: the targets of the notify_* subsystems, routed
	// by the notification configurations of the buckets, queued on a
	// local drive of this node; the deliveries are measured.
	eventFactory, err := metrics.WrapTargetFactory(ctx, event.NewTargetFactory(event.FactoryDeps{Logger: logger.With("component", "events")}), registry)
	if err != nil {
		return nil, err
	}
	events, err := metrics.WrapEvents(ctx, event.NewDispatcher(event.Deps{
		Factory: eventFactory, Rules: notificationRules{store: stores.buckets}, Config: dynConfig, Logger: logger.With("component", "events"),
	}, event.WithRegion(cfg.Region), event.WithQueueRoot(eventQueueRoot(f))), registry)
	if err != nil {
		return nil, err
	}

	// Object layer and the bucket metadata it reads the bucket
	// configuration from; the MRF queue receives the objects it finds
	// damaged, the healing service and the scanner work on it.
	mrf := f.mrf
	var notifier object.HealNotifier
	if mrf != nil {
		notifier = mrf
	}
	// Server-side encryption: the KMS of the configuration seals the data
	// keys of SSE-S3 and SSE-KMS (SSE-C needs none).
	localRoot := ""
	if len(f.local) > 0 && f.local[0] != nil {
		localRoot = f.local[0].Endpoint().Path
	}
	keys, err := newKMS(kmsDeps{cfg: cfg, localRoot: localRoot, docs: stores.docs, clock: d.clock, logger: logger})
	if err != nil {
		return nil, err
	}
	// The requests to the KMS are measured; the scrapes read its state
	// from the bare one.
	kmsState := keys
	if keys, err = metrics.WrapKMS(ctx, keys, registry); err != nil {
		return nil, err
	}
	sse := crypto.New(crypto.Deps{KMS: keys, Logger: logger.With("component", "crypto")})
	// The writes reach the cache of the distributions through the events
	// of the object layer, before they are acknowledged (docs/11 §11.5):
	// the hook is installed once the cache is composed.
	hook := &cacheHook{}
	cacheEvents, err := cache.NewEvents(cache.EventsDeps{Events: events, Invalidator: hook, Logger: logger.With("component", "cache")})
	if err != nil {
		return nil, err
	}
	// The writes of the videos the media rules of their bucket select
	// become jobs of the media worker (the media_worker targets).
	mediaJobs, err := media.NewJobs(media.JobsDeps{Events: cacheEvents, Buckets: stores.buckets, Clock: d.clock, Logger: logger.With("component", "media")})
	if err != nil {
		return nil, err
	}
	// The writes the replication rules of their bucket select become
	// replication tasks (bound once the replication service is built).
	replEvents := &replicationEvents{Service: mediaJobs}
	var objectEvents event.Service = replEvents
	// The remote tiers of the lifecycle transitions (bound once the tier
	// service is built).
	tiers := &tierHolder{}
	objects, err := newObjectLayer(d, stores.buckets, objectEvents, notifier, sse, applied, tiers)
	if err != nil {
		return nil, fmt.Errorf("object layer: %w", err)
	}
	// Bucket lifecycle (ILM): applied by the scanner of the leader, read
	// by the S3 API for x-amz-expiration.
	lifecycle, err := ilm.New(ilm.Deps{Objects: objects, Buckets: stores.buckets, Tiers: tiers, Clock: d.clock, Logger: logger.With("component", "ilm")})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: %w", err)
	}
	// The actions of the lifecycle rules applied by the scanner are
	// measured.
	measuredILM, err := metrics.WrapLifecycle(ctx, lifecycle, registry)
	if err != nil {
		return nil, err
	}
	// The S3 requests in progress: the scanner pauses while the API is
	// busy, so that a burst of writes does not compete with its walk.
	activity := middleware.NewActivity(middleware.ActivityDeps{})
	healing, err := newHealing(d, objects, mrf, dynConfig, measuredILM, activity, stores.docs)
	if err != nil {
		return nil, fmt.Errorf("healing: %w", err)
	}
	if healing.heal != nil {
		// Before serving: a pool added by an expansion gets the buckets
		// of the deployment (failures are logged by the service and
		// healed later by the jobs and the scanner).
		_, _ = healing.heal.HealPools(ctx)
	}
	// The hard quotas of the buckets decorate the object layer the APIs
	// write through (the healing, the scanner and the lifecycle keep the
	// bare layer): the usage of the scanner plus the recent writes.
	var usage quota.UsageSource
	if healing.scanner != nil {
		usage = healing.scanner
	}
	var quotaOpts []quota.Option
	if f.layout.Mode == domain.ModeDistributed {
		// The other nodes write too: every write to a bucket with a quota
		// counts them with a fresh walk once the base is stale.
		quotaOpts = append(quotaOpts, quota.WithRefreshRatio(0))
	}
	if objects, err = quota.New(quota.Deps{Objects: objects, Buckets: stores.buckets, Usage: usage, Clock: d.clock, Logger: logger.With("component", "quota")}, quotaOpts...); err != nil {
		return nil, fmt.Errorf("quota: %w", err)
	}

	// Identities, policies and buckets. IAM is the source of truth of the
	// policy attachments; the policy engine resolves bucket policies from
	// the bucket metadata store, which breaks the bucket → policy → bucket
	// construction cycle.
	identities := iam.New(iam.Deps{Store: stores.iam, Clock: d.clock, Logger: logger.With("component", "iam")})
	if err := identities.Bootstrap(ctx, domain.Credentials{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword}); err != nil {
		return nil, fmt.Errorf("bootstrap root credentials: %w", err)
	}
	// The access rules of the buckets are a resource-based source of the
	// policy engine, next to the bucket policies (docs/10 §10.1).
	access, err := newAccessEvaluator(cfg, stores.buckets, dynConfig, d.clock, logger)
	if err != nil {
		return nil, fmt.Errorf("access rules: %w", err)
	}
	policies := policy.New(policy.Deps{
		Store:       stores.policies,
		Attachments: identities,
		Buckets:     bucketPolicies{store: stores.buckets},
		AccessRules: access.eval,
		Clock:       d.clock,
		Logger:      logger,
	})
	buckets, err := bucket.New(bucket.Deps{Objects: objects, Store: stores.buckets, Policy: policies, Locks: f.locks, KMS: keys, Clock: d.clock, Logger: logger},
		bucket.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}
	accessRules, err := access.service(buckets, stores.buckets, policies, dynConfig, auditor, d.clock, logger)
	if err != nil {
		return nil, fmt.Errorf("access rules: %w", err)
	}
	// Distributions (docs/10 §10.2): the routing by Host of the listeners,
	// the certificates of the domains (distribution.go).
	distStack, err := newDistCore(distCoreDeps{cfg: cfg, f: f, docs: stores.docs, deploymentID: d.deploymentID, matcher: access.matcher,
		registry: registry, clock: d.clock, logger: logger})
	if err != nil {
		return nil, fmt.Errorf("distributions: %w", err)
	}
	// The cache of the distributions (docs/11, cache.go) serves the object
	// reads of their requests: the S3 object routes and the website engine
	// read through it.
	cacheSvc, err := newCache(cacheDeps{cfg: cfg, f: f, docs: stores.docs, objects: objects, dist: distStack, matcher: access.matcher,
		registry: registry, clock: d.clock, logger: logger})
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	hook.set(cacheSvc)
	cachedObjects, err := cache.NewObjects(cache.ObjectsDeps{Objects: objects, Cache: cacheSvc})
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
	sessions, err := session.New(session.Deps{IAM: identities, Store: stores.iam, IDP: providers, Clock: d.clock, Logger: logger},
		session.WithSigningKey(session.DeriveKey(cfg.RootUser, cfg.RootPassword, d.deploymentID)))
	if err != nil {
		return nil, err
	}
	// The HLS presentations of the videos, for the console and the S3
	// listener (share links). The protected videos are encrypted with keys
	// derived, like the session key, from the root credentials and the
	// deployment ID: every node serves the same keys, nothing is stored.
	mediaSvc, err := media.New(media.Deps{Objects: objects, Protections: buckets, Logger: logger.With("component", "media")},
		media.WithProtectionKey(media.DeriveProtectionKey(cfg.RootUser, cfg.RootPassword, d.deploymentID)))
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
		Actions:      d.process(),
		// Every node of the deployment (this one only on a single node),
		// with its background healing and clock offset.
		Cluster: f.info,
		// The state of the KMS and its keys.
		KMS: keys,
		// The TLS of the listeners with their certificates, and the
		// distributed tracing.
		TLS: tlsInfoOf(cfg), Keystore: f.keystore, Telemetry: telemetryInfoOf(cfg, f.obs),
		// The metadata log of the local drives (compaction on demand).
		MetadataLogs: f.metaLogs,
	})
	// /nexo/health/cluster: the nodes and drives of the deployment that
	// answer, the write quorum and whether one more node may go down.
	healthSvc := health.New(health.Deps{
		Readiness: map[string]health.Check{"storage": object.ReadinessCheck(objects)},
		Nodes:     f.info.Nodes,
		Quorum: func(ctx context.Context, maintenance bool) (bool, string) {
			h := objects.Health(ctx, object.HealthOptions{Maintenance: maintenance})
			return h.Healthy, h.Message
		},
	})
	// Observability (observability.go): the scrape endpoints, the cluster
	// view of the logs, the families read from the services at every
	// scrape, the audit targets and the logger configuration.
	obsSvc, err := composeObservability(ctx, d, adminSvc, metrics.Sources{
		Node: f.info, HealQueue: mrf, Heal: healing.heal, Scanner: healing.scanner, Events: events, KMS: kmsState,
	}, auditor, dynConfig)
	if err != nil {
		return nil, fmt.Errorf("observability: %w", err)
	}

	codec := httpx.New()
	ids := httpx.NewIDGenerator()

	// The S3 API evaluates with the engine decorated by the applied
	// dynamic settings (api.root_access, hot reloaded).
	s3Policy := settings.GuardRootAccess(policies, applied)
	// The request limits of the node (S-OPS-10, S-DST-08), read from the
	// dynamic configuration at every request; the refusals are measured.
	limiter, err := ratelimit.New(ratelimit.Deps{Settings: applied, Clock: d.clock, Logger: logger.With("component", "ratelimit")})
	if err != nil {
		return nil, err
	}
	if limiter, err = metrics.WrapRateLimit(ctx, limiter, registry); err != nil {
		return nil, err
	}

	// S3 server (:9000): every route goes through the SigV4 middleware,
	// which admits anonymous requests; the handlers authorize with the
	// policy engine.
	s3Middlewares, err := s3Middlewares(middlewareDeps{
		logger: logger, clock: d.clock, codec: codec, ids: ids,
		registry: registry, verifier: verifier, auditor: auditor, domains: domainsOf(cfg),
		trustedProxies: cfg.TrustedProxies, forwardedHeaders: cfg.ForwardedHeaders, trustForwardedHost: cfg.TrustForwardedHost, dist: distStack,
		tracer: f.obs.trace, telemetry: f.obs.telemetry, limiter: limiter, activity: activity,
	})
	if err != nil {
		return nil, err
	}
	metricsRoutes, err := s3.NewMetricsRoutes(
		s3.MetricsDeps{Registry: registry, Exporter: obsSvc.exporter, Verifier: verifier, Policy: s3Policy, Enc: codec, Clock: d.clock},
		s3.WithPublicMetrics(cfg.PrometheusAuthType == config.PrometheusAuthPublic))
	if err != nil {
		return nil, err
	}
	mediaRoutes, err := s3.NewMediaRoutes(s3.MediaDeps{Media: mediaSvc, Policy: s3Policy, Objects: objects, Signer: signer, Creds: identities,
		Player: d.assets, Enc: codec, Clock: d.clock, Logger: logger}, s3.WithRegion(cfg.Region))
	if err != nil {
		return nil, err
	}
	replicationSvc, err := newReplication(d, objects, stores.buckets, distStack.docs)
	if err != nil {
		return nil, fmt.Errorf("replication: %w", err)
	}
	replEvents.bind(replicationSvc)
	tierSvc, err := newTiers(d, objects, distStack.docs)
	if err != nil {
		return nil, fmt.Errorf("tiers: %w", err)
	}
	tiers.bind(tierSvc)
	updateSvc, err := newUpdates(d, objects, distStack.docs, d.process())
	if err != nil {
		return nil, fmt.Errorf("updates: %w", err)
	}
	if f.updateNode != nil {
		f.updateNode.bind(updateSvc)
	}
	siteReplSvc, err := newSiteReplication(d, identities, policies, buckets, dynConfig, replicationSvc, distStack.docs)
	if err != nil {
		return nil, fmt.Errorf("site replication: %w", err)
	}
	replicationRoutes := s3.NewReplicationRoutes(s3.ReplicationDeps{Buckets: buckets, Replication: replicationSvc, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger})
	lifecycleRoutes := s3.NewLifecycleRoutes(s3.LifecycleDeps{Buckets: buckets, Lifecycle: lifecycle, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger},
		s3.WithDomains(domainsOf(cfg)...), s3.WithHostBuckets(distStack.hostBuckets()))
	notificationRoutes := s3.NewNotificationRoutes(s3.NotificationDeps{Buckets: buckets, Events: events, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger},
		s3.WithRegion(cfg.Region), s3.WithDomains(domainsOf(cfg)...), s3.WithHostBuckets(distStack.hostBuckets()))
	// SSE-C keys need TLS (or a trusted proxy that terminated it), unless
	// the configuration allows them in clear (tests).
	ssecOpt := s3.WithInsecureSSEC(cfg.SSECAllowInsecure)
	s3API := s3.NewAPI(s3.APIDeps{Enc: codec, Domains: domainsOf(cfg), HostBuckets: distStack.hostBuckets(), System: []httpx.Routes{
		s3.NewHealthRoutes(s3.HealthDeps{Health: healthSvc, Enc: codec}),
		metricsRoutes,
		mediaRoutes,
	}, Buckets: buckets, Settings: applied, Logger: logger},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: buckets, Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger},
			s3.WithRegion(cfg.Region)),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: cachedObjects, Policy: s3Policy, Media: mediaSvc, Select: s3select.New(s3select.Deps{Logger: logger}), Enc: codec, Clock: d.clock, Logger: logger}, ssecOpt, sendTimeoutOpt(cfg)),
		s3.NewMultipartRoutes(s3.MultipartDeps{Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger}, ssecOpt),
		s3.NewPresignedRoutes(s3.PresignedDeps{Verifier: verifier, Objects: objects, Buckets: buckets, Policy: s3Policy, Media: mediaSvc, Enc: codec, Clock: d.clock, Logger: logger},
			s3.WithRegion(cfg.Region), ssecOpt),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Verifier: verifier, Objects: objects, Policy: s3Policy, Enc: codec, Clock: d.clock, Logger: logger}, ssecOpt),
		s3.NewSTSRoutes(s3.STSDeps{IAM: identities, Policy: s3Policy, IDP: providers, Enc: codec, Clock: d.clock, Logger: logger}),
		// The ?lifecycle, ?notification and ?replication sub-resources.
		lifecycleRoutes,
		notificationRoutes,
		replicationRoutes,
	)
	if err := distStack.composeHTTP(distHTTPDeps{
		cfg: cfg, objects: cachedObjects, cache: cacheSvc, policy: s3Policy, verifier: verifier, publicAccess: accessRules, media: mediaSvc,
		secrets: accessrule.NewStaticSecrets(cfg.Secrets), registry: registry, ids: ids, clock: d.clock, logger: logger,
		keystore: f.keystore, tracer: f.obs.trace, telemetry: f.obs.telemetry, limiter: limiter, codec: codec,
	}); err != nil {
		return nil, fmt.Errorf("distributions: %w", err)
	}
	s3Srv := server.New(server.Deps{
		Routes:      []httpx.Routes{s3API},
		Middlewares: append(s3Middlewares, lifecycleRoutes, notificationRoutes),
		Hosts:       distStack.s3Hosts,
		Logger:      logger,
	}, append(append(serverOptions(cfg, f.keystore, cfg.Address, "s3"), distStack.tlsOptions(cfg)...), gatedOptions(f)...)...)

	// Console + Admin API server (:9001).
	consoleMiddlewares, err := consoleMiddlewares(middlewareDeps{
		logger: logger, clock: d.clock, codec: codec, ids: ids, registry: registry, trustedProxies: cfg.TrustedProxies,
		forwardedHeaders: cfg.ForwardedHeaders, trustForwardedHost: cfg.TrustForwardedHost,
		auditor: auditor, tracer: f.obs.trace, telemetry: f.obs.telemetry, limiter: limiter,
	})
	if err != nil {
		return nil, err
	}
	adminAPI, err := buildAdminAPI(adminDeps{
		codec: codec, limiter: limiter, clock: d.clock, logger: logger, region: cfg.Region, s3URL: publicS3URL(cfg),
		sessions: sessions, idp: providers, verifier: verifier, signer: signer, admin: adminSvc, iam: identities, policy: policies,
		buckets: buckets, objects: objects, media: mediaSvc, shares: shares, config: dynConfig, locks: f.lockManager, pools: healing.pools, secureCookies: secureConsole(cfg),
		consoleURL: cfg.BrowserRedirectURL, lifecycle: lifecycle, events: events, kms: keys, mediaJobs: mediaJobs,
		accessRules: accessRules, dist: distStack, acmeDirectory: cfg.ACMEDirectory, acmeChallenge: cfg.ACMEChallenge,
		cache: cacheSvc, replication: replicationSvc, siteReplication: siteReplSvc, tiers: tierSvc, updates: updateSvc, diagnostics: newDiagnostics(f, objects, buckets, logger),
		obsTrace: f.obs.trace, obsLogs: obsSvc.logs, obsAudit: auditor, obsHistory: f.obs.history,
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
	}, append(serverOptions(cfg, f.keystore, cfg.ConsoleAddress, "console"), gatedOptions(f)...)...)

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
		accessRules:  accessRules,
		dist:         distStack,
		cache:        cacheSvc,
		replication:  replicationSvc,
		siteRepl:     siteReplSvc,
		tiers:        tierSvc,
		updates:      updateSvc,
		events:       events,
		lifecycle:    lifecycle,
		s3Srv:        s3Srv,
		consSrv:      consSrv,
		healing:      healing,
		unsubscribe:  unsubscribe,
		obs:          obsSvc.runner,
	}, nil
}

// runners returns the background components of the services, started
// after the drives and stopped before them: the event dispatcher, then
// the healing services (stopped first).
func (s *services) runners() lifecycle.Runner {
	healing := s.healing.runner()
	// The observability first: the audit targets receive the entries of
	// the first requests, the logger configuration applies.
	return runners(s.obs, lifecycle.Funcs{
		OnStart: func(ctx context.Context) error {
			if err := s.events.Start(ctx); err != nil {
				return err
			}
			if err := healing.Start(ctx); err != nil {
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			if err := s.cache.Start(ctx); err != nil {
				_ = healing.Stop(context.WithoutCancel(ctx))
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			if err := s.replication.Start(ctx); err != nil {
				_ = s.cache.Stop(context.WithoutCancel(ctx))
				_ = healing.Stop(context.WithoutCancel(ctx))
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			if err := s.siteRepl.Start(ctx); err != nil {
				_ = s.replication.Stop(context.WithoutCancel(ctx))
				_ = s.cache.Stop(context.WithoutCancel(ctx))
				_ = healing.Stop(context.WithoutCancel(ctx))
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			if err := s.tiers.Start(ctx); err != nil {
				_ = s.siteRepl.Stop(context.WithoutCancel(ctx))
				_ = s.replication.Stop(context.WithoutCancel(ctx))
				_ = s.cache.Stop(context.WithoutCancel(ctx))
				_ = healing.Stop(context.WithoutCancel(ctx))
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			// The distributions last: their web listeners serve once
			// everything else runs.
			if err := s.dist.runner().Start(ctx); err != nil {
				_ = s.tiers.Stop(context.WithoutCancel(ctx))
				_ = s.siteRepl.Stop(context.WithoutCancel(ctx))
				_ = s.replication.Stop(context.WithoutCancel(ctx))
				_ = s.cache.Stop(context.WithoutCancel(ctx))
				_ = healing.Stop(context.WithoutCancel(ctx))
				_ = s.events.Stop(context.WithoutCancel(ctx))
				return err
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			s.unsubscribe()
			// A job this node orchestrates ends first.
			derr := s.updates.Stop(ctx)
			derr = errors.Join(derr, s.dist.runner().Stop(ctx))
			derr = errors.Join(derr, s.tiers.Stop(ctx))
			derr = errors.Join(derr, s.siteRepl.Stop(ctx))
			derr = errors.Join(derr, s.replication.Stop(ctx))
			derr = errors.Join(derr, s.cache.Stop(ctx))
			herr := errors.Join(derr, healing.Stop(ctx))
			if err := s.events.Stop(ctx); err != nil {
				return err
			}
			return herr
		},
	})
}

// gatedOptions are the options of the S3 and console servers whose
// handlers a cluster node serves behind the gate of its own listeners
// (App.joinCluster): those listeners already bound the stalls of the
// responses.
func gatedOptions(f *foundation) []server.Option {
	if !f.distributed() {
		return nil
	}
	return []server.Option{server.WithSendTimeout(0)}
}

// sendTimeoutOpt bounds the stalls of the object downloads
// (NEXO_SEND_TIMEOUT; the default of the S3 routes when zero).
func sendTimeoutOpt(cfg config.Static) s3.Option {
	if cfg.SendTimeout <= 0 {
		return nil
	}
	return s3.WithSendTimeout(cfg.SendTimeout)
}

// newDiagnostics returns the profiling and the speed tests of the node
// (S-OPS-11): its local drives, one drive of every other node of a
// cluster for the network test, the object layer.
func newDiagnostics(f *foundation, objects object.Service, buckets bucket.Service, logger *slog.Logger) adminsvc.Diagnostics {
	local := make(map[storage.StorageAPI]bool, len(f.local))
	for _, d := range f.local {
		local[d] = true
	}
	var peers []storage.StorageAPI
	for _, d := range f.drives {
		if !local[d] {
			peers = append(peers, d)
		}
	}
	return adminsvc.NewDiagnostics(adminsvc.DiagnosticsDeps{Drives: f.local, Peers: peers, Objects: objects, Buckets: buckets,
		Logger: logger.With("component", "diagnostics")}, adminsvc.WithDiagnosticsNode(f.nodeID))
}

// compressionPolicy selects the versions to compress from the
// "compression" subsystem of the dynamic configuration, read at every
// write; a configuration that cannot be read compresses nothing.
func compressionPolicy(applied settings.Service, logger *slog.Logger) object.CompressionPolicy {
	return func(ctx context.Context, _, key, contentType string) bool {
		c, err := applied.Compression(ctx)
		if err != nil {
			logger.WarnContext(ctx, "compression: settings unavailable, storing uncompressed", "err", err)
			return false
		}
		return c.Selects(key, contentType)
	}
}
