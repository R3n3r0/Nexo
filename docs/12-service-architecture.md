# 12. Service architecture (interface + implementation)

A binding rule for all of Nexo's Go code: **every functional unit is a service with a separate interface and
implementation**. This also applies to HTTP/RPC routes: every route group is a service (`Routes`) with an interface
and an implementation, which delegates the logic to the domain services. No business logic in handlers, no global
variables, no `init()` with side effects.

## 12.1 Layout

```
internal/
├── domain/                 # shared types and sentinel errors (no dependency on other internal packages)
│   ├── bucket.go object.go identity.go policy.go errors.go ...
├── service/                # one subpackage per service
│   ├── storage/            # StorageAPI: drive access (local, remote)
│   │   ├── service.go      # interface + input/output types of the service
│   │   ├── local.go        # implementation: local drive (xl.meta)
│   │   ├── remote.go       # implementation: RPC client to another node
│   │   ├── options.go      # functional Options
│   │   ├── mock/           # mock of the interface (generated with mockery)
│   │   └── *_test.go
│   ├── object/             # ObjectLayer: fs (SNSD) + erasure (SNMD/cluster)
│   ├── bucket/             # bucket metadata (versioning, policy, lifecycle, lock, quota, tags, cors, events)
│   ├── auth/               # SigV4 verification (header, query, chunked, POST policy)
│   ├── iam/                # users, groups, access keys, STS
│   ├── policy/             # policy engine (IAM + bucket policy + access rules)
│   ├── accessrule/         # prefix/glob/regex rules
│   ├── match/              # the single prefix/glob/regex (RE2) matcher + key normalization: access rules, behaviors, cache
│   ├── share/              # managed, revocable share links
│   ├── config/             # dynamic configuration
│   ├── event/              # bucket notifications
│   ├── lock/               # local and distributed locks (dsync)
│   ├── cluster/            # topology, bootstrap, peers
│   ├── erasure/            # RS coding + bitrot
│   ├── heal/ scanner/ ilm/ replication/
│   ├── kms/ crypto/
│   ├── session/            # console sessions (JWT cookie + CSRF)
│   ├── distribution/       # domains and distributions
│   ├── certs/              # per-domain certificates (upload, ACME from the leader, SNI)
│   ├── cache/              # per-domain HTTP cache
│   ├── metrics/ health/ audit/ trace/
│   └── admin/              # facade for the Admin API (info, usage, heal…)
├── transport/
│   ├── http/
│   │   ├── httpx/          # helpers: XML/JSON encoding, problem+json, request id, S3 errors
│   │   ├── middleware/     # middleware services (Middleware interface)
│   │   ├── s3/             # S3 Routes: bucket, object, multipart, versioning, policy, presigned, post…
│   │   ├── admin/          # Admin API Routes per area (users, policies, buckets, config, heal…)
│   │   ├── console/        # console Routes: embedded SPA, session
│   │   ├── dist/           # distribution Routes (website / s3 on a custom domain)
│   │   └── server/         # Server service: listeners, TLS, graceful shutdown, routing by Host
│   └── rpc/                # internode Routes (storage, lock, peer, bootstrap) + client
├── app/                    # composition: wires implementations ↔ interfaces, mode selection
└── testutil/               # in-process servers, fake drives, fault injection
```

## 12.2 Service contract

```go
// internal/service/bucket/service.go
package bucket

// Service manages the lifecycle and the metadata of buckets.
type Service interface {
    Create(ctx context.Context, in CreateInput) (*domain.Bucket, error)
    Get(ctx context.Context, name string) (*domain.Bucket, error)
    List(ctx context.Context, in ListInput) ([]domain.Bucket, error)
    Delete(ctx context.Context, name string, opts DeleteOptions) error
    SetVersioning(ctx context.Context, name string, v domain.Versioning) error
    // ...
}

// Deps lists the dependencies (interfaces only) of the implementation.
type Deps struct {
    Store  storage.Service
    Events event.Service
    Clock  clock.Clock
    Logger *slog.Logger
}
```

```go
// internal/service/bucket/bucket.go
package bucket

type service struct {
    deps Deps
    opts options
}

// compile-time check: the implementation satisfies the interface
var _ Service = (*service)(nil)

func New(deps Deps, opts ...Option) Service { ... }
```

Rules:
| # | Rule |
|---|--------|
| 1 | The `service.go` file contains **only** the interface, the input/output types and the documentation. The only method form allowed: the adapter of a function type (e.g. `ActionHandlerFunc.Handle`, like `http.HandlerFunc`). |
| 2 | The implementation is an **unexported** type; it is created only through `New(deps, opts...)`. |
| 3 | Dependencies are **interfaces** passed to the constructor (`Deps`); never package variables. |
| 4 | Every method takes `context.Context` as its first parameter. Documented exception: **pure accessors** that do no I/O and do not block (e.g. `StorageAPI.Endpoint()`, `IsOnline()`, `Healing()`, `Target.ID()`, `metrics.Registry.Handler()`, `topology.IDGenerator.NewID()`), marked as such in their comment, and the transport wiring/encoding methods bound to a request or a router (`Middleware.Wrap`, `Routes.Register`, `httpx.Encoder`, `HTTPDoer.Do`, where the context travels in the `*http.Request`). Releases and operations that may make RPCs always take the context (`LockContext.Unlock(ctx)`, `HostResolver.Hostname(ctx)`). |
| 5 | Errors are sentinels in `domain` (`domain.ErrBucketNotFound`), enriched with `fmt.Errorf("...: %w", err)`; the transports map them to S3/HTTP codes. A package may define more specific sentinels as long as they **wrap** those of `domain` (e.g. `topology.ErrFormatMismatch` wraps `domain.ErrFormatMismatch`), so `errors.Is` on the domain sentinel keeps working. |
| 6 | Several implementations of the same interface coexist (e.g. `object.NewFS` and `object.NewErasure`), chosen in `app`. |
| 7 | Services with goroutines also implement `lifecycle.Runner` (`Start(ctx) error`, `Stop(ctx) error`). |
| 8 | Every interface of the `service`, `transport`, `config` and `cli` packages has a mock in `mock/` generated by mockery (a single `.mockery.yaml` configuration at the root, `//go:generate` in `internal/service/doc.go`, `with-expecter`); unit tests use the mocks (external `*_test` package to avoid cycles), integration tests the real implementations. No hand-written fakes for interfaces that have a mock (shared state is modeled with `RunAndReturn` on the mock). Only the tests of unexported helpers stay in the package; when an external test must inspect an internal detail, an `export_test.go` exposes it. Outside this perimeter `clock.Fake` is the reference deterministic implementation of `clock.Clock`. |
| 9 | Decorators for cross-cutting concerns (metrics, tracing, audit, cache) implement the same interface and wrap the service: `metrics.WrapBucket(svc)`. External libraries never leave the package that uses them: `metrics.Registry` is a neutral interface (`Counter/Gauge/Histogram(ctx, metrics.Definition)` returning `metrics.Counter/Gauge/Histogram`, `Handler() http.Handler`) and the unexported type `registry` wraps `*prometheus.Registry` and `promhttp`; no Prometheus type appears outside `internal/service/metrics` (depguard rule). |
| 10 | No imports from `transport` to `app` or from `service` to `transport`. Dependencies only point downwards: `transport → service → domain`. |

## 12.3 Routes are services

```go
// internal/transport/http/s3/routes.go
package s3

// Routes registers a group of routes on a router.
type Routes interface {
    Register(r chi.Router)
}

// BucketRoutes exposes the S3 APIs on buckets.
type BucketRoutes interface{ Routes }

type bucketRoutes struct {
    buckets bucket.Service
    policy  policy.Service
    enc     httpx.Encoder
}

var _ BucketRoutes = (*bucketRoutes)(nil)

func NewBucketRoutes(buckets bucket.Service, policy policy.Service, enc httpx.Encoder) BucketRoutes { ... }

func (h *bucketRoutes) Register(r chi.Router) {
    r.Method(http.MethodPut, "/{bucket}", h.create)
    ...
}
```

- One `Routes` per functional area: `BucketRoutes`, `ObjectRoutes`, `MultipartRoutes`, `PresignedRoutes`,
  `PostPolicyRoutes`, `admin.UserRoutes`, `admin.PolicyRoutes`, `console.SessionRoutes`, `console.SPARoutes`,
  `rpc.StorageRoutes`, `rpc.LockRoutes`, etc.
- The handler does three things: **decodes** the request → **calls** a service → **encodes** the response/error.
- Middlewares are services: `type Middleware interface { Wrap(http.Handler) http.Handler }` with the implementations
  `auth`, `requestid`, `audit`, `metrics`, `ratelimit`, `cors`, `recover`, built like every service with
  `NewX(XDeps, opts ...XOption)` (e.g. `middleware.NewAuth(middleware.AuthDeps{Verifier: v})`); a missing required
  dependency is a wiring error (`domain.ErrInvalidArgument`), never degraded behavior.
- The HTTP server is a service: `server.Service` (`Start/Stop`, registration of the `Routes`, router selection by
  `Host` through `distribution.Resolver`).

## 12.4 Composition (`internal/app`)

```go
func Build(ctx context.Context, cfg config.Static, opts ...Option) (*App, error) {
    layout, _  := topo.Parse(ctx, cfg.Endpoints)                       // mode and drive layout
    drives, _  := openDrives(storage.Deps{...}, layout)                 // storage.NewLocal for each local drive
    depID, _   := storage.NewFormatter(...).EnsureFormats(ctx, layout, drives) // format.json
    store, _   := bucket.NewStore(bucket.StoreDeps{Drives: drives})
    var objects object.Service
    switch layout.Mode {
    case domain.ModeSNSD: objects, _ = object.NewFS(object.Deps{Store: drives[0], Buckets: store, Locks: locks, ...})
    case domain.ModeSNMD: objects, _ = object.NewErasure(object.Deps{Sets: sets, Parity: pool.Parity, DeploymentID: depID, ...})
    default:              return nil, fmt.Errorf("%w", domain.ErrNotImplemented) // cluster: M6
    }
    buckets, _ := bucket.New(bucket.Deps{Objects: objects, Store: store, ...})
    policies   := policy.New(policy.Deps{...})
    verifier,_ := auth.NewVerifier(auth.Deps{Creds: identities, ...}, auth.WithRegion(cfg.Region))
    sessions,_ := session.New(session.Deps{IAM: identities, ...}, session.WithSigningKey(session.DeriveKey(root, secret)))
    s3API      := s3.NewAPI(codec, s3.NewBucketRoutes(s3.BucketDeps{Buckets: buckets, Objects: objects, Policy: policies, Enc: codec}), ...)
    adminAPI   := adminhttp.NewAPI(adminhttp.APIDeps{Enc: codec, Auth: sessionAuth}, adminhttp.NewSessionRoutes(...), ...)
    group.Add("drives", lifecycle.Funcs{OnStop: closeDrives}).Add("events", events).Add("s3", s3Srv).Add("console", consSrv)
    return &App{group: group, ...}, nil
}
```

- A single, readable wiring point, without a DI framework; the mode (SNSD/SNMD/cluster) decides the implementations.
- `Build` **prepares the drives** (creates the system directories, writes or checks `format.json`) but opens no
  sockets: the listeners start in `Start`. On a composition error the drives already opened are closed.
- `App.Run(ctx)` starts the `Runner`s in order, waits for the stop signal and stops them in reverse order (the drives
  last).
- `cmd/` never imports the `service` or `transport` packages (depguard rule `cmd-no-layers`): the constants shared
  with the transports (e.g. the health paths `domain.HealthLivePath`/`HealthReadyPath`) live in `domain`;
  `cmd/nexo` passes only options to `app.Build` (`app.WithAssetsFS(fs.FS)`); `cmd/nexoctl` delegates to
  `commands.Run(commands.DefaultDeps(), args)`: the injection point of the CLI's process dependencies is
  `internal/cli/commands.Deps` (streams, `LookupEnv`, the `Aliases` factory of `cli.AliasService`, `S3` and `Admin`
  (`cli.S3Factory`, `cli.AdminFactory`), `Clock`, `IsTerminal`, `HomeDir`, `Exec`), which the command tests replace
  with the generated mocks and with the in-memory stores built on them (`clitest.MemS3`, `clitest.MemAdmin`);
  `cmd/nexoctl/main_test.go` only verifies the process wiring (exit code of `--version`, `--help`, an unknown command
  or alias).

## 12.5 Frontend and CLI

- **Dashboard**: the same separation in `web/src/services/`: `interfaces.ts` (e.g. `BucketService`) + `http/` (impl
  on the JSON client) + `mock/` (in-memory impl used in tests and in demo mode). Components receive the services from
  a `ServicesProvider` (context) and never import `fetch` directly. The transport types (`types.ts`) derive from the
  OpenAPI contract `api/openapi.yaml` through `http/schema.d.ts`, generated with `npm run gen:api`
  (`openapi-typescript`): the Go code (`internal/transport/http/admin`) is the reference implementation and a
  contract change breaks the console's typecheck.
- **CLI**: `internal/cli/` with `S3Client` and `AdminClient` as interfaces (factories `S3Factory`/`AdminFactory`) and
  the building blocks of the commands (e.g. `Mirrorer`, the engine of `mirror` and `mirror --watch`: plan, execution,
  events; `NewMirrorer(MirrorDeps, ...MirrorerOption)`): the commands decode flags and arguments, call the building
  blocks and print the results; the cobra commands of `internal/cli/commands` receive them through `commands.Deps`
  and are tested with the mocks generated in `internal/cli/mock` (shared state modeled with `RunAndReturn` in
  `internal/cli/clitest`), golden files and `testscript` scripts against a real server (`test/cli`).

## 12.6 Verification in CI
- Custom lint (`go vet` + golangci-lint `depguard` rules) that forbids "upward" imports: `transport` inside `service`,
  `app` inside `service`/`transport`, any internal package inside `domain`, `service` and `transport` inside `cmd/`
  (`cmd-no-layers`) and `github.com/prometheus/**` outside `internal/service/metrics`.
- `internal/service/convention_test.go`: for every `internal/service/*` package it checks that `service.go` exists
  with at least one exported interface and no functions (except function-type adapters), that the implementation is
  unexported and declares `var _ <Interface> = (*impl)(nil)`, and that every interface has its generated mock in
  `mock/`; `TestMockCoverage` extends the mock check to the interfaces of `transport`, `config` and `cli`;
  `TestNoMutableGlobals` forbids package variables (only `var _ I = ...` and the `errors.New`/`fmt.Errorf` sentinels
  are allowed; exception: `internal/version`, set by `-ldflags`).
- Mock coverage: `go generate ./...` must be idempotent (`git diff --exit-code` in CI).
- Admin API contract: `npm run gen:api` must be idempotent (`git diff --exit-code` on
  `web/src/services/http/schema.d.ts`).

## 12.7 Transport authentication and authorization
- **S3** (`:9000`): the `auth` middleware (SigV4 header, presigned, chunked, POST policy) puts the identity in the
  context; every handler asks the `policy.Service` for the corresponding `s3:*` action with the request's conditions
  (`aws:SourceIp`, `s3:prefix`, …) and the bucket policy; the `audit` middleware records the operation (name set by
  the dispatcher with `httpx.SetOperationName`).
- **Admin API** (`:9001/api/v1`): the authentication middleware (`admin.NewAuthenticator`) validates the session
  cookie and, on mutating requests, the origin (`Sec-Fetch-Site: same-origin`, or in its absence the console's
  `Origin`/`Referer`: the ports of the same host are same-site, so the pages served by the S3 listener receive the
  `SameSite=Strict` cookie) and the CSRF token (header `X-CSRF-Token`; the `csrfToken` form field only on the
  `download-zip` route; the token lives in the session payload, never in a readable cookie); the session is
  **re-resolved in IAM on every request** (groups, policies, status), so revocations and disablements take effect
  immediately. Only `POST /auth/login` is public (`PublicRoutes`). Every endpoint maps to an `admin:*` action
  (constants `policy.ActionAdmin*`) evaluated by the same engine; the rules that depend on the caller beyond the
  action (ownership of access keys: root manages all keys, a user only their own, derived credentials none) are
  enforced by the service, which receives the identity in the contract
  (`iam.Service.ListAccessKeys(ctx, caller, parent)`, `CreateAccessKeyInput.Caller`, ...); errors are `problem+json`
  with a stable `code` (`unauthenticated`, `invalid_csrf_token`, `access_denied`).
