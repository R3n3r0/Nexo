# 10. Regole di accesso (wildcard/regex), domini e distribuzioni

Obiettivo: poter definire **chi accede a cosa** con pattern glob e **espressioni regolari**, rendere pubblici
bucket o porzioni di bucket, e associare **più domini** a un bucket tramite **distribuzioni** in stile
CloudFront. Tutto deve essere configurabile da **dashboard, CLI e Admin API**, a caldo, in singolo nodo e in cluster.

## 10.1 Regole di accesso (Access Rules)

Le policy in formato AWS (vedi [09](09-presigned-policy-condivisione.md)) supportano solo wildcard `*` e `?`.
Nexo aggiunge un livello di **regole di accesso per bucket**, più semplice da usare e con supporto regex.
Le regole vengono valutate dallo **stesso policy engine** come una sorgente resource-based aggiuntiva.

### Modello
```yaml
bucket: media
rules:
  - id: immagini-pubbliche
    priority: 10                 # ordine di valutazione (più basso = prima)
    effect: allow                # allow | deny
    principals: ["*"]            # "*" = anonimo/tutti, user:mario, group:marketing, accesskey:AK..., distribution:sito-web
    actions: [read]              # read (GET/HEAD), list, write (PUT/POST/multipart), delete, tag, all; oppure azioni s3:* esplicite
    match:
      type: regex                # prefix | glob | regex
      pattern: '^img/[0-9]{4}/.*\.(jpe?g|png|webp)$'
      caseInsensitive: true
    conditions:                  # opzionali
      sourceIp: ["10.0.0.0/8", "2001:db8::/32"]
      notSourceIp: []
      referer: ["https://*.example.com/*"]
      host: ["cdn.example.com"]  # dominio con cui è arrivata la richiesta
      secureTransport: true
      timeWindow: { from: "2026-01-01T00:00:00Z", to: "2026-12-31T23:59:59Z" }
      methods: [GET, HEAD]
      headers: { "X-Origin-Secret": "${secret:proxy-token}" }
  - id: blocca-privati
    priority: 5
    effect: deny
    principals: ["*"]
    actions: [all]
    match: { type: glob, pattern: "**/private/**" }
```

### Tipi di pattern
| Tipo | Sintassi | Esempio | Note |
|------|----------|---------|------|
| `prefix` | prefisso letterale | `public/` | Più veloce (lookup in trie) |
| `glob` | `*` (un segmento), `**` (più segmenti), `?`, `[abc]`, `{a,b}` | `docs/**/*.pdf`, `{img,video}/*` | Compilato in regex anchored |
| `regex` | **RE2** (`regexp` di Go) | `^users/[a-z0-9_-]+/avatar\.png$` | Tempo lineare garantito: **nessun ReDoS** |

Regole di sicurezza per le regex:
- solo RE2 (niente backreference/lookaround), lunghezza massima 1024 caratteri, max 200 regole per bucket (configurabile);
- validazione e compilazione al salvataggio (errore con posizione), cache delle regex compilate per bucket;
- match sulla **chiave decodificata** (URL-decoding prima del match, normalizzazione Unicode NFC configurabile),
  così `%2e%2e/` o doppi encoding non aggirano le regole;
- gruppi nominati utilizzabili nelle condizioni (es. `(?P<user>[^/]+)` confrontato con `${aws:username}`):
  ```yaml
  match: { type: regex, pattern: '^home/(?P<user>[^/]+)/' }
  conditions: { captureEquals: { user: "${aws:username}" } }
  ```

### Valutazione
1. Raccolta di tutte le sorgenti: policy IAM dell'identità, bucket policy, **access rules**, regole di distribuzione (§10.2).
2. Un `deny` applicabile in qualsiasi sorgente → **nega** (le regole non possono scavalcare object lock né i limiti delle policy inline dei service account).
3. Altrimenti `allow` da almeno una sorgente → consenti.
4. Altrimenti nega (deny-by-default).
5. All'interno delle access rules, a parità di effetto conta la `priority`; la risposta del simulatore indica la regola determinante.

### Scorciatoie "pubblico"
| Preset | Effetto |
|--------|---------|
| `private` | Nessuna regola anonima |
| `public-read` | `allow * read` su tutto il bucket |
| `public-read-list` | `read` + `list` |
| `public-prefix` | `read` su un prefix/glob/regex scelto |
| `public-write` | `write` anonimo (richiede conferma esplicita, con avviso in dashboard) |

### Protezioni globali
- **Block public access** a livello di cluster e di bucket (come AWS): se attivo, le regole con `principals: ["*"]` vengono
  rifiutate al salvataggio o ignorate in valutazione (scelta configurabile), con avviso in dashboard.
- Audit di ogni modifica alle regole (chi, quando, diff).

## 10.2 Distribuzioni (stile CloudFront)

Una **distribuzione** espone un bucket (o un prefix di un bucket) su uno o più **domini personalizzati**, con
comportamenti per percorso. Più proxy/punti di accesso possono puntare allo stesso cluster usando domini diversi
o la stessa distribuzione.

```
 utenti ─▶ cdn.example.com ─┐                     ┌─ distribuzione "sito-web" ─▶ bucket web, prefix /prod
 utenti ─▶ www.example.com ─┼─▶ proxy A ─┐        │      behaviors: /api/* deny, /assets/* cache 1y, /* SPA
 utenti ─▶ static.example.it ─▶ proxy B ─┼─▶ Nexo ─┤
 SDK S3 ─▶ s3.example.com ─────────────── ┘        └─ distribuzione "download" ─▶ bucket rilasci, signed URL
```

### Modello
```yaml
id: sito-web
enabled: true
aliases:                       # più domini per la stessa distribuzione
  - www.example.com
  - example.com
  - "*.preview.example.com"    # wildcard di sottodominio
origin:
  bucket: web
  pathPrefix: /prod            # opzionale: la distribuzione vede solo questo prefix
  versionPinning: false
mode: website                  # website | s3   (vedi sotto)
defaultBehavior:
  access: public               # public | signed | authenticated | deny
  allowedMethods: [GET, HEAD, OPTIONS]
  indexDocument: index.html
  spaFallback: /index.html     # restituisce index.html per i path non trovati (SPA)
  cache: { cacheControl: "public, max-age=300" }
behaviors:                     # valutati in ordine; primo match vince
  - pathPattern: { type: glob, pattern: "/assets/**" }
    cache: { cacheControl: "public, max-age=31536000, immutable" }
  - pathPattern: { type: regex, pattern: '^/downloads/.*\.(zip|iso)$' }
    access: signed             # richiede URL/cookie firmato dalla distribuzione
    signing: { keyGroup: rilasci }
  - pathPattern: { type: prefix, pattern: "/internal/" }
    access: deny
errorPages:
  - { status: 404, path: /404.html, responseStatus: 404 }
  - { status: 403, path: /404.html, responseStatus: 404 }   # nasconde l'esistenza degli oggetti
redirects:
  - { from: '^/vecchio/(.*)$', to: '/nuovo/$1', status: 301 }
  - { hostFrom: example.com, toHost: www.example.com, status: 301 }
  - { httpsOnly: true }        # redirect HTTP → HTTPS
headers:
  response:
    Strict-Transport-Security: "max-age=63072000; includeSubDomains"
    X-Content-Type-Options: nosniff
  cors: { allowedOrigins: ["https://app.example.com"], allowedMethods: [GET], maxAge: 3600 }
restrictions:
  sourceIp: { allow: [], deny: ["203.0.113.0/24"] }
  originSecretHeader: { name: X-Origin-Secret, value: "${secret:proxy-a}" }  # accetta solo traffico dal proxy
  rateLimit: { requestsPerSecond: 200, burst: 400, per: ip }                 # P2
tls:
  certificate: auto            # auto (ACME/Let's Encrypt) | upload:<id> | none (TLS terminato dal proxy)
logging: { accessLog: true, target: webhook:audit }
```

### Modalità di una distribuzione
| `mode` | Comportamento | Uso |
|--------|---------------|-----|
| `website` | Solo GET/HEAD/OPTIONS (POST/PUT se il behavior li consente con `signed`), risposte HTML/plain, index document, error page, SPA fallback, redirect, nessun XML S3 | Siti statici, asset, download pubblici |
| `s3` | API S3 completa sul dominio personalizzato: il dominio è un alias del bucket (es. `s3.cliente.it` → bucket `cliente`), firma SigV4 verificata con l'host pubblico; i presigned generati usano quel dominio | Endpoint S3 dedicati per cliente/applicazione |

### Accesso ai contenuti
| `access` | Descrizione |
|----------|-------------|
| `public` | Anonimo, ma sempre soggetto ai `deny` delle access rules e al block public access |
| `signed` | **URL firmati di distribuzione** (`?Expires=&Signature=&Key-Pair-Id=` stile CloudFront, Ed25519/RSA) o **cookie firmati** per un path/glob: utile per contenuti premium su più file. Policy "custom" con risorsa wildcard, data di inizio/fine, IP |
| `authenticated` | Richiede credenziali S3 (SigV4) o sessione console |
| `deny` | Sempre 403/404 |

Regole di accesso delle due modalità (implementazione):
- **website**: un behavior `public` è rifiutato (403) finché il block public access del bucket (o del cluster) è
  attivo, **qualunque cosa conceda la lettura** (una access rule per `distribution:<id>`, una bucket policy
  pubblica): il guardrail precede la valutazione delle regole. Il flag è letto dalla cache in memoria dei metadati
  del bucket (invalidata dal Notifier), mai con I/O sul percorso di ogni richiesta; una decisione che non si può
  prendere (set senza quorum, metadati mai letti) risponde **503 + `Retry-After`**, mai 403. Un `deny` esplicito
  (access rule, bucket policy) rifiuta sempre; un behavior `authenticated` richiede un allow.
- **s3**: la distribuzione è l'intera API S3 del bucket sul dominio, governata da IAM, bucket policy e access rules
  (un prefisso privato si protegge con una regola `deny`). Del behavior vale solo `defaultBehavior.access`:
  `public` (default), `authenticated` (nessuna richiesta anonima) o `deny`, applicato dopo l'autenticazione SigV4.
  Behavior per path, `signed`, `allowedMethods`, index/SPA, error page, redirect, header di risposta, CORS (vale
  la configurazione CORS del bucket) e impostazioni di cache del behavior sono **rifiutati al salvataggio** (400):
  niente viene accettato e poi ignorato, e `dist test` riporta la stessa decisione delle richieste reali.
  L'ammissione (distribuzione abilitata, indirizzo, origin secret) precede anche la verifica della firma: un client
  che aggira il proxy riceve il 403 dell'ammissione, mai gli errori SigV4.
- I redirect regex espandono le catture del **path codificato**; un target che il browser porterebbe fuori sito
  (`//host`, `/\host`, caratteri di controllo, backslash) non viene seguito. Il probe dell'index document di una
  "directory" (`/dir` → `/dir/`) è autorizzato come la lettura dell'index: un oggetto negato non produce il 302.
- CORS: `allowCredentials: true` con un'origine `*` (o wildcard) è rifiutato al salvataggio; l'origine non viene
  mai riflessa per le regole wildcard.
- `If-Range` (RFC 9110 §13.1.5): il range si applica solo se il validatore coincide con l'ETag forte (un ETag
  debole non coincide mai) o con la data esatta di ultima modifica; altrimenti `200` con l'oggetto intero.

Chiavi di firma: **key group** con più chiavi (rotazione senza downtime), chiave privata generata dalla dashboard
e scaricabile una sola volta, oppure chiave pubblica caricata dall'utente. Generazione URL da CLI/dashboard/SDK.

### Risoluzione dell'host (routing)
Per ogni richiesta, l'header `Host` (o `X-Forwarded-Host` / `Forwarded host=` di un proxy fidato, solo con
`--trust-forwarded-host` / `NEXO_TRUST_FORWARDED_HOST=true`) viene risolto in quest'ordine:
1. alias esatto di una distribuzione;
2. alias wildcard di una distribuzione (match più specifico);
3. `<bucket>.<NEXO_DOMAIN>` (virtual-host S3);
4. dominio S3/console principale (`NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL`);
5. altrimenti: API S3 path-style (configurabile: oppure 421 Misdirected Request).

- Tabella di routing in memoria (mappa + trie per wildcard), aggiornata **a caldo** e propagata a tutti i nodi via peer RPC.
- Conflitti di alias (stesso dominio su due distribuzioni) rifiutati al salvataggio.
- **Domini riservati**: un alias (esatto o wildcard) che coincide con o copre i domini principali
  (`NEXO_SERVER_URL`, `NEXO_BROWSER_REDIRECT_URL`), un `NEXO_DOMAIN` o i virtual-host `<bucket>.<NEXO_DOMAIN>`
  è rifiutato (400); è ammesso solo il virtual-host del bucket di origine della distribuzione stessa (e chi la
  salva deve essere autorizzato su quel bucket). Gli alias di distribuzioni salvate prima di questa regola che la
  violano sono esclusi dalla tabella di routing al caricamento (log di warning), così un alias non dirotta mai
  l'endpoint S3 o i bucket di altri.
- Listener: le distribuzioni rispondono sulla porta S3 (9000) e, opzionalmente, su listener dedicati
  `--web-address :80` e `--web-tls-address :443` per l'esposizione diretta senza proxy.

### Dietro uno o più proxy
- `NEXO_TRUSTED_PROXIES` (CIDR): solo da questi indirizzi si accettano `X-Forwarded-For/Proto/Host`, `Forwarded` (RFC 7239)
  e **PROXY protocol v1/v2** (attivabile per listener).
- **Origin secret header** per distribuzione: ogni proxy invia un header segreto diverso; le richieste senza header
  (accesso diretto aggirando il proxy) vengono rifiutate. Più segreti attivi contemporaneamente per la rotazione.
- L'IP client usato nelle condizioni (`sourceIp`) è quello reale ricavato dagli header fidati.
- Esempi di configurazione nginx, Caddy, Traefik, HAProxy e Cloudflare in `deploy/proxy/`.

Scelte di implementazione (verificate da `test/proxy`, vedi §10.6 "Cluster e Docker"):
- **Host inoltrato non fidato per default**: anche da un proxy fidato si usano indirizzo e schema inoltrati, ma il
  dominio resta l'header `Host` (che tutte le configurazioni di `deploy/proxy` preservano). Alcuni proxy
  (Cloudflare, una `location` nginx che non lo imposta) inoltrano l'`X-Forwarded-Host` del client così com'è: un
  client potrebbe scegliere la distribuzione, le condizioni `nexo:host` e la destinazione dei redirect `httpsOnly`.
  `--trust-forwarded-host` serve solo per i proxy che riscrivono l'`Host`.
- **Una sola famiglia di header** per deployment (`NEXO_FORWARDED_HEADERS`: `x-forwarded`, default, oppure
  `forwarded`): leggerle entrambe permetterebbe a un client di falsificare il proprio indirizzo attraverso un
  proxy che imposta solo l'altra. `X-Forwarded-For` è letto da destra fino al primo indirizzo non fidato.
- **PROXY protocol per listener** (`NEXO_PROXY_PROTOCOL=s3,console,web`), letto prima del TLS: usato solo dai
  peer in `NEXO_TRUSTED_PROXIES`, letto e ignorato dagli altri, facoltativo anche per i proxy fidati (così
  un proxy con `X-Forwarded-*`, le RPC internode e gli health check condividono la porta S3 con un HAProxy
  `send-proxy-v2-ssl`); il TLV SSL rende la richiesta HTTPS (redirect `httpsOnly`, `aws:SecureTransport`).
- **Origin secret**: `restrictions.originSecretHeader` ha un nome (default `X-Origin-Secret`) e fino a 8 valori
  attivi insieme, tutti accettati da tutti i proxy della distribuzione. Un valore in chiaro è salvato come
  hash (`sha256:`); un riferimento `${secret:<nome>}` è risolto su ogni nodo da `NEXO_SECRET_<NOME>` (o
  `_FILE`, `-` e `_` equivalenti) e non finisce mai nella configurazione: è la forma consigliata con più proxy
  (`values: ["${secret:proxy-a}", "${secret:proxy-b}"]`, un segreto per proxy, YAML senza segreti per il
  GitOps). Un riferimento non risolvibile non accetta nessuna richiesta (fail closed). `dist origin-secret
  rotate` genera un nuovo valore (mostrato una volta) mantenendo attivi i precedenti (`--keep`). Il controllo
  vale in entrambe le modalità (website e s3) e precede ogni altra valutazione; le richieste S3 path-style e
  la console sui domini principali non sono distribuzioni e non lo richiedono (si proteggono con la rete).
- La firma degli URL di distribuzione (`dist sign-url`, `POST /distributions/{id}/sign`) e `dist test`
  riportano il behavior del path anche quando la richiesta di prova sarebbe rifiutata dall'ammissione
  (origin secret, indirizzo, distribuzione disabilitata): la firma di una distribuzione dietro proxy trova il
  suo key group.
- Un nodo di un cluster che si sta fermando risponde `503` finché gli health check non lo tolgono dal giro:
  i proxy devono ritentare le letture su 502/503/504 (configurazioni di `deploy/proxy`).

### TLS e certificati per dominio
- Certificati caricati (PEM) o **ACME automatico** (Let's Encrypt/ZeroSSL, challenge HTTP-01 o TLS-ALPN-01; DNS-01 P2 per wildcard).
- In cluster: certificati e stato ACME salvati cifrati in `.nexo.sys`, rinnovo eseguito dal leader, distribuzione a tutti i nodi.
- Selezione via **SNI**; scadenza visibile in dashboard con alert a 21 giorni.
- Un certificato è presentato solo per gli alias delle distribuzioni **abilitate** che lo referenziano
  (`tls.certificate: upload:<id>`, o il certificato ACME di `auto`) e solo se copre il nome e non è scaduto;
  altrimenti il listener usa il proprio certificato (quello statico del server per i domini S3/console). Un
  certificato caricato con un nome dei domini principali, di `NEXO_DOMAIN` o `*.<NEXO_DOMAIN>` (o una wildcard
  che li copre) è rifiutato: chi ha `admin:ManageCertificates` non può sostituire il certificato dell'endpoint S3.

### Cache e invalidazione
Gestione completa della cache **per distribuzione, per dominio e per percorso**: vedi [11 — Cache per dominio](11-cache-domini.md).

### Osservabilità per distribuzione
Metriche `nexo_distribution_requests_total{distribution,host,status}`, byte serviti, cache hit ratio, latenza;
access log in formato combinato o JSON; grafici dedicati in dashboard.

## 10.3 Dashboard

| Codice | Funzionalità |
|--------|--------------|
| D-ACC-01 | Bucket → tab **Regole di accesso**: tabella regole ordinabile (drag & drop della priorità), editor con scelta prefix/glob/regex, principali, azioni, condizioni; vista YAML/JSON |
| D-ACC-02 | **Tester di pattern**: inserisci una o più chiavi → evidenzia quali regole corrispondono e l'esito finale (allow/deny) per un'identità/IP/host scelti; esempi di chiavi esistenti nel bucket |
| D-ACC-03 | Preset pubblico (private / public-read / public-prefix…) con avvisi chiari e stato "Pubblico" visibile nella lista bucket |
| D-ACC-04 | Block public access a livello cluster e bucket |
| D-DST-01 | Sezione **Distribuzioni**: elenco con domini, bucket, stato certificati, traffico |
| D-DST-02 | Wizard di creazione: bucket/prefix → domini → modalità (website/s3) → accesso → TLS; istruzioni DNS (CNAME/A/AAAA) e verifica che il dominio punti al cluster |
| D-DST-03 | Editor behavior: lista ordinata con pattern (prefix/glob/regex), metodi, accesso, cache, header; tester "questo URL quale behavior usa?" |
| D-DST-04 | Error page, redirect, SPA fallback, CORS, header di sicurezza (preset consigliato) |
| D-DST-05 | Key group e generatore di URL/cookie firmati |
| D-DST-06 | Restrizioni: IP allow/deny, origin secret per proxy (generazione e rotazione), rate limit |
| D-DST-07 | Certificati: upload, ACME, stato e scadenze |
| D-DST-08 | Invalidazione cache e cronologia; metriche e log per distribuzione |

## 10.4 CLI

```bash
# Regole di accesso
nexoctl access rule add local/media --id img-pub --allow --principal '*' --action read \
        --regex '^img/[0-9]{4}/.*\.(jpe?g|png)$' --ignore-case
nexoctl access rule add local/media --id no-private --deny --principal '*' --action all --glob '**/private/**' --priority 5
nexoctl access rule ls|info|rm|move local/media [--id ...] [--priority N]
nexoctl access test local/media/img/2026/a.jpg --principal anonymous --ip 1.2.3.4 --host cdn.example.com
nexoctl access public set public-read local/media | access public set public-prefix local/media --glob 'pub/**'
nexoctl access block-public on|off [local | local/media]
nexoctl access export|import local/media rules.yaml

# Distribuzioni
nexoctl dist create local sito-web --bucket web --prefix /prod --mode website --domain www.example.com --domain example.com
nexoctl dist domain add|rm local sito-web '*.preview.example.com'
nexoctl dist behavior add local sito-web --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
nexoctl dist behavior add local sito-web --regex '^/downloads/.*\.zip$' --access signed --key-group rilasci
nexoctl dist error-page|redirect|headers|restrict ... 
nexoctl dist keygroup create local rilasci && nexoctl dist sign-url local sito-web /downloads/v1.zip --expire 1h [--ip 1.2.3.4]
nexoctl dist cert set local sito-web --auto | --cert crt.pem --key key.pem
nexoctl dist origin-secret rotate local sito-web
nexoctl dist test local sito-web https://www.example.com/assets/app.js   # mostra behavior, accesso, header risultanti
nexoctl dist invalidate local sito-web '/assets/**'
nexoctl dist ls|info|edit|disable|enable|rm|export|import
```
Tutti i comandi supportano `--json` e l'import/export YAML (gestione GitOps delle configurazioni).

## 10.5 Admin API

| Metodo | Path |
|--------|------|
| GET/PUT | `/api/v1/buckets/{b}/access-rules` (sostituzione atomica con `If-Match` su ETag della configurazione) |
| POST | `/api/v1/buckets/{b}/access-rules/test` |
| GET/PUT | `/api/v1/access/block-public`, `/api/v1/buckets/{b}/block-public` |
| GET/POST | `/api/v1/distributions` · GET/PUT/DELETE `/api/v1/distributions/{id}` |
| POST | `/api/v1/distributions/{id}/test` · `/invalidations` · `/origin-secret/rotate` |
| GET/POST/DELETE | `/api/v1/keygroups`, `/api/v1/keygroups/{id}/keys` · POST `/api/v1/distributions/{id}/sign` |
| GET/POST/DELETE | `/api/v1/certificates` |
| GET | `/api/v1/routing/resolve?host=` (diagnostica: quale distribuzione/bucket risponde per un host) |

Azioni di policy: `admin:ManageAccessRules`, `admin:ManageDistributions`, `admin:ManageCertificates`, `admin:SignDistributionURL`.
`admin:ManageDistributions` è valutata anche sul **bucket di origine** (risorsa `arn:aws:s3:::<bucket>`): per creare e
modificare una distribuzione (bucket attuale e nuovo), eliminarla, ruotarne l'origin secret e modificare le chiavi
di un key group (aggiunta, rimozione, eliminazione del gruppo), autorizzate su ogni bucket di origine delle
distribuzioni che firmano con quel gruppo. Un utente negato su un bucket non può liberarne gli alias né firmarne i
contenuti premium.

## 10.6 Test

### Server
| ID | Caso | Atteso |
|----|------|--------|
| TAR-01 | Matching prefix/glob/regex: tabella > 200 casi (segmenti, `**`, classi, alternative, case-insensitive, unicode, chiavi con `%`, spazi, `+`) | Esiti attesi |
| TAR-02 | Regex invalida, troppo lunga, oltre il limite di regole | Errore 400 con posizione |
| TAR-03 | Tentativi di bypass: `%2F`, doppio encoding, `./`, `//`, varianti Unicode NFC/NFD, maiuscole | Nessun bypass |
| TAR-04 | Regex patologiche (`(a+)+$` su input lungo) | Tempo lineare (< 1 ms su 1 KiB) |
| TAR-05 | Deny in access rule prevale su allow IAM e su bucket policy; allow da regola senza policy IAM | Conforme §10.1 |
| TAR-06 | Gruppi nominati con `${aws:username}` (home directory) | Ogni utente accede solo alla sua |
| TAR-07 | Condizioni IP (IPv4/IPv6, dietro proxy fidato e non fidato), referer, host, finestra temporale, header | Esiti attesi; IP da proxy non fidato ignorato |
| TAR-08 | Block public access attivo con regola `*` | Rifiutata / ignorata secondo configurazione |
| TAR-09 | Modifica regole su nodo1 → effetto su nodo4 | < 1 s |
| TAR-10 | Benchmark: 200 regole regex, valutazione per richiesta | p99 < 50 µs |
| TDS-01 | Routing host: alias esatti, wildcard, più specifico vince, virtual-host, dominio sconosciuto | Distribuzione/bucket corretti |
| TDS-02 | Alias duplicato su due distribuzioni | 409 |
| TDS-03 | Modalità website: index, SPA fallback, error page con status configurato, redirect regex con gruppi, HTTP→HTTPS, www | Risposte attese |
| TDS-04 | Modalità s3 su dominio custom: SDK con endpoint personalizzato, SigV4 e presigned generati con quel dominio | 200 |
| TDS-05 | Behavior: ordine, primo match vince, metodi non consentiti → 405/403, cache-control e header di risposta applicati | — |
| TDS-06 | URL e cookie firmati: validi, scaduti, firma alterata, IP diverso, risorsa wildcard, rotazione key group | 200 / 403 |
| TDS-07 | Origin secret: con header corretto OK, senza o errato 403, due segreti attivi durante la rotazione | — |
| TDS-08 | PROXY protocol v1/v2 e `Forwarded`/`X-Forwarded-*` solo da `NEXO_TRUSTED_PROXIES` | IP reale corretto, spoofing ignorato |
| TDS-09 | `pathPrefix` di origine: impossibile uscire dal prefix (`/../`, encoded) | 403/404 |
| TDS-10 | TLS: SNI con più certificati, ACME contro Pebble (ACME di test) inclusa la renewal, cluster con leader che rinnova | Certificato servito da tutti i nodi |
| TDS-11 | Cache e invalidazione: vedi test TCC-* in [11](11-cache-domini.md#1112-test) | — |
| TDS-12 | Range, conditional GET, HEAD, `Content-Type` corretti via distribuzione | Conforme |
| TDS-13 | Rate limit per IP (P2) | 429 oltre soglia |
| TDS-14 | Fuzzing di Host header, path e parametri di firma | Nessun panic/bypass |

### CLI
| ID | Caso |
|----|------|
| TCL-A-01 | `access rule add/ls/rm/move` + `access test` coerente con richieste reali via curl |
| TCL-A-02 | `access export` → `import` su altro bucket: regole identiche |
| TCL-D-01 | `dist create` con 2 domini → `curl -H 'Host: ...'` su entrambi restituisce il contenuto |
| TCL-D-02 | `dist sign-url` → curl OK, dopo scadenza 403 |
| TCL-D-03 | `dist test` mostra behavior e header attesi |
| TCL-D-04 | `dist origin-secret rotate`, `dist invalidate`, `dist cert set --cert/--key` |

### Dashboard
| ID | Caso |
|----|------|
| TD-A-01 | Crea regola regex con il tester: chiavi di esempio evidenziate correttamente; regex invalida bloccata con messaggio |
| TD-A-02 | Riordino priorità con drag & drop persiste ed è rispettato dal server |
| TD-A-03 | Preset public-prefix → oggetto accessibile anonimo solo nel prefix; badge "Pubblico" nella lista bucket |
| TD-D-01 | Wizard distribuzione con 2 domini + istruzioni DNS; verifica raggiungibilità (in test: host risolti verso il server) |
| TD-D-02 | Editor behavior + tester URL; SPA fallback e error page funzionanti da browser sul dominio configurato |
| TD-D-03 | Key group: chiave privata mostrata una volta; URL firmato generato apre il file |
| TD-D-04 | Certificato in scadenza evidenziato; upload certificato valido/invalido |
| TD-D-05 | Utente senza `admin:ManageDistributions` non vede/modifica le distribuzioni |

### Cluster e Docker
| ID | Caso |
|----|------|
| TCD-01 | Compose con **2 proxy nginx** (domini diversi, origin secret diversi) davanti al cluster 4 nodi: entrambi i punti di accesso funzionano, accesso diretto ai nodi senza secret rifiutato |
| TCD-02 | Nodo che serve una distribuzione viene fermato: il proxy ripiega sugli altri nodi senza errori |
| TCD-03 | Creazione distribuzione dalla dashboard → attiva su tutti i nodi senza restart |

Implementazione di TCD-01..03 (`test/proxy`, build tag `proxy`, `make test-proxy`, job CI `proxy`): i due
proxy sono **nginx** (`X-Forwarded-*`) e **HAProxy** (PROXY protocol v2 con TLV SSL) invece di due nginx, per
coprire entrambi i modi di descrivere il client (`deploy/docker/compose.multi-proxy.yml`). Senza Docker:
- `TestMultiProxy` (SNSD, SNMD, cluster 4×2 in-process): due proxy Go (`httputil.ReverseProxy`) con TLS, dominio
  e origin secret propri, bilanciamento e failover come nginx; distribuzioni create e modificate dall'Admin API
  della console dietro il proxy A e servite subito da ogni nodo (decine di ms misurate, limite del test 3 s) (TCD-03); accesso diretto ai nodi senza
  secret o con secret errato → 403, con secret ma senza HTTPS → redirect (TCD-01); IP reale e spoofing
  (`X-Forwarded-For`, `Forwarded`, PROXY header di un peer non fidato) attraverso entrambi (TDS-08); nodo
  fermato sotto carico → nessun errore client né 502, scritture e modifiche della distribuzione durante il
  fermo, il nodo riavviato le serve (TCD-02); URL firmati di distribuzione con `IpAddress` del client reale;
  TPS-19 sui domini custom (TCC-C-05 in [11](11-cache-domini.md#1112-test)).
- `TestRealProxies` (cluster 4×2): gli **stessi file** nginx e HAProxy del compose, adattati agli indirizzi di
  loopback, con i binari reali (saltato se mancano, obbligatorio con `NEXO_PROXY_REAL=1`), più le distribuzioni
  d'esempio `multi-proxy/site.yaml`.
