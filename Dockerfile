# syntax=docker/dockerfile:1.7
#
# Nexo server image: the React console is built with Node, embedded into the
# static Go binaries (nexo + nexoctl) and shipped on a distroless base.
#
#   docker build -t nexo .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=$(git describe --tags --always) -t ghcr.io/r3n3r0/nexo .
#
# The deployment mode (SNSD / SNMD / cluster) is chosen by the arguments
# passed to "nexo server" (see docs/08-deployment-modes.md); the image is the same.

ARG NODE_VERSION=22
ARG GO_VERSION=1.27.2

# ---------------------------------------------------------------------------
# Stage web: build the React console (web/dist).
# Always runs on the build platform: the output is platform independent.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS web

WORKDIR /src/web
ENV CI=true

# scripts/ holds the postinstall hook run by npm ci.
COPY web/package.json web/package-lock.json ./
COPY web/scripts ./scripts
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund

COPY web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# Stage go: cross-compile the static binaries for TARGETOS/TARGETARCH.
# TARGETOS and TARGETARCH are set automatically by BuildKit/buildx.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS go

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src

# Download modules first so the layer is cached while sources change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
# Overwrite whatever is in the context with the freshly built console.
COPY --from=web /src/web/dist ./web/dist

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    export GOOS="${TARGETOS}" GOARCH="${TARGETARCH}"; \
    LDFLAGS="-s -w \
      -X github.com/r3n3r0/nexo/internal/version.Version=${VERSION} \
      -X github.com/r3n3r0/nexo/internal/version.Commit=${COMMIT} \
      -X github.com/r3n3r0/nexo/internal/version.Date=${DATE}"; \
    go build -trimpath -tags embedui -ldflags "${LDFLAGS}" -o /out/nexo ./cmd/nexo; \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/nexoctl ./cmd/nexoctl; \
    # Empty data and cache directories, copied into the final image with
    # the nonroot ownership (distroless has no shell to run mkdir/chown).
    mkdir -p /out/data /out/cache

# ---------------------------------------------------------------------------
# Final stage: distroless, non-root (UID 65532), no shell.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

LABEL org.opencontainers.image.title="Nexo" \
      org.opencontainers.image.description="S3-compatible object storage server with embedded web console" \
      org.opencontainers.image.url="https://github.com/r3n3r0/nexo" \
      org.opencontainers.image.source="https://github.com/r3n3r0/nexo" \
      org.opencontainers.image.documentation="https://github.com/r3n3r0/nexo/tree/main/docs" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.vendor="r3n3r0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}"

COPY LICENSE /LICENSE
COPY --from=go /out/nexo /nexo
COPY --from=go /out/nexoctl /nexoctl
COPY --from=go --chown=65532:65532 /out/data /data
# Disk tier of the distribution cache (--cache-dir /cache): optional, a
# volume of its own when used (see compose.*.yml).
COPY --from=go --chown=65532:65532 /out/cache /cache

USER 65532:65532

# 9000: S3 API (and internode RPC), 9001: console + Admin API.
EXPOSE 9000 9001
VOLUME /data

ENV NEXO_ADDRESS=:9000 \
    NEXO_CONSOLE_ADDRESS=:9001

# distroless has no curl: the binary probes its own liveness endpoint.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/nexo", "healthcheck"]

ENTRYPOINT ["/nexo"]
CMD ["server", "/data"]
