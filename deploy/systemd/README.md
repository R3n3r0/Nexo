# Nexo as a systemd service

| File | Purpose |
|------|---------|
| `nexo.service` | Unit: runs `nexo server` as user `nexo`, hardened, with `--dry-run` as a pre-start check |
| `nexo.env.example` | Template for `/etc/nexo/nexo.env` (credentials, `NEXO_ENDPOINTS`, options) |
| `preinstall.sh`, `postinstall.sh` | Package scripts used by the `.deb`/`.rpm` built with GoReleaser |

The unit takes the endpoints from `NEXO_ENDPOINTS`, so it is the same for every mode (SNSD, SNMD,
cluster): change the variable, not the unit.

```bash
# binaries from a release archive
sudo install -m 0755 nexo nexoctl /usr/local/bin/
sudo useradd --system --home-dir /var/lib/nexo --shell /usr/sbin/nologin nexo
sudo install -d -o nexo -g nexo -m 0750 /var/lib/nexo /srv/nexo
sudo install -d -m 0750 /etc/nexo
sudo install -m 0640 -g nexo nexo.env.example /etc/nexo/nexo.env   # then edit it
sudo cp nexo.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nexo
systemctl status nexo
journalctl -u nexo -f
```

With the `.deb`/`.rpm` packages the user, the unit and `/etc/nexo/nexo.env` are installed for you;
edit the env file and run `systemctl enable --now nexo`.

Notes:

- `ProtectSystem=full` leaves `/srv`, `/var` and `/mnt` writable; data directories elsewhere must be
  added to `ReadWritePaths`.
- `systemctl reload nexo` sends `SIGHUP` (TLS certificate reload).
- Ports below 1024 need `AmbientCapabilities=CAP_NET_BIND_SERVICE` (commented in the unit).
