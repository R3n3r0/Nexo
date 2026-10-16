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
- **Docker Swarm**: `docker service update` con `parallelism: 1` (già impostato in `deploy/swarm/stack.yml`).
- **Docker Compose**: `docker compose up -d --no-deps nexo1`, attendi `healthy`, poi `nexo2`, …

## Riavvio senza aggiornamento

```bash
nexoctl admin service restart local          # chiede conferma; --yes negli script
```

Riavvia i servizi del **nodo che risponde** all'Admin API dell'alias nello stesso processo (stesso binario e
stessa configurazione statica): utile dopo un problema, non per installare una nuova versione.
`nexoctl admin service stop` arresta il processo (va riavviato a mano o dal supervisore). Richiedono
`admin:ServiceRestart` e `admin:ServiceStop`.

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
- il bilanciamento dei dati tra pool e la dismissione di un pool non sono ancora disponibili.

Su Kubernetes l'espansione è `distributed.pools` nel chart (anche qui con il riavvio di tutti i pod); con
Docker Compose o Swarm si aggiungono i servizi dei nuovi nodi e si cambia `command` su **tutti**.

## Prima di aggiornare: checklist

- [ ] backup di IAM e configurazione (`admin cluster export`);
- [ ] cluster sano: tutti i nodi online, nessun set degradato, coda MRF vuota (`nexoctl admin info`);
- [ ] spazio e inode liberi sui dischi;
- [ ] orologi sincronizzati;
- [ ] stessa configurazione KMS su tutti i nodi (un nodo con un KMS diverso dalla maggioranza non parte).
