#!/bin/sh
# Static check of the systemd unit (make systemd-verify, CI job
# "packaging"): systemd-analyze verify and security on a scratch root that
# holds the unit, /etc/default/nexo and a nexo binary, so that the check
# needs neither root privileges nor an installed package.
#
#   packaging/systemd-verify.sh [NEXO_BINARY]
#
# NEXO_BINARY defaults to bin/nexo, then to any "nexo" in PATH. The overall
# exposure reported by systemd-analyze security must stay below
# MAX_EXPOSURE (default 2.0, "OK").
set -eu

root_dir=$(cd "$(dirname "$0")/.." && pwd)
bin=${1:-$root_dir/bin/nexo}
[ -x "$bin" ] || bin=$(command -v nexo || true)
[ -n "$bin" ] && [ -x "$bin" ] || { echo "systemd-verify: no nexo binary (build it: make build-go)" >&2; exit 1; }
max=${MAX_EXPOSURE:-2.0}

r=$(mktemp -d)
trap 'rm -rf "$r"' EXIT
mkdir -p "$r/usr/bin" "$r/bin" "$r/etc/default" "$r/etc/systemd/system" "$r/usr/lib/systemd"
cp "$bin" "$r/usr/bin/nexo"
cp /bin/kill "$r/bin/kill"   # ExecReload
cp "$root_dir/deploy/systemd/nexo.env.example" "$r/etc/default/nexo"
cp "$root_dir/deploy/systemd/nexo.service" "$r/etc/systemd/system/nexo.service"
# The targets the unit refers to (multi-user.target, network-online.target, ...).
cp -a /usr/lib/systemd/system "$r/usr/lib/systemd/"

systemd-analyze verify --man=no --root="$r" "$r/etc/systemd/system/nexo.service"
out=$(systemd-analyze security --offline=true --root="$r" "$r/etc/systemd/system/nexo.service" 2>&1 | tail -1)
echo "$out"
score=$(echo "$out" | sed -n 's/.*: \([0-9.]*\) .*/\1/p')
[ -n "$score" ] || { echo "systemd-verify: cannot read the exposure level" >&2; exit 1; }
awk -v s="$score" -v m="$max" 'BEGIN { exit !(s <= m) }' || { echo "systemd-verify: exposure $score above $max" >&2; exit 1; }
echo "systemd-verify: OK"
