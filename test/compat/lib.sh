# shellcheck shell=bash disable=SC2034  # the variables are read by the sourcing scripts
# lib.sh: the Nexo deployment of the compatibility runners of test/compat
# (run-s3tests.sh, run-mint.sh), sourced by them.
#
# The caller sets ROOT (repository), WORK (work directory), REPORT_DIR
# (node logs), NEXO_BIN, NEXOCTL_BIN, PYTHON and log(), then calls
#
#   compat_build [force]           builds bin/nexo and bin/nexoctl when missing
#   compat_certs DIR               self-signed certificate of 127.0.0.1/localhost
#   compat_start MODE DATA CERTS   starts snsd (1 drive), snmd (4 drives) or a
#                                  cluster (4 processes x 2 drives, Linux) over
#                                  TLS with the builtin KMS, and waits until
#                                  every node is ready
#   compat_stop                    stops the nodes (installed as EXIT trap)
#
# compat_start sets ROOT_USER, ROOT_PASSWORD, S3_PORT, S3_URL, ADMIN_URL,
# NODES (name:port) and PIDS.

# The local server is never reached through a proxy.
export NO_PROXY="127.0.0.1,localhost${NO_PROXY:+,$NO_PROXY}"
export no_proxy="$NO_PROXY"

compat_build() {
	if [[ "${1:-0}" == "1" || ! -x "$NEXO_BIN" || ! -x "$NEXOCTL_BIN" ]]; then
		log "building bin/nexo and bin/nexoctl"
		(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$NEXO_BIN" ./cmd/nexo && CGO_ENABLED=0 go build -trimpath -o "$NEXOCTL_BIN" ./cmd/nexoctl)
	fi
}

compat_certs() {
	local dir="$1"
	if [[ ! -s "$dir/public.crt" ]]; then
		mkdir -p "$dir"
		openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
			-subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
			-keyout "$dir/private.key" -out "$dir/public.crt" 2>/dev/null
	fi
}

free_port() { "$PYTHON" -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'; }

# free_ports N prints the first of N consecutive free ports (the cluster
# endpoints span them with an ellipsis).
free_ports() {
	"$PYTHON" - "$1" <<'PY'
import random, socket, sys
n = int(sys.argv[1])
for _ in range(200):
    base = random.randrange(20000, 60000 - n)
    socks = []
    try:
        for p in range(base, base + n):
            s = socket.socket()
            s.bind(("127.0.0.1", p))
            socks.append(s)
    except OSError:
        continue
    finally:
        for s in socks:
            s.close()
    print(base)
    sys.exit(0)
sys.exit("no free port range")
PY
}

NODES=()
PIDS=()

compat_stop() {
	local pid
	for pid in "${PIDS[@]}"; do
		kill "$pid" 2>/dev/null || true
	done
	for pid in "${PIDS[@]}"; do
		wait "$pid" 2>/dev/null || true
	done
	PIDS=()
}

# compat_node NAME S3_PORT CONSOLE_PORT ENDPOINT... starts a node in
# $COMPAT_DATA/NAME (its working directory, see the cluster endpoints).
compat_node() {
	local name="$1" s3="$2" console="$3"
	shift 3
	mkdir -p "$COMPAT_DATA/$name"
	(
		cd "$COMPAT_DATA/$name" || exit 1
		NEXO_ROOT_USER="$ROOT_USER" NEXO_ROOT_PASSWORD="$ROOT_PASSWORD" NEXO_KMS_SECRET_KEY="$KMS_KEY" \
			exec "$NEXO_BIN" server --address "127.0.0.1:$s3" --console-address "127.0.0.1:$console" \
			--certs-dir "$COMPAT_CERTS" --log-format text --log-level warn "$@"
	) >"$REPORT_DIR/nexo-$name.log" 2>&1 &
	NODES+=("$name:$s3")
	PIDS+=("$!")
}

compat_node_logs() {
	local n
	for n in "${NODES[@]}"; do
		log "--- ${n%%:*}:"
		tail -n 30 "$REPORT_DIR/nexo-${n%%:*}.log" >&2 || true
	done
}

compat_start() {
	local mode="$1" console i deadline n pid
	COMPAT_DATA="$2"
	COMPAT_CERTS="$3"
	ROOT_USER="${ROOT_USER:-compat-root}"
	ROOT_PASSWORD="${ROOT_PASSWORD:-$(openssl rand -hex 16)}"
	KMS_KEY="${KMS_KEY:-compat-default:$(openssl rand -base64 32)}"
	rm -rf "$COMPAT_DATA"
	trap compat_stop EXIT
	case "$mode" in
	snsd)
		S3_PORT="$(free_port)"
		CONSOLE_PORT="$(free_port)"
		compat_node n1 "$S3_PORT" "$CONSOLE_PORT" "$COMPAT_DATA/n1/d1"
		;;
	snmd)
		S3_PORT="$(free_port)"
		CONSOLE_PORT="$(free_port)"
		compat_node n1 "$S3_PORT" "$CONSOLE_PORT" "$COMPAT_DATA/n1/d{1...4}"
		;;
	cluster)
		# 4 nodes of 2 drives on consecutive ports; every node runs the
		# same endpoints, /proc/self/cwd being its own directory (Linux).
		S3_PORT="$(free_ports 4)"
		CONSOLE_PORT="$(free_port)"
		for i in 1 2 3 4; do
			console="$CONSOLE_PORT"
			[[ "$i" == "1" ]] || console="$(free_port)"
			compat_node "n$i" "$((S3_PORT + i - 1))" "$console" \
				"https://127.0.0.1:{$S3_PORT...$((S3_PORT + 3))}/proc/self/cwd/d{1...2}"
		done
		;;
	*)
		log "unknown mode: $mode (snsd, snmd or cluster)"
		return 2
		;;
	esac
	S3_URL="https://127.0.0.1:$S3_PORT"
	ADMIN_URL="https://127.0.0.1:$CONSOLE_PORT"
	log "starting bin/nexo ($mode, ${#NODES[@]} node(s)) on $S3_URL"
	deadline=$((SECONDS + 120))
	for n in "${NODES[@]}"; do
		until curl -sfk -o /dev/null "https://127.0.0.1:${n##*:}/nexo/health/ready"; do
			for pid in "${PIDS[@]}"; do
				kill -0 "$pid" 2>/dev/null || { log "bin/nexo exited"; compat_node_logs; return 1; }
			done
			((SECONDS < deadline)) || { log "bin/nexo not ready after 120s"; compat_node_logs; return 1; }
			sleep 0.5
		done
	done
}
