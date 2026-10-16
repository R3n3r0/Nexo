# Distribuzioni e cache

Una **distribuzione** espone un bucket (o un suo prefisso) su uno o più domini tuoi, in stile CloudFront:
un sito statico su `www.example.com`, i download su `download.example.it`, un endpoint S3 dedicato a un
cliente su `s3.cliente.it`. Ogni dominio può avere il proprio certificato TLS e le proprie impostazioni di
cache; le modifiche raggiungono tutti i nodi senza riavvii.

Si gestiscono dalla console (**Distribuzioni**, con un wizard di creazione), con `nexoctl dist` e
`nexoctl cache`, o dall'Admin API (`/api/v1/distributions`, `/api/v1/cache`). Per un esempio completo dall'inizio
alla fine segui il [tutorial del sito statico su due domini](tutorial-sito-statico.md).

## Modalità

| `--mode` | Cosa serve | Uso |
|----------|------------|-----|
| `website` (default) | GET/HEAD/OPTIONS con risposte HTML: index document, SPA, pagine d'errore, redirect, header, behavior per percorso, URL firmati | Siti statici, asset, download |
| `s3` | L'API S3 completa del bucket sul dominio (firma SigV4 verificata con l'host pubblico) | Endpoint S3 dedicati per cliente o applicazione |

In modalità `s3` valgono IAM, bucket policy e regole di accesso del bucket; della distribuzione contano solo
domini, certificato, restrizioni e l'accesso di default (`public`, `authenticated` o `deny`). Le opzioni da
sito web (behavior per percorso, pagine d'errore, redirect, header) vengono rifiutate al salvataggio.

```bash
nexoctl dist create local sito-web --bucket web --prefix /prod --mode website \
  --domain www.example.com --domain example.com --index index.html --spa /index.html
nexoctl dist create local cliente --bucket cliente --mode s3 --domain s3.cliente.it --access authenticated
nexoctl dist list local
nexoctl dist info local sito-web
nexoctl dist disable local sito-web        # i domini rispondono 403
```

## Domini e instradamento

Un dominio è **esatto** (`www.example.com`) o **wildcard** (`*.preview.example.com`) e appartiene a una sola
distribuzione. Per ogni richiesta Nexo guarda l'header `Host` e sceglie, in ordine:

1. un dominio esatto di una distribuzione;
2. il dominio wildcard più specifico;
3. `<bucket>.<NEXO_DOMAIN>` (bucket in stile virtual host);
4. i domini principali di S3 e della console (`NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL`);
5. altrimenti l'API S3 path-style, oppure `421 Misdirected Request` con `--unknown-host misdirected`
   (`NEXO_UNKNOWN_HOST`); i listener web dedicati rispondono sempre `421`.

Non si possono usare come alias i nomi su cui risponde il deployment: i domini principali, `NEXO_DOMAIN` e i
virtual host di altri bucket, gli host degli endpoint dei nodi, `localhost` e il nome della macchina (`409`). Un
dominio sotto la wildcard di un'altra distribuzione (`app.example.com` sotto `*.example.com`) richiede i permessi
anche sul bucket di quella distribuzione (`403` altrimenti).

```console
$ nexoctl dist domain add local sito-web '*.preview.example.com'
$ nexoctl dist resolve local www.example.it
www.example.it: distribution `sito` (alias www.example.it, website), bucket sito
```

Le distribuzioni rispondono su due **listener dedicati**, se configurati: `--web-address` (`NEXO_WEB_ADDRESS`,
HTTP e challenge ACME HTTP-01) e `--web-tls-address` (`NEXO_WEB_TLS_ADDRESS`, HTTPS con i certificati delle
distribuzioni scelti per SNI), utili per esporre i siti sulle porte 80/443 senza esporre l'API S3. Sulla porta S3
(9000) rispondono secondo `--s3-distributions` (`NEXO_S3_DISTRIBUTIONS`): con `auto` (default) solo se il
deployment dichiara i nomi dell'endpoint S3 con `NEXO_SERVER_URL` o `NEXO_DOMAIN` (come nelle configurazioni di
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy)), con `on` sempre, con `off` mai. Senza
nomi dichiarati la porta S3 risponde a qualunque nome la raggiunga (load balancer, alias DNS, nomi di servizio):
un alias potrebbe coincidere con il nome che i client S3 usano. Sulla porta S3 i percorsi `/nexo/` (sonde di
salute, metriche, RPC tra i nodi) restano sempre del server, qualunque sia il dominio.

Se una distribuzione impedisce di usare il deployment, avvia i nodi con `--no-distributions`
(`NEXO_NO_DISTRIBUTIONS=true`): nessun dominio viene instradato, l'Admin API funziona e la distribuzione si
corregge o si rimuove (`nexoctl dist remove`); poi riavvia senza l'opzione.

## Behavior e accesso

Il **behavior di default** vale per tutti i percorsi; i **behavior** aggiuntivi cambiano il comportamento dei
percorsi che corrispondono a un pattern (`--prefix`, `--glob`, `--regex`, il primo che corrisponde vince) e
ereditano il resto dal default.

| `--access` | Chi legge |
|------------|-----------|
| `public` | chiunque, salvo i `deny` delle regole di accesso e il block public access del bucket o del cluster |
| `signed` | solo con URL o cookie firmati dalle chiavi di un key group (`--key-group`) |
| `authenticated` | solo con credenziali S3 (SigV4) o la sessione della console |
| `deny` | nessuno (403) |

```bash
nexoctl dist behavior add local sito-web --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
nexoctl dist behavior add local sito-web --regex '^/downloads/.*\.zip$' --access signed --key-group rilasci
nexoctl dist behavior add local sito-web --prefix /internal/ --access deny --position 1
nexoctl dist behavior list local sito-web
nexoctl dist test local sito-web /downloads/v1.zip     # quale behavior, accesso e header
```

Un behavior `public` di una distribuzione `website` risponde `403` finché il
[block public access](../sicurezza/condivisione.md#regole-di-accesso-pubbliche-e-block-public-access) del
bucket o del cluster è attivo. Il bucket di origine può restare privato: la distribuzione legge gli oggetti per
conto dei visitatori.

## Funzioni da sito web

| Funzione | Comando |
|----------|---------|
| Index document | `--index index.html` su `create`, `edit` o un behavior |
| Single page application | `--spa /index.html`: serve quel file (200) al posto degli oggetti mancanti |
| Pagine d'errore | `nexoctl dist error-page set local sito-web 404 /404.html` (`--response-status` per cambiare lo status) |
| Redirect | `dist redirect add` con `--https-only`, `--host-from example.com --to-host www.example.com` o `--from '^/vecchio/(.*)$' --to '/nuovo/$1'` (`--status 301/302/303/307/308`) |
| Header di risposta | `dist headers --set 'Name: value'`, `--unset`, `--security` (HSTS, nosniff, frame options, referrer policy) |
| CORS | `dist headers --cors-origin https://app.example.com --cors-method GET` |
| Log di accesso | `logging: {accessLog: true, format: combined}` nel YAML della distribuzione (`json` per default: un record `distribution access` nel log del server, con `event_time` l'inizio della richiesta e `time` l'ora del record) |

## URL e cookie firmati

Per i behavior `--access signed` Nexo verifica URL nel formato di CloudFront (`?Expires=&Signature=&Key-Pair-Id=`,
o `?Policy=` per una policy con risorsa wildcard, IP e data di inizio) e i cookie `CloudFront-*`, con chiavi
Ed25519 o RSA raccolte in **key group**:

```bash
nexoctl dist keygroup create local rilasci                    # genera la chiave, salva la privata
nexoctl dist keygroup key add local rilasci --public-key mia.pub.pem   # oppure carica la tua pubblica
nexoctl dist sign-url local sito-web /downloads/v1.zip --expire 1h --ip 203.0.113.0/24
nexoctl dist sign-url local sito-web /premium/ --expire 12h --cookie --resource 'https://www.example.com/premium/*'
```

La firma è verificata prima della cache e non entra nella chiave di cache. Più chiavi nello stesso key group
permettono di ruotarle senza interruzioni. La firma lato server è disponibile anche con
`POST /api/v1/distributions/{id}/sign` (permesso `admin:SignDistributionURL`) e dalla console.

## Certificati

| Comando | Certificato |
|---------|-------------|
| `nexoctl dist cert set local ID --auto` | ottenuto e rinnovato via ACME (Let's Encrypt per default) per tutti i domini esatti |
| `nexoctl dist cert set local ID --cert crt.pem --key key.pem` | caricato da te (catena PEM, foglia per prima) |
| `nexoctl dist cert set local ID --none` | nessuno: TLS terminato da un proxy |

Configurazione ACME del server: `NEXO_ACME_EMAIL`, `NEXO_ACME_DIRECTORY` (default Let's Encrypt),
`NEXO_ACME_CHALLENGE` (`http-01` sul listener web, `tls-alpn-01` sul listener web TLS), `NEXO_ACME_CA_CERT`
per una CA ACME privata. I certificati sono salvati cifrati nel deployment, distribuiti a tutti i nodi e
presentati per SNI; in cluster il rinnovo è eseguito da un solo nodo. `nexoctl dist cert list` mostra stato e
scadenza; la metrica `nexo_tls_certificate_expiry_seconds{certificate}` alimenta gli alert. I domini wildcard
richiedono un certificato caricato.

## Dietro uno o più proxy

- `NEXO_TRUSTED_PROXIES`: solo da questi indirizzi Nexo accetta `X-Forwarded-For/Proto` (o `Forwarded`, con
  `NEXO_FORWARDED_HEADERS=forwarded`) e il **PROXY protocol** sui listener elencati in `NEXO_PROXY_PROTOCOL`
  (`s3`, `console`, `web`). L'IP reale del client alimenta le restrizioni, le regole di accesso e gli URL
  firmati con `--ip`.
- L'`Host` deve arrivare invariato; `NEXO_TRUST_FORWARDED_HOST=true` solo per i proxy che lo riscrivono.
- **Origin secret**: ogni proxy aggiunge un header segreto; le richieste che arrivano ai nodi senza
  (aggirando il proxy) ricevono `403`.

  ```bash
  nexoctl dist origin-secret rotate local sito-web          # stampa il nuovo segreto una volta sola
  nexoctl dist restrict local sito-web --origin-secret-header X-Origin-Secret
  ```

  Con più proxy, un segreto per proxy come riferimento `${secret:proxy-a}` nel YAML, risolto su ogni nodo da
  `NEXO_SECRET_PROXY_A`.
- **Restrizioni per indirizzo**: `nexoctl dist restrict local sito-web --deny-ip 203.0.113.0/24` (o
  `--allow-ip`). `--rate-limit` viene salvato ma non è ancora applicato: limita le richieste sul proxy.

Configurazioni pronte per nginx, Caddy, Traefik, HAProxy e Cloudflare:
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy).

## Cache

Nexo conserva le risposte delle distribuzioni in una cache in memoria (`NEXO_CACHE_MEMORY`, default 256 MiB)
e, con `NEXO_CACHE_DIR`, su disco (`NEXO_CACHE_SIZE`, meglio un SSD separato dai dischi dei dati).
L'header `X-Cache` dice come è stata servita ogni risposta:

| `X-Cache` | Significato |
|-----------|-------------|
| `HIT` | dalla cache, fresca |
| `MISS` | dall'origine (e messa in cache se possibile) |
| `STALE` | copia scaduta servita mentre si aggiorna o perché l'origine non risponde |
| `EXPIRED` | copia scaduta riletta dall'origine |
| `REVALIDATED` | copia scaduta confermata dall'origine (stesso ETag) |
| `BYPASS` | cache saltata (cache disattivata, richiesta con credenziali, cookie o header di bypass) |

### Livelli e policy

Le impostazioni si ereditano: **cluster** (`nexoctl cache settings`) → **distribuzione** → **dominio**
(`--domain`) → **percorso** (`--behavior`). Ogni livello può scegliere una **policy** e cambiarne singoli campi.

| Policy predefinita | TTL | Uso |
|--------------------|-----|-----|
| `standard` | 1h (massimo 7d), rispetta il `Cache-Control` degli oggetti | default |
| `aggressive` | 1d (minimo 1h, massimo 30d), finestre stale lunghe | contenuti che cambiano raramente |
| `immutable-assets` | 365d, anche nei browser | asset con hash nel nome |
| `respect-origin` | solo quello dichiarato dagli oggetti | controllo dal publisher |
| `disabled` | nessuna cache | contenuti sempre freschi |

```bash
nexoctl dist cache set local sito-web --policy standard
nexoctl dist cache set local sito-web --domain www.example.com --policy aggressive --quota 20GiB
nexoctl dist cache set local sito-web --domain '*.preview.example.com' --disable
nexoctl dist cache set local sito-web --behavior '/assets/**' --ttl-default 365d
nexoctl dist cache set local sito-web --behavior '/api/' --behavior-type prefix --disable
nexoctl cache policy create local lunga --ttl-default 1d --ttl-max 30d --query allowlist:v --stale-while-revalidate 5m
nexoctl dist cache show local sito-web
```

Altri campi utili: `--query` (parametri della query nella chiave), `--key-header`/`--key-cookie`,
`--bypass-cookie`, `--negative-ttl` (404/403), `--stale-if-error`, `--compression`, `--range-caching` (video e
file grandi a blocchi), `--warm-on-write`. Le risposte a richieste autenticate non sono mai condivise tra
utenti diversi.

### Invalidazione

- **Automatica** (default): quando un oggetto cambia (PUT, DELETE, copia, metadati, tag) le sue voci vengono
  scartate su tutti i domini e tutti i nodi.
- **Manuale**, per percorso esatto, glob, prefisso, regex, tag, dominio o tutto, con `--soft` per marcare come
  scadute invece di cancellare:

  ```bash
  nexoctl dist invalidate local sito-web '/assets/**' --wait
  nexoctl cache purge local sito-web --tag prodotto-42      # oggetti con x-amz-meta-cache-tag: prodotto-42
  nexoctl cache purge local sito-web --domain static.example.it --all
  nexoctl cache purge list local sito-web
  ```

- **CDN davanti a Nexo**: i webhook di purge (`nexoctl cache webhook add`, tipi `cloudflare`, `fastly`,
  `akamai`, `nginx`, `varnish`, `generic`) propagano ogni invalidazione, con retry e coda persistente. Le
  destinazioni devono essere indirizzi pubblici, salvo le reti in `NEXO_CACHE_WEBHOOK_ALLOW`.

Le invalidazioni e i warm-up richiedono solo `admin:InvalidateCache`: una pipeline di deploy non ha bisogno di
altri permessi.

### Warm-up, diagnostica, statistiche

```bash
nexoctl cache warm local sito-web --prefix /assets/ --bandwidth 20MiB --wait
nexoctl cache inspect local https://www.example.com/assets/app.js    # chiave, policy e livello, stato su ogni nodo
nexoctl cache stats local                                            # hit ratio, byte, top URL per dominio
nexoctl cache settings local --node-header                           # aggiunge X-Cache-Node alle risposte
```

Con `nexoctl cache settings local --debug-secret-file debug.secret`, le richieste con l'header
`X-Nexo-Cache-Debug: <segreto>` ricevono anche `X-Cache-Key` e `X-Cache-Policy`.

### In cluster

`NEXO_CACHE_MODE` sceglie come i nodi condividono la cache:

| Modalità | Comportamento |
|----------|---------------|
| `local` (default) | ogni nodo ha la sua cache |
| `shared` | ogni chiave ha un nodo proprietario (consistent hashing): una sola copia nel cluster, capacità sommata |
| `shared+local` | come `shared` più una piccola cache locale per gli oggetti più richiesti |

## Permessi

| Permesso | Per |
|----------|-----|
| `admin:ManageDistributions` | creare e modificare distribuzioni e key group |
| `admin:ManageCertificates` | caricare e rimuovere certificati |
| `admin:SignDistributionURL` | firmare URL e cookie lato server |
| `admin:ManageCache` | impostazioni, policy e webhook della cache |
| `admin:InvalidateCache` | invalidazioni e warm-up |

## Metriche

`nexo_distribution_requests_total{distribution,host,status}`, `nexo_distribution_bytes_served_total`,
`nexo_distribution_request_duration_seconds`, `nexo_cache_requests_total{distribution,host,result}`,
`nexo_cache_hit_ratio`, `nexo_cache_bytes_served_total{source}`, `nexo_cache_size_bytes`,
`nexo_cache_invalidations_total`, `nexo_cache_webhook_calls_total`. Vedi [Monitoraggio](../operazioni/monitoraggio.md).

## GitOps

Distribuzioni, impostazioni di cache e policy si esportano e importano in YAML:

```bash
nexoctl dist export local > distributions.yaml
nexoctl dist import local distributions.yaml
nexoctl dist cache export local sito-web > sito-web-cache.yaml
nexoctl dist cache import local sito-web-cache.yaml
nexoctl cache policy export local > policies.yaml
nexoctl cache policy import local policies.yaml
```

I segreti (origin secret, token dei webhook) si scrivono come riferimenti `${secret:nome}`, risolti sui nodi
dalle variabili `NEXO_SECRET_*`: i file YAML non contengono segreti.
