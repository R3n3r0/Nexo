#!/bin/sh
# Post-install script of the nexo-media-worker deb/rpm package (nfpm,
# .goreleaser.yaml).
#
# Makes the unit visible to systemd. The service is never enabled nor
# restarted automatically: on a first install /etc/default/nexo-media-worker
# must be filled first (the worker refuses to start without its secret key
# and token); on an upgrade the operator restarts it (the queued jobs are
# kept).
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
    cat <<'EOT'
nexo-media-worker upgraded. The running worker keeps the previous version
until it is restarted: systemctl restart nexo-media-worker (its queued jobs
are kept).
EOT
elif [ "$mode" = install ]; then
    cat <<'EOT'
nexo-media-worker installed. Next steps (docs/user/media-worker.md, "Setup"):
  1. on Nexo: the policy and the user of the worker
     (nexoctl admin policy create / nexoctl admin user add)
  2. edit /etc/default/nexo-media-worker: NEXO_WORKER_S3_ENDPOINT,
     NEXO_WORKER_ACCESS_KEY, NEXO_WORKER_SECRET_KEY, NEXO_WORKER_AUTH_TOKEN
  3. systemctl enable --now nexo-media-worker (the first start downloads
     OpenH264 from Cisco)
  4. on Nexo: nexoctl admin config set ALIAS notify_media_worker enable=on
     endpoint=http://<this host>:9600/jobs auth_token=<the same token>
EOT
fi

exit 0
