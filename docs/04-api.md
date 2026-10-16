# 4. API

## 4.1 Ports and endpoints

| Port | Path | Use |
|------|------|-----|
| 9000 | `/` | S3 API (path-style; virtual-host if `NEXO_DOMAIN` is set) |
| 9000 | `/nexo/rpc/v1/*` | Internode RPC (authenticated peers only) |
| 9000 | `/nexo/health/{live,ready,cluster}` | Health check |
| 9000 | `/nexo/metrics/v1/{cluster,node,bucket}` | Prometheus (bearer JWT with `admin:Prometheus` by default, public with `NEXO_PROMETHEUS_AUTH_TYPE=public`, see 07 §7.6) |
| 9001 | `/` | React console (SPA) |
| 9001 | `/api/v1/*` | Admin API (SigV4 for CLI/SDK, session cookie for the console) |

The bucket name `nexo` is reserved (like `minio` in MinIO): the `/nexo/*` paths of port 9000 serve RPC,
health and metrics, so `CreateBucket` with that name answers `400 InvalidBucketName`. The system routes
answer only path-style requests: in virtual-host style (`{bucket}.{NEXO_DOMAIN}`) the path is always a key
of the bucket (`bkt.example.com/nexo/metrics/x` is the key `nexo/metrics/x`), authenticated with SigV4 like any
other request.

## 4.2 Supported S3 APIs (v1.0)

**Buckets**: `ListBuckets`, `CreateBucket`, `HeadBucket`, `DeleteBucket`, `GetBucketLocation`,
`Get/Put/DeleteBucketPolicy`, `GetBucketPolicyStatus`, `Get/PutBucketVersioning`, `Get/Put/DeleteBucketTagging`,
`Get/Put/DeleteBucketCors`, `Get/Put/DeleteBucketLifecycleConfiguration`, `Get/PutObjectLockConfiguration`,
`Get/Put/DeleteBucketEncryption`, `Get/PutBucketNotificationConfiguration`, `ListObjects`, `ListObjectsV2`,
`ListObjectVersions`, `ListMultipartUploads`.

**Objects**: `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject`,
`GetObjectAttributes`, `Get/Put/DeleteObjectTagging`, `Get/PutObjectRetention`, `Get/PutObjectLegalHold`,
`PostObject`.

**Multipart**: `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`,
`AbortMultipartUpload`, `ListParts`.

**STS**: `AssumeRole` (P1), `AssumeRoleWithWebIdentity` (P2), `AssumeRoleWithLDAPIdentity` (P2).

Unsupported APIs return `501 NotImplemented` with the correct S3 code.

### Errors
Standard S3 XML format (`<Error><Code/><Message/><Resource/><RequestId/></Error>`), centralized mapping
(`internal/transport/http/httpx`, `errors.go`) with an `ErrorCode → HTTP status` table. The `x-amz-request-id` and `x-amz-id-2` headers are always present.

## 4.3 Admin API (`/api/v1`)

JSON, versioned, documented with **OpenAPI 3.1** (`api/openapi.yaml`), from which are generated:
- the TypeScript types of the dashboard (`openapi-typescript` → `web/src/services/http/schema.d.ts`, `npm run gen:api`),
  from which the DTOs of `web/src/services/types.ts` derive; the HTTP client is `web/src/services/http/client.ts`;
- response validation in the contract tests.

Path parameters (`{u}`, `{g}`, `{p}`, `{k}`, `{id}`, `{subsys}`, `{uploadId}`, `{partNumber}`) are segments
encoded exactly once (RFC 3986 percent-encoding, like `encodeURIComponent` and `url.PathEscape`): a `/` inside
a name travels as `%2F` and stays in its segment, `@`, `+`, `,` may or may not be encoded; the server
decodes them exactly once.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/auth/login` · `/auth/logout` · GET `/auth/session` | Console session |
| GET | `/info` | Server/cluster info (pools with quorum and erasure set state, nodes with version, uptime, clock skew and background healing, disks) |
| GET | `/usage` | Data usage per cluster/bucket |
| GET/POST/DELETE | `/buckets`, `/buckets/{b}` | Bucket management (extends S3 with stats, quota, etc.); a caller who can list only part of the bucket (home directory) sees its name, creation date and region (`partial: true`) |
| GET/PUT | `/buckets/{b}/{versioning,policy,lifecycle,encryption,objectlock,quota,tags,events,cors,replication}` | Bucket settings |
| GET/PUT/DELETE | `/buckets/{b}/lifecycle` · POST `/buckets/{b}/lifecycle/apply` | Lifecycle rules in structured JSON ↔ S3 `LifecycleConfiguration` (the same document as the S3 API), validated rule by rule: `400 invalid_configuration` with `ruleErrors` (index, id, S3 code, message). `apply` runs the rules immediately (`s3:PutLifecycleConfiguration` + `admin:ApplyLifecycle`) |
| GET/PUT/DELETE | `/buckets/{b}/events` · `/buckets/{b}/cors` | Event subscriptions (target ARN, events, prefix/suffix) and CORS rules, validated like the lifecycle |
| GET | `/notify/targets` · POST `/notify/targets/{id}/test` | Notification targets (`notify_*` subsystems) with online/offline state and queued events as seen by the answering node; `s3:TestEvent` test event (`admin:ConfigUpdate`) |
| GET/PUT | `/buckets/{b}/objects/{retention,legal-hold}?key=&versionId=` · GET `/attributes` | Retention (mode, retainUntil, `bypassGovernance` with `s3:BypassGovernanceRetention`), legal hold and attributes (checksums, parts) of a version |
| GET/POST | `/kms/status`, `/kms/keys` · POST `/kms/reseal?bucket=` | KMS: status, keys (creation with `admin:KMSCreateKey`), resealing of the data keys of SSE-S3/SSE-KMS objects with the current master key after a rotation (`admin:KMSCreateKey`, outcome per bucket/versions: `resealed`, `failed`, `firstError`; see [07 §7.2](07-security-operations.md)); `501` without KMS |
| GET | `/buckets/{b}/objects?prefix=&delimiter=&cursor=` | Listing for the object browser |
| POST | `/buckets/{b}/objects/upload` · GET `/download` · POST `/download-zip` · DELETE `/objects` | Object operations from the console |
| POST | `/buckets/{b}/objects/restore?key=&versionId=` | Restores a version (copy of the version onto its key, authorized like the S3 `CopyObject`) |
| POST | `/buckets/{b}/objects/share` · GET/DELETE `/shares`, `/shares/{id}` | Managed, revocable share links |
| GET | `/shares/{id}/links?cursor=&limit=` | One link per object of a download prefix share, signed by the share and revoked with it |
| GET/POST/PUT/DELETE | `/users`, `/users/{u}` | Users (`DELETE` also removes the user's managed shares) |
| GET/POST/PUT/DELETE | `/groups`, `/groups/{g}` | Groups |
| GET/POST/PUT/DELETE | `/policies`, `/policies/{p}`, POST `/policies/attach` `/detach` | Policies |
| GET/POST/PUT/DELETE | `/access-keys`, `/access-keys/{k}` | Service accounts |
| GET/PUT | `/config`, `/config/{subsys}` · POST `/config/import` · GET `/config/export` | Configuration (the `GET /config/schema` schema says for each key whether the server applies it, `applied`: among others `api.root_access`, `logger.*`, `audit_*`, `notify_*`, hot; the others are stored and validated but not yet applied) |
| POST | `/heal` · GET `/heal/{id}` | Healing |
| GET (SSE) | `/trace?node=&type=&api=&bucket=&status=&errors=&threshold=` · `/logs?node=&level=&last=` | Live stream (`text/event-stream`) from all the nodes: `trace`/`log` events, `dropped` when the client loses events, heartbeat; redacted queries, no headers or bodies (`admin:ServerTrace`, `admin:ConsoleLog`; see [07 §7.4](07-security-operations.md)) |
| GET | `/audit?node=&api=&bucket=&user=&type=&status=&errors=&since=&until=&requestId=&limit=` | Search of the recent audit records of all the nodes, newest first (`admin:ConsoleLog`) |
| GET | `/metrics` · `/metrics/query?metric=&range=&step=&end=&node=` | Chart series (in-memory ring buffer: one point every 10 s for 24 h) per node and aggregated over the cluster; `admin:Prometheus` **or** `admin:ServerInfo` |
| GET | `/me/permissions` | Effective permissions (for the conditional UI) |
| POST | `/service/{restart,stop}` | Service control (the restart happens in-process; `SIGHUP` reloads only the TLS certificates) |
| GET | `/profile?type=cpu\|heap\|goroutine\|mutex\|block&seconds=` | pprof profile of the answering node (`admin:Profiling`: root, `diagnostics` policy), `application/octet-stream`, one at a time per node (409) |
| POST | `/speedtest/{drive,object,net}` | Speed test of the answering node (`admin:Speedtest`): local disks, object layer (temporary `nexo-speedtest-*` bucket, removed afterwards), network to the other nodes (`501` outside a cluster); phases of at most 60 s, files/objects ≤ 64 MiB, ≤ 1 GiB written per target |
| POST/GET | `/pools/{id}/decommission`, `/rebalance` | Pool management (P2) |

Errors in `application/problem+json` format (RFC 9457) with a stable `code`.

Lists (`/users`, `/groups`, `/policies`, `/access-keys`, `/shares`, `/buckets`, `/shares/{id}/links`)
answer `{items, nextCursor}`: the service selects the page (`limit` 1–1000, default 1000) and the cursor is
opaque, to be passed back as is in `cursor`.

The object operations proxied by the console (download, preview, stat, tags, upload, delete,
rename, zip, restore, share) are evaluated with the same condition keys as the equivalent S3 request
(`s3:versionid`, `s3:x-amz-*`, `s3:RequestObjectTag/*`, `s3:authType`…), built from a single
definition (`policy.RequestConditions`): a policy decides the same way via S3 and via the console. The client for
`aws:SourceIp` and `aws:SecureTransport` is, on both listeners, the one forwarded by the trusted proxies
(`--trusted-proxies`, `X-Forwarded-For`/`X-Forwarded-Proto`), otherwise the connection peer.

Console operations on objects protected by Object Lock behave like the equivalent S3 requests:
deleting a version under retention or legal hold fails (`object_locked`, even for root in COMPLIANCE mode),
`bypassGovernance` applies only where the caller has `s3:BypassGovernanceRetention`; rename and restore create new
versions and leave the protected ones intact.

Object operations by prefix (delete, rename and zip) treat every prefix as a folder,
like the CLI: a non-empty prefix without a trailing `/` denotes the folder with that name (`photos` is `photos/`, never
`photos2/` or `photosx.txt`); `""` is the whole bucket.

Endpoints for access rules, distributions, key groups and certificates: see [10 §10.5](10-domains-access-distributions.md#105-admin-api).

## 4.4 Admin API authorization

Each endpoint maps to an `admin:*` policy action (e.g. `admin:CreateUser`, `admin:ServerInfo`,
`admin:ConfigUpdate`, `admin:Heal`, `admin:ServerTrace`), evaluated by the same policy engine as S3.
The built-in `consoleAdmin` policy grants `admin:*` + `s3:*`.
