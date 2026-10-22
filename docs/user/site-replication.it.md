# Replica di sito

La replica di sito mantiene sincronizzate più installazioni di Nexo (siti), per il disaster recovery o per
servire gli utenti dalla regione più vicina. Ogni sito ha gli stessi:

- utenti, gruppi e access key, con i loro segreti e le loro policy;
- policy con nome;
- bucket, con versioning, object lock, policy, lifecycle, cifratura, quota, tag, CORS e regole media;
- oggetti: ogni bucket si replica nello stesso bucket di ogni altro sito, in modalità attivo-attivo (vedi
  [replica dei bucket](replication.md)).

Una modifica fatta su un sito qualsiasi (un utente creato, una policy modificata, un bucket cancellato, un
oggetto scritto) raggiunge gli altri in pochi secondi. Un sito che era irraggiungibile si riallinea quando torna
disponibile.

La configurazione (`admin config`), le chiavi KMS, i target delle notifiche, le distribuzioni e i certificati
restano propri di ogni sito.

## Requisiti

- Ogni sito usa Nexo ed è raggiungibile dagli altri sulla sua API S3 e sulla sua Admin API (il listener della
  console).
- I siti **condividono le credenziali root** (`NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD`): si chiamano a vicenda
  con queste e cifrano ciò che si scambiano con una chiave derivata da esse.
- Gli orologi sono sincronizzati (NTP): vince la modifica più recente.

## Creare un gruppo

Crea un alias per ogni sito, poi raggruppali da uno qualsiasi:

```bash
nexoctl alias set site-a https://s3.a.example.com nexoadmin '…' --admin-url https://console.a.example.com
nexoctl alias set site-b https://s3.b.example.com nexoadmin '…' --admin-url https://console.b.example.com
nexoctl admin replicate add site-a site-b
```

I nomi degli alias diventano i nomi dei siti. Il primo sito verifica di raggiungere tutti gli altri, salva il
gruppo su ognuno e avvia un primo giro che **aggiunge soltanto** ciò che manca a ciascun sito, senza mai
cancellare nulla. Utenti, policy e bucket già presenti su un sito finiscono su tutti, e gli oggetti scritti
prima del gruppo vengono inviati da una risincronizzazione di ogni bucket.

Nella console, la pagina **Cluster** ha un pannello **Replica di sito** per creare il gruppo, seguirlo e
rimuoverlo. `nexoctl admin replicate info site-a` mostra i siti e lo stato della sincronizzazione con ognuno.

## Come funziona

Il leader di ogni sito esegue ogni pochi secondi un giro con ogni altro sito. Per ogni elemento (un utente, un
gruppo, una access key, una policy, un bucket) confronta i due lati con ciò che entrambi avevano dopo l'ultimo
giro:

- modificato su un solo lato: la modifica viene applicata sull'altro;
- modificato su entrambi: vince la modifica più recente (per un elemento senza data, come un bucket, il sito con
  l'ID di deployment più piccolo);
- cancellato su un lato e modificato sull'altro: vince la modifica, l'elemento ritorna;
- assente su un lato al primo giro: viene aggiunto, mai cancellato.

Poi ogni bucket riceve il versioning e una regola di replica (`site-<nome>`) verso lo stesso bucket di ogni
altro sito, con le credenziali root; gli oggetti già presenti vengono risincronizzati. Queste regole compaiono
in `nexoctl replicate ls`; le tue regole restano.

Una modifica che non può essere applicata (un bucket non vuoto sull'altro sito, una chiave KMS che lì non
esiste) viene ritentata a ogni giro e segnalata come in attesa da `admin replicate info`.

## Rimuovere il gruppo

```bash
nexoctl admin replicate remove site-a
```

Ogni sito raggiunto dimentica il gruppo e le regole di replica `site-…`. Nulla viene cancellato: utenti,
policy, bucket e oggetti restano dove sono. Un sito non raggiungibile mantiene il gruppo: esegui il comando
anche lì.

## Limitazioni

- Un bucket viene cancellato sugli altri siti solo quando lì è vuoto; fino ad allora la cancellazione viene
  ritentata.
- Cancellare un utente non revoca sugli altri siti i link di condivisione che aveva firmato.
- Le credenziali temporanee (STS) e le sessioni della console sono proprie di ogni sito.
- Cambiare la password root su un sito interrompe il gruppo: va cambiata su tutti i siti.
- In un cluster, `admin replicate info` mostra i giri del leader del sito.
