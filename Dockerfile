# Multi-stage build producing the default engine image plus specialized
# remote-fix and Agent Sandbox fix targets.

# Stage 1: build the SPA. Default base path "/" suits server mode.
FROM node:20-alpine AS web
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: build the Go binaries.
FROM golang:1.25.12-bookworm AS build
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
ARG VERSION=dev
ARG COMMIT=dev
ARG IMAGE_TAG=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/fetcher ./cmd/aster \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.imageTag=${IMAGE_TAG}" -o /out/fixexecutor ./cmd/fixexecutor

# Pinned Go toolchain copied into the Agent Sandbox Fix executor.
FROM golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS agent-sandbox-fix-go

# OpenCode executor for consumer-installed Agent Sandbox.
# OpenCode is inherited from its official image pinned by release and OCI digest.
FROM ghcr.io/anomalyco/opencode:1.18.2@sha256:ef9257b3246e9be63d5050924c07f7e6d8d9f135fdfcd8422fc873a408c367af AS agent-sandbox-fix-executor
ARG VERSION=dev
ARG COMMIT=dev
ARG IMAGE_TAG=dev
USER root
COPY --from=agent-sandbox-fix-go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:${PATH} \
    GOTOOLCHAIN=local
RUN apk add --no-cache ca-certificates git=2.54.0-r0 \
 && test "$(go env GOVERSION)" = "go1.25.12" \
 && test "$(git --version)" = "git version 2.54.0" \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && test "$(opencode --version)" = "1.18.2"
COPY --from=build /out/fixexecutor /usr/local/bin/fixexecutor
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Agent Sandbox Fix Executor" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster" \
      org.opencontainers.image.version=${VERSION} \
      org.opencontainers.image.revision=${COMMIT} \
      io.prow-ai-dashboard.image-tag=${IMAGE_TAG}
ENV HOME=/tmp/home \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/fixexecutor"]

# Minimal git-capable engine for reconstructing patches returned by remote fix
# runtimes such as Agent Sandbox. It intentionally omits any coding-agent harness.
FROM golang:1.25.12-alpine AS remote-fixer-runtime
RUN apk add --no-cache ca-certificates git=2.54.0-r0 \
 && addgroup -g 65532 padnonroot \
 && adduser -D -H -u 65532 -G padnonroot padnonroot \
 && test "$(git --version)" = "git version 2.54.0"
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
ENV HOME=/tmp \
    GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster Remote Fixer" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster"
ENTRYPOINT ["/usr/local/bin/server"]

# Stage 3: minimal runtime. distroless/static ships CA certs for HTTPS to GCS,
# GitHub, and the AI endpoint, and runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/fetcher /usr/local/bin/fetcher
COPY --from=build /out/worker /usr/local/bin/worker
COPY --from=build /out/server /usr/local/bin/server
COPY --from=web /src/frontend/dist /app/web
USER 65532:65532
LABEL org.opencontainers.image.source="https://github.com/willie-yao/aster" \
      org.opencontainers.image.title="Aster" \
      org.opencontainers.image.url="https://github.com/willie-yao/aster"
# Server is the default entrypoint; the fetcher CronJob overrides command.
ENTRYPOINT ["/usr/local/bin/server"]
