# 11. Gestione della cache per dominio

Estende le distribuzioni ([10](10-domini-accessi-distribuzioni.md)) con una **cache HTTP integrata** e una gestione
completa della cache **per distribuzione, per dominio e per percorso**, configurabile da dashboard, CLI e Admin API.
Funziona in singolo nodo e in cluster.

Vantaggio rispetto a una CDN esterna: Nexo è anche l'origine, quindi **sa quando un oggetto cambia** e può
invalidare la cache automaticamente e in modo esatto.

## 11.1 Livelli di configurazione

Le impostazioni si ereditano dal livello più generale al più specifico; ogni livello può sovrascrivere il precedente:

```
Globale (cluster) ─▶ Distribuzione ─▶ Dominio (alias) ─▶ Behavior (percorso)
```

Esempio: la distribuzione `sito-web` ha `www.example.com` con cache lunga e `preview.example.com` con cache disattivata,
pur servendo lo stesso bucket.

```yaml
id: sito-web
cache:                              # default della distribuzione
  enabled: true
  policy: standard
domains:
  - host: www.example.com
    cache: { policy: aggressive, quota: 20GiB }
  - host: "*.preview.example.com"
    cache: { enabled: false }       # anteprime sempre fresche
  - host: static.example.it
    cache: { policy: standard, quota: 5GiB, purgeWebhooks: [cloudflare-it] }
behaviors:
  - pathPattern: { type: glob, pattern: "/assets/**" }
    cache: { ttl: { default: 365d, min: 1d, max: 365d }, browserCacheControl: "public, max-age=31536000, immutable" }
  - pathPattern: { type: regex, pattern: '^/api/' }
    cache: { enabled: false }
```

## 11.2 Policy di cache

Una **cache policy** è un oggetto riutilizzabile (come in CloudFront) referenziato per nome. Policy predefinite:
`disabled`, `standard`, `aggressive`, `immutable-assets`, `respect-origin`.

| Campo | Descrizione | Default (`standard`) |
|-------|-------------|----------------------|
| `ttl.default` / `min` / `max` | TTL in cache se l'oggetto non specifica `Cache-Control`; limiti applicati comunque | 1h / 0 / 7d |
| `respectObjectCacheControl` | Usa il `Cache-Control`/`Expires` salvato nei metadati dell'oggetto | true |
| `browserCacheControl` | `Cache-Control` inviato al client (può differire dal TTL interno) | eredita dall'oggetto |
| `cacheKey.queryString` | `none` / `all` / `allowlist: [v, w]` / `denylist` | `none` |
| `cacheKey.headers` | Header inclusi nella chiave (es. `Accept-Language`) | — |
| `cacheKey.cookies` | Cookie inclusi nella chiave | — |
| `cacheKey.normalize` | Ordinamento query, minuscole, rimozione parametri di tracking (`utm_*`, `fbclid`) | true |
| `cacheKey.includeHost` | Chiave distinta per dominio, oppure condivisa tra gli alias della distribuzione | true |
| `staleWhileRevalidate` | Serve la copia scaduta mentre la aggiorna in background | 60s |
| `staleIfError` | Serve la copia scaduta se l'origine (es. set senza quorum) fallisce | 1d |
| `negativeCaching` | TTL per 404/403 | 10s |
| `bypass` | Condizioni che saltano la cache: header `Authorization`, cookie, query `?nocache`, metodi ≠ GET/HEAD | Authorization presente |
| `compression` | Varianti gzip/br/zstd memorizzate separatamente (`Vary: Accept-Encoding`) | gzip + br per tipi testuali |
| `minSize` / `maxObjectSize` | Oggetti fuori range non vengono messi in cache | 0 / 1 GiB |
| `rangeCaching` | Caching a blocchi (es. 8 MiB) per richieste Range e video | true, blocchi 8 MiB |
| `contentTypes` | Allowlist/denylist per content-type | tutti |

Regole di sicurezza: le risposte a richieste **autenticate** o **firmate** non vengono mai condivise tra utenti diversi;
per i contenuti `signed` la firma viene verificata **prima** del lookup in cache e non entra nella chiave.

## 11.3 Archiviazione della cache

| Livello | Tecnologia | Uso |
|---------|-----------|-----|
| L1 memoria | Cache concorrente con eviction W-TinyLFU (libreria tipo `otter`/`ristretto`) | Oggetti piccoli e molto richiesti |
| L2 disco | Directory dedicata (`--cache-dir`, consigliato SSD/NVMe separato dai dischi dati), file con header + body, indice in memoria | Oggetti grandi, blocchi Range |

- **Quote**: globale (`--cache-size`), per distribuzione e **per dominio**; eviction LRU/LFU all'interno della quota.
- **Request collapsing**: richieste simultanee per lo stesso oggetto non in cache producono una sola lettura dall'origine.
- La cache è **opzionale**: senza `--cache-dir` resta attivo solo L1 con limite di memoria.
- In Docker: volume separato `/cache` nei file Compose.

## 11.4 Cache in cluster

| Modalità | Descrizione | Quando usarla |
|----------|-------------|---------------|
| `local` (default) | Ogni nodo ha la sua cache | Load balancer con sticky o pochi nodi |
| `shared` | La chiave di cache viene assegnata a un nodo "proprietario" con **consistent hashing** (rendezvous) sui nodi del cluster: il nodo che riceve la richiesta chiede al proprietario via RPC. Una sola copia per oggetto nel cluster, cache totale = somma delle cache | Molti nodi dietro load balancer round-robin |
| `shared+local` | Come `shared` più un piccolo L1 locale per gli oggetti più caldi | Traffico molto concentrato su pochi oggetti |

- Nodo proprietario offline → si legge direttamente dall'origine (nessun errore), l'anello si ricalcola.
- Le **invalidazioni** vengono propagate a tutti i nodi via peer RPC con un numero di sequenza; un nodo che rientra
  dopo un'assenza svuota la sua cache se ha perso invalidazioni (sequenza non contigua).

## 11.5 Invalidazione

| Tipo | Come |
|------|------|
| **Automatica** | Su PUT/DELETE/CopyObject/CompleteMultipart/cambio metadati/tag/versione corrente di un oggetto, Nexo invalida tutte le voci di cache che lo riguardano su **tutti i domini** che servono quel bucket/prefix. Configurabile (`autoInvalidate: true`) |
| Per percorso | Path esatti o glob/regex (`/assets/**`), limitata a un dominio o a tutti i domini della distribuzione |
| Per tag (surrogate key) | Gli oggetti possono avere `Cache-Tag` (da metadato `x-amz-meta-cache-tag` o da tag S3); purge per tag, es. `prodotto-42` |
| Per dominio | Svuota tutta la cache di un dominio |
| Per distribuzione / globale | Svuota tutto |
| Soft purge | Marca come scaduto (si continua a servire con `stale-while-revalidate`) invece di cancellare |

- Ogni invalidazione ha un ID, uno stato (`in corso` / `completata` su N nodi) e compare nella cronologia.
- **Propagazione esterna**: per ogni dominio si possono configurare **webhook di purge** verso CDN/proxy davanti a Nexo
  (Cloudflare, Fastly, Akamai, nginx `proxy_cache_purge`, Varnish `BAN`, webhook generico con template), invocati sia sulle
  invalidazioni manuali sia su quelle automatiche, con retry e coda persistente.

## 11.6 Preriscaldamento (warm-up)

- `prefetch` di un elenco di path o di un prefix/glob per un dominio (es. dopo un deploy di un sito statico).
- Opzione `warmOnWrite`: gli oggetti scritti che corrispondono a un pattern vengono messi subito in cache.
- Limite di banda/concorrenza per non impattare il traffico.

## 11.7 Header e diagnostica

- Header di risposta: `X-Cache: HIT | MISS | STALE | BYPASS | EXPIRED | REVALIDATED`, `Age`, `X-Cache-Node` (opzionale),
  `X-Cache-Key` (solo con header di debug autorizzato).
- Supporto completo a richieste condizionali verso il client (`ETag`, `Last-Modified`, `304`).
- Endpoint di ispezione: dato un URL, mostra chiave di cache calcolata, policy applicata (con il livello da cui deriva),
  presenza in cache su ciascun nodo, età, scadenza.

## 11.8 Metriche per dominio

`nexo_cache_requests_total{distribution,host,result}`, `nexo_cache_hit_ratio`, `nexo_cache_bytes_served_total{source=cache|origin}`,
`nexo_cache_size_bytes{host,tier}`, `nexo_cache_evictions_total`, `nexo_cache_invalidations_total`, latenza HIT vs MISS,
banda di origine risparmiata. Grafici dedicati in dashboard per ogni dominio e "top oggetti" (più richiesti, più MISS).

## 11.9 Dashboard

| Codice | Funzionalità |
|--------|--------------|
| D-CCH-01 | Pagina **Cache** globale: dimensione usata per livello (memoria/disco), hit ratio, banda risparmiata, top domini |
| D-CCH-02 | Distribuzione → tab **Cache**: tabella dei domini con policy, quota, uso, hit ratio, abilitato/disabilitato, override per dominio |
| D-CCH-03 | Editor **cache policy** riutilizzabili (TTL, chiave, bypass, stale, compressione) con anteprima dell'header `Cache-Control` risultante |
| D-CCH-04 | **Invalidazione**: per path/glob/regex, per tag, per dominio, soft/hard; cronologia con stato per nodo |
| D-CCH-05 | **Ispettore URL**: chiave, policy effettiva e livello di provenienza, stato in cache su ogni nodo |
| D-CCH-06 | Preriscaldamento: incolla elenco URL o scegli prefix, avanzamento |
| D-CCH-07 | Webhook di purge esterni per dominio con test di connessione |
| D-CCH-08 | Grafici per dominio: HIT/MISS/STALE, byte da cache vs origine, top oggetti |

## 11.10 CLI

```bash
nexoctl cache policy create|ls|info|edit|rm local aggressive --ttl-default 1d --ttl-max 30d --query allowlist:v --stale-while-revalidate 5m
nexoctl dist cache set local sito-web --policy standard
nexoctl dist cache set local sito-web --domain www.example.com --policy aggressive --quota 20GiB
nexoctl dist cache set local sito-web --domain '*.preview.example.com' --disable
nexoctl dist cache set local sito-web --behavior '/assets/**' --ttl-default 365d
nexoctl cache purge local sito-web --domain www.example.com '/assets/**' [--soft]
nexoctl cache purge local sito-web --tag prodotto-42
nexoctl cache purge local sito-web --domain static.example.it --all
nexoctl cache purge status local <id>
nexoctl cache inspect local https://www.example.com/assets/app.js
nexoctl cache warm local sito-web --domain www.example.com --from urls.txt | --prefix /assets/
nexoctl cache stats local [sito-web] [--domain www.example.com] [--watch]
nexoctl cache webhook add|ls|rm|test local sito-web --domain static.example.it --type cloudflare --zone ... --token-file ...
```

## 11.11 Admin API

| Metodo | Path |
|--------|------|
| GET/POST/PUT/DELETE | `/api/v1/cache/policies[/{name}]` |
| GET/PUT | `/api/v1/distributions/{id}/cache` (default + override per dominio e behavior) |
| POST | `/api/v1/distributions/{id}/invalidations` (`{paths|patterns|tags|domain|all, soft}`) · GET `/invalidations/{inv}` |
| POST | `/api/v1/distributions/{id}/cache/warm` · GET `/warm/{job}` |
| GET | `/api/v1/cache/inspect?url=` · `/api/v1/cache/stats?distribution=&host=` |
| GET/POST/DELETE | `/api/v1/distributions/{id}/cache/webhooks` · POST `/webhooks/{w}/test` |

Azioni di policy: `admin:ManageCache`, `admin:InvalidateCache` (assegnabile separatamente, es. a una pipeline di deploy).

## 11.12 Test

### Server
| ID | Caso | Atteso |
|----|------|--------|
| TCC-01 | Ereditarietà globale → distribuzione → dominio → behavior | Policy effettiva corretta per ogni combinazione |
| TCC-02 | Stesso bucket su due domini con policy diverse | TTL e header diversi per dominio; dominio con cache disattivata sempre MISS/BYPASS |
| TCC-03 | Chiave di cache: query none/allowlist/denylist, normalizzazione, header, cookie, `includeHost` true/false | HIT/MISS attesi |
| TCC-04 | TTL: `Cache-Control` dell'oggetto rispettato/limitato da min/max; `Expires` | Scadenze corrette (clock finto) |
| TCC-05 | **Invalidazione automatica**: PUT nuovo contenuto → richiesta successiva su tutti i domini restituisce il nuovo contenuto | Mai contenuto vecchio dopo il 200 del PUT (singolo nodo e cluster) |
| TCC-06 | Invalidazione per path, glob, regex, tag, dominio, globale; soft purge | Solo le voci previste rimosse/marcate |
| TCC-07 | `stale-while-revalidate` e `stale-if-error` con origine senza quorum (dischi spenti) | Copia stale servita, `X-Cache: STALE` |
| TCC-08 | Request collapsing: 1000 richieste concorrenti su MISS | 1 sola lettura dall'origine |
| TCC-09 | Range caching: richieste Range su video da 2 GiB | Blocchi in cache, contenuto byte-identico |
| TCC-10 | Varianti di compressione e `Vary` | Client senza `Accept-Encoding` riceve contenuto non compresso |
| TCC-11 | Sicurezza: richiesta autenticata/firmata non serve contenuto in cache a un anonimo; contenuto `signed` con firma scaduta → 403 anche se in cache; cambio regole di accesso invalida le decisioni | Nessuna fuga |
| TCC-12 | Negative caching 404 → oggetto creato → visibile subito (invalidazione automatica) | 200 |
| TCC-13 | Quote per dominio ed eviction; disco cache pieno | Uso entro quota, nessun errore al client |
| TCC-14 | Cache directory corrotta / cancellata a caldo | Voci scartate, servite dall'origine |
| TCC-15 | Webhook di purge esterni: payload corretti (Cloudflare, Varnish, generico), retry con endpoint offline | Recapitati dopo ripristino |
| TCC-16 | Benchmark HIT da L1/L2 vs MISS | HIT L1 p99 < 1 ms su oggetti da 64 KiB |

### Cluster
| ID | Caso | Atteso |
|----|------|--------|
| TCC-C-01 | Modalità `shared`: stessa URL richiesta da 4 nodi | 1 MISS, poi HIT; 1 sola copia nel cluster |
| TCC-C-02 | Nodo proprietario fermato | Nessun errore, letture dall'origine, anello ricalcolato |
| TCC-C-03 | PUT su nodo1 → GET da nodo3 (local e shared) | Contenuto nuovo |
| TCC-C-04 | Nodo isolato durante un'invalidazione e poi rientrato | Svuota la cache (sequenza non contigua), nessun contenuto vecchio |
| TCC-C-05 | Compose con 2 proxy e domini diversi: purge per un dominio non tocca l'altro | — |

### CLI
| ID | Caso |
|----|------|
| TCL-C-01 | `cache policy create` + `dist cache set --domain` → `curl` mostra `Cache-Control` e `X-Cache` attesi |
| TCL-C-02 | `cache purge --domain ... '/assets/**'` → `purge status` completata su tutti i nodi → `curl` MISS |
| TCL-C-03 | `cache inspect` coerente con il comportamento reale |
| TCL-C-04 | `cache warm --from urls.txt` → richieste successive HIT |
| TCL-C-05 | `cache stats --json` valido contro schema |

### Dashboard
| ID | Caso |
|----|------|
| TD-C-01 | Override di policy per un dominio nella tab Cache → effetto verificato con richiesta al dominio |
| TD-C-02 | Editor policy: anteprima `Cache-Control` aggiornata in tempo reale, validazione TTL (min ≤ default ≤ max) |
| TD-C-03 | Invalidazione per glob da dashboard → stato per nodo fino a "completata" |
| TD-C-04 | Ispettore URL mostra policy effettiva e livello di provenienza |
| TD-C-05 | Grafici hit ratio per dominio con dati reali generati dal test |
| TD-C-06 | Utente con solo `admin:InvalidateCache` può invalidare ma non modificare le policy |
