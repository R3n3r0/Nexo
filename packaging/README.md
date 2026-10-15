# Packaging

Release packaging of Nexo (T-9.5, docs/08 §8.7). The release itself is described by
[`../.goreleaser.yaml`](../.goreleaser.yaml) and run by [`../.github/workflows/release.yml`](../.github/workflows/release.yml).

| File | Purpose |
|------|---------|
| `nfpm/preinstall.sh` | deb/rpm: creates the `nexo` system user and group (idempotent) |
| `nfpm/postinstall.sh` | deb/rpm: `systemctl daemon-reload`; next steps on install, restart advice on upgrade; never enables nor restarts the service |
| `nfpm/preremove.sh` | deb/rpm: stops and disables the service on removal (not on upgrade) |
| `nfpm/postremove.sh` | deb/rpm: `systemctl daemon-reload`; data, user and (until a purge) `/etc/default/nexo` are kept |
| `nfpm/nexo.sysusers.conf` | deb/rpm: `/usr/lib/sysusers.d/nexo.conf`, the `nexo` user and group for `systemd-sysusers` (image-based systems) |
| `smoke.sh` | TM-10: version of both binaries, `nexo server --dry-run`, a single-drive server (readiness, embedded console, S3 round trip with `nexoctl`, SIGTERM); `RUNNER=qemu-aarch64-static` for linux/arm64 |
| `systemd-verify.sh` | `systemd-analyze verify` and `security` of `deploy/systemd/nexo.service` on a scratch root (no root privileges needed) |

The scripts receive the package manager arguments of both formats (deb: `install|upgrade|remove|purge
…`; rpm: the number of installed versions) and handle a system without systemd (containers).

## Homebrew

GoReleaser generates the casks `nexo` and `nexoctl` (`homebrew_casks` in `.goreleaser.yaml`, written to
`dist/homebrew/Casks/` by a snapshot) and pushes them to `r3n3r0/homebrew-tap` (`Casks/`) when the
release workflow has the `HOMEBREW_TAP_GITHUB_TOKEN` secret (a token allowed to push to the tap).
They install the binaries of the darwin and linux archives, generate the `nexoctl` shell completions and
remove the macOS quarantine attribute (the binaries are signed with cosign, not notarized by Apple):

```bash
brew install r3n3r0/tap/nexoctl
brew install r3n3r0/tap/nexo
```

## Checks

```bash
make release-check release-snapshot release-smoke   # GoReleaser config, local release in dist/, TM-10
make systemd-verify helm-lint hadolint               # unit, chart, Dockerfiles (make packaging: all of them)
```
