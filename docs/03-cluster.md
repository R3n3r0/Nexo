# 3. Clusterizzazione

## 3.1 Modello scelto

Nexo adotta un modello **shared-nothing a topologia statica per pool** (simile a MinIO):

- Un **deployment** è composto da uno o più **server pool**.
- Un **pool** è un insieme di nodi × dischi definito all'avvio con l'espansione a ellissi:
  ```bash
  nexo server http://node{1...4}.example.com:9000/mnt/disk{1...4}
  # espansione: si aggiunge un secondo pool
  nexo server http://node{1...4}.example.com:9000/mnt/disk{1...4} \
              http://node{5...8}.example.com:9000/mnt/disk{1...8}
  ```
- Ogni pool è suddiviso in **erasure set** di dimensione fissa (2–16 dischi), scelta come il massimo
  divisore comune che distribuisce i dischi uniformemente tra i nodi (un set usa dischi su nodi diversi).
- Ogni oggetto appartiene a **un solo erasure set** di **un solo pool**.
- Tutti i nodi sono **simmetrici**: qualsiasi nodo accetta qualsiasi richiesta S3 (dietro a un load balancer).
- **Nessun nodo master, nessun DB di metadati**.

### Perché non consistent hashing dinamico?
| Aspetto | Pool statici (scelto) | Consistent hashing + membership dinamica |
|---------|----------------------|------------------------------------------|
| Complessità | Bassa | Alta (gossip, ribilanciamento, versioni ring) |
| Espansione | Aggiunta di un pool, nessun movimento dati | Movimento dati proporzionale |
| Prevedibilità prestazioni | Alta | Ribilanciamenti impattano il traffico |
| Granularità crescita | Per pool (gruppo di nodi) | Per nodo |

Il ribilanciamento tra pool resta disponibile come operazione esplicita (S-STO-10).

## 3.2 Placement

1. **Selezione del pool** (solo per nuovi oggetti):
   - se l'oggetto esiste già (qualsiasi versione) in un pool → stesso pool;
   - altrimenti scelta casuale pesata sullo spazio libero, escludendo pool in decommission o > 95% pieni.
2. **Selezione del set**: `set = SipHash-2-4(objectKey, deploymentID) mod numSets`.
3. **Distribuzione degli shard**: permutazione dei dischi del set derivata da `crc32(key)` per bilanciare
   data/parity tra i dischi.

## 3.3 Erasure coding e quorum

Per un set di **N** dischi con parità **P** (default: `N ≤ 5 → P=2`, `6–7 → 3`, `≥ 8 → 4`) e dati **D = N − P**:

| Operazione | Quorum |
|------------|--------|
| Lettura | `D` dischi con shard coerenti |
| Scrittura | `D` dischi (`D+1` se `D == P`, per evitare split-brain) |
| Lock | `N_nodi/2 + 1` dei lock server coinvolti |

Esempio 4 nodi × 4 dischi = 16 dischi, 1 set da 16 con EC:4 → tollera la perdita di 4 dischi (o 1 nodo intero)
in lettura e scrittura; capacità utile 75%.

## 3.4 Comunicazione internode

- Trasporto: HTTP/2 sulla stessa porta S3 sotto `/nexo/rpc/v1/`, mTLS opzionale (raccomandato).
- Autenticazione: JWT a breve durata (20 s) firmato con chiave derivata dalle credenziali root (HMAC) + verifica
  deployment ID; rifiuto di nodi con versione/format incompatibile. La firma è verificata prima di qualsiasi
  claim; il token è legato al nodo destinatario (`aud`) e a **una sola chiamata**: metodo e target (path e
  query, `htm`/`htu`), hash SHA-256 degli argomenti (`bh`: il corpo msgpack, o il primo frame di un upload,
  verificato dalle rotte) e id univoco (`jti`) accettato una volta sola (cache degli id fino alla scadenza);
  un token intercettato non vale per nessun'altra chiamata né per ripetere la stessa. Lo skew tollerato è
  15 s, le risposte d'errore non rivelano mai il deployment ID.
- Legame al deployment: un nodo con i dischi formattati legge il deployment ID dal proprio `format.json`
  **prima di aprire il listener**; un nodo con i dischi vuoti si lega appena lo conosce: quando un peer gli
  scrive il `format.json` o quando il suo bootstrap lo legge o lo genera (il nodo che formatta il deployment
  passa al nuovo ID prima di scrivere i formati sui peer). Un chiamante senza deployment ID (bootstrap)
  accede solo alle chiamate di bootstrap (ping, `bootstrap/*`, lettura e stat di `format.json` senza glob,
  stato del disco: niente elenchi di volumi o di oggetti); finché un nodo non conosce il proprio deployment
  accetta da qualunque chiamante solo le stesse chiamate più la scrittura di `format.json` e del tracker di
  healing. Questa restrizione protegge da errori di configurazione (un nodo di un altro deployment con le
  stesse credenziali root), non da un attaccante: chi ha le credenziali root può sempre ottenere il
  deployment ID (`bootstrap/config`) e firmare token validi.
- Rifiuti: ogni rifiuto (token, deployment, versione, certificato client) è registrato a livello Warn con
  indirizzo remoto e path (mai il token), al più una volta per indirizzo e motivo al minuto, e contato in
  `nexo_rpc_auth_refused_total{reason}`; un indirizzo che accumula 20 fallimenti di autenticazione viene
  rifiutato senza verifica (429) finché il credito non si ricarica (uno ogni 3 s): il server non è un oracolo
  veloce sulla password root.
- Trasporto: solo HTTP/2. Con endpoint `http` il traffico e i token sono in chiaro (avviso all'avvio): la
  legatura dei token limita un'intercettazione passiva, non un attaccante attivo, quindi TLS è necessario su
  reti non fidate. Con `--tls-internode-verify` (`NEXO_TLS_INTERNODE_VERIFY`, endpoint `https`) il TLS è
  **mutuo**: ogni nodo presenta il proprio certificato (`public.crt`, con uso client) e le chiamate internode
  senza un certificato client verificato con le CA di `certs/CAs` sono rifiutate (le richieste S3 sulla stessa
  porta non ne presentano). Si è scelto di non rendere obbligatorio un flag di opt-in per gli endpoint `http`
  (resta l'avviso), per non rompere i deployment su reti private. Il proxy pubblico non inoltra mai
  `/nexo/rpc/`, e una richiesta virtual-host (`<bucket>.<dominio>/nexo/rpc/v1/...`) è una chiave S3, non una
  chiamata internode. Le risposte unarie dei peer sono limitate a 64 MiB.
- Servizi RPC:
  | Servizio | Scopo |
  |----------|-------|
  | `storage` | Accesso remoto ai dischi (ReadVersion, WriteMetadata, CreateFile stream, ReadFileStream, RenameData, Walk, DiskInfo, Delete…) |
  | `lock` | Lock/Unlock/RLock/RUnlock/Refresh/ForceUnlock |
  | `peer` | Invalidazione cache (IAM, bucket metadata, config), server info, trace/log stream, signal (restart), metriche locali |
  | `bootstrap` | Verifica coerenza della configurazione di avvio tra nodi |
- Codifica: msgpack; stream binari per dati con framing e checksum.
- Connection pooling, timeout per chiamata, circuit breaker per disco/nodo offline con riconnessione in background.

## 3.5 Bootstrap del cluster

1. Ogni nodo espande gli endpoint e verifica che la lista sia identica su tutti i nodi (hash della topologia via `bootstrap` RPC).
2. Attesa che almeno il **quorum dei dischi** di ogni set sia raggiungibile (retry con backoff, log chiaro di cosa manca).
3. Se nessun `format.json` esiste → il nodo con il primo endpoint genera il formato (deployment ID, mappa set/dischi) e
   lo scrive su tutti i dischi; gli altri attendono.
4. Dischi nuovi/vuoti in un formato esistente → marcati per **healing** e ripopolati.
5. Caricamento IAM/config dal bucket di sistema, avvio servizi di background (scanner, heal, ILM, replica) con
   **leader election leggera** tramite lock distribuito (`leader` lock con lease) per i job che devono girare una sola volta.

## 3.6 Lock distribuiti (dsync)

- Ogni nodo espone un lock server in memoria.
- Un lock è acquisito se `≥ n/2+1` lock server rispondono positivamente entro il timeout; altrimenti rilascio e retry con jitter.
- **Lease + refresh** ogni 10 s: se il refresh perde il quorum, l'operazione viene annullata (context cancel).
- Lock "orfani" scadono automaticamente dopo 1 min senza refresh.
- Read lock multipli concorrenti, write lock esclusivi.
- `admin lock list/clear` per diagnostica.

## 3.7 Healing

| Meccanismo | Trigger | Azione |
|------------|---------|--------|
| Heal-on-read | Lettura rileva shard mancante/corrotto (bitrot) | Ricostruisce la risposta dai shard rimanenti, accoda heal |
| MRF (Most Recently Failed) | Scrittura riuscita con quorum ma non su tutti i dischi | Coda in memoria (persistita periodicamente) ritentata in background |
| Scanner | Ciclico, con throttling adattivo | Calcolo usage, applicazione ILM, verifica a campione dei shard, heal |
| Disk replacement | Disco nuovo senza `format.json` | Healing completo del disco con checkpoint (`healing.bin`) e ripresa dopo restart |
| Manuale | `nexoctl admin heal` | Heal di bucket/prefix, `--deep` per verifica bitrot completa |

- Un `xl.meta` non decodificabile non blocca il disco: alla prima scrittura o cancellazione viene spostato in
  quarantena (`.nexo.sys/quarantine/`, mai ripulita automaticamente, per l'analisi) e trattato come assente;
  heal-on-read, MRF, `HealObject` e l'healing del disco lo riscrivono da quello degli altri dischi con tutte
  le versioni.
- Le scritture e le cancellazioni rifiutate per mancanza di quorum sono annullate sui dischi che le avevano
  applicate (versione precedente ripristinata), per non lasciare una minoranza divergente.

## 3.8 Espansione, decommission, rebalance

- **Espansione**: si riavviano tutti i nodi con la nuova lista di pool (rolling restart). I nuovi pool vengono
  formattati; i nuovi oggetti si distribuiscono pesati sullo spazio libero.
- **Decommission** (P2): il pool viene marcato read-only, un job distribuito sposta ogni oggetto (tutte le versioni)
  negli altri pool, con stato persistito e ripristinabile; al termine il pool può essere rimosso dalla command line.
- **Rebalance** (P2): sposta oggetti dai pool più pieni verso i meno pieni fino a una soglia di bilanciamento.

## 3.9 Coerenza

- **Read-after-write strong consistency** per PUT/DELETE di nuovi oggetti e sovrascritture (grazie a lock + quorum).
- Listing **consistente** con merge degli stream dai dischi del set (walk ordinato + quorum di listing),
  con cache di listing opzionale per prefissi molto grandi.
- Metadati di bucket/IAM: scritti su quorum, poi invalidazione cache via `peer` RPC; reload periodico come rete di sicurezza.

## 3.10 Multi-sito (P2)

- **Bucket replication**: asincrona, per regola (prefix/tag), attivo-attivo con risoluzione last-writer-wins su mod-time
  + version-id, resync esplicito.
- **Site replication**: sincronizza IAM, policy, configurazione bucket tra N cluster; ogni bucket creato viene replicato.

## 3.11 Scenari di guasto da supportare

| Scenario | Comportamento atteso |
|----------|----------------------|
| 1 disco offline | Letture/scritture OK, heal al rientro |
| P dischi offline in un set | Letture/scritture OK (scritture se quorum D rispettato) |
| > P dischi offline | Errore `503 SlowDown`/`XNexoQuorum` per quel set, altri set funzionanti |
| 1 nodo offline (4 nodi) | Cluster pienamente operativo |
| Partizione di rete 2+2 | Nessuna scrittura possibile senza quorum lock (no split-brain); letture se quorum dati raggiungibile |
| Disco corrotto (bitrot) | Rilevato in lettura, risposta corretta, shard riparato |
| Crash durante la scrittura | Nessun oggetto parziale visibile; tmp ripuliti all'avvio e, durante il servizio, dal janitor (voci di `.nexo.sys/tmp` più vecchie di 24 h, controllo ogni ora) |
| Crash durante il completamento multipart | Il completamento scrive prima un journal (`completing.json`) nella directory dell'upload e sposta le parti in una directory di staging; al nuovo tentativo l'upload viene concluso (versione già committata) o le parti tornano al loro posto: l'upload resta completabile |
| Parti multipart su dischi diversi | Ogni parte richiede `D` dischi che la contengono integra; i dischi che non la hanno la ricevono ricostruita dagli shard prima del commit |
| Clock skew tra nodi | Avviso se > 2 s; SigV4 tollera 15 min; lock indipendenti dal clock |
