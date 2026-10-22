# Tier remoti

Un tier remoto è un bucket S3 di un altro server (un'altra installazione Nexo, MinIO, AWS S3, qualsiasi storage
compatibile S3) in cui il ciclo di vita dei bucket sposta i dati delle versioni vecchie per liberare i dischi
locali. Una versione trasferita conserva i suoi metadati sui dischi (chiave, version id, dimensione, ETag,
checksum, tag, retention, cifratura) e viene letta dal suo tier in modo trasparente: GET, HEAD, range, copie e
replica funzionano come prima. La sua classe di storage diventa il nome del tier.

## Aggiungere un tier

Crea un alias per il server remoto, con credenziali che possano leggere, scrivere, elencare ed eliminare nel
bucket, poi aggiungi il tier:

```console
$ nexoctl alias set archive https://s3.archive.example.com ARCHIVEKEY ARCHIVESECRET
$ nexoctl admin tier add local COLD archive/cold-data --prefix nexo/
Tier COLD added: https://s3.archive.example.com/cold-data/nexo.
$ nexoctl admin tier ls local
NAME             ENDPOINT                                 BUCKET                   CLASS          ACCESS KEY
COLD             https://s3.archive.example.com           cold-data/nexo           -              ARCHIVEKEY
```

Il nome del tier (lettere maiuscole, cifre, `-` e `_`) è la classe di storage indicata dalle regole del ciclo di
vita. Il bucket remoto viene verificato con le credenziali, che il server conserva cifrate. `--storage-class`
imposta la classe di storage degli oggetti remoti: usa una classe leggibile subito (`STANDARD_IA`,
`GLACIER_IR`), non una classe d'archivio che richiede un restore. I tier si gestiscono anche in console, nella
pagina **Cluster**.

`admin tier edit local COLD archive2` sostituisce le credenziali con quelle di un altro alias (endpoint, bucket e
prefisso di un tier non cambiano mai). Le azioni sono `admin:ListTier` e `admin:SetTier`.

## Regole di transizione

Una `Transition` sposta le versioni correnti un certo numero di giorni dopo la loro creazione, una
`NoncurrentVersionTransition` quelle non correnti un certo numero di giorni dopo che sono diventate non correnti:

```console
$ nexoctl ilm rule add --prefix logs/ --transition-days 30 --transition-tier COLD local/app
$ nexoctl ilm rule add --id old-versions --noncurrent-transition-days 7 --transition-tier COLD local/app
```

Le stesse regole si impostano con `PutBucketLifecycleConfiguration` (`StorageClass` è il nome del tier) e nella
scheda **Lifecycle** del bucket. Una regola che non indica alcun tier viene rifiutata (`InvalidStorageClass`).
Lo scanner applica le regole in background; `nexoctl ilm apply local/app` le applica subito.

Quando sono dovute sia una scadenza sia una transizione, vince la scadenza. Le versioni salvate inline (oggetti
piccoli, 128 KiB di default) e quelle vuote restano sui dischi.

## Come vengono protetti i dati

- I byte salvati della versione (cifrati e compressi come sui dischi: il tier non vede mai in chiaro un oggetto
  cifrato) vengono scritti sul tier come un unico oggetto, sotto `<prefisso><deployment id>/`, e la loro
  dimensione viene verificata lì.
- Solo dopo la versione registra il suo tier, sotto il suo lock di scrittura, se è ancora la stessa versione;
  poi i dati locali vengono rilasciati. Una transizione interrotta tra i due passi viene completata
  dall'healing.
- Una versione sostituita o eliminata lascia dietro di sé il suo oggetto sul tier: il leader rimuove, ogni ora,
  gli oggetti del tier a cui nessuna versione fa più riferimento e più vecchi di un'ora. Gli oggetti di altre
  installazioni che condividono il bucket non vengono mai toccati.
- Un tier che contiene i dati di versioni trasferite non può essere rimosso.

Per riportare sui dischi i dati di un oggetto, copialo su se stesso con qualsiasi client S3:

```console
$ aws --endpoint-url http://localhost:9000 s3 cp s3://app/logs/2025-01.log s3://app/logs/2025-01.log \
    --storage-class STANDARD
```

Il tier remoto fa parte della durabilità delle versioni trasferite: proteggilo come un backup (versioning,
replica o le garanzie del provider). Una versione il cui tier non è raggiungibile non può essere letta finché il
tier non torna disponibile.
