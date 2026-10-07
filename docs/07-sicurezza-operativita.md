# 7. Sicurezza e operatività

## 7.1 Threat model (sintesi STRIDE)

| Minaccia | Superficie | Mitigazione |
|----------|-----------|-------------|
| Spoofing | API S3, Admin API, internode | SigV4 con confronto costante, JWT internode a breve scadenza, mTLS opzionale tra nodi |
| Tampering | Dati su disco, traffico | Bitrot hash per blocco, TLS 1.2+ (default 1.3), SSE |
| Repudiation | Operazioni amministrative | Audit log immutabile verso target esterno |
| Information disclosure | Console, errori, log | Nessun segreto nei log, messaggi d'errore generici verso anonimi, segreti IAM cifrati a riposo |
| Denial of service | Upload, listing, login | Limiti di dimensione header/body, timeout, rate limit login, limiti di concorrenza per API |
| Elevation of privilege | Policy engine, console | Deny-by-default, autorizzazione sempre server-side, test negativi sistematici |

## 7.2 Gestione dei segreti
- Credenziali root solo da env/file (`NEXO_ROOT_USER`, `NEXO_ROOT_PASSWORD` o `_FILE`), mai salvate in chiaro.
- IAM e config cifrati in `.nexo.sys` con chiave derivata (Argon2id/HKDF) o fornita dal KMS.
- Secret key degli utenti mostrata una sola volta in console/CLI.
- File config della CLI con permessi 0600 e avviso se più permissivi.

## 7.3 TLS
- Certificati da `~/.nexo/certs/` (`public.crt`, `private.key`, sottocartelle per SNI, `CAs/` per trust).
- Hot reload su cambio file (fsnotify) o SIGHUP.
- Opzione `--tls-internode-verify` per mTLS tra nodi.

## 7.4 Osservabilità
- **Metriche**: `nexo_s3_requests_total{api,status}`, `nexo_s3_request_duration_seconds`, `nexo_drive_*`
  (online, used, free, errors, latency), `nexo_heal_*`, `nexo_scanner_*`, `nexo_ilm_*`, `nexo_cluster_nodes_{online,offline}`.
- **Log**: JSON `slog` con `request_id`, `node`, `api`, `bucket`, `object`, `duration`, `error`.
- **Audit**: record per ogni richiesta autenticata con identità, azione, risorsa, IP, esito, latenza.
- **Tracing**: OpenTelemetry (OTLP) con propagazione del contesto sulle RPC internode.
- **Alert d'esempio** (Prometheus rules in `deploy/`): disco offline, quorum a rischio, healing bloccato, capacità > 85%, tasso 5xx.

## 7.5 Deploy
| Target | Artefatto |
|--------|-----------|
| Bare metal / VM | Binari statici (linux/darwin/windows, amd64/arm64), unit systemd, file env |
| Container | Immagine distroless multi-arch firmata (cosign) + SBOM |
| Docker Compose | Esempi single-node e 4 nodi |
| Kubernetes | Helm chart (StatefulSet, headless service, PVC per disco, PodDisruptionBudget, ServiceMonitor) |

### Raccomandazioni operative
- Dischi dedicati formattati XFS, montati con `noatime`, niente RAID sotto Nexo.
- Stesso numero e dimensione di dischi per nodo all'interno di un pool.
- NTP sincronizzato su tutti i nodi.
- Load balancer davanti ai nodi per S3 (9000) e console (9001) con health check su `/nexo/health/ready`.

## 7.6 Configurazione principale

| Variabile / flag | Default | Descrizione |
|------------------|---------|-------------|
| `--address` / `NEXO_ADDRESS` | `:9000` | Porta API S3 |
| `--console-address` / `NEXO_CONSOLE_ADDRESS` | `:9001` | Porta console/Admin API |
| `NEXO_ROOT_USER` / `NEXO_ROOT_PASSWORD` | — (obbligatori, min 8 caratteri) | Credenziali root |
| `NEXO_REGION` | `us-east-1` | Regione S3 |
| `NEXO_DOMAIN` | — | Abilita virtual-host style |
| `NEXO_STORAGE_CLASS_STANDARD` | auto (`EC:2..4`) | Parità di default |
| `NEXO_KMS_SECRET_KEY` / `NEXO_KMS_ENDPOINT` | — | KMS interno / esterno |
| `NEXO_BROWSER` | `on` | Abilita/disabilita la console |
| `NEXO_CONSOLE_SESSION_DURATION` | `12h` | Durata sessione console |
| `NEXO_PROMETHEUS_AUTH_TYPE` | `jwt` | `jwt` o `public` |
| `--certs-dir` | `~/.nexo/certs` | Certificati TLS |
