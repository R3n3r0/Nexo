# Trace, log e audit

| Strumento | Cosa mostra | Dove |
|-----------|-------------|------|
| Trace live | ogni richiesta completata, di tutti i nodi | `nexoctl admin trace`, console **Monitoraggio → Trace** |
| Log live | i log del server di tutti i nodi | `nexoctl admin logs`, console **Monitoraggio → Log** |
| Audit | un record per richiesta autenticata: chi, cosa, quando, esito | log JSON, ricerca in memoria, target webhook e Kafka |
| OpenTelemetry | span distribuiti per richiesta e chiamata tra nodi | un collector OTLP |

In cluster trace, log e audit sono **aggregati da tutti i nodi**: basta collegarsi a uno qualsiasi.

## Trace live

```console
$ nexoctl admin trace local
Tracing the requests of "local" (Ctrl-C to stop).
2026-10-15 11:51:14.442 local s3    ListObjectsV2  200  0.47ms  GET /foto?delimiter=%2F&list-type=2  rx 0B tx 460B  user admin  from 127.0.0.1
2026-10-15 11:51:14.452 local s3    GetObject      200  0.69ms  GET /foto/hello.txt?x-id=GetObject  rx 0B tx 10B  user admin  from 127.0.0.1
2026-10-15 11:51:14.462 local s3    HeadObject     404  0.36ms  HEAD /foto/missing  user admin  from 127.0.0.1  error: Not Found
```

Una riga per richiesta: ora, nodo, tipo (`s3`, `admin`, `web`, `rpc`), operazione, stato, durata, metodo e
percorso, byte ricevuti e inviati, utente, client, errore. Filtri utili:

```bash
nexoctl admin trace --errors local                                  # solo status >= 400
nexoctl admin trace --status 5xx,403 local
nexoctl admin trace --api PutObject,GetObject --bucket foto local
nexoctl admin trace --min-duration 500ms local                      # solo le richieste lente
nexoctl admin trace --node nexo2.lan:9000 local                     # un solo nodo
nexoctl admin trace --type s3,admin,rpc local                       # anche le chiamate tra nodi
nexoctl admin trace --timeout 1m --json local > trace.jsonl         # JSON Lines, per 1 minuto
```

- Header e corpo delle richieste non sono mai pubblicati; firme, credenziali, token e password nella query sono
  redatti, insieme ai nomi usuali delle credenziali dei siti serviti dalle distribuzioni (`apikey`, `api_key`,
  `api-key`, `x-api-key`, `auth`, `sig`, `jwt`, `session`, `sid`). Gli altri parametri applicativi restano in chiaro:
  non passare segreti in parametri con altri nomi.
- Senza client collegati il costo è nullo; un client lento perde eventi (segnalati come `dropped`) ma non
  rallenta mai le richieste.
- Lo stream si riconnette da solo se il server si riavvia; termina con Ctrl-C, `--timeout` o `--count`.
- Un nodo serve al massimo 256 stream live di trace e 256 di log alla volta (le schede della console, la CLI
  e gli stream che gli altri nodi aprono per loro): oltre, un nuovo stream è rifiutato con 429
  `too_many_requests` finché uno non si chiude.
- Richiede `admin:ServerTrace` (root, `consoleAdmin`, `diagnostics`).

## Log live

I log del server vanno su stderr (JSON di default, `--log-format text` per il testo; con systemd finiscono
nel journal, con Docker in `docker logs`). Per seguirli da remoto, aggregati da tutti i nodi:

```bash
nexoctl admin logs local                          # le ultime 10 righe di ogni nodo, poi le nuove
nexoctl admin logs --level warn --last 100 local
nexoctl admin logs --node nexo2.lan:9000 --json local
```

Livello e formato si cambiano **a caldo** su tutto il deployment, senza riavvio:

```bash
nexoctl admin config set local logger level=debug
nexoctl admin config reset local logger          # torna a --log-level / --log-format
```

Password, token, firme, cookie e chiavi SSE-C sono redatti dai log. Richiede `admin:ConsoleLog`.

## Audit log

Ogni richiesta S3 e della console produce un record di audit con identità (access key e utente), API,
bucket e oggetto, IP del client, esito, latenza, byte, request id e nodo. Fanno eccezione le sonde di
salute anonime (`/nexo/health/*` senza credenziali) e le letture anonime riuscite delle metriche
(`/nexo/metrics/*` con `NEXO_PROMETHEUS_AUTH_TYPE=public`): un load balancer o i probe di Kubernetes le ripetono
ogni pochi secondi e riempirebbero l'anello in memoria espellendo i record utili. Restano registrati uno scrape
autenticato (JWT), uno scrape rifiutato (token assente, scaduto o falso) e ogni richiesta che presenta
credenziali (firma SigV4 o presigned, token), anche verso `/nexo/health/*` e anche se rifiutata: un tentativo
di provare credenziali lascia sempre traccia. I record:

1. finiscono nel log del server (`"msg":"audit"`, componente `audit`; `time` è l'ora del record, `event_time`
   quella della richiesta) con `NEXO_AUDIT_LOG=on` (`--audit-log`) o al livello di log `debug`. Disattivato
   per default: una riga di log per richiesta costa throughput, molto su una console Windows;
2. restano in memoria su ogni nodo (gli ultimi 10000) per la ricerca dalla console (**Monitoraggio →
   Audit**) e dalla CLI;
3. sono consegnati ai **target di audit** configurati, con coda persistente su disco se il target non
   risponde.

### Ricerca

```console
$ nexoctl admin audit --limit 5 local
TIME                    STATUS  DURATION  USER     API            RESOURCE
2026-10-15 11:51:14.464 200       0.29ms  admin    ListObjectsV2  foto
2026-10-15 11:51:14.462 404       0.37ms  admin    HeadObject     foto/missing  error: Not Found
2026-10-15 11:51:14.452 200       0.72ms  admin    GetObject      foto/hello.txt
```

```bash
nexoctl admin audit --user alice --since 1h local
nexoctl admin audit --action DeleteObject --bucket foto --since 2026-10-15T09:00:00Z local
nexoctl admin audit --errors --status 403 --limit 1000 --json local
nexoctl admin audit --request-id 8F1B7F3FB3152251 local
```

La ricerca copre solo i record recenti in memoria: per la conservazione a lungo termine configura un target.

### Target di audit (webhook e Kafka)

I target sono sottosistemi della configurazione dinamica, applicati a caldo. Il webhook riceve un
documento JSON per record (POST):

```bash
nexoctl admin config set local audit_webhook \
  enable=on endpoint=https://siem.example.com/nexo auth_token=s3cr3t     # inviato come "Authorization: Bearer s3cr3t"
```

Kafka riceve un messaggio per record, con chiave `<nodo>/<request id>`:

```bash
nexoctl admin config set local audit_kafka \
  enable=on brokers=kafka1:9092,kafka2:9092 topic=nexo-audit \
  tls=on sasl=on sasl_mechanism=sha512 sasl_username=nexo sasl_password='…'
```

Chiavi comuni: `queue_dir` (sottodirectory della coda persistente, relativa alla radice delle code sul
primo disco locale; percorsi assoluti e `..` sono rifiutati), `queue_limit` (default 100000 record),
`comment`. Per più target dello stesso tipo usa un id: `audit_webhook:siem`, `audit_webhook:backup`. Le
credenziali sono mascherate in `admin config get` e nei log.

## OpenTelemetry

Con un collector OTLP, ogni richiesta S3, della console, di una distribuzione o tra nodi produce uno span
server, e ogni chiamata tra nodi uno span client che propaga il contesto (`traceparent` W3C). Gli attributi
non contengono segreti (metodo, percorso senza query, stato, API, bucket, request id, client).

```bash
NEXO_OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
NEXO_OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
NEXO_OTEL_TRACES_SAMPLER_ARG=0.1 \
nexo server /srv/nexo
```

| Variabile | Default | Note |
|-----------|---------|------|
| `NEXO_OTEL_EXPORTER_OTLP_ENDPOINT` | — (disattivato) | base URL per `http/protobuf` (`/v1/traces` aggiunto se manca), `host:porta` per `grpc` |
| `NEXO_OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` | oppure `grpc` |
| `NEXO_OTEL_TRACES_SAMPLER_ARG` | `1` | quota delle tracce avviate dal nodo; una richiesta con `traceparent` campionato è sempre registrata |

Valgono anche `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TIMEOUT` e `OTEL_EXPORTER_OTLP_CERTIFICATE`.
Disattivato, il tracing non ha costo. Con Helm: `config.otel.*` in `values.yaml`.
