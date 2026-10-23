# Aggiornamento ed espansione

## Aggiornamento di un singolo nodo (SNSD, SNMD)

1. Leggi le note di rilascio e verifica la release ([Installazione](../installation.md#binari-archivi)).
2. Esporta per sicurezza IAM e configurazione: `nexoctl admin cluster export local --output backup.zip`
   (vedi [Backup e migrazione](backup-migration.md)).
3. Sostituisci il binario (o il pacchetto, o il tag dell'immagine) e riavvia il servizio:

   ```bash
   sudo apt install ./nexo_1.0.1_amd64.deb     # oppure: install -m 0755 nexo /usr/local/bin/nexo
   sudo systemctl restart nexo
   nexo healthcheck --ready && nexo version
   ```

Il riavvio ferma il server in modo ordinato (`SIGTERM`: le nuove richieste ricevono `503`, quelle in corso
terminano entro il timeout di arresto) e interrompe il servizio per qualche secondo.

## Aggiornamento rolling di un cluster

Nodi con la stessa **versione major** convivono nello stesso cluster: si aggiorna un nodo alla volta
senza interrompere il servizio, purché il quorum regga con un nodo spento (vedi
[Modalità → Scelta della parità](../deployment-modes.md)). Un nodo con una versione major
diversa dalla maggioranza non entra nel cluster.

Per ogni nodo, uno alla volta:

1. Controlla che si possa spegnere un nodo: `/nexo/health/cluster` deve rispondere `"maintenance": true`
   (e `writeQuorum: true`):

   ```bash
   curl -s http://nexo1.lan:9000/nexo/health/cluster
   ```

2. Installa il nuovo binario e riavvia il servizio sul nodo:

   ```bash
   sudo apt install ./nexo_1.0.1_amd64.deb && sudo systemctl restart nexo
   ```

   Durante l'arresto il nodo risponde `503` a `/nexo/health/ready`: il load balancer lo esclude e le
   letture vengono ritentate su un altro nodo (le configurazioni di `deploy/proxy` lo fanno già).

3. Attendi che sia di nuovo pronto e allineato:

   ```bash
   nexo healthcheck --ready                     # sul nodo; con TLS: --certs-dir /etc/nexo/certs
   nexoctl admin info local                     # tutti i nodi online, la versione nuova sul nodo aggiornato
   ```

4. Passa al nodo successivo. Le scritture arrivate mentre il nodo era spento vengono completate dalla coda
   di healing (MRF) quando torna.

Con gli orchestratori:

- **Kubernetes (Helm)**: `helm upgrade` con il nuovo tag aggiorna lo StatefulSet un pod alla volta; il
  PodDisruptionBudget (`maxUnavailable: 1`) protegge il quorum durante i drain.
- **Kubernetes (operatore)**: cambia `spec.image` del `NexoCluster`; l'operatore sostituisce un pod alla volta,
  ciascuno solo finché il cluster può perdere un nodo ([Operatore Kubernetes](../kubernetes-operator.md)).
- **Docker Swarm**: `docker service update` con `parallelism: 1` (già impostato in `deploy/swarm/stack.yml`).
- **Docker Compose**: `docker compose up -d --no-deps nexo1`, attendi `healthy`, poi `nexo2`, …

## Aggiornamento orchestrato (`nexoctl admin update`)

Quando ogni nodo esegue il binario di un archivio di release (non un pacchetto, non un'immagine), un solo
comando aggiorna l'intero deployment, un nodo alla volta, senza interruzioni:

```bash
sha256sum nexo-v1.1.0-linux-amd64                  # o il file dei checksum della release
nexoctl admin update --sha256 3f5a... cluster https://dl.example.com/nexo-v1.1.0-linux-amd64
```

Il nodo che risponde alla richiesta orchestra il job:

1. **Staging, tutto o niente.** Ogni nodo scarica il binario nella directory di quello che esegue, ne
   verifica lo SHA-256 e lo esegue (`nexo version --json`) per leggerne la versione e verificarne la
   piattaforma. Se un nodo fallisce, o i nodi non concordano sulla versione, nessun nodo cambia.
2. **Attivazione rolling.** Nodo per nodo (per ultimo il nodo che risponde), quando `/nexo/health/cluster`
   consentirebbe una manutenzione (un altro nodo può fermarsi senza perdere il quorum di scrittura di alcun
   erasure set): il binario preparato prende il posto di quello in esecuzione, conservato accanto come
   `nexo.old`, e il processo si sostituisce con il nuovo dopo aver risposto alle richieste in corso (stesso
   PID, argomenti, ambiente e file di log). Il nodo successivo attende che questo sia di nuovo in
   esecuzione con la nuova versione.
3. Il comando segue l'avanzamento e termina quando ogni nodo esegue la nuova versione; dietro un load
   balancer il polling sopravvive al riavvio del nodo che risponde. `--no-wait` termina subito,
   `nexoctl admin update --status cluster` mostra l'ultimo job.

`--all-at-once` riavvia tutti i nodi insieme (un'interruzione di pochi secondi): è necessario per cambiare
la **versione major**, perché nodi di versioni major diverse non formano un cluster. Un nodo che non torna
entro 5 minuti ferma il job e i nodi successivi mantengono il loro binario: sistema il nodo (il binario
precedente è `nexo.old`) e riavvia l'aggiornamento. Su Unix, se il nuovo binario non è neppure eseguibile,
il processo rimette `nexo.old` al suo posto e lo esegue. L'aggiornamento richiede `admin:ServerUpdate`
(`admin:ServerInfo` per lo stato), l'Admin API sui nodi e una directory del binario **scrivibile dall'utente
del server**: con i pacchetti (`/usr/bin`, di root) continua ad aggiornare con il gestore dei pacchetti. In
un container o su Kubernetes l'immagine è il binario: cambia invece il tag dell'immagine (`helm upgrade`),
che aggiorna i pod nello stesso modo.

## Riavvio senza aggiornamento

```bash
nexoctl admin service restart local          # chiede conferma; --yes negli script
```

Riavvia i servizi del **nodo che risponde** all'Admin API dell'alias nello stesso processo (stesso binario e
stessa configurazione statica): utile dopo un problema, non per installare una nuova versione.
`nexoctl admin service stop` arresta il processo (va riavviato a mano o dal supervisore). Richiedono
`admin:ServiceRestart` e `admin:ServiceStop`.

`--cluster` agisce su **tutti i nodi** del deployment:

```bash
nexoctl admin service restart --cluster cluster                  # un nodo alla volta, quorum preservato
nexoctl admin service restart --cluster --all-at-once cluster    # tutti i nodi insieme
nexoctl admin service stop --cluster cluster                     # gli altri nodi, poi quello che risponde
```

Il riavvio rolling segue le stesse regole dell'aggiornamento orchestrato (il nodo successivo quando il
precedente è di nuovo in esecuzione e il deployment può perdere un nodo) e si segue allo stesso modo
(`nexoctl admin update --status`).

Per ricaricare i certificati TLS non serve riavviare: `systemctl reload nexo` (`SIGHUP`) o la sola
sostituzione dei file (vedi [TLS](../security/tls.md)).

## Espansione con un nuovo pool

Un cluster cresce aggiungendo un **pool**: un gruppo di nodi (anche con dischi di dimensione diversa
dal primo pool). I dati esistenti non vengono spostati.

1. Prepara i nuovi nodi come i primi: stessa versione di Nexo, stesse credenziali root, stessa
   configurazione KMS, dischi montati e vuoti, nomi risolvibili da tutti i nodi, porta 9000 aperta tra tutti.
2. Calcola la nuova riga di comando: un argomento con ellissi per pool, **nello stesso ordine** su tutti i
   nodi (il pool esistente resta il primo):

   ```bash
   NEXO_ENDPOINTS="http://nexo{1...4}.lan:9000/mnt/disk{1...4} http://nexo{5...8}.lan:9000/mnt/disk{1...4}"
   ```

   Controllala su un nuovo nodo con `nexo server --dry-run` (pool, set e parità previsti).
3. Ferma **tutti** i nodi del cluster, aggiorna `NEXO_ENDPOINTS` (o gli argomenti) su ogni nodo, vecchio e
   nuovo, e riavviali tutti. Durante il bootstrap i nodi verificano di avere la stessa topologia: un nodo
   rimasto con la riga di comando vecchia non entra nel cluster. È un'interruzione breve e pianificata.
4. I dischi del nuovo pool vengono formattati con lo stesso deployment ID. Verifica con
   `nexoctl admin info local`: due pool, tutti i set `ok`.

Dopo l'espansione:

- gli oggetti già presenti restano nel loro pool, anche quando vengono sovrascritti;
- i nuovi oggetti vanno nei pool in proporzione allo spazio libero (i pool oltre il 95% sono esclusi);
- per distribuire anche gli oggetti esistenti sul nuovo pool, avvia un [ribilanciamento](#ribilanciamento-tra-pool).

Su Kubernetes l'espansione è `distributed.pools` nel chart (anche qui con il riavvio di tutti i pod); con
Docker Compose o Swarm si aggiungono i servizi dei nuovi nodi e si cambia `command` su **tutti**.

## Ribilanciamento tra pool

```bash
nexoctl admin rebalance start cluster
nexoctl admin rebalance status cluster      # oggetti e byte spostati, il pool che si sta svuotando
nexoctl admin rebalance stop cluster
```

Il ribilanciamento sposta oggetti, con tutte le loro versioni, dai pool più pieni della media (di oltre 5
punti) ai pool sotto la media, finché ogni pool è vicino alla media. Gira in background sul nodo leader,
sopravvive ai riavvii (l'avanzamento è nei documenti di sistema) e si può fermare in qualsiasi momento: gli
oggetti già spostati restano dove sono. Richiede `admin:Rebalance`; la console lo mostra in
**Cluster → Gestione dei pool**.

## Dismissione di un pool

Un pool si rimuove in due passi: prima i suoi oggetti vengono spostati sugli altri pool, poi lo si toglie
dalla riga di comando.

```bash
nexoctl admin decommission status cluster      # pool, occupazione, avanzamento della dismissione
nexoctl admin decommission start cluster 2     # la posizione (da 1) del pool nella riga di comando
nexoctl admin decommission cancel cluster 2    # il pool torna a ricevere nuovi oggetti
```

1. Verifica che gli altri pool abbiano spazio libero sufficiente per gli oggetti del pool (un pool oltre
   il 95% non riceve nulla).
2. Avvia la dismissione. Da quel momento il pool non riceve nuovi oggetti, versioni o caricamenti
   multipart: vanno sugli altri pool. Le letture continuano a funzionare mentre gli oggetti si spostano.
3. Un job in background sul leader percorre il pool bucket per bucket e sposta ogni versione di ogni oggetto
   (delete marker inclusi) su un altro pool. Ogni copia viene riletta e confrontata prima di rimuovere la
   copia nel pool, sotto il lock della chiave: un oggetto scritto nel frattempo resta dov'è e viene
   spostato dalla passata successiva. Le passate si ripetono finché una non trova più nulla; gli oggetti non
   spostati vengono ritentati (fino a 10 passate, poi il job riporta `failed` e il pool li conserva). I
   caricamenti multipart iniziati prima della dismissione si completano nel pool e vengono spostati dopo.
4. Quando lo stato riporta `complete`, ferma tutti i nodi, rimuovi il pool dalla riga di comando
   (`NEXO_ENDPOINTS`) di **ogni** nodo e riavviali, come per un'espansione.

L'avanzamento sopravvive ai riavvii e al cambio di leader. Richiede `admin:Decommission`; la console lo
mostra in **Cluster → Gestione dei pool**.

Il **primo pool** della riga di comando non si può dismettere: contiene i documenti di sistema (IAM,
configurazione, metadati dei bucket). Per ritirarne i nodi, sposta i dati su un nuovo deployment con
`nexoctl admin cluster export/import` e `nexoctl mirror --preserve`
([Backup e migrazione](backup-migration.md)).

## Prima di aggiornare: checklist

- [ ] backup di IAM e configurazione (`admin cluster export`);
- [ ] cluster sano: tutti i nodi online, nessun set degradato, coda MRF vuota (`nexoctl admin info`);
- [ ] spazio e inode liberi sui dischi;
- [ ] orologi sincronizzati;
- [ ] stessa configurazione KMS su tutti i nodi (un nodo con un KMS diverso dalla maggioranza non parte).
