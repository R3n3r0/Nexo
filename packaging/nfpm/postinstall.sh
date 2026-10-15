#!/bin/sh
# Post-install script of the nexo deb/rpm package (nfpm, .goreleaser.yaml).
#
# Makes the unit visible to systemd. The service is never enabled nor
# restarted automatically:
#   - on a first install /etc/default/nexo must be edited first (the root
#     password is empty on purpose, so the service refuses to start with a
#     clear error until it is set);
#   - on an upgrade the operator restarts the nodes, one at a time in a
#     cluster (docs/08 §8.7), so that the erasure sets keep their quorum.
#
#   deb: postinst configure [<old-version>]   (old version empty on install)
#   rpm: %post <number of versions installed>  (1 install, 2+ upgrade)
set -e

mode=
case "$1" in
    configure) if [ -n "$2" ]; then mode=upgrade; else mode=install; fi ;;
    1) mode=install ;;
    [0-9]*) mode=upgrade ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ "$mode" = upgrade ]; then
    cat <<'EOF'
Nexo upgraded. The running server keeps the previous version until it is
restarted: systemctl restart nexo (in a cluster one node at a time, waiting
for "nexo healthcheck --ready" before the next one).
EOF
elif [ "$mode" = install ]; then
    cat <<'EOF'
Nexo installed. Next steps:
  1. edit /etc/default/nexo: NEXO_ROOT_USER, NEXO_ROOT_PASSWORD (at least
     8 characters) and NEXO_ENDPOINTS (default /var/lib/nexo/data)
  2. data directories outside /var/lib/nexo must be owned by nexo, e.g.:
     install -d -o nexo -g nexo -m 0750 /mnt/disk1
  3. nexo server --dry-run checks the configuration (run it as nexo)
  4. systemctl enable --now nexo
EOF
fi

exit 0
