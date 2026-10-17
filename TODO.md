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
  cosign signatures, images on Docker Hub, Helm chart attached to the release). Before: green CI on `main`,
  CHANGELOG with the date, the Docker Hub setup below, check that the repository secrets exist (Homebrew tap
  token). After: try deb/rpm/brew/docker/helm installs on clean machines.
- [ ] **Docker Hub (required, one-time setup before the first release)**: the images are published only on Docker
  Hub (docs/08 §8.7 "Docker Hub") and the release fails without the credentials. On Docker Hub (account
  `paolor`): create the public repositories `paolor/nexo` and `paolor/nexoctl` and a personal access token
  (Account settings → Personal access tokens) with the Read, Write, Delete scope (Delete is needed by the
  description update). On GitHub (Settings → Secrets and variables → Actions): secrets `DOCKERHUB_USERNAME`
  (`paolor`, the token's user) and `DOCKERHUB_TOKEN`; optional variable `DOCKERHUB_NAMESPACE` only for a
  namespace other than `paolor` (then also update README, docs, Helm values and Compose files). No chart
  repository is needed: the chart is attached to the GitHub release. After the first tag: check the `verify`,
  `helm` and `dockerhub-description` jobs and the README on Docker Hub.
- [ ] **Merge the development branch into `main`** with a PR and run the full CI (nightly jobs included:
  s3-tests, mint, compose).
- [ ] **License**: v1.0 ships under AGPL-3.0 plus a commercial license. Replace "R3n3r0" in `NOTICE` with the
  legal name of the copyright holder if commercial contracts need it; prepare the commercial license agreement.
- [x] **Contributor License Agreement** (required by dual licensing) — DCO with the relicensing grant of
  `CONTRIBUTING.md`: the `DCO` workflow (`.github/workflows/dco.yml`, `.github/scripts/check-dco.sh`, `make dco`)
  fails a pull request with a commit not signed off by its author, and `.github/pull_request_template.md` recalls
  it. Left on GitHub: make the job "Sign-off (DCO)" a required check of `main`; the commits of the development
  branch are not signed off yet (`git rebase --signoff` before the merge PR, or merge it as the owner).

## 2. Quality and security (before or right after v1.0)

- [x] **Console penetration test** (T-9.3) — done in October 2026, report in
  [docs/security/pentest-2026-10.md](docs/security/pentest-2026-10.md): PT-01..PT-04 fixed with regression tests
  (Go and Playwright TD-S-02/09/10), PT-06..PT-11 accepted with their rationale. Second pass (October 2026):
  PT-05 fixed (disabling a user or an access key revokes its console sessions and managed share links for good:
  revocation epoch in IAM, unit and integration tests), and an authenticated ZAP 2.17.0 active scan of the Admin
  API driven by `api/openapi.yaml` (`make zap-scan`, `test/security/zap-api-scan.sh`; report §6.2): 143k requests,
  no 500, only false positives.
- [x] **O_DIRECT and zero-copy** (T-9.2) — done in October 2026, report in
  [docs/bench/README.md §7](docs/bench/README.md#7-o_direct-and-zero-copy-t-92-october-2026): single-drive
  downloads without SSE go from the page cache to the socket with `sendfile` (HTTP/1.1 without TLS; stall bounds
  kept per 256 KiB piece), −47 to −66% server CPU per GiB. O_DIRECT for large data files was built, measured
  slower than page cache + fsync on the VM disk (−30% on 4 MiB synced writes, −52% on SNMD 4 MiB PUT, and cold
  reads right after a PUT) and left out; RPC streaming cannot use `splice` (HTTP/2 data frames, TLS). Left for
  later: re-measure O_DIRECT on NVMe with deep queues (`BenchmarkCreateFile`, `NEXO_BENCH_DIR`).
- [x] **Rate limiting per API and per distribution** (S-OPS-10, S-DST-08, TDS-13) — done in October 2026:
  `internal/service/ratelimit` (token buckets per client address, access key/user, bucket and distribution;
  concurrency of `api.requests_max` / `api.requests_deadline`), subsystem `ratelimit` of the dynamic
  configuration, 503 SlowDown (S3) / 429 (Admin API, websites) with Retry-After, `nexo_ratelimit_throttled_total`.
  Limits are per node, the configuration replicated by the notifier (documented: N nodes admit N times each
  limit). Left for later: an optional cluster-wide split (divide by the online nodes) if operators ask for it.
- [x] **ACME DNS-01** for wildcard certificates of distributions (providers: Cloudflare, Route 53, RFC 2136) —
  done in October 2026: `internal/service/certs/dns01` (providers, propagation check on the authoritative
  nameservers, `NEXO_ACME_DNS_NAMESERVERS`) on the existing `x/crypto/acme` client, the registry of the providers
  with sealed write-only credentials (`/dns-providers`, `nexoctl admin dns-provider`, console Certificates page),
  `tls.challenge`/`tls.dnsProvider` per distribution (a wildcard alias with `auto` needs `dns-01`). Left for later:
  following a CNAME delegation of `_acme-challenge` (acme-dns style) and more providers.
- [x] **`helm lint` + kubeconform** locally — October 2026: helm v3.22.0 and kubeconform v0.7.0 (release binaries,
  SHA-256 verified) ran `make helm-lint HELM=... KUBECONFORM=...` (`deploy/helm/check.sh`): lint, template and
  kubeconform `-strict` of the default values and every `ci/*-values.yaml` (standalone, SNMD, distributed,
  distributed TLS) on Kubernetes 1.27.0 and 1.34.0, and the endpoint assertions of every mode: chart OK, nothing
  to fix. The Makefile already takes `HELM`/`KUBECONFORM` (or `make release-tools`).
- [x] **govulncheck notice GO-2026-5932** (`golang.org/x/crypto/openpgp` unmaintained, not called by the code) —
  cannot be dropped: the notice is module-level and covers every version of `golang.org/x/crypto`, which Nexo
  requires directly (`acme`, `argon2`, `chacha20poly1305`) and through NATS (`nkeys`), grpc and `x/net`;
  `openpgp` is never built. Accepted, documented in
  [docs/security/threat-model.md §10.1](docs/security/threat-model.md#101-govulncheck).

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
