#!/bin/sh
# Post-remove script of the nexo deb/rpm package (nfpm, .goreleaser.yaml).
#
# Forgets the removed unit. The data (/var/lib/nexo and any other drive),
# the "nexo" user and, until a purge, /etc/default/nexo are kept on
# purpose: object data is never deleted by the package manager.
#
#   deb: postrm remove|purge|upgrade|...
#   rpm: %postun <number of versions left> (0 erase, 1+ upgrade)
set -e

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

case "$1" in
    purge)
        echo "Nexo purged: the data in /var/lib/nexo and the nexo user were kept; remove them by hand if unused."
        ;;
esac

exit 0
