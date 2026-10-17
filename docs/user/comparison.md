# How Nexo compares

There are several open source S3-compatible object stores, and the right one depends on what you run. This page
compares Nexo with the ones people most often consider: MinIO, SeaweedFS, Garage and Ceph RGW. It tries to be
fair: Nexo is young (1.0 was released in October 2026), the others have years of production use behind
them, and each of them does things Nexo does not.

> **About the other projects.** The information on the other projects is a summary written in **October 2026** from their public
> documentation. Projects change: check their own documentation before you decide, and
> [open an issue](https://github.com/R3n3r0/Nexo/issues) if something here is wrong or out of date.

## At a glance

| | Nexo | MinIO (community) | SeaweedFS | Garage | Ceph RGW |
|---|---|---|---|---|---|
| Language | Go | Go | Go | Rust | C++ |
| License | AGPL-3.0 | AGPL-3.0 | Apache-2.0 | AGPL-3.0 | LGPL |
| Status | active, 1.0 in 2026 | archived upstream in 2026, source only; development continues in the commercial AIStor | active | active | active |
| Deployment | one binary: single drive, multiple drives, cluster | one binary: single drive, multiple drives, cluster | master + volume + filer servers (can run in one process) | one binary per node | a Ceph cluster (MON, MGR, OSD) plus RGW daemons |
| Data protection | erasure coding (Reed-Solomon) with bitrot checks and healing | erasure coding with bitrot checks and healing | replication; erasure coding for warm volumes | replication (typically 3 copies) across zones | replication or erasure-coded pools |
| Metadata | stored with the objects, no external database | stored with the objects | filer store (embedded or an external database) | built-in distributed metadata | RADOS |
| Versioning, Object Lock | yes | yes | yes | not in a release yet (in development) | yes |
| Lifecycle | yes | yes | yes | expiration and incomplete uploads only | yes |
| Bucket notifications | webhook, Kafka, NATS, AMQP, Redis, ... with a persistent queue | many targets (webhook, Kafka, NATS, ...) | via the filer's metadata events | no | yes |
| Server-side encryption | SSE-S3, SSE-KMS, SSE-C (built-in KMS or KES) | SSE-S3, SSE-KMS, SSE-C (KES) | SSE-S3, SSE-KMS, SSE-C | SSE-C | SSE-S3, SSE-KMS, SSE-C |
| IAM | users, groups, policies, access keys, STS AssumeRole | users, groups, policies, STS, OIDC, LDAP | users, groups, policies, STS, OIDC | per-key bucket permissions, no policies | users and policies, STS, OIDC, Keystone |
| Web console | included (full administration) | reduced to an object browser in 2025 | basic UIs for master and filer | none built in (community UIs exist) | Ceph Dashboard |
| Geo / multi-site | no | site and bucket replication | async replication between clusters | multi-zone by design | multi-site |
| Beyond S3 | CDN-style distributions on custom domains with cache and ACME certificates | | POSIX/FUSE, WebDAV, HDFS access to the same data | static websites | block (RBD) and file (CephFS) in the same cluster |

## Where Nexo fits

- **You want a single binary with everything included.** Erasure coding, healing, versioning, Object Lock,
  encryption, IAM, a full web console, Prometheus metrics, Grafana dashboards and a CLI for data and
  administration ship together, from one node to a cluster.
- **You are moving off MinIO community.** The deployment model is familiar (drives, erasure sets, pools,
  `/data{1...4}`), the default ports are the same, and the migration copies every version with `nexoctl
  mirror --preserve`. See [Migrating from MinIO](migrate-from-minio.md).
- **You serve content from buckets.** Distributions put a bucket behind your own domains with ACME
  certificates, signed URLs and an in-memory and on-disk cache, without an extra CDN or proxy layer.
- **You care about S3 behaviour, not just the common calls.** Nexo passes all 451 applicable tests of the
  Ceph `s3-tests` suite and the 638 tests of MinIO's `mint` SDK suites, in every deployment mode, in CI. The
  [S3 compatibility](reference/s3-compatibility.md) page lists what is supported and what is not.

## Where another project fits better

- **Geo-distribution across sites or weak links** — Garage is designed for nodes spread over several
  locations; Ceph RGW and MinIO's commercial product have multi-site replication. Nexo has no replication
  between clusters yet.
- **Billions of small files, or file-system access to the same data** — SeaweedFS's volume design and its
  filer (FUSE, WebDAV, HDFS) are built for this.
- **One platform for block, file and object at large scale** — Ceph, if you can run it.
- **External identity (OIDC, LDAP, Keystone)** — not available in Nexo yet; MinIO, SeaweedFS and Ceph RGW support
  some of them.
- **A long production track record** — every other project here has years of it. Nexo has extensive
  automated tests (multi-process clusters, fault injection, upgrade tests, fuzzing) but a short history in
  the field: try it on your workload first.

## Performance

Throughput depends on drives, network, object sizes and concurrency more than on the server. Instead of
publishing numbers measured elsewhere, Nexo ships a script that runs MinIO's `warp` benchmark against Nexo and
any other S3 endpoint on **your** hardware: see [`scripts/bench/warp-compare.sh`](https://github.com/R3n3r0/Nexo/blob/main/scripts/bench/warp-compare.sh) in the repository.
