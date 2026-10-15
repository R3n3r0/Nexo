#!/usr/bin/env bash
# run-s3tests.sh runs the Ceph s3-tests compatibility suite (docs/06 §6.1,
# T-9.1, DoD of T-7.1) against a Nexo server started by the script.
#
#   test/compat/run-s3tests.sh [-- PYTEST_ARGS...]
#
# Steps:
#   1. fetches github.com/ceph/s3-tests at the pinned commit (S3TESTS_COMMIT);
#   2. creates a Python venv and installs requirements.txt of the suite,
#      constrained by test/compat/constraints.txt (reproducible boto3);
#   3. starts bin/nexo over TLS (SSE-C needs it) with the builtin KMS
#      (SSE-S3, SSE-KMS): SNMD with 4 drives by default, NEXO_S3TESTS_MODE
#      selects SNSD (1 drive) or a cluster of 4 processes of 2 drives
#      (Linux; the suite talks to the first node);
#   4. creates the users of the suite with nexoctl admin: "main" (every S3
#      action), "alt" and "tenant" (list and create buckets only: Nexo has
#      a single account, the grants of another account are modeled with
#      bucket policies) and the KMS keys testkey-1 and testkey-2;
#   5. writes s3tests.conf and runs every functional file of the suite
#      (test_s3.py, test_headers.py, test_iam.py, test_sts.py,
#      test_s3select.py, test_sns.py, test_s3control.py) with pytest,
#      deselecting the tests of test/compat/excluded.txt (each with its
#      category and reason);
#   6. writes report.txt, report.json and junit.xml in the report directory
#      (per functional group, per exclusion category and per test).
#
# When git or pip cannot reach the network the run is skipped (exit 0, the
# reason in report/SKIPPED) unless NEXO_S3TESTS_REQUIRE_NETWORK=1 (CI).
# The exit status is 1 when a group with applicable tests passes less than
# NEXO_S3TESTS_THRESHOLD of them (95%, T-9.1), with NEXO_S3TESTS_STRICT=1
# when any test fails, and with NEXO_S3TESTS_CHECK_EXCLUDED when an
# excluded test passes (its exclusion is no longer justified).
#
# Environment:
#   S3TESTS_REPO, S3TESTS_COMMIT    the suite and its pinned commit
#   NEXO_S3TESTS_WORKDIR            work directory (default test/compat/.work)
#   NEXO_S3TESTS_REPORT_DIR         report directory (default $WORKDIR/report)
#   NEXO_BIN, NEXOCTL_BIN           binaries (default bin/nexo, bin/nexoctl,
#                                   built with go build when missing)
#   NEXO_S3TESTS_BUILD=1            rebuild the binaries first
#   NEXO_S3TESTS_REQUIRE_NETWORK=1  fail instead of skipping without network
#   NEXO_S3TESTS_STRICT=1           fail on any failed test
#   NEXO_S3TESTS_TIMEOUT            timeout of the pytest run (default 90m)
#   NEXO_S3TESTS_MODE               snmd (default), snsd or cluster
#   NEXO_S3TESTS_FILES              files of s3tests/functional to run
#                                   (default: every test_*.py of the suite)
#   NEXO_S3TESTS_THRESHOLD          pass rate of every group (default 0.95)
#   NEXO_S3TESTS_CHECK_EXCLUDED     "names": also run the tests excluded by
#                                   name and fail if one passes; "all": the
#                                   marker exclusions too
#   PYTHON                          interpreter of the venv (default python3)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPAT="$ROOT/test/compat"
S3TESTS_REPO="${S3TESTS_REPO:-https://github.com/ceph/s3-tests}"
S3TESTS_COMMIT="${S3TESTS_COMMIT:-5522d1c351f75bc00ae0f64f742f3f095f5939d9}"
WORK="${NEXO_S3TESTS_WORKDIR:-$COMPAT/.work}"
REPORT_DIR="${NEXO_S3TESTS_REPORT_DIR:-$WORK/report}"
NEXO_BIN="${NEXO_BIN:-$ROOT/bin/nexo}"
NEXOCTL_BIN="${NEXOCTL_BIN:-$ROOT/bin/nexoctl}"
REQUIRE_NETWORK="${NEXO_S3TESTS_REQUIRE_NETWORK:-0}"
STRICT="${NEXO_S3TESTS_STRICT:-0}"
TIMEOUT="${NEXO_S3TESTS_TIMEOUT:-90m}"
MODE="${NEXO_S3TESTS_MODE:-snmd}"
FILES="${NEXO_S3TESTS_FILES:-test_s3.py test_headers.py test_iam.py test_sts.py test_s3select.py test_sns.py test_s3control.py}"
THRESHOLD="${NEXO_S3TESTS_THRESHOLD:-0.95}"
CHECK_EXCLUDED="${NEXO_S3TESTS_CHECK_EXCLUDED:-}"
PYTHON="${PYTHON:-python3}"

if [[ "${1:-}" == "--" ]]; then
	shift
fi

log() { printf 's3-tests: %s\n' "$*" >&2; }
# shellcheck source=test/compat/lib.sh
. "$COMPAT/lib.sh"

skip() {
	mkdir -p "$REPORT_DIR"
	printf '%s\n' "$*" >"$REPORT_DIR/SKIPPED"
	if [[ "$REQUIRE_NETWORK" == "1" ]]; then
		log "FAILED: $* (NEXO_S3TESTS_REQUIRE_NETWORK=1)"
		exit 1
	fi
	log "SKIPPED: $*"
	exit 0
}

case "$MODE" in
snsd | snmd | cluster) ;;
*) log "unknown NEXO_S3TESTS_MODE: $MODE (snsd, snmd or cluster)"; exit 2 ;;
esac
case "$CHECK_EXCLUDED" in
"" | names | all) ;;
*) log "unknown NEXO_S3TESTS_CHECK_EXCLUDED: $CHECK_EXCLUDED (names or all)"; exit 2 ;;
esac

for tool in git curl openssl "$PYTHON"; do
	command -v "$tool" >/dev/null || { log "missing tool: $tool"; exit 2; }
done

mkdir -p "$WORK"
rm -rf "$REPORT_DIR"
mkdir -p "$REPORT_DIR"

# 1. The suite at the pinned commit.
SUITE="$WORK/s3-tests"
if [[ "$(git -C "$SUITE" rev-parse HEAD 2>/dev/null || true)" != "$S3TESTS_COMMIT" ]]; then
	log "fetching $S3TESTS_REPO@$S3TESTS_COMMIT"
	rm -rf "$SUITE"
	git init -q "$SUITE"
	git -C "$SUITE" remote add origin "$S3TESTS_REPO"
	if ! git -C "$SUITE" fetch -q --depth 1 origin "$S3TESTS_COMMIT"; then
		rm -rf "$SUITE"
		skip "git cannot fetch $S3TESTS_REPO@$S3TESTS_COMMIT (no network?)"
	fi
	git -C "$SUITE" -c advice.detachedHead=false checkout -q FETCH_HEAD
fi

# 2. The virtual environment, reinstalled when the requirements change.
VENV="$WORK/venv"
stamp="$(cat "$SUITE/requirements.txt" "$COMPAT/constraints.txt" | sha256sum | cut -d' ' -f1)-$("$PYTHON" -c 'import sys; print("%d.%d" % sys.version_info[:2])')"
if [[ "$(cat "$VENV/.nexo-stamp" 2>/dev/null || true)" != "$stamp" ]]; then
	log "creating the venv ($("$PYTHON" --version))"
	rm -rf "$VENV"
	"$PYTHON" -m venv "$VENV"
	if ! "$VENV/bin/pip" install -q --disable-pip-version-check \
		-r "$SUITE/requirements.txt" -c "$COMPAT/constraints.txt"; then
		rm -rf "$VENV"
		skip "pip cannot install the requirements of s3-tests (no network?)"
	fi
	echo "$stamp" >"$VENV/.nexo-stamp"
fi

# 3. The server (NEXO_S3TESTS_MODE), TLS, builtin KMS.
compat_build "${NEXO_S3TESTS_BUILD:-0}"
CERTS="$WORK/certs"
compat_certs "$CERTS"
ROOT_USER="s3tests-root"
# The access keys of the sample configuration of the suite; the secret
# keys have the AWS length (40) and the main one contains '/' and '+'
# (signature canonicalization).
MAIN_AK="0555b35654ad1656d804"
MAIN_SK="h7GhxuBLTrlhVUyxSPUKUV8r/2EI4ngq+xD7iBdB"
ALT_AK="NOPQRSTUVWXYZABCDEFG"
ALT_SK="nopqrstuvwxyzabcdefghijklmnabcdefghijklm"
TENANT_AK="HIJKLMNOPQRSTUVWXYZA"
TENANT_SK="opqrstuvwxyzabcdefghijklmnopqrstuvwxyzab"
compat_start "$MODE" "$WORK/data" "$CERTS"

# 4. Users, policies and KMS keys with nexoctl.
export NEXOCTL_CONFIG_DIR="$WORK/nexoctl"
rm -rf "$NEXOCTL_CONFIG_DIR"
ctl() { SSL_CERT_FILE="$CERTS/public.crt" "$NEXOCTL_BIN" "$@" >/dev/null; }
ctl alias set s3tests "$S3_URL" "$ROOT_USER" "$ROOT_PASSWORD" --admin-url "$ADMIN_URL"
POLICIES="$WORK/policies"
mkdir -p "$POLICIES"
cat >"$POLICIES/main.json" <<'EOF'
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["s3:*"], "Resource": ["arn:aws:s3:::*"]}]}
EOF
cat >"$POLICIES/alt.json" <<'EOF'
{"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Action": ["s3:ListAllMyBuckets", "s3:CreateBucket"], "Resource": ["arn:aws:s3:::*"]}]}
EOF
ctl admin policy create s3tests s3tests-main "$POLICIES/main.json"
ctl admin policy create s3tests s3tests-alt "$POLICIES/alt.json"
ctl admin user add s3tests "$MAIN_AK" "$MAIN_SK" --policy s3tests-main
ctl admin user add s3tests "$ALT_AK" "$ALT_SK" --policy s3tests-alt
ctl admin user add s3tests "$TENANT_AK" "$TENANT_SK" --policy s3tests-alt
ctl admin kms key create s3tests testkey-1
ctl admin kms key create s3tests testkey-2

# 5. The configuration of the suite and the run.
CONF="$WORK/s3tests.conf"
cat >"$CONF" <<EOF
[DEFAULT]
host = 127.0.0.1
port = $S3_PORT
is_secure = True
ssl_verify = False

[fixtures]
bucket prefix = nexo-{random}-

[s3 main]
display_name = $MAIN_AK
user_id = $MAIN_AK
email = main@nexo.test
api_name =
access_key = $MAIN_AK
secret_key = $MAIN_SK
kms_keyid = testkey-1
kms_keyid2 = testkey-2

[s3 alt]
display_name = $ALT_AK
user_id = $ALT_AK
email = alt@nexo.test
access_key = $ALT_AK
secret_key = $ALT_SK

[s3 tenant]
display_name = $TENANT_AK
user_id = $TENANT_AK
email = tenant@nexo.test
access_key = $TENANT_AK
secret_key = $TENANT_SK
tenant = testx

# IAM and STS tests are not run (Nexo has no IAM API); the sections are
# mandatory for the suite.
[iam]
email = iam@nexo.test
user_id = s3tests-iam
access_key = s3tests-iam
secret_key = s3tests-iam-unused
display_name = s3tests-iam

[iam root]
access_key = s3tests-iam-root
secret_key = s3tests-iam-root-unused
user_id = s3tests-iam-root
email = iam-root@nexo.test

[iam alt root]
access_key = s3tests-iam-alt-root
secret_key = s3tests-iam-alt-root-unused
user_id = s3tests-iam-alt-root
email = iam-alt-root@nexo.test
EOF

TARGETS=()
for f in $FILES; do
	[[ -f "$SUITE/s3tests/functional/$f" ]] || { log "no s3tests/functional/$f in the suite"; exit 2; }
	TARGETS+=("s3tests/functional/$f")
done
log "running $FILES (report in $REPORT_DIR)"
status=0
(
	cd "$SUITE"
	# The boto3 clients of the suite skip the verification (ssl_verify =
	# False); the tests that open their own connections (requests without
	# verify=, raw sockets) verify the server against the self-signed
	# certificate of the run.
	export REQUESTS_CA_BUNDLE="$CERTS/public.crt" SSL_CERT_FILE="$CERTS/public.crt"
	S3TEST_CONF="$CONF" PYTHONPATH="$COMPAT" PYTHONDONTWRITEBYTECODE=1 \
		NEXO_S3TESTS_SUITE="ceph/s3-tests" NEXO_S3TESTS_COMMIT="$S3TESTS_COMMIT" NEXO_S3TESTS_MODE="$MODE" \
		timeout "$TIMEOUT" "$VENV/bin/python" -m pytest "${TARGETS[@]}" \
		-p nexo_s3tests --nexo-excluded "$COMPAT/excluded.txt" --nexo-report-dir "$REPORT_DIR" \
		--nexo-threshold "$THRESHOLD" --nexo-check-excluded "$CHECK_EXCLUDED" \
		--junitxml "$REPORT_DIR/junit.xml" -p no:cacheprovider -q -rN --tb=short "$@" \
		>"$REPORT_DIR/pytest.log" 2>&1
) || status=$?
tail -n 40 "$REPORT_DIR/pytest.log" >&2 || true

# pytest: 0 all passed, 1 some failed, other codes are usage or internal
# errors (124: timeout).
if [[ "$status" != "0" && "$status" != "1" ]]; then
	log "pytest exited with status $status"
	exit "$status"
fi
if [[ ! -s "$REPORT_DIR/report.json" ]]; then
	log "no report produced"
	exit 1
fi
met="$("$PYTHON" -c 'import json, sys; print(json.load(open(sys.argv[1]))["target"]["met"])' "$REPORT_DIR/report.json")"
if [[ "$met" != "True" ]]; then
	log "some groups pass less than $THRESHOLD of their applicable tests"
	exit 1
fi
if [[ -n "$CHECK_EXCLUDED" ]]; then
	passing="$("$PYTHON" -c 'import json, sys; print("\n".join(json.load(open(sys.argv[1]))["check"]["passing"]))' "$REPORT_DIR/report.json")"
	if [[ -n "$passing" ]]; then
		log "excluded tests that pass (remove their exclusion):"
		printf '%s\n' "$passing" >&2
		exit 1
	fi
fi
if [[ "$STRICT" == "1" && "$status" == "1" ]]; then
	log "some tests failed (NEXO_S3TESTS_STRICT=1)"
	exit 1
fi
log "done"
