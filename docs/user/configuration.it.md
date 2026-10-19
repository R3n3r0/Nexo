# Configurazione

Nexo ha due livelli di configurazione:

- **statica**: letta all'avvio da flag, variabili d'ambiente `NEXO_*` e un file JSON opzionale; cambiarla
  richiede un riavvio;
- **dinamica**: sottosistemi chiave=valore salvati nel deployment (su tutti i nodi), modificabili a caldo
  dalla console (**Configurazione**), da `nexoctl admin config` o dall'Admin API (`/api/v1/config`).

## Configurazione statica

Precedenza: **flag > variabili d'ambiente > file `--config` > default**. Ogni flag di `nexo server` ha la
sua variabile `NEXO_<FLAG>` (trattini → underscore, maiuscole), con le eccezioni indicate (`--config` è
`NEXO_CONFIG_FILE`, `--dry-run` non ha variabile). I valori di
tipo lista accettano più voci separate da virgola nelle variabili e flag ripetuti o separati da virgola
sulla riga di comando.

### Credenziali e endpoint

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_ROOT_USER` / `NEXO_ROOT_USER_FILE` | — | obbligatoria | Utente root (≥ 3 caratteri). Solo da ambiente o file, mai da flag |
| `NEXO_ROOT_PASSWORD` / `NEXO_ROOT_PASSWORD_FILE` | — | obbligatoria | Password root (≥ 8 caratteri). La variante `_FILE` vince sulla variabile semplice |
| `NEXO_ENDPOINTS` | argomenti di `nexo server` | — | Dischi e nodi, separati da spazi (gli argomenti sulla riga di comando vincono). Vedi [Modalità](deployment-modes.md) |
| `NEXO_CONFIG_FILE` | `--config` | — | File JSON di configurazione (precedenza più bassa) |

### Rete e URL pubblici

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_ADDRESS` | `--address` | `:9000` | Listener API S3 (anche health, metriche e RPC tra i nodi) |
| `NEXO_CONSOLE_ADDRESS` | `--console-address` | `:9001` | Listener console e Admin API |
| `NEXO_REGION` | `--region` | `us-east-1` | Regione S3 riportata ai client e usata nella firma |
| `NEXO_DOMAIN` | `--domain` | — | Abilita le richieste virtual-host (`<bucket>.<dominio>`) |
| `NEXO_SERVER_URL` | `--server-url` | — | URL pubblico S3 usato nei link presigned e di condivisione (dietro proxy) |
| `NEXO_BROWSER_REDIRECT_URL` | `--browser-redirect-url` | — | URL pubblico della console |
| `NEXO_TRUSTED_PROXIES` | `--trusted-proxies` | — | IP o CIDR dei reverse proxy fidati |
| `NEXO_FORWARDED_HEADERS` | `--forwarded-headers` | `x-forwarded` | Famiglia di header letta dai proxy fidati: `x-forwarded` (`X-Forwarded-For/Proto/Host`) o `forwarded` (RFC 7239) |
| `NEXO_TRUST_FORWARDED_HOST` | `--trust-forwarded-host` | `false` | Usa l'host inoltrato al posto dell'header `Host` (solo per proxy che lo riscrivono) |
| `NEXO_PROXY_PROTOCOL` | `--proxy-protocol` | — | Listener che accettano il PROXY protocol v1/v2 dai proxy fidati: `s3`, `console`, `web` |
| `NEXO_WEB_ADDRESS` | `--web-address` | — | Listener HTTP che serve solo le distribuzioni e le challenge ACME HTTP-01 (es. `:80`) |
| `NEXO_WEB_TLS_ADDRESS` | `--web-tls-address` | — | Listener TLS delle sole distribuzioni, con i loro certificati per SNI (es. `:443`) |
| `NEXO_UNKNOWN_HOST` | `--unknown-host` | `path-style` | Risposta del listener S3 a un host sconosciuto: `path-style` (API S3) o `misdirected` (421) |
| `NEXO_S3_DISTRIBUTIONS` | `--s3-distributions` | `auto` | Distribuzioni servite anche dal listener S3: `auto` (solo se `NEXO_SERVER_URL` o `NEXO_DOMAIN` dichiarano i nomi dell'endpoint S3), `on` (opt-in esplicito), `off` (solo i listener web) |
| `NEXO_NO_DISTRIBUTIONS` | `--no-distributions` | `false` | Ripristino: nessun host instradato a una distribuzione su nessun listener (restano modificabili dall'Admin API) |

### TLS

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_CERTS_DIR` | `--certs-dir` | — (HTTP) | Directory con `public.crt` e `private.key` (abilita TLS su tutti i listener), ricaricata a caldo |
| `NEXO_TLS_MIN_VERSION` | `--tls-min-version` | `1.2` | Versione TLS minima: `1.2` o `1.3` |
| `NEXO_TLS_INTERNODE_VERIFY` | `--tls-internode-verify` | `false` | mTLS tra i nodi (endpoint `https://`) |

Dettagli in [TLS e certificati](security/tls.md).

### Storage ed erasure coding

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_METADATA_LOG` | `--metadata-log` | `false` | Tiene i metadati degli oggetti in un log segmentato con group commit su ogni disco (scritture piccole più veloci); i dischi vengono convertiti all'avvio, e riconvertiti quando lo disattivi. Vedi [Log dei metadati](operations/metadata-log.md) |
| `NEXO_STORAGE_CLASS_STANDARD` | `--storage-class-standard` | automatico | Parità della classe `STANDARD` come `EC:n` (SNMD e cluster) |
| `NEXO_STORAGE_CLASS_RRS` | `--storage-class-rrs` | `EC:1` | Parità della classe `REDUCED_REDUNDANCY` (al più quella di `STANDARD`) |

### Accessi e distribuzioni

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_ACCESS_RULES_MAX` | `--access-rules-max` | `200` | Numero massimo di regole di accesso per bucket |
| `NEXO_KEY_NORMALIZATION` | `--key-normalization` | `nfc` | Normalizzazione Unicode delle chiavi confrontate dalle regole: `nfc` o `none` |
| `NEXO_BLOCK_PUBLIC_ACCESS` | `--block-public-access` | `false` | Block public access a livello di cluster (finché non viene cambiato a caldo) |
| `NEXO_BLOCK_PUBLIC_ACCESS_MODE` | `--block-public-access-mode` | `reject` | `reject` (regole pubbliche rifiutate al salvataggio) o `ignore` (ignorate in valutazione) |
| `NEXO_SECRET_<NOME>` / `NEXO_SECRET_<NOME>_FILE` | — | — | Segreti referenziati come `${secret:<nome>}` da regole di accesso, origin secret e webhook (nome in minuscolo, `-` e `_` equivalenti) |
| `NEXO_ACME_DIRECTORY` | `--acme-directory` | Let's Encrypt | Directory ACME dei certificati automatici |
| `NEXO_ACME_EMAIL` | `--acme-email` | — | Contatto dell'account ACME |
| `NEXO_ACME_CHALLENGE` | `--acme-challenge` | `http-01` | `http-01` (sul listener web) o `tls-alpn-01` (sul listener web TLS) |
| `NEXO_ACME_CA_CERT` | `--acme-ca-cert` | — | CA (PEM) di un server ACME privato |
| `NEXO_ACME_DNS_NAMESERVERS` | `--acme-dns-nameservers` | — | nameserver (`host[:porta]`, separati da virgola) interrogati dalla verifica di propagazione DNS-01 al posto di quelli autoritativi della zona (DNS split-horizon) |

### Cache delle distribuzioni

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_CACHE_DIR` | `--cache-dir` | — (solo memoria) | Directory del livello su disco (SSD/NVMe separato dai dischi dati) |
| `NEXO_CACHE_SIZE` | `--cache-size` | `10GiB` | Dimensione del livello su disco (di quello in memoria senza `--cache-dir`) |
| `NEXO_CACHE_MEMORY` | `--cache-memory` | `256MiB` | Dimensione del livello in memoria |
| `NEXO_CACHE_MODE` | `--cache-mode` | `local` | In cluster: `local`, `shared` o `shared+local` |
| `NEXO_CACHE_WEBHOOK_ALLOW` | `--cache-webhook-allow` | — | Reti interne raggiungibili dai webhook di purge (di default solo indirizzi pubblici) |

Le dimensioni accettano `KiB`, `MiB`, `GiB`, `TiB` (e `KB`, `MB`, `GB`, `TB`).

### Cifratura (KMS e SSE)

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `NEXO_KMS_SECRET_KEY` / `_FILE` | — | Master key del KMS integrato: `<nome>:<base64 di 32 byte>` |
| `NEXO_KMS_SECRET_KEY_PREVIOUS` / `_FILE` | — | Master key sostituite (separate da virgola), solo per la rotazione |
| `NEXO_KMS_ENDPOINT` | — | URL `https://` dei server KES (separati da virgola) |
| `NEXO_KMS_KEY_NAME` | — | Chiave di default su KES |
| `NEXO_KMS_CLIENT_CERT` / `NEXO_KMS_CLIENT_KEY` | — | Certificato e chiave client per l'mTLS verso KES |
| `NEXO_KMS_CAPATH` | radici di sistema | CA dei server KES (file o directory) |
| `NEXO_SSEC_ALLOW_INSECURE` | `off` | Accetta chiavi SSE-C su HTTP in chiaro: **solo test** |

Solo da ambiente (nessun flag). Dettagli in [KMS e SSE](security/kms-sse.md).

### Log, metriche e tracing

| Variabile | Flag | Default | Descrizione |
|-----------|------|---------|-------------|
| `NEXO_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, `error` (modificabile a caldo con il sottosistema `logger`) |
| `NEXO_LOG_FORMAT` | `--log-format` | `json` | `json` o `text` |
| `NEXO_AUDIT_LOG` | `--audit-log` | `false` | Scrive anche nel log del server ogni record di audit (uno per richiesta); il livello `debug` li registra comunque |
| `NEXO_PROMETHEUS_AUTH_TYPE` | — | `jwt` | Endpoint `/nexo/metrics/v1/*`: `jwt` (bearer token) o `public` |
| `NEXO_OTEL_EXPORTER_OTLP_ENDPOINT` | — | — (disattivato) | Collector OpenTelemetry (`http(s)://…`) |
| `NEXO_OTEL_EXPORTER_OTLP_PROTOCOL` | — | `http/protobuf` | `http/protobuf` o `grpc` |
| `NEXO_OTEL_TRACES_SAMPLER_ARG` | — | `1` | Quota (0–1) delle tracce campionate |

Valgono anche le variabili standard `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT` e
`OTEL_EXPORTER_OTLP_CERTIFICATE`. Vedi [Monitoraggio](operations/monitoring.md) e
[Trace, log e audit](operations/trace-log-audit.md).

### Parametri avanzati del cluster (solo ambiente)

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `NEXO_LOCK_STALE_AFTER` | `60s` | Dopo quanto un lock non più rinnovato da un nodo viene rilasciato |
| `NEXO_LOCK_REFRESH_INTERVAL` | `10s` | Intervallo di rinnovo dei lock (minore di `NEXO_LOCK_STALE_AFTER`) |
| `NEXO_RPC_CALL_TIMEOUT` | `30s` | Durata massima di una chiamata tra nodi |
| `NEXO_HEAL_RETRY_MAX_DELAY` | `5m` | Attesa massima tra due tentativi di healing in background di un oggetto |
| `NEXO_CACHE_RESYNC_INTERVAL` | `2m` | Ricarica periodica dello stato condiviso (IAM, policy, configurazione) su ogni nodo |
| `NEXO_SEND_TIMEOUT` | `20s` | Dopo quanto una risposta il cui client ha smesso di leggere viene interrotta: download S3 e dei siti delle distribuzioni, console e Admin API, su tutti i listener; una connessione HTTP/2 che non accetta byte per lo stesso tempo viene chiusa con tutti i suoi stream |

### File di configurazione

`--config nexo.json` (o `NEXO_CONFIG_FILE`) legge un documento JSON con le chiavi in camelCase dei flag;
le chiavi assenti restano ai default. Le credenziali root, i segreti e i parametri "solo ambiente" non si
possono mettere nel file.

```json
{
  "address": ":9000",
  "consoleAddress": ":9001",
  "region": "eu-south-1",
  "serverUrl": "https://s3.example.com",
  "browserRedirectUrl": "https://console.example.com",
  "certsDir": "/etc/nexo/certs",
  "trustedProxies": ["10.0.0.0/8"],
  "logLevel": "info",
  "logFormat": "json",
  "endpoints": ["/mnt/disk{1...4}"],
  "storageClassStandard": "EC:2",
  "cacheDir": "/var/cache/nexo",
  "cacheMode": "local"
}
```

### Validare senza avviare

`nexo server --dry-run` applica la stessa precedenza, valida tutti i valori (credenziali, URL, CIDR,
parità, KMS, cache…), la topologia, i permessi dei dischi, le porte e i certificati, e stampa un
riepilogo; con un errore esce con codice diverso da zero e l'elenco di **tutti** i problemi trovati.

## Configurazione dinamica

```bash
nexoctl admin config get local                       # tutti i sottosistemi
nexoctl admin config get local logger api            # solo alcuni
nexoctl admin config set local logger level=debug    # le altre chiavi restano
nexoctl admin config reset local logger              # torna ai default
nexoctl admin config export local --output config.json
nexoctl admin config import local config.json
```

Le modifiche valgono per tutto il deployment (in cluster vengono propagate a tutti i nodi). I valori
sensibili (password, token, credenziali negli URL) sono mostrati mascherati; reinviare il valore
mascherato conserva quello salvato. Lo schema completo, con il default di ogni chiave e se il server la
applica, è su `GET /api/v1/config/schema` e nella pagina **Configurazione** della console.

| Sottosistema | Chiavi | Applicato a caldo |
|--------------|--------|-------------------|
| `logger` | `level` (`debug`…`error`), `format` (`text`, `json`) | ✅ |
| `api` | `cors_allow_origin` (default `*`), `root_access` (default `on`; `off` nega le credenziali root sull'API S3) | ✅ |
| `api` | `requests_max` (default `0`: nessun limite), `requests_deadline` (default `10s`): vedi [Limiti di richieste](#limiti-di-richieste) | ✅ alla richiesta successiva |
| `ratelimit` | `s3_ip`, `s3_access_key`, `s3_bucket`, `admin_ip`, `admin_user` e i loro `_burst`: vedi [Limiti di richieste](#limiti-di-richieste) | ✅ alla richiesta successiva |
| `scanner` | `speed` (`fastest`…`slowest`), `cycle` (default `1m`) | ✅ all'inizio di ogni ciclo |
| `access` | `block_public`, `block_public_mode` (`reject`, `ignore`) | ✅ |
| `notify_webhook`, `notify_kafka`, `notify_nats`, `notify_amqp`, `notify_redis`, `notify_postgres` (anche `:<id>`) | `enable`, chiavi del target, `queue_dir`, `queue_limit`, `comment` | ✅ (vedi [Eventi](events-lifecycle.md)) |
| `audit_webhook`, `audit_kafka` (anche `:<id>`) | come i `notify_*` dello stesso tipo | ✅ (vedi [Audit](operations/trace-log-audit.md#audit-log)) |
| `browser` | `session_duration` (12h), `upload_max_size` | ❌ non ancora applicati |
| `region` | `name`, `comment` | ❌ la regione effettiva è quella di `--region` |

`nexoctl admin config set` avvisa quando imposta una chiave non applicata (`region.name is stored but not
applied yet by the server`); la console la segnala allo stesso modo.

### Limiti di richieste

Ogni nodo limita le richieste che serve con dei token bucket: una chiave ammette in media il suo valore in
richieste al secondo e il suo `_burst` (default: il valore stesso) tutte insieme. Sono tutte `0` per default:
nessun limite.

| Chiave | Limita | Risposta oltre la soglia |
|--------|--------|--------------------------|
| `ratelimit.s3_ip` | Richieste S3 di un indirizzo client (il client dietro i [proxy fidati](#rete-e-url-pubblici); IPv6 per rete /64) | `503 SlowDown` (XML S3) |
| `ratelimit.s3_access_key` | Richieste S3 di una access key (richieste autenticate; le URL presigned contano sull'indirizzo e sul bucket) | `503 SlowDown` |
| `ratelimit.s3_bucket` | Richieste S3 a un bucket, chiunque le invii | `503 SlowDown` |
| `ratelimit.admin_ip` | Richieste all'Admin API di un indirizzo client (console e `nexoctl admin`) | `429 too_many_requests` (problem+json) |
| `ratelimit.admin_user` | Richieste all'Admin API di un utente autenticato | `429 too_many_requests` |
| `api.requests_max` | Richieste S3 servite insieme da un nodo; una richiesta oltre la soglia attende uno slot per `api.requests_deadline` | `503 SlowDown` |

Ogni rifiuto porta `Retry-After` (secondi): gli SDK S3 ripetono le `SlowDown` con backoff. I probe di salute
(`/nexo/health/*`), le raccolte delle metriche (`/nexo/metrics/*`) e l'RPC tra i nodi non sono mai limitati; le
[distribuzioni](distributions/README.md#limite-di-richieste) hanno un limite proprio. Le richieste rifiutate sono
contate in `nexo_ratelimit_throttled_total{api,kind}` e marcate nell'audit log (tag `throttled` con il tipo di
limite) e nella trace live (`error: Service Unavailable (rate limit: ip)`).

```bash
nexoctl admin config set local ratelimit s3_ip=200 s3_ip_burst=400 admin_ip=20
nexoctl admin config set local api requests_max=1024 requests_deadline=5s
nexoctl admin config reset local ratelimit
```

**In un cluster i limiti sono per nodo.** La configurazione è la stessa su ogni nodo (una modifica li raggiunge
tutti subito), ma ogni nodo conta le proprie richieste: dietro un load balancer che distribuisce i client su N
nodi il deployment ammette fino a N volte ciascun limite (un client che invia a un solo nodo incontra il limite di
quel nodo). Dividi il limite voluto per il deployment per il numero dei nodi. Così il controllo resta locale
(nessuna chiamata tra i nodi per richiesta) e continua a funzionare quando dei nodi cadono.
