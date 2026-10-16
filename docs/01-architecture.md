# 1. Architecture

## 1.1 Goals

| Goal | Description |
|------|-------------|
| S3 compatibility | Existing clients (aws-cli, aws-sdk-*, rclone, s3cmd, boto3, Terraform) work without changes. |
| Operational simplicity | One static binary, no mandatory external dependency (no DB, no ZooKeeper/etcd). |
| Durability | Reed-Solomon erasure coding + bitrot protection + automatic healing. |
| Horizontal scalability | From 1 node / 1 disk to N pools of M nodes; expansion without downtime. |
| Complete management | The React dashboard and the CLI cover 100% of the administrative operations. |
| Observability | Prometheus metrics, structured logs, audit log, OpenTelemetry tracing. |

### Non-goals (v1)
- POSIX/NFS filesystem gateway.
- Gateway to other clouds (Azure/GCS).
- Compatibility with rare deprecated S3 APIs (SigV2, BitTorrent; `SelectObjectContent` is optional in v1).

## 1.2 Components

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                                  nexo server                                 │
│                                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌───────────────────────────────────┐   │
│  │ S3 HTTP API  │  │  Admin API   │  │ Console (React SPA, go:embed)     │   │
│  │  :9000       │  │  :9001/api   │  │ :9001/                            │   │
│  └──────┬───────┘  └──────┬───────┘  └───────────────┬───────────────────┘   │
│         │  SigV4          │ SigV4 / Session JWT      │ calls Admin API        │
│  ┌──────▼─────────────────▼──────────────────────────▼───────────────────┐   │
│  │ Middleware: request-id, tracing, rate limit, auth, audit, metrics      │   │
│  └──────┬────────────────────────────────────────────────────────────────┘   │
│  ┌──────▼──────┐  ┌──────────┐ ┌───────────┐ ┌───────────┐ ┌────────────┐    │
│  │    IAM      │  │  Policy  │ │  Config   │ │  Events   │ │  KMS/SSE   │    │
│  │ users/groups│  │  engine  │ │  store    │ │  notifier │ │            │    │
│  └──────┬──────┘  └──────────┘ └───────────┘ └───────────┘ └────────────┘    │
│  ┌──────▼────────────────────────────────────────────────────────────────┐   │
│  │ ObjectLayer (interface)                                               │   │
│  │   ├─ ServerPools  ── selects pool (free space / existence)            │   │
│  │   │    └─ ErasureSets ── hash(key) → set                               │   │
│  │   │         └─ ErasureObjects ── encode/decode RS, quorum, xl.meta     │   │
│  │   └─ Background services: Scanner, Healer, ILM, Replication, MRF      │   │
│  └──────┬────────────────────────────────────────────────────────────────┘   │
│  ┌──────▼──────────────────────┐   ┌──────────────────────────────────────┐   │
│  │ StorageAPI (interface)      │   │ Lock: local + dsync (distributed)    │   │
│  │  ├─ xlStorage (local disk)  │   └──────────────────────────────────────┘   │
│  │  └─ storageRESTClient (net) │◀──── Internode RPC :9000/nexo/rpc (mTLS) ─▶   │
│  └─────────────────────────────┘                                             │
└──────────────────────────────────────────────────────────────────────────────┘
```

### Responsibilities

| Component | Package | Responsibility |
|-----------|---------|----------------|
| S3 API | `internal/s3api` | S3 routing (path-style and virtual-host), XML parsing/encoding, S3 error mapping. |
| Auth | `internal/auth` | SigV4 verification (header, presigned query, `aws-chunked` streaming, checksum trailer), POST policy. |
| IAM | `internal/iam` | Users, groups, service accounts, policies, STS (AssumeRole; OIDC and LDAP in later phases). |
| Policy | `internal/policy` | Evaluation of IAM policies and bucket policies (AWS format), conditions, `${aws:username}` variables. |
| ObjectLayer | `internal/object` | Logical API (PutObject, GetObject, ListObjects, Multipart, Versions). |
| Erasure | `internal/erasure` | Reed-Solomon coding (`klauspost/reedsolomon`), bitrot (HighwayHash256), quorum. |
| Storage | `internal/storage` | Disk access: `xl.meta` (msgpack), atomic writes through the page cache (tmp + rename + fsync; O_DIRECT measured and rejected, [bench §7.2](bench/README.md#72-o_direct-for-large-data-files-rejected)), part streams handed to `sendfile` for zero-copy single-drive downloads ([bench §7.1](bench/README.md#71-zero-copy-downloads-kept)). |
| Lock | `internal/dsync` | Local and distributed quorum locks with refresh and lease. |
| Cluster | `internal/cluster` | Topology, endpoints, bootstrap, peer health, peer notifications. |
| RPC | `internal/rpc` | Internode transport (HTTP/2 + msgpack, streaming), internode JWT authentication. |
| Background | `internal/scanner`, `internal/heal`, `internal/ilm`, `internal/replication` | Data scanning, usage, healing, lifecycle, replication. |
| Events | `internal/event` | Bucket notifications to Webhook, Kafka, NATS, AMQP, Redis, PostgreSQL. |
| Crypto | `internal/crypto`, `internal/kms` | SSE-S3, SSE-C, SSE-KMS; internal or external KMS (Vault/KES). |
| Admin API | `internal/admin` | REST JSON for management (used by the CLI and the dashboard). |
| Console | `internal/console` + `web/` | SPA server, sessions, proxy to the Admin API, SSE/WebSocket for live logs and metrics. |
| Metrics | `internal/metrics` | Prometheus `/nexo/metrics/v1/{cluster,node,bucket}`. |
| Config | `internal/config` | Dynamic config (KV) stored in the `.nexo.sys` system bucket, plus env/flags. |
| CLI | `cmd/nexoctl`, `internal/cli` | S3 + admin client, aliases, table/JSON output. |

## 1.3 Technology stack

### Backend (Go ≥ 1.27)
| Area | Choice | Rationale |
|------|--------|-----------|
| HTTP | `net/http` + lightweight router (`go-chi/chi` v5) | Performance, fine-grained control over streaming. |
| Erasure coding | `github.com/klauspost/reedsolomon` | SIMD (AVX2/AVX512/NEON), de facto standard. |
| Bitrot hash | `github.com/minio/highwayhash` | Very fast, 256 bits. |
| Metadata serialization | `github.com/tinylib/msgp` | Codegen, zero-alloc. |
| Compression | `github.com/klauspost/compress` (S2/zstd) | Optional transparent compression. |
| Config/CLI | `spf13/cobra` + `spf13/pflag` | Standard for Go CLIs. |
| Log | `log/slog` (JSON) | Standard library. |
| Metrics | `prometheus/client_golang` | Standard. |
| Tracing | OpenTelemetry Go SDK | Interoperability. |
| JWT | `golang-jwt/jwt/v5` | Console sessions, internode tokens, STS. |
| Crypto | `crypto/*` + `golang.org/x/crypto` (Argon2id, ChaCha20-Poly1305) | DARE format for SSE. |

### Frontend
| Area | Choice |
|------|--------|
| Framework | React 19 + TypeScript (strict) |
| Build | Vite |
| Routing | React Router v7 (data router) |
| Data fetching | TanStack Query |
| UI | Mantine (components, tables, forms, notifications, light/dark theme) |
| Large tables | TanStack Table + virtualization (`@tanstack/react-virtual`) |
| Charts | Recharts |
| Policy JSON editor | Monaco Editor (lazy-loaded) |
| Upload | client-side multipart with presigned URLs / upload through the console API, parallel chunks, resume |
| i18n | `i18next` (it, en) |
| Test | Vitest, React Testing Library, MSW, Playwright, axe-core |

### Embedding the dashboard in Go

```go
// web/embed.go
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
```

- `make build` runs `npm ci && npm run build` in `web/`, then `go build` with the `-tags embedui` tag.
- Without the tag (backend development) a stub serving a "console not included" page is used.
- The console server:
  - serves the static files with `Cache-Control: public, max-age=31536000, immutable` for the hashed assets;
  - `index.html` with `no-cache` and a CSP header with a nonce;
  - **SPA fallback**: every path that is neither `/api/*` nor an asset → `index.html`;
  - gzip/br compression precomputed at build time (`vite-plugin-compression`).
- In development: `vite dev` on `:5173` with a `/api` → `:9001` proxy.

## 1.4 Repository layout

> The final layout and the **interface + implementation for every service and every route group** convention are in [12 — Service architecture](12-service-architecture.md), which takes precedence over this outline.

```
nexo/
├── cmd/
│   ├── nexo/              # server main
│   ├── nexoctl/           # CLI main
│   └── tools/traceability # development tool: feature → test matrix (not in releases)
├── internal/
│   ├── admin/  auth/  cli/  cluster/  config/  console/  crypto/  dsync/
│   ├── erasure/  event/  heal/  iam/  ilm/  kms/  metrics/  object/
│   ├── policy/  replication/  rpc/  s3api/  scanner/  storage/
│   ├── testutil/          # test helpers, in-process server, fake cluster
│   └── tools/             # logic of the development tools (e.g. traceability)
├── pkg/
│   └── nexoadmin/         # public Go client of the Admin API (used by nexoctl)
├── web/                   # React dashboard (Vite)
│   ├── src/  e2e/  public/
│   └── embed.go
├── test/
│   ├── integration/       # tests with aws-sdk-go-v2 against a real server
│   ├── cluster/           # docker-compose + chaos tests
│   ├── compat/            # s3-tests (Ceph) and mint runners
│   ├── cli/               # testscript scripts (.txtar)
│   └── TRACEABILITY.md    # generated traceability matrix (06 §6.8)
├── deploy/
│   ├── docker/  helm/  systemd/
├── docs/
├── Makefile
└── go.mod                 # module github.com/r3n3r0/nexo
```

## 1.5 On-disk data model

```
/data1/
├── .nexo.sys/
│   ├── format.json            # deployment id, disk id, position in the set, algorithm
│   ├── tmp/                   # writes in progress (cleaned at startup)
│   ├── multipart/             # multipart parts in progress
│   ├── buckets/<bucket>/      # bucket metadata (.metadata.bin: policy, versioning, lifecycle...)
│   ├── config/                # cluster config, IAM (encrypted)
│   └── healing.bin            # healing state of the disk
└── <bucket>/
    └── <object-key>/
        ├── xl.meta            # metadata of all the versions (msgpack)
        └── <data-dir-uuid>/
            └── part.1         # erasure shard + per-block bitrot checksum
```

- **Small objects (< 128 KiB)**: the data is inlined in `xl.meta` to reduce IOPS.
- **xl.meta** holds for each version: version-id, mod-time, size, ETag, content-type, user metadata,
  erasure layout (data/parity, block size, shard index, distribution), checksums, delete-marker flag,
  replication state, ILM transition state, sealed SSE keys.
- **Atomic writes**: data in `.nexo.sys/tmp/<uuid>` → fsync → atomic `RenameData` to the final location
  → `xl.meta` update.

## 1.6 Flow of a PutObject (cluster)

1. Middleware: request-id, SigV4 verification, policy evaluation (`s3:PutObject`).
2. `ServerPools`: if the object already exists in a pool → same pool; otherwise the pool with the most free space (weighted).
3. `ErasureSets`: `set = sipHash(key, deploymentID) % numSets`.
4. Distributed write lock on the object name (quorum `n/2+1` of the set's nodes).
5. Body streaming: 1 MiB blocks → RS encode (D data + P parity) → parallel write to the N disks
   (local or via `storageRESTClient`), with a per-block bitrot hash.
6. Commit: `RenameData` on all the disks; success if `≥ writeQuorum`. Failed disks → **MRF** queue for healing.
7. `s3:ObjectCreated:Put` event notification, audit log, metrics, asynchronous replication if configured.

## 1.7 Architecture decisions (short ADRs)

| ADR | Decision | Rejected alternative | Reason |
|-----|----------|----------------------|--------|
| 001 | No metadata DB: metadata lives next to the data (`xl.meta`) | etcd/Raft for object metadata | Removes a single point of failure and a bottleneck; scales linearly. |
| 002 | Static pool topology, expandable by adding pools | Dynamic consistent hashing with rebalancing | Predictability, no massive rebalancing; see [Cluster](03-cluster.md). |
| 003 | IAM and config stored as encrypted objects in `.nexo.sys` | External DB | Same durability as the data, zero dependencies. |
| 004 | JSON Admin API separate from the S3 API | Proprietary extensions inside S3 | Clean separation, versionable (`/api/v1`). |
| 005 | Dashboard embedded in the binary | Separate container | Deployment as a single artifact, UI version always aligned with the server. |
| 006 | The console authenticates with a session (HttpOnly cookie + CSRF) issued after login with access/secret key or OIDC | Secret key in the browser | The secret never stays in the browser. |
| 007 | The CLI uses SigV4 for the Admin API too | Custom token | Reuse of the credentials and of the signature already implemented. |
