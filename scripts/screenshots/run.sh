#!/usr/bin/env bash
# Regenerates the screenshots of the README (docs/images/screenshots):
# starts a demo deployment of bin/nexo (one node, 4 drives) on loopback
# ports, seeds it with nexoctl (seed.sh), generates some minutes of traffic
# for the metric charts (traffic.sh), captures the console with Playwright
# (web/screenshots) and optimizes the PNG files (pngquant/oxipng when
# installed). Every process it starts is stopped by PID on exit.
#
# The media screenshots (player, media panel, media rules, image variants)
# need ffmpeg (the demo video), Pillow (the demo photos) and
# bin/nexo-media-worker with OpenH264 (renditions, poster, thumbnails, image
# variants); the player needs a browser that decodes H.264 (Google Chrome:
# the Chromium of Playwright does not). Without them they are skipped.
#
# Run through `make screenshots` (which builds bin/nexo, bin/nexoctl and
# bin/nexo-media-worker first).
#
# Environment:
#   SCREENSHOTS_WORKDIR          work directory (default: a new temporary one, removed on exit)
#   SCREENSHOTS_DIR              output directory (default docs/images/screenshots)
#   SCREENSHOTS_TRAFFIC_SECONDS  traffic before the capture (default 840: the 15 min charts are full)
#   SCREENSHOTS_PORT_BASE        first port (default 38000): S3 +0, console +1, web +80, webhook +90,
#                                media worker +91
#   SCREENSHOTS_SCALE            deviceScaleFactor of the browser (default 1)
#   NEXO_E2E_CHROMIUM            Chromium executable (optional, as for npm run e2e; default
#                                google-chrome when installed, for the H.264 of the player)
#   NEXO_WORKER_OPENH264         libopenh264 of the media worker (default: downloaded from Cisco
#                                into the work directory with nexo-media-worker fetch-openh264)
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
here="$repo/scripts/screenshots"
nexo="${NEXO:-$repo/bin/nexo}"
media_worker="${NEXO_MEDIA_WORKER:-$repo/bin/nexo-media-worker}"
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
worker_address="127.0.0.1:$((base + 91))"

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

for port in "$base" "$((base + 1))" "$((base + 80))" "$((base + 90))" "$((base + 91))"; do
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

# The media worker of the demo video and photos, with credentials of its own
# (seed.sh creates the user and the notify_media_worker target).
start_media_worker() {
  if [ ! -x "$media_worker" ]; then
    echo "screenshots: $media_worker not found (make build-media-worker), no media outputs" >&2
    return 1
  fi
  local lib="${NEXO_WORKER_OPENH264:-}"
  if [ -z "$lib" ] && ! lib="$("$media_worker" fetch-openh264 --dir "$work/media-worker/lib")"; then
    echo "screenshots: OpenH264 could not be downloaded (set NEXO_WORKER_OPENH264), no media outputs" >&2
    return 1
  fi
  export DEMO_MEDIA_WORKER_USER=media-worker
  DEMO_MEDIA_WORKER_SECRET="demo-secret-media-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  DEMO_MEDIA_WORKER_TOKEN="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  export DEMO_MEDIA_WORKER_SECRET DEMO_MEDIA_WORKER_TOKEN
  echo "screenshots: starting nexo-media-worker"
  NEXO_WORKER_S3_ENDPOINT="$DEMO_S3_URL" NEXO_WORKER_ACCESS_KEY="$DEMO_MEDIA_WORKER_USER" \
    NEXO_WORKER_SECRET_KEY="$DEMO_MEDIA_WORKER_SECRET" NEXO_WORKER_AUTH_TOKEN="$DEMO_MEDIA_WORKER_TOKEN" \
    NEXO_WORKER_OPENH264="$lib" NEXO_WORKER_NAME=demo-worker \
    "$media_worker" --address "$worker_address" --data-dir "$work/media-worker" >"$work/media-worker.log" 2>&1 &
  pids+=("$!")
  for _ in $(seq 1 100); do
    if curl -fs -o /dev/null "http://$worker_address/healthz"; then
      export DEMO_MEDIA_WORKER_URL="http://$worker_address/jobs"
      return 0
    fi
    kill -0 "${pids[-1]}" 2>/dev/null || break
    sleep 0.1
  done
  echo "screenshots: nexo-media-worker did not start, no media outputs:" >&2
  cat "$work/media-worker.log" >&2
  return 1
}
start_media_worker || true

"$here/seed.sh" "$work"
if [ -s "$work/player.url" ]; then
  SCREENSHOTS_PLAYER_URL="$(cat "$work/player.url")"
  export SCREENSHOTS_PLAYER_URL
fi
# The Chromium of Playwright has no H.264 decoder: the player needs Chrome.
if [ -z "${NEXO_E2E_CHROMIUM:-}" ]; then
  for chrome in google-chrome-stable google-chrome chrome; do
    if command -v "$chrome" >/dev/null; then
      NEXO_E2E_CHROMIUM="$(command -v "$chrome")"
      export NEXO_E2E_CHROMIUM
      break
    fi
  done
fi

# The traffic goes on during the capture: the latest values of the charts
# are not zero.
echo "screenshots: generating traffic for ${traffic_seconds}s"
"$here/traffic.sh" "$work" "$((traffic_seconds + 600))" >"$work/traffic.log" 2>&1 &
pids+=("$!")
sleep "$traffic_seconds"

echo "screenshots: capturing into $out"
(cd "$repo/web" && SCREENSHOTS_DIR="$out" npx playwright test -c screenshots/playwright.config.ts screenshots.spec.ts)

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
  if [ -f "$out/video-player.png" ]; then
    cp "$out/video-player.png" "$repo/docs/user/images/console/"
  fi
fi
du -ch "$out"/*.png | tail -n 1
