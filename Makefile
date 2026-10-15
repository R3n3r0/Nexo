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
IMAGE       ?= ghcr.io/r3n3r0/nexo
PKGS        := ./...
# Pinned versions of the security scanners (make security).
GOVULNCHECK_VERSION ?= v1.1.4
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

export CGO_ENABLED ?= 0

.PHONY: all build build-go build-web build-embedded e2e test test-race test-integration test-cluster test-compose test-proxy test-s3compat bench-rpc lint web-lint \
        web-test generate fmt clean run-single run-snmd run-cluster docker docker-nexoctl help security fuzz bench \
        release-tools release-check release-snapshot release-smoke helm-lint systemd-verify hadolint packaging

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

## test-compose: run TM-06/TM-09 against a running deploy/docker/compose.cluster.yml (build tag "compose", nightly CI)
test-compose:
	NEXO_COMPOSE_FILE=deploy/docker/compose.cluster.yml $(GO) test -tags compose -count=1 -v ./test/compose/...

## bench-rpc: measure the throughput of a remote drive over loopback against the local drive (docs/05 T-6.1)
bench-rpc:
	$(GO) test -run '^$$' -bench 'CreateFile|ReadFileStream' -benchtime 2s -count 3 ./internal/transport/rpc/

## security: scan for known vulnerabilities and insecure code: govulncheck (modules and standard library), gosec (false positives carry a "#nosec Gxxx -- reason") and npm audit of the production dependencies of the dashboard (docs/security)
security:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) -quiet -exclude-dir=$(WEB_DIR) ./...
	cd $(WEB_DIR) && $(NPM) audit --omit=dev --audit-level=moderate

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

## generate: run go generate (mocks via mockery)
generate:
	$(GO) generate $(PKGS)

## fmt: format Go sources
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './$(WEB_DIR)/node_modules/*')

## clean: remove build artefacts
clean:
	rm -rf $(BIN_DIR) $(WEB_DIR)/dist coverage*.txt

## run-single: start a single-node single-drive server on ./data
run-single: build-go
	NEXO_ROOT_USER=$${NEXO_ROOT_USER:-admin} NEXO_ROOT_PASSWORD=$${NEXO_ROOT_PASSWORD:-password123} \
	  $(BIN_DIR)/nexo server --log-format text ./data

## run-snmd: start a single-node multi-drive server on ./data{1..4}
run-snmd: build-go
	NEXO_ROOT_USER=$${NEXO_ROOT_USER:-admin} NEXO_ROOT_PASSWORD=$${NEXO_ROOT_PASSWORD:-password123} \
	  $(BIN_DIR)/nexo server --log-format text ./data1 ./data2 ./data3 ./data4

## run-cluster: start a 4-node cluster (2 drives per node) on 127.0.0.1:{9101..9104} in ./cluster (Linux), Ctrl-C stops it
run-cluster: build-go
	@mkdir -p cluster; trap 'kill $$(jobs -p) 2>/dev/null' INT TERM EXIT; \
	for i in 1 2 3 4; do \
	  mkdir -p cluster/n$$i; \
	  ( cd cluster/n$$i && NEXO_ROOT_USER=$${NEXO_ROOT_USER:-admin} NEXO_ROOT_PASSWORD=$${NEXO_ROOT_PASSWORD:-password123} \
	    exec ../../$(BIN_DIR)/nexo server --log-format text --address 127.0.0.1:910$$i --console-address 127.0.0.1:920$$i \
	      'http://127.0.0.1:{9101...9104}/proc/self/cwd/d{1...2}' ) & \
	done; wait

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

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' -e 's/:/\t/' | column -t -s "$$(printf '\t')"
