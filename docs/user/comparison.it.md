# Confronto con le alternative

Esistono diversi object storage open source compatibili con S3, e quello giusto dipende da cosa devi far
girare. Questa pagina confronta Nexo con quelli considerati più spesso: MinIO, SeaweedFS, Garage e Ceph RGW.
Cerca di essere onesta: Nexo è giovane (la 1.0 è uscita a ottobre 2026), gli altri hanno anni di uso in
produzione alle spalle, e ognuno di loro fa cose che Nexo non fa.

> **Sugli altri progetti.** Le informazioni sugli altri progetti sono un riassunto scritto a **ottobre 2026** dalla loro documentazione
> pubblica. I progetti cambiano: controlla la loro documentazione prima di decidere, e
> [apri una issue](https://github.com/R3n3r0/Nexo/issues) se qualcosa qui è sbagliato o non aggiornato.

## In sintesi

| | Nexo | MinIO (community) | SeaweedFS | Garage | Ceph RGW |
|---|---|---|---|---|---|
| Linguaggio | Go | Go | Go | Rust | C++ |
| Licenza | AGPL-3.0 | AGPL-3.0 | Apache-2.0 | AGPL-3.0 | LGPL |
| Stato | attivo, 1.0 nel 2026 | archiviato upstream nel 2026, solo sorgenti; lo sviluppo continua nel prodotto commerciale AIStor | attivo | attivo | attivo |
| Deployment | un binario: un disco, più dischi, cluster | un binario: un disco, più dischi, cluster | server master + volume + filer (possono girare in un processo) | un binario per nodo | un cluster Ceph (MON, MGR, OSD) più i demoni RGW |
| Protezione dei dati | erasure coding (Reed-Solomon) con controllo del bitrot e healing | erasure coding con controllo del bitrot e healing | replica; erasure coding per i volumi "warm" | replica (di solito 3 copie) tra zone | pool replicati o con erasure coding |
| Metadati | salvati con gli oggetti, nessun database esterno | salvati con gli oggetti | filer store (integrato o un database esterno) | metadati distribuiti integrati | RADOS |
| Versioning, Object Lock | sì | sì | sì | non ancora in una release (in sviluppo) | sì |
| Lifecycle | sì | sì | sì | solo scadenza e upload incompleti | sì |
| Notifiche dei bucket | webhook, Kafka, NATS, AMQP, Redis, ... con coda persistente | molti target (webhook, Kafka, NATS, ...) | tramite gli eventi dei metadati del filer | no | sì |
| Cifratura lato server | SSE-S3, SSE-KMS, SSE-C (KMS integrato o KES) | SSE-S3, SSE-KMS, SSE-C (KES) | SSE-S3, SSE-KMS, SSE-C | SSE-C | SSE-S3, SSE-KMS, SSE-C |
| IAM | utenti, gruppi, policy, access key, STS AssumeRole | utenti, gruppi, policy, STS, OIDC, LDAP | utenti, gruppi, policy, STS, OIDC | permessi per chiave sui bucket, niente policy | utenti e policy, STS, OIDC, Keystone |
| Console web | inclusa (amministrazione completa) | ridotta a un object browser nel 2025 | UI di base per master e filer | nessuna integrata (esistono UI della comunità) | Ceph Dashboard |
| Geo / multi-sito | no | replica di sito e di bucket | replica asincrona tra cluster | multi-zona per progetto | multi-site |
| Oltre S3 | distribuzioni stile CDN su domini propri con cache e certificati ACME | | accesso POSIX/FUSE, WebDAV, HDFS agli stessi dati | siti statici | block (RBD) e file (CephFS) nello stesso cluster |

## Quando scegliere Nexo

- **Vuoi un solo binario con tutto incluso.** Erasure coding, healing, versioning, Object Lock, cifratura, IAM,
  una console web completa, metriche Prometheus, dashboard Grafana e una CLI per dati e amministrazione,
  da un nodo a un cluster.
- **Stai lasciando MinIO community.** Il modello di deployment è familiare (dischi, erasure set, pool,
  `/data{1...4}`), le porte predefinite sono le stesse, e la migrazione copia ogni versione con `nexoctl mirror
  --preserve`. Vedi [Migrare da MinIO](migrate-from-minio.md).
- **Servi contenuti dai bucket.** Le distribuzioni mettono un bucket dietro i tuoi domini con certificati ACME,
  URL firmati e una cache in memoria e su disco, senza una CDN o un proxy in più.
- **Ti interessa il comportamento S3, non solo le chiamate più comuni.** Nexo supera tutti i 451 test
  applicabili della suite Ceph `s3-tests` e i 638 test delle suite SDK `mint` di MinIO, in ogni modalità di
  deployment, in CI. La pagina [Compatibilità S3](reference/s3-compatibility.md) elenca cosa è supportato e
  cosa no.

## Quando è meglio un altro progetto

- **Distribuzione geografica su più siti o collegamenti deboli**: Garage è progettato per nodi sparsi in più
  luoghi; Ceph RGW e il prodotto commerciale di MinIO hanno la replica multi-sito. Nexo non ha ancora la
  replica tra cluster.
- **Miliardi di file piccoli, o accesso ai dati come file system**: il design a volumi di SeaweedFS e il suo
  filer (FUSE, WebDAV, HDFS) sono fatti per questo.
- **Una piattaforma unica per block, file e object su larga scala**: Ceph, se sei in grado di gestirlo.
- **Identità esterne (OIDC, LDAP, Keystone)**: non ancora disponibili in Nexo; MinIO, SeaweedFS e Ceph RGW ne
  supportano alcune.
- **Una lunga storia in produzione**: tutti gli altri progetti qui ce l'hanno. Nexo ha molti test automatici
  (cluster multi-processo, iniezione di guasti, test di aggiornamento, fuzzing) ma poca storia sul campo:
  provalo prima sul tuo carico.

## Prestazioni

Il throughput dipende da dischi, rete, dimensione degli oggetti e concorrenza più che dal server. Invece di
pubblicare numeri misurati altrove, Nexo include uno script che esegue il benchmark `warp` di MinIO contro Nexo
e qualsiasi altro endpoint S3 sul **tuo** hardware: vedi [`scripts/bench/warp-compare.sh`](https://github.com/R3n3r0/Nexo/blob/main/scripts/bench/warp-compare.sh) nel repository.
