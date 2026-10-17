# Nexo build and verification targets.

MODULE      := github.com/r3n3r0/nexo
BIN_DIR     := bin
WEB_DIR     := web
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
  -X $(MODULE)/internal/version.Version=$(VERSION) \
  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
  -X $(MODULE)/internal/version.Date=$(DATE)
GOFLAGS     ?=
GO          ?= go
NPM         ?= npm
DOCKER      ?= docker
IMAGE       ?= docker.io/paolor/nexo
PKGS        := ./...
# Pinned versions of the security scanners (make security).
GOVULNCHECK_VERSION ?= v1.8.0
GOSEC_VERSION       ?= v2.29.0
# make fuzz: seconds per target, parallel workers, and an optional regexp
# selecting the targets (FUZZ=Normalize runs FuzzNormalize only).
FUZZTIME      ?= 60s
FUZZ_PARALLEL ?= 2
FUZZ          ?=
# make bench: deployment modes measured by test/bench/warp.sh.
BENCH_MODES   ?= snsd snmd cluster
# Packaging and release (docs/08 §8.7): pinned tools (make release-tools
# installs the Go ones in $(TOOLS_DIR)), the GoReleaser pipes skipped by
# make release-snapshot (docker and sign need a daemon and an OIDC token;
# add sbom when syft is not installed) and the Kubernetes versions whose
# schemas validate the Helm chart.
GORELEASER_VERSION  ?= v2.18.3
HELM_VERSION        ?= v3.22.0
KUBECONFORM_VERSION ?= v0.7.0
SYFT_VERSION        ?= v1.54.1
COSIGN_VERSION      ?= v3.1.3
HADOLINT_VERSION    ?= v2.14.0
TOOLS_DIR           ?= $(CURDIR)/$(BIN_DIR)/tools
GORELEASER          ?= goreleaser
HELM                ?= helm
KUBECONFORM         ?= kubeconform
HADOLINT            ?= hadolint
RELEASE_SKIP        ?= docker,sign
KUBE_VERSIONS       ?= 1.27.0 1.34.0
# User documentation (docs/user, mkdocs.yml): the generated CLI reference,
# the MkDocs site in English (/) and Italian (/it/) (MkDocs, Material for
# MkDocs and mkdocs-static-i18n pinned in docs/requirements.txt, installed by
# make docs-tools in $(DOCS_VENV), the same file of the CI job "docs";
# MKDOCS=mkdocs uses one already on PATH) and the static page of the Admin
# API rendered by Redocly.
DOCS_CLI_DIR        ?= docs/user/cli
PYTHON              ?= python3
DOCS_REQUIREMENTS   ?= docs/requirements.txt
DOCS_VENV           ?= $(CURDIR)/$(BIN_DIR)/docs-venv
MKDOCS              ?= $(DOCS_VENV)/bin/mkdocs
REDOCLY_VERSION     ?= 2.56.1

export CGO_ENABLED ?= 0

.PHONY: all build publish-images build-go build-web build-embedded e2e test test-race test-integration test-cluster test-compose test-proxy test-s3compat test-mint test-upgrade bench-rpc lint web-lint \
        web-test generate fmt clean run-single run-snmd run-cluster docker docker-nexoctl help security fuzz bench \
        release-tools release-check release-snapshot release-smoke helm-lint systemd-verify hadolint packaging \
        docs docs-check docs-api docs-tools docs-site traceability traceability-check dco zap-scan screenshots

all: build

## build: build the web dashboard and both binaries with the dashboard embedded
build: build-web
	$(MAKE) build-embedded
	$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/nexoctl ./cmd/nexoctl

## build-embedded: build bin/nexo embedding the dashboard already built in web/dist (no npm ci)
build-embedded:
	$(GO) build $(GOFLAGS) -tags embedui -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/nexo ./cmd/nexo

## build-go: build both binaries without the embedded dashboard
build-go:
	$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/nexo ./cmd/nexo
	$(GO) build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/nexoctl ./cmd/nexoctl

## build-web: build the React dashboard into web/dist
build-web:
	cd $(WEB_DIR) && $(NPM) ci && $(NPM) run build

## test: run the Go unit tests
test:
	$(GO) test $(GOFLAGS) $(PKGS)

## test-race: run the Go unit tests with the race detector
test-race:
	CGO_ENABLED=1 $(GO) test $(GOFLAGS) -race -count=1 $(PKGS)

## test-integration: run the S3/IAM/presigned suite in every mode (SNSD, SNMD, cluster in-process), slow tests included
test-integration:
	$(GO) test $(GOFLAGS) -count=1 -timeout 20m ./test/integration/...

## test-cluster: run the multi-process cluster tests (TC-*, build tag "cluster") against a fresh bin/nexo
test-cluster:
	$(GO) test $(GOFLAGS) -tags cluster -count=1 -timeout 40m -v ./test/cluster/...

## test-proxy: run Nexo behind two edge proxies with their own domains and origin secrets (TCD-01..03, TCC-C-05, TPS-19 on custom domains; build tag "proxy", Linux): in-process proxies in every mode, and the nginx/HAProxy files of compose.multi-proxy.yml with the real binaries when installed (NEXO_PROXY_REAL=1 requires them)
test-proxy:
	CGO_ENABLED=1 $(GO) test $(GOFLAGS) -tags proxy -race -count=1 -timeout 15m ./test/proxy/...

## test-s3compat: run the Ceph s3-tests compatibility suite against a fresh bin/nexo (SNMD; NEXO_S3TESTS_MODE=snsd|cluster), test/compat, nightly CI
test-s3compat: build-go
	test/compat/run-s3tests.sh

## test-mint: run the SDK suites of MinIO Mint without Docker against a fresh bin/nexo (SNMD; NEXO_MINT_MODE=snsd|cluster, NEXO_MINT_SDKS selects suites), test/compat, nightly CI
test-mint: build-go
	test/compat/run-mint.sh

## test-upgrade: upgrade tests (T-9.8, build tag "upgrade", Linux): the release before (NEXO_UPGRADE_FROM, default f70cc7a, built from git) writes SNMD and cluster deployments, the working tree takes them over (SNMD, rolling upgrade of 4 nodes under load) and the release before is started again (downgrade)
test-upgrade:
	$(GO) test $(GOFLAGS) -tags upgrade -count=1 -timeout 30m -v ./test/upgrade/...

## test-compose: run TM-06/TM-09 against a running deploy/docker/compose.cluster.yml (build tag "compose", nightly CI)
test-compose:
	NEXO_COMPOSE_FILE=deploy/docker/compose.cluster.yml $(GO) test -tags compose -count=1 -v ./test/compose/...

## bench-rpc: measure the throughput of a remote drive over loopback against the local drive (docs/05 T-6.1)
bench-rpc:
	$(GO) test -run '^$$' -bench 'CreateFile|ReadFileStream' -benchtime 2s -count 3 ./internal/transport/rpc/

## security: scan for known vulnerabilities and insecure code: govulncheck (modules and standard library), gosec (false positives carry a "#nosec Gxxx -- reason") and npm audit of the production dependencies of the dashboard (docs/security)
# The scanners run with the toolchain of the module (go env GOVERSION in the
# module, go.mod's go line): "go run pkg@version" ignores the go.mod of the
# main module, so an older installed go would build them, and govulncheck
# could not load the packages of the module.
security:
	GOTOOLCHAIN=$$($(GO) env GOVERSION) $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	GOTOOLCHAIN=$$($(GO) env GOVERSION) $(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) -quiet -exclude-dir=$(WEB_DIR) ./...
	cd $(WEB_DIR) && $(NPM) audit --omit=dev --audit-level=moderate

## zap-scan: authenticated OWASP ZAP active scan of the Admin API driven by api/openapi.yaml against bin/nexo (run make build first; needs Java 17+, downloads ZAP; test/security/zap-api-scan.sh)
zap-scan:
	test/security/zap-api-scan.sh

## dco: check that every commit of DCO_BASE..HEAD (default origin/main) is signed off by its author, as the DCO workflow does on pull requests
DCO_BASE ?= origin/main
dco:
	.github/scripts/check-dco.sh $(DCO_BASE) HEAD

## fuzz: run every Go fuzz target (func FuzzXxx in *_test.go) for FUZZTIME each (default 60s, FUZZ=regexp selects targets); a crasher is written to the testdata/fuzz directory of its package
fuzz:
	@set -e; grep -rlE --include='*_test.go' --exclude-dir=node_modules '^func Fuzz[A-Za-z0-9_]+\(f \*testing\.F\)' . | sort | \
	while read -r file; do \
	  pkg="$$(dirname "$$file")"; \
	  for name in $$(grep -oE '^func Fuzz[A-Za-z0-9_]+' "$$file" | cut -d' ' -f2); do \
	    if [ -n "$(FUZZ)" ] && ! echo "$$name" | grep -qE '$(FUZZ)'; then continue; fi; \
	    echo "== $$pkg $$name ($(FUZZTIME))"; \
	    $(GO) test -run '^$$' -fuzz "^$$name\$$" -fuzztime $(FUZZTIME) -parallel $(FUZZ_PARALLEL) "$$pkg"; \
	  done; \
	done

## bench: run the warp benchmarks of docs/bench (put, get, mixed, list) against a fresh bin/nexo in every mode of BENCH_MODES (snsd snmd cluster); results in bin/bench/<mode>, parameters in test/bench/warp.sh (DURATION, CONCURRENCY, BENCH_TMPFS=1, PROFILE=1, ...)
bench: build-go
	@for m in $(BENCH_MODES); do test/bench/warp.sh $$m || exit 1; done

## lint: run gofmt, go vet and golangci-lint
lint:
	@out="$$(gofmt -l . 2>/dev/null | grep -v '^$(WEB_DIR)/node_modules' || true)"; \
	if [ -n "$$out" ]; then echo "gofmt: files need formatting:"; echo "$$out"; exit 1; fi
	$(GO) vet $(PKGS)
	golangci-lint run ./...

## web-lint: lint the dashboard
web-lint:
	cd $(WEB_DIR) && $(NPM) run lint

## web-test: run the dashboard unit tests
web-test:
	cd $(WEB_DIR) && $(NPM) test

## e2e: run the Playwright suite against bin/nexo (run make build first)
e2e:
	cd $(WEB_DIR) && $(NPM) run e2e

## screenshots: regenerate the console screenshots of the README in docs/images/screenshots: a seeded demo deployment (one node, 4 drives, ports 38000+), some minutes of traffic, Playwright (web/screenshots, not part of make e2e) and pngquant/oxipng when installed (scripts/screenshots/run.sh; SCREENSHOTS_TRAFFIC_SECONDS=60 for a quick run)
# The binaries are built without the -dirty suffix of the version: the
# screenshots show it, and are regenerated before they are committed.
screenshots:
	$(MAKE) build VERSION=$$(git describe --tags --always 2>/dev/null || echo dev)
	scripts/screenshots/run.sh

## generate: run go generate (mocks via mockery)
generate:
	$(GO) generate $(PKGS)

## docs: generate the Markdown reference of the nexoctl commands in docs/user/cli (hidden command nexoctl gendocs)
docs:
	$(GO) run ./cmd/nexoctl gendocs $(DOCS_CLI_DIR)

## docs-check: fail when the CLI reference of docs/user/cli does not match the commands (CI job "docs")
docs-check: docs
	@if ! git diff --quiet -- $(DOCS_CLI_DIR) || [ -n "$$(git status --porcelain -- $(DOCS_CLI_DIR))" ]; then \
	  git status --short -- $(DOCS_CLI_DIR); \
	  echo "$(DOCS_CLI_DIR) is stale: run make docs and commit the result"; exit 1; \
	fi

## docs-api: render api/openapi.yaml as a static HTML page (Redocly, npx) in bin/docs/admin-api.html
docs-api:
	@mkdir -p $(BIN_DIR)/docs
	npx --yes @redocly/cli@$(REDOCLY_VERSION) build-docs api/openapi.yaml --disableGoogleFont \
	  --title "Nexo Admin API" -o $(BIN_DIR)/docs/admin-api.html

## docs-tools: install MkDocs, Material for MkDocs and mkdocs-static-i18n of docs/requirements.txt in a virtualenv (bin/docs-venv)
docs-tools: $(DOCS_VENV)/.requirements

$(DOCS_VENV)/.requirements: $(DOCS_REQUIREMENTS)
	$(PYTHON) -m venv $(DOCS_VENV)
	$(DOCS_VENV)/bin/pip install --disable-pip-version-check -q -r $(DOCS_REQUIREMENTS)
	cp $(DOCS_REQUIREMENTS) $@

## docs-site: build the user documentation site of mkdocs.yml in bin/site, English at / and Italian at /it/ (mkdocs build --strict, then the Admin API page next to its reference page in both languages: bin/site/reference/admin-api/redoc.html and bin/site/it/reference/admin-api/redoc.html)
docs-site: docs $(if $(filter $(DOCS_VENV)/bin/mkdocs,$(MKDOCS)),docs-tools)
	NO_MKDOCS_2_WARNING=1 $(MKDOCS) build --strict -f mkdocs.yml -d $(CURDIR)/$(BIN_DIR)/site
	$(MAKE) docs-api
	cp $(BIN_DIR)/docs/admin-api.html $(BIN_DIR)/site/reference/admin-api/redoc.html
	cp $(BIN_DIR)/docs/admin-api.html $(BIN_DIR)/site/it/reference/admin-api/redoc.html

## fmt: format Go sources
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './$(WEB_DIR)/node_modules/*')

## clean: remove build artefacts
clean:
	rm -rf $(BIN_DIR) $(WEB_DIR)/dist coverage*.txt

## run-single: start a single-node single-drive server on $(CURDIR)/data
run-single: build-go
	@mkdir -p $(CURDIR)/data
	NEXO_ROOT_USER=$${NEXO_ROOT_USER:-admin} NEXO_ROOT_PASSWORD=$${NEXO_ROOT_PASSWORD:-password123} \
	  $(BIN_DIR)/nexo server --log-format text $(CURDIR)/data

## run-snmd: start a single-node multi-drive server on $(CURDIR)/data{1...4}
run-snmd: build-go
	@mkdir -p $(CURDIR)/data1 $(CURDIR)/data2 $(CURDIR)/data3 $(CURDIR)/data4
	NEXO_ROOT_USER=$${NEXO_ROOT_USER:-admin} NEXO_ROOT_PASSWORD=$${NEXO_ROOT_PASSWORD:-password123} \
	  $(BIN_DIR)/nexo server --log-format text '$(CURDIR)/data{1...4}'

# The S3 and console ports of the nodes of run-cluster: the console of
# each node listens on its S3 port + 1, where nexoctl looks for the Admin
# API ("nexoctl alias set local http://127.0.0.1:9101 ..." needs no
# --admin-url). The endpoints are listed without ellipsis (the ports are
# not consecutive): together they form a single pool.
CLUSTER_PORTS := 9101 9103 9105 9107

## run-cluster: start a 4-node cluster (2 drives per node) on 127.0.0.1:{9101,9103,9105,9107} (consoles on S3 port + 1) in ./cluster (Linux), Ctrl-C stops it
run-cluster: build-go
	@mkdir -p cluster; trap 'kill $$(jobs -p) 2>/dev/null' INT TERM EXIT; \
	endpoints=''; for p in $(CLUSTER_PORTS); do \
	  endpoints="$$endpoints http://127.0.0.1:$$p/proc/self/cwd/d1 http://127.0.0.1:$$p/proc/self/cwd/d2"; \
	done; \
	for p in $(CLUSTER_PORTS); do \
	  mkdir -p cluster/n$$p; \
	  ( cd cluster/n$$p && NEXO_ROOT_USER=$${NEXO_ROOT_USER:-admin} NEXO_ROOT_PASSWORD=$${NEXO_ROOT_PASSWORD:-password123} \
	    exec ../../$(BIN_DIR)/nexo server --log-format text --address 127.0.0.1:$$p --console-address 127.0.0.1:$$((p+1)) \
	      $$endpoints ) & \
	done; \
	echo "nexoctl alias set local http://127.0.0.1:9101 $${NEXO_ROOT_USER:-admin} $${NEXO_ROOT_PASSWORD:-password123}  # Admin API on 127.0.0.1:9102"; \
	wait

## docker: build the server image from the sources (Dockerfile: console, nexo and nexoctl)
docker:
	$(DOCKER) build -f Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## docker-nexoctl: build the CLI image from the sources (Dockerfile.nexoctl)
docker-nexoctl:
	$(DOCKER) build -f Dockerfile.nexoctl \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE)ctl:$(VERSION) -t $(IMAGE)ctl:latest .

## release-tools: install the pinned GoReleaser, Helm, kubeconform and syft in bin/tools (export PATH=$$PWD/bin/tools:$$PATH)
release-tools:
	GOBIN=$(TOOLS_DIR) GOTOOLCHAIN=auto $(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
	GOBIN=$(TOOLS_DIR) GOTOOLCHAIN=auto $(GO) install helm.sh/helm/v3/cmd/helm@$(HELM_VERSION)
	GOBIN=$(TOOLS_DIR) GOTOOLCHAIN=auto $(GO) install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)
	GOBIN=$(TOOLS_DIR) GOTOOLCHAIN=auto $(GO) install github.com/anchore/syft/cmd/syft@$(SYFT_VERSION)

## release-check: validate .goreleaser.yaml (goreleaser check)
release-check:
	$(GORELEASER) check

## publish-images: build the multi-arch nexo and nexoctl images locally and push them to ghcr.io and/or Docker Hub with the credentials in the environment (GHCR_USERNAME/GHCR_TOKEN, DOCKERHUB_USERNAME/DOCKERHUB_TOKEN; see scripts/publish-images.sh)
publish-images:
	scripts/publish-images.sh

## release-snapshot: build the whole release locally in dist/ without publishing (binaries for every OS/arch with the console, archives, deb/rpm, checksums, SBOMs, Homebrew casks); RELEASE_SKIP=docker,sign by default
release-snapshot:
	@test -f $(WEB_DIR)/dist/index.html || $(MAKE) build-web
	$(GORELEASER) release --snapshot --clean --skip=$(RELEASE_SKIP)

## release-smoke: TM-10 on the snapshot: version, dry run, server and S3 round trip with the linux/amd64 binaries (and linux/arm64 when qemu-aarch64-static is installed)
release-smoke:
	@set -e; v="v$$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' dist/metadata.json | head -1)"; \
	t="$$(mktemp -d)"; trap 'rm -rf "$$t"' EXIT; \
	mkdir -p "$$t/amd64" "$$t/arm64"; \
	cp dist/nexo_linux_amd64_v1/nexo dist/nexoctl_linux_amd64_v1/nexoctl "$$t/amd64/"; \
	packaging/smoke.sh "$$t/amd64" "$$v"; \
	if command -v qemu-aarch64-static >/dev/null 2>&1; then \
	  cp dist/nexo_linux_arm64_v8.0/nexo dist/nexoctl_linux_arm64_v8.0/nexoctl "$$t/arm64/"; \
	  RUNNER=qemu-aarch64-static packaging/smoke.sh "$$t/arm64" "$$v"; \
	else echo "qemu-aarch64-static not installed: linux/arm64 not run"; fi

## helm-lint: helm lint --strict, helm template and kubeconform -strict of deploy/helm/nexo with the default and the ci/*-values.yaml (KUBE_VERSIONS)
helm-lint:
	HELM=$(HELM) KUBECONFORM=$(KUBECONFORM) KUBE_VERSIONS="$(KUBE_VERSIONS)" deploy/helm/check.sh

## systemd-verify: systemd-analyze verify and security (exposure <= 2.0) of deploy/systemd/nexo.service with bin/nexo
systemd-verify:
	@test -x $(BIN_DIR)/nexo || $(MAKE) build-go
	packaging/systemd-verify.sh $(BIN_DIR)/nexo

## hadolint: lint the Dockerfiles (sources and GoReleaser)
hadolint:
	$(HADOLINT) Dockerfile Dockerfile.nexoctl deploy/docker/Dockerfile.goreleaser deploy/docker/Dockerfile.nexoctl.goreleaser

## packaging: every packaging check that needs no Docker daemon: release-check, helm-lint, systemd-verify, hadolint
packaging: release-check helm-lint systemd-verify hadolint

## traceability: regenerate test/TRACEABILITY.md, the features of docs/02 mapped to the tests tagged "covers: <code>" (docs/06 §6.8; tool in cmd/tools/traceability, not part of any release binary)
traceability:
	$(GO) run ./cmd/tools/traceability

## traceability-check: fail when a P0/P1 feature has no test, a tag names a code outside the catalog or test/TRACEABILITY.md is stale (CI and release)
traceability-check:
	$(GO) run ./cmd/tools/traceability -check

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' -e 's/:/\t/' | column -t -s "$$(printf '\t')"
