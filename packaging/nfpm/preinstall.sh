#!/bin/sh
# Pre-install script of the nexo deb/rpm package (nfpm, .goreleaser.yaml).
#
# Creates the "nexo" system user and group before the package files owned
# by them (/var/lib/nexo, /etc/default/nexo) are unpacked. Idempotent: it
# runs again on every upgrade.
#
#   deb: preinst install|upgrade [<old-version>]
#   rpm: %pre <number of versions installed after this one> (1 install, 2+ upgrade)
set -e

if ! getent group nexo >/dev/null 2>&1; then
    groupadd --system nexo
fi

if ! getent passwd nexo >/dev/null 2>&1; then
    nologin=/usr/sbin/nologin
    [ -x "$nologin" ] || nologin=/sbin/nologin
    useradd --system --gid nexo --home-dir /var/lib/nexo --no-create-home \
        --shell "$nologin" --comment "Nexo object storage" nexo
fi

exit 0
