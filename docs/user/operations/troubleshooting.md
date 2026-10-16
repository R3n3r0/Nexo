# Troubleshooting

First of all, collect the status:

```bash
nexoctl admin info local                      # nodes, drives, sets, quorum, clock skew, healing
curl -s http://nexo1.lan:9000/nexo/health/cluster
nexoctl admin logs --level warn --last 200 local
nexoctl admin trace --errors local            # the failing requests, in real time
nexo server --dry-run …                       # the configuration of a node that does not start
```

With `--debug` every `nexoctl` command prints the HTTP requests and responses on stderr (secrets redacted).

## The server does not start

| Message | Cause and remedy |
|---------|------------------|
| `root user is required (NEXO_ROOT_USER, at least 3 characters)` / `root password is required (… at least 8 characters)` / `root password must be at least 8 characters (NEXO_ROOT_PASSWORD has 5)` | Root credentials missing or too short. With systemd: `NEXO_ROOT_PASSWORD` in `/etc/default/nexo` (empty after the package installation) |
| `drive path must be absolute: "./data"` | Use absolute paths (`$PWD/data`, `/srv/nexo`) |
| `either all or none of the arguments must contain ellipsis patterns` | Do not mix arguments with `{1...4}` and without |
| `… is not writable by this user (uid …)` | Owner or permissions of the drive: `chown nexo:nexo` (systemd) or `chown 65532:65532` (Docker); with systemd also `ReadWritePaths=` in the drop-in |
| `address and console address must differ` / `address already in use` | Ports: `--address` and `--console-address` must be different and free |
| `invalid configuration: kms: …` | Format of the master key (`<name>:<base64 of 32 bytes>`) or incomplete KES configuration |
| `tls internode verify needs TLS` | `--tls-internode-verify` requires `--certs-dir` and `https://` endpoints |
| `format mismatch` / `belongs to deployment …` | The drives belong to another deployment or to another topology: do not reuse drives without wiping them, use the same command line as the installation |

`nexo server --dry-run` reports all these problems at once, without writing to the drives.

## A cluster node keeps waiting (bootstrap)

The logs periodically show:

```text
cluster: waiting for the peers: 2 of 4 nodes verified, 3 needed   offline=[…] different_topology=[…]
cluster: waiting for the drives   missing=[…]
```

- `offline`: the node cannot reach those peers on port 9000. Check name resolution **from this
  node** (`getent hosts nexo3.lan`), the firewall, and that the peer is running.
- `different_topology`: those nodes were started with different arguments (pool order, names, ports).
  The command line must be identical on every node.
- `incompatible_version`: different major version; upgrade the nodes to the same release.
- If **more than half** of the nodes disagree, the node exits with `topology mismatch` (or `ErrKMSMismatch` if
  the KMS configuration differs) and the list of differences.
- `waiting for the drives`: drives are missing for the quorum of some set (drives not mounted, nodes down).

Until the bootstrap completes the node answers `503` to everything: this is normal when the nodes start
together.

## Quorum errors (`503 SlowDownWrite`, `SlowDownRead`, `XNexoLockTimeout`)

An S3 request that does not reach the quorum answers `503` with code `SlowDownWrite` (write
quorum), `SlowDownRead` (read quorum), `XNexoLockTimeout` (lock not obtained within 30 s:
resource locked by another operation or too many nodes offline) or `ServiceUnavailable`
(a required drive or node is offline); `SlowDown` remains only for throttled requests. The SDKs
retry `503` responses; `nexoctl` does not retry the first three (the server has already waited) and prints
`the deployment has no quorum (too many drives or nodes offline): check "nexoctl admin info"` for
`SlowDownWrite`/`SlowDownRead`, and `the object is locked by another operation (a slow or stalled read or write)
or too many nodes are offline` for `XNexoLockTimeout`: with every node online it is contention (for example a
download of more than 8 parts whose client stopped reading, aborted after `NEXO_SEND_TIMEOUT`). When fewer
nodes than the lock quorum are reachable the request fails immediately, without waiting the 30 s. In the
Admin API the `code` is `read_quorum`, `write_quorum`, `lock_timeout`, `node_offline` or `disk_not_found`,
which the console translates.

1. `nexoctl admin info local`: which sets are `degraded` or without quorum, which drives or nodes are offline.
2. Bring nodes and drives back online (network, service, mounts). Reads come back as soon as there are D
   consistent drives, writes with D (or D+1) drives and with N/2+1 nodes for the locks.
3. When they are back, the MRF queue repairs the objects written in the meantime; verify with
   `nexoctl admin heal -r --dry-run local`.

A set without quorum does not block the others: the objects of the other sets stay available. Locks left
held by a crashed node expire after `NEXO_LOCK_STALE_AFTER` (60 s); in extreme cases use
`nexoctl admin lock list local` and `nexoctl admin lock clear local RESOURCE` (requires `admin:ForceUnlock`).

## Clock skew

Symptoms:

- `cluster: clock skew between nodes` in the logs, skew reported in `nexoctl admin info`;
- requests rejected with `RequestTimeTooSkewed` (the client is more than 15 minutes off from the server);
- presigned URLs or share links that appear expired or "not yet valid".

Remedy: synchronize every node and client with NTP (chrony, systemd-timesyncd) and check with
`chronyc tracking` or `timedatectl`. The warning threshold between nodes is 2 seconds. Distributed locks do
not depend on clocks; the order of the versions of an object stays monotonic even with skewed clocks.

## Slow or faulty drives

- `nexoctl admin trace --min-duration 1s local`: which operations are slow and on which node.
- Metrics `nexo_drive_operation_seconds_total{drive,op}` / `nexo_drive_operations_total` (average latency
  per drive) and `nexo_drive_errors_total{drive,type}` (`timeout`, `faulty`, `corrupt`, `full`, …).
- `nexoctl admin speedtest local drive` measures write and read speed of the node's local drives (off
  hours).
- A drive that keeps returning errors or timeouts must be replaced ([Healing and disks](healing-disks.md)):
  with enough parity the set keeps working without it.
- Check the file system: XFS, `noatime`, no RAID or LVM under Nexo, no other process making heavy use of
  the same drive; `dmesg` for I/O errors.

## Permissions and denied access

| Symptom | Check |
|---------|-------|
| `AccessDenied` (403) for a user | `nexoctl admin policy simulate --user alice --action s3:GetObject local/photos/a.jpg -v` shows the decision and the deciding statement |
| The console does not show a menu or an action | the console shows only what the user can do: check the policies of the user and of their groups (`nexoctl admin user info local alice`) |
| `alias set` exits with code 4 | unknown, disabled or expired access key, or wrong secret |
| "listing the buckets is denied" on `alias set` | valid credentials but without `s3:ListAllMyBuckets`: normal for users restricted to one bucket |
| Console login: "Too many failed attempts" | wait a minute: failed attempts are rate-limited |
| Root rejected on the S3 API | `api.root_access` is `off`: `nexoctl admin config set local api root_access=on` (the console keeps working) |
| `503 SlowDown` with `Retry-After`, or `429 too_many_requests` on the console | a rate limit of the node refused the request: `nexoctl admin config get local ratelimit api` (the limits are per node), `nexo_ratelimit_throttled_total{api,kind}` names the limit; a distribution has its own (`nexoctl dist info`). `SlowDownRead`/`SlowDownWrite` are a lost quorum instead |
| `SignatureDoesNotMatch` behind a proxy | the proxy changes the `Host` header (or the port): it must forward it unchanged (`proxy_set_header Host $http_host`) |
| Anonymous request rejected despite a public rule | block public access enabled on the bucket or the cluster (`nexoctl access block-public status local/bucket`) |

## Space, quotas and objects

| Code | Meaning |
|------|---------|
| `XNexoQuotaExceeded` (507) | bucket quota exceeded (`nexoctl quota info local/bucket`) |
| `XNexoStorageFull` (507) | drives full: free some space or add a pool |
| `XNexoParentIsObject` (400) | a key cannot also be the prefix of other keys: `a/b` exists and `a/b/c` is written |
| `XNexoObjectExistsAsDirectory` (400) | the opposite case: `a/b/c` exists and `a/b` is written |
| `XNexoObjectLocked` | version protected by retention or legal hold |

## TLS

- A client rejects the certificate: check the chain and the names
  (`openssl s_client -connect nexo.lan:9000 -servername nexo.lan`), that the certificate covers the name used
  by the client and, in a cluster, the endpoint names.
- Certificate renewed but not served: the directory is watched and reloaded within a few seconds (or
  `systemctl reload nexo`); an inconsistent key/certificate pair is discarded and the previous
  certificates stay in place (logs and metric `nexo_tls_server_certificate_reloads_total{result}`).
- Container `unhealthy` with TLS: the healthcheck uses `NEXO_CERTS_DIR` to switch to HTTPS; check that the
  variable is set in the container.

## Notifications and events

- `nexoctl admin notify targets list local`: online/offline status and queue of every target.
- `nexoctl admin notify targets test local arn:nexo:sqs:us-east-1:_:webhook`: sends a test event.
- Webhooks do not follow redirects (a `3xx` is a failed delivery); the events stay queued
  (`queue_limit`) until the target comes back.

## Collecting information for a bug report

```bash
nexo version
nexoctl --json admin info local > info.jsonl
nexoctl admin logs --last 1000 --timeout 5s --json local > logs.jsonl
nexoctl admin profile --type goroutine local          # if a node seems stuck
```

Logs and traces contain no secrets, but they do contain bucket names, keys and IP addresses: review them
before sharing them.
