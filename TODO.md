# TODO

Work left after v1.0, in the recommended order of priority. Each item refers to the feature code in the catalog
([docs/02](docs/02-features.md)) and to the roadmap task ([docs/05](docs/05-roadmap.md)); test coverage is
in [test/TRACEABILITY.md](test/TRACEABILITY.md).

Estimates come from the cost of the milestones already done (M0–M9 plus three review rounds: about 32M agent
tokens) and include implementation, tests, critical review and fixes. "pd" = person-days for an experienced Go
developer who knows the codebase.

An item is done only with: unit and integration tests in the three modes (SNSD, SNMD, cluster) where it makes
sense, CLI and console when the catalog calls for them, user documentation in `docs/user`, a CHANGELOG line, an
updated traceability matrix (`make traceability`), and green checks (`make lint test-race test-cluster`, `make e2e`
when the console is touched).

## 1. Release

- [ ] **Release candidate `v1.0.0-rc.1`** (T-9.5): the tag starts `release.yml` (binaries, deb/rpm, Homebrew, SBOM,
  cosign signatures, images on ghcr.io). Before: green CI on `main`, CHANGELOG with the date, check that the
  repository secrets exist (Homebrew tap token, cosign). After: try deb/rpm/brew/docker installs on clean machines.
- [ ] **Merge the development branch into `main`** with a PR and run the full CI (nightly jobs included:
  s3-tests, mint, compose).
- [ ] **License**: v1.0 ships under AGPL-3.0 plus a commercial license. Replace "R3n3r0" in `NOTICE` with the
  legal name of the copyright holder if commercial contracts need it; prepare the commercial license agreement.
- [ ] **Contributor License Agreement** (required by dual licensing): enable a CLA bot, or DCO with a grant of
  relicensing rights, on GitHub.

## 2. Quality and security (before or right after v1.0)

- [ ] **Console penetration test** (T-9.3) — 1–2M tokens, 3–5 pd. Session and CSRF, XSS in pages that show user
  data (object names, metadata, tags, policy JSON, streamed logs/traces), upload and download, Admin API with
  low-privilege users (horizontal and vertical escalation), clickjacking and security headers, console CORS, login
  brute force. Tools: OWASP ZAP/Burp against `bin/nexo`, one Playwright test per issue found.
- [ ] **O_DIRECT and zero-copy** (T-9.2) — 2–3M tokens, 5–8 pd. O_DIRECT for large writes and reads on local
  drives (buffer alignment, fallback on filesystems without support such as tmpfs/ZFS), `sendfile`/`splice` where
  the body does not go through encryption, erasure or bitrot (SNSD without SSE). Measure with `docs/bench` before
  and after; keep only what really helps.
- [ ] **Rate limiting per API and per distribution** (S-OPS-10, S-DST-08, TDS-13) — 1–2M tokens, 4–6 pd. Enforce
  `api.requests_max` / `api.requests_deadline` (validated today but not applied), limits per user, bucket and IP,
  the per-distribution limit already stored in the configuration; consistent across the cluster.
- [ ] **ACME DNS-01** for wildcard certificates of distributions — 1M tokens, 3–4 pd (providers: Cloudflare,
  Route 53, RFC 2136).
- [ ] **`helm lint` + kubeconform** locally: they only run in CI today (the dev container lacks helm and
  kubeconform).
- [ ] **govulncheck notice GO-2026-5932** (`golang.org/x/crypto/openpgp` unmaintained, not called by the code):
  drop the transitive dependency when possible.

## 3. M10 — P2 features

| # | Item | Codes | Tokens | Agent time | Person-days | Risk |
|---|------|-------|--------|------------|-------------|------|
| 3.1 | Active-active bucket replication + resync | S-BKT-12, D-BKT-05 (T-10.1) | 4–6M | 6–9 h | 15–25 | high |
| 3.2 | Site replication (IAM, config, buckets across clusters) | S-STO-11, C-ADM-11 (T-10.2) | 5–8M | 8–12 h | 20–30 | high |
| 3.3 | Pool decommission and rebalance | S-STO-09/10, C-ADM-10, D-CLU-03 (T-10.3) | 3–5M | 5–8 h | 15–20 | high |
| 3.4 | OIDC (console SSO, AssumeRoleWithWebIdentity) and LDAP/AD | S-IAM-08/09, D-AUTH-02 (T-10.4) | 3–4M | 4–6 h | 10–15 | medium |
| 3.5 | ILM tiering to S3/GCS/Azure, restore | S-STO-12 (T-10.5) | 4–6M | 6–8 h | 15–20 | medium-high |
| 3.6 | SelectObjectContent (CSV/JSON/Parquet) | S-OBJ-16 (T-10.6) | 3–5M | 4–7 h | 12–20 | medium |
| 3.7 | Transparent S2 compression per content type | S-OBJ-14 (T-10.6) | 1–2M | 2–3 h | 4–6 | medium |
| 3.8 | Coordinated cluster restart/stop, rolling binary update (`admin update`) | S-OPS-09, C-ADM-09 (T-10.7) | 2–3M | 3–4 h | 6–10 | medium |
| 3.9 | Canned ACLs (private, public-read) for compatibility | S-BKT-13 | 1M | 1–2 h | 2–4 | low |
| 3.10 | Kubernetes operator (CRDs, controller, upgrades, tests with kind) | T-10.8 | 4–6M | 6–9 h | 15–25 | medium |
| | **M10 total** | | **about 30–46M** | **about 45–70 h** | **about 115–175** | |

Recommended order: 3.4 (OIDC/LDAP, much requested and independent) → 3.1 (replication, the base for 3.2) → 3.3 →
3.8 → 3.2 → 3.5 → 3.7 → 3.6 → 3.9 → 3.10.

Notes per item:

- **3.1 Replication**: rules per prefix/tag, persistent queue as for events, `x-amz-replication-status`,
  replication of delete markers and versions, full resync, lag metrics. Tests with two real clusters in
  `test/cluster`.
- **3.2 Site replication**: needs 3.1; synchronizes IAM, policies, bucket configuration and distributions;
  conflict handling (last writer wins with a hybrid logical clock).
- **3.3 Decommission/rebalance**: move objects while preserving quorum and keeping the pool readable, resume after
  a crash, no object lost when nodes fail midway (fault tests like TC-05/TC-09).
- **3.4 OIDC/LDAP**: claims → policies, LDAP groups → Nexo groups, SSO button in the console, STS
  `AssumeRoleWithWebIdentity` / `AssumeRoleWithLDAPIdentity`.
- **3.5 Tiering**: lifecycle transition, "stub" objects with local metadata, `RestoreObject`, tier credentials
  encrypted in the system store.
- **3.6 Select**: parser for the S3 Select SQL subset, event-stream output, memory limits.
- **3.10 Operator**: `Tenant`/`Pool` CRDs, pool expansion, rolling upgrades, certificates; the Helm chart stays for
  those who don't want the operator.

## 4. Periodic maintenance

- [ ] Update Go and golangci-lint at every minor release (latest: Go 1.27.2).
- [ ] Re-run `make security` and the compatibility suites (`make test-s3compat`, `make test-mint`) at every release.
- [ ] Bump `NEXO_UPGRADE_FROM` in `make test-upgrade` to the latest published release.
