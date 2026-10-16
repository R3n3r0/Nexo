# 2. Catalogo delle funzionalità

Priorità: **P0** = MVP indispensabile · **P1** = v1.0 · **P2** = successiva alla v1.0.
Ogni funzionalità ha un codice (es. `S-OBJ-01`) richiamato nella [roadmap](05-roadmap.md) e nel [piano di test](06-test-plan.md).
I test che coprono ogni codice sono nella matrice di tracciabilità [`test/TRACEABILITY.md`](../test/TRACEABILITY.md)
(tag `covers: <codice>` nei test, [06 §6.8](06-test-plan.md#68-matrice-di-tracciabilità)): con la v1.0 ogni funzionalità
P0/P1 ha almeno un test. Le funzionalità P2 consegnate in anticipo nella v1.0 sono indicate come "P1 (anticipata, era P2)".

## 2.1 Server

### Bucket
| Codice | Funzionalità | Priorità |
|--------|--------------|----------|
| S-BKT-01 | CreateBucket / DeleteBucket / HeadBucket / ListBuckets, validazione nomi DNS-compliant | P0 |
| S-BKT-02 | GetBucketLocation, regione configurabile | P0 |
| S-BKT-03 | Bucket policy (Put/Get/Delete), accesso anonimo per bucket **e per prefix** (private/download/upload/public) | P0 |
| S-BKT-04 | Versioning (Enabled / Suspended) | P1 |
| S-BKT-05 | Tagging del bucket | P1 |
| S-BKT-06 | CORS per bucket | P1 |
| S-BKT-07 | Lifecycle (expiration, noncurrent expiration, abort multipart incompleti, transition a tier remoto P2) | P1 |
| S-BKT-08 | Object Lock (governance/compliance, retention di default, legal hold), attivabile alla creazione del bucket o in seguito su un bucket con versioning abilitato | P1 |
| S-BKT-09 | Quota per bucket (hard) | P1 |
| S-BKT-10 | Notifiche eventi bucket | P1 |
| S-BKT-11 | Crittografia di default del bucket (SSE-S3 / SSE-KMS) | P1 |
| S-BKT-12 | Replica bucket verso altro cluster (attiva-attiva) | P2 |
| S-BKT-13 | ACL canned di base (private, public-read) solo per compatibilità; fino ad allora vale l'Object Ownership "BucketOwnerEnforced" (vedi §2.4) | P2 |

### Oggetti
| Codice | Funzionalità | Priorità |
|--------|--------------|----------|
| S-OBJ-01 | PutObject / GetObject / HeadObject / DeleteObject | P0 |
| S-OBJ-02 | Range GET, conditional request (If-Match, If-None-Match, If-Modified-Since…) | P0 |
| S-OBJ-03 | ListObjects V1 / V2 (prefix, delimiter, paginazione, start-after) | P0 |
| S-OBJ-04 | CopyObject (anche con REPLACE metadata, copy-source-range) | P0 |
| S-OBJ-05 | DeleteObjects (batch fino a 1000) | P0 |
| S-OBJ-06 | Multipart upload (Create, UploadPart, UploadPartCopy, Complete, Abort, ListParts, ListMultipartUploads) | P0 |
| S-OBJ-07 | Metadati utente `x-amz-meta-*`, Content-Type/Encoding/Disposition, Cache-Control, Expires | P0 |
| S-OBJ-08 | Presigned URL GET/PUT/HEAD/DELETE e multipart, override `response-*`, scadenza max 7 gg (vedi [09](09-presigned-policy-sharing.md)) | P0 |
| S-OBJ-08b | POST policy: upload da form HTML con condizioni (`content-length-range`, `starts-with`, redirect) | P0 |
| S-OBJ-08c | Share link gestiti e **revocabili** (service account dedicato per link) + elenco condivisioni | P0 |
| S-OBJ-09 | Checksum S3 (CRC32, CRC32C, SHA1, SHA256, CRC64NVME), Content-MD5 | P1 |
| S-OBJ-10 | ListObjectVersions, operazioni per versionId, delete marker | P1 |
| S-OBJ-11 | Object tagging | P1 |
| S-OBJ-12 | Retention / legal hold per oggetto | P1 |
| S-OBJ-13 | SSE-S3, SSE-C, SSE-KMS | P1 |
| S-OBJ-14 | Compressione trasparente (S2) per content-type configurati | P2 |
| S-OBJ-15 | GetObjectAttributes (anticipata in M7) | P1 |
| S-OBJ-16 | SelectObjectContent (CSV/JSON/Parquet) | P2 |

### Regole di accesso, domini e distribuzioni (vedi [10](10-domains-access-distributions.md))
| Codice | Funzionalità | Priorità |
|--------|--------------|----------|
| S-ACC-01 | Access rules per bucket con pattern prefix / glob / **regex RE2**, principali, azioni, priorità, allow/deny | P0 |
| S-ACC-02 | Condizioni: IP/CIDR, referer, host, HTTPS, finestra temporale, header, gruppi regex vs `${aws:username}` | P1 |
| S-ACC-03 | Preset pubblico (public-read, public-prefix…) e block public access cluster/bucket | P0 |
| S-ACC-04 | Tester/simulatore di regole | P0 |
| S-DST-01 | Distribuzioni: più domini (anche wildcard) per bucket/prefix, routing per Host aggiornato a caldo in cluster | P0 |
| S-DST-02 | Modalità `website` (index, error page, SPA fallback, redirect) e `s3` (endpoint S3 su dominio custom) | P0 |
| S-DST-03 | Behavior per path (prefix/glob/regex): metodi, accesso, cache-control, header, CORS | P0 |
| S-DST-04 | URL e cookie firmati di distribuzione con key group e rotazione | P1 |
| S-DST-05 | Proxy fidati, PROXY protocol, origin secret per proxy, restrizioni IP | P0 |
| S-DST-06 | TLS per dominio: upload e ACME automatico (HTTP-01/TLS-ALPN-01; DNS-01 P2) | P1 |
| S-DST-07 | Cache integrata (memoria + disco) con policy per distribuzione / **dominio** / percorso, quote per dominio (vedi [11](11-domain-cache.md)) | P1 |
| S-DST-07b | Invalidazione automatica alla scrittura, purge per path/glob/regex/tag/dominio, soft purge, webhook verso CDN/proxy esterni | P1 |
| S-DST-07c | Cache condivisa in cluster (consistent hashing), stale-while-revalidate/if-error, request collapsing, range caching, warm-up | P1 / P2 |
| S-DST-08 | Rate limit per distribuzione; metriche e access log per distribuzione | P2 / P1 |

### Autenticazione e IAM
| Codice | Funzionalità | Priorità |
|--------|--------------|----------|
| S-IAM-01 | Utente root da env (`NEXO_ROOT_USER`, `NEXO_ROOT_PASSWORD`) | P0 |
| S-IAM-02 | Firma AWS SigV4 (header, query, streaming chunked, unsigned payload, trailer) | P0 |
| S-IAM-03 | Utenti (create, disable, delete, cambio segreto) | P0 |
| S-IAM-04 | Policy IAM in formato AWS (condizioni, variabili, NotAction…), policy predefinite (`readonly`, `readwrite`, `writeonly`, `consoleAdmin`, `diagnostics`) | P0 |
| S-IAM-04b | Simulatore di policy (allow/deny + statement determinante) e permessi effettivi per utente | P1 |
| S-IAM-05 | Gruppi e attach policy a utenti/gruppi | P1 |
| S-IAM-06 | Service account / access key multiple per utente, con scadenza e policy inline restrittiva | P1 |
| S-IAM-07 | STS AssumeRole (credenziali temporanee) | P1 |
| S-IAM-08 | OpenID Connect (AssumeRoleWithWebIdentity, login SSO console) | P2 |
| S-IAM-09 | LDAP/Active Directory | P2 |

### Storage, durabilità, cluster
| Codice | Funzionalità | Priorità |
|--------|--------------|----------|
| S-STO-00 | **Due modalità con lo stesso binario**: singolo nodo e cluster, rilevate dagli argomenti (vedi [08](08-deployment-modes.md)) | P0 |
| S-STO-01 | Singolo nodo, singolo disco (SNSD): niente erasure, file system diretto | P0 |
| S-STO-02 | Singolo nodo, più dischi (SNMD): erasure coding 2–16 dischi per set | P0 |
| S-STO-03 | Parità configurabile per storage class (`STANDARD`, `REDUCED_REDUNDANCY`) | P1 |
| S-STO-04 | Protezione bitrot (HighwayHash) e verifica in lettura | P0 |
| S-STO-05 | Healing: on-read, MRF, scanner in background, sostituzione disco | P1 |
| S-STO-06 | Cluster distribuito multi-nodo (vedi [03-cluster](03-cluster.md)) | P1 |
| S-STO-07 | Lock distribuiti a quorum | P1 |
| S-STO-08 | Espansione tramite nuovi pool | P1 |
| S-STO-09 | Decommission di un pool | P2 |
| S-STO-10 | Ribilanciamento tra pool | P2 |
| S-STO-11 | Site replication (più cluster, IAM + bucket sincronizzati) | P2 |
| S-STO-12 | Tiering ILM verso S3 remoto | P2 |

### Operatività
| Codice | Funzionalità | Priorità |
|--------|--------------|----------|
| S-OPS-01 | Config da flag/env/file + config dinamica via Admin API, hot reload | P0 |
| S-OPS-02 | TLS (certificati da directory, SNI multipli, hot reload) | P0 |
| S-OPS-03 | Health check `/nexo/health/live`, `/ready`, `/cluster` | P0 |
| S-OPS-04 | Metriche Prometheus (cluster, nodo, bucket, API, dischi) | P1 |
| S-OPS-05 | Log strutturati JSON (o testo) su stderr, livello e formato cambiati a caldo; i target HTTP/Kafka sono quelli del log di audit (S-OPS-06), i log del server si raccolgono dallo stream del processo | P1 |
| S-OPS-06 | Audit log (chi, cosa, quando, esito) con ricerca e target webhook/Kafka | P1 |
| S-OPS-07 | Trace live delle richieste (stream) e log live per la console | P1 |
| S-OPS-08 | Server info (versione, uptime, dischi, uso, stato rete) | P0 |
| S-OPS-09 | Restart/stop coordinato del cluster, aggiornamento binario rolling | P2 |
| S-OPS-10 | Rate limiting / throttling per API | P2 |
| S-OPS-11 | Diagnostica: speedtest drive/net/obj, profiling pprof (protetto) | P1 (anticipata, era P2) |
| S-OPS-12 | Rilascio come binario statico multi-OS/arch (tar/zip, deb, rpm, Homebrew) | P0 |
| S-OPS-13 | Immagini Docker multi-arch `nexo` e `nexoctl`, healthcheck integrato, `*_FILE` secrets, Compose single/SNMD/cluster | P0 |
| S-OPS-14 | `nexo server --dry-run` (validazione configurazione), URL pubblico per link dietro proxy | P1 |
| S-OPS-15 | Export/import completo di IAM, config e metadati bucket (migrazione tra modalità, backup) | P1 |

## 2.2 Dashboard (Console React)

| Codice | Pagina / funzionalità | Dettaglio | Priorità |
|--------|----------------------|-----------|----------|
| D-AUTH-01 | Login | Access/secret key, errore credenziali, sessione con scadenza, logout | P0 |
| D-AUTH-02 | Login SSO | Pulsante OIDC | P2 |
| D-DASH-01 | Overview | Capacità usata/libera, n. bucket/oggetti, stato nodi/dischi, throughput, grafici | P0 |
| D-BKT-01 | Lista bucket | Ricerca, ordinamento, dimensione, n. oggetti, data creazione, creazione/eliminazione | P0 |
| D-BKT-02 | Dettaglio bucket – Impostazioni | Versioning, object lock, quota, tag, crittografia, policy di accesso (private/public/custom) | P1 |
| D-BKT-03 | Dettaglio bucket – Lifecycle | CRUD regole con form guidato + vista JSON | P1 |
| D-BKT-04 | Dettaglio bucket – Eventi | CRUD sottoscrizioni verso target configurati | P1 |
| D-BKT-05 | Dettaglio bucket – Replica | Regole di replica, stato | P2 |
| D-OBJ-01 | Object browser | Navigazione a cartelle (prefix/delimiter), breadcrumb, paginazione infinita virtualizzata | P0 |
| D-OBJ-02 | Upload | Drag&drop file e cartelle, multipart, progress, pausa/riprendi/annulla, coda | P0 |
| D-OBJ-03 | Download | Singolo oggetto, multiplo come zip (streaming lato server) | P0 |
| D-OBJ-04 | Azioni oggetto | Elimina (anche ricorsivo prefix), rinomina/sposta (copy+delete), crea cartella, condividi (presigned con scadenza) | P0 |
| D-OBJ-05 | Anteprima | Immagini, PDF, testo, JSON, video/audio | P1 |
| D-OBJ-06 | Dettaglio oggetto | Metadati, tag, versioni (ripristino, eliminazione versione), retention, legal hold | P1 |
| D-IAM-01 | Utenti | Lista, crea, disabilita, elimina, assegna policy/gruppi | P0 |
| D-IAM-02 | Gruppi | CRUD, membri, policy | P1 |
| D-IAM-03 | Policy | Lista, editor JSON (Monaco) con validazione, policy builder visuale | P0 |
| D-SHR-01 | Condividi | Dialog link presigned (download/upload), scadenza, content-disposition, QR, copia | P0 |
| D-SHR-02 | Generatore upload | URL PUT e POST policy con snippet HTML/curl/JS | P1 |
| D-SHR-03 | Condivisioni | Elenco link attivi, revoca singola/multipla | P0 |
| D-POL-01 | Accesso bucket | Private/public/custom, regole anonime per prefix, editor bucket policy | P0 |
| D-POL-03 | Simulatore policy e permessi effettivi utente | | P1 |
| D-ACC-01..04 | Regole di accesso | Editor regole glob/regex con priorità drag&drop, tester di pattern, preset pubblico, block public access | P0 |
| D-CCH-01..08 | Cache | Pagina cache globale, tab cache per distribuzione con override per dominio, editor policy, invalidazione con stato per nodo, ispettore URL, warm-up, webhook, grafici per dominio | P1 |
| D-DST-01..08 | Distribuzioni | Elenco, wizard domini+DNS, editor behavior con tester URL, error page/redirect/header, key group e URL firmati, origin secret, certificati, invalidazione e metriche | P0/P1 |
| D-IAM-04 | Access key | Crea service account con policy inline e scadenza, mostra segreto una sola volta, download credenziali | P1 |
| D-MON-01 | Metriche | Grafici per richieste, errori, latenze, banda, uso disco; intervallo temporale | P1 |
| D-MON-02 | Log e trace live | Stream con filtri (nodo, API, status), pausa | P1 |
| D-MON-03 | Audit | Ricerca eventi audit | P1 (anticipata, era P2) |
| D-CLU-01 | Cluster | Pool, nodi, dischi (stato, uso, healing), stato erasure set | P1 |
| D-CLU-02 | Healing | Avvio heal, progresso, oggetti riparati | P1 |
| D-CLU-03 | Pool | Espansione (istruzioni), decommission, rebalance | P2 |
| D-CFG-01 | Configurazione | Regione, notifiche target, log target, KMS, compressione, scanner | P1 |
| D-CFG-02 | Licenza/Info | Versione, build, deployment ID | P0 |
| D-UX-01 | Tema chiaro/scuro, i18n it/en, responsive, accessibilità WCAG 2.1 AA | | P1 |
| D-UX-02 | Controllo accessi UI | Menu e azioni visibili in base ai permessi effettivi dell'utente | P0 |

## 2.3 CLI (`nexoctl`)

Sintassi: `nexoctl <gruppo> <comando> [ALIAS/BUCKET/PREFIX] [flag]`. Output `--json` su ogni comando, codici di uscita stabili.

| Codice | Comando | Descrizione | Priorità |
|--------|---------|-------------|----------|
| C-CFG-01 | `alias set/list/remove` | Gestione endpoint e credenziali in `~/.nexoctl/config.json` (permessi 0600) | P0 |
| C-OBJ-01 | `ls [-r] [--versions]` | Lista bucket/oggetti | P0 |
| C-OBJ-02 | `mb` / `rb [--force]` | Crea / rimuove bucket | P0 |
| C-OBJ-03 | `cp [-r]` | Copia locale↔remoto, remoto↔remoto, multipart parallelo, progress bar | P0 |
| C-OBJ-04 | `mv`, `rm [-r] [--versions]` | Sposta, elimina | P0 |
| C-OBJ-05 | `cat`, `pipe`, `head`, `stat` | Stream su stdout/stdin, metadati | P0 |
| C-OBJ-06 | `mirror [--watch] [--remove] [--preserve]` | Sincronizzazione directory/bucket; `--preserve` copia tutte le versioni e i delete marker tra due bucket (migrazione, docs/08 §8.2) | P1 |
| C-OBJ-07 | `share download/upload [--post]`, `share list/revoke`, `presign --method` | URL presigned, POST policy, share link revocabili | P0 |
| C-OBJ-07b | `anonymous set/get/list/set-json` | Accesso anonimo per bucket/prefix e bucket policy | P0 |
| C-OBJ-08 | `find`, `du`, `diff`, `tree` | Utility | P1 |
| C-OBJ-09 | `version enable/suspend/info`, `tag`, `retention`, `legalhold`, `ilm`, `event`, `quota`, `encrypt`, `anonymous` | Impostazioni bucket/oggetto | P1 |
| C-ADM-01 | `admin info` | Stato server/cluster | P0 |
| C-ADM-02 | `admin user add/remove/enable/disable/list/info` | Utenti | P0 |
| C-ADM-03 | `admin group ...` | Gruppi | P1 |
| C-ADM-04 | `admin policy create/remove/list/info/attach/detach/entities/simulate` | Policy | P0 |
| C-ACC-01 | `access rule add/ls/info/rm/move`, `access test`, `access public`, `access block-public`, `access export/import` | Regole di accesso | P0 |
| C-DST-01 | `dist create/ls/info/edit/rm`, `dist domain`, `dist behavior`, `dist keygroup`, `dist sign-url`, `dist cert`, `dist origin-secret`, `dist test`, `dist invalidate` | Distribuzioni | P0 |
| C-CCH-01 | `cache policy ...`, `dist cache set [--domain] [--behavior]`, `cache purge/inspect/warm/stats/webhook` | Cache per dominio | P1 |
| C-ADM-13 | `admin cluster export/import` | Backup/migrazione IAM, config, metadati bucket | P1 |
| C-ADM-05 | `admin accesskey create/list/remove/edit` | Service account | P1 |
| C-ADM-06 | `admin config get/set/reset/export/import` | Configurazione | P1 |
| C-ADM-07 | `admin heal [-r] [--dry-run]` | Healing | P1 |
| C-ADM-08 | `admin trace`, `admin logs` | Stream live | P1 |
| C-ADM-09 | `admin service restart/stop` | Controllo servizio (riavvio e arresto del nodo che riceve la richiesta già nella v1.0; coordinati sul cluster con S-OPS-09) | P2 |
| C-ADM-10 | `admin decommission start/status/cancel`, `admin rebalance` | Gestione pool | P2 |
| C-ADM-11 | `admin replicate add/info/remove` | Site replication | P2 |
| C-ADM-12 | `admin speedtest`, `admin prometheus generate` | Diagnostica/integrazione | P1 (anticipata, era P2) |
| C-UX-00 | Distribuzione | Binario statico per Linux/macOS/Windows e immagine Docker `nexoctl` | P0 |
| C-UX-01 | Autocompletamento bash/zsh/fish/powershell, `--quiet`, `--no-color`, `--debug` (dump HTTP) | | P0 |

## 2.4 Differenze note rispetto ad Amazon S3

Scelte di progetto verificate con le suite Ceph `s3-tests` e MinIO `mint` (`test/compat`, [06 §6.1](06-test-plan.md));
ogni test escluso per questi motivi è elencato con la sua motivazione in `test/compat/excluded.txt` e
`test/compat/mint-excluded.txt`.

| Area | Comportamento di Nexo | Motivo |
|------|-----------------------|--------|
| Chiavi e prefissi | Una chiave non può essere anche il prefisso di altre chiavi (`a/b` e `a/b/c`): la seconda scrittura è rifiutata con `XNexoParentIsObject` (400) | Gli oggetti sono directory dei dischi con `xl.meta` accanto ai dati ([01 §1.5](01-architecture.md)), come in MinIO |
| ACL | Ogni bucket si comporta come un bucket S3 con Object Ownership `BucketOwnerEnforced` (il default di AWS dal 2023): ACL diverse da `private`, grant espliciti e proprietà dell'oggetto per chi lo scrive sono rifiutati (`NotImplemented`); l'accesso si concede con bucket policy e regole di accesso anonimo | S-BKT-13 (P2) |
| Account | Un solo account: gli utenti IAM hanno i diritti delle loro policy, non quelli di "proprietario" dei bucket che creano; un bucket esistente risponde sempre `BucketAlreadyOwnedByYou`, anche in `us-east-1` | Modello IAM di Nexo (S-IAM-*) |
| Firme | Solo SigV4 (header, query, chunked, POST policy); niente SigV2 (anche `aws s3 presign` della AWS CLI v1 va configurato con `s3.signature_version = s3v4`). Una firma con scope su un'altra regione è rifiutata (`AuthorizationHeaderMalformed`), come da un endpoint regionale di S3: un deployment ha una sola regione. In un URL presigned ogni header `x-amz-*` deve essere firmato (`AccessDenied`, "headers present in the request which were not signed"), tranne `x-amz-content-sha256`, che se malformato rende la firma non valida (`SignatureDoesNotMatch`) | [01 §1.1](01-architecture.md), non obiettivi; un header non firmato cambierebbe l'operazione concessa dall'URL |
| Algoritmi di checksum | CRC32, CRC32C, CRC64NVME, SHA1, SHA256; gli algoritmi aggiunti da S3 nel 2025 (MD5, SHA512, XXHASH64/3/128) sono rifiutati (`InvalidRequest`) | S-OBJ-09 |
| Risposte | Come S3: `x-amz-meta-*` in minuscolo, nessun `x-amz-version-id` per la versione `null` nelle risposte di scrittura, `If-Match` su chiave assente → 404 `NoSuchKey`, `partNumber` oltre le parti → 416 `InvalidPartNumber`, chiave SSE-C diversa → 403, header SSE-S3/KMS su GET/HEAD → 400, ripetizione di una `CompleteMultipartUpload` già riuscita → stesso esito, lista delle parti strettamente crescente (`InvalidPartOrder`), `x-amz-delete-marker` assente quando sarebbe `false`, `GetBucketPolicyStatus` senza policy → 404 `NoSuchBucketPolicy`, la versione scritta prima dell'attivazione del versioning è la versione `null` del bucket (riportata e indirizzabile come `null` da letture, listing, lifecycle e console), `CompleteMultipartUpload` di un upload COMPOSITE senza il checksum di ogni parte → 400 `InvalidRequest` (con `BadDigest` prima, se un checksum dato non corrisponde), `x-amz-mp-object-size` diverso dalla somma delle parti → 400 `InvalidRequest`, una versione di una chiave assente → 404 `NoSuchVersion` (non `NoSuchKey`), `NextPartNumberMarker` di `ListParts`/`GetObjectAttributes` sempre l'ultima parte della pagina, `PutBucketTagging` → 200, `Content-MD5` vuoto → `InvalidDigest`, ACL canned sconosciuta → 400 `InvalidArgument`, POST su un oggetto senza sotto-risorsa → 405 `MethodNotAllowed`, `ListObjectVersions` con versioni e delete marker nell'ordine del listing, copia di un oggetto su sé stesso che ne cambia solo la cifratura (es. decifra) ammessa, messaggi di errore con le virgolette in chiaro (es. `Policy Condition failed: ["eq", ...]`), header `Date` con fuso `-0000` (botocore) accettato. `DeleteObject` con `If-Match` (ETag o `*`) cancella solo se la versione bersaglio ha quell'ETag, altrimenti 412 `PreconditionFailed` (anche per una versione assente o un delete marker); le condizioni dei directory bucket (`x-amz-if-match-last-modified-time`, `x-amz-if-match-size`) → 501, mai ignorate. `AbortMultipartUpload` di un upload sconosciuto → 404 `NoSuchUpload` (MinIO risponde 204). Il `Grantee` delle ACL ripete il tipo come elemento `<Type>` (richiesto dagli SDK MinIO, ignorato da quelli AWS). `Expect` diverso da `100-continue` → 417 (server HTTP di Go, RFC 9110) | Compatibilità con gli SDK |
| Autorizzazione | Una richiesta su un oggetto è autorizzata (es. `s3:GetObject`) prima di cercare la chiave: chi ha solo `s3:ListBucket` riceve 403 anche per chiavi assenti; chi ha `s3:GetObject` riceve 404 per una chiave assente anche senza `s3:ListBucket` | Valutazione delle policy indipendente dallo stato dei dati |
| Metadati non ASCII | I valori `x-amz-meta-*` sono restituiti con i byte ricevuti (UTF-8), senza codifica RFC 2047 né transcodifica in ISO-8859-1 | Gli SDK Go e JavaScript leggono gli header in UTF-8 |
| Letture e scritture concorrenti | Una GET legge un'istantanea: risolta la versione, apre i file dei dati e rilascia subito il lock di lettura, così una PUT o una DELETE della stessa chiave non attende il trasferimento e chi legge riceve per intero la versione risolta anche se nel frattempo è stata sostituita e i suoi dati eliminati (un file aperto sopravvive alla rimozione, POSIX). In erasure coding la lettura tiene aperto lo shard di **ogni** drive online: i file locali, e quelli dei drive remoti tramite un handle che il loro nodo tiene aperto senza trasferire dati (`storage/open-file`, chiuso a fine lettura, all'arresto ordinato del nodo che legge, alla prima apertura del suo processo successivo se è ripartito dopo un crash, e comunque dopo 1 ora in cui il nodo che legge non lo usa né lo rinnova: finché la lettura è in corso lo rinnova ogni 15 minuti, anche se non ne legge mai lo shard (`storage/touch-handles`), e uno sweep periodico chiude quelli dimenticati anche senza altre letture); così uno shard aggiunto a metà lettura (bitrot, drive o nodo che cade, shard lento) legge la stessa versione e la lettura conserva tutta la ridondanza della parità. Restano sotto lock, senza aprire nulla prima, le letture di più di 8 parti (`object.WithPinnedReadParts`) e tutte le letture su Windows; restano sotto lock fino alla fine anche le letture in cui un drive online non ha potuto aprire il suo shard per un motivo diverso dallo shard mancante (nodo di una release precedente durante un rolling upgrade, tabella degli handle del nodo piena, errore di rete). Un client che smette di leggere per più di 20 s (`NEXO_SEND_TIMEOUT`) vede comunque la risposta interrotta, sia sull'API S3 sia sui siti delle distribuzioni (listener S3 e web): il limite vale per ogni risposta di ogni listener, e una connessione HTTP/2 che non accetta byte per lo stesso tempo viene chiusa con tutti i suoi stream | MVCC sui data dir delle versioni; MinIO tiene il lock per tutto il trasferimento |
| CORS | Come S3, `Access-Control-Allow-Headers` riporta in minuscolo gli header richiesti e una richiesta non preflight che dichiara `Access-Control-Request-Method` è valutata con quel metodo. Un bucket **senza** configurazione CORS segue la politica dell'intero deployment `api.cors_allow_origin` (default `*`, [07 §7.6](07-security-operations.md)): la preflight risponde 200 con le origini ammesse invece del 403 `CORSResponse: CORS is not enabled for this bucket` di S3. Un bucket con configurazione CORS si comporta come in S3 (solo le sue regole decidono, 403 se nessuna corrisponde) | Console e applicazioni web servite da altri domini funzionano senza configurare ogni bucket (come MinIO); restringibile per deployment |
| Notifiche eventi | I record (`Records[]`) hanno la forma di S3 (`eventVersion` `2.1`, `eventName` senza prefisso `s3:`, es. `ObjectCreated:Put`, chiave URL-encoded, `sequencer`), con `eventSource` `nexo:s3`; il payload dei webhook aggiunge in testa `EventName` (con prefisso, `s3:ObjectCreated:Put`) e `Key` (`bucket/chiave`) come MinIO. Configurazioni sovrapposte (stesso tipo di evento e filtri prefix/suffix che si intersecano) sono rifiutate con `400 InvalidArgument` come in S3. Il `sequencer` è il tempo di commit della versione (o della rimozione), monotono per chiave anche tra nodi con orologi sfasati. Le modifiche Object Lock (retention, legal hold) non generano eventi, come in S3: MinIO pubblica `s3:ObjectCreated:PutRetention`/`PutLegalHold`, che un consumer di `s3:ObjectCreated:*` leggerebbe come nuovi oggetti | Compatibilità con i consumer scritti per S3 e per MinIO |
