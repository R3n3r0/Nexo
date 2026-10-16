#!/usr/bin/env bash
# Seeds the demo deployment of the README screenshots with nexoctl: buckets,
# objects, versions, Object Lock, lifecycle, events, IAM, a share link, an
# access rule, two distributions with their cache, and some traffic so that
# the cache statistics and the metrics have data.
#
# Usage: seed.sh WORKDIR
# Environment: NEXOCTL (default bin/nexoctl), DEMO_S3_URL, DEMO_CONSOLE_URL,
# DEMO_WEB_URL, DEMO_ROOT_USER, DEMO_ROOT_PASSWORD, DEMO_WEBHOOK_URL.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="${1:?usage: seed.sh WORKDIR}"
nexoctl="${NEXOCTL:-$repo/bin/nexoctl}"
s3="${DEMO_S3_URL:-http://127.0.0.1:38000}"
console="${DEMO_CONSOLE_URL:-http://127.0.0.1:38001}"
web="${DEMO_WEB_URL:-http://127.0.0.1:38080}"
webhook="${DEMO_WEBHOOK_URL:-http://127.0.0.1:38090/events}"

export NEXOCTL_CONFIG_DIR="$work/nexoctl"
export NO_COLOR=1
nx() { "$nexoctl" -q "$@"; }

files="$work/files"
if [ ! -d "$files" ]; then
  python3 "$repo/scripts/screenshots/gen_files.py" "$files"
fi

nx alias set demo "$s3" "${DEMO_ROOT_USER:-admin}" "${DEMO_ROOT_PASSWORD:-demo-password-2026}" --admin-url "$console"

echo "seed: buckets and objects"
nx mb --with-versioning demo/photos
nx mb demo/website
nx mb --with-lock demo/backups
nx mb demo/logs
nx mb --with-versioning demo/analytics

nx tag set demo/photos "team=media&env=production"
nx tag set demo/website "team=web&env=production"
nx anonymous set download demo/website
nx tag set demo/backups "team=ops&compliance=soc2"

nx encrypt set sse-s3 demo/backups
nx encrypt set sse-s3 demo/analytics

nx cp --recursive "$files/photos/" demo/photos/
nx cp --recursive "$files/website/" demo/website/
nx cp --recursive --attr "source=pgbackrest" "$files/backups/" demo/backups/
nx cp --recursive "$files/logs/" demo/logs/
nx cp --recursive "$files/analytics/" demo/analytics/

# A few versions of the same objects.
for _ in 1 2 3; do
  nx cp "$files/photos/albums/2026/alps-trip/metadata.json" demo/photos/albums/2026/alps-trip/metadata.json
  nx cp "$files/analytics/reports/2026-09/summary.json" demo/analytics/reports/2026-09/summary.json
done
nx tag set demo/photos/albums/2026/alps-trip/summit-sunrise.jpg "rating=5&featured=true"

echo "seed: Object Lock"
nx retention set --default --mode governance --validity 30d demo/backups
nx retention set --mode compliance --validity 1y -r demo/backups/postgres/
nx legalhold set demo/backups/configs/etc-2026-10-15.tar.zst || true

echo "seed: lifecycle"
nx ilm rule add --id expire-app-logs --prefix web/ --expire-days 30 demo/logs
nx ilm rule add --id expire-worker-logs --prefix worker/ --expire-days 14 demo/logs
nx ilm rule add --id abort-uploads --abort-mpu-days 2 demo/logs
nx ilm rule add --id keep-3-versions --noncurrent-expire-days 30 --newer-noncurrent 3 demo/photos
nx ilm rule add --id cleanup-markers --expired-delete-marker --abort-mpu-days 7 demo/photos
nx ilm rule add --id old-raw --prefix raw/ --size-gt 5MiB --noncurrent-expire-days 90 demo/photos
nx ilm rule add --id wal-segments --prefix postgres/wal/ --noncurrent-expire-days 45 demo/backups

echo "seed: events"
nx admin config set demo notify_webhook:media enable=on endpoint="$webhook"
nx admin config set demo notify_webhook:audit enable=on endpoint="$webhook"
arn_media="arn:nexo:sqs:us-east-1:media:webhook"
arn_audit="arn:nexo:sqs:us-east-1:audit:webhook"
nx event add demo/photos "$arn_media" --event put --suffix .jpg
nx event add demo/photos "$arn_media" --event delete --prefix albums/
nx event add demo/backups "$arn_audit" --event put,delete

echo "seed: IAM"
cat >"$work/website-publish.json" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ListWebsite",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::website"]
    },
    {
      "Sid": "PublishWebsite",
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": ["arn:aws:s3:::website/*"]
    },
    {
      "Sid": "NeverTouchBackups",
      "Effect": "Deny",
      "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::backups", "arn:aws:s3:::backups/*"]
    }
  ]
}
JSON
cat >"$work/backups-writer.json" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:ListBucket", "s3:PutObjectRetention"],
      "Resource": ["arn:aws:s3:::backups", "arn:aws:s3:::backups/*"]
    }
  ]
}
JSON
cat >"$work/logs-reader.json" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:ListBucket"],
      "Resource": ["arn:aws:s3:::logs", "arn:aws:s3:::logs/*"],
      "Condition": { "IpAddress": { "aws:SourceIp": ["10.0.0.0/8", "192.168.0.0/16"] } }
    }
  ]
}
JSON
nx admin policy create demo website-publish "$work/website-publish.json"
nx admin policy create demo backups-writer "$work/backups-writer.json"
nx admin policy create demo logs-reader "$work/logs-reader.json"

nx admin user add demo web-deployer 'demo-secret-web-0001' --policy website-publish
nx admin user add demo backup-agent 'demo-secret-bkp-0001' --policy backups-writer
nx admin user add demo photo-editor 'demo-secret-pho-0001'
nx admin user add demo data-analyst 'demo-secret-ana-0001'
nx admin user add demo oncall-engineer 'demo-secret-ops-0001'
nx admin user add demo contractor 'demo-secret-ctr-0001' --disabled
nx admin group add demo editors photo-editor web-deployer --policy readwrite
nx admin group add demo operations oncall-engineer backup-agent --policy logs-reader,diagnostics
nx admin group add demo data-team data-analyst --policy readonly
nx admin accesskey create demo --user web-deployer --name ci-pipeline --description "Deploys the website from CI" >/dev/null

echo "seed: sharing and access rules"
nx share download --expire 7d demo/analytics/reports/2026-09/traffic-by-region.csv >/dev/null
nx share download --expire 2d demo/photos/albums/2026/alps-trip/summit-sunrise.jpg >/dev/null
nx access rule add demo/photos --id public-thumbnails --allow --principal '*' --action read \
  --regex '^thumbnails/.*\.(jpe?g|png)$' --ignore-case --description 'Thumbnails are public'
nx access rule add demo/photos --id editors-albums --allow --principal 'group:editors' --action all \
  --glob 'albums/**' --description 'Editors manage the albums'
nx access rule add demo/photos --id no-raw-outside-office --deny --principal '*' --action read \
  --prefix raw/ --not-source-ip 10.0.0.0/8 --priority 10 --description 'Raw files only from the office network'

echo "seed: distributions and cache"
nx cache settings demo --enable --policy standard --node-header
nx dist create demo website --bucket website --mode website \
  --domain www.example.com --domain example.com --index index.html \
  --cache-control 'public, max-age=300' --comment 'Company website'
nx dist cache set demo website --enable --policy standard
nx dist cache set demo website --behavior '/assets/**' --ttl-default 365d
nx dist create demo media --bucket photos --prefix /thumbnails --mode website \
  --domain img.example.org --domain cdn.example.org --comment 'Photo thumbnails'
nx dist cache set demo media --enable --policy standard

echo "seed: traffic"
paths=(/ /about/ /blog/ /blog/packing-list-for-the-alps/ /blog/choosing-a-tent/ /assets/css/site.css
  /assets/js/app.js /assets/js/vendor.js /assets/img/logo.svg /assets/img/hero.jpg /assets/img/team.jpg
  /robots.txt /sitemap.xml /missing-page)
for round in $(seq 1 40); do
  for p in "${paths[@]}"; do
    host=www.example.com
    if [ $((round % 4)) -eq 0 ]; then host=example.com; fi
    curl -s -o /dev/null -H "Host: $host" "$web$p" &
  done
  for t in glacier-lake summit-sunrise old-bridge lighthouse; do
    curl -s -o /dev/null -H "Host: img.example.org" "$web/$t-320.jpg" &
  done
  wait
done
# S3 traffic for the request and throughput charts.
for round in $(seq 1 6); do
  nx cat demo/analytics/reports/2026-09/traffic-by-region.csv >/dev/null
  nx ls --recursive demo/photos >/dev/null
  nx cp "$files/backups/postgres/orders-db-2026-10-15.sql.gz" "demo/logs/tmp/upload-$round.bin"
  nx cp "demo/logs/tmp/upload-$round.bin" "$work/download.bin"
  nx rm "demo/logs/tmp/upload-$round.bin"
done
echo "seed: done"
