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
| 1 | Il file `service.go` contiene **solo** l'interfaccia, i tipi di input/output e la documentazione. Unica forma di metodo ammessa: l'adattatore di un tipo funzione (es. `ActionHandlerFunc.Handle`, come `http.HandlerFunc`). |
| 2 | L'implementazione è un tipo **non esportato**; si crea solo tramite `New(deps, opts...)`. |
| 3 | Le dipendenze sono **interfacce** passate nel costruttore (`Deps`); mai variabili di package. |
| 4 | Ogni metodo riceve `context.Context` come primo parametro. Eccezione documentata: gli **accessori puri** che non fanno I/O né bloccano (es. `StorageAPI.Endpoint()`, `IsOnline()`, `Healing()`, `Target.ID()`, `metrics.Registry.Handler()`, `topology.IDGenerator.NewID()`), indicati come tali nel commento, e i metodi di wiring/encoding del transport legati a una richiesta o a un router (`Middleware.Wrap`, `Routes.Register`, `httpx.Encoder`, `HTTPDoer.Do` dove il contesto viaggia nella `*http.Request`). Rilasci e operazioni che potranno fare RPC prendono sempre il contesto (`LockContext.Unlock(ctx)`, `HostResolver.Hostname(ctx)`). |
| 5 | Gli errori sono sentinel in `domain` (`domain.ErrBucketNotFound`), arricchiti con `fmt.Errorf("...: %w", err)`; i transport li mappano in codici S3/HTTP. Un package può definire sentinel più specifici purché **avvolgano** quelli di `domain` (es. `topology.ErrFormatMismatch` wrappa `domain.ErrFormatMismatch`), così `errors.Is` sul sentinel di dominio continua a funzionare. |
| 6 | Più implementazioni della stessa interfaccia convivono (es. `object.NewFS` e `object.NewErasure`), scelte in `app`. |
| 7 | Servizi con goroutine implementano anche `lifecycle.Runner` (`Start(ctx) error`, `Stop(ctx) error`). |
| 8 | Ogni interfaccia dei package `service`, `transport`, `config` e `cli` ha un mock in `mock/` generato da mockery (configurazione unica `.mockery.yaml` alla radice, `//go:generate` in `internal/service/doc.go`, `with-expecter`); i test unitari usano i mock (package esterno `*_test` per evitare cicli), i test di integrazione le implementazioni reali. Niente fake scritti a mano per interfacce che hanno un mock (lo stato condiviso si modella con `RunAndReturn` sul mock). Restano nel package solo i test degli helper non esportati; quando un test esterno deve ispezionare un dettaglio interno lo espone un `export_test.go`. Fuori da questo perimetro `clock.Fake` è l'implementazione deterministica di riferimento di `clock.Clock`. |
| 9 | Decoratori per responsabilità trasversali (metriche, tracing, audit, cache) implementano la stessa interfaccia e avvolgono il servizio: `metrics.WrapBucket(svc)`. Le librerie esterne non escono mai dal package che le usa: `metrics.Registry` è un'interfaccia neutrale (`Counter/Gauge/Histogram(ctx, metrics.Definition)` che restituiscono `metrics.Counter/Gauge/Histogram`, `Handler() http.Handler`) e il tipo non esportato `registry` incapsula `*prometheus.Registry` e `promhttp`; nessun tipo Prometheus compare fuori da `internal/service/metrics` (regola depguard). |
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
  `auth`, `requestid`, `audit`, `metrics`, `ratelimit`, `cors`, `recover`, costruite come ogni servizio con
  `NewX(XDeps, opts ...XOption)` (es. `middleware.NewAuth(middleware.AuthDeps{Verifier: v})`); una dipendenza
  obbligatoria mancante è un errore di wiring (`domain.ErrInvalidArgument`), mai un comportamento degradato.
- Il server HTTP è un servizio: `server.Service` (`Start/Stop`, registrazione dei `Routes`, selezione del router
  per `Host` tramite `distribution.Resolver`).

## 12.4 Composizione (`internal/app`)

```go
func Build(ctx context.Context, cfg config.Static, opts ...Option) (*App, error) {
    layout, _  := topo.Parse(ctx, cfg.Endpoints)                       // modalità e layout dei dischi
    drives, _  := openDrives(storage.Deps{...}, layout)                 // storage.NewLocal per ogni disco locale
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

- Un solo punto di wiring, leggibile, senza framework DI; la modalità (SNSD/SNMD/cluster) decide le implementazioni.
- `Build` **prepara i dischi** (crea le directory di sistema, scrive o verifica `format.json`) ma non apre socket:
  i listener partono in `Start`. In caso di errore di composizione i dischi già aperti vengono chiusi.
- `App.Run(ctx)` avvia i `Runner` in ordine, attende il segnale di stop e li ferma in ordine inverso (i dischi per ultimi).
- I `cmd/` non importano mai i package `service` o `transport` (regola depguard `cmd-no-layers`): le costanti
  condivise con i transport (es. i path di health `domain.HealthLivePath`/`HealthReadyPath`) stanno in `domain`;
  `cmd/nexo` passa a `app.Build` solo opzioni
  (`app.WithAssetsFS(fs.FS)`); `cmd/nexoctl` delega a `commands.Run(commands.DefaultDeps(), args)`: il punto di
  iniezione delle dipendenze di processo della CLI è `internal/cli/commands.Deps` (stream, `LookupEnv`,
  factory `Aliases` di `cli.AliasService`, `S3` e `Admin` (`cli.S3Factory`, `cli.AdminFactory`), `Clock`,
  `IsTerminal`, `HomeDir`, `Exec`), che i test dei comandi sostituiscono con i mock generati e con gli
  archivi in memoria costruiti su di essi (`clitest.MemS3`, `clitest.MemAdmin`); `cmd/nexoctl/main_test.go`
  verifica solo il cablaggio del processo (exit code di `--version`, `--help`, comando o alias sconosciuto).

## 12.5 Frontend e CLI

- **Dashboard**: stessa separazione in `web/src/services/`: `interfaces.ts` (es. `BucketService`) + `http/` (impl
  sul client JSON) + `mock/` (impl in memoria usata nei test e in modalità demo). I componenti ricevono i servizi
  da un `ServicesProvider` (context), mai importano `fetch` direttamente. I tipi di trasporto (`types.ts`) derivano
  dal contratto OpenAPI `api/openapi.yaml` tramite `http/schema.d.ts`, generato con `npm run gen:api`
  (`openapi-typescript`): il Go (`internal/transport/http/admin`) è l'implementazione di riferimento e un
  cambio di contratto rompe il typecheck della console.
- **CLI**: `internal/cli/` con `S3Client` e `AdminClient` come interfacce (factory `S3Factory`/`AdminFactory`)
  e i building block dei comandi (es. `Mirrorer`, il motore di `mirror` e `mirror --watch`: piano, esecuzione,
  eventi; `NewMirrorer(MirrorDeps, ...MirrorerOption)`): i comandi decodificano flag e argomenti, chiamano i
  building block e stampano i risultati;
  i comandi cobra di `internal/cli/commands` le ricevono tramite `commands.Deps` e vengono testati con i mock
  generati in `internal/cli/mock` (stato condiviso modellato con `RunAndReturn` in `internal/cli/clitest`),
  golden file e script `testscript` contro un server reale (`test/cli`).

## 12.6 Verifica in CI
- Lint custom (`go vet` + regole `depguard` di golangci-lint) che vieta import "verso l'alto": `transport`
  dentro `service`, `app` dentro `service`/`transport`, qualsiasi package interno dentro `domain`, `service` e
  `transport` dentro `cmd/` (`cmd-no-layers`) e `github.com/prometheus/**` fuori da `internal/service/metrics`.
- `internal/service/convention_test.go`: per ogni package `internal/service/*` verifica che esista `service.go`
  con almeno un'interfaccia esportata e nessuna funzione (salvo gli adattatori di tipo funzione), che
  l'implementazione sia non esportata e dichiari `var _ <Interfaccia> = (*impl)(nil)`, e che ogni interfaccia
  abbia il mock generato in `mock/`; `TestMockCoverage` estende il controllo dei mock alle interfacce di
  `transport`, `config` e `cli`; `TestNoMutableGlobals` vieta le variabili di package (ammessi solo
  `var _ I = ...` e i sentinel `errors.New`/`fmt.Errorf`; eccezione `internal/version`, valorizzato da `-ldflags`).
- Copertura dei mock: `go generate ./...` deve essere idempotente (`git diff --exit-code` in CI).
- Contratto Admin API: `npm run gen:api` deve essere idempotente (`git diff --exit-code` su
  `web/src/services/http/schema.d.ts`).

## 12.7 Autenticazione e autorizzazione dei transport
- **S3** (`:9000`): il middleware `auth` (SigV4 header, presigned, chunked, POST policy) mette l'identità nel
  contesto; ogni handler chiede al `policy.Service` l'azione `s3:*` corrispondente con le condizioni della
  richiesta (`aws:SourceIp`, `s3:prefix`, …) e la bucket policy; il middleware `audit` registra l'operazione
  (nome impostato dal dispatcher con `httpx.SetOperationName`).
- **Admin API** (`:9001/api/v1`): il middleware di autenticazione (`admin.NewAuthenticator`) valida il cookie di
  sessione e, sulle richieste mutanti, l'origine (`Sec-Fetch-Site: same-origin`, o in sua assenza `Origin`/`Referer`
  della console: le porte di uno stesso host sono same-site, quindi le pagine servite dal listener S3 ricevono il
  cookie `SameSite=Strict`) e il token CSRF (header `X-CSRF-Token`; il campo di form `csrfToken` solo sulla rotta
  `download-zip`; il token sta nel payload della sessione, mai in un cookie leggibile); la sessione viene **ri-risolta in IAM a ogni richiesta** (gruppi, policy, stato), così
  revoche e disabilitazioni hanno effetto immediato. Solo `POST /auth/login` è pubblico (`PublicRoutes`).
  Ogni endpoint mappa su un'azione `admin:*` (costanti `policy.ActionAdmin*`) valutata dallo stesso motore;
  le regole che dipendono dal chiamante oltre l'azione (ownership delle access key: root gestisce tutte le chiavi,
  un utente solo le proprie, le credenziali derivate nessuna) sono applicate dal servizio, che riceve l'identità
  nel contratto (`iam.Service.ListAccessKeys(ctx, caller, parent)`, `CreateAccessKeyInput.Caller`, ...);
  gli errori sono `problem+json` con `code` stabile (`unauthenticated`, `invalid_csrf_token`, `access_denied`).
