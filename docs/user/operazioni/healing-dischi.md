# Healing e dischi

Nelle modalità con erasure coding (SNMD e cluster) Nexo ripara da solo i dati mancanti o corrotti,
ricostruendoli dagli altri dischi del set. In SNSD non c'è ridondanza: il bitrot viene rilevato (la lettura
fallisce invece di restituire dati sbagliati) ma non riparato.

## Come funziona

| Meccanismo | Quando parte | Cosa fa |
|------------|--------------|---------|
| Heal on read | una lettura trova un blocco mancante o corrotto | risponde con i dati ricostruiti e accoda la riparazione |
| Coda MRF | una scrittura è riuscita con il quorum ma non su tutti i dischi (disco o nodo spento) | ripete la riparazione in background, con attese crescenti fino a `NEXO_HEAL_RETRY_MAX_DELAY` (5 min) |
| Scanner | ciclico (`scanner.cycle`, default 1 min di pausa tra i cicli) | calcola l'uso, applica il lifecycle, verifica i dati a campione e accoda le riparazioni |
| Disco nuovo | un disco vuoto al posto di uno del deployment (controllo ogni 30 s) | lo formatta e lo ripopola con tutti gli oggetti, con checkpoint per riprendere dopo un riavvio |
| Manuale | `nexoctl admin heal` o la pagina **Healing** della console | ripara un bucket, un prefisso o tutto, anche con verifica completa del bitrot |

Un `xl.meta` illeggibile non blocca il disco: viene spostato in `.nexo.sys/quarantine/` (mai ripulita
automaticamente, per l'analisi) e riscritto dalle copie degli altri dischi. Le scritture rifiutate per
mancanza di quorum vengono annullate sui dischi che le avevano applicate.

Lo stato è visibile in `nexoctl admin info` (riga `background healing` per nodo, dischi in healing), nella
pagina **Cluster** della console e nelle metriche `nexo_mrf_queue_*`, `nexo_heal_*`,
`nexo_drive_healing`.

## Healing manuale

```console
$ nexoctl admin heal -r --dry-run local/dati          # solo verifica, nessuna modifica
Heal 2e78a33e-…: done, scanned 6, healed 1, failed 0, 976.6KiB
  drive /mnt/disk3 (pool 1 set 1 disk 3) online: missing 1, corrupt 0, offline 0, healed 0
  object dati/f1.bin: disk 3 missing -> missing

$ nexoctl admin heal -r local/dati                    # ripara
Heal 3daac8d1-…: done, scanned 6, healed 1, failed 0, 976.6KiB
  drive /mnt/disk3 (pool 1 set 1 disk 3) online: missing 1, corrupt 0, offline 0, healed 1
  object dati/f1.bin: disk 3 missing -> ok
```

| Comando | Effetto |
|---------|---------|
| `nexoctl admin heal local` | formato dei dischi e metadati di tutti i bucket |
| `nexoctl admin heal -r local` | tutti gli oggetti di tutti i bucket |
| `nexoctl admin heal -r local/foto/2026/` | gli oggetti sotto un prefisso |
| `nexoctl admin heal -r --deep local/foto` | verifica il checksum di **ogni** blocco (legge tutti i dati: lento) |
| `nexoctl admin heal -r --remove local/foto` | elimina gli oggetti "dangling", presenti su troppo pochi dischi per essere mai più letti |
| `nexoctl admin heal --background -r local` | avvia il job e stampa l'id senza attendere |
| `nexoctl admin heal status local [ID]` | stato dei job in corso e recenti |

Il codice di uscita è 1 se il job fallisce o se qualche oggetto non si può riparare. Richiede `admin:Heal`.

## Sostituire un disco

Il disco nuovo deve essere montato **nello stesso percorso** del vecchio, vuoto, scrivibile dall'utente
del servizio. Non serve riavviare: entro 30 secondi il nodo lo riconosce, lo formatta e lo ripopola.

1. Verifica quale disco è guasto: `nexoctl admin info local` (stato `offline` o errori), metrica
   `nexo_drive_errors_total`, alert `NexoDriveOffline`.
2. Controlla che il set abbia ancora il quorum (`/nexo/health/cluster`: `writeQuorum: true`).
3. Sostituisci il disco, formattalo (XFS) e montalo nello stesso punto di mount, con il proprietario
   corretto:

   ```bash
   sudo mkfs.xfs -f /dev/sdc
   sudo mount -o noatime /dev/sdc /mnt/disk3          # e aggiorna /etc/fstab (UUID)
   sudo chown nexo:nexo /mnt/disk3 && sudo chmod 0750 /mnt/disk3
   # Docker: sudo chown 65532:65532 /mnt/disk3
   ```

4. Nei log del nodo compaiono `heal: empty drive formatted and marked for healing`, poi `heal: healing
   drive` e infine `heal: drive healed` con il numero di oggetti e byte ripristinati. Durante il
   ripopolamento il disco risulta `healing` in `admin info` e nella console.
5. Se il nodo non rileva il disco (per esempio era stato rimosso dalla configurazione del mount mentre il
   processo girava), riavvia il solo nodo: all'avvio i dischi vuoti vengono formattati e ripopolati, e un
   ripopolamento interrotto riprende dal checkpoint.

In cluster la procedura è la stessa sul nodo che ospita il disco; per sostituire **tutti** i dischi di un
nodo (nodo reinstallato) ferma il nodo, prepara i dischi vuoti e riavvialo: torna nel cluster e si
ripopola.

> **Attenzione**: non copiare mai a mano i file di un disco su un altro, e non riusare un disco di un altro
> deployment senza cancellarlo: un `format.json` di un altro deployment viene rifiutato.

## Nodo spento o irraggiungibile

Finché il quorum regge, un nodo spento non interrompe il servizio: le scritture finiscono sugli altri
dischi e la coda MRF le completa quando il nodo torna. Alla riaccensione non serve alcuna azione; per
verificare che tutto sia tornato in pari:

```bash
nexoctl admin info local                 # tutti i nodi online, coda MRF a zero
nexoctl admin heal -r --dry-run local    # nessun oggetto da riparare
```
