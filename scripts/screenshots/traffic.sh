#!/usr/bin/env bash
# Generates varied traffic against the demo deployment for DURATION seconds so
# that the metric charts of the screenshots have a believable shape: anonymous
# S3 reads (the public website bucket and the public thumbnails), some 404/403,
# signed uploads and downloads with nexoctl, and requests to the distributions.
#
# Usage: traffic.sh WORKDIR [DURATION]
set -uo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="${1:?usage: traffic.sh WORKDIR [DURATION]}"
duration="${2:-300}"
nexoctl="${NEXOCTL:-$repo/bin/nexoctl}"
s3="${DEMO_S3_URL:-http://127.0.0.1:38000}"
web="${DEMO_WEB_URL:-http://127.0.0.1:38080}"
export NEXOCTL_CONFIG_DIR="$work/nexoctl"
export NO_COLOR=1
files="$work/files"

s3_paths=(/website/index.html /website/about/index.html /website/assets/css/site.css /website/assets/js/app.js
  /website/assets/img/hero.jpg /photos/thumbnails/glacier-lake-320.jpg /photos/thumbnails/lighthouse-320.jpg
  /website/blog/index.html /website/missing.html /backups/manifest.json)
web_paths=(/ /about/ /blog/ /assets/css/site.css /assets/js/app.js /assets/img/hero.jpg /assets/img/logo.svg /nope)
hosts=(www.example.com www.example.com example.com)

start=$(date +%s)
i=0
while [ $(($(date +%s) - start)) -lt "$duration" ]; do
  t=$(($(date +%s) - start))
  # A slow wave between 4 and 24 requests per round, with some jitter.
  n=$(awk -v t="$t" -v r="$RANDOM" 'BEGIN { printf "%d", 14 + 10 * sin(t / 45.0) + (r % 5) }')
  for _ in $(seq 1 "$n"); do
    curl -s -o /dev/null "$s3${s3_paths[RANDOM % ${#s3_paths[@]}]}" &
    curl -s -o /dev/null -H "Host: ${hosts[RANDOM % ${#hosts[@]}]}" "$web${web_paths[RANDOM % ${#web_paths[@]}]}" &
  done
  if [ $((i % 3)) -eq 0 ]; then
    obj="demo/logs/ingest/batch-$((i % 7)).bin"
    "$nexoctl" -q cp "$files/backups/postgres/wal/000000010000000400000010.gz" "$obj" >/dev/null 2>&1 &
  fi
  if [ $((i % 4)) -eq 1 ]; then
    "$nexoctl" -q cat demo/photos/raw/DSC_0412.dng >/dev/null 2>&1 &
  fi
  if [ $((i % 5)) -eq 2 ]; then
    "$nexoctl" -q ls --recursive demo/logs >/dev/null 2>&1 &
  fi
  wait
  i=$((i + 1))
  sleep 1
done
"$nexoctl" -q rm --recursive --force demo/logs/ingest/ >/dev/null 2>&1 || true
