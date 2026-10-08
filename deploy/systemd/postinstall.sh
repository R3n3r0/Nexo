#!/bin/sh
# deb/rpm post-install script (nfpm): reload systemd so the unit is visible.
# The service is not enabled automatically: edit /etc/nexo/nexo.env first.
set -e

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'
Nexo installed. Next steps:
  1. edit /etc/nexo/nexo.env (root credentials, NEXO_ENDPOINTS)
  2. create the data directory, e.g.: install -d -o nexo -g nexo -m 0750 /srv/nexo
  3. systemctl enable --now nexo
EOF

exit 0
