# Riferimento Admin API

L'Admin API è l'API REST con cui la console, `nexoctl` e le tue automazioni amministrano Nexo. È servita sul
listener della console (porta 9001) sotto `/api/v1`, scambia JSON ed è descritta da un documento OpenAPI 3.1:

- **specifica**: [`api/openapi.yaml`](https://github.com/R3n3r0/Nexo/blob/main/api/openapi.yaml), la
  fonte da cui sono generati i tipi TypeScript della console;
- **client Go**: [`pkg/nexoadmin`](https://github.com/R3n3r0/Nexo/tree/main/pkg/nexoadmin), lo stesso usato
  da `nexoctl`;
- **riferimento navigabile**: nel sito della documentazione (`make docs-site`) la specifica è pubblicata come
  pagina statica: <a href="redoc.html">apri il riferimento interattivo dell'Admin API</a>.

## Riferimento HTML in locale

La pagina è generata da [Redocly](https://redocly.com/docs/cli/) e contiene già tutte le operazioni e gli
schemi, consultabili anche senza rete:

```bash
make docs-api                     # bin/docs/admin-api.html (richiede Node.js: npx @redocly/cli)
xdg-open bin/docs/admin-api.html  # oppure open su macOS
```

In alternativa si può aprire `api/openapi.yaml` con qualsiasi visualizzatore OpenAPI (Swagger UI, l'estensione
OpenAPI del proprio editor) o importarlo in un client HTTP come Postman o Bruno.

## Autenticazione

Ogni richiesta usa una di due forme.

**Firma AWS Signature Version 4** (script, SDK, CLI): servizio `s3`, regione del server, header
`Authorization`. Gli URL presigned non sono accettati. Con curl:

```console
$ curl -s --aws-sigv4 "aws:amz:us-east-1:s3" --user "$ACCESS_KEY:$SECRET_KEY" \
    "https://console.example.com/api/v1/users?limit=2"
{"items":[{"accessKey":"alice", ...}],"nextCursor":"..."}
```

**Sessione della console** (browser): `POST /api/v1/auth/login` con `{"accessKey": ..., "secretKey": ...}`
imposta il cookie `nexo_session` (`HttpOnly`, `SameSite=Strict`) e restituisce un `csrfToken`. Ogni
richiesta che modifica qualcosa deve portare l'header `X-CSRF-Token` e arrivare dall'origine della console:

```bash
curl -s -c jar -H 'Content-Type: application/json' \
  -d '{"accessKey":"admin","secretKey":"supersegreta"}' https://console.example.com/api/v1/auth/login
# {"id":"...","accessKey":"admin","identity":{...},"csrfToken":"...","expiresAt":"..."}
curl -s -b jar -X POST -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" -H 'Origin: https://console.example.com' \
  -d '{"name":"report"}' https://console.example.com/api/v1/buckets
```

Senza credenziali la risposta è `401 unauthenticated`; senza il token CSRF `403 invalid_csrf_token`.

## Autorizzazione

Ogni operazione richiede delle azioni di policy, elencate nella specifica nel campo `x-nexo-actions`: azioni
`admin:*` (per esempio `admin:CreateUser`, `admin:ManageDistributions`, `admin:InvalidateCache`) e azioni
`s3:*` per le operazioni su bucket e oggetti. Sono valutate dallo stesso motore delle policy IAM, quindi un
utente può ricevere solo la parte di amministrazione che gli serve. Alcune operazioni sono self-service: ogni
utente gestisce le proprie access key e i propri link di condivisione. `GET /api/v1/me/permissions` dice che
cosa può fare l'utente corrente (la console lo usa per mostrare solo le azioni consentite).

## Errori

Gli errori sono documenti RFC 9457 `application/problem+json` con un `code` stabile in snake_case:

```json
{
  "type": "about:blank",
  "title": "Not Found",
  "status": 404,
  "detail": "bucket not found: report",
  "instance": "/api/v1/buckets/report",
  "code": "bucket_not_found",
  "requestId": "DFEBCBB5A41BB795"
}
```

Codici frequenti: `unauthenticated`, `access_denied`, `invalid_csrf_token`, `invalid_argument`,
`bucket_not_found`, `object_not_found`, `user_not_found`, `policy_not_found`, `conflict`,
`malformed_policy` (con `errors` posizionati), `invalid_configuration` (con `ruleErrors`),
`invalid_access_rule`, `public_access_blocked`, `object_locked`, `entity_too_large`, `not_implemented`. Il
`requestId` compare anche nei log e nell'audit del server.

## Paginazione e concorrenza

- Le liste rispondono `{"items": [...], "nextCursor": "..."}` e accettano `limit` (1–1000, default 1000) e
  `cursor` (il `nextCursor` della pagina precedente, opaco).
- Le risorse sostituite per intero (distribuzioni, regole di accesso, policy di cache) restituiscono un
  `ETag`: un `PUT` con `If-Match` fallisce con `412 precondition_failed` se nel frattempo un altro client le
  ha cambiate.

## Gruppi di operazioni

| Tag | Percorsi principali | Per |
|-----|---------------------|-----|
| session | `/auth/login`, `/auth/logout`, `/auth/session`, `/me/permissions` | sessione della console |
| server | `/info`, `/usage`, `/heal`, `/service/restart`, `/service/stop` | stato, utilizzo, healing, servizio |
| monitoring | `/metrics`, `/metrics/query`, `/trace`, `/logs`, `/audit` | metriche, trace e log in streaming (SSE), audit |
| diagnostics | `/profile`, `/speedtest/{kind}` | profili pprof, test di velocità |
| buckets | `/buckets`, `/buckets/{bucket}/{versioning,policy,quota,tags,objectlock,encryption,access,lifecycle,events,cors}` | bucket e loro impostazioni |
| replication | `/buckets/{bucket}/replication`, `.../replication/{targets,status,resync}` | regole di replica del bucket, destinazioni remote, stato e risincronizzazione |
| site-replication | `/site-replication`, `.../sync`, `.../peer/...` (tra i siti, solo root) | gruppo di replica di sito e sua sincronizzazione |
| objects | `/buckets/{bucket}/objects`, `.../stat`, `.../versions`, `.../download`, `.../download-zip`, `.../retention`, `.../legal-hold` | navigazione e gestione degli oggetti |
| uploads | `/buckets/{bucket}/objects/upload`, `/buckets/{bucket}/uploads/...` | upload semplici e multipart dalla console |
| shares | `/shares`, `/buckets/{bucket}/objects/share`, `/presign`, `/post-policy` | link di condivisione, presigned, POST policy |
| users, groups, access-keys, policies | `/users`, `/groups`, `/access-keys`, `/policies`, `/policies/simulate` | identità e permessi |
| config | `/config`, `/config/{subsys}`, `/config/schema`, `/config/export`, `/config/import` | configurazione dinamica |
| cluster | `/cluster/export`, `/cluster/import`, `/locks` | migrazione dei metadati, lock distribuiti |
| notify, kms | `/notify/targets`, `/kms/status`, `/kms/keys`, `/kms/reseal` | target degli eventi, chiavi KMS |
| access-rules | `/buckets/{bucket}/access-rules`, `.../test`, `.../public`, `/access/block-public` | regole di accesso, block public access |
| distributions | `/distributions`, `/distributions/{id}/{test,sign,origin-secret/rotate}`, `/keygroups`, `/routing/resolve` | distribuzioni, key group, instradamento |
| certificates | `/certificates`, `/dns-providers` | certificati delle distribuzioni, provider DNS della challenge ACME DNS-01 |
| cache | `/cache/{policies,settings,inspect,stats,invalidations}`, `/distributions/{id}/cache/...`, `/distributions/{id}/invalidations` | cache, invalidazioni, warm-up, webhook |

La specifica completa conta 180 operazioni; il riferimento HTML le mostra con parametri, corpi e
risposte d'esempio.

## Esempi

```bash
SIG=(--aws-sigv4 "aws:amz:us-east-1:s3" --user "$ACCESS_KEY:$SECRET_KEY")
API=https://console.example.com/api/v1

curl -s "${SIG[@]}" "$API/info"                                      # stato del deployment
curl -s "${SIG[@]}" "$API/buckets/foto/objects?prefix=2026/&limit=100"   # oggetti di un prefisso
curl -s "${SIG[@]}" -X POST -H 'Content-Type: application/json' \
  -d '{"paths":["/index.html"],"soft":true}' "$API/distributions/sito/invalidations"   # invalidazione della cache
```

Per un client tipizzato in Go usa `pkg/nexoadmin`; per altri linguaggi genera un client dalla specifica (per
esempio con `openapi-generator`).
