# 1. Architettura

## 1.1 Obiettivi

| Obiettivo | Descrizione |
|-----------|-------------|
| Compatibilità S3 | I client esistenti (aws-cli, aws-sdk-*, rclone, s3cmd, boto3, Terraform) funzionano senza modifiche. |
| Semplicità operativa | Un binario statico, nessuna dipendenza esterna obbligatoria (niente DB, niente ZooKeeper/etcd). |
| Durabilità | Erasure coding Reed-Solomon + protezione bitrot + healing automatico. |
| Scalabilità orizzontale | Da 1 nodo / 1 disco a N pool di M nodi; espansione senza downtime. |
| Gestione completa | Dashboard React e CLI coprono il 100% delle operazioni amministrative. |
| Osservabilità | Metriche Prometheus, log strutturati, audit log, tracing OpenTelemetry. |

### Non-obiettivi (v1)
- Filesystem POSIX/NFS gateway.
- Gateway verso altri cloud (Azure/GCS).
- Compatibilità con API S3 deprecate rare (SigV2, BitTorrent, `SelectObjectContent` in v1 è opzionale).

## 1.2 Componenti

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                                  nexo server                                 │
│                                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────────────────────────┐   │
│  │ S3 HTTP API  │  │  Admin API   │  │ Console (React SPA, go:embed)     │   │
│  │  :9000       │  │  :9001/api   │  │ :9001/                            │   │
│  └──────┬───────┘  └──────┬───────┘  └───────────────┬───────────────────┘   │
│         │  SigV4          │ SigV4 / Session JWT      │ chiama Admin API       │
│  ┌──────▼─────────────────▼──────────────────────────▼───────────────────┐   │
│  │ Middleware: request-id, tracing, rate limit, auth, audit, metrics      │   │
│  └──────┬────────────────────────────────────────────────────────────────┘   │
│  ┌──────▼──────┐  ┌──────────┐ ┌───────────┐ ┌───────────┐ ┌────────────┐    │
│  │    IAM      │  │  Policy  │ │  Config   │ │  Events   │ │  KMS/SSE   │    │
│  │ users/groups│  │  engine  │ │  store    │ │  notifier │ │            │    │
│  └──────┬──────┘  └──────────┘ └───────────┘ └───────────┘ └────────────┘    │
│  ┌──────▼────────────────────────────────────────────────────────────────┐   │
│  │ ObjectLayer (interfaccia)                                             │   │
│  │   ├─ ServerPools  ── seleziona pool (spazio libero / esistenza)        │   │
│  │   │    └─ ErasureSets ── hash(key) → set                               │   │
│  │   │         └─ ErasureObjects ── encode/decode RS, quorum, xl.meta     │   │
│  │   └─ Servizi di background: Scanner, Healer, ILM, Replication, MRF     │   │
│  └──────┬────────────────────────────────────────────────────────────────┘   │
│  ┌──────▼──────────────────────┐   ┌──────────────────────────────────────┐   │
│  │ StorageAPI (interfaccia)    │   │ Lock: local + dsync (distribuito)    │   │
│  │  ├─ xlStorage (disco locale)│   └──────────────────────────────────────┘   │
│  │  └─ storageRESTClient (rete)│◀──── Internode RPC :9000/nexo/rpc (mTLS) ─▶   │
│  └─────────────────────────────┘                                             │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Responsabilità

| Componente | Package | Responsabilità |
|------------|---------|----------------|
| S3 API | `internal/s3api` | Routing S3 (path-style e virtual-host), parsing/encoding XML, mapping errori S3. |
| Auth | `internal/auth` | Verifica SigV4 (header, query presigned, streaming chunked `aws-chunked`, trailer checksum), POST policy. |
| IAM | `internal/iam` | Utenti, gruppi, service account, policy, STS (AssumeRole, OIDC, LDAP in fasi successive). |
| Policy | `internal/policy` | Valutazione policy IAM e bucket policy (formato AWS), condizioni, variabili `${aws:username}`. |
| ObjectLayer | `internal/object` | API logica (PutObject, GetObject, ListObjects, Multipart, Versioni). |
| Erasure | `internal/erasure` | Codifica Reed-Solomon (`klauspost/reedsolomon`), bitrot (HighwayHash256), quorum. |
| Storage | `internal/storage` | Accesso a disco: `xl.meta` (msgpack), scrittura atomica (tmp + rename + fsync), O_DIRECT opzionale. |
| Lock | `internal/dsync` | Lock locali e distribuiti a quorum con refresh e lease. |
| Cluster | `internal/cluster` | Topologia, endpoint, bootstrap, health dei peer, notifiche peer. |
| RPC | `internal/rpc` | Trasporto internode (HTTP/2 + msgpack, streaming), autenticazione JWT internode. |
| Background | `internal/scanner`, `internal/heal`, `internal/ilm`, `internal/replication` | Scansione dati, usage, healing, lifecycle, replica. |
| Events | `internal/event` | Notifiche bucket verso Webhook, Kafka, NATS, AMQP, Redis, PostgreSQL. |
| Crypto | `internal/crypto`, `internal/kms` | SSE-S3, SSE-C, SSE-KMS; KMS interno o esterno (Vault/KES). |
| Admin API | `internal/admin` | REST JSON per gestione (usata da CLI e dashboard). |
| Console | `internal/console` + `web/` | Server SPA, sessioni, proxy verso Admin API, SSE/WebSocket per log e metriche live. |
| Metrics | `internal/metrics` | Prometheus `/nexo/metrics/v1/{cluster,node,bucket}`. |
| Config | `internal/config` | Config dinamica (KV) memorizzata nel bucket di sistema `.nexo.sys`, più env/flag. |
| CLI | `cmd/nexoctl`, `internal/cli` | Client S3 + admin, alias, output tabellare/JSON. |

## 1.3 Stack tecnologico

### Backend (Go ≥ 1.27)
| Ambito | Scelta | Motivazione |
|--------|--------|-------------|
| HTTP | `net/http` + router leggero (`go-chi/chi` v5) | Prestazioni, controllo fine su streaming. |
| Erasure coding | `github.com/klauspost/reedsolomon` | SIMD (AVX2/AVX512/NEON), standard de-facto. |
| Hash bitrot | `github.com/minio/highwayhash` | Molto veloce, 256 bit. |
| Serializzazione metadati | `github.com/tinylib/msgp` | Codegen, zero-alloc. |
| Compressione | `github.com/klauspost/compress` (S2/zstd) | Compressione trasparente opzionale. |
| Config/CLI | `spf13/cobra` + `spf13/pflag` | Standard per CLI Go. |
| Log | `log/slog` (JSON) | Standard library. |
| Metriche | `prometheus/client_golang` | Standard. |
| Tracing | OpenTelemetry Go SDK | Interoperabilità. |
| JWT | `golang-jwt/jwt/v5` | Sessioni console, token internode, STS. |
| Crypto | `crypto/*` + `golang.org/x/crypto` (Argon2id, ChaCha20-Poly1305) | DARE format per SSE. |

### Frontend
| Ambito | Scelta |
|--------|--------|
| Framework | React 19 + TypeScript (strict) |
| Build | Vite |
| Routing | React Router v7 (data router) |
| Data fetching | TanStack Query |
| UI | Mantine (componenti, tabelle, form, notifiche, tema chiaro/scuro) |
| Tabelle grandi | TanStack Table + virtualizzazione (`@tanstack/react-virtual`) |
| Grafici | Recharts |
| Editor JSON policy | Monaco Editor (lazy-loaded) |
| Upload | multipart client-side con presigned URL / upload via console API, chunk paralleli, resume |
| i18n | `i18next` (it, en) |
| Test | Vitest, React Testing Library, MSW, Playwright, axe-core |

### Embedding della dashboard in Go

```go
// web/embed.go
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
```

- `make build` esegue `npm ci && npm run build` in `web/`, poi `go build` con tag `-tags embedui`.
- Senza il tag (sviluppo backend) viene usato uno stub che restituisce una pagina "console non inclusa".
- Il server console:
  - serve i file statici con `Cache-Control: public, max-age=31536000, immutable` per gli asset hashati;
  - `index.html` con `no-cache` e header CSP con nonce;
  - **fallback SPA**: ogni path non `/api/*` e non asset → `index.html`;
  - compressione gzip/br pre-calcolata in fase di build (`vite-plugin-compression`).
- In sviluppo: `vite dev` su `:5173` con proxy `/api` → `:9001`.

## 1.4 Layout del repository

> Il layout definitivo e la convenzione **interfaccia + implementazione per ogni servizio e per ogni gruppo di rotte** sono in [12 — Architettura a servizi](12-architettura-servizi.md), che prevale su questo schema.

```
nexo/
├── cmd/
│   ├── nexo/              # main server
│   ├── nexoctl/           # main CLI
│   └── tools/traceability # strumento di sviluppo: matrice funzionalità → test (fuori dai rilasci)
├── internal/
│   ├── admin/  auth/  cli/  cluster/  config/  console/  crypto/  dsync/
│   ├── erasure/  event/  heal/  iam/  ilm/  kms/  metrics/  object/
│   ├── policy/  replication/  rpc/  s3api/  scanner/  storage/
│   ├── testutil/          # helper per test, server in-process, cluster fake
│   └── tools/             # logica degli strumenti di sviluppo (es. traceability)
├── pkg/
│   └── nexoadmin/         # client Go pubblico dell'Admin API (usato da nexoctl)
├── web/                   # dashboard React (Vite)
│   ├── src/  e2e/  public/
│   └── embed.go
├── test/
│   ├── integration/       # test con aws-sdk-go-v2 contro server reale
│   ├── cluster/           # docker-compose + test di chaos
│   ├── compat/            # runner s3-tests (Ceph) e mint
│   ├── cli/               # script testscript (.txtar)
│   └── TRACEABILITY.md    # matrice di tracciabilità generata (06 §6.8)
├── deploy/
│   ├── docker/  helm/  systemd/
├── docs/
├── Makefile
└── go.mod                 # module github.com/r3n3r0/nexo
```

## 1.5 Modello dei dati su disco

```
/data1/
├── .nexo.sys/
│   ├── format.json            # id deployment, id disco, posizione nel set, algoritmo
│   ├── tmp/                   # scritture in corso (pulite all'avvio)
│   ├── multipart/             # parti multipart in corso
│   ├── buckets/<bucket>/      # metadati bucket (.metadata.bin: policy, versioning, lifecycle...)
│   ├── config/                # config cluster, IAM (cifrati)
│   └── healing.bin            # stato healing del disco
└── <bucket>/
    └── <object-key>/
        ├── xl.meta            # metadati di tutte le versioni (msgpack)
        └── <data-dir-uuid>/
            └── part.1         # shard erasure + checksum bitrot per blocco
```

- **Oggetti piccoli (< 128 KiB)**: i dati sono inline in `xl.meta` per ridurre IOPS.
- **xl.meta** contiene per ogni versione: version-id, mod-time, size, ETag, content-type, metadati utente,
  layout erasure (data/parity, block size, indice shard, distribuzione), checksum, flag delete-marker,
  stato replica, stato transizione ILM, chiavi SSE sigillate.
- **Scritture atomiche**: dati in `.nexo.sys/tmp/<uuid>` → fsync → `RenameData` atomico in posizione finale
  → aggiornamento `xl.meta`.

## 1.6 Flusso di una PutObject (cluster)

1. Middleware: request-id, verifica SigV4, valutazione policy (`s3:PutObject`).
2. `ServerPools`: se l'oggetto esiste già in un pool → stesso pool; altrimenti pool con più spazio libero (pesato).
3. `ErasureSets`: `set = sipHash(key, deploymentID) % numSets`.
4. Lock distribuito di scrittura sul nome oggetto (quorum `n/2+1` dei nodi del set).
5. Stream del body: blocchi da 1 MiB → encode RS (D data + P parity) → scrittura parallela sugli N dischi
   (locali o via `storageRESTClient`), con hash bitrot per blocco.
6. Commit: `RenameData` su tutti i dischi; successo se `≥ writeQuorum`. Dischi falliti → coda **MRF** per healing.
7. Notifica evento `s3:ObjectCreated:Put`, audit log, metriche, eventuale replica asincrona.

## 1.7 Decisioni architetturali (ADR sintetiche)

| ADR | Decisione | Alternativa scartata | Motivo |
|-----|-----------|----------------------|--------|
| 001 | Nessun DB di metadati: i metadati vivono accanto ai dati (`xl.meta`) | etcd/Raft per metadati oggetti | Elimina un single point of failure e un collo di bottiglia; scala linearmente. |
| 002 | Topologia statica a pool, espandibile aggiungendo pool | Consistent hashing dinamico con ribilanciamento | Prevedibilità, nessun ribilanciamento massivo; vedi [Cluster](03-cluster.md). |
| 003 | IAM e config salvati come oggetti cifrati in `.nexo.sys` | DB esterno | Stessa durabilità dei dati, zero dipendenze. |
| 004 | Admin API JSON separata dall'API S3 | Estensioni proprietarie dentro S3 | Separazione netta, versionabile (`/api/v1`). |
| 005 | Dashboard embedded nel binario | Container separato | Deploy in un solo artefatto, versione UI sempre allineata al server. |
| 006 | Console autentica con sessione (cookie HttpOnly + CSRF) emessa dopo login con access/secret key o OIDC | Secret key nel browser | Il segreto non resta mai nel browser. |
| 007 | CLI usa SigV4 anche per l'Admin API | Token custom | Riutilizzo delle credenziali e della firma già implementata. |
