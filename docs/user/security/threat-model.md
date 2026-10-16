# Threat model

The complete analysis (STRIDE per surface: S3 API, Admin API and console, RPC between nodes, distributions,
cache, events, KMS, CLI), with the mitigations in the code, the automated scans and the fuzzing, is in
[docs/security/threat-model.md](https://github.com/R3n3r0/Nexo/blob/main/docs/security/threat-model.md).
The penetration test of the console and of the Admin API (October 2026) is reported in
[docs/security/pentest-2026-10.md](https://github.com/R3n3r0/Nexo/blob/main/docs/security/pentest-2026-10.md).
This page summarizes what matters to whoever operates a deployment.

## Summary

| Threat | Surface | Mitigation in Nexo |
|--------|---------|--------------------|
| Spoofing | S3 API, Admin API, RPC between nodes | SigV4 with constant-time comparison; console sessions with `HttpOnly`/`SameSite=Strict` cookies, CSRF tokens and HSTS over TLS; short-lived tokens between nodes bound to the single call; optional mTLS between nodes |
| Tampering | data on disk, traffic | per-block checksums (bitrot), TLS 1.2+, SSE encryption |
| Repudiation | administrative and data operations | audit log for every request, deliverable to an external target |
| Information disclosure | console, errors, logs | no secrets in the logs (credentials, signatures, tokens, keys redacted), generic errors towards anonymous users, IAM secrets encrypted at rest |
| Denial of service | uploads, listings, login, live streams | size limits and timeouts, login attempt limits, at most 256 live trace and log streams per node, bounded peer responses, linear-time RE2 regexes |
| Elevation of privilege | policy engine, console | deny by default, authorization always server side, the console shows only what the user can do but the server checks every request again |

## Production recommendations

1. **TLS everywhere**: `--certs-dir` on every listener and `https://` endpoints with
   `--tls-internode-verify` in a cluster ([TLS](tls.md)). Without TLS, cookies, signatures and traffic
   between nodes travel in clear text.
2. **Long, random root credentials**, the same on every node and different between deployments: they also
   sign the tokens between nodes. Disable root on the S3 API (`api.root_access=off`) and use dedicated users.
3. **A reverse proxy in front** for per-client request and connection limits, which Nexo does not enforce
   yet on the S3 API and on public domains; never expose `/nexo/rpc/`.
4. **Least privilege**: policies per bucket or prefix, access keys with an expiry and an inline policy,
   groups; `diagnostics` for monitoring; `admin:Profiling` only for those who administer the nodes.
5. **External audit**: an `audit_webhook` or `audit_kafka` target pointing to a system that the Nexo
   administrator cannot modify.
6. **KMS**: in production prefer KES with a client certificate over the built-in KMS, whose master key lives
   in the process environment; in any case keep a safe copy of the keys.
7. **Block public access** at cluster level if no bucket should be public, and periodically review the
   anonymous access (`nexoctl anonymous list`) and the active shares.
8. **Updates**: apply the releases with security fixes; `make security` (govulncheck,
   gosec, npm audit) runs in CI on every change.
