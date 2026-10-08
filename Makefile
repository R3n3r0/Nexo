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

.PHONY: all build build-go build-web build-embedded e2e test test-race lint web-lint web-test generate fmt clean \
        run-single run-snmd docker help

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

## docker: build the container image
docker:
	$(DOCKER) build -f deploy/docker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/^## //' -e 's/:/\t/' | column -t -s "$$(printf '\t')"
