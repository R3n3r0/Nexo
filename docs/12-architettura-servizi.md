# 12. Architettura a servizi (interfaccia + implementazione)

Regola vincolante per tutto il codice Go di Nexo: **ogni unità funzionale è un servizio con un'interfaccia e
un'implementazione separate**. Vale anche per le rotte HTTP/RPC: ogni gruppo di rotte è un servizio (`Routes`)
con interfaccia e implementazione, che delega la logica ai servizi di dominio. Nessuna logica di business nei
handler, nessuna variabile globale, nessun `init()` con effetti collaterali.

## 12.1 Layout

```
internal/
├── domain/                 # tipi condivisi e errori sentinel (nessuna dipendenza da altri package interni)
│   ├── bucket.go object.go identity.go policy.go errors.go ...
├── service/                # un sottopackage per servizio
│   ├── storage/            # StorageAPI: accesso ai dischi (locale, remoto)
│   │   ├── service.go      # interfaccia + tipi di input/output del servizio
│   │   ├── local.go        # implementazione: disco locale (xl.meta)
│   │   ├── remote.go       # implementazione: client RPC verso un altro nodo
│   │   ├── options.go      # Option funzionali
│   │   ├── mock/           # mock dell'interfaccia (generato con mockery)
│   │   └── *_test.go
│   ├── object/             # ObjectLayer: fs (SNSD) + erasure (SNMD/cluster)
│   ├── bucket/             # metadati bucket (versioning, policy, lifecycle, lock, quota, tag, cors, eventi)
│   ├── auth/               # verifica SigV4 (header, query, chunked, POST policy)
│   ├── iam/                # utenti, gruppi, access key, STS
│   ├── policy/             # policy engine (IAM + bucket policy + access rules)
│   ├── accessrule/         # regole prefix/glob/regex
│   ├── share/              # share link gestiti e revocabili
│   ├── config/             # configurazione dinamica
│   ├── event/              # notifiche bucket
│   ├── lock/               # lock locali e distribuiti (dsync)
│   ├── cluster/            # topologia, bootstrap, peer
│   ├── erasure/            # codifica RS + bitrot
│   ├── heal/ scanner/ ilm/ replication/
│   ├── kms/ crypto/
│   ├── session/            # sessioni console (JWT cookie + CSRF)
│   ├── distribution/       # domini e distribuzioni
│   ├── cache/              # cache HTTP per dominio
│   ├── metrics/ health/ audit/ trace/
│   └── admin/              # facciata per l'Admin API (info, usage, heal…)
├── transport/
│   ├── http/
│   │   ├── httpx/          # helper: encoding XML/JSON, problem+json, request-id, errori S3
│   │   ├── middleware/     # servizi middleware (Middleware interface)
│   │   ├── s3/             # Routes S3: bucket, object, multipart, versioning, policy, presigned, post…
│   │   ├── admin/          # Routes Admin API per area (users, policies, buckets, config, heal…)
│   │   ├── console/        # Routes console: SPA embedded, sessione
│   │   ├── dist/           # Routes distribuzioni (website / s3 su dominio custom)
│   │   └── server/         # Server service: listener, TLS, graceful shutdown, routing per Host
│   └── rpc/                # Routes internode (storage, lock, peer, bootstrap) + client
├── app/                    # composizione: collega implementazioni ↔ interfacce, selezione modalità
└── testutil/               # server in-process, dischi fittizi, fault injection
```

## 12.2 Contratto di un servizio

```go
// internal/service/bucket/service.go
package bucket

// Service gestisce il ciclo di vita e i metadati dei bucket.
type Service interface {
    Create(ctx context.Context, in CreateInput) (*domain.Bucket, error)
    Get(ctx context.Context, name string) (*domain.Bucket, error)
    List(ctx context.Context, in ListInput) ([]domain.Bucket, error)
    Delete(ctx context.Context, name string, opts DeleteOptions) error
    SetVersioning(ctx context.Context, name string, v domain.Versioning) error
    // ...
}

// Deps elenca le dipendenze (solo interfacce) dell'implementazione.
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

// compile-time check: l'implementazione soddisfa l'interfaccia
var _ Service = (*service)(nil)

func New(deps Deps, opts ...Option) Service { ... }
```

Regole:
| # | Regola |
|---|--------|
| 1 | Il file `service.go` contiene **solo** l'interfaccia, i tipi di input/output e la documentazione. |
| 2 | L'implementazione è un tipo **non esportato**; si crea solo tramite `New(deps, opts...)`. |
| 3 | Le dipendenze sono **interfacce** passate nel costruttore (`Deps`); mai variabili di package. |
| 4 | Ogni metodo riceve `context.Context` come primo parametro. |
| 5 | Gli errori sono sentinel in `domain` (`domain.ErrBucketNotFound`), arricchiti con `fmt.Errorf("...: %w", err)`; i transport li mappano in codici S3/HTTP. |
| 6 | Più implementazioni della stessa interfaccia convivono (es. `object.NewFS` e `object.NewErasure`), scelte in `app`. |
| 7 | Servizi con goroutine implementano anche `lifecycle.Runner` (`Start(ctx) error`, `Stop(ctx) error`). |
| 8 | Ogni interfaccia ha un mock in `mock/` (mockery, `//go:generate`); i test unitari usano i mock, i test di integrazione le implementazioni reali. |
| 9 | Decoratori per responsabilità trasversali (metriche, tracing, audit, cache) implementano la stessa interfaccia e avvolgono il servizio: `metrics.WrapBucket(svc)`. |
| 10 | Nessun import da `transport` verso `app` e da `service` verso `transport`. Dipendenze solo verso il basso: `transport → service → domain`. |

## 12.3 Le rotte sono servizi

```go
// internal/transport/http/s3/routes.go
package s3

// Routes registra un gruppo di rotte su un router.
type Routes interface {
    Register(r chi.Router)
}

// BucketRoutes espone le API S3 sui bucket.
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

- Un `Routes` per area funzionale: `BucketRoutes`, `ObjectRoutes`, `MultipartRoutes`, `PresignedRoutes`,
  `PostPolicyRoutes`, `admin.UserRoutes`, `admin.PolicyRoutes`, `console.SessionRoutes`, `console.SPARoutes`,
  `rpc.StorageRoutes`, `rpc.LockRoutes`, ecc.
- Il handler fa tre cose: **decodifica** la richiesta → **chiama** un servizio → **codifica** la risposta/errore.
- I middleware sono servizi: `type Middleware interface { Wrap(http.Handler) http.Handler }` con implementazioni
  `auth`, `requestid`, `audit`, `metrics`, `ratelimit`, `cors`, `recover`.
- Il server HTTP è un servizio: `server.Service` (`Start/Stop`, registrazione dei `Routes`, selezione del router
  per `Host` tramite `distribution.Resolver`).

## 12.4 Composizione (`internal/app`)

```go
func Build(ctx context.Context, cfg config.Static) (*App, error) {
    store   := storage.NewLocalSet(cfg.Endpoints...)           // o remoto in cluster
    locks   := lock.NewLocal()                                  // o lock.NewDistributed(peers)
    var objects object.Service
    switch cfg.Mode {
    case domain.ModeSNSD:    objects = object.NewFS(object.Deps{Store: store, Locks: locks, ...})
    default:                 objects = object.NewErasure(object.Deps{Pools: pools, Locks: locks, ...})
    }
    objects  = metrics.WrapObject(objects, reg)
    buckets  := bucket.New(bucket.Deps{Objects: objects, ...})
    ...
    s3Routes := []s3.Routes{ s3.NewBucketRoutes(...), s3.NewObjectRoutes(...), ... }
    srv := server.New(server.Deps{Routes: ..., Middlewares: ..., Resolver: dist})
    return &App{runners: []lifecycle.Runner{srv, scanner, healer, ...}}, nil
}
```

- Un solo punto di wiring, leggibile, senza framework DI; la modalità (SNSD/SNMD/cluster) decide le implementazioni.
- `App.Run(ctx)` avvia i `Runner` in ordine, attende il segnale di stop e li ferma in ordine inverso.

## 12.5 Frontend e CLI

- **Dashboard**: stessa separazione in `web/src/services/`: `interfaces.ts` (es. `BucketService`) + `http/` (impl
  su client OpenAPI) + `mock/` (impl in memoria usata nei test e in modalità demo). I componenti ricevono i servizi
  da un `ServicesProvider` (context), mai importano `fetch` direttamente.
- **CLI**: `internal/cli/` con `S3Client` e `AdminClient` come interfacce; i comandi cobra ricevono le interfacce
  e vengono testati con implementazioni finte.

## 12.6 Verifica in CI
- Lint custom (`go vet` + regola `depguard` di golangci-lint) che vieta import "verso l'alto" e import di
  `transport` dentro `service`.
- Test che per ogni package `internal/service/*` esista un `service.go` con un'interfaccia esportata e un
  `var _ Service = (*…)(nil)`.
- Copertura dei mock: `go generate ./...` deve essere idempotente (diff vuoto in CI).
