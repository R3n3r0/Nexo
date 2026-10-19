# Nexo as a systemd service

| File | Purpose |
|------|---------|
| `nexo.service` | Unit: runs `nexo server` as user `nexo` in a sandbox (`ProtectSystem=strict`, no capability, syscall filter), with `nexo server --dry-run` as a pre-start check |
| `nexo.env.example` | `/etc/default/nexo`: credentials, `NEXO_ENDPOINTS`, options |
| `../../packaging/nfpm/*.sh` | Scripts of the `.deb`/`.rpm` packages (user `nexo`, systemd reload, stop on removal) |
| `nexo-media-worker.service` | Unit of the [media worker](../../docs/user/media-worker.md): dynamic user, same sandbox, lower priority than Nexo |
| `nexo-media-worker.env.example` | `/etc/default/nexo-media-worker`: S3 endpoint, credentials and token of the worker |

The unit takes the endpoints from `NEXO_ENDPOINTS`, so it is the same for every mode (SNSD, SNMD,
cluster) and every node: change the variable, never the unit. `systemd-analyze security nexo`
rates it 1.5 ("OK").

## With the packages (deb/rpm)

```bash
sudo apt install ./nexo_1.0.0_amd64.deb ./nexoctl_1.0.0_amd64.deb     # or: sudo dnf install ./nexo-1.0.0-1.x86_64.rpm ...
sudoedit /etc/default/nexo          # NEXO_ROOT_PASSWORD (empty: the service refuses to start), NEXO_ENDPOINTS
sudo systemctl enable --now nexo
journalctl -u nexo -f               # a configuration error is reported by the --dry-run pre-start check
```

The package creates the `nexo` user and group, `/var/lib/nexo` (default drive `/var/lib/nexo/data`),
`/etc/nexo` (certificates, secret files) and installs the unit in `/usr/lib/systemd/system`. It
never enables, restarts or removes data by itself: an upgrade leaves the old process running until
`systemctl restart nexo`; a removal stops and disables the service but keeps `/var/lib/nexo`, the
user and (until a purge) `/etc/default/nexo`.

## Manual install (release archive)

```bash
sudo install -m 0755 nexo nexoctl /usr/local/bin/
sudo useradd --system --home-dir /var/lib/nexo --shell /usr/sbin/nologin nexo
sudo install -d -m 0750 -g nexo /etc/nexo
sudo install -m 0640 -g nexo systemd/nexo.env.example /etc/default/nexo   # then edit it
sudo install -m 0644 systemd/nexo.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nexo
```

The unit runs `nexo` by name: systemd (≥ 239) finds `/usr/local/bin/nexo` before `/usr/bin/nexo`.

## Drives outside /var/lib/nexo

The file system is read-only for the service except `/var/lib/nexo` and `/var/cache/nexo`. Every
other drive needs a drop-in and the right owner:

```bash
sudo install -d -o nexo -g nexo -m 0750 /mnt/disk{1..4}
sudo systemctl edit nexo
#   [Service]
#   ReadWritePaths=/mnt/disk1 /mnt/disk2 /mnt/disk3 /mnt/disk4
# /etc/default/nexo:  NEXO_ENDPOINTS=/mnt/disk{1...4}
sudo systemctl restart nexo
```

A forgotten path is reported by the `--dry-run` pre-start check (drive not writable) and the
service does not start. Data under `/home` is not reachable (`ProtectHome=true`).

## Cluster of several machines

Same packages, same unit, same `/etc/default/nexo` on every node, except nothing: the endpoints list
all the nodes and each node recognizes its own by host name or address.

```bash
# /etc/default/nexo on nexo1.lan ... nexo4.lan (4 nodes x 4 drives, one pool)
NEXO_ROOT_USER=admin
NEXO_ROOT_PASSWORD=<the same on every node>
NEXO_ENDPOINTS=http://nexo{1...4}.lan:9000/mnt/disk{1...4}
# optional: TLS between the nodes and for the clients (https endpoints)
#NEXO_CERTS_DIR=/etc/nexo/certs
```

1. The host names (`nexo1.lan` …) must resolve to the node addresses on every node (DNS or
   `/etc/hosts`), and port 9000 must be open between the nodes (internode RPC), 9000/9001 for the
   clients (or a load balancer, `deploy/proxy`).
2. Keep the clocks in sync (chrony, systemd-timesyncd): the cluster checks the clock skew between
   the nodes, and signed requests carry a timestamp.
3. Start the service on all the nodes (`systemctl enable --now nexo`): the first ones wait for the
   others (bootstrap) and become ready together.
4. Expansion with a second pool: append the new endpoints to `NEXO_ENDPOINTS` on **every** node
   (`NEXO_ENDPOINTS="http://nexo{1...4}.lan:9000/mnt/disk{1...4} http://nexo{5...8}.lan:9000/mnt/disk{1...4}"`,
   quoted because of the space) and restart all the nodes.
5. Upgrades: install the new package everywhere, then restart **one node at a time**, waiting for
   `nexo healthcheck --ready` (with TLS `nexo healthcheck --ready --certs-dir /etc/nexo/certs`: HTTPS,
   trusting the certificates of the node) or `curl -f http://localhost:9000/nexo/health/ready` before
   the next.

## Notes

- `systemctl reload nexo` sends `SIGHUP`: the TLS certificates of `NEXO_CERTS_DIR` are reloaded
  without dropping connections.
- `Type=exec`, not `Type=notify`: a cluster node becomes ready only when enough nodes have started
  (bootstrap), so a readiness notification would block `systemctl start` on the first nodes of a
  cluster started one at a time. Wait for readiness with `nexo healthcheck --ready` instead.
- The `nexo` user is created by the package (`useradd`) and declared in
  `/usr/lib/sysusers.d/nexo.conf` for `systemd-sysusers`.
- Ports below 1024 (`NEXO_WEB_ADDRESS=:80`) need `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` and
  `AmbientCapabilities=CAP_NET_BIND_SERVICE` in a drop-in.
- `LimitNOFILE=1048576`, `TasksMax=infinity`: one file descriptor per open object part and connection.
- Checked in CI by `make systemd-verify` (`systemd-analyze verify` and `systemd-analyze security`
  on a root with the unit, the env file and the binary).

## Media worker

`nexo-media-worker.service` runs [nexo-media-worker](../../docs/user/media-worker.md) (renditions, posters,
thumbnails, image variants) on the same machine as Nexo or another one. It is installed by the
`nexo-media-worker` deb/rpm package (with `/etc/default/nexo-media-worker`, root, mode 0600) and shipped in
`systemd/` of its archives. `systemd-analyze security nexo-media-worker` rates it 1.3 ("OK").

- `DynamicUser=yes`: no account to create; the state directory `/var/lib/nexo-media-worker` (a symlink to
  `/var/lib/private/nexo-media-worker`) holds the job queue, the work files and OpenH264, downloaded from
  Cisco at the first start.
- The sandbox of `nexo.service` (read-only file system, no capability, `@system-service` system calls,
  `MemoryDenyWriteExecute`: the Cisco builds of OpenH264 need no writable code), plus `PrivateUsers`.
- `OOMScoreAdjust=500`, `CPUWeight=50`, `IOWeight=50`: on a shared machine Nexo keeps priority. Add a memory
  limit in a drop-in (`MemoryMax=4G`): about 1 GiB per video job and 2 GiB per image job.

```bash
sudo apt install ./nexo-media-worker_1.0.0_amd64.deb    # or: sudo dnf install ./nexo-media-worker-1.0.0-1.x86_64.rpm
sudoedit /etc/default/nexo-media-worker                 # NEXO_WORKER_S3_ENDPOINT, _ACCESS_KEY, _SECRET_KEY, _AUTH_TOKEN
sudo systemctl enable --now nexo-media-worker
# on Nexo: the user and policy of the worker, then the target (docs/user/media-worker.md, "Setup")
nexoctl admin config set local notify_media_worker enable=on endpoint=http://<this host>:9600/jobs auth_token=<token>
```

Manual install from the archive:

```bash
sudo install -m 0755 nexo-media-worker /usr/local/bin/
sudo install -m 0600 systemd/nexo-media-worker.env.example /etc/default/nexo-media-worker   # then edit it
sudo install -m 0644 systemd/nexo-media-worker.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now nexo-media-worker
```

`make systemd-verify` checks both units (`systemd-analyze verify` and `security`).
