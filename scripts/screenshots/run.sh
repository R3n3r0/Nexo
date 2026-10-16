#!/usr/bin/env bash
# Regenerates the screenshots of the README (docs/images/screenshots):
# starts a demo deployment of bin/nexo (one node, 4 drives) on loopback
# ports, seeds it with nexoctl (seed.sh), generates some minutes of traffic
# for the metric charts (traffic.sh), captures the console with Playwright
# (web/screenshots) and optimizes the PNG files (pngquant/oxipng when
# installed). Every process it starts is stopped by PID on exit.
#
# Run through `make screenshots` (which builds bin/nexo and bin/nexoctl first).
#
# Environment:
#   SCREENSHOTS_WORKDIR          work directory (default: a new temporary one, removed on exit)
#   SCREENSHOTS_DIR              output directory (default docs/images/screenshots)
#   SCREENSHOTS_TRAFFIC_SECONDS  traffic before the capture (default 840: the 15 min charts are full)
#   SCREENSHOTS_PORT_BASE        first port (default 38000): S3 +0, console +1, web +80, webhook +90
#   SCREENSHOTS_SCALE            deviceScaleFactor of the browser (default 1)
#   NEXO_E2E_CHROMIUM            Chromium executable (optional, as for npm run e2e)
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
here="$repo/scripts/screenshots"
nexo="${NEXO:-$repo/bin/nexo}"
out="${SCREENSHOTS_DIR:-$repo/docs/images/screenshots}"
traffic_seconds="${SCREENSHOTS_TRAFFIC_SECONDS:-840}"
base="${SCREENSHOTS_PORT_BASE:-38000}"

if [ -n "${SCREENSHOTS_WORKDIR:-}" ]; then
  work="$SCREENSHOTS_WORKDIR"
  cleanup_work=0
else
  work="$(mktemp -d "${TMPDIR:-/tmp}/nexo-screenshots.XXXXXX")"
  cleanup_work=1
fi
mkdir -p "$work" "$out"
work="$(cd "$work" && pwd)"

export DEMO_S3_URL="http://127.0.0.1:$base"
export DEMO_CONSOLE_URL="http://127.0.0.1:$((base + 1))"
export DEMO_WEB_URL="http://127.0.0.1:$((base + 80))"
export DEMO_WEBHOOK_URL="http://127.0.0.1:$((base + 90))/events"
export DEMO_ROOT_USER="${DEMO_ROOT_USER:-admin}"
export DEMO_ROOT_PASSWORD="${DEMO_ROOT_PASSWORD:-demo-password-2026}"

pids=()
stop() {
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    for _ in $(seq 1 100); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$pid" 2>/dev/null || true
  done
  if [ "$cleanup_work" = 1 ]; then
    rm -rf "$work"
  fi
}
trap stop EXIT

for port in "$base" "$((base + 1))" "$((base + 80))" "$((base + 90))"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "screenshots: port $port is in use (set SCREENSHOTS_PORT_BASE)" >&2
    exit 1
  fi
done

python3 "$here/webhook_sink.py" "$((base + 90))" &
pids+=("$!")

echo "screenshots: starting nexo in $work"
mkdir -p "$work/data"
NEXO_ROOT_USER="$DEMO_ROOT_USER" NEXO_ROOT_PASSWORD="$DEMO_ROOT_PASSWORD" \
  NEXO_KMS_SECRET_KEY="demo-key:$(printf '%032d' 0 | base64)" \
  "$nexo" server --log-format text \
  --address "127.0.0.1:$base" --console-address "127.0.0.1:$((base + 1))" \
  --web-address "127.0.0.1:$((base + 80))" --cache-memory 128MiB \
  "$work/data/disk{1...4}" >"$work/nexo.log" 2>&1 &
pids+=("$!")

for _ in $(seq 1 300); do
  if curl -fs -o /dev/null "$DEMO_CONSOLE_URL/nexo/health/ready"; then break; fi
  kill -0 "${pids[1]}" 2>/dev/null || { cat "$work/nexo.log" >&2; exit 1; }
  sleep 0.1
done
curl -fs -o /dev/null "$DEMO_CONSOLE_URL/nexo/health/ready"

"$here/seed.sh" "$work"

# The traffic goes on during the capture: the latest values of the charts
# are not zero.
echo "screenshots: generating traffic for ${traffic_seconds}s"
"$here/traffic.sh" "$work" "$((traffic_seconds + 600))" >"$work/traffic.log" 2>&1 &
pids+=("$!")
sleep "$traffic_seconds"

echo "screenshots: capturing into $out"
(cd "$repo/web" && SCREENSHOTS_DIR="$out" npx playwright test -c screenshots/playwright.config.ts)

if command -v pngquant >/dev/null; then
  pngquant --force --skip-if-larger --strip --quality 70-92 --speed 1 --ext .png "$out"/*.png || true
else
  echo "screenshots: pngquant not found, the PNG files are not quantized" >&2
fi
if command -v oxipng >/dev/null; then
  oxipng -q -o 4 --strip safe "$out"/*.png
fi
# A few of them illustrate the user documentation (MkDocs serves only docs/user).
if [ "$out" = "$repo/docs/images/screenshots" ]; then
  mkdir -p "$repo/docs/user/images/console"
  cp "$out/overview.png" "$out/object-browser.png" "$out/monitoring.png" "$repo/docs/user/images/console/"
fi
du -ch "$out"/*.png | tail -n 1
