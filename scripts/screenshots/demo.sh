#!/usr/bin/env bash
# Records the demo animation of the README (docs/images/demo.gif): starts
# the demo deployment of bin/nexo (one node, 4 drives) on loopback ports,
# seeds it like the screenshots (seed.sh) with a minute of traffic for the
# charts, records the console with Playwright (web/screenshots/demo.spec.ts:
# sign in, overview, a new bucket, an upload, a preview, a share link) and
# turns the video into an optimized GIF with ffmpeg (two-pass palette; the
# first second, the blank page before the sign-in form, is cut).
#
# Run through `make demo-gif` (which builds bin/nexo and bin/nexoctl first).
#
# Environment:
#   DEMO_GIF          output file (default docs/images/demo.gif)
#   DEMO_GIF_WIDTH    width of the GIF (default 960)
#   DEMO_GIF_FPS      frames per second of the GIF (default 12)
#   DEMO_PORT_BASE    first port (default 38200): S3 +0, console +1, web +80, webhook +90
#   NEXO_E2E_CHROMIUM Chromium executable (optional, as for npm run e2e)
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
here="$repo/scripts/screenshots"
nexo="${NEXO:-$repo/bin/nexo}"
gif="${DEMO_GIF:-$repo/docs/images/demo.gif}"
width="${DEMO_GIF_WIDTH:-960}"
fps="${DEMO_GIF_FPS:-12}"
base="${DEMO_PORT_BASE:-38200}"

command -v ffmpeg >/dev/null || { echo "demo: ffmpeg is required" >&2; exit 1; }

work="$(mktemp -d "${TMPDIR:-/tmp}/nexo-demo.XXXXXX")"
export DEMO_S3_URL="http://127.0.0.1:$base"
export DEMO_CONSOLE_URL="http://127.0.0.1:$((base + 1))"
export DEMO_WEB_URL="http://127.0.0.1:$((base + 80))"
export DEMO_WEBHOOK_URL="http://127.0.0.1:$((base + 90))/events"
export DEMO_ROOT_USER="${DEMO_ROOT_USER:-admin}"
export DEMO_ROOT_PASSWORD="${DEMO_ROOT_PASSWORD:-demo-password-2026}"

pids=()
stop() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do
    for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$pid" 2>/dev/null || true
  done
  rm -rf "$work"
}
trap stop EXIT

python3 "$here/webhook_sink.py" "$((base + 90))" &
pids+=("$!")

mkdir -p "$work/data"
NEXO_ROOT_USER="$DEMO_ROOT_USER" NEXO_ROOT_PASSWORD="$DEMO_ROOT_PASSWORD" \
  NEXO_KMS_SECRET_KEY="demo-key:$(printf '%032d' 0 | base64)" \
  "$nexo" server --log-format text \
  --address "127.0.0.1:$base" --console-address "127.0.0.1:$((base + 1))" \
  --web-address "127.0.0.1:$((base + 80))" --cache-memory 128MiB \
  "$work/data/disk{1...4}" >"$work/nexo.log" 2>&1 &
pids+=("$!")
for _ in $(seq 1 300); do
  curl -fs -o /dev/null "$DEMO_CONSOLE_URL/nexo/health/ready" && break
  kill -0 "${pids[1]}" 2>/dev/null || { cat "$work/nexo.log" >&2; exit 1; }
  sleep 0.1
done

"$here/seed.sh" "$work"
"$here/traffic.sh" "$work" 600 >"$work/traffic.log" 2>&1 &
pids+=("$!")
sleep 60

echo "demo: recording"
(cd "$repo/web" && DEMO_VIDEO_DIR="$work/video" DEMO_PHOTOS="$work/files/photos/albums/2026/alps-trip" \
  npx playwright test -c screenshots/playwright.config.ts demo.spec.ts)
video="$(find "$work/video" -name '*.webm' | head -1)"
[ -n "$video" ] || { echo "demo: no video recorded" >&2; exit 1; }

echo "demo: converting to $gif"
mkdir -p "$(dirname "$gif")"
filters="fps=$fps,scale=$width:-1:flags=lanczos"
ffmpeg -v error -y -ss 1.2 -i "$video" -vf "$filters,palettegen=stats_mode=diff" "$work/palette.png"
ffmpeg -v error -y -ss 1.2 -i "$video" -i "$work/palette.png" \
  -lavfi "$filters [x]; [x][1:v] paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
  -loop 0 "$gif"
du -h "$gif"
