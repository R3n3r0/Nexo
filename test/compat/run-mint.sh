#!/usr/bin/env bash
# run-mint.sh runs the SDK suites of MinIO Mint (docs/06 §6.1, T-9.1)
# against a Nexo server started by the script, without Docker: the suites
# of github.com/minio/mint (pinned commit) are built on the host with the
# toolchains installed there (Go, Python, Node.js, Java, Ruby, PHP), then
# run with the environment of mint.sh.
#
#   test/compat/run-mint.sh [SDK...]
#
# Steps:
#   1. fetches minio/mint at MINT_COMMIT and prepares a copy in the work
#      directory with the paths of its scripts (/mint/...) rewritten;
#   2. builds every selected suite (NEXO_MINT_SDKS, or the arguments) once
#      per version stamp: Go programs, a Python venv (minio-py, awscli,
#      s3cmd), npm (minio-js), Gradle/Maven Central (Java), RubyGems,
#      Composer (PHP); a suite whose toolchain or packages are missing is
#      reported as not built, with the reason;
#   3. starts bin/nexo over TLS with the builtin KMS (NEXO_MINT_MODE: snmd,
#      snsd or cluster, see lib.sh) and runs each suite with MINT_MODE=full,
#      the root credentials and the certificate of the run trusted;
#   4. writes report.txt and report.json (per suite: passed, failed, not
#      applicable as reported by the suite, expected failures of
#      test/compat/mint-excluded.txt with their reason) and the log of
#      every suite in the report directory.
#
# Exit status: 1 when a suite fails a test not listed in mint-excluded.txt,
# when a suite cannot be built with NEXO_MINT_REQUIRE_ALL=1 (CI), or when
# the server does not start. Without network (git) the run is skipped
# (exit 0, report/SKIPPED) unless NEXO_MINT_REQUIRE_NETWORK=1.
#
# Environment:
#   MINT_REPO, MINT_COMMIT       the suites and their pinned commit
#   MINT_MAVEN_REPO, MINT_MAVEN_MIRROR  Maven Central and its fallback (Java)
#   NEXO_MINT_SDKS               suites to run (default: all of run/core)
#   NEXO_MINT_MODE               snmd (default), snsd or cluster
#   NEXO_MINT_WORKDIR            work directory (default test/compat/.work/mint)
#   NEXO_MINT_REPORT_DIR         report directory (default $WORKDIR/report)
#   NEXO_MINT_TIMEOUT            timeout of each suite (default 30m)
#   NEXO_MINT_REQUIRE_NETWORK=1  fail instead of skipping without network
#   NEXO_MINT_REQUIRE_ALL=1      fail when a suite cannot be built
#   NEXO_BIN, NEXOCTL_BIN        binaries (default bin/nexo, bin/nexoctl)
#   PYTHON                       interpreter of the venv (default python3)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPAT="$ROOT/test/compat"
MINT_REPO="${MINT_REPO:-https://github.com/minio/mint}"
MINT_COMMIT="${MINT_COMMIT:-12559d50625b722d11fd798ae8ac2fb204e66dd1}"
WORK="${NEXO_MINT_WORKDIR:-$COMPAT/.work/mint}"
REPORT_DIR="${NEXO_MINT_REPORT_DIR:-$WORK/report}"
MODE="${NEXO_MINT_MODE:-snmd}"
TIMEOUT="${NEXO_MINT_TIMEOUT:-30m}"
NEXO_BIN="${NEXO_BIN:-$ROOT/bin/nexo}"
NEXOCTL_BIN="${NEXOCTL_BIN:-$ROOT/bin/nexoctl}"
PYTHON="${PYTHON:-python3}"
ALL_SDKS="aws-sdk-go-v2 aws-sdk-java-v2 aws-sdk-php aws-sdk-ruby awscli healthcheck mc minio-go minio-java minio-js minio-py s3cmd s3select versioning"
if [[ $# -gt 0 ]]; then
	SDKS="$*"
else
	SDKS="${NEXO_MINT_SDKS:-$ALL_SDKS}"
fi

# The versions mint resolves to "latest" at image build time, pinned here
# so that nightly results stay comparable (bump deliberately).
MINIO_GO_VERSION="${MINIO_GO_VERSION:-v7.4.1}"
MINIO_PY_VERSION="7.2.20"
MINIO_JS_VERSION="${MINIO_JS_VERSION:-8.0.7}"
MINIO_JAVA_VERSION="${MINIO_JAVA_VERSION:-9.0.3}"
MC_VERSION="${MC_VERSION:-RELEASE.2025-08-13T08-35-41Z}"
AWSCLI_VERSION="1.42.67"
BOTOCORE_VERSION="1.40.67"
S3CMD_VERSION="${S3CMD_VERSION:-2.4.0}"
SPOTBUGS_VERSION="4.2.2"
JUNIT_VERSION="5.11.4"
# Maven Central, and the mirror tried when it throttles (429).
MAVEN="${MINT_MAVEN_REPO:-https://repo1.maven.org/maven2}"
MAVEN_MIRROR="${MINT_MAVEN_MIRROR:-https://maven-central.storage-download.googleapis.com/maven2}"

log() { printf 'mint: %s\n' "$*" >&2; }
# shellcheck source=test/compat/lib.sh
. "$COMPAT/lib.sh"

skip() {
	mkdir -p "$REPORT_DIR"
	printf '%s\n' "$*" >"$REPORT_DIR/SKIPPED"
	if [[ "${NEXO_MINT_REQUIRE_NETWORK:-0}" == "1" ]]; then
		log "FAILED: $* (NEXO_MINT_REQUIRE_NETWORK=1)"
		exit 1
	fi
	log "SKIPPED: $*"
	exit 0
}

for tool in git curl openssl jq "$PYTHON"; do
	command -v "$tool" >/dev/null || { log "missing tool: $tool"; exit 2; }
done
for sdk in $SDKS; do
	[[ " $ALL_SDKS " == *" $sdk "* ]] || { log "unknown suite: $sdk ($ALL_SDKS)"; exit 2; }
done

mkdir -p "$WORK"
rm -rf "$REPORT_DIR"
mkdir -p "$REPORT_DIR"

# 1. Mint at the pinned commit, and the copy the suites run from.
SRC="$WORK/src"
if [[ "$(git -C "$SRC" rev-parse HEAD 2>/dev/null || true)" != "$MINT_COMMIT" ]]; then
	log "fetching $MINT_REPO@$MINT_COMMIT"
	rm -rf "$SRC"
	git init -q "$SRC"
	git -C "$SRC" remote add origin "$MINT_REPO"
	if ! git -C "$SRC" fetch -q --depth 1 origin "$MINT_COMMIT"; then
		rm -rf "$SRC"
		skip "git cannot fetch $MINT_REPO@$MINT_COMMIT (no network?)"
	fi
	git -C "$SRC" -c advice.detachedHead=false checkout -q FETCH_HEAD
fi
MINT_ROOT="$WORK/root"
STAMP="$MINT_COMMIT $MINIO_GO_VERSION $MINIO_PY_VERSION $MINIO_JS_VERSION $MINIO_JAVA_VERSION $MC_VERSION $AWSCLI_VERSION $S3CMD_VERSION"
if [[ "$(cat "$MINT_ROOT/.nexo-stamp" 2>/dev/null || true)" != "$STAMP" ]]; then
	rm -rf "$MINT_ROOT"
	mkdir -p "$MINT_ROOT"
	cp -a "$SRC/." "$MINT_ROOT/"
	rm -rf "$MINT_ROOT/.git"
	# The scripts of the image refer to /mint.
	grep -rlF '/mint/' "$MINT_ROOT/run" "$MINT_ROOT/build" | while read -r f; do
		sed -i "s#/mint/#$MINT_ROOT/#g" "$f"
	done
	echo "$STAMP" >"$MINT_ROOT/.nexo-stamp"
fi
export MINT_ROOT_DIR="$MINT_ROOT"
MINT_DATA_DIR="$MINT_ROOT/data"
if [[ ! -s "$MINT_DATA_DIR/datafile-129-MB" ]]; then
	log "creating the data files"
	(cd "$MINT_ROOT" && MINT_ROOT_DIR="$MINT_ROOT" ./create-data-files.sh >/dev/null)
fi

# The tools of the suites stay in the work directory.
VENV="$WORK/venv"
export GEM_HOME="$WORK/gems"
export HOME="$WORK/home"
mkdir -p "$HOME" "$GEM_HOME"
export PATH="$VENV/bin:$GEM_HOME/bin:$WORK/bin:$PATH"
mkdir -p "$WORK/bin"

# build_<suite> builds a suite in $MINT_ROOT/run/core/<suite>; a failure
# leaves the suite unbuilt (its reason in report/build-<suite>.log).
build_go_dir() { (cd "$1" && CGO_ENABLED=0 go build -ldflags "-s -w" "${@:2}"); }
build_aws-sdk-go-v2() { build_go_dir "$MINT_ROOT/run/core/aws-sdk-go-v2"; }
build_healthcheck() { build_go_dir "$MINT_ROOT/run/core/healthcheck"; }
build_versioning() { build_go_dir "$MINT_ROOT/build/versioning" -o "$MINT_ROOT/run/core/versioning/tests"; }
build_minio-go() {
	local dir="$MINT_ROOT/run/core/minio-go"
	curl -fsSL --retry 5 --retry-delay 3 -o "$dir/main.go" "https://raw.githubusercontent.com/minio/minio-go/$MINIO_GO_VERSION/functional_tests.go"
	tail -n +34 "$dir/versioning_test.go" >>"$dir/main.go"
	sed -i '/testStatObjectWithVersioning()/a\
		testBucketVersioningExcludedPrefixes()
' "$dir/main.go"
	(cd "$dir" && go get "github.com/minio/minio-go/v7@$MINIO_GO_VERSION" && go mod tidy && CGO_ENABLED=0 go build -ldflags "-s -w" -o minio-go main.go)
}
venv() {
	if [[ ! -x "$VENV/bin/python" ]]; then
		"$PYTHON" -m venv "$VENV"
		"$VENV/bin/pip" install -q --disable-pip-version-check --upgrade pip
	fi
}
build_minio-py() {
	venv
	"$VENV/bin/pip" install -q --disable-pip-version-check faker "minio==$MINIO_PY_VERSION"
	curl -fsSL --retry 5 --retry-delay 3 -o "$MINT_ROOT/run/core/minio-py/tests.py" "https://raw.githubusercontent.com/minio/minio-py/$MINIO_PY_VERSION/tests/functional/tests.py"
}
build_s3select() { venv && "$VENV/bin/pip" install -q --disable-pip-version-check "minio==$MINIO_PY_VERSION"; }
build_awscli() { venv && "$VENV/bin/pip" install -q --disable-pip-version-check "awscli==$AWSCLI_VERSION" "botocore==$BOTOCORE_VERSION"; }
build_s3cmd() { venv && "$VENV/bin/pip" install -q --disable-pip-version-check "s3cmd==$S3CMD_VERSION"; }
build_minio-js() {
	command -v npm >/dev/null || { echo "npm not installed"; return 1; }
	local dir="$MINT_ROOT/test-run/minio-js"
	rm -rf "$dir"
	git clone -q --depth 1 -b "$MINIO_JS_VERSION" https://github.com/minio/minio-js.git "$dir"
	(cd "$dir" && npm install --no-audit --no-fund --quiet)
	(cd "$MINT_ROOT/run/core/minio-js" && npm install --no-audit --no-fund --quiet)
	# run.sh copies the clone and runs npm install itself: done here.
	sed -i 's#^npm install --quiet &>/dev/null$#:#' "$MINT_ROOT/run/core/minio-js/run.sh"
}
build_minio-java() {
	command -v javac >/dev/null || { echo "javac not installed"; return 1; }
	local dir="$MINT_ROOT/run/core/minio-java" jar cp=""
	rm -rf "$dir/minio-java.git"
	git clone -q --depth 1 -b "$MINIO_JAVA_VERSION" https://github.com/minio/minio-java.git "$dir/minio-java.git"
	sed -i.bak '/Assertions\.assertEquals(/{N;/"cors: expected: "/s/expectedConfig, config, "cors:/Xml.marshal(expectedConfig), Xml.marshal(config), "cors:/;}' \
		"$dir/minio-java.git/functional/FunctionalTest.java"
	for jar in "io/minio/minio/$MINIO_JAVA_VERSION/minio-$MINIO_JAVA_VERSION-all.jar" \
		"io/minio/minio-admin/$MINIO_JAVA_VERSION/minio-admin-$MINIO_JAVA_VERSION-all.jar" \
		"com/github/spotbugs/spotbugs-annotations/$SPOTBUGS_VERSION/spotbugs-annotations-$SPOTBUGS_VERSION.jar" \
		"org/junit/jupiter/junit-jupiter-api/$JUNIT_VERSION/junit-jupiter-api-$JUNIT_VERSION.jar" \
		"org/junit/platform/junit-platform-commons/1.11.4/junit-platform-commons-1.11.4.jar" \
		"org/opentest4j/opentest4j/1.3.0/opentest4j-1.3.0.jar"; do
		# Kept between builds: Maven Central throttles (429) repeated
		# downloads.
		if [[ ! -s "$dir/$(basename "$jar")" ]]; then
			curl -fsSL --retry 3 --retry-delay 3 -o "$dir/$(basename "$jar").part" "$MAVEN/$jar" ||
				curl -fsSL --retry 3 --retry-delay 3 -o "$dir/$(basename "$jar").part" "$MAVEN_MIRROR/$jar"
			mv "$dir/$(basename "$jar").part" "$dir/$(basename "$jar")"
		fi
		cp="$cp${cp:+:}$dir/$(basename "$jar")"
	done
	javac -cp "$cp" "$dir/minio-java.git/functional"/*.java
	cp -a "$dir/minio-java.git/functional"/*.class "$dir/"
	rm -rf "$dir/minio-java.git"
}
build_aws-sdk-java-v2() {
	command -v gradle >/dev/null || { echo "gradle not installed"; return 1; }
	(cd "$MINT_ROOT/build/aws-sdk-java-v2" && gradle --quiet --no-daemon clean build &&
		cp app/build/libs/FunctionalTests.jar "$MINT_ROOT/run/core/aws-sdk-java-v2/" && rm -rf app/build)
}
build_aws-sdk-ruby() {
	command -v gem >/dev/null || { echo "ruby not installed"; return 1; }
	gem install --no-document aws-sdk-resources aws-sdk multipart_body
}
build_aws-sdk-php() {
	command -v php >/dev/null || { echo "php not installed"; return 1; }
	local dir="$MINT_ROOT/run/core/aws-sdk-php"
	curl -fsSL --retry 5 --retry-delay 3 -o "$WORK/composer-setup.php" https://getcomposer.org/installer
	php "$WORK/composer-setup.php" --quiet --install-dir="$dir"
	# From git (the archives come from the GitHub API, which throttles
	# unauthenticated clients).
	php "$dir/composer.phar" --working-dir="$dir" --no-interaction --quiet install --prefer-source
}
build_mc() {
	local dir="$MINT_ROOT/run/core/mc"
	GOBIN="$dir" go install "github.com/minio/mc@$MC_VERSION"
	curl -fsSL --retry 5 --retry-delay 3 -o "$dir/functional-tests.sh" "https://raw.githubusercontent.com/minio/mc/$MC_VERSION/functional-tests.sh"
	chmod +x "$dir/functional-tests.sh"
}

# 2. The suites, built once per stamp.
declare -A BUILT
for sdk in $SDKS; do
	marker="$MINT_ROOT/.built-$sdk"
	if [[ -f "$marker" ]]; then
		BUILT[$sdk]=1
		continue
	fi
	log "building $sdk"
	# In the background so that errexit applies to the build function (an
	# if condition would disable it).
	(
		set -e
		"build_$sdk"
	) >"$REPORT_DIR/build-$sdk.log" 2>&1 &
	if wait "$!"; then
		touch "$marker"
		BUILT[$sdk]=1
	else
		BUILT[$sdk]=0
		log "$sdk: build failed (report/build-$sdk.log): $(tail -n 3 "$REPORT_DIR/build-$sdk.log" | tr '\n' ' ')"
	fi
done

# 3. The server and the suites.
compat_build "${NEXO_MINT_BUILD:-0}"
CERTS="$WORK/certs"
compat_certs "$CERTS"
ROOT_USER="mint-root"
compat_start "$MODE" "$WORK/data" "$CERTS"

# Trust the certificate of the run: a bundle with the system CAs for the
# libraries that replace them (Go, Python, Ruby), a Java trust store.
BUNDLE="$WORK/ca-bundle.crt"
system_bundle="$(openssl version -d | sed 's/.*"\(.*\)"/\1/')/certs/ca-certificates.crt"
{ cat "$system_bundle" 2>/dev/null || true; cat "$CERTS/public.crt"; } >"$BUNDLE"
export SSL_CERT_FILE="$BUNDLE" REQUESTS_CA_BUNDLE="$BUNDLE" AWS_CA_BUNDLE="$BUNDLE" CURL_CA_BUNDLE="$BUNDLE" NODE_EXTRA_CA_CERTS="$CERTS/public.crt"
if command -v keytool >/dev/null; then
	STORE="$WORK/truststore.p12"
	rm -f "$STORE"
	keytool -importcert -noprompt -storetype PKCS12 -keystore "$STORE" -storepass changeit -alias nexo -file "$CERTS/public.crt" >/dev/null 2>&1
	export JAVA_TOOL_OPTIONS="-Djavax.net.ssl.trustStore=$STORE -Djavax.net.ssl.trustStoreType=PKCS12 -Djavax.net.ssl.trustStorePassword=changeit"
fi
mkdir -p "$HOME/.mc/certs/CAs"
cp "$CERTS/public.crt" "$HOME/.mc/certs/CAs/nexo.crt"
# PHP (curl, openssl) reads its CAs from its ini settings only.
mkdir -p "$WORK/php-ini"
printf 'curl.cainfo=%s\nopenssl.cafile=%s\n' "$BUNDLE" "$BUNDLE" >"$WORK/php-ini/nexo.ini"
export PHP_INI_SCAN_DIR=":$WORK/php-ini"
# AWS CLI v1 presigns the S3 URLs with Signature Version 2 unless told
# otherwise; Nexo authenticates SigV4 only, as the AWS regions opened since
# 2014 (the suite then configures the credentials and the region).
mkdir -p "$HOME/.aws"
printf '[default]\ns3 =\n    signature_version = s3v4\n' >"$HOME/.aws/config"

# Credentials of the host environment would take precedence over those of
# the suites (AWS CLI, SDKs).
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE AWS_DEFAULT_PROFILE AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE
export SERVER_ENDPOINT="127.0.0.1:$S3_PORT" ACCESS_KEY="$ROOT_USER" SECRET_KEY="$ROOT_PASSWORD"
export ENABLE_HTTPS=1 SERVER_REGION=us-east-1 MINT_MODE=full MINT_DATA_DIR ENABLE_VIRTUAL_STYLE=0 RUN_ON_FAIL=1
export SERVER_IP=127.0.0.1 SERVER_PORT="$S3_PORT"

for sdk in $SDKS; do
	mkdir -p "$REPORT_DIR/$sdk"
	if [[ "${BUILT[$sdk]}" != "1" ]]; then
		echo "not built" >"$REPORT_DIR/$sdk/status"
		continue
	fi
	log "running $sdk"
	# The awscli suite appends the parts to /tmp/multipart (a fresh file in
	# the container of mint.sh): one left by an interrupted run would
	# corrupt the next CompleteMultipartUpload.
	rm -f /tmp/multipart
	start=$SECONDS
	rv=0
	(cd "$MINT_ROOT/run/core/$sdk" && timeout "$TIMEOUT" ./run.sh "$REPORT_DIR/$sdk/log.json" "$REPORT_DIR/$sdk/error.log") || rv=$?
	echo "exit $rv in $((SECONDS - start))s" >"$REPORT_DIR/$sdk/status"
done

# 4. The report.
# shellcheck disable=SC2086 # one argument per suite
"$PYTHON" "$COMPAT/mint_report.py" "$REPORT_DIR" "$COMPAT/mint-excluded.txt" "$MODE" "$MINT_COMMIT" $SDKS
cat "$REPORT_DIR/report.txt" >&2
if [[ "$(jq -r .unexpected_failures "$REPORT_DIR/report.json")" != "0" ]]; then
	log "some suites failed tests not listed in mint-excluded.txt"
	exit 1
fi
if [[ "${NEXO_MINT_REQUIRE_ALL:-0}" == "1" && "$(jq -r .not_built "$REPORT_DIR/report.json")" != "0" ]]; then
	log "some suites could not be built (NEXO_MINT_REQUIRE_ALL=1)"
	exit 1
fi
log "done"
