# Replica dei bucket

Un bucket può replicare i suoi oggetti in un bucket di un altro server: un'altra installazione di Nexo o un
qualsiasi server S3 che accetti scritture con versioning. La copia è asincrona: la scrittura viene confermata
appena la sorgente l'ha salvata, e una coda in background la invia a ogni destinazione.

## Cosa viene replicato

Per ogni nuova versione selezionata da una regola:

- i dati, i metadati, il content type e i tag della versione, con **ID di versione, data di modifica ed ETag**
  identici a quelli della sorgente;
- i **delete marker** (salvo che la regola li escluda) e, se la regola lo chiede, le **cancellazioni
  definitive** di versioni;
- le modifiche successive dei tag.

Gli oggetti cifrati con una chiave fornita dal client (SSE-C) non vengono replicati: il server non ha mai
quella chiave. L'ETag di un upload multipart viene mantenuto solo per una versione non cifrata; una cifrata
viene salvata con l'ETag calcolato dalla destinazione.

Ogni versione mostra il suo stato nell'header `x-amz-replication-status` di `HEAD` e `GET`: `PENDING`, poi
`COMPLETED` (o `FAILED` esauriti i tentativi) sulla sorgente, e `REPLICA` sulla destinazione.

## Requisiti

- **Versioning** abilitato su entrambi i bucket. Finché un bucket ha regole di replica, il suo versioning non
  può essere sospeso.
- Credenziali per la destinazione con `s3:PutObject`, `s3:DeleteObject` e `s3:PutObjectTagging` sul bucket
  remoto. Se la destinazione è un server Nexo verifica anche `s3:ReplicateObject`, `s3:ReplicateDelete` e
  `s3:ReplicateTags`: queste azioni permettono di impostare l'ID e la data della versione.
- Sulla sorgente, `admin:SetBucketTarget` per registrare una destinazione e `s3:PutReplicationConfiguration`
  per le regole (`admin:GetBucketTarget` e `s3:GetReplicationConfiguration` per leggerle,
  `s3:ResetBucketReplicationState` per una risincronizzazione).

## Configurazione con nexoctl

Crea un alias per ogni server, poi replica un bucket nel bucket dell'altro alias:

```bash
nexoctl alias set site-a https://a.example.com AKIA... ...
nexoctl alias set site-b https://b.example.com AKIB... ...
nexoctl replicate add site-a/photos site-b/photos
```

`replicate add` salva su `site-a` URL e credenziali di `site-b` (cifrate, non più mostrate) e aggiunge una
regola. Le opzioni scelgono cosa inviare:

| Opzione | Effetto |
| --- | --- |
| `--prefix docs/` | solo le chiavi sotto il prefisso |
| `--tags class=public` | solo gli oggetti con questi tag (tutti) |
| `--priority N` | se più regole verso la stessa destinazione selezionano la stessa chiave, vince la più alta |
| `--no-delete-markers` | i delete marker restano solo sulla sorgente |
| `--deletes` | cancella le versioni anche sulla destinazione quando vengono cancellate sulla sorgente |
| `--existing` | permette a `replicate resync` di inviare le versioni scritte prima della regola |
| `--disable` | aggiunge la regola disabilitata |
| `--id ID` | nome della regola (predefinito `REMOTE_ALIAS-BUCKET`); lo stesso ID sostituisce la regola |

`nexoctl replicate ls site-a/photos` elenca le regole, `nexoctl replicate status site-a/photos` mostra coda,
versioni replicate ed errori per ogni destinazione. `nexoctl replicate rm site-a/photos --id site-b-photos`
rimuove una regola (`--all` le rimuove tutte) e le destinazioni non più usate. Ciò che è già stato replicato
resta sulla destinazione.

La scheda **Replica** di un bucket nella console fa lo stesso: destinazioni, regole, stato e risincronizzazione.

## Attivo-attivo

Replica ciascun bucket verso l'altro:

```bash
nexoctl replicate add site-a/shared site-b/shared --deletes
nexoctl replicate add site-b/shared site-a/shared --deletes
```

Una versione scritta su un lato arriva all'altro come replica e non viene mai rimandata indietro. Se la stessa
chiave viene scritta quasi insieme sui due lati, entrambi finiscono con entrambe le versioni e **la più
recente (per data di modifica) è quella corrente su tutti e due**: vince l'ultima scrittura. Tieni gli orologi
dei server sincronizzati (NTP).

## Oggetti esistenti

Una regola replica le versioni scritte dopo di lei. Per inviare quelle precedenti, aggiungi la regola con
`--existing` e avvia una risincronizzazione:

```bash
nexoctl replicate resync site-a/photos site-b/photos
```

La risincronizzazione scorre tutte le versioni del bucket, chiede alla destinazione se le ha già e accoda
quelle mancanti. Gira su un nodo del cluster, sopravvive ai riavvii e si segue con `replicate status`.

## Errori

Una versione che non può essere inviata (destinazione irraggiungibile, credenziali rifiutate) resta in coda e
viene ritentata con un ritardo crescente, da un secondo fino a cinque minuti, per circa trenta tentativi; poi
viene marcata `FAILED` e una risincronizzazione la invia di nuovo. La coda è salvata sui dischi del nodo che ha
ricevuto la scrittura, quindi un riavvio non la perde.

## Limitazioni

- I contatori di `replicate status` sono quelli del nodo che risponde: in un cluster ogni nodo invia le
  scritture che ha ricevuto.
- Una modifica dei soli metadati (diversi dai tag) non viene replicata; una copia sulla stessa chiave sì.
- Le destinazioni e le loro credenziali non fanno parte di `admin cluster export`. Sono cifrate con una chiave
  derivata dalle credenziali root: dopo aver cambiato la password root, registra di nuovo le destinazioni.
- La [replica di sito](site-replication.md) mantiene sincronizzati IAM, policy e tutti i bucket tra più
  installazioni.

## Admin API e API S3

Le regole sono servite anche dall'API S3 (`PUT/GET/DELETE /{bucket}?replication`, con l'ARN di una destinazione
come bucket di destinazione). L'Admin API ha `/buckets/{bucket}/replication`, `/replication/targets`,
`/replication/status` e `/replication/resync` (vedi il [riferimento dell'Admin API](reference/admin-api.md)).
