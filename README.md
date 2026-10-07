# Nexo

**Nexo** è un object storage compatibile con l'API **Amazon S3**, scritto in **Go**, ispirato a MinIO.
Include un server S3, una CLI di amministrazione (`nexoctl`) e una dashboard web in **React**
servita direttamente dal binario Go. È progettato per girare sia su un singolo nodo sia in
**cluster distribuito** con erasure coding.

> Stato: **fase di progettazione**. Questo repository contiene per ora solo i documenti di progetto.

## Documenti di progetto

| # | Documento | Contenuto |
|---|-----------|-----------|
| 1 | [Architettura](docs/01-architettura.md) | Componenti, layout del repository, stack, scelte tecniche |
| 2 | [Funzionalità](docs/02-funzionalita.md) | Catalogo completo delle funzionalità di server, dashboard e CLI con priorità |
| 3 | [Cluster](docs/03-cluster.md) | Topologia, erasure coding, quorum, lock distribuiti, healing, espansione |
| 4 | [API](docs/04-api.md) | API S3 supportate, Admin API, Internode RPC |
| 5 | [Roadmap e task](docs/05-roadmap-task.md) | Milestone, task con ID, dipendenze e criteri di accettazione |
| 6 | [Piano di test](docs/06-piano-test.md) | Strategia e casi di test per server, CLI, dashboard e cluster |
| 7 | [Sicurezza e operatività](docs/07-sicurezza-operativita.md) | Threat model, TLS, KMS, osservabilità, deploy |
| 8 | [Modalità e rilascio](docs/08-modalita-deploy.md) | Singolo nodo (SNSD/SNMD) e cluster, binario e Docker/Compose |
| 9 | [Presigned, policy e condivisione](docs/09-presigned-policy-condivisione.md) | URL presigned, POST policy, bucket/IAM policy, share link revocabili |
| 10 | [Accessi, domini e distribuzioni](docs/10-domini-accessi-distribuzioni.md) | Regole di accesso con glob/regex, bucket pubblici, più domini per bucket, distribuzioni stile CloudFront dietro proxy |
| 11 | [Cache per dominio](docs/11-cache-domini.md) | Cache integrata memoria/disco, policy per distribuzione/dominio/percorso, invalidazione automatica e manuale, cache condivisa in cluster |

## In breve

```
            ┌───────────────────────────── nexo server ─────────────────────────────┐
 S3 SDK ───▶│ :9000  S3 API (SigV4) ─┐                                              │
 nexoctl ──▶│ :9001  Admin API ──────┼──▶ IAM ─▶ ObjectLayer ─▶ Pools ─▶ ErasureSets │
 Browser ──▶│ :9001  Console React ──┘            (embed.FS)            │           │
            │                                                     Drive locali/remoti│
            └────────────────────────── Internode RPC (mTLS) ◀──────────────────────┘
```

- **Due modalità, stesso binario**: singolo nodo (1 disco o più dischi con erasure coding) e cluster multi-nodo.
- **Rilascio** come binario statico (Linux/macOS/Windows, amd64/arm64) **e** come immagine Docker (+ Compose per singolo nodo e cluster).
- **Tre interfacce**: API S3, CLI `nexoctl` e dashboard web React servita dal binario Go.
- **URL presigned, POST policy, policy IAM/bucket, accesso anonimo, share link revocabili** da API, CLI e dashboard.
- **Regole di accesso con glob e regex**, bucket o prefix pubblici, **più domini per bucket** e **distribuzioni stile CloudFront** (behavior per path, URL firmati, TLS/ACME, origin secret per proxy), configurabili da dashboard e CLI.
- **Cache per dominio**: policy di cache per distribuzione, dominio e percorso, invalidazione automatica alla scrittura, purge per path/tag/dominio, cache condivisa nel cluster.
- **Compatibilità S3**: SigV4, bucket, oggetti, multipart, versioning, policy, lifecycle, object lock, SSE.
- **Cluster**: pool di server, erasure set Reed-Solomon, nessun database di metadati centrale.
