#!/bin/sh
# Static check of the systemd unit (make systemd-verify, CI job
# "packaging"): systemd-analyze verify and security on a scratch root that
# holds the unit, /etc/default/nexo and a nexo binary, so that the check
# needs neither root privileges nor an installed package.
#
#   packaging/systemd-verify.sh [NEXO_BINARY [WORKER_BINARY]]
#
# NEXO_BINARY defaults to bin/nexo, then to any "nexo" in PATH. The overall
# exposure reported by systemd-analyze security must stay below
# MAX_EXPOSURE (default 2.0, "OK"). The unit of the media worker
# (nexo-media-worker.service, /etc/default/nexo-media-worker) gets the same
# checks with WORKER_BINARY: by default the nexo-media-worker next to
# NEXO_BINARY's build (dist/nexo-media-worker_linux_amd64_v1 for a GoReleaser
# snapshot, bin/nexo-media-worker), skipped with a message when there is none.
#
# covers: S-OPS-12 (the units installed by the deb and rpm packages)
set -eu

root_dir=$(cd "$(dirname "$0")/.." && pwd)
bin=${1:-$root_dir/bin/nexo}
[ -x "$bin" ] || bin=$(command -v nexo || true)
if [ -z "$bin" ] || [ ! -x "$bin" ]; then
    echo "systemd-verify: no nexo binary (build it: make build-go)" >&2
    exit 1
fi
max=${MAX_EXPOSURE:-2.0}
worker=${2:-}
if [ -z "$worker" ]; then
    for w in "$(dirname "$bin")/../nexo-media-worker_linux_amd64_v1/nexo-media-worker" \
        "$(dirname "$bin")/nexo-media-worker" "$root_dir/bin/nexo-media-worker"; do
        if [ -x "$w" ]; then worker=$w; break; fi
    done
fi

r=$(mktemp -d)
trap 'rm -rf "$r"' EXIT
mkdir -p "$r/usr/bin" "$r/bin" "$r/etc/default" "$r/etc/systemd/system" "$r/usr/lib/systemd"
cp "$bin" "$r/usr/bin/nexo"
cp /bin/kill "$r/bin/kill"   # ExecReload
cp "$root_dir/deploy/systemd/nexo.env.example" "$r/etc/default/nexo"
cp "$root_dir/deploy/systemd/nexo.service" "$r/etc/systemd/system/nexo.service"
# The targets the unit refers to (multi-user.target, network-online.target, ...).
cp -a /usr/lib/systemd/system "$r/usr/lib/systemd/"

# check UNIT: systemd-analyze verify, and security below MAX_EXPOSURE.
check() {
    systemd-analyze verify --man=no --root="$r" "$r/etc/systemd/system/$1"
    out=$(systemd-analyze security --offline=true --root="$r" "$r/etc/systemd/system/$1" 2>&1 | tail -1)
    echo "$out"
    score=$(echo "$out" | sed -n 's/.*: \([0-9.]*\) .*/\1/p')
    [ -n "$score" ] || { echo "systemd-verify: cannot read the exposure level of $1" >&2; exit 1; }
    awk -v s="$score" -v m="$max" 'BEGIN { exit !(s <= m) }' || { echo "systemd-verify: $1: exposure $score above $max" >&2; exit 1; }
}
check nexo.service
if [ -n "$worker" ]; then
    cp "$worker" "$r/usr/bin/nexo-media-worker"
    cp "$root_dir/deploy/systemd/nexo-media-worker.env.example" "$r/etc/default/nexo-media-worker"
    cp "$root_dir/deploy/systemd/nexo-media-worker.service" "$r/etc/systemd/system/nexo-media-worker.service"
    check nexo-media-worker.service
else
    echo "systemd-verify: no nexo-media-worker binary (make build-media-worker): nexo-media-worker.service not checked"
fi
echo "systemd-verify: OK"
