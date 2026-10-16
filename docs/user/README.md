# Nexo user documentation

**Nexo** is an object store compatible with the **Amazon S3** API: a single binary (`nexo`) contains the
S3 server, the Admin API and the web console; `nexoctl` is the CLI for data and administration. The same
binary runs on a single drive, on multiple drives with erasure coding, and as a multi-node cluster.

This guide is for those who install, use and operate Nexo. The project documents (architecture, roadmap,
test plan) are in [`docs/`](https://github.com/R3n3r0/Nexo/tree/main/docs) in the repository.

## Where to start

| If you want to… | Read |
|-----------------|------|
| try Nexo in 5 minutes | [Quickstart](quickstart.md) |
| install it on a server, in Docker or on Kubernetes | [Installation](installation.md) |
| choose between single drive, multiple drives and cluster | [Deployment modes](deployment-modes.md) |
| look up every flag and environment variable | [Configuration](configuration.md) |

## Operations guide

| Topic | Page |
|-------|------|
| Health checks, Prometheus metrics, Grafana, alerts | [Monitoring](operations/monitoring.md) |
| Live trace and logs, audit log, OpenTelemetry | [Trace, logs and audit](operations/trace-log-audit.md) |
| Healing, replacing a drive | [Healing and disks](operations/healing-disks.md) |
| Rolling upgrade, expansion with a new pool | [Upgrade and expansion](operations/upgrade-expansion.md) |
| Configuration backup, migration between modes | [Backup and migration](operations/backup-migration.md) |
| Quorum, clock skew, slow drives, permissions | [Troubleshooting](operations/troubleshooting.md) |

## Security

| Topic | Page |
|-------|------|
| Users, groups, policies (ready-made examples), access keys, STS | [Users, policies and credentials](security/users-policies.md) |
| Share links, presigned URLs, POST policies, anonymous access | [Sharing and anonymous access](security/sharing.md) |
| Access rules with prefix, glob and regex | [Access rules](security/access-rules.md) |
| TLS, certificates, mTLS between nodes | [TLS and certificates](security/tls.md) |
| KMS, server-side encryption, key rotation | [KMS and SSE](security/kms-sse.md) |
| Threats and mitigations | [Threat model](security/threat-model.md) |

## Features

| Topic | Page |
|-------|------|
| Custom domains, behaviors, signed URLs, cache | [Distributions and cache](distributions/README.md) |
| Static site on two domains behind nginx with ACME TLS | [Tutorial: static site](distributions/tutorial-static-site.md) |
| Notifications to webhooks and Kafka, lifecycle rules | [Events and lifecycle](events-lifecycle.md) |

## Reference

| Reference | Page |
|-----------|------|
| All the `nexoctl` commands (generated from the code) | [CLI reference](reference/cli.md) |
| Admin API (`/api/v1`, OpenAPI 3.1) | [Admin API reference](reference/admin-api.md) |
| Supported S3 APIs, differences, `s3-tests` results | [S3 compatibility](reference/s3-compatibility.md) |

## Conventions

- The examples use the alias `local` (see [Quickstart](quickstart.md#4-configure-nexoctl)), the root user
  `admin` and the password `supersecret`: in production use a long, random password.
- The default ports are **9000** (S3 API, health, metrics, RPC between nodes) and **9001** (console and
  Admin API).
- The pages say when a behavior depends on the mode; unless noted otherwise, it applies to
  **SNSD**, **SNMD** and **cluster**.
- The site of this documentation is built with `make docs-site` (MkDocs Material, see
  [CLI reference](reference/cli.md#generating-the-documentation)).
- Every page has an Italian translation next to it (`<page>.it.md`); on the site, use the language
  selector.
