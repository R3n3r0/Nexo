#!/usr/bin/env bash
# Runs the same warp benchmarks against several S3 endpoints, one after the
# other, and prints a Markdown table to compare them: Nexo against MinIO or
# any other S3-compatible server, on your hardware.
#
#   scripts/bench/warp-compare.sh NAME=URL,ACCESS_KEY,SECRET_KEY...
#   scripts/bench/warp-compare.sh --local
#
# Examples:
#   scripts/bench/warp-compare.sh \
#     nexo=http://10.0.0.5:9000,admin,secret1 minio=http://10.0.0.6:9000,minioadmin,secret2
#   MINIO_BIN=/usr/local/bin/minio DRIVES=4 scripts/bench/warp-compare.sh --local
#
# With endpoints, the servers must already run, ideally on identical machines
# and drives; an https:// URL turns on TLS (--insecure skips its
# verification with WARP_INSECURE=1). Each benchmark uses its own bucket
# (warp-compare-<bench>) and removes its objects at the end.
#
# --local starts bin/nexo and, when MINIO_BIN (or minio in PATH) exists,
# MinIO, each in turn on DRIVES fresh directories under DATA_DIR on
# 127.0.0.1, so both run with the same layout on the same drives; a server is
# stopped before the next one starts. Client and server then share the CPU:
# fine for a first look, not for publishing numbers.
#
# Output, under OUT: the warp output of every run (<name>-<bench>.txt), its
# data (warp analyze <file>.json.zst) and results.md, the table.
#
# Environment (defaults in brackets):
#   BENCHES       ["put get mixed"]; also list
#   DURATION      of each benchmark [60s]
#   CONCURRENCY   operations in flight [16]
#   SIZE          objects of put and get [16MiB]; OBJECTS prepared for get [64]
#   MIXED_SIZE    largest object of mixed, sizes random up to it [1MiB];
#                 MIXED_OBJECTS [1000]
#   LIST_OBJECTS  objects of 1 KiB prepared for list [10000]
#   OUT           [bin/bench/compare]
#   WARP          warp binary [warp in PATH, else go install
#                 github.com/minio/warp@$WARP_VERSION into OUT/.bin]
#   WARP_VERSION  [v1.8.2]
#   --local only: NEXO_BIN [bin/nexo], MINIO_BIN [minio in PATH], DRIVES [4],
#                 DATA_DIR [a new temporary directory, removed at the end]
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BENCHES=${BENCHES:-put get mixed}
DURATION=${DURATION:-60s}
CONCURRENCY=${CONCURRENCY:-16}
SIZE=${SIZE:-16MiB}
OBJECTS=${OBJECTS:-64}
MIXED_SIZE=${MIXED_SIZE:-1MiB}
MIXED_OBJECTS=${MIXED_OBJECTS:-1000}
LIST_OBJECTS=${LIST_OBJECTS:-10000}
OUT=${OUT:-$ROOT/bin/bench/compare}
WARP_VERSION=${WARP_VERSION:-v1.8.2}

usage() {
	sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
}
[ $# -gt 0 ] || usage

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

WARP=${WARP:-$(command -v warp || true)}
if [ -z "$WARP" ]; then
	echo "warp-compare: installing github.com/minio/warp@$WARP_VERSION" >&2
	GOBIN="$OUT/.bin" go install "github.com/minio/warp@$WARP_VERSION"
	WARP="$OUT/.bin/warp"
fi

pid=""
created=""
# stop stops the local server, if one runs.
stop() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		pid=""
	fi
}
cleanup() {
	stop
	if [ -n "$created" ]; then
		rm -rf "$created"
	fi
}
trap cleanup EXIT INT TERM

# wait_ok URL waits for a health endpoint to answer 200.
wait_ok() {
	for _ in $(seq 1 300); do
		if curl -sf -o /dev/null "$1"; then
			return 0
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			break
		fi
		sleep 0.2
	done
	echo "warp-compare: $1 is not healthy, see $OUT/*.log" >&2
	exit 1
}

# drives DIR prints the drive arguments of the local servers: DIR/d1 or
# DIR/d{1...N} (the same ellipsis syntax for Nexo and MinIO).
drives() {
	local i
	for i in $(seq 1 "$DRIVES"); do
		mkdir -p "$1/d$i"
	done
	if [ "$DRIVES" = 1 ]; then
		echo "$1/d1"
	else
		echo "$1/d{1...$DRIVES}"
	fi
}

results=()

# bench NAME URL ACCESS_KEY SECRET_KEY runs every benchmark against one
# endpoint and appends its rows to results.
bench() {
	local name=$1 url=$2 access=$3 secret=$4 host tls=() b args summary avg reqs
	host=${url#*://}
	host=${host%%/*}
	case "$url" in
	https://*)
		tls=(--tls)
		if [ "${WARP_INSECURE:-0}" = 1 ]; then
			tls+=(--insecure)
		fi
		;;
	esac
	for b in $BENCHES; do
		case "$b" in
		put) args=(put --obj.size "$SIZE") ;;
		get) args=(get --obj.size "$SIZE" --objects "$OBJECTS") ;;
		mixed) args=(mixed --obj.size "$MIXED_SIZE" --obj.randsize --objects "$MIXED_OBJECTS") ;;
		list) args=(list --obj.size 1KiB --objects "$LIST_OBJECTS") ;;
		*)
			echo "warp-compare: unknown benchmark $b" >&2
			exit 2
			;;
		esac
		echo "== $name $b" >&2
		(cd "$OUT" && "$WARP" "${args[@]}" --host "$host" "${tls[@]}" \
			--access-key "$access" --secret-key "$secret" --bucket "warp-compare-$b" \
			--duration "$DURATION" --concurrent "$CONCURRENCY" --no-color \
			--benchdata "$name-$b") 2>&1 | tee "$OUT/$name-$b.txt" >&2
		# One report for put, get and list; mixed has one per operation type
		# and then their total (its request times are per type: see the file).
		reqs=""
		if [ "$b" = mixed ]; then
			summary=$(sed -n '/^Report: Total\./,$p' "$OUT/$name-$b.txt")
		else
			summary=$(sed -n '/^Report:/,$p' "$OUT/$name-$b.txt")
			reqs=$(grep -m1 '^ \* Reqs:' <<<"$summary" | sed -n 's/.*50%: *\([^,]*\),.*99%: *\([^,]*\),.*/\1 \/ \2/p')
		fi
		avg=$(grep -m1 '^ \* Average:' <<<"$summary" | sed 's/^ \* Average: *//')
		results+=("| $name | $b | ${avg:-see $name-$b.txt} | ${reqs:--} |")
	done
}

if [ "$1" = --local ]; then
	NEXO_BIN=${NEXO_BIN:-$ROOT/bin/nexo}
	MINIO_BIN=${MINIO_BIN:-$(command -v minio || true)}
	DRIVES=${DRIVES:-4}
	if [ ! -x "$NEXO_BIN" ]; then
		echo "warp-compare: $NEXO_BIN not found: run make build-go" >&2
		exit 1
	fi
	if [ -z "${DATA_DIR:-}" ]; then
		DATA_DIR=$(mktemp -d "${TMPDIR:-/tmp}/nexo-compare.XXXXXX")
		created=$DATA_DIR
	fi
	access=benchadmin
	secret=benchpassword123

	NEXO_ROOT_USER=$access NEXO_ROOT_PASSWORD=$secret "$NEXO_BIN" server --log-format text \
		--log-level warn --address 127.0.0.1:9600 --console-address 127.0.0.1:9601 \
		"$(drives "$DATA_DIR/nexo")" >"$OUT/nexo.log" 2>&1 &
	pid=$!
	wait_ok http://127.0.0.1:9600/nexo/health/ready
	bench nexo http://127.0.0.1:9600 "$access" "$secret"
	stop
	rm -rf "${DATA_DIR:?}/nexo"

	if [ -n "$MINIO_BIN" ]; then
		MINIO_ROOT_USER=$access MINIO_ROOT_PASSWORD=$secret "$MINIO_BIN" server --quiet \
			--address 127.0.0.1:9600 --console-address 127.0.0.1:9601 \
			"$(drives "$DATA_DIR/minio")" >"$OUT/minio.log" 2>&1 &
		pid=$!
		wait_ok http://127.0.0.1:9600/minio/health/ready
		bench minio http://127.0.0.1:9600 "$access" "$secret"
		stop
	else
		echo "warp-compare: no MinIO binary (MINIO_BIN), only Nexo was measured" >&2
	fi
else
	for target in "$@"; do
		name=${target%%=*}
		IFS=, read -r url access secret <<<"${target#*=}"
		if [ "$name" = "$target" ] || [ -z "$url" ] || [ -z "$access" ] || [ -z "$secret" ]; then
			echo "warp-compare: expected NAME=URL,ACCESS_KEY,SECRET_KEY, got $target" >&2
			exit 2
		fi
		bench "$name" "$url" "$access" "$secret"
	done
fi

{
	echo "Benchmarks: $BENCHES; duration $DURATION, concurrency $CONCURRENCY; put/get $SIZE" \
		"($OBJECTS objects for get), mixed up to $MIXED_SIZE ($MIXED_OBJECTS objects); warp $("$WARP" --version 2>/dev/null | awk '{print $3}')."
	echo
	echo "| Target | Benchmark | Throughput | Request time (median / p99) |"
	echo "|--------|-----------|------------|------------------------------|"
	printf '%s\n' "${results[@]}"
} >"$OUT/results.md"
echo
cat "$OUT/results.md"
