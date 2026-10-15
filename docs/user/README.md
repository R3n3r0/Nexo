# Documentazione utente di Nexo

**Nexo** è un object storage compatibile con l'API **Amazon S3**: un solo binario (`nexo`) contiene il
server S3, l'Admin API e la console web; `nexoctl` è la CLI per i dati e l'amministrazione. Lo stesso
binario gira su un singolo disco, su più dischi con erasure coding e in cluster multi-nodo.

Questa guida è per chi installa, usa e gestisce Nexo. I documenti di progetto (architettura, roadmap,
piano di test) sono in [`docs/`](https://github.com/R3n3r0/Nexo/tree/main/docs) del repository.

## Da dove partire

| Se vuoi… | Leggi |
|----------|-------|
| provare Nexo in 5 minuti | [Quickstart](quickstart.md) |
| installarlo su un server, in Docker o su Kubernetes | [Installazione](installazione.md) |
| scegliere tra singolo disco, più dischi e cluster | [Modalità di deployment](modalita.md) |
| conoscere ogni flag e variabile d'ambiente | [Configurazione](configurazione.md) |

## Guida operativa

| Argomento | Pagina |
|-----------|--------|
| Health check, metriche Prometheus, Grafana, alert | [Monitoraggio](operazioni/monitoraggio.md) |
| Trace e log live, audit log, OpenTelemetry | [Trace, log e audit](operazioni/trace-log-audit.md) |
| Healing, sostituzione di un disco | [Healing e dischi](operazioni/healing-dischi.md) |
| Aggiornamento rolling, espansione con un nuovo pool | [Aggiornamento ed espansione](operazioni/aggiornamento-espansione.md) |
| Backup della configurazione, migrazione tra modalità | [Backup e migrazione](operazioni/backup-migrazione.md) |
| Quorum, orologi sfasati, dischi lenti, permessi | [Risoluzione dei problemi](operazioni/troubleshooting.md) |

## Sicurezza

| Argomento | Pagina |
|-----------|--------|
| Utenti, gruppi, policy (esempi pronti), access key, STS | [Utenti, policy e credenziali](sicurezza/utenti-policy.md) |
| Share link, URL presigned, POST policy, accesso anonimo | [Condivisione e accesso anonimo](sicurezza/condivisione.md) |
| Regole di accesso con prefix, glob e regex | [Regole di accesso](sicurezza/regole-accesso.md) |
| TLS, certificati, mTLS tra i nodi | [TLS e certificati](sicurezza/tls.md) |
| KMS, cifratura lato server, rotazione delle chiavi | [KMS e SSE](sicurezza/kms-sse.md) |
| Minacce e mitigazioni | [Threat model](sicurezza/threat-model.md) |

## Funzionalità

| Argomento | Pagina |
|-----------|--------|
| Domini personalizzati, behavior, URL firmati, cache | [Distribuzioni e cache](distribuzioni/README.md) |
| Sito statico su due domini dietro nginx con TLS ACME | [Tutorial: sito statico](distribuzioni/tutorial-sito-statico.md) |
| Notifiche verso webhook e Kafka, regole di lifecycle | [Eventi e lifecycle](eventi-lifecycle.md) |

## Riferimenti

| Riferimento | Pagina |
|-------------|--------|
| Tutti i comandi di `nexoctl` (generati dal codice) | [Reference CLI](riferimenti/cli.md) |
| Admin API (`/api/v1`, OpenAPI 3.1) | [Reference Admin API](riferimenti/admin-api.md) |
| API S3 supportate, differenze, risultati di `s3-tests` | [Compatibilità S3](riferimenti/compatibilita-s3.md) |

## Convenzioni

- Gli esempi usano l'alias `local` (vedi [Quickstart](quickstart.md#4-configura-nexoctl)), l'utente root
  `admin` e la password `supersegreta`: in produzione usa una password lunga e casuale.
- Le porte di default sono **9000** (API S3, health, metriche, RPC tra i nodi) e **9001** (console e
  Admin API).
- Le pagine indicano quando un comportamento dipende dalla modalità; in assenza di note vale per
  **SNSD**, **SNMD** e **cluster**.
- Il sito di questa documentazione si costruisce con `make docs-site` (MkDocs Material, vedi
  [Reference CLI](riferimenti/cli.md#generare-la-documentazione)).
