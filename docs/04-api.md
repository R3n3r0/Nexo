# 4. API

## 4.1 Porte ed endpoint

| Porta | Path | Uso |
|-------|------|-----|
| 9000 | `/` | API S3 (path-style; virtual-host se `NEXO_DOMAIN` impostato) |
| 9000 | `/nexo/rpc/v1/*` | Internode RPC (solo peer autenticati) |
| 9000 | `/nexo/health/{live,ready,cluster}` | Health check |
| 9000 | `/nexo/metrics/v1/{cluster,node,bucket}` | Prometheus (auth bearer o pubblico se configurato) |
| 9001 | `/` | Console React (SPA) |
| 9001 | `/api/v1/*` | Admin API (SigV4 per CLI/SDK, cookie di sessione per la console) |

## 4.2 API S3 supportate (v1.0)

**Bucket**: `ListBuckets`, `CreateBucket`, `HeadBucket`, `DeleteBucket`, `GetBucketLocation`,
`Get/Put/DeleteBucketPolicy`, `GetBucketPolicyStatus`, `Get/PutBucketVersioning`, `Get/Put/DeleteBucketTagging`,
`Get/Put/DeleteBucketCors`, `Get/Put/DeleteBucketLifecycleConfiguration`, `Get/PutObjectLockConfiguration`,
`Get/Put/DeleteBucketEncryption`, `Get/PutBucketNotificationConfiguration`, `ListObjects`, `ListObjectsV2`,
`ListObjectVersions`, `ListMultipartUploads`.

**Oggetti**: `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject`,
`Get/Put/DeleteObjectTagging`, `Get/PutObjectRetention`, `Get/PutObjectLegalHold`, `PostObject`.

**Multipart**: `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`,
`AbortMultipartUpload`, `ListParts`.

**STS**: `AssumeRole` (P1), `AssumeRoleWithWebIdentity` (P2), `AssumeRoleWithLDAPIdentity` (P2).

Le API non supportate restituiscono `501 NotImplemented` con codice S3 corretto.

### Errori
Formato XML S3 standard (`<Error><Code/><Message/><Resource/><RequestId/></Error>`), mapping centralizzato
(`internal/s3api/errors.go`) con tabella `ErrorCode → HTTP status`. Header `x-amz-request-id` e `x-amz-id-2` sempre presenti.

## 4.3 Admin API (`/api/v1`)

JSON, versionata, documentata con **OpenAPI 3.1** (`api/openapi.yaml`) da cui si generano:
- client TypeScript per la dashboard (`openapi-typescript` + `openapi-fetch`);
- validazione delle risposte nei test di contratto.

| Metodo | Path | Descrizione |
|--------|------|-------------|
| POST | `/auth/login` · `/auth/logout` · GET `/auth/session` | Sessione console |
| GET | `/info` | Info server/cluster (pool, nodi, dischi, versioni, uptime) |
| GET | `/usage` | Uso dati per cluster/bucket |
| GET/POST/DELETE | `/buckets`, `/buckets/{b}` | Gestione bucket (estende S3 con stat, quota, ecc.) |
| GET/PUT | `/buckets/{b}/{versioning,policy,lifecycle,encryption,objectlock,quota,tags,events,replication}` | Impostazioni bucket |
| GET | `/buckets/{b}/objects?prefix=&delimiter=&cursor=` | Listing per object browser |
| POST | `/buckets/{b}/objects/upload` · GET `/download` · POST `/download-zip` · DELETE `/objects` | Operazioni oggetti dalla console |
| POST | `/buckets/{b}/objects/share` | Genera presigned URL |
| GET/POST/PUT/DELETE | `/users`, `/users/{u}` | Utenti |
| GET/POST/PUT/DELETE | `/groups`, `/groups/{g}` | Gruppi |
| GET/POST/PUT/DELETE | `/policies`, `/policies/{p}`, POST `/policies/attach` `/detach` | Policy |
| GET/POST/PUT/DELETE | `/access-keys`, `/access-keys/{k}` | Service account |
| GET/PUT | `/config`, `/config/{subsys}` · POST `/config/import` · GET `/config/export` | Configurazione |
| POST | `/heal` · GET `/heal/{id}` | Healing |
| GET (SSE) | `/trace`, `/logs` | Stream live |
| GET | `/metrics/query?metric=&range=` | Serie storiche per grafici (ring buffer in memoria, opzionale Prometheus remoto) |
| GET | `/me/permissions` | Permessi effettivi (per UI condizionale) |
| POST | `/service/{restart,stop}` | Controllo servizio |
| POST/GET | `/pools/{id}/decommission`, `/rebalance` | Gestione pool (P2) |

Errori in formato `application/problem+json` (RFC 9457) con `code` stabile.

Endpoint per regole di accesso, distribuzioni, key group e certificati: vedi [10 §10.5](10-domini-accessi-distribuzioni.md#105-admin-api).

## 4.4 Autorizzazione Admin API

Ogni endpoint mappa su un'azione di policy `admin:*` (es. `admin:CreateUser`, `admin:ServerInfo`,
`admin:ConfigUpdate`, `admin:Heal`, `admin:ServerTrace`), valutata dallo stesso policy engine dell'S3.
La policy predefinita `consoleAdmin` concede `admin:*` + `s3:*`.
