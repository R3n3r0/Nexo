# Threat model

L'analisi completa (STRIDE per superficie: API S3, Admin API e console, RPC tra i nodi, distribuzioni,
cache, eventi, KMS, CLI), con le mitigazioni nel codice, le scansioni automatiche e il fuzzing, è in
[docs/security/threat-model.md](https://github.com/R3n3r0/Nexo/blob/main/docs/security/threat-model.md).
Il penetration test della console e dell'Admin API (ottobre 2026) è descritto in
[docs/security/pentest-2026-10.md](https://github.com/R3n3r0/Nexo/blob/main/docs/security/pentest-2026-10.md).
Questa pagina riassume ciò che conta per chi gestisce un deployment.

## Sintesi

| Minaccia | Superficie | Mitigazione in Nexo |
|----------|-----------|---------------------|
| Furto d'identità | API S3, Admin API, RPC tra i nodi | SigV4 con confronto a tempo costante; sessioni console con cookie `HttpOnly`/`SameSite=Strict`, token CSRF e HSTS su TLS; token tra nodi a breve scadenza legati alla singola chiamata; mTLS opzionale tra i nodi |
| Manomissione | dati su disco, traffico | checksum per blocco (bitrot), TLS 1.2+, cifratura SSE |
| Ripudio | operazioni amministrative e sui dati | audit log per ogni richiesta, consegnabile a un target esterno |
| Divulgazione | console, errori, log | nessun segreto nei log (credenziali, firme, token, chiavi redatti), errori generici verso gli anonimi, segreti IAM cifrati a riposo |
| Negazione del servizio | upload, listing, login, stream live | limiti di dimensione e timeout, limite ai tentativi di login, al massimo 256 stream live di trace e di log per nodo, risposte peer limitate, regex RE2 a tempo lineare |
| Elevazione di privilegi | motore di policy, console | deny di default, autorizzazione sempre lato server, la console mostra solo ciò che l'utente può fare ma il server ricontrolla ogni richiesta |

## Raccomandazioni per la produzione

1. **TLS ovunque**: `--certs-dir` su tutti i listener ed endpoint `https://` con
   `--tls-internode-verify` in cluster ([TLS](tls.md)). Senza TLS cookie, firme e traffico tra i nodi
   viaggiano in chiaro.
2. **Credenziali root lunghe e casuali**, uguali su tutti i nodi e diverse tra deployment: firmano anche i
   token tra i nodi. Disattiva root sull'API S3 (`api.root_access=off`) e usa utenti dedicati.
3. **Un reverse proxy davanti** per i limiti di richieste e connessioni per client, che Nexo non applica
   ancora sull'API S3 e sui domini pubblici; mai esporre `/nexo/rpc/`.
4. **Permessi minimi**: policy per bucket o prefisso, access key con scadenza e policy inline, gruppi;
   `diagnostics` per il monitoraggio; `admin:Profiling` solo a chi amministra i nodi.
5. **Audit esterno**: un target `audit_webhook` o `audit_kafka` verso un sistema che l'amministratore di
   Nexo non può modificare.
6. **KMS**: in produzione preferisci KES con certificato client al KMS integrato, la cui master key sta
   nell'ambiente del processo; conserva comunque una copia sicura delle chiavi.
7. **Block public access** a livello di cluster se nessun bucket deve essere pubblico, e verifica
   periodicamente gli accessi anonimi (`nexoctl anonymous list`) e gli share attivi.
8. **Aggiornamenti**: applica le release con le correzioni di sicurezza; `make security` (govulncheck,
   gosec, npm audit) gira in CI a ogni modifica.
