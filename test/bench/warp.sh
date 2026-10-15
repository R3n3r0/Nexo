#!/usr/bin/env bash
# Runs the warp benchmarks of docs/bench (TP-01..TP-04 of docs/06 §6.6)
# against bin/nexo in one deployment mode, on this machine:
#
#   test/bench/warp.sh snsd      one drive
#   test/bench/warp.sh snmd      four drives (erasure coding EC:2)
#   test/bench/warp.sh cluster   four processes x two drives on 127.0.0.1
#
# Every run starts fresh servers on empty drives, runs the benchmarks in
# BENCHES (put, get, mixed, list) and writes, under OUT, the warp output of
# each benchmark (<bench>.txt), its data (warp-*.json.zst, see "warp
# analyze") and summary.txt. With PROFILE=1 a CPU profile of the first node
# is taken through the Admin API (POST /api/v1/profile) in the middle of
# every benchmark: OUT/<bench>.cpu.pb.gz, read with "go tool pprof".
#
# Environment (defaults in brackets):
#   NEXO_BIN      server binary [bin/nexo]
#   WARP          warp binary [warp in PATH, else go install
#                 github.com/minio/warp@$WARP_VERSION into OUT/.bin]
#   WARP_VERSION  [v1.8.2]
#   DATA_DIR      parent of the drives [a new temporary directory, removed
#                 at the end; BENCH_TMPFS=1 puts it on /dev/shm]
#   OUT           [bin/bench/<mode>]
#   DURATION      of each benchmark [20s]; PUT_DURATION, GET_DURATION,
#                 MIXED_DURATION, LIST_DURATION override it per benchmark
#                 (put keeps every object it writes until it ends: bound its
#                 duration by the free space of the drives)
#   CONCURRENCY   operations in flight [16]
#   LARGE_SIZE    objects of put and get [64MiB]; LARGE_OBJECTS objects
#                 prepared for get [32]
#   MIXED_SIZE    largest object of mixed, sizes are random up to it
#                 [1MiB]; MIXED_OBJECTS [1000]
#   LIST_OBJECTS  objects prepared for list (1 KiB each) [10000]
#   BENCHES       ["put get mixed list"]
#   PROFILE       1 profiles the first node during each benchmark [0]:
#                 PROFILE_SECONDS [5] of CPU, PROFILE_DELAY [5] seconds
#                 after the start of the benchmark
set -euo pipefail

MODE=${1:-snsd}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NEXO_BIN=${NEXO_BIN:-$ROOT/bin/nexo}
WARP_VERSION=${WARP_VERSION:-v1.8.2}
OUT=${OUT:-$ROOT/bin/bench/$MODE}
DURATION=${DURATION:-20s}
CONCURRENCY=${CONCURRENCY:-16}
LARGE_SIZE=${LARGE_SIZE:-64MiB}
LARGE_OBJECTS=${LARGE_OBJECTS:-32}
MIXED_SIZE=${MIXED_SIZE:-1MiB}
MIXED_OBJECTS=${MIXED_OBJECTS:-1000}
LIST_OBJECTS=${LIST_OBJECTS:-10000}
BENCHES=${BENCHES:-put get mixed list}
PROFILE=${PROFILE:-0}
ACCESS_KEY=benchadmin
SECRET_KEY=benchpassword123
REGION=us-east-1

case "$MODE" in
snsd | snmd | cluster) ;;
*)
	echo "usage: $0 snsd|snmd|cluster" >&2
	exit 2
	;;
esac
if [ ! -x "$NEXO_BIN" ]; then
	echo "warp.sh: $NEXO_BIN not found: run make build-go" >&2
	exit 1
fi
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)

WARP=${WARP:-$(command -v warp || true)}
if [ -z "$WARP" ]; then
	echo "warp.sh: installing github.com/minio/warp@$WARP_VERSION" >&2
	GOBIN="$OUT/.bin" go install "github.com/minio/warp@$WARP_VERSION"
	WARP="$OUT/.bin/warp"
fi

created=""
if [ -z "${DATA_DIR:-}" ]; then
	parent=${TMPDIR:-/tmp}
	if [ "${BENCH_TMPFS:-0}" = 1 ]; then
		parent=/dev/shm
	fi
	DATA_DIR=$(mktemp -d "$parent/nexo-bench.XXXXXX")
	created=$DATA_DIR
fi

pids=()
cleanup() {
	for pid in "${pids[@]}"; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in "${pids[@]}"; do
		wait "$pid" 2>/dev/null || true
	done
	if [ -n "$created" ]; then
		rm -rf "$created"
	fi
}
trap cleanup EXIT INT TERM

# start NAME DIR ADDRESS CONSOLE ENDPOINT... starts one server in DIR.
start() {
	local name=$1 dir=$2 address=$3 console=$4
	shift 4
	mkdir -p "$dir"
	(cd "$dir" && NEXO_ROOT_USER=$ACCESS_KEY NEXO_ROOT_PASSWORD=$SECRET_KEY \
		exec "$NEXO_BIN" server --log-format text --log-level warn \
		--address "$address" --console-address "$console" "$@") >"$OUT/$name.log" 2>&1 &
	pids+=("$!")
}

# wait_ok URL waits for a health endpoint to answer 200.
wait_ok() {
	for _ in $(seq 1 300); do
		if curl -sf -o /dev/null "$1"; then
			return 0
		fi
		sleep 0.2
	done
	echo "warp.sh: $1 is not healthy, see $OUT/*.log" >&2
	exit 1
}

case "$MODE" in
snsd)
	start nexo "$DATA_DIR" 127.0.0.1:9500 127.0.0.1:9501 "$DATA_DIR/d1"
	hosts=127.0.0.1:9500
	wait_ok http://127.0.0.1:9500/nexo/health/ready
	;;
snmd)
	start nexo "$DATA_DIR" 127.0.0.1:9500 127.0.0.1:9501 "$DATA_DIR/d{1...4}"
	hosts=127.0.0.1:9500
	wait_ok http://127.0.0.1:9500/nexo/health/ready
	;;
cluster)
	# Every process sees its own drives at /proc/self/cwd/d1, d2 (one
	# working directory per node), as make run-cluster does.
	hosts=""
	for i in 1 2 3 4; do
		start "nexo$i" "$DATA_DIR/n$i" "127.0.0.1:951$i" "127.0.0.1:952$i" \
			"http://127.0.0.1:{9511...9514}/proc/self/cwd/d{1...2}"
		hosts="$hosts${hosts:+,}127.0.0.1:951$i"
	done
	for i in 1 2 3 4; do
		wait_ok "http://127.0.0.1:951$i/nexo/health/ready"
	done
	wait_ok http://127.0.0.1:9511/nexo/health/cluster
	;;
esac
console=http://127.0.0.1:9501
if [ "$MODE" = cluster ]; then
	console=http://127.0.0.1:9521
fi

# profile NAME DELAY SECONDS takes a CPU profile of the first node for
# SECONDS, DELAY seconds after the start of the benchmark (after the
# preparation of its objects).
profile() {
	sleep "$2"
	curl -sf -X POST --aws-sigv4 "aws:amz:$REGION:s3" -u "$ACCESS_KEY:$SECRET_KEY" \
		-o "$OUT/$1.cpu.pb.gz" "$console/api/v1/profile?type=cpu&seconds=$3" ||
		echo "warp.sh: profile of $1 failed" >&2
}

common=(--host "$hosts" --access-key "$ACCESS_KEY" --secret-key "$SECRET_KEY" --region "$REGION"
	--concurrent "$CONCURRENCY" --no-color --host-select roundrobin)

: >"$OUT/summary.txt"
for bench in $BENCHES; do
	case "$bench" in
	put) args=(put --obj.size "$LARGE_SIZE" --duration "${PUT_DURATION:-$DURATION}") ;;
	get) args=(get --obj.size "$LARGE_SIZE" --objects "$LARGE_OBJECTS" --duration "${GET_DURATION:-$DURATION}") ;;
	mixed) args=(mixed --obj.size "$MIXED_SIZE" --obj.randsize --objects "$MIXED_OBJECTS" --duration "${MIXED_DURATION:-$DURATION}") ;;
	list) args=(list --obj.size 1KiB --objects "$LIST_OBJECTS" --duration "${LIST_DURATION:-$DURATION}") ;;
	*)
		echo "warp.sh: unknown benchmark $bench" >&2
		exit 2
		;;
	esac
	if [ "$PROFILE" = 1 ]; then
		profile "$bench" "${PROFILE_DELAY:-5}" "${PROFILE_SECONDS:-5}" &
		prof=$!
	fi
	(cd "$OUT" && "$WARP" "${args[@]}" "${common[@]}") 2>&1 | tee "$OUT/$bench.txt"
	if [ "$PROFILE" = 1 ]; then
		wait "$prof" || true
	fi
	{
		echo "== $MODE $bench"
		grep -E '^Report:|^ \* (Average|Reqs|TTFB):' "$OUT/$bench.txt" || true
	} >>"$OUT/summary.txt"
done
echo
cat "$OUT/summary.txt"
