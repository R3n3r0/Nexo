# Operatore Kubernetes

L'operatore di Nexo esegue cluster Nexo su Kubernetes a partire da una sola risorsa, `NexoCluster`: crea uno
StatefulSet per pool, i Service e un disruption budget, e sostituisce da sé i pod quando il loro template
cambia, uno alla volta e solo finché il cluster può perdere un nodo. Il chart Helm
([Installazione](installation.md)) resta il modo più semplice per un singolo deployment; l'operatore
aggiunge aggiornamenti guidati dallo stato di salute, espansione con nuovi pool e protezione dei dischi.

## Installazione

```bash
kubectl apply -k deploy/operator          # namespace nexo-operator, CRD, ClusterRole, l'operatore
kubectl -n nexo-operator rollout status deploy/nexo-operator
```

L'operatore osserva tutti i namespace; `WATCH_NAMESPACE` (separati da virgola) in
`deploy/operator/manager.yaml` lo limita. L'immagine è `docker.io/paolor/nexo-operator` (`Dockerfile.operator`
la costruisce dai sorgenti); `make operator-build` costruisce il binario, che funziona anche fuori dal
cluster con un kubeconfig.

## Un cluster

```yaml
apiVersion: v1
kind: Secret
metadata: {name: lake-credentials, namespace: storage}
stringData:
  root-user: lake-admin
  root-password: change-me-please
---
apiVersion: nexo.r3n3r0.github.io/v1alpha1
kind: NexoCluster
metadata: {name: lake, namespace: storage}
spec:
  image: docker.io/paolor/nexo:1.0.0
  credentials: {secretName: lake-credentials}
  pools:
    - name: pool-0
      servers: 4
      volumesPerServer: 4
      storage: 100Gi
      storageClassName: fast-ssd
```

```bash
kubectl -n storage get nexo
NAME   PHASE   READY   IMAGE                         AGE
lake   Ready   4/4     docker.io/paolor/nexo:1.0.0   3m
```

L'operatore crea:

| Oggetto | Nome | Ruolo |
|---|---|---|
| StatefulSet per pool | `lake-pool-0` | `servers` pod, ciascuno con `volumesPerServer` PersistentVolumeClaim (`data1`..), avviati insieme |
| Service headless | `lake-hl` | i nomi host stabili dei pod (`lake-pool-0-0.lake-hl.storage.svc.cluster.local`) |
| Service | `lake` | API S3 (porta 9000) |
| Service | `lake-console` | console e Admin API (porta 9001) |
| PodDisruptionBudget | `lake` | i drain dei nodi fermano un pod del cluster alla volta |

Ogni pod esegue `nexo server` con un endpoint per pool, in ordine
(`http://lake-pool-0-{0...3}.lake-hl.storage.svc.cluster.local:9000/data{1...4}`); le credenziali root e la
chiave KMS sono montate dai loro Secret. Le altre impostazioni sono `spec.env` (variabili `NEXO_*`, vedi
[Configurazione](configuration.md)); `resources`, `nodeSelector`, `tolerations` e `affinity` (predefinita: i
pod di un pool distribuiti su nodi diversi quando possibile) si possono impostare per pool.

| Campo | Significato |
|---|---|
| `image`, `imagePullPolicy`, `imagePullSecrets` | l'immagine del server |
| `credentials.secretName` (`userKey`, `passwordKey`) | credenziali root, chiavi `root-user` e `root-password` per default |
| `kms.secretName` (`key`) | chiave master del KMS integrato (`NEXO_KMS_SECRET_KEY`), chiave `kms-secret-key` per default |
| `tls.secretName` | un Secret `kubernetes.io/tls` (con `ca.crt` facoltativo, come lo scrive cert-manager): S3, console e traffico tra i nodi in TLS |
| `pools[]` | `name`, `servers`, `volumesPerServer`, `storage`, `storageClassName`, `resources`, `nodeSelector`, `tolerations`, `affinity` |
| `updateStrategy` | `Rolling` (default) o `AllAtOnce` |
| `env`, `resources`, `serviceType`, `clusterDomain`, `serviceAccountName`, `podLabels`, `podAnnotations`, `priorityClassName` | il resto di pod e Service |

Con TLS il certificato deve coprire i pod (`*.lake-hl.storage.svc.cluster.local`) e i Service
(`lake.storage.svc.cluster.local`, `lake-console...` e i nomi esterni). I certificati vengono ricaricati dai
server quando il Secret cambia: nessun riavvio.

Lo stato riporta la fase (`Pending`, `Starting`, `Ready`, `Updating`, `Degraded`, `Invalid`), i pod pronti,
l'immagine eseguita da tutti i pod, i pool creati e l'ultimo rapporto di `/nexo/health/cluster`; le condizioni
`Available`, `Progressing` e `Degraded` lo spiegano (`kubectl describe nexo lake`).

## Aggiornamenti

Gli StatefulSet usano la strategia `OnDelete`: è l'operatore, non Kubernetes, a sostituire i pod il cui
template è cambiato (una nuova immagine, nuove impostazioni).

- **Rolling** (default): un pod alla volta. Un pod viene sostituito solo quando tutti i pod sono pronti e
  `/nexo/health/cluster` riporta tutti i nodi e i dischi online con `maintenance: true` (un altro nodo può
  fermarsi senza perdere il quorum di scrittura di alcun erasure set); il successivo quando il nuovo pod è
  pronto e il cluster è di nuovo sano. Un cluster che non può perdere un nodo attende (`Progressing` con
  motivo `WaitingForQuorum`).
- **AllAtOnce**: tutti i pod da aggiornare insieme, una breve interruzione. Necessario per cambiare la
  **versione major**: nodi di versioni major diverse non formano un cluster.

Ciò che tutti i nodi devono condividere cambia sempre su tutti i pod insieme, qualunque sia la strategia: la
topologia (un nuovo pool) e il contenuto dei Secret delle credenziali e del KMS. Cambiare le credenziali root
riavvia quindi l'intero cluster.

L'aggiornamento orchestrato del binario (`nexoctl admin update`) non si applica su Kubernetes: l'immagine è il
binario, cambia invece `spec.image`.

## Espansione e pool

Un pool non viene mai ridimensionato: l'API rifiuta modifiche a `servers` o `volumesPerServer` di un pool
esistente. Il cluster cresce **aggiungendo in coda** un pool a `spec.pools`; tutti i pod si riavviano con la
nuova topologia (come per un'[espansione](operations/upgrade-expansion.md#espansione-con-un-nuovo-pool) fuori da
Kubernetes).

Un pool si rimuove in due passi: prima lo si dismette (`nexoctl admin decommission start ... <n>`, attendendo
`complete`), poi lo si toglie da `spec.pools` confermandolo con l'annotazione:

```bash
kubectl -n storage annotate nexo lake nexo.r3n3r0.github.io/decommissioned-pools=pool-0
```

Senza l'annotazione l'operatore rifiuta la modifica (fase `Invalid`) e lascia il pool in esecuzione.

## I dati

L'operatore non elimina mai un PersistentVolumeClaim: gli StatefulSet mantengono i loro claim quando vengono
ridimensionati o eliminati, ed eliminare un `NexoCluster` elimina StatefulSet, pod e Service ma non i dischi.
Ricreando il cluster con lo stesso nome, namespace e pool li si ritrova. Per liberare lo spazio, elimina i
claim (`kubectl -n storage delete pvc -l nexo.r3n3r0.github.io/cluster=lake`) quando i dati non servono più.
