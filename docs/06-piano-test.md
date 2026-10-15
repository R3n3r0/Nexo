# 6. Piano di test

## 6.1 Strategia (piramide)

| Livello | Server (Go) | CLI (Go) | Dashboard (React) |
|---------|-------------|----------|-------------------|
| Unit | `go test -race`, table-driven, mock di `StorageAPI`/`ObjectLayer` | Test dei comandi con client finto, golden file dell'output | Vitest + React Testing Library + MSW |
| Fuzz | `go test -fuzz` su parser XML, SigV4, policy, `xl.meta`, endpoint ellissi | Parser di path/alias | — |
| Contratto | Risposte Admin API validate contro `openapi.yaml` | Client `nexoadmin` vs OpenAPI | Client TS generato: typecheck in CI |
| Integrazione | Server in-process + `aws-sdk-go-v2`, `boto3`, `aws-cli` | `testscript` (`.txtar`) contro server in-process | Playwright contro binario `nexo` reale con UI embedded |
| Compatibilità | Ceph `s3-tests`, MinIO `mint` | — | — |
| Cluster/Chaos | docker-compose 4×4 + toxiproxy + kill/pausa container | CLI contro cluster | Pagine Cluster/Healing contro cluster |
| Prestazioni | `go test -bench`, `warp`, k6 | Throughput `cp`/`mirror` | Lighthouse, bundle size budget |
| Sicurezza | `gosec`, `govulncheck`, ZAP sulla console, test authz negativi | Permessi file config | CSP, XSS, CSRF |

### Pipeline CI

| Trigger | Job |
|---------|-----|
| Ogni PR | lint Go/TS, unit Go (`-race`, copertura), unit web, build con UI embedded, integrazione server, testscript CLI, Playwright smoke (Chromium), contratto OpenAPI |
| Merge su `main` | + Playwright completo (Chromium, Firefox, WebKit), fuzz 5 min per target, cluster smoke 4 nodi |
| Notturna | s3-tests, mint, suite chaos cluster, fuzz 1 h, benchmark con confronto vs baseline (regressione > 10% = fallimento) |
| Release | Test di upgrade, scansione immagini (trivy), SBOM |

### Suite di compatibilità Ceph `s3-tests`

`test/compat/run-s3tests.sh` (`make test-s3compat`, job notturno `s3tests-nightly`) scarica `ceph/s3-tests` a un
commit fissato, crea un venv Python con le dipendenze bloccate da `test/compat/constraints.txt`, avvia `bin/nexo`
su TLS con il KMS integrato (SNMD a 4 dischi; `NEXO_S3TESTS_MODE=snsd|cluster` per le altre modalità, tutte e tre nel
job notturno), crea con
`nexoctl admin` gli utenti `main`, `alt` e `tenant` della suite e le chiavi KMS, genera `s3tests.conf` ed esegue
`test_s3.py` con pytest. Il plugin `test/compat/nexo_s3tests.py` deseleziona i test di `test/compat/excluded.txt`
(ognuno con il motivo: funzionalità fuori catalogo o P2, comportamenti specifici di RGW, differenze note di
[02 §2.4](02-funzionalita.md)), classifica i test per gruppo funzionale e scrive `report.txt`/`report.json`
(esito per test e per gruppo). Senza rete (git/pip) l'esecuzione locale è saltata e documentata in
`report/SKIPPED`; in CI (`NEXO_S3TESTS_REQUIRE_NETWORK=1`) è un errore. Il job fallisce se un gruppo obiettivo
di M7 (versioning, object lock, multipart, tagging, SSE-C, CORS, checksum) supera meno del 90% dei test applicabili.

**Soglie di copertura**: `internal/{auth,policy,erasure,storage,object,iam}` ≥ 85%; resto backend ≥ 70%; web ≥ 75% (statements).

---

## 6.2 Test del server

### Unit
| ID | Area | Casi |
|----|------|------|
| TS-U-01 | Endpoint/ellissi | `{1...4}`, `{a...d}`, zero-padding `{01...16}`, combinazioni, input non validi, dischi duplicati, mix http/https → errore |
| TS-U-02 | Calcolo set | Per 1–32 nodi × 1–32 dischi: dimensione set valida, simmetria sui nodi, parità di default |
| TS-U-03 | Erasure | Encode/decode per ogni (D,P); ricostruzione con 1..P shard mancanti; errore con P+1; blocchi parziali; oggetti da 0 byte, 1 byte, block size ±1 |
| TS-U-04 | Bitrot | Corruzione di 1 bit rilevata; hash per blocco; verifica streaming |
| TS-U-05 | `xl.meta` | Round-trip, versioni multiple, inline data, compatibilità con versioni precedenti del formato (fixture binarie), fuzz |
| TS-U-06 | Quorum metadati | Scelta della versione corretta con dischi divergenti (mod-time, data-dir diversi), metadati corrotti |
| TS-U-07 | SigV4 | Vettori AWS ufficiali; scadenza presigned (`X-Amz-Expires` max 7 gg); clock skew > 15 min; header firmati mancanti; chunked con firma chunk errata; trailer checksum |
| TS-U-08 | Policy engine | Allow/Deny, deny esplicito prevale, wildcard in Action/Resource, ogni operatore Condition, variabili, NotAction/NotResource, policy invalide rifiutate |
| TS-U-09 | Nomi bucket/oggetti | Regole DNS bucket; chiavi con unicode, `//`, `..`, `\`, null byte, lunghezza 1024 byte; path traversal impossibile |
| TS-U-10 | Errori S3 | Ogni `ErrorCode` → status HTTP e XML corretti |
| TS-U-11 | Lifecycle | Valutazione regole con clock finto: filtri prefix/tag/size, giorni, date, noncurrent (anche la versione `null` scritta prima del versioning), MPU; le cancellazioni sono condizionate alla versione valutata (una scrittura intercalata tra valutazione e cancellazione non viene mai rimossa né nascosta) |
| TS-U-12 | Object lock | Matrice governance/compliance × bypass × legal hold × utente |
| TS-U-13 | Crypto | DARE encrypt/decrypt, range su oggetti cifrati, rotazione della master key con ri-sigillatura delle chiavi dati (`admin kms reseal`), chiavi distinte per ogni caricamento di una parte, checksum sigillati, SSE-C chiave errata → 403; anti-troncamento (una sorgente che fallisce, anche con `io.ErrUnexpectedEOF`, fa fallire lo stream e la codifica erasure: un corpo di dimensione ignota troncato è `400 IncompleteBody` in ogni modalità, con e senza SSE); upload multipart in corso ri-sigillati da `reseal`; chiavi redatte in log e JSON; in matrice anche con un KES simulato su mTLS (`testutil.WithKES`) e SSE-C rifiutata su HTTP in chiaro |
| TS-U-14 | dsync | Acquisizione con quorum, perdita quorum durante refresh, scadenza lock orfani, read/write concorrenti |
| TS-U-15 | Placement | Distribuzione uniforme delle chiavi sui set (test chi-quadro), stabilità dell'hash tra versioni |

### Integrazione (server in-process, SDK reali)
| ID | Scenario | Atteso |
|----|----------|--------|
| TS-I-01 | Ciclo bucket: create, list, head, delete (vuoto / non vuoto) | 200/409 `BucketNotEmpty` |
| TS-I-02 | Put/Get oggetti da 0 B, 1 KiB, 127 KiB (inline), 129 KiB, 100 MiB | Contenuto ed ETag identici |
| TS-I-03 | Range GET: `bytes=0-0`, `-500`, `100-`, oltre la fine | 206 / 416 corretti |
| TS-I-04 | Conditional GET/PUT (`If-None-Match: *` su PUT) | 304/412 |
| TS-I-05 | Listing: 10k oggetti con prefix/delimiter annidati, paginazione 1/1000, `start-after`, encoding-type=url | Nessun duplicato/omesso |
| TS-I-06 | Multipart: parti fuori ordine, parte ripetuta, parte < 5 MiB non finale → errore, abort, ListParts paginato, UploadPartCopy | Conforme S3 |
| TS-I-07 | Presigned URL GET/PUT con curl; URL scaduto | 200 / 403 |
| TS-I-08 | POST policy da form HTML con condizioni (content-length-range, starts-with) | 204/403 |
| TS-I-09 | Utente readonly/writeonly/custom: matrice di 20 operazioni × policy | Allow/Deny attesi |
| TS-I-10 | Bucket pubblico in lettura, accesso anonimo a list/get/put | Solo quanto concesso |
| TS-I-11 | Service account con policy inline più restrittiva del parent | Intersezione rispettata |
| TS-I-12 | Versioning: put×3, delete (marker), get versione specifica, delete versione, suspend; bucket versionato dopo le prime scritture (versione `null` indirizzabile, cancellazione di tutte le versioni e forzata dalla console senza marker residui) | Conforme S3 |
| TS-I-13 | Concorrenza: 100 goroutine PUT sulla stessa chiave + GET | Sempre un oggetto completo e consistente |
| TS-I-14 | Read-after-write: PUT poi GET/LIST immediati | Sempre visibile |
| TS-I-15 | Eventi: webhook di test riceve `ObjectCreated`/`ObjectRemoved` con payload S3 standard; target offline → recapito dopo ripristino; `sequencer` monotono per chiave con orologi sfasati; nessun evento per retention/legal hold; i webhook non seguono i redirect | — |
| TS-I-16 | Quota: superamento | Errore e nessun dato scritto |
| TS-I-17 | Riavvio del server: tutti i dati, IAM, config, multipart in corso preservati | — |
| TS-I-18 | Crash (SIGKILL) durante PUT di 1 GiB | Nessun oggetto parziale; tmp ripuliti |
| TS-I-19 | Compatibilità client: aws-cli, boto3, rclone, s3cmd, Terraform provider S3 backend | Script smoke verdi |
| TS-I-20 | Admin API: ogni endpoint con utente autorizzato/non autorizzato, input invalidi → `problem+json` | Contratto rispettato |

### Erasure single-node (multi-drive)
| ID | Scenario | Atteso |
|----|----------|--------|
| TS-E-01 | 4 dischi, 1 disco rimosso (directory rinominata) | Lettura/scrittura OK |
| TS-E-02 | 4 dischi EC:2, 2 dischi rimossi | Lettura OK, scrittura OK solo se quorum |
| TS-E-03 | 3 dischi su 4 rimossi | `503`, nessuna corruzione; dopo ripristino tutto OK |
| TS-E-04 | Shard corrotto (bit flip) | GET corretto, shard riparato, metrica bitrot incrementata |
| TS-E-05 | Disco svuotato e reinserito | Healing completo; ripresa dopo restart a metà healing |
| TS-E-06 | Disco lento (fault injection latenza) | Latenza p99 contenuta grazie a letture da shard più veloci |
| TS-E-07 | Disco pieno | Errore `XNexoStorageFull`, nessun oggetto parziale |

## 6.3 Test del cluster (docker-compose 4 nodi × 4 dischi + toxiproxy)

| ID | Scenario | Atteso |
|----|----------|--------|
| TC-01 | Avvio nodi in ordine casuale con ritardi fino a 60 s | Cluster pronto quando c'è quorum |
| TC-02 | Avvio con topologia diversa su un nodo | Il nodo rifiuta di partire con errore chiaro |
| TC-03 | Load balancer round-robin: PUT su nodo1, GET su nodo3 | Coerenza immediata |
| TC-04 | Utente/policy creati su nodo2 | Validi subito su tutti i nodi |
| TC-05 | Stop di 1 nodo durante carico `warp` mixed | 0 errori lato client (con retry SDK), poi heal automatico |
| TC-06 | Stop di 2 nodi su 4 | Letture OK, scritture rifiutate senza corruzione (quorum) |
| TC-07 | Partizione di rete 2+2 (toxiproxy) | Nessuno split-brain: nessuna scrittura accettata da entrambe le metà |
| TC-08 | Latenza 200 ms + perdita pacchetti 5% su un nodo | Operazioni completate entro timeout, nessun lock bloccato |
| TC-09 | Nodo che crasha mentre tiene un write lock | Lock rilasciato entro la scadenza, altre scritture riprendono |
| TC-10 | Sostituzione completa dei dischi di un nodo | Healing ricostruisce tutto; checksum di tutti gli oggetti uguali a quelli originali |
| TC-11 | Espansione: aggiunta di un secondo pool con rolling restart | Nessun downtime visibile; nuovi oggetti nel nuovo pool; vecchi leggibili |
| TC-12 | Rolling upgrade binario N → N+1 | Nessun errore client |
| TC-13 | Clock skew 5 s su un nodo | Avviso nei log/console; nessuna anomalia funzionale |
| TC-14 | Job singleton (scanner/ILM) | Esattamente un'istanza attiva; failover se il leader muore |
| TC-15 | Soak test 24 h con carico misto + chaos casuale (Jepsen-like, verifica con checker di linearizzabilità sulle chiavi) | Nessuna violazione di consistenza, nessun leak di memoria/goroutine |

Verifica dati: ogni test chaos usa un **generatore deterministico** (seed) e un checker che confronta lo stato finale
(chiave → hash contenuto/versione) con il registro delle operazioni confermate.

## 6.4 Test della CLI (`nexoctl`)

Strumento principale: `github.com/rogpeppe/go-internal/testscript` con file `.txtar` in `test/cli/`,
server Nexo in-process avviato dal setup, `HOME` isolata.

### Unit
| ID | Caso |
|----|------|
| TCL-U-01 | Parsing target `alias/bucket/prefix`, path locali Windows/Unix, glob |
| TCL-U-02 | Config alias: creazione con permessi 0600, file corrotto → errore chiaro, migrazione versione config |
| TCL-U-03 | Formattazione output: tabella, `--json` (JSON Lines validi), `--no-color`, dimensioni human-readable |
| TCL-U-04 | Codici di uscita: 0 ok, 1 errore generico, 2 uso errato, 3 non trovato, 4 accesso negato |
| TCL-U-05 | Help e usage: golden file per ogni comando (rileva modifiche non volute) |
| TCL-U-06 | Calcolo part size multipart per file da 1 B a 5 TiB (max 10000 parti) |

### End-to-end (testscript)
| ID | Scenario | Atteso |
|----|----------|--------|
| TCL-E-01 | `alias set` con credenziali errate | Errore autenticazione, exit 4 |
| TCL-E-02 | `mb`, `ls`, `rb` su bucket vuoto e non vuoto (`--force`) | Output e stato corretti |
| TCL-E-03 | `cp` file singolo, ricorsivo, remoto→remoto, stdin→remoto (`pipe`), con metadati e tag | Hash identici |
| TCL-E-04 | `cp` di 2 GiB interrotto (SIGINT) | Abort multipart pulito, nessun upload pendente |
| TCL-E-05 | `mv`, `rm -r --force`, `rm --versions` | — |
| TCL-E-06 | `mirror` due volte | Seconda esecuzione: 0 trasferimenti |
| TCL-E-07 | `mirror --remove` e `--watch` (file creato/modificato/cancellato) | Sincronizzazione entro 2 s |
| TCL-E-08 | `share download --expire 1h` | URL scaricabile con curl |
| TCL-E-09 | `stat`, `cat`, `head -n`, `find --name "*.log" --older-than 1d`, `du`, `tree` | Output atteso |
| TCL-E-10 | `admin user add/list/disable/remove`, login con utente disabilitato | Accesso negato |
| TCL-E-11 | `admin policy create` da file JSON invalido | Errore con riga/colonna |
| TCL-E-12 | `admin policy attach` + verifica permessi effettivi con operazioni | — |
| TCL-E-13 | `admin accesskey create --expiry` | Chiave scade |
| TCL-E-14 | `admin config set/get/export/import` | Round-trip identico |
| TCL-E-15 | `admin info` su cluster con un nodo offline | Nodo mostrato offline, exit 0 |
| TCL-E-16 | `admin heal -r --dry-run` | Report senza modifiche |
| TCL-E-17 | `admin trace` riceve le richieste generate da un altro processo | — |
| TCL-E-18 | Tutti i comandi con `--json` | Output valido contro JSON schema |
| TCL-E-19 | Autocompletamento bash/zsh generato e funzionante su alias/bucket | — |
| TCL-E-20 | Cross-platform: suite eseguita su Linux, macOS, Windows in CI | — |

## 6.5 Test della dashboard (React)

### Unit / componenti (Vitest + RTL + MSW)
| ID | Componente | Casi |
|----|-----------|------|
| TD-U-01 | Form login | Validazione campi, errore 401 mostrato, disabilitazione durante submit, redirect post-login alla pagina richiesta |
| TD-U-02 | Auth guard | Sessione scaduta (401 da qualsiasi query) → redirect a login con messaggio |
| TD-U-03 | Tabella bucket | Ordinamento, ricerca, stato vuoto, loading skeleton, errore con retry |
| TD-U-04 | Dialog crea bucket | Validazione nome DNS live, opzioni versioning/lock/quota, errore `BucketAlreadyExists` |
| TD-U-05 | Object browser | Breadcrumb da prefix, navigazione cartelle, selezione multipla, paginazione a cursore, chiavi con caratteri speciali (spazi, `#`, `%`, unicode) |
| TD-U-06 | Upload manager | Calcolo parti, concorrenza max, retry parte fallita, pausa/riprendi, annulla (chiama abort), progress aggregato |
| TD-U-07 | Editor policy | JSON invalido evidenziato, errori di validazione dal server mostrati, policy builder ↔ JSON sincronizzati |
| TD-U-08 | Lifecycle form | Conversione form ↔ XML/JSON regole, validazioni (giorni > 0, filtro) |
| TD-U-09 | UI permission-aware | Con permessi readonly mancano pulsanti crea/elimina; azioni non permesse disabilitate con tooltip |
| TD-U-10 | Dialog share | Scadenza max 7 gg, copia negli appunti |
| TD-U-11 | Grafici metriche | Rendering con dati vuoti, serie multiple, formattazione unità |
| TD-U-12 | Log live | Riconnessione SSE dopo disconnessione, pausa/ripresa, limite buffer righe |
| TD-U-13 | i18n | Nessuna chiave mancante in it/en (test che confronta i file) |
| TD-U-14 | Hook API | Gestione errori `problem+json`, invalidazione cache TanStack Query dopo mutazioni |

### End-to-end (Playwright contro `bin/nexo` reale con UI embedded)
Setup: global setup avvia `nexo server` su directory temporanea (e, per la suite cluster, il docker-compose), crea utenti di test via Admin API.

| ID | Flusso | Atteso |
|----|--------|--------|
| TD-E-01 | Login root → overview → logout | Cookie rimosso, accesso a pagina protetta → login |
| TD-E-02 | Login con credenziali errate ×5 | Messaggio errore, rate limiting dopo N tentativi |
| TD-E-03 | Deep link `/buckets/foo/browse/a/b/` dopo login | Pagina corretta (verifica fallback SPA lato Go) |
| TD-E-04 | Crea bucket, verifica in lista, elimina | — |
| TD-E-05 | Upload drag&drop di una cartella con 50 file annidati | Struttura prefix corretta |
| TD-E-06 | Upload file 200 MiB, annulla a metà | Nessun upload multipart pendente lato server |
| TD-E-07 | Download singolo e zip di 3 oggetti | File scaricati con contenuto atteso |
| TD-E-08 | Crea cartella, sposta oggetto, elimina prefix ricorsivamente | — |
| TD-E-09 | Share link: apertura in contesto anonimo | Download OK; dopo scadenza 403 |
| TD-E-10 | Anteprima immagine, PDF, testo | Rendering |
| TD-E-11 | Versioning: abilita, carica 3 versioni, ripristina la prima, elimina una versione | — |
| TD-E-12 | Crea policy custom (solo bucket `x`), crea utente, assegna; login con quell'utente | Vede solo `x`, non vede menu Admin |
| TD-E-13 | Crea access key con scadenza | Segreto mostrato una sola volta; funziona con SDK |
| TD-E-14 | Regola lifecycle e sottoscrizione evento webhook | Persistite e visibili via CLI |
| TD-E-15 | Pagina Cluster con un nodo fermato (suite cluster) | Nodo/dischi offline evidenziati, healing visibile al riavvio |
| TD-E-16 | Trace live mentre la CLI esegue operazioni | Righe compaiono in tempo reale |
| TD-E-17 | Sessione scaduta durante l'uso | Redirect a login senza perdita della route |
| TD-E-18 | Tema scuro, lingua inglese, viewport mobile 375 px | Layout senza overflow |
| TD-E-19 | Accessibilità: `@axe-core/playwright` su tutte le pagine | Nessuna violazione serious/critical |
| TD-E-20 | Visual regression (screenshot) su pagine principali | Diff < soglia |

### Sicurezza console
| ID | Caso | Atteso |
|----|------|--------|
| TD-S-01 | Richiesta mutante senza token CSRF | 403 |
| TD-S-02 | Cookie sessione: `HttpOnly`, `Secure` (con TLS), `SameSite=Strict`; il token CSRF non sta in alcun cookie | Verificato |
| TD-S-03 | Chiave oggetto con `<script>` mostrata nel browser | Nessuna esecuzione (escaping + CSP) |
| TD-S-04 | Anteprima di file HTML/SVG caricato | Servito con `Content-Disposition: attachment` o sandbox, mai eseguito nell'origine della console |
| TD-S-05 | Header `Content-Security-Policy`, `X-Frame-Options: DENY`, `Referrer-Policy` | Presenti su index.html |
| TD-S-06 | Utente non admin chiama direttamente `/api/v1/users` | 403 (autorizzazione sempre lato server) |
| TD-S-07 | Path traversal su asset statici (`/../../etc/passwd`, encoded) | 404 |
| TD-S-08 | Pagina HTML servita dalla porta S3 dello stesso host (link presigned) che invia POST form `no-cors` all'Admin API con il cookie di sessione e un token CSRF trapelato (`/service/stop`, `/auth/logout`, `download-zip`) | Tutte rifiutate (403 `invalid_csrf_token`: `Sec-Fetch-Site` non `same-origin`); nessun cookie leggibile con il token; server e sessione intatti |

### Prestazioni dashboard
- Bundle iniziale ≤ 300 KiB gzip (Monaco e grafici lazy-loaded) — controllo `size-limit` in CI.
- Lighthouse performance ≥ 90 su Overview e Object browser.
- Object browser con 100k oggetti nel prefix: primo rendering < 1 s, scroll fluido.

## 6.6 Test di prestazioni server

| ID | Test | Obiettivo indicativo (4 nodi NVMe, 25 GbE) |
|----|------|------------------------------------------|
| TP-01 | `warp put` 64 MiB | ≥ 80% della banda di rete |
| TP-02 | `warp get` 64 MiB | ≥ 90% della banda di rete |
| TP-03 | `warp mixed` 1 KiB–1 MiB | ≥ 10k ops/s per nodo, p99 < 50 ms |
| TP-04 | Listing 1M oggetti in un prefix | ≥ 10k chiavi/s |
| TP-05 | Benchmark Go (`erasure`, `sigv4`, `xl.meta`) con confronto vs baseline (`benchstat`) | Nessuna regressione > 10% |
| TP-06 | Memoria a regime sotto carico | Stabile, nessun leak in 24 h |

## 6.7 Test di modalità, rilascio e presigned

I test specifici sono definiti nei documenti dedicati e fanno parte della pipeline:
- **Modalità e rilascio** (singolo nodo vs cluster, binario, Docker, Compose, migrazione): [08 §8.6](08-modalita-deploy.md#86-test-specifici-di-modalità-e-rilascio) — TM-01…TM-12.
- **URL presigned, POST policy, policy di accesso, share link** su server, CLI e dashboard: [09 §9.6](09-presigned-policy-condivisione.md#96-test) — TPS-*, TCL-P-*, TD-P-*.

Matrice CI:

| Suite | SNSD | SNMD (4 dischi) | Cluster (4×2, Docker) |
|-------|------|-----------------|------------------------|
| Integrazione S3/IAM/presigned | ogni PR | ogni PR | merge su `main` + notturna |
| CLI testscript | ogni PR | merge | notturna |
| Playwright dashboard | ogni PR (smoke) / merge (completa) | — | notturna |
| s3-tests / mint | notturna | notturna | notturna |
| Smoke immagine Docker e binari | ogni PR | — | ogni PR (compose up + test base) |
| Più proxy davanti al deployment (`test/proxy`, TCD-01..03, TCC-C-05; job `proxy`) | ogni PR | ogni PR | ogni PR (proxy Go in-process e nginx/HAProxy reali di `compose.multi-proxy.yml`) |

Test di **regole di accesso, domini e distribuzioni** (server, CLI, dashboard, cluster con più proxy):
[10 §10.6](10-domini-accessi-distribuzioni.md#106-test) — TAR-*, TDS-*, TCL-A-*, TCL-D-*, TD-A-*, TD-D-*, TCD-*.

Test di **cache per dominio** (server, cluster, CLI, dashboard): [11 §11.12](11-cache-domini.md#1112-test) — TCC-*, TCC-C-*, TCL-C-*, TD-C-*.

## 6.8 Matrice di tracciabilità

Ogni funzionalità del [catalogo](02-funzionalita.md) deve avere almeno un test associato. La matrice viene
mantenuta in `test/TRACEABILITY.md` (generata da tag nei test, es. `// covers: S-OBJ-06`) e controllata in CI:
una funzionalità P0/P1 senza test fa fallire la pipeline di release.
