# Tutorial: un sito statico su due domini dietro nginx

In questo tutorial pubblichi un sito statico su `www.example.com` e `www.example.it` (più il dominio nudo
`example.com`, rediretto su `www`), con:

- certificati TLS ottenuti e rinnovati automaticamente da Nexo via ACME (Let's Encrypt);
- nginx davanti, che ospita anche altri siti sulla stessa macchina;
- redirect HTTP → HTTPS, pagina 404, header di sicurezza;
- asset con cache di un anno, un'area `/premium/` accessibile solo con URL firmati;
- cache dei contenuti e invalidazione dopo un deploy.

Tutti i comandi sono stati provati con i binari di questo repository, con un server ACME di test (Pebble)
al posto di Let's Encrypt e nginx 1.24.

## Architettura

```text
                         ┌──────────────────── nginx ─────────────────────┐
 browser ── :80  ──────▶ │ http  { server_name www.example.com ... }      │ ──▶ Nexo :8080 (web, HTTP)
                         │        X-Forwarded-For / X-Forwarded-Proto      │
 browser ── :443 ──────▶ │ stream { ssl_preread: SNI dei domini del sito } │ ──▶ Nexo :8443 (web, TLS)
                         │        TCP passthrough + PROXY protocol         │
                         │        altri SNI ─▶ gli altri siti di nginx     │
                         └─────────────────────────────────────────────────┘
```

- Su **443** nginx non termina TLS per i domini del sito: li inoltra a Nexo (TCP passthrough, scelto in base
  all'SNI) aggiungendo il **PROXY protocol**, così Nexo conosce l'IP reale del client. È Nexo a presentare i
  certificati ottenuti via ACME.
- Su **80** nginx inoltra le richieste HTTP (redirect verso HTTPS e challenge ACME HTTP-01) con gli header
  `X-Forwarded-*`.
- Nexo serve le distribuzioni su due listener dedicati (`--web-address`, `--web-tls-address`) in ascolto solo
  su `127.0.0.1`: l'API S3 e la console restano dove sono.

> **Alternativa**: se preferisci che sia nginx a terminare TLS (con certbot), usa
> [`deploy/proxy/nginx.conf`](https://github.com/R3n3r0/Nexo/blob/main/deploy/proxy/nginx.conf): le
> distribuzioni sono servite sulla porta S3 e protette con un *origin secret*. Il resto del tutorial
> (distribuzione, behavior, cache) non cambia; salta solo la parte ACME (`dist cert set --none`).

## 1. DNS

Punta tutti i domini all'indirizzo pubblico della macchina con nginx:

```text
www.example.com.   300  IN  A     203.0.113.10
example.com.       300  IN  A     203.0.113.10
www.example.it.    300  IN  A     203.0.113.10
```

Le porte 80 e 443 devono essere raggiungibili da Internet: Let's Encrypt verifica il controllo dei domini
con una richiesta HTTP sulla porta 80.

## 2. Configurare Nexo

Aggiungi all'ambiente del servizio (`/etc/default/nexo` con il pacchetto deb/rpm, o il file env di Docker):

```bash
NEXO_WEB_ADDRESS=127.0.0.1:8080        # distribuzioni in HTTP e challenge ACME HTTP-01
NEXO_WEB_TLS_ADDRESS=127.0.0.1:8443    # distribuzioni in HTTPS, certificati per SNI
NEXO_PROXY_PROTOCOL=web                # i listener web accettano il PROXY protocol...
NEXO_TRUSTED_PROXIES=127.0.0.1         # ...e gli X-Forwarded-* solo da nginx
NEXO_ACME_EMAIL=ops@example.com        # contatto dell'account Let's Encrypt
```

e riavvia (`sudo systemctl restart nexo`). Senza `NEXO_ACME_DIRECTORY` Nexo usa la directory di produzione di
Let's Encrypt; per le prove puoi usare quella di staging
(`NEXO_ACME_DIRECTORY=https://acme-staging-v02.api.letsencrypt.org/directory`), che ha limiti più larghi ma
certificati non fidati dai browser.

Il listener che riceve il PROXY protocol continua ad accettare connessioni senza (le richieste HTTP di nginx
sulla porta 80); un header PROXY che arriva da un indirizzo fuori da `NEXO_TRUSTED_PROXIES` viene ignorato.

> **In cluster**: imposta le stesse variabili su tutti i nodi (con l'indirizzo privato del nodo al posto di
> `127.0.0.1`) e metti tutti i nodi negli `upstream` di nginx. I certificati ACME sono salvati cifrati nel
> cluster e il rinnovo è eseguito da un solo nodo; la challenge HTTP-01 può arrivare a qualsiasi nodo.

## 3. Configurare nginx

Il blocco `stream` richiede il modulo stream di nginx (pacchetto `libnginx-mod-stream` su Debian/Ubuntu,
incluso in `nginx` su RHEL e nei pacchetti di nginx.org) e va nel file principale `/etc/nginx/nginx.conf`,
**fuori** dal blocco `http`:

```nginx
# /etc/nginx/nginx.conf (livello principale, accanto a "http { ... }")
stream {
    # I domini del sito vanno a Nexo, tutto il resto agli altri siti di nginx.
    map $ssl_preread_server_name $sni_backend {
        www.example.com  nexo_web_tls;
        www.example.it   nexo_web_tls;
        example.com      nexo_web_tls;
        default          altri_siti;
    }
    upstream nexo_web_tls { server 127.0.0.1:8443; }
    upstream altri_siti   { server 127.0.0.1:4443; }

    server {
        listen 443;
        listen [::]:443;
        ssl_preread    on;
        proxy_pass     $sni_backend;
        proxy_protocol on;
    }
}
```

Con `proxy_protocol on` anche gli altri siti ricevono il PROXY protocol: i loro blocchi `server` HTTPS
ascoltano quindi su `127.0.0.1:4443` invece che su 443, con `listen 127.0.0.1:4443 ssl proxy_protocol;` e
`set_real_ip_from 127.0.0.1; real_ip_header proxy_protocol;` per conservare l'IP dei client.

Per la porta 80, un normale blocco `server` (per esempio `/etc/nginx/conf.d/sito.conf`):

```nginx
upstream nexo_web {
    server 127.0.0.1:8080;
    keepalive 16;
}

server {
    listen 80;
    listen [::]:80;
    server_name www.example.com www.example.it example.com;

    location / {
        proxy_pass         http://nexo_web;
        proxy_http_version 1.1;
        proxy_set_header   Connection        "";
        proxy_set_header   Host              $http_host;
        proxy_set_header   X-Forwarded-For   $remote_addr;
        proxy_set_header   X-Forwarded-Proto $scheme;
        proxy_set_header   Forwarded         "";
    }
}
```

- `Host` va inoltrato invariato: Nexo sceglie la distribuzione in base al dominio.
- Nexo legge una sola famiglia di header inoltrati (`X-Forwarded-*` per default): l'header `Forwarded` di un
  client va eliminato.

```bash
sudo nginx -t && sudo systemctl reload nginx
```

## 4. Caricare il sito

```console
$ nexoctl mb local/sito
Bucket created successfully `local/sito`.
$ nexoctl mirror public/ local/sito
```

dove `public/` è la cartella generata dal tuo static site generator, per esempio:

```text
public/
├── index.html
├── 404.html
├── assets/app.css
└── premium/guida.pdf
```

Il bucket resta **privato**: è la distribuzione a decidere cosa è pubblico sui domini.

## 5. Creare la distribuzione

```console
$ nexoctl dist create local sito --bucket sito --mode website \
    --domain www.example.com --domain www.example.it --domain example.com --index index.html
Created the distribution `sito` (website) of `sito` on www.example.com, www.example.it, example.com.
$ nexoctl dist error-page set local sito 404 /404.html
Errors 404 of `sito` answer /404.html.
$ nexoctl dist redirect add local sito --host-from example.com --to-host www.example.com
Added the redirection 1 of `sito`: host example.com → www.example.com (301).
$ nexoctl dist redirect add local sito --https-only
Added the redirection 2 of `sito`: HTTP → HTTPS (301).
$ nexoctl dist headers local sito --security
Updated the headers of `sito` (4 response headers).
```

- `--index index.html` serve `index.html` per i percorsi che finiscono con `/` (e `/chi-siamo` diventa
  `/chi-siamo/` se esiste `chi-siamo/index.html`);
- la pagina 404 sostituisce l'errore per gli oggetti mancanti (per una single page application usa invece
  `--spa /index.html`);
- i redirect si applicano in ordine, vince il primo: qui il dominio nudo va su `www` e ogni richiesta HTTP
  passa a HTTPS (`301`);
- `--security` aggiunge `Strict-Transport-Security`, `X-Content-Type-Options`, `X-Frame-Options` e
  `Referrer-Policy`.

## 6. Certificati automatici

```console
$ nexoctl dist cert set local sito --auto
$ nexoctl dist cert list local
ID                       SOURCE  STATUS         EXPIRES                 DOMAINS                  USED BY
acme-sito                acme    valid          2027-01-13 12:10:32 UTC example.com,www.example.com,www.example.it sito
```

In pochi secondi Nexo ottiene un certificato con tutti i domini della distribuzione (nei log:
`certs: ordering an ACME certificate` e poi `certs: ACME certificate obtained`). Il rinnovo è automatico
prima della scadenza; aggiungendo o togliendo un dominio (`nexoctl dist domain add|remove`) il certificato
viene richiesto di nuovo con i nomi aggiornati. Lo stato è anche nella console (**Distribuzioni** →
**Certificati**) e nella metrica `nexo_tls_certificate_expiry_seconds`.

Se la richiesta fallisce (`STATUS` diverso da `valid`), il motivo è nei log del server: di solito il DNS non
punta ancora alla macchina o la porta 80 non arriva a Nexo. Con `NEXO_ACME_CHALLENGE=tls-alpn-01` la verifica
avviene invece sulla porta 443 (funziona grazie al passthrough di nginx) e la porta 80 non serve.

## 7. Verificare

```console
$ curl -sI https://www.example.com/
HTTP/2 200
content-type: text/html; charset=utf-8
etag: "a364651c001eb0c6e796a39165827d26"
referrer-policy: strict-origin-when-cross-origin
server: Nexo
strict-transport-security: max-age=63072000; includeSubDomains
x-cache: MISS
x-content-type-options: nosniff
x-frame-options: SAMEORIGIN
$ curl -sI http://www.example.com/chi-siamo | grep -i '^HTTP\|^location'
HTTP/1.1 301 Moved Permanently
Location: https://www.example.com/chi-siamo
$ curl -sI https://example.com/x | grep -i '^HTTP\|^location'
HTTP/2 301
location: https://www.example.com/x
$ curl -s https://www.example.it/non-esiste
<!doctype html><title>404</title><h1>Pagina non trovata</h1>
```

`nexoctl dist test` mostra come verrebbe servito un percorso senza fare la richiesta, e `dist resolve` quale
distribuzione risponde a un dominio:

```console
$ nexoctl dist resolve local www.example.it
www.example.it: distribution `sito` (alias www.example.it, website), bucket sito
```

## 8. Asset con cache lunga

Se i file in `assets/` hanno nomi con un hash del contenuto (`app.3f9a1c.css`), possono restare in cache un
anno, nei browser e in Nexo:

```console
$ nexoctl dist behavior add local sito --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
$ nexoctl dist cache set local sito --policy standard
Cache of `sito`, distribution: policy standard.
$ nexoctl dist cache set local sito --behavior '/assets/**' --policy immutable-assets
Cache of `sito`, behavior glob /assets/**: policy immutable-assets.
```

I **behavior** si applicano ai percorsi che corrispondono al pattern (il primo che corrisponde vince) e
ereditano il resto dal behavior di default. La cache di Nexo conserva le risposte in memoria (e su disco con
`NEXO_CACHE_DIR`): l'header `X-Cache` dice se una risposta arriva dalla cache (`HIT`) o dall'origine (`MISS`).

```console
$ nexoctl cache inspect local https://www.example.com/assets/app.css
URL: https://www.example.com/assets/app.css
...
Policy: immutable-assets (behavior)
TTL: default 365d (behavior), min 30d, max 365d
Cache-Control: public, max-age=31536000, immutable
Node local: cached in memory (object), age 0s, fresh for 31535999s, 17B
```

## 9. Contenuti riservati con URL firmati

L'area `/premium/` è servita solo con un URL (o un cookie) firmato, in stile CloudFront. Crea un **key group**:
la chiave privata generata dal server viene mostrata una sola volta e `nexoctl` la salva nella sua
configurazione.

```console
$ nexoctl dist keygroup create local premium
Created the key group `premium` with the key KZONJWDFG2WYLS (ed25519); private key saved to /home/ops/.nexoctl/keys/local/KZONJWDFG2WYLS.pem.
$ nexoctl dist behavior add local sito --glob '/premium/**' --access signed --key-group premium
Added a behavior for glob /premium/** to `sito` (2 behaviors).
$ curl -s -o /dev/null -w '%{http_code}\n' https://www.example.com/premium/guida.pdf
403
$ nexoctl dist sign-url local sito /premium/guida.pdf --expire 1h
https://www.example.com/premium/guida.pdf?Expires=1792069683&Signature=rjZU2T2I...&Key-Pair-Id=KZONJWDFG2WYLS
$ curl -s -o /dev/null -w '%{http_code}\n' 'https://www.example.com/premium/guida.pdf?Expires=1792069683&Signature=rjZU2T2I...&Key-Pair-Id=KZONJWDFG2WYLS'
200
```

- `--ip 203.0.113.7` (o un CIDR) limita l'URL a un client; `--not-before` ne ritarda la validità;
- `--cookie --resource 'https://www.example.com/premium/*'` firma invece tre cookie `CloudFront-*` validi per
  tutti i file dell'area, da impostare dalla tua applicazione dopo il login dell'utente;
- la firma copre l'URL completo, porta compresa: se il sito risponde su una porta diversa da 443, passa
  `--host www.example.com:PORTA`;
- la tua applicazione può firmare da sé con la chiave privata (formato compatibile con gli SDK CloudFront) o
  chiedere la firma all'Admin API (`POST /api/v1/distributions/{id}/sign`, permesso
  `admin:SignDistributionURL`);
- per ruotare le chiavi: `nexoctl dist keygroup key add local premium`, aggiorna chi firma, poi
  `nexoctl dist keygroup key remove local premium VECCHIA_KEYID` (le firme della chiave rimossa smettono
  subito di essere valide).

## 10. Aggiornare il sito e invalidare la cache

Quando un oggetto del bucket cambia, Nexo scarta da solo le voci in cache di quell'oggetto, su tutti i nodi
(invalidazione automatica, attiva per default):

```console
$ nexoctl mirror --overwrite public/ local/sito
$ curl -sI https://www.example.com/ | grep -i x-cache
x-cache: MISS
```

Per invalidare a mano (per esempio dopo aver cambiato le impostazioni o per contenuti che dipendono da più
oggetti), da una pipeline di deploy che ha solo `admin:InvalidateCache`:

```console
$ nexoctl dist invalidate local sito '/assets/**' --wait
Invalidation inv-18deb2a317f89272-b0784049 (distribution sito, glob /assets/**): completed on 1/1 node, 0 entries removed.
$ nexoctl cache purge local sito /index.html --domain www.example.com
Invalidation inv-18deb2a318ce0c49-3c3c374e (distribution sito, domain www.example.com, path /index.html): completed on 1/1 node, 1 entry removed.
$ nexoctl cache warm local sito --prefix /assets/ --wait
Warm-up warm-18deb2ab7ae2bf2b-32b4ead8 of `sito`: completed, 1/1 objects.
```

`nexoctl cache stats local` riporta hit ratio, byte serviti dalla cache e gli URL più richiesti per dominio.

## 11. Log di accesso e GitOps

La configurazione completa della distribuzione si esporta in YAML, da versionare insieme al sito:

```console
$ nexoctl dist export local sito -o sito.yaml
Exported 1 distribution to sito.yaml.
$ nexoctl dist cache export local sito > sito-cache.yaml
```

Il log di accesso si attiva nel YAML (sezione `logging`) e si applica con `dist import`:

```yaml
logging:
  accessLog: true
  format: combined      # oppure json (default)
```

```console
$ nexoctl dist import local sito.yaml
Distribution `sito` updated.
```

Ogni richiesta finisce nel log del server con il componente `distribution-access` e l'IP reale del client
ricevuto con il PROXY protocol:

```text
{"level":"INFO","msg":"203.0.113.7 - - [15/Oct/2026:12:09:31 +0000] \"GET / HTTP/2.0\" 200 54 \"-\" \"Mozilla/5.0 ...\"","component":"distribution-access","distribution":"sito"}
```

## Riepilogo dei comandi

```bash
nexoctl mb local/sito
nexoctl mirror public/ local/sito
nexoctl dist create local sito --bucket sito --mode website \
  --domain www.example.com --domain www.example.it --domain example.com --index index.html
nexoctl dist error-page set local sito 404 /404.html
nexoctl dist redirect add local sito --host-from example.com --to-host www.example.com
nexoctl dist redirect add local sito --https-only
nexoctl dist headers local sito --security
nexoctl dist cert set local sito --auto
nexoctl dist behavior add local sito --glob '/assets/**' --cache-control 'public, max-age=31536000, immutable'
nexoctl dist cache set local sito --policy standard
nexoctl dist cache set local sito --behavior '/assets/**' --policy immutable-assets
nexoctl dist keygroup create local premium
nexoctl dist behavior add local sito --glob '/premium/**' --access signed --key-group premium
nexoctl dist sign-url local sito /premium/guida.pdf --expire 1h
```

Per i concetti (modalità, behavior, accessi, cache, proxy) vedi la [panoramica delle distribuzioni](README.md).
