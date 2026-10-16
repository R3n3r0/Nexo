#!/bin/sh
# Authenticated OWASP ZAP active scan of the Admin API (make zap-scan,
# docs/security/pentest-2026-10.md §6.2): starts bin/nexo (SNSD, a temporary
# drive, loopback ports), logs in as root, creates fixtures named like the
# values ZAP fills the parameters with, imports api/openapi.yaml into ZAP
# and runs the active scan of /api/v1 with the console session (cookie,
# CSRF header and console Origin added to every request), then writes the
# reports (JSON, Markdown, HTML) and prints the alerts by risk.
#
#   make build && test/security/zap-api-scan.sh
#
# Environment:
#   NEXO_BIN         server binary (default bin/nexo, built by make build)
#   ZAP_HOME         an unpacked ZAP 2.17+ (default: ZAP_VERSION is downloaded,
#                    checksum-verified, into ZAP_CACHE)
#   ZAP_CACHE        download directory (default ${TMPDIR:-/tmp}/nexo-zap)
#   ZAP_PORT         first of the two loopback ports of nexo (S3, console)
#                    (default 37000; ZAP's own proxy port is ZAP_PORT+10)
#   ZAP_MINUTES      bound of the active scan in minutes (default 60)
#   ZAP_STRENGTH     attack strength of the active rules: low, medium (default),
#                    high or insane
#   ZAP_THRESHOLD    alert threshold of the rules: low, medium (default) or high
#   ZAP_REPORT_DIR   output directory (default ${TMPDIR:-/tmp}/nexo-zap-report)
#   ZAP_FAIL_ON      fail when an alert of this risk or higher remains:
#                    high (default), medium, low, informational or none
#
# The run of the report used ZAP_STRENGTH=high ZAP_THRESHOLD=low
# ZAP_FAIL_ON=none: at that strength ZAP raises the false positives triaged
# in the report (SQL injection on /routing/resolve, path traversal on new
# access key names and on a base64 cursor), which the default gate would fail.
#
# Needs Java 17+ (ZAP 2.17), curl, unzip and sha256sum. The server is
# throwaway: the scan creates, changes and deletes everything it can reach.
# The routes that would end the scan are excluded: logout (the session),
# service restart/stop, the endless streams (trace, logs), the CPU profile,
# the speed test, and the imports of a whole configuration or cluster state.
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NEXO_BIN=${NEXO_BIN:-$ROOT/bin/nexo}
ZAP_VERSION=2.17.0
# SHA-256 of ZAP_2.17.0_Crossplatform.zip (GitHub release v2.17.0).
ZAP_SHA256=94c8f767b1c2e94f0db66b3ae56514d5e3f5a728ee1b6c798e0c8fe2d61fbff0
ZAP_CACHE=${ZAP_CACHE:-${TMPDIR:-/tmp}/nexo-zap}
ZAP_PORT=${ZAP_PORT:-37000}
ZAP_MINUTES=${ZAP_MINUTES:-60}
ZAP_STRENGTH=${ZAP_STRENGTH:-medium}
ZAP_THRESHOLD=${ZAP_THRESHOLD:-medium}
ZAP_REPORT_DIR=${ZAP_REPORT_DIR:-${TMPDIR:-/tmp}/nexo-zap-report}
ZAP_FAIL_ON=${ZAP_FAIL_ON:-high}

S3_ADDR=127.0.0.1:$ZAP_PORT
CONSOLE_ADDR=127.0.0.1:$((ZAP_PORT + 1))
ZAP_PROXY_PORT=$((ZAP_PORT + 10))
API=http://$CONSOLE_ADDR/api/v1
ROOT_USER=zapadmin
ROOT_PASSWORD=zap-root-secret-$$

die() {
    echo "zap-api-scan: $*" >&2
    exit 1
}

[ -x "$NEXO_BIN" ] || die "$NEXO_BIN not found: run make build first"
command -v java >/dev/null || die "java not found (ZAP $ZAP_VERSION needs Java 17+)"
for tool in curl unzip sha256sum; do
    command -v "$tool" >/dev/null || die "$tool not found"
done

# --- ZAP ---------------------------------------------------------------------
if [ -z "${ZAP_HOME:-}" ]; then
    ZAP_HOME=$ZAP_CACHE/ZAP_$ZAP_VERSION
    if [ ! -x "$ZAP_HOME/zap.sh" ]; then
        mkdir -p "$ZAP_CACHE"
        zip=$ZAP_CACHE/ZAP_${ZAP_VERSION}_Crossplatform.zip
        echo "zap-api-scan: downloading ZAP $ZAP_VERSION"
        curl -fsSL -o "$zip" "https://github.com/zaproxy/zaproxy/releases/download/v$ZAP_VERSION/ZAP_${ZAP_VERSION}_Crossplatform.zip"
        echo "$ZAP_SHA256  $zip" | sha256sum -c - >/dev/null || die "checksum mismatch for $zip"
        unzip -q -o "$zip" -d "$ZAP_CACHE"
        rm -f "$zip"
    fi
fi
[ -x "$ZAP_HOME/zap.sh" ] || die "no zap.sh in $ZAP_HOME"

work=$(mktemp -d)
nexo_pid=
cleanup() {
    if [ -n "$nexo_pid" ]; then
        kill "$nexo_pid" 2>/dev/null || true
        wait "$nexo_pid" 2>/dev/null || true
    fi
    rm -rf "$work"
}
trap cleanup EXIT INT TERM
mkdir -p "$ZAP_REPORT_DIR" "$work/data" "$work/zap"

# --- nexo --------------------------------------------------------------------
# The log is filtered on the fly (the scan sends millions of requests): the
# audit records of the Admin API are reduced to "status<TAB>access key<TAB>
# route" in audit.tsv, and nexo.log keeps the errors, the warnings but the
# expected 501 of the features of other deployment modes, and anything that
# is not JSON (a panic).
mkfifo "$work/log"
awk -v audit="$ZAP_REPORT_DIR/audit.tsv" '
/"msg":"audit"/ {
    if ($0 !~ /"type":"admin"/) next
    api = st = ak = ""
    if (match($0, /"api":"[^"]*"/)) api = substr($0, RSTART + 7, RLENGTH - 8)
    if (match($0, /"status_code":[0-9]+/)) st = substr($0, RSTART + 14, RLENGTH - 14)
    if (match($0, /"access_key":"[^"]*"/)) ak = substr($0, RSTART + 14, RLENGTH - 15)
    print st "\t" ak "\t" api > audit
    next
}
!/^\{/ { print; next }
/"level":"(ERROR|WARN)"/ && !/"status":501/ { print }
' <"$work/log" >"$ZAP_REPORT_DIR/nexo.log" &
awk_pid=$!
NEXO_ROOT_USER=$ROOT_USER NEXO_ROOT_PASSWORD=$ROOT_PASSWORD \
    "$NEXO_BIN" server --address "$S3_ADDR" --console-address "$CONSOLE_ADDR" \
    --log-format json --log-level info "$work/data" >"$work/log" 2>&1 &
nexo_pid=$!
i=0
until curl -fsS -o /dev/null "http://$CONSOLE_ADDR/nexo/health/live" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || die "nexo did not start (see $ZAP_REPORT_DIR/nexo.log)"
    kill -0 "$nexo_pid" 2>/dev/null || die "nexo exited (see $ZAP_REPORT_DIR/nexo.log)"
    sleep 1
done

login=$(curl -fsS -c "$work/cookies" -H 'Content-Type: application/json' \
    -d "{\"accessKey\":\"$ROOT_USER\",\"secretKey\":\"$ROOT_PASSWORD\"}" "$API/auth/login")
csrf=$(printf '%s' "$login" | sed -n 's/.*"csrfToken":"\([^"]*\)".*/\1/p')
session=$(awk '$6 == "nexo_session" { print $7 }' "$work/cookies")
if [ -z "$csrf" ] || [ -z "$session" ]; then
    die "login failed: $login"
fi

# api METHOD PATH [BODY]: an Admin API call as root; failures are ignored
# (fixtures that already exist).
api() {
    curl -sS -o /dev/null -X "$1" -b "nexo_session=$session" -H "X-CSRF-Token: $csrf" \
        -H 'Sec-Fetch-Site: same-origin' -H 'Content-Type: application/json' \
        ${3:+--data "$3"} "$API$2" || true
}

# Fixtures named like the values ZAP gives the parameters: the name of the
# parameter for the path parameters ({bucket} is "bucket", see the copy of
# the spec below), the formhandler fields for the query parameters and the
# bodies, so that the generated requests reach existing resources.
api POST /buckets '{"name":"bucket","versioning":true}'
curl -sS -o /dev/null -X POST -b "nexo_session=$session" -H "X-CSRF-Token: $csrf" -H 'Sec-Fetch-Site: same-origin' \
    -H 'Content-Type: text/plain' --data-binary "@$ROOT/README.md" "$API/buckets/bucket/objects/upload?key=key" || true
api POST /policies '{"name":"policy","document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::bucket/*"]}]}}'
api POST /users '{"accessKey":"user","secretKey":"user-secret-1234","policyNames":["policy"]}'
api POST /groups '{"name":"group","members":["user"]}'
api POST /access-keys '{"accessKey":"key","secretKey":"key-secret-12345","parentUser":"user"}'

# --- ZAP plan ----------------------------------------------------------------
# The importer of ZAP leaves the path parameters declared on a path item (not
# on its operations) as literal "{bucket}": the copy of the spec it imports
# names the fixtures in the paths instead.
sed -e '/^  \//{
s/{bucket}/bucket/g
s/{user}/user/g
s/{group}/group/g
s/{policy}/policy/g
s/{key}/key/g
s/{subsys}/api/g
}' "$ROOT/api/openapi.yaml" >"$work/openapi.yaml"

cat >"$work/plan.yaml" <<EOF
env:
  contexts:
    - name: nexo-admin-api
      urls: ["$API/"]
      includePaths: ["$API/.*"]
      excludePaths:
        - "$API/auth/logout.*"
        - "$API/service/(restart|stop).*"
        - "$API/(trace|logs).*"
        - "$API/profile.*"
        - "$API/speedtest/.*"
        - "$API/(config|cluster)/import.*"
  parameters:
    failOnError: true
    failOnWarning: false
    progressToStdout: true
jobs:
  - type: replacer
    parameters:
      deleteAllRules: true
    rules:
      - description: console session
        matchType: req_header
        matchString: Cookie
        replacementString: "nexo_session=$session"
      - description: CSRF token
        matchType: req_header
        matchString: X-CSRF-Token
        replacementString: "$csrf"
      - description: console origin
        matchType: req_header
        matchString: Origin
        replacementString: "http://$CONSOLE_ADDR"
  - type: openapi
    parameters:
      apiFile: "$work/openapi.yaml"
      targetUrl: "$API"
      context: nexo-admin-api
  # The import sends every operation once, DELETE included: the fixtures
  # are created again before the active scan.
  - type: requestor
    requests:
      - url: "$API/buckets"
        method: POST
        httpVersion: HTTP/1.1
        headers: ["Content-Type: application/json"]
        data: '{"name":"bucket","versioning":true}'
      - url: "$API/buckets/bucket/objects/upload?key=key"
        method: POST
        httpVersion: HTTP/1.1
        headers: ["Content-Type: text/plain"]
        data: "fixture"
      - url: "$API/policies"
        method: POST
        httpVersion: HTTP/1.1
        headers: ["Content-Type: application/json"]
        data: '{"name":"policy","document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::bucket/*"]}]}}'
      - url: "$API/users"
        method: POST
        httpVersion: HTTP/1.1
        headers: ["Content-Type: application/json"]
        data: '{"accessKey":"user","secretKey":"user-secret-1234","policyNames":["policy"]}'
      - url: "$API/groups"
        method: POST
        httpVersion: HTTP/1.1
        headers: ["Content-Type: application/json"]
        data: '{"name":"group","members":["user"]}'
      - url: "$API/access-keys"
        method: POST
        httpVersion: HTTP/1.1
        headers: ["Content-Type: application/json"]
        data: '{"accessKey":"key","secretKey":"key-secret-12345","parentUser":"user"}'
  - type: passiveScan-wait
    parameters:
      maxDuration: 10
  - type: activeScan
    parameters:
      context: nexo-admin-api
      maxScanDurationInMins: $ZAP_MINUTES
      maxRuleDurationInMins: 5
      threadPerHost: 4
      addQueryParam: false
      handleAntiCSRFTokens: false
    policyDefinition:
      defaultStrength: $ZAP_STRENGTH
      defaultThreshold: $ZAP_THRESHOLD
  - type: passiveScan-wait
    parameters:
      maxDuration: 10
  - type: report
    parameters:
      template: traditional-json
      reportDir: "$ZAP_REPORT_DIR"
      reportFile: zap-report.json
  - type: report
    parameters:
      template: traditional-md
      reportDir: "$ZAP_REPORT_DIR"
      reportFile: zap-report.md
  - type: report
    parameters:
      template: traditional-html
      reportDir: "$ZAP_REPORT_DIR"
      reportFile: zap-report.html
EOF

# Values of the generated parameters and bodies (formhandler add-on).
n=0
set --
for field in bucket=bucket key=key user=user group=group policy=policy \
    accessKey=key parentUser=user secretKey=zap-secret-12345 prefix=zap/; do
    set -- "$@" -config "formhandler.fields.field($n).fieldId=${field%%=*}" \
        -config "formhandler.fields.field($n).value=${field#*=}" \
        -config "formhandler.fields.field($n).enabled=true"
    n=$((n + 1))
done

echo "zap-api-scan: scanning $API (at most $ZAP_MINUTES minutes, strength $ZAP_STRENGTH, threshold $ZAP_THRESHOLD)"
"$ZAP_HOME/zap.sh" -cmd -dir "$work/zap" -port "$ZAP_PROXY_PORT" \
    -config check.onstart=false -config database.recoverylog=false -config start.checkForUpdates=false \
    -config start.dayLastChecked=today -config callhome.telemetry.enabled=false \
    "$@" -autorun "$work/plan.yaml" >"$ZAP_REPORT_DIR/zap.log" 2>&1 || {
    tail -40 "$ZAP_REPORT_DIR/zap.log" >&2
    cp "$work/zap/zap.log" "$ZAP_REPORT_DIR/zap-internal.log" 2>/dev/null || true
    die "ZAP failed (see $ZAP_REPORT_DIR/zap.log)"
}
# The log of ZAP itself (its errors and warnings are counted in the report).
cp "$work/zap/zap.log" "$ZAP_REPORT_DIR/zap-internal.log" 2>/dev/null || true

# The session must have survived the scan, or its second half ran
# unauthenticated.
status=$(curl -sS -o /dev/null -w '%{http_code}' -b "nexo_session=$session" "$API/auth/session")
[ "$status" = 200 ] || echo "zap-api-scan: warning: the session ended during the scan (GET /auth/session: $status)" >&2

# Stop the server: the filter flushes the audit records.
kill "$nexo_pid"
wait "$nexo_pid" 2>/dev/null || true
nexo_pid=
wait "$awk_pid" 2>/dev/null || true

# Coverage, from the audit log of the server: how many requests reached the
# Admin API with the session, and their status codes. A 500 is a finding.
audit=$ZAP_REPORT_DIR/audit.tsv
echo "zap-api-scan: $(wc -l <"$audit") Admin API requests, $(awk -F '\t' -v u="$ROOT_USER" '$2 == u' "$audit" | wc -l) with the root session; status codes:"
cut -f1 "$audit" | sort | uniq -c | sed 's/^/    /'
if awk -F '\t' '$1 == 500 { found = 1 } END { exit !found }' "$audit"; then
    echo "zap-api-scan: internal errors (500) on:" >&2
    awk -F '\t' '$1 == 500 { print $3 }' "$audit" | sort | uniq -c | sed 's/^/    /' >&2
fi

report=$ZAP_REPORT_DIR/zap-report.json
[ -s "$report" ] || die "no report in $report"
summary() {
    # riskcode 3 high, 2 medium, 1 low, 0 informational
    grep -o '"riskcode": *"[0-3]"' "$report" | grep -c "\"$1\"" || true
}
high=$(summary 3) medium=$(summary 2) low=$(summary 1) info=$(summary 0)
echo "zap-api-scan: alerts high=$high medium=$medium low=$low informational=$info"
echo "zap-api-scan: reports in $ZAP_REPORT_DIR (zap-report.json, .md, .html)"
case $ZAP_FAIL_ON in
high) limit=$high ;;
medium) limit=$((high + medium)) ;;
low) limit=$((high + medium + low)) ;;
informational) limit=$((high + medium + low + info)) ;;
none) limit=0 ;;
*) die "ZAP_FAIL_ON must be high, medium, low, informational or none" ;;
esac
[ "$limit" -eq 0 ] || die "$limit alerts at or above $ZAP_FAIL_ON"
