# Nexo

**Nexo** è un object storage compatibile con l'API **Amazon S3**, scritto in Go. Un solo binario
(`nexo`) contiene il server S3, l'Admin API e la console web; `nexoctl` è la CLI per i dati e
l'amministrazione. Lo stesso binario gira su un disco, su più dischi con erasure coding e in cluster
multi-nodo, senza database di metadati esterni.

- **API S3**: SigV4, bucket, oggetti, multipart, versioning, Object Lock, lifecycle, notifiche, CORS,
  checksum, cifratura SSE-S3/SSE-KMS/SSE-C, URL presigned e POST policy; superati tutti i 451 test applicabili
  della suite Ceph `s3-tests` e i 638 delle suite SDK di MinIO `mint`, in singolo disco, più dischi e cluster.
- **Durabilità**: erasure coding Reed-Solomon con parità configurabile, protezione dal bitrot, healing
  automatico, sostituzione dei dischi a caldo, espansione con nuovi pool.
- **Sicurezza**: utenti, gruppi e policy IAM, access key con scadenza, STS, regole di accesso con glob e
  regex, link di condivisione revocabili, TLS con ricarica a caldo, mTLS tra i nodi, KMS integrato o KES,
  audit log.
- **Distribuzioni**: un bucket servito su domini personalizzati in stile CloudFront, con certificati ACME,
  URL firmati, cache in memoria e su disco con invalidazione automatica.
- **Operatività**: console web, metriche Prometheus con dashboard Grafana e alert pronti, trace e log in
  tempo reale, OpenTelemetry, pacchetti deb/rpm, Homebrew, immagini Docker, Helm chart, unit systemd.

## Avvio rapido

Con Docker:

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersegreta \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo:latest server /data
```

Con il binario ([release](https://github.com/R3n3r0/Nexo/releases) o `make build`):

```bash
export NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersegreta
nexo server ~/nexo-data          # percorso assoluto; più dischi: nexo server /mnt/disk{1...4}
```

Poi apri la console su <http://localhost:9001> (utente `admin`, password `supersegreta`) oppure usa la CLI:

```bash
nexoctl alias set local http://localhost:9000 admin supersegreta
nexoctl mb local/foto
nexoctl cp hello.txt local/foto/
nexoctl share download --expire 2h local/foto/hello.txt     # link revocabile
```

Qualsiasi client S3 funziona con l'endpoint `http://localhost:9000` e le stesse credenziali, per esempio
`aws --endpoint-url http://localhost:9000 s3 ls`.

## Documentazione

La **documentazione utente** è in [`docs/user`](docs/user/README.md) (anche su
<https://github.com/R3n3r0/Nexo/tree/main/docs/user>, per chi legge questo file da un archivio di release):

| | |
|---|---|
| [Quickstart](docs/user/quickstart.md) | il percorso completo in cinque minuti |
| [Installazione](docs/user/installazione.md) | binari, deb/rpm, Homebrew, Docker, Compose, Helm, systemd |
| [Modalità di deployment](docs/user/modalita.md) | singolo disco, più dischi, cluster, dimensionamento |
| [Configurazione](docs/user/configurazione.md) | flag, variabili d'ambiente, configurazione dinamica |
| [Guida operativa](docs/user/operazioni/monitoraggio.md) | monitoraggio, healing, aggiornamenti, backup, risoluzione dei problemi |
| [Sicurezza](docs/user/sicurezza/utenti-policy.md) | policy, condivisione, regole di accesso, TLS, KMS |
| [Distribuzioni e cache](docs/user/distribuzioni/README.md) | domini personalizzati, con un [tutorial](docs/user/distribuzioni/tutorial-sito-statico.md) completo |
| [Riferimenti](docs/user/riferimenti/cli.md) | CLI, [Admin API](docs/user/riferimenti/admin-api.md), [compatibilità S3](docs/user/riferimenti/compatibilita-s3.md) |

`make docs-site` la pubblica come sito statico con MkDocs (in `bin/site`).

### Documenti di progetto

| # | Documento | Contenuto |
|---|-----------|-----------|
| 1 | [Architettura](docs/01-architettura.md) | Componenti, layout del repository, stack, scelte tecniche |
| 2 | [Funzionalità](docs/02-funzionalita.md) | Catalogo delle funzionalità di server, dashboard e CLI, differenze rispetto ad S3 |
| 3 | [Cluster](docs/03-cluster.md) | Topologia, erasure coding, quorum, lock distribuiti, healing, espansione |
| 4 | [API](docs/04-api.md) | API S3 supportate, Admin API, Internode RPC |
| 5 | [Roadmap e task](docs/05-roadmap-task.md) | Milestone, task con ID, dipendenze e criteri di accettazione, stato |
| 6 | [Piano di test](docs/06-piano-test.md) | Strategia e casi di test per server, CLI, dashboard e cluster |
| 7 | [Sicurezza e operatività](docs/07-sicurezza-operativita.md) | Threat model, TLS, KMS, osservabilità |
| 8 | [Modalità e rilascio](docs/08-modalita-deploy.md) | Modalità di deployment, packaging e processo di rilascio |
| 9 | [Presigned, policy e condivisione](docs/09-presigned-policy-condivisione.md) | URL presigned, POST policy, policy, share link revocabili |
| 10 | [Accessi, domini e distribuzioni](docs/10-domini-accessi-distribuzioni.md) | Regole di accesso, domini, distribuzioni dietro proxy |
| 11 | [Cache per dominio](docs/11-cache-domini.md) | Cache memoria/disco, policy per livello, invalidazione, cache in cluster |
| 12 | [Architettura a servizi](docs/12-architettura-servizi.md) | Convenzione vincolante per servizi, rotte e composizione in `internal/app` |

Le modifiche di ogni release sono nel [CHANGELOG](CHANGELOG.md); la copertura dei test di ogni funzionalità del
catalogo è nella [matrice di tracciabilità](test/TRACEABILITY.md).

## Sviluppo

Servono Go (versione in `go.mod`) e, per la console, Node.js.

| Comando | Cosa esegue |
|---------|-------------|
| `make build` | console React e binari `bin/nexo` e `bin/nexoctl` con la console inclusa (`make build-go`: solo Go) |
| `make test` / `make test-race` | test Go (unit e integrazione in-process nelle tre modalità) |
| `make test-integration` | suite di integrazione S3/IAM/presigned in tutte le modalità, test lenti inclusi |
| `make test-cluster` | test del cluster con 4 processi `bin/nexo` reali (kill, pausa, riavvio) |
| `make test-s3compat` | suite di compatibilità Ceph `s3-tests` |
| `make lint` / `make web-lint` / `make web-test` / `make e2e` | lint Go, lint e test della console, Playwright |
| `make run-cluster` | cluster locale di 4 processi su `127.0.0.1:{9101...9104}` (Linux) |
| `make docs` / `make docs-check` | riferimento della CLI in `docs/user/cli` |
| `make traceability` / `make traceability-check` | matrice funzionalità → test in `test/TRACEABILITY.md` |
| `make security` | govulncheck, gosec e `npm audit` |
| `make help` | tutti i target |

Il codice segue la convenzione di [docs/12](docs/12-architettura-servizi.md): ogni servizio ha
un'interfaccia e un'implementazione non esportata, la composizione avviene solo in `internal/app`.
