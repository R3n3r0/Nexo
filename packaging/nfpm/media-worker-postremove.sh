#!/bin/sh
# Post-remove script of the nexo-media-worker deb/rpm package (nfpm,
# .goreleaser.yaml).
#
# Forgets the removed unit. The state directory (job queue, OpenH264) and,
# until a purge, /etc/default/nexo-media-worker are kept.
#
#   deb: postrm remove|purge|upgrade|...
#   rpm: %postun <number of versions left> (0 erase, 1+ upgrade)
set -e

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

case "$1" in
    purge)
        echo "nexo-media-worker purged: its state (/var/lib/private/nexo-media-worker) was kept; remove it by hand if unused."
        ;;
esac

exit 0
