# TODO

Lavoro rimasto dopo la v1.0, in ordine di priorità consigliato. Ogni voce rimanda al codice di catalogo
([docs/02](docs/02-funzionalita.md)) e al task della roadmap ([docs/05](docs/05-roadmap-task.md)); la copertura dei
test è in [test/TRACEABILITY.md](test/TRACEABILITY.md).

Le stime vengono dal costo delle milestone già fatte (M0–M9 più tre giri di revisione: circa 32M token di agenti) e
comprendono implementazione, test, revisione critica e correzioni. "gg" = giorni/persona per uno sviluppatore
esperto di Go che conosce il codice.

Ogni voce si chiude solo con: test di unità e di integrazione nelle tre modalità (SNSD, SNMD, cluster) dove ha
senso, CLI e console se previste dal catalogo, documentazione utente in `docs/user`, riga nel CHANGELOG, matrice di
tracciabilità aggiornata (`make traceability`), e i controlli verdi (`make lint test-race test-cluster`, `make e2e`
se tocca la console).

## 1. Rilascio

- [ ] **Release candidate `v1.0.0-rc.1`** (T-9.5): il tag avvia `release.yml` (binari, deb/rpm, Homebrew, SBOM,
  firme cosign, immagini su ghcr.io). Prima: CI verde su `main`, CHANGELOG con la data, verificare che i segreti del
  repository (token Homebrew tap, cosign) esistano. Dopo: provare install da deb/rpm/brew/docker su macchine pulite.
- [ ] **Unire il branch di sviluppo su `main`** con una PR e far girare la CI completa (job nightly compresi:
  s3-tests, mint, compose).
- [ ] **Licenza**: la v1.0 esce con AGPL-3.0 più licenza commerciale. Sostituire "R3n3r0" in `NOTICE` con il nome
  legale del titolare se serve per i contratti commerciali; preparare il testo del contratto di licenza commerciale.
- [ ] **CLA per i contributori** (necessaria per la doppia licenza): attivare un bot CLA o la DCO con cessione dei
  diritti di rilicenza su GitHub.

## 2. Qualità e sicurezza (prima o subito dopo la v1.0)

- [ ] **Penetration test della console** (T-9.3) — 1–2M token, 3–5 gg. Sessione e CSRF, XSS nelle pagine che
  mostrano dati degli utenti (nomi di oggetti, metadati, tag, policy JSON, log/trace in streaming), upload e
  download, Admin API con utenti a privilegi ridotti (escalation orizzontale e verticale), clickjacking e header di
  sicurezza, CORS della console, brute force del login. Strumenti: OWASP ZAP/Burp contro `bin/nexo`, test
  Playwright per ogni problema trovato.
- [ ] **O_DIRECT e zero-copy** (T-9.2) — 2–3M token, 5–8 gg. O_DIRECT per scritture e letture grandi sui drive
  locali (allineamento dei buffer, fallback su filesystem che non lo supportano, es. tmpfs/ZFS), `sendfile`/`splice`
  dove il corpo non passa da cifratura, erasure o bitrot (SNSD senza SSE). Misurare con `docs/bench` prima e dopo;
  tenere solo ciò che migliora davvero.
- [ ] **Rate limiting per API e per distribuzione** (S-OPS-10, S-DST-08, TDS-13) — 1–2M token, 4–6 gg. Applicare
  `api.requests_max` / `api.requests_deadline` (oggi validati ma non applicati), limiti per utente, bucket e IP,
  limite per distribuzione già salvato nella configurazione; coerente nel cluster.
- [ ] **ACME DNS-01** per i certificati wildcard delle distribuzioni — 1M token, 3–4 gg (provider: Cloudflare,
  Route 53, RFC 2136).
- [ ] **`helm lint` + kubeconform** in locale: ora girano solo in CI (nel container di sviluppo mancano helm e
  kubeconform).
- [ ] **Avviso govulncheck GO-2026-5932** (`golang.org/x/crypto/openpgp` non mantenuto, non chiamato dal codice):
  togliere la dipendenza transitiva quando possibile.

## 3. M10 — Funzionalità P2

| # | Voce | Codici | Token | Tempo agenti | gg/persona | Rischio |
|---|------|--------|-------|--------------|------------|---------|
| 3.1 | Replica dei bucket attiva-attiva + resync | S-BKT-12, D-BKT-05 (T-10.1) | 4–6M | 6–9 h | 15–25 | alto |
| 3.2 | Site replication (IAM, config, bucket tra cluster) | S-STO-11, C-ADM-11 (T-10.2) | 5–8M | 8–12 h | 20–30 | alto |
| 3.3 | Decommission e ribilanciamento dei pool | S-STO-09/10, C-ADM-10, D-CLU-03 (T-10.3) | 3–5M | 5–8 h | 15–20 | alto |
| 3.4 | OIDC (SSO console, AssumeRoleWithWebIdentity) e LDAP/AD | S-IAM-08/09, D-AUTH-02 (T-10.4) | 3–4M | 4–6 h | 10–15 | medio |
| 3.5 | Tiering ILM verso S3/GCS/Azure, restore | S-STO-12 (T-10.5) | 4–6M | 6–8 h | 15–20 | medio-alto |
| 3.6 | SelectObjectContent (CSV/JSON/Parquet) | S-OBJ-16 (T-10.6) | 3–5M | 4–7 h | 12–20 | medio |
| 3.7 | Compressione trasparente S2 per content-type | S-OBJ-14 (T-10.6) | 1–2M | 2–3 h | 4–6 | medio |
| 3.8 | Restart/stop coordinato del cluster, aggiornamento rolling (`admin update`) | S-OPS-09, C-ADM-09 (T-10.7) | 2–3M | 3–4 h | 6–10 | medio |
| 3.9 | ACL canned (private, public-read) per compatibilità | S-BKT-13 | 1M | 1–2 h | 2–4 | basso |
| 3.10 | Operatore Kubernetes (CRD, controller, upgrade, test con kind) | T-10.8 | 4–6M | 6–9 h | 15–25 | medio |
| | **Totale M10** | | **circa 30–46M** | **circa 45–70 h** | **circa 115–175** | |

Ordine consigliato: 3.4 (OIDC/LDAP, molto richiesto e indipendente) → 3.1 (replica, base per 3.2) → 3.3 → 3.8 →
3.2 → 3.5 → 3.7 → 3.6 → 3.9 → 3.10.

Note per voce:

- **3.1 Replica**: regole per prefisso/tag, coda persistente come per gli eventi, stato `x-amz-replication-status`,
  replica di delete marker e versioni, resync completo, metriche di ritardo. Test con due cluster reali in
  `test/cluster`.
- **3.2 Site replication**: richiede 3.1; sincronizza IAM, policy, configurazione dei bucket e distribuzioni;
  gestione dei conflitti (ultimo scrittore vince con orologio ibrido).
- **3.3 Decommission/rebalance**: spostare gli oggetti a quorum preservato con il pool ancora leggibile, ripresa
  dopo un crash, nessun oggetto perso con nodi che cadono a metà (test di fault come TC-05/TC-09).
- **3.4 OIDC/LDAP**: claim → policy, gruppi LDAP → gruppi Nexo, pulsante SSO nella console, STS
  `AssumeRoleWithWebIdentity` / `AssumeRoleWithLDAPIdentity`.
- **3.5 Tiering**: transizione nel lifecycle, oggetti "stub" con metadati locali, `RestoreObject`, credenziali del
  tier cifrate nello store di sistema.
- **3.6 Select**: parser SQL del sottoinsieme S3 Select, output in event stream, limiti di memoria.
- **3.10 Operatore**: CRD `Tenant`/`Pool`, espansione pool, rolling upgrade, certificati; il chart Helm resta per
  chi non vuole l'operatore.

## 4. Manutenzione periodica

- [ ] Aggiornare Go e golangci-lint a ogni minor release (ultimo: Go 1.27.2).
- [ ] Rieseguire `make security` e le suite di compatibilità (`make test-s3compat`, `make test-mint`) a ogni release.
- [ ] Aggiornare `NEXO_UPGRADE_FROM` in `make test-upgrade` all'ultima release pubblicata.
