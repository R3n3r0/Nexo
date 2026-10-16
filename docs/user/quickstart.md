# Quickstart

In five minutes: a Nexo server on one drive, the console in the browser, `nexoctl` configured, a bucket,
an uploaded file and a revocable share link. Choose the **binary** or **Docker**: the steps from 3
onward are the same.

## 1. Start the server

### With the binary

Download the `nexo_X.Y.Z_<os>_<arch>.tar.gz` and `nexoctl_X.Y.Z_<os>_<arch>.tar.gz` archives from the
[releases page](https://github.com/R3n3r0/Nexo/releases) (or build from source with
`make build`, see [Installation](installation.md#from-source)), then:

```bash
tar xzf nexo_*_linux_amd64.tar.gz nexo
tar xzf nexoctl_*_linux_amd64.tar.gz nexoctl
mkdir -p ~/nexo-data

export NEXO_ROOT_USER=admin
export NEXO_ROOT_PASSWORD=supersecret      # at least 8 characters
./nexo server ~/nexo-data
```

The drive path must be **absolute** (the shell expands `~`; a relative path such as `./data`
is rejected). The server writes JSON logs to stderr; the last startup line is:

```json
{"level":"INFO","msg":"nexo started","version":"…","mode":"snsd","s3":"0.0.0.0:9000","console":"0.0.0.0:9001"}
```

In the following examples `nexoctl` is on the `PATH` (otherwise use `./nexoctl`).

### With Docker

```bash
docker run -d --name nexo -p 9000:9000 -p 9001:9001 \
  -e NEXO_ROOT_USER=admin -e NEXO_ROOT_PASSWORD=supersecret \
  -v nexo-data:/data ghcr.io/r3n3r0/nexo:latest server /data

docker ps --filter name=nexo     # STATUS becomes "healthy" within a few seconds
```

The image also contains `nexoctl` (`docker exec nexo /nexoctl …`). To build it from source:
`make docker` (local image `ghcr.io/r3n3r0/nexo:latest`).

The root credentials are passed **only** through environment variables (or from files with
`NEXO_ROOT_USER_FILE` and `NEXO_ROOT_PASSWORD_FILE`): there is no flag, so that they never show up in the
process list.

## 2. Check that it responds

```bash
curl -s http://localhost:9000/nexo/health/live
# {"ok":true,"status":"ok"}
curl -s http://localhost:9000/nexo/health/ready
# {"ok":true,"status":"ok","checks":{"storage":"ok"}}
```

## 3. Log in to the console

Open <http://localhost:9001>: on the **Sign in to Nexo** page enter **Access key** `admin` and
**Secret key** `supersecret`. The session lasts 12 hours. From the console you can create buckets, upload
files (folders too, with drag and drop), share them and manage users and policies.

## 4. Configure nexoctl

An **alias** binds a name to an endpoint and to credentials (saved in `~/.nexoctl/config.json`,
permissions 0600). The credentials are verified before they are saved:

```console
$ nexoctl alias set local http://localhost:9000 admin supersecret
Added alias "local" for http://localhost:9000 (credentials verified).

$ nexoctl admin info local
local  Nexo v1.0.0 (commit 2558bd3, built 2026-10-08T12:00:00Z)
Mode: snsd  Region: us-east-1  Deployment: db114646-23ae-4596-966e-a3ed2ef8248f
Uptime: 8s  Buckets: 0  Objects: 0  Usage: 0B
Pool 1: 1 set(s) of 1 drive(s), parity 0, quorum read 1 write 1
  set 1  ok  1 of 1 drive(s) online
Nodes: 1 online, 0 offline
  local  online  version v1.0.0  uptime 8s  1 drive(s)  (this node)
    /data  online  used 1.2GiB of 50.0GiB (free 48.8GiB)
```

With Docker use the CLI inside the container: `docker exec nexo /nexoctl alias set local http://localhost:9000 admin supersecret`
and prefix the following commands with `docker exec nexo /nexoctl` instead of `nexoctl`.

The administration commands (`nexoctl admin …`, managed links) talk to the Admin API on the port
after the alias's one (9000 → 9001); `--admin-url` sets it explicitly.

## 5. Create a bucket and upload a file

```console
$ echo "hi Nexo!" > hello.txt
$ nexoctl mb local/photos
Bucket created successfully `local/photos`.
$ nexoctl cp hello.txt local/photos/
`hello.txt` -> `local/photos/hello.txt` (9B)
Copied 1 object (9B).
$ nexoctl ls local/photos
[2026-10-15 11:42:10 UTC]      9B STANDARD hello.txt
```

`cp -r` copies whole folders, `mirror` synchronizes a directory with a bucket, and `cat`, `stat` and `rm`
work as you would expect: the complete list is in the [CLI reference](reference/cli.md).

## 6. Share with a link

```console
$ nexoctl share download --expire 2h local/photos/hello.txt
URL: local/photos/hello.txt
Expire: 2h0m0s
Share: http://localhost:9000/photos/hello.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=share-2h5zvraevd7j%2F…
ID: 2h5zvraevd7j (revoke with: nexoctl share revoke local 2h5zvraevd7j)

$ curl -s "http://localhost:9000/photos/hello.txt?X-Amz-Algorithm=…"
hi Nexo!
```

The link is **managed**: it is signed by a dedicated service account and can be revoked at any time, with
immediate effect:

```console
$ nexoctl share list local
ID             OWNER            ACCESS   EXPIRES                 TARGET
2h5zvraevd7j   admin            download 2026-10-15 13:42:10 UTC local/photos/hello.txt
$ nexoctl share revoke local 2h5zvraevd7j
Revoked share 2h5zvraevd7j.
```

After the revocation the same URL answers `403`. For a classic (non-revocable) presigned URL use
`nexoctl share download --raw` or `nexoctl presign`; the details are in
[Sharing and anonymous access](security/sharing.md).

## 7. Stop the server

`Ctrl-C` (or `docker stop nexo`) stops the server gracefully: new requests get `503`, the ones
in flight complete. The data stays in the directory (or in the volume) and the next start finds it again.

## Next steps

- a non-root user for applications: [Users, policies and credentials](security/users-policies.md);
- more drives or more nodes: [Deployment modes](deployment-modes.md);
- TLS: [TLS and certificates](security/tls.md);
- metrics and alerts: [Monitoring](operations/monitoring.md);
- any S3 SDK works with endpoint `http://localhost:9000`, region `us-east-1` and the
  user's credentials (path-style; virtual-host style with `NEXO_DOMAIN`, see
  [Configuration](configuration.md)).
