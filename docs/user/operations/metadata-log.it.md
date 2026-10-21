# Log dei metadati (scritture piccole più veloci)

Per default ogni oggetto ha su ogni disco una directory con un file `xl.meta`: un PUT di un oggetto piccolo crea una
directory e un file, li scrive su disco e li rinomina al loro posto. Sui dischi con un flush lento (SSD e HDD da
desktop, dischi virtuali, Windows) questo limita il throughput delle scritture piccole.

Con il **log dei metadati** i metadati degli oggetti finiscono in segmenti a sola aggiunta su ogni disco. Le
scritture concorrenti condividono un solo flush, e un indice in memoria trova ogni oggetto. Gli oggetti piccoli
(fino a 128 KiB, tenuti inline) non hanno più bisogno di alcuna directory; quelli grandi tengono i dati nelle
directory dei dati.

| Misura (PUT da 4 KiB, warp, concorrenza 20, disco singolo) | file xl.meta | log dei metadati |
|---|---|---|
| Throughput | circa 1.800 oggetti/s | circa 3.800 oggetti/s |
| Latenza media | 11 ms | 5 ms |
| Latenza p99 | 30 ms | 16 ms |

Il guadagno cresce con il costo di un flush sul disco: un flush serve tutte le scritture arrivate mentre il
precedente era in corso.

## Attivarlo

```bash
NEXO_METADATA_LOG=true nexo server /data          # oppure: nexo server --metadata-log /data
```

All'avvio ogni disco locale converte una volta i suoi file `xl.meta` nel log (il log riporta `drive converted to the
metadata log`). Da lì in poi il disco usa il log finché l'opzione è attiva.

**Disattivarlo è reversibile**: avvia il server senza l'opzione e ogni disco riconverte il log in file `xl.meta`
prima di servire. Fallo prima di un downgrade a una versione senza log dei metadati, che non vedrebbe gli oggetti di
un disco in modalità log.

In un cluster imposta l'opzione su tutti i nodi.

## Impostazioni

Il sottosistema **storage** della configurazione (console **Configurazione**, oppure
`nexoctl admin config set <alias> storage …`) si applica subito:

| Chiave | Default | Significato |
|--------|---------|-------------|
| `metadata_log_segment_mib` | `64` | Dimensione dei prossimi segmenti, in MiB (1–1024). Segmenti più grandi significano meno file, più piccoli una compattazione più leggera |
| `metadata_log_memory_percent` | `25` | Quota della RAM che l'indice di un disco può usare; oltre, il server registra un avviso |

L'indice occupa circa 70 byte più la lunghezza della chiave per oggetto (circa 130 MB per un milione di oggetti con
chiavi da 60 byte).

## Monitoraggio e compattazione

`nexoctl admin metadata-log status <alias>` mostra il log di ogni disco del deployment: segmenti, quota dei byte
ancora vivi, oggetti indicizzati, memoria dell'indice, group commit dall'avvio, aree danneggiate saltate al replay.
Gli stessi valori sono nel campo `metadataLog` dei dischi di `GET /api/v1/info` (Admin API) e in Prometheus:

| Metrica (etichette `drive`, `pool`, `set`) | Significato |
|---|---|
| `nexo_metadata_log_segments`, `nexo_metadata_log_bytes`, `nexo_metadata_log_live_bytes` | Segmenti e loro byte, quelli vivi (il resto è spazzatura) |
| `nexo_metadata_log_objects`, `nexo_metadata_log_index_bytes`, `nexo_metadata_log_index_budget_bytes` | Oggetti indicizzati, memoria dell'indice e suo budget |
| `nexo_metadata_log_flushes_total`, `nexo_metadata_log_records_total`, `nexo_metadata_log_flush_seconds_total` | Group commit, record resi durevoli, tempo speso nei flush (`records/flushes` è il raggruppamento, `seconds/flushes` la latenza di un flush) |
| `nexo_metadata_log_compactions_total`, `nexo_metadata_log_compacted_bytes_total` | Segmenti compattati e byte recuperati |
| `nexo_metadata_log_damaged_areas_total` | Aree danneggiate saltate dal replay (eseguire un heal del set) |

`nexoctl admin metadata-log compact <alias>` (Admin API `POST /api/v1/storage/metadata-log/compact`, azione
`admin:Heal`) riscrive subito ogni segmento che contiene spazzatura, sui dischi del nodo che risponde: dopo una grande
cancellazione, per esempio, per restituire lo spazio senza attendere la compattazione in background. In un cluster,
eseguirlo verso ogni nodo. Come la compattazione in background, copia e rende durevoli i record vivi prima di
rimuovere un segmento.

## Come funziona

- **Segmenti**: `.nexo.sys/metalog/seg-*.log` su ogni disco. Ogni record ha un checksum CRC-32C; all'avvio i
  segmenti vengono riletti per ricostruire l'indice.
- **Group commit**: un writer per disco scrive insieme i record in attesa e li porta su disco con un solo flush; una
  scrittura viene confermata solo quando è su disco, come con i file.
- **Compattazione**: sovrascritture e cancellazioni lasciano spazzatura. Ogni 30 secondi i segmenti con meno di metà
  dei byte ancora vivi vengono riscritti e poi rimossi.
- **Erasure coding e healing** funzionano come prima: ogni disco di un set ha il suo log, e un disco sostituito o
  svuotato a server avviato riparte con un log vuoto che l'healing riempie.
- Il volume di sistema (IAM, configurazione, upload multipart) mantiene i suoi file.

## Durabilità

Una scrittura confermata sopravvive a un riavvio, a un crash del processo e a una mancanza di corrente:

- Una scrittura viene confermata solo dopo il flush del suo record (`fdatasync`); per un oggetto grande la directory
  dei dati, e ogni directory creata per lui, passa prima su disco.
- All'avvio i segmenti vengono riletti: un record troncato da un crash non era mai stato confermato e viene tagliato.
- Un record danneggiato (un settore guasto) perde solo se stesso: la rilettura riprende dal record valido successivo
  e registra `damaged area skipped`. Con l'erasure coding l'healing ripristina l'oggetto dagli altri dischi.
- La compattazione copia i record vivi, portati su disco, prima di rimuovere un segmento, e conserva le
  cancellazioni che nascondono record più vecchi finché nessun segmento più vecchio può ricomparire.
- Le conversioni nei due sensi vengono scritte per intero e portate su disco prima di rimuovere la forma precedente:
  una conversione interrotta viene rifatta all'avvio successivo.

Il design è in [docs/proposals/small-object-log.md](https://github.com/R3n3r0/Nexo/blob/main/docs/proposals/small-object-log.md).
