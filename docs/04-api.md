# 4. API

## 4.1 Porte ed endpoint

| Porta | Path | Uso |
|-------|------|-----|
| 9000 | `/` | API S3 (path-style; virtual-host se `NEXO_DOMAIN` impostato) |
| 9000 | `/nexo/rpc/v1/*` | Internode RPC (solo peer autenticati) |
| 9000 | `/nexo/health/{live,ready,cluster}` | Health check |
| 9000 | `/nexo/metrics/v1/{cluster,node,bucket}` | Prometheus (bearer JWT con `admin:Prometheus` di default, pubblico con `NEXO_PROMETHEUS_AUTH_TYPE=public`, vedi 07 §7.6) |
| 9001 | `/` | Console React (SPA) |
| 9001 | `/api/v1/*` | Admin API (SigV4 per CLI/SDK, cookie di sessione per la console) |

Il nome di bucket `nexo` è riservato (come `minio` in MinIO): i path `/nexo/*` della porta 9000 servono RPC,
health e metriche, quindi `CreateBucket` con quel nome risponde `400 InvalidBucketName`. Le rotte di sistema
rispondono solo alle richieste path-style: in virtual-host (`{bucket}.{NEXO_DOMAIN}`) il path è sempre una chiave
del bucket (`bkt.example.com/nexo/metrics/x` è la chiave `nexo/metrics/x`), autenticata con SigV4 come ogni
altra richiesta.

## 4.2 API S3 supportate (v1.0)

**Bucket**: `ListBuckets`, `CreateBucket`, `HeadBucket`, `DeleteBucket`, `GetBucketLocation`,
`Get/Put/DeleteBucketPolicy`, `GetBucketPolicyStatus`, `Get/PutBucketVersioning`, `Get/Put/DeleteBucketTagging`,
`Get/Put/DeleteBucketCors`, `Get/Put/DeleteBucketLifecycleConfiguration`, `Get/PutObjectLockConfiguration`,
`Get/Put/DeleteBucketEncryption`, `Get/PutBucketNotificationConfiguration`, `ListObjects`, `ListObjectsV2`,
`ListObjectVersions`, `ListMultipartUploads`.

**Oggetti**: `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`, `DeleteObjects`, `CopyObject`,
`GetObjectAttributes`, `Get/Put/DeleteObjectTagging`, `Get/PutObjectRetention`, `Get/PutObjectLegalHold`,
`PostObject`.

**Multipart**: `CreateMultipartUpload`, `UploadPart`, `UploadPartCopy`, `CompleteMultipartUpload`,
`AbortMultipartUpload`, `ListParts`.

**STS**: `AssumeRole` (P1), `AssumeRoleWithWebIdentity` (P2), `AssumeRoleWithLDAPIdentity` (P2).

Le API non supportate restituiscono `501 NotImplemented` con codice S3 corretto.

### Errori
Formato XML S3 standard (`<Error><Code/><Message/><Resource/><RequestId/></Error>`), mapping centralizzato
(`internal/transport/http/httpx`, `errors.go`) con tabella `ErrorCode → HTTP status`. Header `x-amz-request-id` e `x-amz-id-2` sempre presenti.

## 4.3 Admin API (`/api/v1`)

JSON, versionata, documentata con **OpenAPI 3.1** (`api/openapi.yaml`) da cui si generano:
- i tipi TypeScript della dashboard (`openapi-typescript` → `web/src/services/http/schema.d.ts`, `npm run gen:api`),
  da cui derivano i DTO di `web/src/services/types.ts`; il client HTTP è `web/src/services/http/client.ts`;
- validazione delle risposte nei test di contratto.

I parametri di path (`{u}`, `{g}`, `{p}`, `{k}`, `{id}`, `{subsys}`, `{uploadId}`, `{partNumber}`) sono segmenti
codificati una sola volta (percent-encoding RFC 3986, come `encodeURIComponent` e `url.PathEscape`): un `/` dentro
un nome viaggia come `%2F` e resta nel suo segmento, `@`, `+`, `,` possono essere codificati o no; il server li
decodifica una volta sola.

| Metodo | Path | Descrizione |
|--------|------|-------------|
| POST | `/auth/login` · `/auth/logout` · GET `/auth/session` | Sessione console |
| GET | `/info` | Info server/cluster (pool con quorum e stato degli erasure set, nodi con versione, uptime, sfasamento dell'orologio e riparazione in background, dischi) |
| GET | `/usage` | Uso dati per cluster/bucket |
| GET/POST/DELETE | `/buckets`, `/buckets/{b}` | Gestione bucket (estende S3 con stat, quota, ecc.); chi può elencare solo una parte del bucket (home directory) ne vede nome, data di creazione e regione (`partial: true`) |
| GET/PUT | `/buckets/{b}/{versioning,policy,lifecycle,encryption,objectlock,quota,tags,events,cors,replication}` | Impostazioni bucket |
| GET/PUT/DELETE | `/buckets/{b}/lifecycle` · POST `/buckets/{b}/lifecycle/apply` | Regole lifecycle in JSON strutturato ↔ `LifecycleConfiguration` S3 (lo stesso documento dell'API S3), validate regola per regola: `400 invalid_configuration` con `ruleErrors` (indice, id, codice S3, messaggio). `apply` esegue subito le regole (`s3:PutLifecycleConfiguration` + `admin:ApplyLifecycle`) |
| GET/PUT/DELETE | `/buckets/{b}/events` · `/buckets/{b}/cors` | Sottoscrizioni eventi (ARN del target, eventi, prefix/suffix) e regole CORS, validate come il lifecycle |
| GET | `/notify/targets` · POST `/notify/targets/{id}/test` | Target di notifica (sottosistemi `notify_*`) con stato online/offline ed eventi in coda visti dal nodo che risponde; evento di prova `s3:TestEvent` (`admin:ConfigUpdate`) |
| GET/PUT | `/buckets/{b}/objects/{retention,legal-hold}?key=&versionId=` · GET `/attributes` | Retention (mode, retainUntil, `bypassGovernance` con `s3:BypassGovernanceRetention`), legal hold e attributi (checksum, parti) di una versione |
| GET/POST | `/kms/status`, `/kms/keys` · POST `/kms/reseal?bucket=` | KMS: stato, chiavi (creazione con `admin:KMSCreateKey`), ri-sigillatura delle chiavi dati degli oggetti SSE-S3/SSE-KMS con la master key corrente dopo una rotazione (`admin:KMSCreateKey`, esito per bucket/versioni: `resealed`, `failed`, `firstError`; vedi [07 §7.2](07-sicurezza-operativita.md)); `501` senza KMS |
| GET | `/buckets/{b}/objects?prefix=&delimiter=&cursor=` | Listing per object browser |
| POST | `/buckets/{b}/objects/upload` · GET `/download` · POST `/download-zip` · DELETE `/objects` | Operazioni oggetti dalla console |
| POST | `/buckets/{b}/objects/restore?key=&versionId=` | Ripristina una versione (copia della versione sulla sua chiave, autorizzata come la `CopyObject` S3) |
| POST | `/buckets/{b}/objects/share` · GET/DELETE `/shares`, `/shares/{id}` | Share link gestiti e revocabili |
| GET | `/shares/{id}/links?cursor=&limit=` | Un link per oggetto di uno share di prefix in download, firmati dallo share e revocati con esso |
| GET/POST/PUT/DELETE | `/users`, `/users/{u}` | Utenti (`DELETE` rimuove anche gli share gestiti dell'utente) |
| GET/POST/PUT/DELETE | `/groups`, `/groups/{g}` | Gruppi |
| GET/POST/PUT/DELETE | `/policies`, `/policies/{p}`, POST `/policies/attach` `/detach` | Policy |
| GET/POST/PUT/DELETE | `/access-keys`, `/access-keys/{k}` | Service account |
| GET/PUT | `/config`, `/config/{subsys}` · POST `/config/import` · GET `/config/export` | Configurazione (lo schema `GET /config/schema` dice per ogni chiave se il server la applica, `applied`: oggi `api.root_access`, a caldo; le altre sono salvate e validate ma non ancora applicate) |
| POST | `/heal` · GET `/heal/{id}` | Healing |
| GET (SSE) | `/trace`, `/logs` | Stream live |
| GET | `/metrics/query?metric=&range=` | Serie storiche per grafici (ring buffer in memoria, opzionale Prometheus remoto) |
| GET | `/me/permissions` | Permessi effettivi (per UI condizionale) |
| POST | `/service/{restart,stop}` | Controllo servizio |
| POST/GET | `/pools/{id}/decommission`, `/rebalance` | Gestione pool (P2) |

Errori in formato `application/problem+json` (RFC 9457) con `code` stabile.

Le liste (`/users`, `/groups`, `/policies`, `/access-keys`, `/shares`, `/buckets`, `/shares/{id}/links`)
rispondono `{items, nextCursor}`: il servizio seleziona la pagina (`limit` 1–1000, default 1000) e il cursore è
opaco, da ripassare tal quale in `cursor`.

Le operazioni sugli oggetti proxate dalla console (download, anteprima, stat, tag, upload, eliminazione,
rinomina, zip, ripristino, share) sono valutate con le stesse chiavi di condizione della richiesta S3
equivalente (`s3:versionid`, `s3:x-amz-*`, `s3:RequestObjectTag/*`, `s3:authType`…), costruite da un'unica
definizione (`policy.RequestConditions`): una policy decide allo stesso modo via S3 e via console. Il client di
`aws:SourceIp` e `aws:SecureTransport` è, su entrambi i listener, quello inoltrato dai proxy fidati
(`--trusted-proxies`, `X-Forwarded-For`/`X-Forwarded-Proto`), altrimenti il peer della connessione.

Le operazioni della console su oggetti protetti da Object Lock si comportano come le richieste S3 equivalenti:
l'eliminazione di una versione sotto retention o legal hold fallisce (`object_locked`, anche per root in COMPLIANCE),
`bypassGovernance` vale solo dove il chiamante ha `s3:BypassGovernanceRetention`; rinomina e ripristino creano nuove
versioni e lasciano intatte quelle protette.

Le operazioni sugli oggetti per prefisso (eliminazione, rinomina e zip) trattano ogni prefisso come una cartella,
come la CLI: un prefisso non vuoto senza `/` finale indica la cartella omonima (`photos` è `photos/`, mai
`photos2/` né `photosx.txt`); `""` è l'intero bucket.

Endpoint per regole di accesso, distribuzioni, key group e certificati: vedi [10 §10.5](10-domini-accessi-distribuzioni.md#105-admin-api).

## 4.4 Autorizzazione Admin API

Ogni endpoint mappa su un'azione di policy `admin:*` (es. `admin:CreateUser`, `admin:ServerInfo`,
`admin:ConfigUpdate`, `admin:Heal`, `admin:ServerTrace`), valutata dallo stesso policy engine dell'S3.
La policy predefinita `consoleAdmin` concede `admin:*` + `s3:*`.
