#!/bin/sh
# deb/rpm pre-install script (nfpm): create the nexo system user and group
# before the package files, owned by it, are unpacked.
set -e

if ! getent group nexo >/dev/null 2>&1; then
    groupadd --system nexo
fi

if ! getent passwd nexo >/dev/null 2>&1; then
    useradd --system --gid nexo --home-dir /var/lib/nexo --no-create-home \
        --shell /usr/sbin/nologin --comment "Nexo object storage" nexo
fi

exit 0
