#!/bin/sh
# Pre-remove script of the nexo-media-worker deb/rpm package (nfpm,
# .goreleaser.yaml).
#
# Stops and disables the service when the package is removed, not when it
# is upgraded.
#
#   deb: prerm remove|upgrade|deconfigure|failed-upgrade ...
#   rpm: %preun <number of versions left after this one> (0 erase, 1+ upgrade)
set -e

case "$1" in
    remove | 0) ;;
    *) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl stop nexo-media-worker.service >/dev/null 2>&1 || true
    systemctl disable nexo-media-worker.service >/dev/null 2>&1 || true
fi

exit 0
