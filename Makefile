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

export CGO_ENABLED ?= 0

.PHONY: all build build-go build-web build-embedded e2e test test-race test-integration test-cluster test-compose test-s3compat bench-rpc lint web-lint \
        web-test generate fmt clean run-single run-snmd run-cluster docker help

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

## test-s3compat: run the Ceph s3-tests compatibility suite against a fresh bin/nexo (SNMD; NEXO_S3TESTS_MODE=snsd|cluster), test/compat, nightly CI
test-s3compat: build-go
	test/compat/run-s3tests.sh

## test-compose: run TM-06/TM-09 against a running deploy/docker/compose.cluster.yml (build tag "compose", nightly CI)
test-compose:
	NEXO_COMPOSE_FILE=deploy/docker/compose.cluster.yml $(GO) test -tags compose -count=1 -v ./test/compose/...

## bench-rpc: measure the throughput of a remote drive over loopback against the local drive (docs/05 T-6.1)
bench-rpc:
	$(GO) test -run '^$$' -bench 'CreateFile|ReadFileStream' -benchtime 2s -count 3 ./internal/transport/rpc/

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

## docker: build the container image
docker:
	$(DOCKER) build -f deploy/docker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' -e 's/:/\t/' | column -t -s "$$(printf '\t')"
