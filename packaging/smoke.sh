#!/bin/sh
# Smoke test of a release build (TM-10, docs/08 §8.6): run by
# "make release-smoke" on the GoReleaser output and by the release workflow
# on every OS/arch runner (arm64 through QEMU).
#
#   packaging/smoke.sh DIR [EXPECTED_VERSION]
#
# DIR holds the nexo and nexoctl binaries of one platform (e.g.
# dist/nexo_linux_amd64_v1 and dist/nexoctl_linux_amd64_v1 merged, or an
# extracted archive pair). EXPECTED_VERSION (e.g. v1.0.0) must be the
# version printed by both binaries. RUNNER (e.g. qemu-aarch64-static,
# "arch -x86_64") prefixes every command, for a foreign architecture.
# SMOKE_QUICK=1 stops after the dry run (Windows, where the server cannot
# be stopped with SIGTERM from a POSIX shell).
#
# Checks: version of both binaries; nexo server --dry-run; a single-drive
# server started on loopback ports answers /nexo/health/ready, serves the
# embedded console and passes an S3 round trip with nexoctl (alias, mb,
# cp, cat, rb); it stops on SIGTERM.
#
# covers: S-OPS-12, S-OPS-14, C-UX-00
set -eu

dir=${1:?usage: smoke.sh DIR [EXPECTED_VERSION]}
want=${2:-}
runner=${RUNNER:-}
s3port=${SMOKE_S3_PORT:-19700}
consoleport=${SMOKE_CONSOLE_PORT:-19701}
nexo="$dir/nexo"
nexoctl="$dir/nexoctl"
[ -x "$nexo" ] || nexo="$dir/nexo.exe"
[ -x "$nexoctl" ] || nexoctl="$dir/nexoctl.exe"

work=$(mktemp -d)
pid=
cleanup() {
    if [ -n "$pid" ]; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
    rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() {
    echo "smoke: FAIL: $*" >&2
    [ -f "$work/server.log" ] && tail -20 "$work/server.log" >&2
    exit 1
}

# shellcheck disable=SC2086
run() { $runner "$@"; }

echo "smoke: versions"
v1=$(run "$nexo" version) || fail "nexo version"
v2=$(run "$nexoctl" version) || fail "nexoctl version"
echo "  $v1"
echo "  $v2"
if [ -n "$want" ]; then
    case "$v1" in "nexo $want "*) ;; *) fail "nexo reports \"$v1\", want $want" ;; esac
    case "$v2" in "nexoctl $want "*) ;; *) fail "nexoctl reports \"$v2\", want $want" ;; esac
fi

export NEXO_ROOT_USER=smoke-admin
export NEXO_ROOT_PASSWORD=smoke-password-123
export NEXOCTL_CONFIG_DIR="$work/nexoctl"

echo "smoke: dry run"
run "$nexo" server --dry-run --address "127.0.0.1:$s3port" --console-address "127.0.0.1:$consoleport" \
    "$work/data" >"$work/dryrun.log" 2>&1 || { cat "$work/dryrun.log" >&2; fail "nexo server --dry-run"; }

if [ "${SMOKE_QUICK:-}" = 1 ]; then
    echo "smoke: OK, quick ($dir)"
    exit 0
fi

echo "smoke: server"
# Not through run(): $! must be the server itself (or the QEMU process that
# forwards the signals to it), not a subshell.
# shellcheck disable=SC2086
$runner "$nexo" server --address "127.0.0.1:$s3port" --console-address "127.0.0.1:$consoleport" \
    --log-format text "$work/data" >"$work/server.log" 2>&1 &
pid=$!
i=0
until run "$nexo" healthcheck --url "http://127.0.0.1:$s3port/nexo/health/ready" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 120 ] || fail "server not ready after 60s"
    kill -0 "$pid" 2>/dev/null || fail "server exited"
    sleep 0.5
done

if command -v curl >/dev/null 2>&1; then
    curl -fsS "http://127.0.0.1:$consoleport/" | grep -q '<title>Nexo' || fail "console not embedded"
fi

echo "smoke: S3 round trip"
printf 'hello nexo\n' >"$work/hello.txt"
run "$nexoctl" -q alias set smoke "http://127.0.0.1:$s3port" "$NEXO_ROOT_USER" "$NEXO_ROOT_PASSWORD" || fail "alias set"
run "$nexoctl" -q mb smoke/smoke-bucket || fail "mb"
run "$nexoctl" -q cp "$work/hello.txt" smoke/smoke-bucket/hello.txt || fail "cp"
got=$(run "$nexoctl" cat smoke/smoke-bucket/hello.txt) || fail "cat"
[ "$got" = "hello nexo" ] || fail "cat returned \"$got\""
run "$nexoctl" -q rb --force smoke/smoke-bucket || fail "rb"

echo "smoke: stop"
kill -TERM "$pid"
i=0
while kill -0 "$pid" 2>/dev/null; do
    i=$((i + 1))
    [ "$i" -lt 60 ] || fail "server still running 30s after SIGTERM"
    sleep 0.5
done
wait "$pid" 2>/dev/null || true
pid=
echo "smoke: OK ($dir)"
