# Modalità di deployment

Lo stesso binario (e la stessa immagine Docker) funziona in tre modalità, scelte **dagli endpoint**
passati a `nexo server`, senza flag dedicati:

| Modalità | Comando d'esempio | Uso tipico |
|----------|-------------------|------------|
| **SNSD**: un nodo, un disco | `nexo server /srv/nexo` | sviluppo, test, edge, piccoli volumi |
| **SNMD**: un nodo, più dischi | `nexo server /mnt/disk{1...4}` | un server con più dischi, tolleranza ai guasti dei dischi |
| **Cluster**: più nodi, più dischi | `nexo server http://nexo{1...4}.lan:9000/mnt/disk{1...4}` | produzione, alta disponibilità, crescita per pool |

Regole: tutti percorsi locali e uno solo → SNSD; tutti percorsi locali e più di uno → SNMD; almeno un URL
→ cluster (allora tutti gli endpoint devono essere URL). I percorsi devono essere **assoluti**.

Tutte le funzionalità (S3, IAM, policy, presigned, versioning, object lock, lifecycle, eventi, SSE,
distribuzioni, cache, console, CLI) sono disponibili in tutte le modalità. Cambiano la protezione dei dati
e la scalabilità:

| | SNSD | SNMD | Cluster |
|-|------|------|---------|
| Guasto di dischi | affidato al sottosistema di storage (RAID, cloud volume) | ✅ fino alla parità P per set | ✅ fino alla parità P per set |
| Guasto di un nodo | ❌ | ❌ | ✅ (con nodi e quorum sufficienti) |
| Bitrot | rilevato | rilevato e riparato | rilevato e riparato |
| Healing | — | ✅ | ✅ |
| Lock e RPC tra nodi | locali | locali | distribuiti, a quorum |
| Crescita | migrazione | migrazione | aggiunta di pool |

La modalità è scritta in `format.json` su ogni disco: riavviare un disco SNSD con una topologia erasure
(o viceversa) fallisce con un errore esplicito. `nexoctl admin info` e la pagina **Cluster** della console
mostrano sempre modalità, pool, set, nodi e dischi.

## Ellissi

Gli endpoint usano le ellissi `{inizio...fine}` (tre punti), espanse da Nexo stesso:

| Forma | Espansione |
|-------|-----------|
| `/mnt/disk{1...4}` | `/mnt/disk1` … `/mnt/disk4` |
| `/mnt/disk{01...16}` | `/mnt/disk01` … `/mnt/disk16` (lo zero iniziale fissa la larghezza) |
| `http://node{a...d}:9000/data` | `http://nodea:9000/data` … `http://noded:9000/data` |
| `http://nexo{1...4}.lan:9000/mnt/disk{1...4}` | 16 endpoint, 4 per nodo |

- Un argomento con ellissi è **un pool**; più argomenti con ellissi sono più pool. Argomenti senza ellissi
  (`/mnt/a /mnt/b /mnt/c /mnt/d`) formano un unico pool; non si possono mescolare argomenti con e senza
  ellissi.
- Bash non espande `{1...4}` (con tre punti); con altre shell, o per sicurezza, metti l'argomento tra
  virgolette: `nexo server "/mnt/disk{1...4}"`.
- In cluster ogni nodo esegue **lo stesso comando** e riconosce i propri endpoint dai suoi nomi host o
  indirizzi e dalla porta di `--address`.

## SNSD: un disco

```bash
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD=supersegreta nexo server /srv/nexo
```

Gli oggetti sono file sul file system (con `xl.meta` per metadati e versioni), senza erasure coding: la
protezione dai guasti è quella del disco sottostante. Adatto a sviluppo, test, edge e volumi già
ridondati. Per passare a SNMD o cluster serve una migrazione (vedi
[Backup e migrazione](operations/backup-migration.md)).

## SNMD: più dischi su un nodo

```bash
nexo server "/mnt/disk{1...4}"
```

I dischi formano uno o più **erasure set** di 2–16 dischi; ogni oggetto è diviso in D blocchi di dati e P
di parità (Reed-Solomon) distribuiti sui dischi del suo set, con un checksum per blocco (bitrot). Nessuna
rete coinvolta: lock locali, nessun overhead RPC.

## Cluster

```bash
# su ognuno dei 4 nodi, identico
NEXO_ROOT_USER=admin NEXO_ROOT_PASSWORD='<uguale su tutti i nodi>' \
  nexo server "http://nexo{1...4}.lan:9000/mnt/disk{1...4}"
```

All'avvio i nodi si cercano (**bootstrap**): verificano che topologia, versione e configurazione KMS
coincidano, attendono il quorum dei dischi di ogni set e formattano i dischi nuovi. Finché il bootstrap non
è completo un nodo risponde `503` (anche `/nexo/health/ready`), quindi i nodi possono partire in qualsiasi
ordine. Il primo nodo dell'elenco formatta il deployment appena raggiunge il quorum; un nodo che parte dopo
che gli altri lo hanno già formattato attende qualche secondo (INFO `empty local drives, formatting them once
settled`), formatta i propri dischi, registra a livello INFO `drive joined a fresh deployment, healing its system
configuration` e copia la configurazione già scritta dagli altri: non è un guasto (i dischi sostituiti di un
deployment con dati sono invece segnalati con un WARN, `empty local drives in an existing deployment`).
Un nodo avviato con argomenti diversi dalla maggioranza non entra nel cluster ed esce con un errore che
elenca i nodi in disaccordo e, per ciascuno, gli endpoint presenti solo sulla riga di comando di questo nodo
(`only on this node`) o solo su quella del nodo in disaccordo (`only on that node`).

Tutti i nodi sono simmetrici: qualsiasi nodo serve qualsiasi richiesta S3 o della console, quindi davanti
si mette un load balancer (vedi `deploy/docker/nginx.conf` e
[`deploy/proxy`](https://github.com/R3n3r0/Nexo/tree/main/deploy/proxy)) con health check su
`/nexo/health/ready`.

### Come si formano i set

Ogni pool è diviso in erasure set della stessa dimensione: Nexo sceglie la dimensione più grande tra 2 e 16
che divide il numero di dischi del pool e distribuisce ogni set in modo uniforme sui nodi.

| Pool | Set | Parità di default | Dischi per oggetto (D+P) |
|------|-----|-------------------|--------------------------|
| 1 nodo × 4 dischi | 1 × 4 | EC:2 | 2+2 |
| 1 nodo × 3 dischi | 1 × 3 | EC:1 | 2+1 |
| 4 nodi × 2 dischi | 1 × 8 | EC:4 | 4+4 |
| 4 nodi × 4 dischi | 1 × 16 | EC:4 | 12+4 |
| 8 nodi × 4 dischi | 2 × 16 | EC:4 | 12+4 |
| 6 nodi × 2 dischi | 1 × 12 | EC:4 | 8+4 |

Controlla in anticipo il risultato con `nexo server --dry-run <endpoint>`:

```text
configuration OK: mode=distributed s3=:9000 console=:9001 endpoints=http://nexo{1...4}.lan:9000/mnt/disk{1...4}
  node: nexo1.lan:9000 (4 node(s))
  pool 1: 1 erasure set(s) of 16 drive(s), parity 4
  local drives: 4 writable, 0 formatted
```

### Scelta della parità

La parità di default dipende dalla dimensione del set: 2 fino a 5 dischi, 3 per 6–7, 4 da 8 in su (mai più
della metà dei dischi). Si cambia con `--storage-class-standard EC:n` (`NEXO_STORAGE_CLASS_STANDARD`); la
classe `REDUCED_REDUNDANCY` ha parità 1 se non configurata (`--storage-class-rrs`, al più quella di
`STANDARD`). Un client sceglie la classe per oggetto con l'header S3 `x-amz-storage-class`.

| Set di N dischi, parità P | Effetto |
|---------------------------|---------|
| Capacità utile | (N − P) / N della capacità grezza |
| Letture | possibili con almeno D = N − P dischi del set |
| Scritture | possibili con almeno D dischi (D + 1 quando D = P, per evitare lo split-brain) |
| Lock (cluster) | scritture con N/2 + 1 nodi raggiungibili |

Più parità = più guasti tollerati e meno capacità. Esempi:

- **4 nodi × 4 dischi, EC:4**: capacità utile 75%; si perdono fino a 4 dischi qualsiasi, o un nodo
  intero, senza interruzioni.
- **4 nodi × 2 dischi, EC:4**: capacità utile 50%; con un nodo spento letture e scritture continuano; con
  due nodi spenti le letture sono ancora possibili ma le scritture no (quorum dei dati e dei lock).
- **SNMD 4 dischi, EC:2**: capacità utile 50%; si scrive con 3 dischi su 4 e si legge con 2.

Ogni versione ricorda la parità con cui è stata scritta: cambiare la classe vale per i nuovi oggetti.

### Dimensionamento

- **Stessi dischi** per numero e dimensione su tutti i nodi di un pool: lo spazio utile di un set è
  limitato dal disco più piccolo.
- Dischi dedicati formattati **XFS**, montati con `noatime`, **niente RAID** sotto Nexo (la ridondanza la
  fa l'erasure coding) e niente LVM che unisca più dischi.
- Per continuare a scrivere con un nodo spento servono almeno **3 nodi** (i lock richiedono N/2 + 1 nodi)
  e una parità almeno pari ai dischi che un nodo ha in ogni set (con i default è così da 3 nodi × 4 dischi
  e da 4 nodi × 2 dischi in su); con 2 nodi lo spegnimento di uno blocca le scritture. 4 o più nodi
  lasciano margine per la manutenzione.
- Prevedi spazio libero: i nuovi oggetti evitano i pool oltre il 95%; l'alert `NexoCapacityHigh` scatta
  all'85% (vedi [Monitoraggio](operations/monitoring.md)).
- La cache delle distribuzioni (`--cache-dir`) va su un disco separato (SSD/NVMe), mai su un disco dati.
- Per misurare dischi e rete prima di andare in produzione: `nexoctl admin speedtest local drive|object|net`.

### Rete

| Porta | Chi la usa | Note |
|-------|-----------|------|
| 9000 | client S3, load balancer, **tutti gli altri nodi** (RPC su `/nexo/rpc/v1/`) | deve essere aperta tra i nodi |
| 9001 | browser (console), `nexoctl admin`, Admin API | non serve tra i nodi |

- Ogni nome host degli endpoint deve risolvere all'indirizzo del nodo **su ogni nodo** (DNS o `/etc/hosts`).
- Il traffico tra i nodi è HTTP/2 autenticato da token a breve scadenza legati alla singola chiamata; con
  endpoint `http://` viaggia in chiaro (il nodo lo segnala all'avvio): usalo solo su una rete privata
  fidata.
- Il proxy pubblico non deve mai inoltrare `/nexo/rpc/` (le configurazioni di `deploy/proxy` lo bloccano).
- Orologi sincronizzati (NTP): un nodo con più di 2 s di differenza viene segnalato nei log e in
  `nexoctl admin info`; le richieste firmate tollerano 15 minuti.

### TLS tra i nodi

1. Prepara su ogni nodo una directory con `public.crt` e `private.key`, con un certificato che copra i nomi
   degli endpoint (`nexo1.lan` … `nexo4.lan`) e i nomi pubblici, e le CA fidate in `CAs/`.
2. Avvia ogni nodo con `NEXO_CERTS_DIR` (o `--certs-dir`) ed endpoint `https://`:

   ```bash
   NEXO_CERTS_DIR=/etc/nexo/certs nexo server "https://nexo{1...4}.lan:9000/mnt/disk{1...4}"
   ```

3. Per l'autenticazione reciproca aggiungi `--tls-internode-verify` (`NEXO_TLS_INTERNODE_VERIFY=true`): le
   chiamate tra nodi senza un certificato client verificato con le CA di `CAs/` sono rifiutate (il
   certificato deve ammettere l'uso client).

Dettagli e rotazione dei certificati in [TLS e certificati](security/tls.md).

## Espansione con un pool

Un cluster cresce **aggiungendo un pool** (un gruppo di nodi e dischi), senza spostare dati:

```bash
# prima: un pool
nexo server "http://nexo{1...4}.lan:9000/mnt/disk{1...4}"
# dopo: due pool, lo stesso comando su tutti gli 8 nodi
nexo server "http://nexo{1...4}.lan:9000/mnt/disk{1...4}" "http://nexo{5...8}.lan:9000/mnt/disk{1...4}"
```

Gli oggetti esistenti restano nel loro pool (anche le sovrascritture); i nuovi oggetti vanno nei pool in
proporzione allo spazio libero. La procedura completa è in
[Aggiornamento ed espansione](operations/upgrade-expansion.md#espansione-con-un-nuovo-pool). Per
distribuire gli oggetti esistenti anche sul nuovo pool, o per ritirare un pool, vedi
[Ribilanciamento](operations/upgrade-expansion.md#ribilanciamento-tra-pool) e
[Dismissione di un pool](operations/upgrade-expansion.md#dismissione-di-un-pool).

## Da una modalità all'altra

| Da → A | Come |
|--------|------|
| SNSD → SNMD o cluster | nuovo deployment + `nexoctl admin cluster export/import` + `nexoctl mirror --preserve` |
| SNMD → cluster | come sopra |
| Cluster → cluster più grande | aggiunta di un pool, nessuna migrazione |

Vedi [Backup e migrazione](operations/backup-migration.md).
